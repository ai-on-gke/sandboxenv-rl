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

package job

import (
	"context"
	"fmt"
	"time"

	"github.com/agent-substrate/env/internal/benchmarking/schema"
	"github.com/agent-substrate/env/internal/benchmarking/swebench"
)

// SimulatedJobDriver runs a closed-loop (step-barrier batch + park policy +
// lookahead prefetch) or open-loop (Poisson arrival rate) simulated RL job
// by delegating to swebench.Runner.
type SimulatedJobDriver struct {
	RunID     string
	Spec      schema.JobSpec
	Scheduled schema.ScheduledJob
	Client    swebench.EnvClient
	Seed      int64
	SleepFunc func(ctx context.Context, d time.Duration) error
}

// Name returns the job's unique name.
func (d *SimulatedJobDriver) Name() string {
	return d.Spec.Name
}

// Run executes the simulated job in either `open-loop` or `closed-loop` mode.
func (d *SimulatedJobDriver) Run(ctx context.Context, rec *swebench.Recorder) error {
	if d.Client == nil {
		return fmt.Errorf("SimulatedJobDriver (%s): Client is nil", d.Spec.Name)
	}
	if rec == nil {
		return fmt.Errorf("SimulatedJobDriver (%s): Recorder is nil", d.Spec.Name)
	}

	concurrency := d.Spec.Concurrency
	if concurrency <= 0 {
		concurrency = 4
	}

	if d.Spec.Mode == schema.JobModeOpenLoop {
		var instances []swebench.Instance
		seen := make(map[string]bool)
		for _, st := range d.Scheduled.Steps {
			for _, t := range st.Tasks {
				if !seen[t.InstanceID] {
					seen[t.InstanceID] = true
					instances = append(instances, swebench.Instance{
						InstanceID: t.InstanceID,
						Image:      t.Image,
					})
				}
			}
		}
		if len(instances) == 0 {
			n := d.Spec.Instances
			if n <= 0 {
				n = 4
			}
			instances = swebench.SyntheticInstances(n, d.Spec.ImagePrefix)
		}
		dur := time.Duration(d.Spec.DurationSec * float64(time.Second))
		if dur <= 0 {
			dur = 500 * time.Millisecond
		}
		rate := d.Spec.ArrivalRate
		if rate <= 0 {
			rate = 10.0
		}

		runner := &swebench.Runner{
			Config: swebench.Config{
				RunID:        d.RunID,
				JobName:      d.Spec.Name,
				Mode:         swebench.ModeOpenLoop,
				Concurrency:  concurrency,
				ArrivalRate:  rate,
				Duration:     dur,
				Seed:         d.Seed,
				SnapshotLoop: d.Spec.SnapshotLoop,
				Instances:    instances,
			},
			Client:   d.Client,
			Recorder: rec,
		}
		return runner.Run(ctx)
	}

	runner := &swebench.Runner{
		Config: swebench.Config{
			RunID:        d.RunID,
			JobName:      d.Spec.Name,
			Mode:         swebench.ModeSteps,
			Concurrency:  concurrency,
			Seed:         d.Seed,
			SnapshotLoop: d.Spec.SnapshotLoop,
		},
		Client:     d.Client,
		Recorder:   rec,
		Scheduled:  d.Scheduled,
		ParkPolicy: d.Spec.ParkPolicy,
		SleepFunc:  d.SleepFunc,
	}
	return runner.Run(ctx)
}
