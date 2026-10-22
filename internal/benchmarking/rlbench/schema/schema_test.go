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

package schema

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const (
	baselineEvalPath  = "/usr/local/google/home/jesschen/.gemini/jetski/brain/3b363e48-23dd-4b3e-9f78-46777067d670/scratch/baseline-eval.jsonl"
	baselineStepsPath = "/usr/local/google/home/jesschen/.gemini/jetski/brain/3b363e48-23dd-4b3e-9f78-46777067d670/scratch/baseline-steps.jsonl"
)

func openBaselineFile(t *testing.T, absPath, relName string) *os.File {
	t.Helper()
	f, err := os.Open(absPath)
	if err == nil {
		return f
	}
	fallback := filepath.Join("..", "..", "..", "..", "..", "..", relName)
	f, err2 := os.Open(fallback)
	if err2 == nil {
		return f
	}
	t.Fatalf("open baseline file %s (fallback %s): %v", absPath, fallback, err)
	return nil
}

func TestScenarioYAMLStrict(t *testing.T) {
	validYAML := []byte(`
schema_version: v1
name: swebench-multi-step
sut:
  endpoint: ate-env-api.default.svc.cluster.local:8080
  atespace: default
  base_template: swebench-base
  node_pool: worker-pool
  runtime_class: gvisor
  replicas: 2
  extra_args:
    log_level: info
jobs:
  - name: sim-loop-1
    kind: sim
    dataset: benchmark/swebench.jsonl
    trajectory: benchmark/traj.yaml
    mode: steps
    park: suspend-async
    images: 4
    tasks_per_image: 2
    rollouts: 2
    steps: 5
    width: 8
    step_pause: 2s
    prefetch: true
    seed: 42
    schedule_mode: sampled
run_window:
  steady_state_start_step: 2
  steady_state_end_step: 5
  warmup_steps: 1
  max_duration: 15m
overlays:
  - name: async-park-prefetch
    description: Enable async suspend and next-step prefetch
    park: suspend-async
    prefetch: true
    env_vars:
      ATE_PREFETCH: "1"
    flags:
      --enable-fast-restore: "true"
`)

	sc, err := DecodeYAMLStrict[Scenario](validYAML)
	if err != nil {
		t.Fatalf("DecodeYAMLStrict valid Scenario failed: %v", err)
	}
	if sc.Name != "swebench-multi-step" || len(sc.Jobs) != 1 || sc.Jobs[0].ScheduleMode != "sampled" {
		t.Fatalf("unexpected decoded Scenario: %+v", sc)
	}
	if len(sc.Overlays) != 1 || sc.Overlays[0].Prefetch == nil || !*sc.Overlays[0].Prefetch {
		t.Fatalf("unexpected decoded Overlays: %+v", sc.Overlays)
	}

	unknownFieldYAML := []byte(`
schema_version: v1
name: bad
unknown_top_field: true
sut: {}
jobs:
  - name: j1
    kind: sim
`)
	if _, err := DecodeYAMLStrict[Scenario](unknownFieldYAML); err == nil {
		t.Fatal("expected error for unknown YAML field, got nil")
	}

	missingJobsYAML := []byte(`
schema_version: v1
name: no-jobs
sut: {}
jobs: []
`)
	if _, err := DecodeYAMLStrict[Scenario](missingJobsYAML); err == nil {
		t.Fatal("expected error for Scenario with empty jobs, got nil")
	}
}

func TestScheduleJSONStrict(t *testing.T) {
	validJSON := `{
		"schema_version": "v1",
		"scenario_name": "swebench-multi-step",
		"seed": 42,
		"jobs": [
			{
				"job": "sim-loop-1",
				"steps": [
					{
						"step": 1,
						"step_pause": "1s",
						"placements": [
							{
								"env_id": "env-0",
								"instance_id": "astropy__astropy-12907",
								"image": "gcr.io/test/astropy:latest",
								"rollout": 0,
								"retire_after": false
							}
						],
						"prefetch_images": ["gcr.io/test/django:latest"]
					}
				]
			}
		]
	}`
	sched, err := DecodeJSONStrict[Schedule](strings.NewReader(validJSON))
	if err != nil {
		t.Fatalf("DecodeJSONStrict valid Schedule failed: %v", err)
	}
	if sched.ScenarioName != "swebench-multi-step" || len(sched.Jobs[0].Steps[0].Placements) != 1 {
		t.Fatalf("unexpected Schedule: %+v", sched)
	}

	unknownJSON := `{"schema_version":"v1","scenario_name":"s","jobs":[],"bogus":123}`
	if _, err := DecodeJSONStrict[Schedule](strings.NewReader(unknownJSON)); err == nil {
		t.Fatal("expected error for unknown JSON field in Schedule, got nil")
	}

	missingVersionJSON := `{"scenario_name":"s","jobs":[{"job":"j","steps":[{"step":1,"placements":[{"env_id":"e","instance_id":"i","image":"img"}]}]}]}`
	if _, err := DecodeJSONStrict[Schedule](strings.NewReader(missingVersionJSON)); err == nil {
		t.Fatal("expected error for missing schema_version in Schedule, got nil")
	}
}

func TestMeasurementsSystemAndStatsStrict(t *testing.T) {
	measJSON := `{
		"schema_version": "v1",
		"run_id": "run-123",
		"collected_at": "2026-10-22T01:25:00Z",
		"node_cache_before": [{"node": "node-1", "hits": 10, "misses": 2}],
		"node_cache_after": [{"node": "node-1", "hits": 25, "misses": 3}],
		"restores": [{"env": "env-1", "total_ms": 120.5, "cache_hit": true}],
		"prom_metrics": [{"name": "atelet_restore_total", "value": 15}]
	}`
	m, err := DecodeJSONStrict[Measurements](strings.NewReader(measJSON))
	if err != nil {
		t.Fatalf("DecodeJSONStrict Measurements failed: %v", err)
	}
	if len(m.NodeCacheAfter) != 1 || m.NodeCacheAfter[0].Hits != 25 {
		t.Fatalf("unexpected Measurements: %+v", m)
	}

	sysJSON := `{
		"schema_version": "v1",
		"cluster": "gke-rlbench-1",
		"kubernetes_version": "v1.32.1",
		"node_pools": {"default-pool": 3},
		"runtime_class": "gvisor",
		"git_sha": "abc1234"
	}`
	if _, err := DecodeJSONStrict[SystemDescription](strings.NewReader(sysJSON)); err != nil {
		t.Fatalf("DecodeJSONStrict SystemDescription failed: %v", err)
	}

	statsLine := `{"schema_version":"v1","run_id":"r1","scenario":"sc1","mode":"steps","valid":true,"tasks":4,"failed":0,"rewarded":4,"reward_rate":1.0,"step_wall_p50_ms":17691.8,"step_wall_p95_ms":17835.1,"ttfi_p50_ms":3937.4,"ttfi_p95_ms":5937.9,"wake_p50_ms":3685.5,"wake_p95_ms":5688.6,"suspend_p50_ms":114.5,"suspend_p95_ms":127.4,"cache_hit_rate":0.95,"orphan_envs":0,"cpu_core_seconds":42.5,"gib_seconds":128.0}`
	recs, err := DecodeJSONLStrict[StatsRecord](strings.NewReader(statsLine + "\n"))
	if err != nil || len(recs) != 1 {
		t.Fatalf("DecodeJSONLStrict StatsRecord failed: %v (len=%d)", err, len(recs))
	}
	if recs[0].RewardRate != 1.0 || recs[0].CacheHitRate != 0.95 {
		t.Fatalf("unexpected StatsRecord: %+v", recs[0])
	}

	invalidStatsLine := `{"schema_version":"v1","run_id":"r1","scenario":"sc1","mode":"steps","valid":false}`
	if _, err := DecodeJSONLStrict[StatsRecord](strings.NewReader(invalidStatsLine)); err == nil {
		t.Fatal("expected validation failure when valid=false without invalid_reasons")
	}
}

func TestBaselineLiveClusterFilesDecodeCleanly(t *testing.T) {
	evalFile := openBaselineFile(t, baselineEvalPath, "baseline-eval.jsonl")
	defer evalFile.Close()

	evalEvents, err := DecodeJSONLStrict[Event](evalFile)
	if err != nil {
		t.Fatalf("DecodeJSONLStrict on baseline-eval.jsonl failed: %v", err)
	}
	if len(evalEvents) != 29 {
		t.Fatalf("baseline-eval.jsonl event count = %d, want 29", len(evalEvents))
	}
	// Verify field preservation across create, step (grade with exit/reward/tail), and run (with count).
	first := evalEvents[0]
	if first.Run != "swe-base-eval3" || first.Event != "create" || first.Env != "swe-base-eval3-astropy--astropy-12907-0" || !first.OK {
		t.Fatalf("unexpected first eval event: %+v", first)
	}
	gradeEv := evalEvents[19]
	if gradeEv.Event != "step" || gradeEv.Name != "grade" || gradeEv.Reward == nil || *gradeEv.Reward != 1 || gradeEv.Exit == nil || *gradeEv.Exit != 0 || gradeEv.Tail == "" {
		t.Fatalf("grade event fields not preserved: %+v", gradeEv)
	}
	lastEval := evalEvents[len(evalEvents)-1]
	if lastEval.Event != "run" || lastEval.Name != "eval" || lastEval.Count != 2 {
		t.Fatalf("run event fields not preserved: %+v", lastEval)
	}

	stepsFile := openBaselineFile(t, baselineStepsPath, "baseline-steps.jsonl")
	defer stepsFile.Close()

	stepEvents, err := DecodeJSONLStrict[Event](stepsFile)
	if err != nil {
		t.Fatalf("DecodeJSONLStrict on baseline-steps.jsonl failed: %v", err)
	}
	if len(stepEvents) != 58 {
		t.Fatalf("baseline-steps.jsonl event count = %d, want 58", len(stepEvents))
	}
	barrierEv := stepEvents[26]
	if barrierEv.Event != "step_barrier" || barrierEv.Step != 1 || barrierEv.Count != 2 {
		t.Fatalf("step_barrier event fields not preserved: %+v", barrierEv)
	}
	wakeEv := stepEvents[30]
	if wakeEv.Event != "wake" || wakeEv.Step != 2 || wakeEv.Task != 1 || wakeEv.Attempts != 1 {
		t.Fatalf("wake event fields not preserved: %+v", wakeEv)
	}
}
