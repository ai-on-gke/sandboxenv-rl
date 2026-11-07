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

package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/agent-substrate/env/internal/benchmarking/schema"
)

// A train log the way a wrapper might produce it: a banner echoing profile
// defaults first, then the launcher's CLI overrides, then the trainer's
// resolved config dump. Only the last of these is what actually ran.
const bannerThenResolvedLog = `[driver] profile defaults: data.train_batch_size=1024 actor_rollout_ref.rollout.n=8 max_concurrent_sessions=512
python -m verl.trainer.main_ppo data.train_batch_size=64 actor_rollout_ref.rollout.n=32 trainer.total_training_steps=40
(TaskRunner pid=1) {'data': {'train_batch_size': 64, 'train_files': 'x.parquet'}, 'actor_rollout_ref': {'rollout': {'rollout_n': 32}}, 'max_concurrent_sessions': 256}
(TaskRunner pid=1) step:1 - timing_s/step:12.0 - timing_s/gen:8.0 - critic/score/mean:0.5
`

func TestTrainerLoadParamPrefersResolvedThenLastCLI(t *testing.T) {
	if got := trainerLoadParam(bannerThenResolvedLog, trainBatchResolvedRe, trainBatchCLIRe); got != 64 {
		t.Fatalf("train_batch_size = %d, want 64 (resolved config), not the banner's 1024", got)
	}
	if got := trainerLoadParam(bannerThenResolvedLog, rolloutNResolvedRe, rolloutNCLIRe); got != 32 {
		t.Fatalf("rollout_n = %d, want 32", got)
	}
	if got := trainerLoadParam(bannerThenResolvedLog, maxSessionsRsvRe, maxSessionsCLIRe); got != 256 {
		t.Fatalf("max_concurrent_sessions = %d, want resolved 256", got)
	}
	// Without a resolved dump, the last CLI occurrence wins over the banner.
	cliOnly := "[driver] data.train_batch_size=1024\npython main.py data.train_batch_size=16\n"
	if got := trainerLoadParam(cliOnly, trainBatchResolvedRe, trainBatchCLIRe); got != 16 {
		t.Fatalf("CLI-only train_batch_size = %d, want last match 16", got)
	}
	if got := trainerLoadParam("no params here", trainBatchResolvedRe, trainBatchCLIRe); got != 0 {
		t.Fatalf("absent param = %d, want 0", got)
	}
}

func TestEnrichSystemDescriptionUsesEffectiveLoadParams(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "train.log"), []byte(bannerThenResolvedLog), 0o644); err != nil {
		t.Fatal(err)
	}
	var sys schema.SystemDescription
	enrichSystemDescriptionFromArtifactDir(dir, &sys, nil, nil)
	if sys.Load == nil {
		t.Fatal("expected Load from train.log")
	}
	if sys.Load.TasksPerStep != 64 || sys.Load.GroupSize != 32 || sys.Load.Steps != 40 || sys.Load.MaxConcurrent != 256 {
		t.Fatalf("Load = %+v, want tasks_per_step=64 group=32 steps=40 max_conc=256", sys.Load)
	}
	if sys.Load.TrajectoriesPerStep != 64*32 {
		t.Fatalf("TrajectoriesPerStep = %d, want %d", sys.Load.TrajectoriesPerStep, 64*32)
	}
}

func TestDeclaredLoadParamsOverrideSniffedValues(t *testing.T) {
	scen := &schema.Scenario{
		Jobs: []schema.JobSpec{
			{Name: "sim", Mode: schema.JobModeClosedLoop, TasksPerStep: 999}, // not a real job: ignored
			{Name: "job-0", Kind: "real", RealMode: "trace", TasksPerStep: 16, GroupSize: 8, Steps: 50, MaxConcurrent: 128},
		},
	}
	declared := declaredLoadParams(scen)
	if declared == nil || declared.TasksPerStep != 16 || declared.GroupSize != 8 || declared.Steps != 50 || declared.MaxConcurrent != 128 {
		t.Fatalf("declaredLoadParams = %+v", declared)
	}

	// Sniffed values from the log are replaced by what the scenario asked for;
	// fields the scenario leaves unset keep the sniffed value.
	sys := schema.SystemDescription{Load: &schema.LoadParams{
		Tasks: 2048, TasksPerStep: 64, GroupSize: 32, Steps: 40, MaxConcurrent: 256, DistinctImages: 7, Repeat: 1,
	}}
	applyDeclaredLoad(&sys, declared)
	lp := sys.Load
	if lp.TasksPerStep != 16 || lp.GroupSize != 8 || lp.Steps != 50 || lp.MaxConcurrent != 128 {
		t.Fatalf("declared values must win: %+v", lp)
	}
	if lp.Tasks != 2048 || lp.DistinctImages != 7 {
		t.Fatalf("unset declared fields must keep sniffed values: %+v", lp)
	}
	if lp.TrajectoriesPerStep != 16*8 {
		t.Fatalf("TrajectoriesPerStep = %d, want %d", lp.TrajectoriesPerStep, 16*8)
	}

	// No Load yet: declared values create it.
	var fresh schema.SystemDescription
	applyDeclaredLoad(&fresh, declared)
	if fresh.Load == nil || fresh.Load.TasksPerStep != 16 || fresh.Load.Repeat != 1 {
		t.Fatalf("fresh Load = %+v", fresh.Load)
	}

	// Scenarios with no real job, or a real job declaring nothing, change nothing.
	if declaredLoadParams(&schema.Scenario{Jobs: []schema.JobSpec{{Name: "sim", Mode: schema.JobModeClosedLoop, TasksPerStep: 4}}}) != nil {
		t.Fatal("simulated-only scenario must not declare load")
	}
	if declaredLoadParams(&schema.Scenario{Jobs: []schema.JobSpec{{Name: "r", Kind: "real", RealMode: "measure"}}}) != nil {
		t.Fatal("real job without load fields must not declare load")
	}
	before := *sys.Load
	applyDeclaredLoad(&sys, nil)
	if *sys.Load != before {
		t.Fatal("nil declared must be a no-op")
	}
}
