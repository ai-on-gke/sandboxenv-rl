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

package job

import (
	"bufio"
	"encoding/json"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/agent-substrate/env/internal/benchmarking/schema"
	"github.com/agent-substrate/env/internal/benchmarking/swebench"
)

var (
	// Matches VeRL trainer step lines such as:
	// `step:12 - timing_s/step:4.250 - timing_s/gen:2.800 - timing_s/update_actor:1.100 - critic/score/mean:0.625`
	// or `[verl] step=12 step_time_s=4.25 gen_time_s=2.80 reward=0.625`
	verlColonRe = regexp.MustCompile(`(?i)\bstep\s*[:=]\s*(\d+)\b`)
	kvMetricRe  = regexp.MustCompile(`([a-zA-Z0-9_./@-]+)\s*[:=]\s*([-+]?\d+(?:\.\d+)?(?:[eE][-+]?\d+)?)`)
	paramSyncRe = regexp.MustCompile(`(?i)(?:timing_s/param_sync|parameter sync time)\s*[:=]\s*([-+]?\d+(?:\.\d+)?(?:[eE][-+]?\d+)?)`)
)

// ParseTrainerLog parses stdout/stderr log lines from VeRL (`verl.trainer.main_ppo` /
// `verl.experimental.fully_async_policy`), Slime, SkyRL, or MLLog JSON lines into
// canonical `swebench.Event` step records. Multiple log lines for the same step
// (such as `FullyAsyncRollouter` and `FullyAsyncTrainer` lines) and preceding
// `timing_s/param_sync` lines are merged into one `EventStep` per training step.
func ParseTrainerLog(jobName, text string) []swebench.Event {
	var out []swebench.Event
	stepIndex := make(map[int]int) // step -> index in out
	pendingMetrics := make(map[string]float64)

	scanner := bufio.NewScanner(strings.NewReader(text))
	scanner.Buffer(make([]byte, 0, 64*1024), 4<<20)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		if pm := paramSyncRe.FindStringSubmatch(line); len(pm) == 2 && !verlColonRe.MatchString(line) {
			if val, err := strconv.ParseFloat(pm[1], 64); err == nil {
				pendingMetrics["timing_s/param_sync"] = val
			}
		}
		if ev, ok := parseMLLogLine(jobName, line); ok {
			out = append(out, ev)
			continue
		}
		if ev, ok := parseKeyValueStepLine(jobName, line); ok {
			for k, v := range pendingMetrics {
				if _, exists := ev.Metrics[k]; !exists {
					ev.Metrics[k] = v
				}
			}
			clear(pendingMetrics)
			if idx, seen := stepIndex[ev.Step]; seen {
				existing := &out[idx]
				if existing.Metrics == nil {
					existing.Metrics = make(map[string]float64)
				}
				for k, v := range ev.Metrics {
					existing.Metrics[k] = v
				}
				if ev.DurMS > 0 {
					existing.DurMS = ev.DurMS
				}
			} else if ev.DurMS > 0 || hasValMetrics(ev.Metrics) {
				stepIndex[ev.Step] = len(out)
				out = append(out, ev)
			} else {
				// Buffer metrics from rollouter step line until the trainer step line with timing_s/step arrives.
				for k, v := range ev.Metrics {
					pendingMetrics[k] = v
				}
			}
		}
	}
	if len(out) > 0 && len(pendingMetrics) > 0 {
		last := &out[len(out)-1]
		if last.Metrics == nil {
			last.Metrics = make(map[string]float64)
		}
		for k, v := range pendingMetrics {
			if _, exists := last.Metrics[k]; !exists {
				last.Metrics[k] = v
			}
		}
	}
	return out
}

func parseMLLogLine(jobName, line string) (swebench.Event, bool) {
	idx := strings.Index(line, ":::MLLOG")
	raw := line
	if idx >= 0 {
		raw = strings.TrimSpace(line[idx+len(":::MLLOG"):])
	} else if !strings.HasPrefix(line, "{") {
		return swebench.Event{}, false
	}
	var payload struct {
		Key      string             `json:"key"`
		Step     int                `json:"step"`
		DurMS    float64            `json:"dur_ms"`
		StepTime float64            `json:"step_time_s"`
		Metrics  map[string]float64 `json:"metrics"`
	}
	if err := json.Unmarshal([]byte(raw), &payload); err != nil {
		return swebench.Event{}, false
	}
	if payload.Step <= 0 && payload.Key == "" {
		return swebench.Event{}, false
	}
	durMS := payload.DurMS
	if durMS <= 0 && payload.StepTime > 0 {
		durMS = payload.StepTime * 1000.0
	}
	if durMS <= 0 && payload.Metrics != nil {
		if v, ok := payload.Metrics["timing_s/step"]; ok {
			durMS = v * 1000.0
		} else if v, ok := payload.Metrics["step_time_ms"]; ok {
			durMS = v
		}
	}
	return swebench.Event{
		Job:     jobName,
		Event:   swebench.EventStep,
		Step:    payload.Step,
		DurMS:   durMS,
		OK:      true,
		Metrics: payload.Metrics,
	}, true
}

func hasValMetrics(m map[string]float64) bool {
	for k := range m {
		if strings.HasPrefix(k, "val-core/") || strings.HasPrefix(k, "val-aux/") {
			return true
		}
	}
	return false
}

func parseKeyValueStepLine(jobName, line string) (swebench.Event, bool) {
	m := verlColonRe.FindStringSubmatch(line)
	if len(m) < 2 {
		return swebench.Event{}, false
	}
	step, err := strconv.Atoi(m[1])
	if err != nil || step < 0 {
		return swebench.Event{}, false
	}
	metrics := make(map[string]float64)
	for _, kv := range kvMetricRe.FindAllStringSubmatch(line, -1) {
		key := kv[1]
		if strings.EqualFold(key, "step") {
			continue
		}
		val, err := strconv.ParseFloat(kv[2], 64)
		if err == nil {
			metrics[key] = val
			if key == "timing_s/timing_s/param_sync" {
				metrics["timing_s/param_sync"] = val
			}
		}
	}
	if len(metrics) == 0 {
		return swebench.Event{}, false
	}
	if gs, ok := metrics["training/global_step"]; ok && gs >= 1 {
		step = int(gs)
	}
	if step == 0 && !hasValMetrics(metrics) {
		return swebench.Event{}, false
	}

	var durMS float64
	switch {
	case metrics["timing_s/step"] > 0:
		durMS = metrics["timing_s/step"] * 1000.0
	case metrics["timing_s/total"] > 0:
		durMS = metrics["timing_s/total"] * 1000.0
	case metrics["timing_s/train_step"] > 0:
		durMS = metrics["timing_s/train_step"] * 1000.0
	case metrics["perf/time_per_step"] > 0:
		durMS = metrics["perf/time_per_step"] * 1000.0
	case metrics["step_time_s"] > 0:
		durMS = metrics["step_time_s"] * 1000.0
	case metrics["step_time_ms"] > 0:
		durMS = metrics["step_time_ms"]
	case metrics["dur_ms"] > 0:
		durMS = metrics["dur_ms"]
	}
	return swebench.Event{
		Job:     jobName,
		Event:   swebench.EventStep,
		Step:    step,
		DurMS:   durMS,
		OK:      true,
		Metrics: metrics,
	}, true
}

// MergeTrainingConvergenceSummary reads a `training_convergence_summary.json`
// file (`steps` and `validations` maps keyed by step number, including step 0)
// and merges all step and validation metrics into stepEvs.
func MergeTrainingConvergenceSummary(path, jobName string, stepEvs []swebench.Event) ([]swebench.Event, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return stepEvs, err
	}
	var doc struct {
		Steps       map[string]map[string]float64 `json:"steps"`
		Validations map[string]map[string]float64 `json:"validations"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return stepEvs, err
	}
	byStep := make(map[int]int, len(stepEvs))
	for i := range stepEvs {
		byStep[stepEvs[i].Step] = i
	}
	mergeMap := func(s int, m map[string]float64) {
		if len(m) == 0 {
			return
		}
		var durMS float64
		if v := m["timing_s/step"]; v > 0 {
			durMS = v * 1000.0
		} else if v := m["perf/time_per_step"]; v > 0 {
			durMS = v * 1000.0
		}
		if idx, exists := byStep[s]; exists {
			if stepEvs[idx].Metrics == nil {
				stepEvs[idx].Metrics = make(map[string]float64, len(m))
			}
			for k, v := range m {
				stepEvs[idx].Metrics[k] = v
			}
			if stepEvs[idx].DurMS <= 0 && durMS > 0 {
				stepEvs[idx].DurMS = durMS
			}
		} else {
			cp := make(map[string]float64, len(m))
			for k, v := range m {
				cp[k] = v
			}
			byStep[s] = len(stepEvs)
			stepEvs = append(stepEvs, swebench.Event{
				Job:     jobName,
				Event:   swebench.EventStep,
				Step:    s,
				DurMS:   durMS,
				OK:      true,
				Metrics: cp,
			})
		}
	}
	for k, m := range doc.Steps {
		if s, err := strconv.Atoi(strings.TrimSpace(k)); err == nil && s >= 0 {
			mergeMap(s, m)
		}
	}
	for k, m := range doc.Validations {
		if s, err := strconv.Atoi(strings.TrimSpace(k)); err == nil && s >= 0 {
			mergeMap(s, m)
		}
	}
	sort.Slice(stepEvs, func(i, j int) bool { return stepEvs[i].Step < stepEvs[j].Step })
	return stepEvs, nil
}

// rawTraceRecord decodes both canonical schema.TraceEvent lines and extended
// tool trace records emitted by ate_env_tool.py when ATE_ENV_TRACE_DIR is set.
type rawTraceRecord struct {
	SchemaVersion string            `json:"schema_version"`
	TS            string            `json:"ts"`
	Run           string            `json:"run"`
	RunID         string            `json:"run_id"`
	Job           string            `json:"job"`
	Worker        string            `json:"worker"`
	Step          int               `json:"step"`
	Env           string            `json:"env"`
	Instance      string            `json:"instance"`
	Image         string            `json:"image"`
	Op            string            `json:"op"`
	Command       []string          `json:"command"`
	DurMS         float64           `json:"dur_ms"`
	SizeMB        float64           `json:"size_mb"`
	Bytes         int64             `json:"bytes"`
	DeletedCount  int               `json:"deleted_count"`
	OrphanCount   int               `json:"orphan_count"`
	OK            bool              `json:"ok"`
	Exit          *int              `json:"exit"`
	Reward        *float64          `json:"reward"`
	Err           string            `json:"err"`
	Snapshot      string            `json:"snapshot"`
	Tags          map[string]string `json:"tags"`
}

func isGradeTraceRecord(te rawTraceRecord) bool {
	if strings.EqualFold(strings.TrimSpace(te.Op), "grade") {
		return true
	}
	if te.Tags != nil && strings.EqualFold(te.Tags["phase"], "grade") {
		return true
	}
	for _, c := range te.Command {
		if strings.Contains(c, "run_tests.sh") || strings.Contains(c, "/eval.sh") {
			return true
		}
	}
	return false
}

// ParseTraceJSONLFile reads a JSONL file of `schema.TraceEvent` lines (such as
// those emitted by `ate_env_tool.py` or `uni_agent_substrate` when `ATE_ENV_TRACE_DIR`
// is set) and converts them into canonical `swebench.Event` lifecycle, phase, TTFI, and task events.
func ParseTraceJSONLFile(path, defaultJob string) ([]swebench.Event, error) {
	rawBytes, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}

	type pendingClaim struct {
		claimMS    float64
		slotWaitMS float64
	}
	var records []rawTraceRecord
	hasEpisodeOp := false

	scanner := bufio.NewScanner(strings.NewReader(string(rawBytes)))
	scanner.Buffer(make([]byte, 0, 64*1024), 4<<20)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		var te rawTraceRecord
		if err := json.Unmarshal([]byte(line), &te); err != nil {
			return nil, err
		}
		rawOp := strings.ToLower(strings.TrimSpace(te.Op))
		if rawOp == "episode" || rawOp == "rollout" {
			hasEpisodeOp = true
		}
		records = append(records, te)
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}

	var out []swebench.Event
	pendingFirstCmd := make(map[string]pendingClaim) // env -> claim timing
	envWaitByEnv := make(map[string]float64)

	for _, te := range records {
		runID := te.Run
		if runID == "" {
			runID = te.RunID
		}
		jobName := te.Job
		if jobName == "" {
			jobName = defaultJob
		}
		ts, err := time.Parse(time.RFC3339Nano, te.TS)
		if err != nil {
			ts = time.Now().UTC()
		}
		rawOp := strings.ToLower(strings.TrimSpace(te.Op))
		if rawOp != "episode" && rawOp != "rollout" && te.Env != "" && te.DurMS > 0 {
			envWaitByEnv[te.Env] += te.DurMS
		}
		phaseName := normalizeTraceOp(rawOp)
		source := ""
		var slotWaitMS float64
		if te.Tags != nil {
			source = te.Tags["source"]
			if sw, err := strconv.ParseFloat(te.Tags["sdk_slot_wait_ms"], 64); err == nil && sw > 0 {
				slotWaitMS = sw
			}
		}
		if source == "" {
			switch rawOp {
			case "wake", "acquire":
				source = string(schema.SourceWarmPool)
			case "create", "init_env":
				source = string(schema.SourceWarmPool)
			}
		}

		switch rawOp {
		case "create", "init_env":
			var cacheHitPtr *bool
			if te.Tags != nil && te.Tags["cache_hit"] != "" {
				ch := strings.EqualFold(te.Tags["cache_hit"], "true") || te.Tags["cache_hit"] == "1"
				cacheHitPtr = &ch
			}
			out = append(out, swebench.Event{
				Time:     ts,
				RunID:    runID,
				Job:      jobName,
				Worker:   te.Worker,
				Event:    swebench.EventCreate,
				Source:   source,
				Env:      te.Env,
				Instance: te.Instance,
				Image:    te.Image,
				Step:     te.Step,
				DurMS:    te.DurMS,
				OK:       te.OK,
				CacheHit: cacheHitPtr,
				Err:      te.Err,
				Tags:     te.Tags,
			})
			if te.OK && te.Env != "" {
				pendingFirstCmd[te.Env] = pendingClaim{claimMS: te.DurMS, slotWaitMS: slotWaitMS}
			}
		case "wake", "acquire":
			sw := slotWaitMS
			if sw == 0 {
				sw = te.DurMS
			}
			out = append(out, swebench.Event{
				Time:     ts,
				RunID:    runID,
				Job:      jobName,
				Worker:   te.Worker,
				Event:    swebench.EventWake,
				Source:   source,
				Env:      te.Env,
				Instance: te.Instance,
				Image:    te.Image,
				Step:     te.Step,
				DurMS:    te.DurMS,
				OK:       te.OK,
				Err:      te.Err,
				Tags:     te.Tags,
				Metrics: map[string]float64{
					"sdk_slot_wait_ms": sw,
				},
			})
			if te.OK && te.Env != "" {
				if _, exists := pendingFirstCmd[te.Env]; !exists {
					pendingFirstCmd[te.Env] = pendingClaim{claimMS: te.DurMS, slotWaitMS: slotWaitMS}
				}
			}
		case "prewarm":
			out = append(out, swebench.Event{
				Time:     ts,
				RunID:    runID,
				Job:      jobName,
				Worker:   te.Worker,
				Event:    swebench.EventPrewarm,
				Env:      te.Env,
				Instance: te.Instance,
				Image:    te.Image,
				Step:     te.Step,
				DurMS:    te.DurMS,
				OK:       te.OK,
				Err:      te.Err,
				Tags:     te.Tags,
			})
		case "delete", "destroy", "close":
			out = append(out, swebench.Event{
				Time:     ts,
				RunID:    runID,
				Job:      jobName,
				Worker:   te.Worker,
				Event:    swebench.EventDelete,
				Env:      te.Env,
				Instance: te.Instance,
				Image:    te.Image,
				Step:     te.Step,
				DurMS:    te.DurMS,
				OK:       te.OK,
				Err:      te.Err,
				Tags:     te.Tags,
			})
		case "suspend", "snapshot":
			out = append(out, swebench.Event{
				Time:     ts,
				RunID:    runID,
				Job:      jobName,
				Worker:   te.Worker,
				Event:    swebench.EventSuspend,
				Env:      te.Env,
				Instance: te.Instance,
				Image:    te.Image,
				Step:     te.Step,
				DurMS:    te.DurMS,
				SizeMB:   te.SizeMB,
				Bytes:    te.Bytes,
				OK:       te.OK,
				Err:      te.Err,
				Snapshot: te.Snapshot,
				Tags:     te.Tags,
			})
		case "reap":
			out = append(out, swebench.Event{
				Time:         ts,
				RunID:        runID,
				Job:          jobName,
				Worker:       te.Worker,
				Event:        swebench.EventReap,
				DurMS:        te.DurMS,
				DeletedCount: te.DeletedCount,
				OrphanCount:  te.OrphanCount,
				OK:           te.OK || te.Err == "",
				Err:          te.Err,
				Tags:         te.Tags,
				Metrics: map[string]float64{
					"deleted_count": float64(te.DeletedCount),
					"orphan_count":  float64(te.OrphanCount),
				},
			})
		case "episode", "rollout":
			// Handled below as EventTask.
		default:
			rpcOK := te.OK || (te.Err == "" && te.Exit != nil)
			isGrade := isGradeTraceRecord(te)
			if isGrade {
				phaseName = "grade"
			}
			out = append(out, swebench.Event{
				Time:     ts,
				RunID:    runID,
				Job:      jobName,
				Worker:   te.Worker,
				Event:    swebench.EventPhase,
				Name:     phaseName,
				Phase:    phaseName,
				Source:   source,
				Env:      te.Env,
				Instance: te.Instance,
				Image:    te.Image,
				Step:     te.Step,
				DurMS:    te.DurMS,
				OK:       rpcOK,
				Err:      te.Err,
				Snapshot: te.Snapshot,
				Tags:     te.Tags,
			})
			if isGrade {
				out = append(out, swebench.Event{
					Time:     ts,
					RunID:    runID,
					Job:      jobName,
					Worker:   te.Worker,
					Event:    swebench.EventStep,
					Name:     "grade",
					Phase:    "grade",
					Source:   source,
					Env:      te.Env,
					Instance: te.Instance,
					Image:    te.Image,
					Step:     te.Step,
					DurMS:    te.DurMS,
					OK:       rpcOK,
					Err:      te.Err,
					Tags:     te.Tags,
				})
			}
		}

		// When a trace records an execution or episode completion (`op` in
		// `exec`, `shell`, `episode`, `rollout`), emit TTFI on the first command
		// following a create/wake on the same env, and emit a task summary event
		// (for episode/rollout when episode events exist, or per exec otherwise).
		switch rawOp {
		case "exec", "shell", "episode", "rollout":
			rpcOK := te.OK || (te.Err == "" && te.Exit != nil)
			var rewardPtr *int
			if te.Reward != nil {
				r := 0
				if *te.Reward >= 0.5 {
					r = 1
				}
				rewardPtr = &r
			} else if te.Exit != nil {
				r := 0
				if *te.Exit == 0 {
					r = 1
				}
				rewardPtr = &r
			} else if te.OK {
				r := 1
				rewardPtr = &r
			}
			if pc, isFirst := pendingFirstCmd[te.Env]; isFirst && (rawOp == "exec" || rawOp == "shell" || !hasEpisodeOp) {
				delete(pendingFirstCmd, te.Env)
				out = append(out, swebench.Event{
					Time:     ts,
					RunID:    runID,
					Job:      jobName,
					Worker:   te.Worker,
					Event:    swebench.EventTTFI,
					Source:   source,
					Env:      te.Env,
					Instance: te.Instance,
					Image:    te.Image,
					Step:     te.Step,
					DurMS:    pc.slotWaitMS + pc.claimMS + te.DurMS,
					OK:       rpcOK,
					Err:      te.Err,
					Metrics: map[string]float64{
						"sdk_slot_wait_ms":       pc.slotWaitMS,
						"wire_claim_to_ready_ms": pc.claimMS,
						"ready_to_first_cmd_ms":  te.DurMS,
					},
				})
			}
			isTaskEvent := !hasEpisodeOp || rawOp == "episode" || rawOp == "rollout"
			if isTaskEvent {
				var taskMetrics map[string]float64
				if (rawOp == "episode" || rawOp == "rollout") && te.Env != "" {
					ew := envWaitByEnv[te.Env]
					delete(envWaitByEnv, te.Env)
					th := te.DurMS - ew
					if th < 0 {
						th = 0
					}
					taskMetrics = map[string]float64{
						"env_wait_ms":  ew,
						"llm_think_ms": th,
					}
				}
				out = append(out, swebench.Event{
					Time:     ts,
					RunID:    runID,
					Job:      jobName,
					Worker:   te.Worker,
					Event:    swebench.EventTask,
					Source:   source,
					Env:      te.Env,
					Instance: te.Instance,
					Image:    te.Image,
					Step:     te.Step,
					DurMS:    te.DurMS,
					OK:       rpcOK,
					Reward:   rewardPtr,
					Err:      te.Err,
					Tags:     te.Tags,
					Metrics:  taskMetrics,
				})
			}
		}
	}
	return out, nil
}

func normalizeTraceOp(op string) string {
	switch strings.ToLower(strings.TrimSpace(op)) {
	case "create", "init_env":
		return "create"
	case "wake", "acquire":
		return "wake"
	case "exec", "execute", "run_command", "shell":
		return "agent_exec"
	case "delete", "destroy", "close":
		return "delete"
	case "snapshot", "suspend":
		return "snapshot"
	case "resume", "restore":
		return "resume"
	default:
		return op
	}
}
