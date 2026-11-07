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
	"math"
	"sort"
	"strconv"
	"strings"

	"github.com/agent-substrate/env/internal/benchmarking/schema"
)

func promSeriesWindowDelta(ps schema.PrometheusSeries) float64 {
	if len(ps.Values) >= 2 {
		first := ps.Values[0]
		last := ps.Values[len(ps.Values)-1]
		if !math.IsNaN(first) && !math.IsNaN(last) {
			if last >= first {
				return last - first
			}
			return last
		}
	}
	if ps.Value != nil && !math.IsNaN(*ps.Value) {
		return *ps.Value
	}
	return 0
}

func labelsMatch(got, want map[string]string) bool {
	if len(want) == 0 {
		return true
	}
	for k, v := range want {
		if got == nil || got[k] != v {
			return false
		}
	}
	return true
}

func promBucketQuantile(buckets map[float64]float64, total float64, phi float64) float64 {
	if total <= 0 || len(buckets) == 0 {
		return 0
	}
	var finiteLEs []float64
	for le := range buckets {
		if !math.IsInf(le, 1) && !math.IsNaN(le) {
			finiteLEs = append(finiteLEs, le)
		}
	}
	sort.Float64s(finiteLEs)
	if len(finiteLEs) == 0 {
		return 0
	}
	target := phi * total
	for i, le := range finiteLEs {
		cnt := buckets[le]
		if cnt >= target {
			if i == 0 {
				if cnt <= 0 {
					return 0
				}
				return le * (target / cnt)
			}
			prevLE := finiteLEs[i-1]
			prevCnt := buckets[prevLE]
			span := cnt - prevCnt
			if span <= 0 {
				return le
			}
			return prevLE + (le-prevLE)*((target-prevCnt)/span)
		}
	}
	return finiteLEs[len(finiteLEs)-1]
}

// windowed reports whether a cumulative Prometheus series (a histogram's
// _bucket/_count/_sum or a _total counter) carries the two samples a run
// window needs. A single value is the counter's total since the process
// started, so an ate-env-api scraped once after the run would report every
// run it ever served; such a series is skipped rather than misread.
func windowed(ps schema.PrometheusSeries) bool {
	return len(ps.Values) >= 2
}

// PromHistogramStat computes an OpStat (in milliseconds) from Prometheus cumulative
// histogram series (`<baseName>_bucket`, `<baseName>_count`, `<baseName>_sum`) or
// pre-aggregated MetricSummary entries in Measurements. Histogram series count
// only when windowed (two samples); the quantiles are bucket estimates in the
// manner of histogram_quantile. MaxMS (and the summary's Min) come from the
// client's `<family>_max_seconds` / `<family>_min_seconds` gauges when
// scraped, and stay 0 otherwise: a histogram holds no observation, only
// bucket bounds.
func PromHistogramStat(meas *schema.Measurements, baseName string, labelFilter map[string]string) (Stat, bool) {
	st, _, ok := promHistogram(meas, baseName, labelFilter)
	return st, ok
}

// promHistogramDetail carries what Stat has no field for.
type promHistogramDetail struct {
	SumMS float64
	MinMS float64
	P99MS float64
}

// promSeriesLast is the newest value of a series.
func promSeriesLast(ps schema.PrometheusSeries) (float64, bool) {
	if n := len(ps.Values); n > 0 && !math.IsNaN(ps.Values[n-1]) {
		return ps.Values[n-1], true
	}
	if ps.Value != nil && !math.IsNaN(*ps.Value) {
		return *ps.Value, true
	}
	return 0, false
}

func promHistogram(meas *schema.Measurements, baseName string, labelFilter map[string]string) (Stat, promHistogramDetail, bool) {
	var detail promHistogramDetail
	if meas == nil {
		return Stat{}, detail, false
	}
	bucketName := baseName + "_bucket"
	countName := baseName + "_count"
	sumName := baseName + "_sum"
	family := strings.TrimSuffix(baseName, "_seconds")
	maxName := family + "_max_seconds"
	minName := family + "_min_seconds"
	buckets := make(map[float64]float64)
	var infCount, infErrCount float64
	var explicitCount, explicitErrCount float64
	var sumSeconds, maxSeconds, minSeconds float64
	var sawBucket, sawSum, sawMax, sawMin bool

	isErr := func(labels map[string]string) bool {
		return labels != nil && (labels["status"] == "error" || labels["outcome"] == "error")
	}
	for _, ps := range meas.PromMetrics {
		switch {
		case (ps.Name == bucketName || ps.Name == baseName) && ps.Labels != nil && ps.Labels["le"] != "":
			if !labelsMatch(ps.Labels, labelFilter) || !windowed(ps) {
				continue
			}
			sawBucket = true
			delta := promSeriesWindowDelta(ps)
			leStr := ps.Labels["le"]
			var le float64
			if leStr == "+Inf" || leStr == "Inf" || leStr == "+inf" {
				le = math.Inf(1)
				infCount += delta
				if isErr(ps.Labels) {
					infErrCount += delta
				}
			} else {
				parsed, err := strconv.ParseFloat(leStr, 64)
				if err != nil {
					continue
				}
				le = parsed
			}
			buckets[le] += delta
		case ps.Name == countName && labelsMatch(ps.Labels, labelFilter) && windowed(ps):
			delta := promSeriesWindowDelta(ps)
			explicitCount += delta
			if isErr(ps.Labels) {
				explicitErrCount += delta
			}
		case (ps.Name == sumName || (ps.Name == baseName && (ps.Labels == nil || ps.Labels["le"] == ""))) && labelsMatch(ps.Labels, labelFilter) && windowed(ps):
			// queries.yaml scrapes <family>_sum under the family's base name.
			sawSum = true
			sumSeconds += promSeriesWindowDelta(ps)
		case ps.Name == maxName && labelsMatch(ps.Labels, labelFilter):
			// Gauges, not counters: the last value is the process-lifetime
			// extreme, exact where the buckets can only bound it.
			if v, ok := promSeriesLast(ps); ok && (!sawMax || v > maxSeconds) {
				maxSeconds, sawMax = v, true
			}
		case ps.Name == minName && labelsMatch(ps.Labels, labelFilter):
			if v, ok := promSeriesLast(ps); ok && v > 0 && (!sawMin || v < minSeconds) {
				minSeconds, sawMin = v, true
			}
		}
	}

	// Errors come from one source: the +Inf bucket when buckets exist (it is
	// the histogram's own count), else the _count series. queries.yaml
	// scrapes both, so adding them would count every error twice.
	totalCount, errCount := infCount, infErrCount
	if totalCount == 0 && explicitCount > 0 {
		totalCount, errCount = explicitCount, explicitErrCount
	}
	if sawBucket && totalCount > 0 {
		p50 := promBucketQuantile(buckets, totalCount, 0.50) * 1000.0
		p90 := promBucketQuantile(buckets, totalCount, 0.90) * 1000.0
		p95 := promBucketQuantile(buckets, totalCount, 0.95) * 1000.0
		detail.P99MS = round2(promBucketQuantile(buckets, totalCount, 0.99) * 1000.0)
		if sawSum {
			detail.SumMS = round2(sumSeconds * 1000.0)
		}
		if sawMin {
			detail.MinMS = round2(minSeconds * 1000.0)
		}
		st := Stat{
			Count:  int(math.Round(totalCount)),
			Failed: int(math.Round(errCount)),
			P50MS:  round2(p50),
			P90MS:  round2(p90),
			P95MS:  round2(p95),
		}
		if sawMax {
			st.MaxMS = round2(maxSeconds * 1000.0)
		}
		return st, detail, true
	}

	// Check pre-aggregated MetricSummary maps (CustomMetrics / ServerSummary).
	msKey := strings.TrimSuffix(baseName, "_seconds") + "_ms"
	for _, m := range []map[string]schema.MetricSummary{meas.CustomMetrics, meas.ServerSummary} {
		if m == nil {
			continue
		}
		if summary, ok := m[msKey]; ok && summary.Count > 0 {
			p95 := summary.P95
			if p95 == 0 {
				p95 = summary.P90
			}
			detail.SumMS, detail.MinMS, detail.P99MS = summary.Sum, summary.Min, summary.P99
			return Stat{
				Count: summary.Count,
				P50MS: round2(summary.P50),
				P90MS: round2(summary.P90),
				P95MS: round2(p95),
				MaxMS: round2(summary.Max),
			}, detail, true
		}
		if summary, ok := m[baseName]; ok && summary.Count > 0 {
			p95 := summary.P95
			if p95 == 0 {
				p95 = summary.P90
			}
			detail.SumMS, detail.MinMS, detail.P99MS = summary.Sum*1000.0, summary.Min*1000.0, summary.P99*1000.0
			return Stat{
				Count: summary.Count,
				P50MS: round2(summary.P50 * 1000.0),
				P90MS: round2(summary.P90 * 1000.0),
				P95MS: round2(p95 * 1000.0),
				MaxMS: round2(summary.Max * 1000.0),
			}, detail, true
		}
	}

	return Stat{}, detail, false
}

// upsertStatPrimary puts a metrics-derived stat in place of the trace-derived
// one for the same event: metrics first, since traces add latency of their
// own and are often not captured at all. Size and retry fields the metrics
// do not carry are kept from the trace stat.
func (rep *Report) upsertStatPrimary(st Stat) {
	if st.Count <= 0 {
		return
	}
	for i := range rep.Stats {
		if rep.Stats[i].Event == st.Event && rep.Stats[i].Name == st.Name {
			if st.SizeMBP50 == 0 {
				st.SizeMBP50 = rep.Stats[i].SizeMBP50
				st.SizeMBP95 = rep.Stats[i].SizeMBP95
				st.SizeMBMax = rep.Stats[i].SizeMBMax
				st.CheckpointMBS = rep.Stats[i].CheckpointMBS
			}
			if st.Retried == 0 {
				st.Retried = rep.Stats[i].Retried
			}
			rep.Stats[i] = st
			return
		}
	}
	rep.Stats = append(rep.Stats, st)
}

func statToMetricSummaryMS(st Stat, d promHistogramDetail) schema.MetricSummary {
	return schema.MetricSummary{
		Count: st.Count,
		Sum:   d.SumMS,
		Min:   d.MinMS,
		P50:   st.P50MS,
		P90:   st.P90MS,
		P95:   st.P95MS,
		P99:   d.P99MS,
		Max:   st.MaxMS,
		Unit:  "ms",
	}
}

// ApplyMetricsPrimaryWithTraceFallback populates operation stats, sub-phase
// custom metrics, task counts, reward rate, and headline metrics from
// Prometheus/trainer metrics first, falling back to trace-derived stats only
// when metrics are absent.
func (rep *Report) ApplyMetricsPrimaryWithTraceFallback(meas *schema.Measurements) {
	if rep == nil {
		return
	}

	if meas != nil {
		if rep.CustomMetrics == nil {
			rep.CustomMetrics = make(map[string]schema.MetricSummary)
		}

		// 1. Sub-phase metrics for TTFI breakdown.
		if sw, d, ok := promHistogram(meas, "ate_env_client_slot_wait_seconds", nil); ok {
			rep.CustomMetrics["sdk_slot_wait_ms"] = statToMetricSummaryMS(sw, d)
		}
		if wc, d, ok := promHistogram(meas, "ate_env_client_wire_claim_seconds", nil); ok {
			rep.CustomMetrics["wire_claim_to_ready_ms"] = statToMetricSummaryMS(wc, d)
		}
		if rf, d, ok := promHistogram(meas, "ate_env_client_ready_to_first_command_seconds", nil); ok {
			rep.CustomMetrics["ready_to_first_cmd_ms"] = statToMetricSummaryMS(rf, d)
		}

		// 2. Primary operation & phase latency stats (metrics-first, trace fallback).
		if ttfi, ok := PromHistogramStat(meas, "ate_env_client_ttfi_seconds", nil); ok {
			ttfi.Event = EventTTFI
			rep.upsertStatPrimary(ttfi)
		}
		// create: the cold-create claim RPC. ate_env_client_acquire_seconds has
		// no source label (CLIENT_FAMILIES), so the per-source view lives in
		// the wire_claim sub-phase.
		if createSt, ok := PromHistogramStat(meas, "ate_env_client_wire_claim_seconds", map[string]string{"source": "cold_create"}); ok {
			createSt.Event = EventCreate
			rep.upsertStatPrimary(createSt)
		} else if apiCreateSt, ok := PromHistogramStat(meas, "ate_env_api_create_to_ready_seconds", nil); ok {
			apiCreateSt.Event = EventCreate
			rep.upsertStatPrimary(apiCreateSt)
		}
		// No third branch on ate_env_client_acquire_seconds: it has no source
		// label and times warm claims and cold creates alike, so a window with
		// no cold create would report warm claims as creates. The trace
		// create stat, if any, stays.

		if wakeSt, ok := PromHistogramStat(meas, "ate_env_client_warm_claim_seconds", nil); ok {
			wakeSt.Event = EventWake
			rep.upsertStatPrimary(wakeSt)
		} else if apiWakeSt, ok := PromHistogramStat(meas, "ate_env_api_wake_wait_seconds", nil); ok {
			apiWakeSt.Event = EventWake
			rep.upsertStatPrimary(apiWakeSt)
		}

		if suspSt, ok := PromHistogramStat(meas, "ate_env_client_suspend_seconds", nil); ok {
			suspSt.Event = EventSuspend
			rep.upsertStatPrimary(suspSt)
		}
		if delSt, ok := PromHistogramStat(meas, "ate_env_client_release_seconds", nil); ok {
			delSt.Event = EventDelete
			rep.upsertStatPrimary(delSt)
		}
		if execSt, ok := PromHistogramStat(meas, "ate_env_client_exec_seconds", nil); ok {
			execSt.Event = EventPhase
			execSt.Name = "agent_exec"
			rep.upsertStatPrimary(execSt)
		}
		if gradeSt, ok := PromHistogramStat(meas, "ate_env_client_grade_seconds", nil); ok {
			gradePhase := gradeSt
			gradePhase.Event = EventPhase
			gradePhase.Name = "grade"
			rep.upsertStatPrimary(gradePhase)
			gradeStep := gradeSt
			gradeStep.Event = EventStep
			gradeStep.Name = "grade"
			rep.upsertStatPrimary(gradeStep)
		}
		if rfSt, ok := PromHistogramStat(meas, "ate_env_client_read_file_seconds", nil); ok {
			rfSt.Event = EventPhase
			rfSt.Name = "read_file"
			rep.upsertStatPrimary(rfSt)
		}
		if wfSt, ok := PromHistogramStat(meas, "ate_env_client_write_file_seconds", nil); ok {
			wfSt.Event = EventPhase
			wfSt.Name = "write_file"
			rep.upsertStatPrimary(wfSt)
		}
		if taskSt, ok := PromHistogramStat(meas, "ate_env_fleet_task_latency_seconds", nil); ok {
			taskSt.Event = EventTask
			rep.upsertStatPrimary(taskSt)
		}

		// 3. Task & failure counts from fleet counter if available.
		var fleetTasks, fleetFailed float64
		for _, ps := range meas.PromMetrics {
			if ps.Name == "ate_env_fleet_tasks_total" {
				d := promSeriesWindowDelta(ps)
				fleetTasks += d
				if ps.Labels != nil && ps.Labels["status"] == "error" {
					fleetFailed += d
				}
			}
		}
		if fleetTasks > 0 {
			rep.Tasks = int(math.Round(fleetTasks))
			rep.Failed = int(math.Round(fleetFailed))
		}
	}

	// 4. Fallback task count from rollout step metrics when EventTask & fleet counter are absent.
	if rep.Tasks == 0 && len(rep.RolloutBreakdown) > 0 {
		totalStepTasks := 0
		for _, rs := range rep.RolloutBreakdown {
			totalStepTasks += rs.Tasks
		}
		if totalStepTasks > 0 {
			rep.Tasks = totalStepTasks
		}
	}

	// 5. Primary reward rate from trainer step metrics if available, else keep trace-derived RewardRate.
	if cm, ok := rep.CustomMetrics["critic/rewards/mean"]; ok && cm.Count > 0 {
		rep.RewardRate = round4(cm.Sum / float64(cm.Count))
		if rep.Tasks > 0 {
			rep.Rewarded = int(math.Round(rep.RewardRate * float64(rep.Tasks)))
		}
	} else if cm, ok := rep.CustomMetrics["critic/score/mean"]; ok && cm.Count > 0 {
		rep.RewardRate = round4(cm.Sum / float64(cm.Count))
		if rep.Tasks > 0 {
			rep.Rewarded = int(math.Round(rep.RewardRate * float64(rep.Tasks)))
		}
	}

	rep.refreshHeadlineFromReport()
}

// refreshHeadlineFromReport rebuilds the headline from the report's stats and
// custom metrics with the one set of rules (buildHeadlineBaseline), keeping
// the claim-rate fields, which only the event stream can supply.
func (rep *Report) refreshHeadlineFromReport() {
	if rep == nil {
		return
	}
	old := rep.Headline
	hb := (&Builder{}).buildHeadlineBaseline(rep)
	if old != nil {
		hb.ClaimsCount = old.ClaimsCount
		hb.PeakClaimsPerSec = old.PeakClaimsPerSec
		hb.SustainedClaimsPerSec = old.SustainedClaimsPerSec
		if hb.TasksPerMin == 0 {
			hb.TasksPerMin = old.TasksPerMin
		}
	}
	rep.Headline = hb
}
