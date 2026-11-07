// Copyright 2026 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package measure

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// SnapshotsDirName is the artifact subdirectory holding periodic
// measurements samples that `bench report` rolls up into measurements.json.
const SnapshotsDirName = "measurements_snapshots"

// SamplerOptions configures RunSnapshotSampler.
type SamplerOptions struct {
	// Dir receives one `<RFC3339-UTC>.json` Measurements file per sample.
	Dir string
	// Interval between samples; must be > 0.
	Interval time.Duration
	// Duration bounds the sampling window; 0 samples until ctx is cancelled.
	Duration time.Duration
	// OnError is called for a failed sample; the loop continues. nil ignores.
	OnError func(err error)
	// Now overrides the clock (tests).
	Now func() time.Time
}

// SamplerResult is what RunSnapshotSampler leaves behind.
type SamplerResult struct {
	// Written is the number of snapshot files written.
	Written int
	// Failed is the number of samples that collected or wrote nothing.
	Failed int
	// Last is the last successful collection: the system description it
	// carries is the most recent view of the cluster, which `bench measure`
	// writes as system_description.json. nil when nothing succeeded.
	Last *CollectResult
}

// ErrNoSnapshots is returned when every sample failed: the caller asked for
// telemetry and got none, which must not look like success.
var ErrNoSnapshots = errors.New("measure: no snapshot was written")

// RunSnapshotSampler runs the collectors in req every Interval and writes
// each result to Dir until ctx is cancelled or Duration elapses. A single
// post-run `bench measure` sees an idle cluster (sandboxes already released,
// packing at 0%); sampling during the job window is what makes
// `active_sandboxes` and `cluster_packing_*_pct` reflect the run. A cancelled
// context ends the loop normally; it returns ErrNoSnapshots (with the
// per-sample errors already reported through OnError) when nothing was
// written.
func RunSnapshotSampler(ctx context.Context, req CollectRequest, opts SamplerOptions) (SamplerResult, error) {
	var out SamplerResult
	if opts.Interval <= 0 {
		return out, errors.New("measure: sampler interval must be > 0")
	}
	if opts.Dir == "" {
		return out, errors.New("measure: sampler dir is required")
	}
	if err := os.MkdirAll(opts.Dir, 0o755); err != nil {
		return out, fmt.Errorf("mkdir %s: %w", opts.Dir, err)
	}
	now := opts.Now
	if now == nil {
		now = func() time.Time { return time.Now().UTC() }
	}
	if opts.Duration > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, opts.Duration)
		defer cancel()
	}

	ticker := time.NewTicker(opts.Interval)
	defer ticker.Stop()
	fail := func(err error) {
		out.Failed++
		if opts.OnError != nil && ctx.Err() == nil {
			opts.OnError(err)
		}
	}
	sample := func() {
		ts := now()
		sreq := req
		sreq.CollectedAt = ts
		res, err := RunCollectors(ctx, sreq)
		if err != nil {
			fail(err)
			return
		}
		name := ts.Format("20060102T150405.000Z") + ".json"
		if err := WriteMeasurements(filepath.Join(opts.Dir, name), res.Measurements); err != nil {
			fail(err)
			return
		}
		out.Written++
		out.Last = res
	}

	sample() // one sample immediately, so short windows still produce data
	for {
		select {
		case <-ctx.Done():
			if out.Written == 0 {
				return out, fmt.Errorf("%w (%d sample(s) failed)", ErrNoSnapshots, out.Failed)
			}
			return out, nil
		case <-ticker.C:
			sample()
		}
	}
}
