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

package schema

import (
	"errors"
	"fmt"
	"math"
	"strings"
	"time"
)

// AcquisitionSource identifies how an environment was acquired for a step/task.
type AcquisitionSource string

const (
	SourceWarmPool       AcquisitionSource = "warm_pool"
	SourceColdCreate     AcquisitionSource = "cold_create"
	SourceColdStart      AcquisitionSource = "cold_start"
	SourceGoldenRestore  AcquisitionSource = "golden_restore"
	SourceFork           AcquisitionSource = "fork"
	SourceResume         AcquisitionSource = "resume"
	SourceSnapshotResume AcquisitionSource = "snapshot_resume"
)

// Event is the unified JSONL event record emitted by swebench-loader,
// prewarm/reap stages, SDK telemetry, and trainer adapters.
type Event struct {
	SchemaVersion        string             `json:"schema_version,omitempty"`
	Time                 string             `json:"ts"`
	RunID                string             `json:"run"`
	Job                  string             `json:"job,omitempty"`
	Worker               string             `json:"worker,omitempty"`
	Event                string             `json:"event"`
	Name                 string             `json:"name,omitempty"`
	Phase                string             `json:"phase,omitempty"`
	Source               AcquisitionSource  `json:"source,omitempty"`
	Env                  string             `json:"env,omitempty"`
	Instance             string             `json:"instance,omitempty"`
	Image                string             `json:"image,omitempty"`
	Task                 int                `json:"task,omitempty"`
	Step                 int                `json:"step,omitempty"`
	DurMS                float64            `json:"dur_ms"`
	SlotWaitMS           float64            `json:"slot_wait_ms,omitempty"`
	OK                   bool               `json:"ok"`
	Timeout              *bool              `json:"timeout,omitempty"`
	Exit                 *int               `json:"exit,omitempty"`
	Reward               *int               `json:"reward,omitempty"`
	Detail               string             `json:"detail,omitempty"`
	Attempts             int                `json:"attempts,omitempty"`
	Count                int                `json:"count,omitempty"`
	Err                  string             `json:"err,omitempty"`
	Tail                 string             `json:"tail,omitempty"`
	Snapshot             string             `json:"snapshot,omitempty"`
	Node                 string             `json:"node,omitempty"`
	Bytes                int64              `json:"bytes,omitempty"`
	CacheHit             *bool              `json:"cache_hit,omitempty"`
	Labels               map[string]string  `json:"labels,omitempty"`
	Tags                 map[string]string  `json:"tags,omitempty"`
	Metrics              map[string]float64 `json:"metrics,omitempty"`
	WorkingSetHits       int                `json:"working_set_hits,omitempty"`
	WorkingSetMisses     int                `json:"working_set_misses,omitempty"`
	WorkingSetHitRate    float64            `json:"working_set_hit_rate,omitempty"`
	StandingPoolSize     int                `json:"standing_pool_size,omitempty"`
	Created              int                `json:"created,omitempty"`
	Reused               int                `json:"reused,omitempty"`
	Retired              int                `json:"retired,omitempty"`
	PrefetchLeadMarginMS float64            `json:"prefetch_lead_margin_ms,omitempty"`
	ReleaseOverlapMS     float64            `json:"release_overlap_ms,omitempty"`
}

// Validate checks Event invariants while remaining backwards-compatible with
// existing swebench baseline event streams (where schema_version is omitted).
func (e *Event) Validate() error {
	if e.SchemaVersion != "" && e.SchemaVersion != SchemaVersion {
		return fmt.Errorf("event: schema_version must be %q when set, got %q", SchemaVersion, e.SchemaVersion)
	}
	if err := requireRFC3339("event.ts", e.Time); err != nil {
		return err
	}
	if strings.TrimSpace(e.RunID) == "" {
		return errors.New("event: run is required")
	}
	if strings.TrimSpace(e.Event) == "" {
		return errors.New("event: event is required")
	}
	if e.DurMS < 0 || math.IsNaN(e.DurMS) || math.IsInf(e.DurMS, 0) {
		return fmt.Errorf("event: dur_ms must be a finite number >= 0, got %v", e.DurMS)
	}
	if e.SlotWaitMS < 0 || math.IsNaN(e.SlotWaitMS) || math.IsInf(e.SlotWaitMS, 0) {
		return fmt.Errorf("event: slot_wait_ms must be a finite number >= 0, got %v", e.SlotWaitMS)
	}
	if e.Task < 0 {
		return fmt.Errorf("event: task must be >= 0, got %d", e.Task)
	}
	if e.Step < 0 {
		return fmt.Errorf("event: step must be >= 0, got %d", e.Step)
	}
	if e.Attempts < 0 {
		return fmt.Errorf("event: attempts must be >= 0, got %d", e.Attempts)
	}
	if e.Count < 0 {
		return fmt.Errorf("event: count must be >= 0, got %d", e.Count)
	}
	if e.Bytes < 0 {
		return fmt.Errorf("event: bytes must be >= 0, got %d", e.Bytes)
	}
	switch e.Source {
	case "", SourceWarmPool, SourceColdCreate, SourceColdStart, SourceGoldenRestore, SourceFork, SourceResume, SourceSnapshotResume:
	default:
		return fmt.Errorf("event: invalid source %q", e.Source)
	}
	for k, v := range e.Metrics {
		if math.IsNaN(v) || math.IsInf(v, 0) {
			return fmt.Errorf("event: metrics[%q] must be finite", k)
		}
	}
	return nil
}

// RunWindow is the run_window.json artifact describing the active and
// steady-state time window of a benchmark run.
type RunWindow struct {
	SchemaVersion string            `json:"schema_version"`
	RunID         string            `json:"run_id"`
	TestName      string            `json:"test_name,omitempty"`
	Tag           string            `json:"tag,omitempty"`
	StartTS       string            `json:"start_ts"`
	EndTS         string            `json:"end_ts"`
	SteadyStartTS string            `json:"steady_start_ts,omitempty"`
	SteadyEndTS   string            `json:"steady_end_ts,omitempty"`
	Cluster       string            `json:"cluster,omitempty"`
	SUTEndpoint   string            `json:"sut_endpoint,omitempty"`
	Metadata      map[string]string `json:"metadata,omitempty"`
}

// Validate checks RunWindow invariants.
func (w *RunWindow) Validate() error {
	if err := checkSchemaVersion(w.SchemaVersion); err != nil {
		return fmt.Errorf("run_window: %w", err)
	}
	if strings.TrimSpace(w.RunID) == "" {
		return errors.New("run_window: run_id is required")
	}
	if err := requireRFC3339("run_window.start_ts", w.StartTS); err != nil {
		return err
	}
	if err := requireRFC3339("run_window.end_ts", w.EndTS); err != nil {
		return err
	}
	start, _ := time.Parse(time.RFC3339Nano, w.StartTS)
	end, _ := time.Parse(time.RFC3339Nano, w.EndTS)
	if end.Before(start) {
		return fmt.Errorf("run_window: end_ts (%s) is before start_ts (%s)", w.EndTS, w.StartTS)
	}
	if err := optionalRFC3339("run_window.steady_start_ts", w.SteadyStartTS); err != nil {
		return err
	}
	if err := optionalRFC3339("run_window.steady_end_ts", w.SteadyEndTS); err != nil {
		return err
	}
	if w.SteadyStartTS != "" && w.SteadyEndTS != "" {
		sStart, _ := time.Parse(time.RFC3339Nano, w.SteadyStartTS)
		sEnd, _ := time.Parse(time.RFC3339Nano, w.SteadyEndTS)
		if sEnd.Before(sStart) {
			return fmt.Errorf("run_window: steady_end_ts (%s) is before steady_start_ts (%s)", w.SteadyEndTS, w.SteadyStartTS)
		}
	}
	return nil
}
