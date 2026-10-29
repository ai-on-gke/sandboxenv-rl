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

package overlay

import (
	"strings"
	"testing"

	"github.com/agent-substrate/env/internal/benchmarking/schema"
	"github.com/agent-substrate/env/internal/benchmarking/swebench"
)

func TestPresetOverlays(t *testing.T) {
	for _, name := range PresetNames {
		ov, err := PresetOverlay(name)
		if err != nil {
			t.Fatalf("PresetOverlay(%q): %v", name, err)
		}
		if ov.Name != name {
			t.Errorf("PresetOverlay(%q).Name = %q", name, ov.Name)
		}
	}
	if _, err := PresetOverlay("nonexistent"); err == nil {
		t.Fatal("expected error for unknown preset")
	}
}

func TestCompareOverlayPairAndRender(t *testing.T) {
	sched := &schema.Schedule{
		SchemaVersion: schema.VersionV1,
		Scenario:      "test-scen",
		Seed:          7,
		Jobs: []schema.ScheduledJob{
			{
				Job: "j1",
				Steps: []schema.ScheduledStep{
					{Step: 1, Tasks: []schema.ScheduledTask{{TaskID: "t1", InstanceID: "i1", Image: "img"}}},
				},
			},
		},
	}

	r1 := 1
	repA := swebench.Build([]swebench.Event{
		{RunID: "a", Event: swebench.EventCreate, DurMS: 200, OK: true, Source: string(schema.SourceColdStart)},
		{RunID: "a", Event: swebench.EventTTFI, DurMS: 500, OK: true},
		{RunID: "a", Event: swebench.EventTask, Step: 1, DurMS: 600, OK: true, Reward: &r1},
		{RunID: "a", Event: swebench.EventRun, Name: "steps", DurMS: 1000, OK: true, Count: 1},
	})
	repB := swebench.Build([]swebench.Event{
		{RunID: "b", Event: swebench.EventCreate, DurMS: 50, OK: true, Source: string(schema.SourceWarmPool)},
		{RunID: "b", Event: swebench.EventTTFI, DurMS: 120, OK: true},
		{RunID: "b", Event: swebench.EventTask, Step: 1, DurMS: 250, OK: true, Reward: &r1},
		{RunID: "b", Event: swebench.EventRun, Name: "steps", DurMS: 500, OK: true, Count: 1},
	})

	cmp, err := CompareOverlayPair("pool-off", repA, sched, "pool-on", repB, sched)
	if err != nil {
		t.Fatalf("CompareOverlayPair: %v", err)
	}
	if !cmp.ScheduleMatch {
		t.Fatal("expected ScheduleMatch=true")
	}
	md := RenderComparisonMarkdown(cmp)
	for _, want := range []string{"pool-off", "pool-on", "create_p50_ms", "warm_pool", "cold_start"} {
		if !strings.Contains(md, want) {
			t.Errorf("markdown missing %q:\n%s", want, md)
		}
	}

	schedDiff := *sched
	schedDiff.Seed = 99
	if _, err := CompareOverlayPair("pool-off", repA, sched, "pool-on", repB, &schedDiff); err == nil {
		t.Fatal("expected error when schedules differ")
	}
}
