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

package trace

import (
	"bytes"
	"context"
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/agent-substrate/env/internal/benchmarking/schema"
	"github.com/agent-substrate/env/internal/benchmarking/swebench"
)

type chainFakeSandbox struct {
	id        string
	image     string
	backend   *chainFakeBackend
	lastWrite time.Time
}

func (s *chainFakeSandbox) WriteFile(_ context.Context, path string, content []byte, _ fs.FileMode) error {
	now := time.Now()
	s.backend.mu.Lock()
	s.lastWrite = now
	s.backend.writeSizes = append(s.backend.writeSizes, len(content))
	s.backend.writePaths = append(s.backend.writePaths, path)
	s.backend.mu.Unlock()
	return nil
}

func (s *chainFakeSandbox) Run(_ context.Context, command string, _ time.Duration) (swebench.Result, error) {
	now := time.Now()
	s.backend.mu.Lock()
	if !s.lastWrite.IsZero() {
		s.backend.observedThinks = append(s.backend.observedThinks, now.Sub(s.lastWrite))
	}
	s.backend.commands = append(s.backend.commands, command)
	s.backend.mu.Unlock()
	return swebench.Result{ExitCode: 0, Stdout: "1 passed\n"}, nil
}

func (s *chainFakeSandbox) Suspend(_ context.Context) error {
	s.backend.mu.Lock()
	s.backend.suspends++
	s.backend.mu.Unlock()
	return nil
}

func (s *chainFakeSandbox) Delete(_ context.Context) error {
	return s.backend.Delete(context.Background(), s.id)
}

type chainFakeBackend struct {
	mu             sync.Mutex
	live           map[string]*chainFakeSandbox
	writeSizes     []int
	writePaths     []string
	commands       []string
	observedThinks []time.Duration
	suspends       int
}

func newChainFakeBackend() *chainFakeBackend {
	return &chainFakeBackend{live: make(map[string]*chainFakeSandbox)}
}

func (b *chainFakeBackend) Create(_ context.Context, id, image string) (swebench.Sandbox, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	sb := &chainFakeSandbox{id: id, image: image, backend: b}
	b.live[id] = sb
	return sb, nil
}

func (b *chainFakeBackend) Delete(_ context.Context, id string) error {
	b.mu.Lock()
	delete(b.live, id)
	b.mu.Unlock()
	return nil
}

// TestTraceToScheduleToWorkingSetChain verifies the end-to-end chain:
// trace.jsonl -> ConvertTrace -> disk artifacts (schedule.json + placement.json +
// trajectory.yaml + instances.jsonl) -> swebench.ReadSchedule & LoadTrajectory
// -> swebench.Run over an in-process fake backend preserves step counts,
// working-set reuse, payload sizes, intra-trajectory think times, and
// inter-step trainer pauses.
func TestTraceToScheduleToWorkingSetChain(t *testing.T) {
	t0 := time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC)
	exitZero := 0
	ms := func(d int) time.Time {
		return t0.Add(time.Duration(d) * time.Millisecond)
	}

	// Step 1 runs from t0+0ms to t0+50ms:
	//   - env-1 and env-2 each: create (5ms), write_file (5ms, 64B), think (25ms), exec (10ms).
	//   - env-1 suspends (5ms) and survives to step 2; env-2 deletes (5ms) and retires.
	// Trainer step pause of 30ms between t0+50ms and t0+80ms.
	// Step 2 runs from t0+80ms to t0+130ms:
	//   - env-1 is reused from working set; env-3 is newly created (5ms).
	//   - both run write_file (5ms, 64B), think (25ms), exec (10ms), delete (5ms).
	rawEvents := []schema.TraceEvent{
		// Step 1: env-1
		{SchemaVersion: schema.SchemaVersion, TS: ms(5), Job: "grpo", Step: 1, Env: "env-1", Op: "create", DurMS: 5, OK: true},
		{SchemaVersion: schema.SchemaVersion, TS: ms(10), Job: "grpo", Step: 1, Env: "env-1", Op: "write_file", Path: "/workspace/patch.diff", Bytes: 64, DurMS: 5, OK: true},
		{SchemaVersion: schema.SchemaVersion, TS: ms(45), Job: "grpo", Step: 1, Env: "env-1", Op: "exec", Command: []string{"sh", "-c", "pytest -q"}, DurMS: 10, Exit: &exitZero, OK: true},
		{SchemaVersion: schema.SchemaVersion, TS: ms(50), Job: "grpo", Step: 1, Env: "env-1", Op: "suspend", DurMS: 5, OK: true},
		// Step 1: env-2
		{SchemaVersion: schema.SchemaVersion, TS: ms(5), Job: "grpo", Step: 1, Env: "env-2", Op: "create", DurMS: 5, OK: true},
		{SchemaVersion: schema.SchemaVersion, TS: ms(10), Job: "grpo", Step: 1, Env: "env-2", Op: "write_file", Path: "/workspace/patch.diff", Bytes: 64, DurMS: 5, OK: true},
		{SchemaVersion: schema.SchemaVersion, TS: ms(45), Job: "grpo", Step: 1, Env: "env-2", Op: "exec", Command: []string{"sh", "-c", "pytest -q"}, DurMS: 10, Exit: &exitZero, OK: true},
		{SchemaVersion: schema.SchemaVersion, TS: ms(50), Job: "grpo", Step: 1, Env: "env-2", Op: "delete", DurMS: 5, OK: true},
		// Step 2 (starts at t0+80ms after 30ms trainer pause): env-1 (reused)
		{SchemaVersion: schema.SchemaVersion, TS: ms(85), Job: "grpo", Step: 2, Env: "env-1", Op: "write_file", Path: "/workspace/patch.diff", Bytes: 64, DurMS: 5, OK: true},
		{SchemaVersion: schema.SchemaVersion, TS: ms(120), Job: "grpo", Step: 2, Env: "env-1", Op: "exec", Command: []string{"sh", "-c", "pytest -q"}, DurMS: 10, Exit: &exitZero, OK: true},
		{SchemaVersion: schema.SchemaVersion, TS: ms(125), Job: "grpo", Step: 2, Env: "env-1", Op: "delete", DurMS: 5, OK: true},
		// Step 2: env-3 (fresh)
		{SchemaVersion: schema.SchemaVersion, TS: ms(85), Job: "grpo", Step: 2, Env: "env-3", Op: "create", DurMS: 5, OK: true},
		{SchemaVersion: schema.SchemaVersion, TS: ms(90), Job: "grpo", Step: 2, Env: "env-3", Op: "write_file", Path: "/workspace/patch.diff", Bytes: 64, DurMS: 5, OK: true},
		{SchemaVersion: schema.SchemaVersion, TS: ms(125), Job: "grpo", Step: 2, Env: "env-3", Op: "exec", Command: []string{"sh", "-c", "pytest -q"}, DurMS: 10, Exit: &exitZero, OK: true},
		{SchemaVersion: schema.SchemaVersion, TS: ms(130), Job: "grpo", Step: 2, Env: "env-3", Op: "delete", DurMS: 5, OK: true},
	}

	var traceBuf bytes.Buffer
	enc := json.NewEncoder(&traceBuf)
	for _, ev := range rawEvents {
		if err := enc.Encode(ev); err != nil {
			t.Fatalf("encode trace event: %v", err)
		}
	}

	conv, err := ConvertTrace(&traceBuf, ConvertOptions{
		ScenarioName: "chain-scenario",
		DefaultJob:   "grpo",
	})
	if err != nil {
		t.Fatalf("ConvertTrace failed: %v", err)
	}

	// Write artifacts to disk and reload via file-only boundaries.
	artDir := t.TempDir()
	if err := WriteConvertedArtifacts(artDir, conv); err != nil {
		t.Fatalf("WriteConvertedArtifacts failed: %v", err)
	}

	diskSched, err := swebench.ReadSchedule(filepath.Join(artDir, "schedule.json"))
	if err != nil {
		t.Fatalf("ReadSchedule failed: %v", err)
	}
	diskTraj, err := swebench.LoadTrajectory(filepath.Join(artDir, "trajectory.yaml"))
	if err != nil {
		t.Fatalf("LoadTrajectory failed: %v", err)
	}
	diskInstances, err := swebench.LoadDataset(filepath.Join(artDir, "instances.jsonl"))
	if err != nil {
		t.Fatalf("LoadDataset(instances.jsonl) failed: %v", err)
	}

	rawPlace, err := os.ReadFile(filepath.Join(artDir, "placement.json"))
	if err != nil {
		t.Fatalf("read placement.json: %v", err)
	}
	diskPlacement, err := schema.DecodeJSONStrict[schema.PlacementPlan](bytes.NewReader(rawPlace))
	if err != nil {
		t.Fatalf("DecodeJSONStrict placement.json: %v", err)
	}

	// Pass through swebench.ResolveSchedule in explicit mode.
	resolvedSched, err := swebench.ResolveSchedule(swebench.ResolveScheduleOptions{
		ScenarioName: "chain-scenario",
		Jobs: []swebench.JobScheduleSpec{
			{
				JobID:       "grpo",
				Source:      swebench.ScheduleSourceExplicit,
				ExplicitJob: &diskSched.Jobs[0],
			},
		},
	})
	if err != nil {
		t.Fatalf("swebench.ResolveSchedule explicit failed: %v", err)
	}

	if len(resolvedSched.Jobs) != 1 || len(resolvedSched.Jobs[0].Steps) != 2 {
		t.Fatalf("expected 1 job with 2 steps, got %+v", resolvedSched.Jobs)
	}
	step1 := resolvedSched.Jobs[0].Steps[0]
	if step1.StepPauseMS != 30 {
		t.Fatalf("expected step 1 step_pause_ms 30, got %v", step1.StepPauseMS)
	}
	if len(diskPlacement.Jobs) != 1 || len(diskPlacement.Jobs[0].Assignments) != 4 {
		t.Fatalf("expected 4 placement assignments, got %+v", diskPlacement.Jobs)
	}
	if !diskPlacement.Jobs[0].Assignments[1].RetireAfter {
		t.Fatalf("expected env-2 to retire after step 1, got %+v", diskPlacement.Jobs[0].Assignments[1])
	}
	if diskPlacement.Jobs[0].Assignments[2].ReuseEnvID != "env-1" {
		t.Fatalf("expected env-1 to be reused in step 2, got %+v", diskPlacement.Jobs[0].Assignments[2])
	}
	if len(diskTraj.Steps) != 2 || time.Duration(diskTraj.Steps[1].Think) != 25*time.Millisecond {
		t.Fatalf("expected 2 trajectory steps with 25ms think time on step 2, got %+v", diskTraj.Steps)
	}

	// Replay through swebench.Run over the in-process fake backend.
	backend := newChainFakeBackend()
	eventsPath := filepath.Join(artDir, "events.jsonl")
	ef, err := os.Create(eventsPath)
	if err != nil {
		t.Fatalf("create events.jsonl: %v", err)
	}
	rec := swebench.NewRecorder(ef)
	rec.SetWorker("worker-0")
	rec.SetJob("grpo")

	sum, err := swebench.Run(context.Background(), swebench.Config{
		RunID:      "chain-run",
		Backend:    backend,
		Instances:  diskInstances,
		Trajectory: diskTraj,
		Recorder:   rec,
		Mode:       swebench.ModeSteps,
		Width:      4,
		Rollouts:   1,
		Steps:      2,
		Park:       swebench.ParkSuspendAsync,
		Schedule:   resolvedSched,
	})
	_ = rec.Close()
	_ = ef.Close()
	if err != nil {
		t.Fatalf("swebench.Run failed: %v", err)
	}
	if sum.Tasks != 4 || sum.Failed != 0 || sum.Rewarded != 4 {
		t.Fatalf("unexpected Run summary: %+v", sum)
	}
	if sum.Created != 3 || sum.Reused != 1 || sum.WorkingSetHits != 1 {
		t.Fatalf("unexpected working-set reuse/churn counts: created=%d reused=%d hits=%d", sum.Created, sum.Reused, sum.WorkingSetHits)
	}
	if len(backend.writeSizes) != 4 || backend.writeSizes[0] != 64 {
		t.Fatalf("expected 4 writes of 64 bytes, got %v", backend.writeSizes)
	}
	if len(backend.commands) != 4 || backend.commands[0] != "pytest -q" {
		t.Fatalf("expected 4 runs of \"pytest -q\", got %v", backend.commands)
	}
	for i, th := range backend.observedThinks {
		if th < 20*time.Millisecond {
			t.Fatalf("rollout %d observed think time %v < 20ms (expected ~25ms)", i, th)
		}
	}
	// Total wall time must include 2 steps * 25ms think + 30ms inter-step pause >= 75ms.
	if sum.Duration < 75*time.Millisecond {
		t.Fatalf("expected total run duration >= 75ms (2x25ms think + 30ms step_pause), got %v", sum.Duration)
	}

	// Verify emitted events shard strictly validates against schema.Event.
	shardBytes, err := os.ReadFile(eventsPath)
	if err != nil {
		t.Fatalf("read events.jsonl: %v", err)
	}
	if err := schema.ValidateJSONLAgainstJSONSchema("event.schema.json", bytes.NewReader(shardBytes)); err != nil {
		t.Fatalf("event.schema.json validation failed: %v", err)
	}
	emitted, err := schema.DecodeJSONLStrict[schema.Event](bytes.NewReader(shardBytes))
	if err != nil {
		t.Fatalf("strict decode emitted events failed: %v", err)
	}
	if len(emitted) == 0 {
		t.Fatal("expected emitted events in shard")
	}
}

func TestConvertTraceRejectsUnknownFieldsAndEmpty(t *testing.T) {
	if _, err := ConvertTrace(strings.NewReader(""), ConvertOptions{}); err == nil {
		t.Fatal("expected error on empty trace")
	}
	bad := `{"schema_version":"v1","ts":"2026-10-22T12:00:00Z","env":"e1","op":"shell","dur_ms":5,"ok":true,"unknown_field":123}` + "\n"
	if _, err := ConvertTrace(strings.NewReader(bad), ConvertOptions{}); err == nil {
		t.Fatal("expected DisallowUnknownFields error on unknown field in trace.jsonl")
	}
}
