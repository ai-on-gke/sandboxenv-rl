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

// Package job implements the multi-job scenario runner, simulated closed-loop
// and open-loop job drivers, real RL job orchestrator (including PR #112
// Qwen3-1.7B VeRL support), and trainer telemetry/trace adapters.
package job

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/agent-substrate/env/internal/benchmarking/schema"
	"github.com/agent-substrate/env/internal/benchmarking/swebench"
)

// JobDriver executes a single scheduled or real RL job against the SUT and
// emits canonical benchmark Events into the shared Recorder.
type JobDriver interface {
	Name() string
	Run(ctx context.Context, rec *swebench.Recorder) error
}

// ScenarioRunner coordinates one or more concurrent JobDrivers according to a
// Scenario and pre-generated Schedule, writing `schedule.json` and `events.jsonl`
// into the run output directory.
type ScenarioRunner struct {
	RunID      string
	Scenario   *schema.Scenario
	Schedule   *schema.Schedule
	OutDir     string
	ClientFunc func(job schema.JobSpec) swebench.EnvClient
	CmdRunner  CommandExecutor
	SleepFunc  func(ctx context.Context, d time.Duration) error
}

// GenerateSchedule builds a deterministic `schema.Schedule` for all jobs in a
// Scenario using the scenario's seed.
func GenerateSchedule(sc *schema.Scenario, dataset []swebench.Instance) (*schema.Schedule, error) {
	if sc == nil {
		return nil, fmt.Errorf("scenario is nil")
	}
	return swebench.ResolveScenarioSchedule(*sc, "", map[string][]swebench.Instance{"": dataset})
}

// Run executes all jobs in the Scenario concurrently (honoring per-job
// `start_offset_ms`), writes `schedule.json` and `events.jsonl` in `OutDir`,
// and returns the first error encountered.
func (sr *ScenarioRunner) Run(ctx context.Context) error {
	if sr.Scenario == nil {
		return fmt.Errorf("ScenarioRunner.Scenario is nil")
	}
	if sr.OutDir == "" {
		return fmt.Errorf("ScenarioRunner.OutDir is required")
	}
	if err := os.MkdirAll(sr.OutDir, 0o755); err != nil {
		return fmt.Errorf("mkdir %s: %w", sr.OutDir, err)
	}
	runID := sr.RunID
	if runID == "" {
		runID = fmt.Sprintf("%s-%d", sr.Scenario.Name, time.Now().UTC().Unix())
	}

	sched := sr.Schedule
	if sched == nil {
		var err error
		sched, err = GenerateSchedule(sr.Scenario, nil)
		if err != nil {
			return fmt.Errorf("generate schedule: %w", err)
		}
		sr.Schedule = sched
	}

	if err := swebench.WriteSchedule(filepath.Join(sr.OutDir, "schedule.json"), sched); err != nil {
		return fmt.Errorf("write schedule.json: %w", err)
	}

	eventsPath := filepath.Join(sr.OutDir, "events.jsonl")
	rec, err := swebench.NewFileRecorder(eventsPath, runID)
	if err != nil {
		return fmt.Errorf("create recorder: %w", err)
	}
	defer rec.Close()

	schedByJob := make(map[string]schema.ScheduledJob, len(sched.Jobs))
	for _, sj := range sched.Jobs {
		schedByJob[sj.Job] = sj
	}

	sleepFn := sr.SleepFunc
	if sleepFn == nil {
		sleepFn = defaultSleep
	}

	var wg sync.WaitGroup
	errCh := make(chan error, len(sr.Scenario.Jobs))

	for idx, jobSpec := range sr.Scenario.Jobs {
		sj := schedByJob[jobSpec.Name]
		driver, err := sr.buildDriver(idx, jobSpec, sj, runID)
		if err != nil {
			return fmt.Errorf("build driver for job %s: %w", jobSpec.Name, err)
		}
		wg.Add(1)
		go func(spec schema.JobSpec, drv JobDriver) {
			defer wg.Done()
			if spec.StartOffsetMS > 0 {
				if err := sleepFn(ctx, time.Duration(spec.StartOffsetMS*float64(time.Millisecond))); err != nil {
					errCh <- err
					return
				}
			}
			if err := drv.Run(ctx, rec); err != nil {
				errCh <- fmt.Errorf("job %s: %w", spec.Name, err)
			}
		}(jobSpec, driver)
	}

	wg.Wait()
	close(errCh)
	for e := range errCh {
		if e != nil {
			return e
		}
	}
	return nil
}

func (sr *ScenarioRunner) buildDriver(idx int, spec schema.JobSpec, sj schema.ScheduledJob, runID string) (JobDriver, error) {
	switch spec.Mode {
	case schema.JobModeClosedLoop, schema.JobModeOpenLoop:
		var client swebench.EnvClient
		if sr.ClientFunc != nil {
			client = sr.ClientFunc(spec)
		}
		if client == nil {
			return nil, fmt.Errorf("ClientFunc returned nil EnvClient for simulated job %q", spec.Name)
		}
		seed := int64(42)
		if sr.Scenario != nil && sr.Scenario.Seed != 0 {
			seed = sr.Scenario.Seed + int64(idx)*1000
		}
		return &SimulatedJobDriver{
			RunID:     runID,
			Spec:      spec,
			Scheduled: sj,
			Client:    client,
			Seed:      seed,
			SleepFunc: sr.SleepFunc,
		}, nil
	case schema.JobModeReal:
		ns := "ate-bench"
		serverAddr := "http://ate-env-server.ate-bench.svc.cluster.local:8080"
		if sr.Scenario != nil {
			if sr.Scenario.SUT.Namespace != "" {
				ns = sr.Scenario.SUT.Namespace
			}
			if sr.Scenario.SUT.ServerAddr != "" {
				serverAddr = sr.Scenario.SUT.ServerAddr
			}
		}
		return &RealJobDriver{
			RunID:      runID,
			Namespace:  ns,
			ServerAddr: serverAddr,
			Spec:       spec,
			OutDir:     sr.OutDir,
			Executor:   sr.CmdRunner,
		}, nil
	default:
		return nil, fmt.Errorf("unsupported job mode %q", spec.Mode)
	}
}

func defaultSleep(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
