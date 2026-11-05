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

package swebench

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"github.com/agent-substrate/env/internal/benchmarking/schema"
)

// An environment leaves the warm pool once: at its claim, or at its delete
// when nothing claimed it. A delete after a claim, and the delete of an
// environment that was never warm, must not lower the pool a concurrent
// prefetch is filling.
func TestWarmPoolStatsCountsAnEnvironmentOutOnce(t *testing.T) {
	var ws warmPoolStats
	a, b, c := &environment{id: "a"}, &environment{id: "b"}, &environment{id: "c"}
	ws.warmed(a)
	ws.warmed(b)
	if ws.cur != 2 || ws.peak != 2 {
		t.Fatalf("after two warms: cur=%d peak=%d, want 2 and 2", ws.cur, ws.peak)
	}
	ws.claimed(a) // a leaves the pool for its replay
	ws.retire(a)  // its delete must not count it out again
	ws.retire(c)  // c was never warm
	if ws.cur != 1 {
		t.Fatalf("after a claim and two deletes: cur=%d, want 1 (b is still warm)", ws.cur)
	}
	ws.warmed(c) // a prefetch beside b: the pool is 2 again
	if ws.cur != 2 || ws.peak != 2 {
		t.Fatalf("after the prefetch: cur=%d peak=%d, want 2 and 2", ws.cur, ws.peak)
	}
	ws.claimed(b)
	ws.claimed(c)
	ws.retire(b)
	ws.retire(c)
	s := ws.snapshot()
	if ws.cur != 0 || s.Hits != 3 || s.Created != 3 || s.Retired != 4 || s.StandingPoolSize != 2 {
		t.Fatalf("final: cur=%d stats=%+v, want cur 0, 3 hits, 3 created, 4 retired, pool 2", ws.cur, s)
	}
}

// A delete that fails after the replay leaves the environment with its
// handle; the window's cleanup deletes it. It is retired once, and the
// standing pool counts the suspends, not the deletes.
func TestStrategyRetiresAFailedDeleteOnce(t *testing.T) {
	f := newFake()
	var out bytes.Buffer
	cfg := baseConfig(t, f, &out)
	cfg.Mode = ModeSteps
	cfg.Strategy = StrategyNaive
	cfg.Rollouts = 1
	cfg.Steps = 1
	cfg.Park = ParkSuspend
	first := EnvID(cfg.RunID, cfg.Instances[0].InstanceID, 0)
	f.failDelete[first] = 1

	sum, err := Run(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	if sum.Tasks != 3 || sum.Failed != 0 {
		t.Fatalf("tasks=%d failed=%d, want 3 and 0", sum.Tasks, sum.Failed)
	}
	if sum.WorkingSetHits != 3 || sum.WorkingSetMisses != 0 || sum.StandingPoolSize != 3 ||
		sum.Created != 3 || sum.Reused != 3 || sum.Retired != 3 {
		t.Fatalf("working set: %+v, want 3 hits, 0 misses, pool 3, 3 created, 3 reused, 3 retired", sum)
	}
	if f.deleted[first] != 1 || f.deletedByID[first] != 0 {
		t.Fatalf("%s: deleted %d times by handle and %d by name, want 1 and 0", first, f.deleted[first], f.deletedByID[first])
	}
	if n := f.awake.Load(); n != 0 {
		t.Fatalf("%d environments still awake after the run", n)
	}
	events := parseEvents(t, out.String())
	var deletes, failedDeletes int
	for _, e := range events {
		if e.Event == EventDelete && e.Env == first {
			deletes++
			if !e.OK {
				failedDeletes++
			}
		}
	}
	if deletes != 2 || failedDeletes != 1 {
		t.Fatalf("%s: %d delete events of which %d failed, want 2 and 1", first, deletes, failedDeletes)
	}
}

// A strategy parks by suspend (delete for none) and runs one pass; the step
// loop's settings are refused rather than recorded on a run that ignored
// them. ValidateConfig makes the checks without a backend, for -dry-run.
func TestValidateConfigRejectsStepLoopSettingsWithAStrategy(t *testing.T) {
	base := func() Config {
		cfg := baseConfig(t, nil, nil)
		cfg.Mode = ModeSteps
		cfg.Strategy = StrategyNaive
		cfg.Rollouts = 1
		cfg.Steps = 1
		cfg.Park = ParkSuspend
		return cfg
	}
	if err := ValidateConfig(base()); err != nil {
		t.Fatalf("ValidateConfig(naive, suspend) = %v, want nil without a backend", err)
	}
	for name, edit := range map[string]func(*Config){
		"park delete with naive":  func(c *Config) { c.Park = ParkDelete },
		"park suspend with none":  func(c *Config) { c.Strategy = StrategyNone },
		"step pause":              func(c *Config) { c.StepPause = time.Second },
		"park each":               func(c *Config) { c.ParkEach = true },
		"images per step":         func(c *Config) { c.ImagesPerStep = 1 },
		"prefetch":                func(c *Config) { c.Prefetch = true },
		"schedule":                func(c *Config) { c.Schedule = &schema.Schedule{} },
		"loader label key":        func(c *Config) { c.Labels = map[string]string{"park": "x"} },
		"two steps":               func(c *Config) { c.Steps = 2 },
		"window size with naive":  func(c *Config) { c.WindowSize = 2 },
		"strategy with open-loop": func(c *Config) { c.Mode = ModeOpenLoop; c.ArrivalRate = 1 },
	} {
		cfg := base()
		edit(&cfg)
		if err := ValidateConfig(cfg); err == nil {
			t.Errorf("%s: ValidateConfig() = nil, want an error", name)
		}
	}
	for name, edit := range map[string]func(*Config){
		"none parks by delete":            func(c *Config) { c.Strategy = StrategyNone; c.Park = ParkDelete },
		"eval mode reads no park":         func(c *Config) { c.Mode = ModeEval; c.TasksPerImage = 2; c.Park = ParkDelete },
		"sliding with a window":           func(c *Config) { c.Strategy = StrategySliding; c.WindowSize = 2 },
		"an empty park takes the implied": func(c *Config) { c.Park = "" },
	} {
		cfg := base()
		edit(&cfg)
		if err := ValidateConfig(cfg); err != nil {
			t.Errorf("%s: ValidateConfig() = %v, want nil", name, err)
		}
	}

	// The name check -dry-run relies on: 20 + 1 + 36 + 1 + 6 = 64 characters.
	cfg := base()
	cfg.RunID = strings.Repeat("r", maxRunIDLen)
	cfg.Instances = []Instance{{InstanceID: strings.Repeat("a", 40), Image: "reg/img@" + goodDigest, Patch: "P", EvalScript: "E"}}
	cfg.Rollouts = 100001
	if err := ValidateConfig(cfg); err == nil || !strings.Contains(err.Error(), "more than Substrate's 63") {
		t.Fatalf("ValidateConfig(64-character name) = %v, want it refused", err)
	}
	cfg.Rollouts = 10
	if err := ValidateConfig(cfg); err != nil {
		t.Fatalf("ValidateConfig(58-character name) = %v, want nil", err)
	}
}
