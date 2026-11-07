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

// makePromBuckets builds the windowed series of one histogram: every bucket
// starts at 0 and ends at its count, plus the matching _count and _sum, as a
// sampler that scraped the family before and after the run would leave them.
func makePromBuckets(metric string, labels map[string]string, leCounts map[string]float64) []schema.PrometheusSeries {
	return makePromBucketsWithSum(metric, labels, leCounts, 0)
}

func makePromBucketsWithSum(metric string, labels map[string]string, leCounts map[string]float64, sumSeconds float64) []schema.PrometheusSeries {
	var out []schema.PrometheusSeries
	withLabels := func(extra map[string]string) map[string]string {
		lbls := map[string]string{}
		for k, v := range labels {
			lbls[k] = v
		}
		for k, v := range extra {
			lbls[k] = v
		}
		return lbls
	}
	var total float64
	for le, cnt := range leCounts {
		if le == "+Inf" {
			total = cnt
		}
		out = append(out, schema.PrometheusSeries{
			Name:   metric + "_bucket",
			Labels: withLabels(map[string]string{"le": le}),
			Values: []float64{0, cnt},
			Value:  floatPtr(cnt),
		})
	}
	out = append(out,
		schema.PrometheusSeries{Name: metric + "_count", Labels: withLabels(nil), Values: []float64{0, total}, Value: floatPtr(total)},
		schema.PrometheusSeries{Name: metric + "_sum", Labels: withLabels(nil), Values: []float64{0, sumSeconds}, Value: floatPtr(sumSeconds)},
	)
	return out
}

func TestPromHistogramStatCountsErrorsOnce(t *testing.T) {
	// 8 ok + 2 error observations, buckets and _count both scraped: Failed is 2, not 4.
	var prom []schema.PrometheusSeries
	prom = append(prom, makePromBuckets("ate_env_client_acquire_seconds", map[string]string{"status": "ok"}, map[string]float64{"1.0": 8, "+Inf": 8})...)
	prom = append(prom, makePromBuckets("ate_env_client_acquire_seconds", map[string]string{"status": "error"}, map[string]float64{"1.0": 0, "+Inf": 2})...)
	st, ok := PromHistogramStat(&schema.Measurements{PromMetrics: prom}, "ate_env_client_acquire_seconds", nil)
	if !ok || st.Count != 10 || st.Failed != 2 {
		t.Fatalf("stat = %+v ok=%v, want Count=10 Failed=2", st, ok)
	}
	if st.MaxMS != 0 {
		t.Fatalf("MaxMS = %v, want 0: a histogram has no observed maximum", st.MaxMS)
	}
}

func TestPromHistogramStatSkipsUnwindowedSeries(t *testing.T) {
	// One sample per series is a process-lifetime total, not a run window.
	prom := []schema.PrometheusSeries{
		{Name: "ate_env_api_create_to_ready_seconds_bucket", Labels: map[string]string{"le": "1.0", "status": "ok"}, Value: floatPtr(48)},
		{Name: "ate_env_api_create_to_ready_seconds_bucket", Labels: map[string]string{"le": "+Inf", "status": "ok"}, Value: floatPtr(48)},
		{Name: "ate_env_api_create_to_ready_seconds_count", Labels: map[string]string{"status": "ok"}, Value: floatPtr(48)},
	}
	if st, ok := PromHistogramStat(&schema.Measurements{PromMetrics: prom}, "ate_env_api_create_to_ready_seconds", nil); ok {
		t.Fatalf("unwindowed series must not yield a stat, got %+v", st)
	}
	rep := &Report{Stats: []Stat{{Event: EventCreate, Count: 24, P50MS: 190, P95MS: 425, MaxMS: 499}}}
	rep.AttachClusterTelemetry(nil, &schema.Measurements{PromMetrics: prom})
	if st, _ := rep.StatFor(EventCreate, ""); st.Count != 24 || st.P50MS != 190 {
		t.Fatalf("trace create stat must survive an unwindowed api series, got %+v", st)
	}
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
	prom = append(prom, makePromBuckets("ate_env_client_wire_claim_seconds", map[string]string{"status": "ok", "source": "cold_create"}, map[string]float64{
		"1.0":  5,
		"2.0":  10,
		"+Inf": 10,
	})...)
	prom = append(prom, makePromBuckets("ate_env_client_grade_seconds", map[string]string{"status": "ok", "source": "warm_pool"}, map[string]float64{
		"0.5":  8,
		"1.0":  10,
		"+Inf": 10,
	})...)
	prom = append(prom, makePromBucketsWithSum("ate_env_client_slot_wait_seconds", map[string]string{"status": "ok", "source": "warm_pool"}, map[string]float64{
		"0.05": 5,
		"0.1":  10,
		"+Inf": 10,
	}, 0.6)...)
	// Exact extremes: the client exports them as gauges next to each histogram.
	prom = append(prom,
		schema.PrometheusSeries{Name: "ate_env_client_ttfi_max_seconds", Labels: map[string]string{"status": "ok", "source": "warm_pool"}, Values: []float64{0.9, 1.8}, Value: floatPtr(1.8)},
		schema.PrometheusSeries{Name: "ate_env_client_ttfi_min_seconds", Labels: map[string]string{"status": "ok", "source": "warm_pool"}, Values: []float64{0.4, 0.3}, Value: floatPtr(0.3)},
		schema.PrometheusSeries{Name: "ate_env_client_slot_wait_min_seconds", Labels: map[string]string{"status": "ok", "source": "warm_pool"}, Value: floatPtr(0.012)},
	)
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
	if ttfi.MaxMS != 1800.0 {
		t.Errorf("TTFI MaxMS = %v, want 1800 from the _max_seconds gauge", ttfi.MaxMS)
	}
	if rep.Headline == nil || rep.Headline.TTFIP50MS != 875.0 || rep.Headline.CreateP50MS != 1000.0 {
		t.Fatalf("unexpected Headline: %+v", rep.Headline)
	}
	if gradeSt, ok := rep.StatFor(EventPhase, "grade"); !ok || gradeSt.Count != 10 {
		t.Fatalf("grade phase stat = %+v (ok=%v), want Count=10", gradeSt, ok)
	}
	if gradeSt, _ := rep.StatFor(EventPhase, "grade"); gradeSt.MaxMS != 0 {
		t.Errorf("grade MaxMS = %v, want 0 when no _max_seconds gauge was scraped", gradeSt.MaxMS)
	}
	sw := rep.CustomMetrics["sdk_slot_wait_ms"]
	if sw.Count != 10 || sw.Sum != 600 || sw.P99 == 0 {
		t.Fatalf("sdk_slot_wait_ms = %+v, want Count=10 Sum=600 (from _sum) and a P99", sw)
	}
	if sw.Min != 12 {
		t.Errorf("sdk_slot_wait_ms Min = %v, want 12 from the _min_seconds gauge", sw.Min)
	}
	if rep.Headline.SDKSlotWaitP50MS != sw.P50 {
		t.Fatalf("headline slot wait %v != custom metric %v", rep.Headline.SDKSlotWaitP50MS, sw.P50)
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
