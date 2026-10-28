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

// Package overlay provides feature overlay presets and A/B comparison helpers
// for benchmarking runs across warm pools, snapshot modes, and cold starts.
package overlay

import (
	"fmt"
	"math"
	"reflect"
	"strings"

	"github.com/agent-substrate/env/internal/benchmarking/schema"
	"github.com/agent-substrate/env/internal/benchmarking/swebench"
)

// PresetNames lists the built-in feature overlay presets.
var PresetNames = []string{
	"pool-off",
	"pool-on",
	"snapshot-sync",
	"snapshot-async",
	"cold-only",
	"warm-pool",
}

// PresetOverlay returns the built-in FeatureOverlay for the given preset name.
func PresetOverlay(name string) (*schema.FeatureOverlay, error) {
	switch name {
	case "pool-off":
		return &schema.FeatureOverlay{
			Name:          "pool-off",
			WarmPoolSize:  0,
			SnapshotMode:  "sync",
			PrewarmPolicy: "none",
		}, nil
	case "pool-on":
		return &schema.FeatureOverlay{
			Name:          "pool-on",
			WarmPoolSize:  32,
			SnapshotMode:  "sync",
			PrewarmPolicy: "warm",
		}, nil
	case "snapshot-sync":
		return &schema.FeatureOverlay{
			Name:          "snapshot-sync",
			WarmPoolSize:  16,
			SnapshotMode:  "sync",
			PrewarmPolicy: "warm",
		}, nil
	case "snapshot-async":
		return &schema.FeatureOverlay{
			Name:          "snapshot-async",
			WarmPoolSize:  16,
			SnapshotMode:  "async",
			PrewarmPolicy: "warm",
		}, nil
	case "cold-only":
		return &schema.FeatureOverlay{
			Name:          "cold-only",
			WarmPoolSize:  0,
			SnapshotMode:  "none",
			PrewarmPolicy: "cold",
		}, nil
	case "warm-pool":
		return &schema.FeatureOverlay{
			Name:          "warm-pool",
			WarmPoolSize:  32,
			SnapshotMode:  "async",
			PrewarmPolicy: "warm",
		}, nil
	default:
		return nil, fmt.Errorf("unknown overlay preset %q (available: %s)", name, strings.Join(PresetNames, ", "))
	}
}

// ResolveOverlay looks up name in scenarioOverlays first, falling back to built-in presets.
func ResolveOverlay(name string, scenarioOverlays map[string]schema.FeatureOverlay) (*schema.FeatureOverlay, error) {
	if scenarioOverlays != nil {
		if ov, ok := scenarioOverlays[name]; ok {
			cp := ov
			if cp.Name == "" {
				cp.Name = name
			}
			return &cp, nil
		}
	}
	return PresetOverlay(name)
}

// OverlayMetricDiff captures a metric comparison between overlay A and overlay B.
type OverlayMetricDiff struct {
	Metric   string  `json:"metric"`
	ValueA   float64 `json:"value_a"`
	ValueB   float64 `json:"value_b"`
	Delta    float64 `json:"delta"`
	DeltaPct float64 `json:"delta_pct"`
}

// OverlayPairComparison holds the side-by-side comparison of two overlay runs.
type OverlayPairComparison struct {
	OverlayA      string              `json:"overlay_a"`
	OverlayB      string              `json:"overlay_b"`
	ScheduleMatch bool                `json:"schedule_match"`
	ValidA        bool                `json:"valid_a"`
	ValidB        bool                `json:"valid_b"`
	Metrics       []OverlayMetricDiff `json:"metrics"`
	SourcesA      map[string]int      `json:"sources_a,omitempty"`
	SourcesB      map[string]int      `json:"sources_b,omitempty"`
}

// CompareOverlayPair validates that two runs used the identical resolved schedule
// and computes side-by-side diffs across create, ttfi, wake, throughput, reward rate,
// and sandbox acquisition sources.
func CompareOverlayPair(
	nameA string, repA *swebench.Report, schedA *schema.Schedule,
	nameB string, repB *swebench.Report, schedB *schema.Schedule,
) (*OverlayPairComparison, error) {
	if repA == nil || repB == nil {
		return nil, fmt.Errorf("both overlay reports must be non-nil")
	}
	schedMatch := schedulesEqual(schedA, schedB)
	if !schedMatch {
		return nil, fmt.Errorf("schedule mismatch between overlay %q and %q: A/B comparison requires identical schedules", nameA, nameB)
	}

	validA := repA.Validity == nil || repA.Validity.Valid
	validB := repB.Validity == nil || repB.Validity.Valid

	var metrics []OverlayMetricDiff
	addMetric := func(name string, a, b float64) {
		delta := b - a
		var pct float64
		if math.Abs(a) > 1e-9 {
			pct = (delta / a) * 100.0
		}
		metrics = append(metrics, OverlayMetricDiff{
			Metric:   name,
			ValueA:   a,
			ValueB:   b,
			Delta:    delta,
			DeltaPct: pct,
		})
	}

	createA, okA := repA.StatFor(swebench.EventCreate, "")
	if !okA {
		createA, _ = repA.StatFor(swebench.EventPhase, "create")
	}
	createB, okB := repB.StatFor(swebench.EventCreate, "")
	if !okB {
		createB, _ = repB.StatFor(swebench.EventPhase, "create")
	}
	addMetric("create_p50_ms", createA.P50MS, createB.P50MS)
	addMetric("create_p95_ms", createA.P95MS, createB.P95MS)

	ttfiA, _ := repA.StatFor(swebench.EventTTFI, "")
	ttfiB, _ := repB.StatFor(swebench.EventTTFI, "")
	addMetric("ttfi_p50_ms", ttfiA.P50MS, ttfiB.P50MS)
	addMetric("ttfi_p95_ms", ttfiA.P95MS, ttfiB.P95MS)

	wakeA, _ := repA.StatFor(swebench.EventWake, "")
	wakeB, _ := repB.StatFor(swebench.EventWake, "")
	addMetric("wake_p50_ms", wakeA.P50MS, wakeB.P50MS)
	addMetric("wake_p95_ms", wakeA.P95MS, wakeB.P95MS)

	addMetric("throughput_tasks_per_min", repA.ThroughputTasksPerMin(), repB.ThroughputTasksPerMin())
	addMetric("reward_rate", repA.RewardRate, repB.RewardRate)

	return &OverlayPairComparison{
		OverlayA:      nameA,
		OverlayB:      nameB,
		ScheduleMatch: schedMatch,
		ValidA:        validA,
		ValidB:        validB,
		Metrics:       metrics,
		SourcesA:      repA.AcquisitionSources,
		SourcesB:      repB.AcquisitionSources,
	}, nil
}

// RenderComparisonMarkdown formats an OverlayPairComparison as a Markdown table.
func RenderComparisonMarkdown(cmp *OverlayPairComparison) string {
	if cmp == nil {
		return ""
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "## Overlay Comparison: `%s` vs `%s`\n\n", cmp.OverlayA, cmp.OverlayB)
	fmt.Fprintf(&sb, "- **Schedule Match**: `%v`\n", cmp.ScheduleMatch)
	fmt.Fprintf(&sb, "- **Validity**: `%s=%v`, `%s=%v`\n\n", cmp.OverlayA, cmp.ValidA, cmp.OverlayB, cmp.ValidB)
	fmt.Fprintf(&sb, "| Metric | %s | %s | Delta | Delta %% |\n", cmp.OverlayA, cmp.OverlayB)
	sb.WriteString("|---|---:|---:|---:|---:|\n")
	for _, m := range cmp.Metrics {
		fmt.Fprintf(&sb, "| `%s` | %.2f | %.2f | %+.2f | %+.1f%% |\n",
			m.Metric, m.ValueA, m.ValueB, m.Delta, m.DeltaPct)
	}
	if len(cmp.SourcesA) > 0 || len(cmp.SourcesB) > 0 {
		sb.WriteString("\n### Acquisition Sources\n\n")
		fmt.Fprintf(&sb, "| Source | %s | %s |\n", cmp.OverlayA, cmp.OverlayB)
		sb.WriteString("|---|---:|---:|\n")
		for _, src := range []string{string(schema.SourceWarmPool), string(schema.SourceColdStart), string(schema.SourceSnapshotResume)} {
			a := cmp.SourcesA[src]
			b := cmp.SourcesB[src]
			if a > 0 || b > 0 {
				fmt.Fprintf(&sb, "| `%s` | %d | %d |\n", src, a, b)
			}
		}
	}
	return sb.String()
}

func schedulesEqual(a, b *schema.Schedule) bool {
	if a == nil && b == nil {
		return true
	}
	if a == nil || b == nil {
		return false
	}
	return a.ScenarioName == b.ScenarioName && a.Scenario == b.Scenario && a.Seed == b.Seed && reflect.DeepEqual(a.Jobs, b.Jobs)
}
