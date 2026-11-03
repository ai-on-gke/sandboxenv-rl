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

// PromHistogramStat computes an OpStat (in milliseconds) from Prometheus cumulative
// histogram series (`<baseName>_bucket`, `<baseName>_count`, `<baseName>_sum`) or
// pre-aggregated MetricSummary entries in Measurements.
func PromHistogramStat(meas *schema.Measurements, baseName string, labelFilter map[string]string) (Stat, bool) {
	if meas == nil {
		return Stat{}, false
	}
	bucketName := baseName + "_bucket"
	countName := baseName + "_count"
	buckets := make(map[float64]float64)
	var infCount float64
	var errCount float64
	var explicitCount float64
	var sawBucket bool

	for _, ps := range meas.PromMetrics {
		if (ps.Name == bucketName || ps.Name == baseName) && ps.Labels != nil && ps.Labels["le"] != "" {
			if !labelsMatch(ps.Labels, labelFilter) {
				continue
			}
			sawBucket = true
			delta := promSeriesWindowDelta(ps)
			leStr := ps.Labels["le"]
			var le float64
			if leStr == "+Inf" || leStr == "Inf" || leStr == "+inf" {
				le = math.Inf(1)
				infCount += delta
				if ps.Labels["status"] == "error" || ps.Labels["outcome"] == "error" {
					errCount += delta
				}
			} else {
				parsed, err := strconv.ParseFloat(leStr, 64)
				if err != nil {
					continue
				}
				le = parsed
			}
			buckets[le] += delta
		} else if ps.Name == countName && labelsMatch(ps.Labels, labelFilter) {
			delta := promSeriesWindowDelta(ps)
			explicitCount += delta
			if ps.Labels != nil && (ps.Labels["status"] == "error" || ps.Labels["outcome"] == "error") {
				errCount += delta
			}
		}
	}

	totalCount := infCount
	if totalCount == 0 && explicitCount > 0 {
		totalCount = explicitCount
	}
	if sawBucket && totalCount > 0 {
		p50 := promBucketQuantile(buckets, totalCount, 0.50) * 1000.0
		p90 := promBucketQuantile(buckets, totalCount, 0.90) * 1000.0
		p95 := promBucketQuantile(buckets, totalCount, 0.95) * 1000.0
		maxMS := promBucketQuantile(buckets, totalCount, 0.999) * 1000.0
		return Stat{
			Count:  int(math.Round(totalCount)),
			Failed: int(math.Round(errCount)),
			P50MS:  round2(p50),
			P90MS:  round2(p90),
			P95MS:  round2(p95),
			MaxMS:  round2(maxMS),
		}, true
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
			return Stat{
				Count:  summary.Count,
				P50MS:  round2(summary.P50),
				P90MS:  round2(summary.P90),
				P95MS:  round2(p95),
				MaxMS:  round2(summary.Max),
			}, true
		}
		if summary, ok := m[baseName]; ok && summary.Count > 0 {
			p95 := summary.P95
			if p95 == 0 {
				p95 = summary.P90
			}
			return Stat{
				Count:  summary.Count,
				P50MS:  round2(summary.P50 * 1000.0),
				P90MS:  round2(summary.P90 * 1000.0),
				P95MS:  round2(p95 * 1000.0),
				MaxMS:  round2(summary.Max * 1000.0),
			}, true
		}
	}

	return Stat{}, false
}

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

func statToMetricSummaryMS(st Stat) schema.MetricSummary {
	return schema.MetricSummary{
		Count: st.Count,
		P50:   st.P50MS,
		P90:   st.P90MS,
		P95:   st.P95MS,
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
		if sw, ok := PromHistogramStat(meas, "ate_env_client_slot_wait_seconds", nil); ok {
			rep.CustomMetrics["sdk_slot_wait_ms"] = statToMetricSummaryMS(sw)
		}
		if wc, ok := PromHistogramStat(meas, "ate_env_client_wire_claim_seconds", nil); ok {
			rep.CustomMetrics["wire_claim_to_ready_ms"] = statToMetricSummaryMS(wc)
		}
		if rf, ok := PromHistogramStat(meas, "ate_env_client_ready_to_first_command_seconds", nil); ok {
			rep.CustomMetrics["ready_to_first_cmd_ms"] = statToMetricSummaryMS(rf)
		}

		// 2. Primary operation & phase latency stats (metrics-first, trace fallback).
		if ttfi, ok := PromHistogramStat(meas, "ate_env_client_ttfi_seconds", nil); ok {
			ttfi.Event = EventTTFI
			rep.upsertStatPrimary(ttfi)
		}
		if createSt, ok := PromHistogramStat(meas, "ate_env_client_acquire_seconds", map[string]string{"source": "cold_create"}); ok {
			createSt.Event = EventCreate
			rep.upsertStatPrimary(createSt)
		} else if apiCreateSt, ok := PromHistogramStat(meas, "ate_env_api_create_to_ready_seconds", nil); ok {
			apiCreateSt.Event = EventCreate
			rep.upsertStatPrimary(apiCreateSt)
		} else if acqSt, ok := PromHistogramStat(meas, "ate_env_client_acquire_seconds", nil); ok {
			acqSt.Event = EventCreate
			rep.upsertStatPrimary(acqSt)
		}

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

func (rep *Report) refreshHeadlineFromReport() {
	if rep == nil {
		return
	}
	if rep.Headline == nil {
		rep.Headline = &HeadlineBaseline{}
	}
	hb := rep.Headline
	if rep.Tasks > 0 {
		hb.TasksPerMin = rep.ThroughputTasksPerMin()
	}
	if st, ok := rep.StatFor(EventTTFI, ""); ok && st.Count > 0 {
		hb.TTFIP50MS = st.P50MS
		hb.TTFIP90MS = st.P90MS
		hb.TTFIP95MS = st.P95MS
		hb.TTFIMaxMS = st.MaxMS
	}
	if cm, ok := rep.CustomMetrics["sdk_slot_wait_ms"]; ok && cm.Count > 0 {
		hb.SDKSlotWaitP50MS = cm.P50
		hb.SDKSlotWaitP95MS = cm.P95
	}
	if cm, ok := rep.CustomMetrics["wire_claim_to_ready_ms"]; ok && cm.Count > 0 {
		hb.WireClaimToReadyP50MS = cm.P50
		hb.WireClaimToReadyP95MS = cm.P95
	}
	if cm, ok := rep.CustomMetrics["ready_to_first_cmd_ms"]; ok && cm.Count > 0 {
		hb.ReadyToFirstCmdP50MS = cm.P50
		hb.ReadyToFirstCmdP95MS = cm.P95
	}
	if st, ok := rep.StatFor(EventCreate, ""); ok && st.Count > 0 {
		hb.CreateP50MS = st.P50MS
		hb.CreateP95MS = st.P95MS
		if hb.WireClaimToReadyP50MS == 0 {
			hb.WireClaimToReadyP50MS = st.P50MS
			hb.WireClaimToReadyP95MS = st.P95MS
		}
	}
	if st, ok := rep.StatFor(EventWake, ""); ok && st.Count > 0 {
		hb.WakeP50MS = st.P50MS
		hb.WakeP95MS = st.P95MS
	}
	for _, execName := range []struct {
		ev   string
		name string
	}{
		{EventStep, "exec"},
		{EventStep, "agent_exec"},
		{EventPhase, "agent_exec"},
		{EventStep, "patch"},
		{EventStep, "setup"},
	} {
		if st, ok := rep.StatFor(execName.ev, execName.name); ok && st.Count > 0 {
			hb.ExecP50MS = st.P50MS
			hb.ExecP95MS = st.P95MS
			break
		}
	}
	if st, ok := rep.StatFor(EventSuspend, ""); ok && st.Count > 0 {
		hb.SuspendP50MS = st.P50MS
		hb.SuspendP95MS = st.P95MS
		if st.SizeMBP50 > 0 {
			hb.SuspendSizeMBP50 = st.SizeMBP50
			hb.SuspendSizeMBP95 = st.SizeMBP95
			hb.CheckpointMBS = st.CheckpointMBS
		}
	}
	if st, ok := rep.StatFor(EventDelete, ""); ok && st.Count > 0 {
		hb.DeleteP50MS = st.P50MS
		hb.DeleteP95MS = st.P95MS
	}
}
