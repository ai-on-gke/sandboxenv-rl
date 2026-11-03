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
	"testing"

	"github.com/agent-substrate/env/internal/benchmarking/schema"
)

func makePromBuckets(metric string, labels map[string]string, leCounts map[string]float64) []schema.PrometheusSeries {
	var out []schema.PrometheusSeries
	for le, cnt := range leCounts {
		lbls := map[string]string{"le": le}
		for k, v := range labels {
			lbls[k] = v
		}
		out = append(out, schema.PrometheusSeries{
			Name:   metric + "_bucket",
			Labels: lbls,
			Value:  floatPtr(cnt),
		})
	}
	return out
}

func TestMetricsPrimaryWithTraceFallback(t *testing.T) {
	// 1. Metrics-only report (no trace events; only trainer step custom metrics + Prometheus buckets).
	rep := &Report{
		RunID: "metrics-only-run",
		Mode:  "steps",
		RunMS: 60000,
		CustomMetrics: map[string]schema.MetricSummary{
			"critic/rewards/mean": {Count: 2, Sum: 1.50, P50: 0.75, P95: 0.80, Max: 0.80},
		},
	}

	var prom []schema.PrometheusSeries
	prom = append(prom, makePromBuckets("ate_env_client_ttfi_seconds", map[string]string{"status": "ok", "source": "warm_pool"}, map[string]float64{
		"0.5":  2,
		"1.0":  6,
		"2.0":  10,
		"+Inf": 10,
	})...)
	prom = append(prom, makePromBuckets("ate_env_client_acquire_seconds", map[string]string{"status": "ok", "source": "cold_create"}, map[string]float64{
		"1.0":  5,
		"2.0":  10,
		"+Inf": 10,
	})...)
	prom = append(prom, makePromBuckets("ate_env_client_grade_seconds", map[string]string{"status": "ok", "source": "warm_pool"}, map[string]float64{
		"0.5":  8,
		"1.0":  10,
		"+Inf": 10,
	})...)
	prom = append(prom,
		schema.PrometheusSeries{
			Name:   "ate_env_fleet_tasks_total",
			Labels: map[string]string{"status": "ok"},
			Values: []float64{10, 26}, // delta = 16
			Value:  floatPtr(26),
		},
		schema.PrometheusSeries{
			Name:   "ate_env_fleet_tasks_total",
			Labels: map[string]string{"status": "error"},
			Values: []float64{0, 2}, // delta = 2
			Value:  floatPtr(2),
		},
	)

	meas := &schema.Measurements{PromMetrics: prom}
	rep.AttachClusterTelemetry(nil, meas)

	if rep.Tasks != 18 || rep.Failed != 2 {
		t.Fatalf("Tasks=%d Failed=%d, want Tasks=18 Failed=2", rep.Tasks, rep.Failed)
	}
	if rep.RewardRate != 0.75 || rep.Rewarded != 14 {
		t.Fatalf("RewardRate=%v Rewarded=%d, want 0.75 and 14", rep.RewardRate, rep.Rewarded)
	}
	ttfi, ok := rep.StatFor(EventTTFI, "")
	if !ok || ttfi.Count != 10 {
		t.Fatalf("TTFI stat = %+v (ok=%v), want Count=10", ttfi, ok)
	}
	if ttfi.P50MS != 875.0 {
		t.Errorf("TTFI P50MS = %v, want 875.0", ttfi.P50MS)
	}
	if rep.Headline == nil || rep.Headline.TTFIP50MS != 875.0 || rep.Headline.CreateP50MS != 1000.0 {
		t.Fatalf("unexpected Headline: %+v", rep.Headline)
	}
	if gradeSt, ok := rep.StatFor(EventPhase, "grade"); !ok || gradeSt.Count != 10 {
		t.Fatalf("grade phase stat = %+v (ok=%v), want Count=10", gradeSt, ok)
	}

	// 2. Fallback to trace when metrics are absent.
	traceRep := &Report{
		RunID:      "trace-fallback-run",
		Tasks:      12,
		Rewarded:   6,
		RewardRate: 0.5,
		Stats: []Stat{
			{Event: EventTTFI, Count: 12, P50MS: 420.0, P95MS: 900.0, MaxMS: 1100.0},
		},
	}
	traceRep.AttachClusterTelemetry(nil, &schema.Measurements{})
	if st, ok := traceRep.StatFor(EventTTFI, ""); !ok || st.P50MS != 420.0 {
		t.Fatalf("expected trace TTFI fallback to remain 420ms, got %+v", st)
	}
	if traceRep.Tasks != 12 || traceRep.RewardRate != 0.5 {
		t.Fatalf("expected trace Tasks=12 RewardRate=0.5, got %d, %v", traceRep.Tasks, traceRep.RewardRate)
	}
}
