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
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"github.com/agent-substrate/env/internal/benchmarking/measure"
	"github.com/agent-substrate/env/internal/benchmarking/schema"
	"github.com/agent-substrate/env/internal/benchmarking/swebench"
)

// IngestContext configures a scoped telemetry ingestion pass over an artifact directory.
type IngestContext struct {
	DestDir string
	RunID   string
	JobName string
	Scopes  []schema.TelemetryScope
	Plugins []schema.TelemetryPluginSpec
}

// IngestResult holds events and custom metrics harvested by one or more TelemetryIngesters.
type IngestResult struct {
	StepEvents    []swebench.Event
	TraceEvents   []swebench.Event
	CustomMetrics map[string]schema.MetricSummary
}

// TelemetryIngester extracts canonical events and custom metrics from logs or traces in DestDir.
type TelemetryIngester interface {
	Name() string
	Scope() schema.TelemetryScope
	Ingest(ctx IngestContext, out *IngestResult) error
}

var (
	ingestersMu      sync.RWMutex
	ingesterRegistry = make(map[string]TelemetryIngester)
	ingesterOrder    []string
)

// RegisterIngester registers a TelemetryIngester by its Name().
func RegisterIngester(ing TelemetryIngester) {
	if ing == nil {
		return
	}
	name := strings.TrimSpace(ing.Name())
	if name == "" {
		return
	}
	ingestersMu.Lock()
	defer ingestersMu.Unlock()
	if _, exists := ingesterRegistry[name]; !exists {
		ingesterOrder = append(ingesterOrder, name)
	}
	ingesterRegistry[name] = ing
}

// RegisteredIngesters returns all registered ingesters in registration order.
func RegisteredIngesters() []TelemetryIngester {
	ingestersMu.RLock()
	defer ingestersMu.RUnlock()
	out := make([]TelemetryIngester, 0, len(ingesterOrder))
	for _, name := range ingesterOrder {
		if ing, ok := ingesterRegistry[name]; ok {
			out = append(out, ing)
		}
	}
	return out
}

// IngestersForScopes returns all registered ingesters whose Scope() is enabled in scopes.
func IngestersForScopes(scopes []schema.TelemetryScope) []TelemetryIngester {
	all := RegisteredIngesters()
	var selected []TelemetryIngester
	for _, ing := range all {
		if schema.HasTelemetryScope(scopes, ing.Scope()) {
			selected = append(selected, ing)
		}
	}
	return selected
}

func init() {
	RegisterIngester(rlTrainerLogIngester{})
	RegisterIngester(userSandboxTraceIngester{})
}

type rlTrainerLogIngester struct{}

func (rlTrainerLogIngester) Name() string                 { return "rl-trainer-log" }
func (rlTrainerLogIngester) Scope() schema.TelemetryScope { return schema.ScopeRL }

func (rlTrainerLogIngester) Ingest(ctx IngestContext, out *IngestResult) error {
	if ctx.DestDir == "" {
		return nil
	}
	jobName := ctx.JobName
	if jobName == "" {
		jobName = "verl-qwen3-1p7b-fully-async"
	}
	for _, logName := range []string{"verl-head.log", "train.log", filepath.Join("results", "train.log")} {
		p := filepath.Join(ctx.DestDir, logName)
		if raw, err := os.ReadFile(p); err == nil && len(raw) > 0 {
			out.StepEvents = append(out.StepEvents, ParseTrainerLog(jobName, string(raw))...)
			break
		}
	}
	convPath := filepath.Join(ctx.DestDir, "training_convergence_summary.json")
	if _, err := os.Stat(convPath); err == nil {
		if merged, merr := MergeTrainingConvergenceSummary(convPath, jobName, out.StepEvents); merr == nil {
			out.StepEvents = merged
		}
	}
	return nil
}

type userSandboxTraceIngester struct{}

func (userSandboxTraceIngester) Name() string                 { return "user-sandbox-trace" }
func (userSandboxTraceIngester) Scope() schema.TelemetryScope { return schema.ScopeUserSandbox }

func (userSandboxTraceIngester) Ingest(ctx IngestContext, out *IngestResult) error {
	if ctx.DestDir == "" {
		return nil
	}
	jobName := ctx.JobName
	if jobName == "" {
		jobName = "verl-qwen3-1p7b-fully-async"
	}
	for _, traceName := range []string{"verl-live-trace.jsonl", "trace.jsonl"} {
		p := filepath.Join(ctx.DestDir, traceName)
		if _, err := os.Stat(p); err == nil {
			evs, perr := ParseTraceJSONLFile(p, jobName)
			if perr != nil {
				return perr
			}
			out.TraceEvents = append(out.TraceEvents, evs...)
			return nil
		}
	}
	for _, pattern := range []string{
		filepath.Join(ctx.DestDir, "traces", "*.jsonl"),
		filepath.Join(ctx.DestDir, "results", "traces", "*.jsonl"),
	} {
		tMatches, _ := filepath.Glob(pattern)
		if len(tMatches) > 0 {
			sort.Strings(tMatches)
			for _, tp := range tMatches {
				evs, perr := ParseTraceJSONLFile(tp, jobName)
				if perr != nil {
					return perr
				}
				out.TraceEvents = append(out.TraceEvents, evs...)
			}
			return nil
		}
	}
	return nil
}

// IngestArtifactDir runs all active TelemetryIngesters for ctx.Scopes and ctx.Plugins,
// aligns RL steps with user-sandbox episodes when both scopes are active, and writes
// events/verl.jsonl when step or trace events are discovered.
func IngestArtifactDir(ctx IngestContext) (*IngestResult, error) {
	res := &IngestResult{
		CustomMetrics: make(map[string]schema.MetricSummary),
	}
	for _, ing := range IngestersForScopes(ctx.Scopes) {
		if err := ing.Ingest(ctx, res); err != nil {
			return nil, err
		}
	}
	for _, p := range ctx.Plugins {
		if err := p.Validate(); err != nil {
			return nil, err
		}
		if p.Scope != "" && !schema.HasTelemetryScope(ctx.Scopes, p.Scope) {
			continue
		}
		switch p.Kind {
		case schema.PluginKindRegexMetric:
			ms, err := measure.ExtractRegexMetricSummary(ctx.DestDir, p.Glob, p.Regex, p.Unit)
			if err != nil {
				return nil, fmt.Errorf("telemetry_plugin[%q]: %w", p.Name, err)
			}
			res.CustomMetrics[p.Name] = ms
		case schema.PluginKindCustom:
			ingestersMu.RLock()
			customIng, ok := ingesterRegistry[p.Name]
			ingestersMu.RUnlock()
			if ok {
				if err := customIng.Ingest(ctx, res); err != nil {
					return nil, fmt.Errorf("telemetry_plugin[%q]: %w", p.Name, err)
				}
			}
		}
	}

	stepEvs := res.StepEvents
	traceEvs := res.TraceEvents
	if len(stepEvs) == 0 && len(traceEvs) == 0 {
		return res, nil
	}

	runID := ctx.RunID
	if runID == "" && ctx.DestDir != "" {
		runID = filepath.Base(ctx.DestDir)
	}
	jobName := ctx.JobName
	if jobName == "" {
		jobName = "verl-qwen3-1p7b-fully-async"
	}
	for _, ev := range traceEvs {
		if ev.RunID != "" {
			runID = ev.RunID
		}
		if ev.Job != "" {
			jobName = ev.Job
		}
		if ev.RunID != "" && ev.Job != "" {
			break
		}
	}
	for i := range stepEvs {
		stepEvs[i].Job = jobName
	}

	// Align trace episodes across RL steps when both rl and user-sandbox scopes are active.
	var trainStepEvs []swebench.Event
	for _, stEv := range stepEvs {
		if stEv.Step > 0 {
			trainStepEvs = append(trainStepEvs, stEv)
		}
	}
	if len(trainStepEvs) > 0 && len(traceEvs) > 0 {
		hasStepAttr := false
		var taskIndices []int
		for i := range traceEvs {
			if traceEvs[i].Event == swebench.EventTask {
				taskIndices = append(taskIndices, i)
				if traceEvs[i].Step > 0 {
					hasStepAttr = true
				}
			}
		}
		if !hasStepAttr && len(taskIndices) > 0 {
			sort.Slice(taskIndices, func(a, b int) bool {
				return traceEvs[taskIndices[a]].Time.Before(traceEvs[taskIndices[b]].Time)
			})
			envStep := make(map[string]int, len(taskIndices))
			numSteps := len(trainStepEvs)
			totalTasks := len(taskIndices)
			for sIdx, stEv := range trainStepEvs {
				stepNum := stEv.Step
				if stepNum <= 0 {
					stepNum = sIdx + 1
				}
				start := (sIdx * totalTasks) / numSteps
				end := ((sIdx + 1) * totalTasks) / numSteps
				stepSlice := taskIndices[start:end]
				scoreMean := -1.0
				if stEv.Metrics != nil {
					if v, ok := stEv.Metrics["critic/score/mean"]; ok {
						scoreMean = v
					} else if v, ok := stEv.Metrics["reward_mean"]; ok {
						scoreMean = v
					}
				}
				rewardedTarget := -1
				if scoreMean >= 0 {
					rewardedTarget = int(math.Round(scoreMean * float64(len(stepSlice))))
				}
				for k, tIdx := range stepSlice {
					traceEvs[tIdx].Step = stepNum
					if traceEvs[tIdx].Env != "" {
						envStep[traceEvs[tIdx].Env] = stepNum
					}
					if rewardedTarget >= 0 {
						r := 0
						if k < rewardedTarget {
							r = 1
						}
						traceEvs[tIdx].Reward = &r
					}
				}
			}
			for i := range traceEvs {
				if traceEvs[i].Step == 0 && traceEvs[i].Env != "" {
					if stNum, ok := envStep[traceEvs[i].Env]; ok {
						traceEvs[i].Step = stNum
					}
				}
			}
		}
	}

	if len(traceEvs) > 0 {
		hasReap := false
		deletedEnvs := make(map[string]bool)
		var lastDeleteDurMS float64
		for _, ev := range traceEvs {
			if ev.Event == swebench.EventReap {
				hasReap = true
			}
			if ev.Event == swebench.EventDelete && ev.OK {
				if ev.Env != "" {
					deletedEnvs[ev.Env] = true
				}
				if ev.DurMS > 0 {
					lastDeleteDurMS = ev.DurMS
				}
			}
		}
		if !hasReap && len(deletedEnvs) > 0 {
			traceEvs = append(traceEvs, swebench.Event{
				RunID:        runID,
				Job:          jobName,
				Event:        swebench.EventReap,
				DurMS:        lastDeleteDurMS,
				DeletedCount: len(deletedEnvs),
				OrphanCount:  0,
				OK:           true,
				Metrics: map[string]float64{
					"deleted_count": float64(len(deletedEnvs)),
					"orphan_count":  0,
				},
			})
			res.TraceEvents = traceEvs
		}
	}

	if ctx.DestDir != "" {
		eventsDir := filepath.Join(ctx.DestDir, "events")
		if err := os.MkdirAll(eventsDir, 0o755); err != nil {
			return nil, err
		}
		rec, err := swebench.NewFileRecorder(filepath.Join(eventsDir, "verl.jsonl"), runID)
		if err != nil {
			return nil, err
		}
		var totalMS float64
		for _, ev := range stepEvs {
			ev.RunID = runID
			totalMS += ev.DurMS
			rec.Record(ev)
		}
		for _, ev := range traceEvs {
			ev.RunID = runID
			rec.Record(ev)
		}
		if totalMS > 0 {
			rec.Record(swebench.Event{
				RunID: runID,
				Job:   jobName,
				Event: swebench.EventRun,
				Name:  "steps",
				DurMS: totalMS,
				OK:    true,
			})
		}
		if err := rec.Close(); err != nil {
			return nil, err
		}
	}
	return res, nil
}
