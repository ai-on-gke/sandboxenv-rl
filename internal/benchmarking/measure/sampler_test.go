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
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/agent-substrate/env/internal/benchmarking/schema"
)

// countingCollector records one scalar per sample so the test can tell
// samples apart.
type countingCollector struct {
	n    int
	fail error // returned from every Collect when set
}

func (countingCollector) Name() string                 { return "test-counting" }
func (countingCollector) Scope() schema.TelemetryScope { return schema.ScopeSubstrate }
func (c *countingCollector) Collect(_ context.Context, _ CollectRequest, out *CollectResult) error {
	c.n++
	if c.fail != nil {
		return c.fail
	}
	out.EnsureServerSummary()["active_sandboxes"] = singleScalarSummary(float64(c.n*10), "count")
	out.SystemDescription = &schema.SystemDescription{SchemaVersion: schema.SchemaVersion, Cluster: "test"}
	return nil
}

// registerCounting registers col for the test and removes it afterwards.
func registerCounting(t *testing.T, col *countingCollector) {
	t.Helper()
	RegisterCollector(col)
	t.Cleanup(func() { unregisterCollector(col.Name()) })
}

func TestRunSnapshotSamplerWritesOneFilePerSample(t *testing.T) {
	col := &countingCollector{}
	registerCounting(t, col)
	dir := filepath.Join(t.TempDir(), SnapshotsDirName)

	tick := time.Date(2026, 4, 19, 12, 0, 0, 0, time.UTC)
	now := func() time.Time { tick = tick.Add(time.Second); return tick }

	sr, err := RunSnapshotSampler(context.Background(), CollectRequest{
		RunID:             "run-s",
		Scopes:            []schema.TelemetryScope{schema.ScopeSubstrate},
		EnabledCollectors: []string{"test-counting"},
	}, SamplerOptions{Dir: dir, Interval: 5 * time.Millisecond, Duration: 60 * time.Millisecond, Now: now})
	if err != nil {
		t.Fatalf("RunSnapshotSampler: %v", err)
	}
	if sr.Written < 3 || sr.Failed != 0 {
		t.Fatalf("result %+v, want >= 3 written (immediate + ticks) and none failed", sr)
	}
	if sr.Last == nil || sr.Last.SystemDescription == nil || sr.Last.SystemDescription.Cluster != "test" {
		t.Fatalf("Last must carry the final sample's system description, got %+v", sr.Last)
	}
	files, _ := filepath.Glob(filepath.Join(dir, "*.json"))
	if len(files) != sr.Written {
		t.Fatalf("%d files on disk, sampler reported %d", len(files), sr.Written)
	}
	raw, err := os.ReadFile(files[0])
	if err != nil {
		t.Fatal(err)
	}
	m, err := schema.DecodeJSONStrict[schema.Measurements](bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("snapshot is not a valid Measurements: %v", err)
	}
	if m.RunID != "run-s" || m.ServerSummary["active_sandboxes"].P50 != 10 {
		t.Fatalf("unexpected first snapshot: %+v", m)
	}
}

func TestRunSnapshotSamplerStopsOnCancel(t *testing.T) {
	registerCounting(t, &countingCollector{})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan int, 1)
	go func() {
		sr, _ := RunSnapshotSampler(ctx, CollectRequest{
			RunID:             "run-c",
			Scopes:            []schema.TelemetryScope{schema.ScopeSubstrate},
			EnabledCollectors: []string{"test-counting"},
		}, SamplerOptions{Dir: t.TempDir(), Interval: time.Hour})
		done <- sr.Written
	}()
	time.Sleep(20 * time.Millisecond)
	cancel()
	select {
	case n := <-done:
		if n != 1 {
			t.Fatalf("n = %d, want exactly the immediate sample", n)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("sampler did not stop on cancel")
	}
}

func TestRunSnapshotSamplerRejectsBadOptions(t *testing.T) {
	if _, err := RunSnapshotSampler(context.Background(), CollectRequest{RunID: "x"}, SamplerOptions{Dir: t.TempDir()}); err == nil {
		t.Fatal("expected error for zero interval")
	}
	if _, err := RunSnapshotSampler(context.Background(), CollectRequest{RunID: "x"}, SamplerOptions{Interval: time.Second}); err == nil {
		t.Fatal("expected error for empty dir")
	}
}

func TestRunSnapshotSamplerFailsWhenEverySampleFails(t *testing.T) {
	registerCounting(t, &countingCollector{fail: errors.New("scrape down")})
	dir := t.TempDir()
	var seen []error
	sr, err := RunSnapshotSampler(context.Background(), CollectRequest{
		RunID:             "run-f",
		Scopes:            []schema.TelemetryScope{schema.ScopeSubstrate},
		EnabledCollectors: []string{"test-counting"},
	}, SamplerOptions{Dir: dir, Interval: 5 * time.Millisecond, Duration: 30 * time.Millisecond,
		OnError: func(e error) { seen = append(seen, e) }})
	if !errors.Is(err, ErrNoSnapshots) {
		t.Fatalf("err = %v, want ErrNoSnapshots", err)
	}
	if sr.Written != 0 || sr.Failed == 0 || len(seen) != sr.Failed {
		t.Fatalf("result %+v, OnError calls %d: want 0 written and every failure reported", sr, len(seen))
	}
	if files, _ := filepath.Glob(filepath.Join(dir, "*.json")); len(files) != 0 {
		t.Fatalf("no file may be written when every sample fails, got %v", files)
	}
}
