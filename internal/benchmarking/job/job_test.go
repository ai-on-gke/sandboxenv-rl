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
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/agent-substrate/env/internal/benchmarking/schema"
	"github.com/agent-substrate/env/internal/benchmarking/swebench"
)

type fakeEnvClient struct {
	seq atomic.Int64
}

func (f *fakeEnvClient) Create(_ context.Context, image, instanceID string) (swebench.EnvHandle, error) {
	id := f.seq.Add(1)
	return swebench.EnvHandle{
		Name:   fmt.Sprintf("env-%d", id),
		Source: string(schema.SourceWarmPool),
	}, nil
}

func (f *fakeEnvClient) Exec(_ context.Context, _, _ string, _ time.Duration) (swebench.ExecResult, error) {
	return swebench.ExecResult{ExitCode: 0, Stdout: "ok"}, nil
}

func (f *fakeEnvClient) Delete(_ context.Context, _ string) error {
	return nil
}

func (f *fakeEnvClient) Snapshot(_ context.Context, envName string) (string, error) {
	return "snap-" + envName, nil
}

func (f *fakeEnvClient) Resume(_ context.Context, snapshot, _, _ string) (swebench.EnvHandle, error) {
	return swebench.EnvHandle{
		Name:   "resumed-" + snapshot,
		Source: string(schema.SourceSnapshotResume),
	}, nil
}

type fakeCmdExecutor struct {
	calls []string
	out   string
}

func (f *fakeCmdExecutor) Run(_ context.Context, name string, _ map[string]string, args ...string) ([]byte, error) {
	f.calls = append(f.calls, name+" "+strings.Join(args, " "))
	return []byte(f.out), nil
}

func TestScenarioRunnerMultiJobClosedAndOpenLoop(t *testing.T) {
	dir := t.TempDir()
	sc := &schema.Scenario{
		SchemaVersion: schema.VersionV1,
		Name:          "two-job-test",
		Seed:          99,
		SUT: schema.SUTSpec{
			Namespace: "ate-bench",
		},
		Jobs: []schema.JobSpec{
			{
				Name:           "job-closed",
				Mode:           schema.JobModeClosedLoop,
				Steps:          2,
				BatchSize:      2,
				Concurrency:    2,
				StepPauseMS:    5,
				ParkPolicy:     schema.ParkPolicySnapshot,
				LookaheadSteps: 1,
				Instances:      3,
			},
			{
				Name:          "job-open",
				Mode:          schema.JobModeOpenLoop,
				StartOffsetMS: 2,
				Concurrency:   2,
				ArrivalRate:   50.0,
				DurationSec:   0.05,
				Instances:     2,
			},
		},
	}

	runner := &ScenarioRunner{
		RunID:    "run-multi",
		Scenario: sc,
		OutDir:   dir,
		ClientFunc: func(_ schema.JobSpec) swebench.EnvClient {
			return &fakeEnvClient{}
		},
		SleepFunc: func(_ context.Context, _ time.Duration) error { return nil },
	}
	if err := runner.Run(context.Background()); err != nil {
		t.Fatalf("ScenarioRunner.Run: %v", err)
	}

	eventsPath := filepath.Join(dir, "events.jsonl")
	rep, err := swebench.BuildReport(eventsPath)
	if err != nil {
		t.Fatalf("BuildReport: %v", err)
	}
	if len(rep.Jobs) != 2 {
		t.Fatalf("len(rep.Jobs) = %d, want 2", len(rep.Jobs))
	}
	if rep.Interference == nil || rep.Interference.ActiveJobs != 2 {
		t.Fatalf("expected Interference summary for 2 jobs, got %+v", rep.Interference)
	}
}

func TestRealJobDriverVeRLAndTraceIngestion(t *testing.T) {
	dir := t.TempDir()
	traceDir := filepath.Join(dir, "traces", "qwen3-1p7b-verl")
	if err := os.MkdirAll(traceDir, 0o755); err != nil {
		t.Fatal(err)
	}
	traceLine := `{"schema_version":"v1","ts":"2026-04-19T12:00:00Z","run":"run-verl","job":"qwen3-1p7b-verl","step":1,"op":"create","env":"env-verl-1","dur_ms":145.0,"ok":true}` + "\n" +
		`{"schema_version":"v1","ts":"2026-04-19T12:00:01Z","run":"run-verl","job":"qwen3-1p7b-verl","step":1,"op":"exec","command":["python3","-c","print(42)"],"env":"env-verl-1","dur_ms":420.0,"ok":true,"exit":0}` + "\n"
	for _, rawLine := range strings.Split(strings.TrimSpace(traceLine), "\n") {
		if err := schema.ValidateAgainstJSONSchema("trace_event.schema.json", []byte(rawLine)); err != nil {
			t.Fatalf("ValidateAgainstJSONSchema(trace_event.schema.json): %v", err)
		}
	}
	if err := os.WriteFile(filepath.Join(traceDir, "worker-0.jsonl"), []byte(traceLine), 0o644); err != nil {
		t.Fatal(err)
	}

	execFake := &fakeCmdExecutor{
		out: "step:1 - fully_async/processing_time/tp50:12.500 - fully_async/processing_time/max:25.000 - timing_s/agent_loop/tool_calls/mean:0.120 - timing_s/agent_loop/tool_calls/max:0.240\n" +
			"step:1 - timing_s/step:3.500 - timing_s/gen:2.100 - timing_s/update_actor:1.200 - timing_s/old_log_prob:0.200 - timing_s/reward:2.39e-05 - timing_s/adv:1.5e-03 - critic/score/mean:0.750\n" +
			"[FullyAsyncTrainer] _fit_update_weights:  Parameter sync time: 2.34 s\n" +
			"step:2 - fully_async/processing_time/tp50:11.000 - fully_async/processing_time/max:22.000 - timing_s/agent_loop/tool_calls/mean:0.110 - timing_s/agent_loop/tool_calls/max:0.220\n" +
			"step:2 - timing_s/step:3.200 - timing_s/gen:1.900 - timing_s/update_actor:1.100 - timing_s/old_log_prob:0.180 - timing_s/reward:1.80e-05 - timing_s/adv:1.2e-03 - critic/score/mean:0.812\n",
	}

	eventsPath := filepath.Join(dir, "events.jsonl")
	rec, err := swebench.NewFileRecorder(eventsPath, "run-verl")
	if err != nil {
		t.Fatal(err)
	}

	drv := &RealJobDriver{
		RunID:      "run-verl",
		Namespace:  "default",
		ServerAddr: "http://ate-env:8080",
		OutDir:     dir,
		Executor:   execFake,
		Spec: schema.JobSpec{
			Name:            "qwen3-1p7b-verl",
			Mode:            schema.JobModeReal,
			TrainerManifest: "examples/Qwen3-1.7B-verl/job-fully-async.yaml",
			TraceMode:       "trace",
		},
	}
	if err := drv.Run(context.Background(), rec); err != nil {
		t.Fatalf("RealJobDriver.Run: %v", err)
	}
	_ = rec.Close()

	rep, err := swebench.BuildReport(eventsPath)
	if err != nil {
		t.Fatalf("BuildReport: %v", err)
	}
	if len(rep.Steps) != 2 {
		t.Fatalf("len(rep.Steps) = %d, want 2", len(rep.Steps))
	}
	if rep.Tasks != 1 || rep.AcquisitionSources["warm_pool"] < 1 {
		t.Fatalf("unexpected Tasks/AcquisitionSources: tasks=%d sources=%+v", rep.Tasks, rep.AcquisitionSources)
	}
	if rep.Headline == nil || rep.Headline.TTFIP50MS != 565.0 || rep.Headline.WireClaimToReadyP50MS != 145.0 || rep.Headline.ReadyToFirstCmdP50MS != 420.0 {
		t.Fatalf("unexpected Headline TTFI breakdown: %+v", rep.Headline)
	}
	if rep.RLBreakdown == nil || rep.RLBreakdown.ParamSyncP50MS != 2340.0 || rep.RLBreakdown.RewardP50MS <= 0 {
		t.Fatalf("unexpected RLBreakdown: %+v", rep.RLBreakdown)
	}
	if rep.Convergence == nil || rep.Convergence.TotalStepsRun != 2 || rep.Convergence.Converged {
		t.Fatalf("unexpected Convergence: %+v", rep.Convergence)
	}
}

func TestIngestArtifactDirScopesAndPlugins(t *testing.T) {
	dir := t.TempDir()
	trainerLog := "(TaskRunner pid=1) step:1 - timing_s/step:10.0 - timing_s/gen:6.0 - timing_s/update_actor:3.0 - critic/score/mean:0.50 - val-core/princeton-nlp/SWE-bench_Verified/acc/mean@1:0.25\n"
	if err := os.WriteFile(filepath.Join(dir, "verl-head.log"), []byte(trainerLog), 0o644); err != nil {
		t.Fatal(err)
	}
	traceJSONL := `{"ts":"2026-04-20T18:00:00Z","run":"rlb-1","job":"verl-job","op":"create","env":"sb-1","instance":"inst-1","dur_ms":100,"ok":true}` + "\n" +
		`{"ts":"2026-04-20T18:00:01Z","run":"rlb-1","job":"verl-job","op":"exec","env":"sb-1","instance":"inst-1","dur_ms":200,"ok":true,"exit":0}` + "\n"
	if err := os.WriteFile(filepath.Join(dir, "trace.jsonl"), []byte(traceJSONL), 0o644); err != nil {
		t.Fatal(err)
	}

	// Only user-sandbox scope: should ingest trace events and skip trainer step events.
	resUserOnly, err := IngestArtifactDir(IngestContext{
		DestDir: dir,
		Scopes:  []schema.TelemetryScope{schema.ScopeUserSandbox},
		Plugins: []schema.TelemetryPluginSpec{
			{
				Name:  "val_acc",
				Kind:  schema.PluginKindRegexMetric,
				Scope: schema.ScopeUserSandbox,
				Glob:  "verl-head.log",
				Regex: `acc/mean@1:(?P<value>[0-9.]+)`,
			},
		},
	})
	if err != nil {
		t.Fatalf("IngestArtifactDir(user-sandbox): %v", err)
	}
	if len(resUserOnly.StepEvents) != 0 || len(resUserOnly.TraceEvents) == 0 {
		t.Fatalf("expected 0 step events and >0 trace events for ScopeUserSandbox, got steps=%d traces=%d", len(resUserOnly.StepEvents), len(resUserOnly.TraceEvents))
	}
	if got := resUserOnly.CustomMetrics["val_acc"]; got.Count != 1 || got.P50 != 0.25 {
		t.Fatalf("unexpected val_acc plugin metric: %+v", got)
	}

	// Both rl and user-sandbox scopes: should ingest both and extract ValScore on Convergence curve.
	resBoth, err := IngestArtifactDir(IngestContext{
		DestDir: dir,
		Scopes:  []schema.TelemetryScope{schema.ScopeRL, schema.ScopeUserSandbox},
	})
	if err != nil {
		t.Fatalf("IngestArtifactDir(rl,user-sandbox): %v", err)
	}
	if len(resBoth.StepEvents) != 1 || len(resBoth.TraceEvents) == 0 {
		t.Fatalf("expected 1 step event and >0 trace events, got steps=%d traces=%d", len(resBoth.StepEvents), len(resBoth.TraceEvents))
	}
	rep, err := swebench.BuildReport(filepath.Join(dir, "events", "verl.jsonl"))
	if err != nil {
		t.Fatalf("BuildReport: %v", err)
	}
	if rep.Convergence == nil || len(rep.Convergence.Curve) != 1 || rep.Convergence.Curve[0].ValScore != 0.25 {
		t.Fatalf("expected ValScore=0.25 in convergence curve, got %+v", rep.Convergence)
	}
}
