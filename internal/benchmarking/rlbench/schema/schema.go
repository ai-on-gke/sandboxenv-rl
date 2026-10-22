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

// Package schema defines versioned wire contracts, embedded JSON Schemas, and
// strict decoders for RL benchmark scenarios, schedules, placements, run
// windows, traces, events, measurements, node-cache snapshots, restore
// samples, system descriptions, reports, and stats rows.
package schema

import (
	"bufio"
	"bytes"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path"
	"strings"
	"time"

	"github.com/google/jsonschema-go/jsonschema"
	"sigs.k8s.io/yaml"
)

// SchemaVersion is the wire schema version for all W1 RL benchmark artifacts.
const SchemaVersion = "v1"

// SchemaFS embeds the canonical W1 JSON Schema definitions under v1/*.schema.json.
//
//go:embed v1/*.schema.json
var SchemaFS embed.FS

// Validator is implemented by schema types that enforce semantic invariants.
type Validator interface {
	Validate() error
}

// ParkPolicy enumerates how a step-mode working set parks environments between steps.
type ParkPolicy string

const (
	// ParkSuspendAsync suspends with asynchronous durability (on-node snapshot with background upload).
	ParkSuspendAsync ParkPolicy = "suspend-async"
	// ParkSuspendSync suspends with synchronous durability (waits for durable object storage).
	ParkSuspendSync ParkPolicy = "suspend-sync"
	// ParkFork parks via snapshot/fork semantics.
	ParkFork ParkPolicy = "fork"
	// ParkDelete deletes environments at the step boundary instead of parking.
	ParkDelete ParkPolicy = "delete"
	// ParkNone leaves environments unparked across step boundaries.
	ParkNone ParkPolicy = "none"
	// ParkLegacySuspend is the legacy swebench-loader "suspend" policy, which maps
	// to ParkSuspendSync per DESIGN §5.4 (ate-env-api hardcodes DURABILITY_MODE_SYNCHRONOUS).
	ParkLegacySuspend ParkPolicy = "suspend"
)

// NormalizeParkPolicy validates p against the canonical W1 park policies and
// normalizes legacy "suspend" to ParkSuspendSync per DESIGN §5.4.
func NormalizeParkPolicy(p ParkPolicy) (ParkPolicy, error) {
	switch p {
	case ParkSuspendAsync, ParkSuspendSync, ParkFork, ParkDelete, ParkNone:
		return p, nil
	case ParkLegacySuspend:
		return ParkSuspendSync, nil
	default:
		return "", fmt.Errorf("invalid park policy %q (want %q, %q, %q, %q, %q, or legacy %q)",
			p, ParkSuspendAsync, ParkSuspendSync, ParkFork, ParkDelete, ParkNone, ParkLegacySuspend)
	}
}

// Scenario defines one benchmark run against a System Under Test (SUT).
type Scenario struct {
	SchemaVersion string           `json:"schema_version"`
	Name          string           `json:"name"`
	Repeat        int              `json:"repeat,omitempty"`
	SUT           SUTSpec          `json:"sut"`
	Jobs          []JobSpec        `json:"jobs"`
	WindowPolicy  WindowPolicy     `json:"window_policy,omitempty"`
	Variants      []FeatureOverlay `json:"variants,omitempty"`
}

// Validate checks Scenario invariants.
func (s Scenario) Validate() error {
	if s.SchemaVersion != SchemaVersion {
		return fmt.Errorf("scenario: unsupported schema_version %q (want %q)", s.SchemaVersion, SchemaVersion)
	}
	if s.Name == "" {
		return errors.New("scenario: name is required")
	}
	if s.Repeat < 0 {
		return fmt.Errorf("scenario: repeat must be >= 0, got %d", s.Repeat)
	}
	if err := s.SUT.Validate(); err != nil {
		return fmt.Errorf("scenario sut: %w", err)
	}
	if len(s.Jobs) == 0 {
		return errors.New("scenario: at least one job is required")
	}
	seenJobs := make(map[string]bool, len(s.Jobs))
	for i, j := range s.Jobs {
		if err := j.Validate(); err != nil {
			return fmt.Errorf("scenario job[%d]: %w", i, err)
		}
		if seenJobs[j.Name] {
			return fmt.Errorf("scenario: duplicate job name %q", j.Name)
		}
		seenJobs[j.Name] = true
	}
	if err := s.WindowPolicy.Validate(); err != nil {
		return fmt.Errorf("scenario window_policy: %w", err)
	}
	for i, v := range s.Variants {
		if err := v.Validate(); err != nil {
			return fmt.Errorf("scenario variant[%d]: %w", i, err)
		}
	}
	return nil
}

// SUTSpec describes the System Under Test configuration.
type SUTSpec struct {
	Backend      string            `json:"backend,omitempty"`
	Access       string            `json:"access,omitempty"`
	Endpoint     string            `json:"endpoint,omitempty"`
	Substrate    *SubstrateSUTSpec `json:"substrate,omitempty"`
	NodePool     string            `json:"node_pool,omitempty"`
	RuntimeClass string            `json:"runtime_class,omitempty"`
	Replicas     int               `json:"replicas,omitempty"`
	ExtraArgs    map[string]string `json:"extra_args,omitempty"`
}

// SubstrateSUTSpec holds Substrate-specific SUT configuration parameters.
type SubstrateSUTSpec struct {
	Atespace     string `json:"atespace,omitempty"`
	BaseTemplate string `json:"base_template,omitempty"`
}

// Validate checks SUTSpec invariants.
func (s SUTSpec) Validate() error {
	if s.Backend != "" && s.Backend != "substrate" && s.Backend != "agent-sandbox" {
		return fmt.Errorf("invalid backend %q (want \"substrate\" or \"agent-sandbox\")", s.Backend)
	}
	if s.Access != "" && s.Access != "sdk" && s.Access != "ate-env-api" && s.Access != "direct" {
		return fmt.Errorf("invalid access %q (want \"sdk\", \"ate-env-api\", or \"direct\")", s.Access)
	}
	if s.Replicas < 0 {
		return fmt.Errorf("replicas must be >= 0, got %d", s.Replicas)
	}
	return nil
}

// JobSpec defines one simulated or real RL job loop within a Scenario.
type JobSpec struct {
	Name          string     `json:"name"`
	Kind          string     `json:"kind"`
	LoaderMode    string     `json:"loader_mode,omitempty"`
	RealMode      string     `json:"real_mode,omitempty"`
	StartOffsetMS float64    `json:"start_offset_ms,omitempty"`
	Dataset       string     `json:"dataset,omitempty"`
	Trajectory    string     `json:"trajectory,omitempty"`
	Park          ParkPolicy `json:"park,omitempty"`
	Images        int        `json:"images,omitempty"`
	TasksPerStep  int        `json:"tasks_per_step,omitempty"`
	GroupSize     int        `json:"group_size,omitempty"`
	Steps         int        `json:"steps,omitempty"`
	MaxConcurrent int        `json:"max_concurrent,omitempty"`
	StepPauseMS   float64    `json:"step_pause_ms,omitempty"`
	Prefetch      bool       `json:"prefetch,omitempty"`
	Seed          int64      `json:"seed,omitempty"`
	ScheduleMode  string     `json:"schedule_mode,omitempty"`
}

// Validate checks JobSpec invariants.
func (j JobSpec) Validate() error {
	if j.Name == "" {
		return errors.New("job name is required")
	}
	switch j.Kind {
	case "simulated":
		if j.Dataset == "" || j.Trajectory == "" {
			return fmt.Errorf("job %q: simulated job requires dataset and trajectory", j.Name)
		}
		if j.RealMode != "" {
			return fmt.Errorf("job %q: simulated job cannot set real_mode %q", j.Name, j.RealMode)
		}
		if j.LoaderMode != "" && j.LoaderMode != "eval" && j.LoaderMode != "steps" && j.LoaderMode != "open-loop" {
			return fmt.Errorf("job %q: invalid loader_mode %q (want \"eval\", \"steps\", or \"open-loop\")", j.Name, j.LoaderMode)
		}
		if j.LoaderMode == "steps" && j.Steps < 1 {
			return fmt.Errorf("job %q: steps loader_mode requires steps >= 1, got %d", j.Name, j.Steps)
		}
	case "real":
		if j.LoaderMode != "" {
			return fmt.Errorf("job %q: real job cannot set loader_mode %q", j.Name, j.LoaderMode)
		}
		if j.RealMode != "measure" && j.RealMode != "trace" {
			return fmt.Errorf("job %q: real job requires real_mode \"measure\" or \"trace\", got %q", j.Name, j.RealMode)
		}
	default:
		return fmt.Errorf("job %q: invalid kind %q (want \"simulated\" or \"real\")", j.Name, j.Kind)
	}
	if j.ScheduleMode != "" && j.ScheduleMode != "fixed" && j.ScheduleMode != "sampled" && j.ScheduleMode != "explicit" {
		return fmt.Errorf("job %q: invalid schedule_mode %q (want \"fixed\", \"sampled\", or \"explicit\")", j.Name, j.ScheduleMode)
	}
	if j.Park != "" {
		if _, err := NormalizeParkPolicy(j.Park); err != nil {
			return fmt.Errorf("job %q: %w", j.Name, err)
		}
	}
	if j.Images < 0 || j.TasksPerStep < 0 || j.GroupSize < 0 || j.Steps < 0 || j.MaxConcurrent < 0 {
		return fmt.Errorf("job %q: numeric counts must be >= 0", j.Name)
	}
	if j.StartOffsetMS < 0 || j.StepPauseMS < 0 {
		return fmt.Errorf("job %q: start_offset_ms and step_pause_ms must be >= 0", j.Name)
	}
	return nil
}

// WindowPolicy configures steady-state step boundaries and run duration caps inside Scenario.
type WindowPolicy struct {
	SteadyStateStartStep int     `json:"steady_state_start_step,omitempty"`
	SteadyStateEndStep   int     `json:"steady_state_end_step,omitempty"`
	WarmupSteps          int     `json:"warmup_steps,omitempty"`
	MaxDurationS         float64 `json:"max_duration_s,omitempty"`
}

// Validate checks WindowPolicy invariants.
func (w WindowPolicy) Validate() error {
	if w.SteadyStateStartStep < 0 || w.SteadyStateEndStep < 0 || w.WarmupSteps < 0 {
		return errors.New("step bounds must be >= 0")
	}
	if w.SteadyStateEndStep > 0 && w.SteadyStateStartStep > w.SteadyStateEndStep {
		return fmt.Errorf("steady_state_start_step (%d) > steady_state_end_step (%d)", w.SteadyStateStartStep, w.SteadyStateEndStep)
	}
	if w.MaxDurationS < 0 {
		return fmt.Errorf("max_duration_s must be >= 0, got %f", w.MaxDurationS)
	}
	return nil
}

// FeatureOverlay bundles A/B feature overrides onto a base Scenario.
type FeatureOverlay struct {
	Name        string            `json:"name"`
	Description string            `json:"description,omitempty"`
	Park        ParkPolicy        `json:"park,omitempty"`
	Prefetch    *bool             `json:"prefetch,omitempty"`
	EnvVars     map[string]string `json:"env_vars,omitempty"`
	Flags       map[string]string `json:"flags,omitempty"`
}

// Validate checks FeatureOverlay invariants.
func (o FeatureOverlay) Validate() error {
	if o.Name == "" {
		return errors.New("feature overlay name is required")
	}
	if o.Park != "" {
		if _, err := NormalizeParkPolicy(o.Park); err != nil {
			return fmt.Errorf("feature overlay %q: %w", o.Name, err)
		}
	}
	return nil
}

// Schedule is the deterministic resolved per-step task schedule (schedule.json).
type Schedule struct {
	SchemaVersion string         `json:"schema_version"`
	ScenarioName  string         `json:"scenario_name"`
	Seed          int64          `json:"seed,omitempty"`
	Jobs          []ScheduledJob `json:"jobs"`
}

// Validate checks Schedule invariants.
func (s Schedule) Validate() error {
	if s.SchemaVersion != SchemaVersion {
		return fmt.Errorf("schedule: unsupported schema_version %q (want %q)", s.SchemaVersion, SchemaVersion)
	}
	if s.ScenarioName == "" {
		return errors.New("schedule: scenario_name is required")
	}
	if len(s.Jobs) == 0 {
		return errors.New("schedule: at least one scheduled job is required")
	}
	for i, j := range s.Jobs {
		if err := j.Validate(); err != nil {
			return fmt.Errorf("schedule job[%d]: %w", i, err)
		}
	}
	return nil
}

// ScheduledJob holds the resolved step list for one job.
type ScheduledJob struct {
	Job           string          `json:"job"`
	StartOffsetMS float64         `json:"start_offset_ms,omitempty"`
	Steps         []ScheduledStep `json:"steps"`
}

// Validate checks ScheduledJob invariants.
func (j ScheduledJob) Validate() error {
	if j.Job == "" {
		return errors.New("job name is required")
	}
	if j.StartOffsetMS < 0 {
		return fmt.Errorf("job %q: start_offset_ms must be >= 0, got %f", j.Job, j.StartOffsetMS)
	}
	if len(j.Steps) == 0 {
		return fmt.Errorf("job %q: at least one step is required", j.Job)
	}
	for i, st := range j.Steps {
		if err := st.Validate(); err != nil {
			return fmt.Errorf("job %q step[%d]: %w", j.Job, i, err)
		}
	}
	return nil
}

// ScheduledStep holds the tasks and prefetch list for a single step.
type ScheduledStep struct {
	Step           int             `json:"step"`
	StepPauseMS    float64         `json:"step_pause_ms,omitempty"`
	Tasks          []ScheduledTask `json:"tasks"`
	PrefetchImages []string        `json:"prefetch_images,omitempty"`
}

// Validate checks ScheduledStep invariants.
func (s ScheduledStep) Validate() error {
	if s.Step <= 0 {
		return fmt.Errorf("step must be >= 1, got %d", s.Step)
	}
	if s.StepPauseMS < 0 {
		return fmt.Errorf("step %d: step_pause_ms must be >= 0, got %f", s.Step, s.StepPauseMS)
	}
	if len(s.Tasks) == 0 {
		return fmt.Errorf("step %d: at least one task is required", s.Step)
	}
	for i, t := range s.Tasks {
		if err := t.Validate(); err != nil {
			return fmt.Errorf("step %d task[%d]: %w", s.Step, i, err)
		}
	}
	return nil
}

// ScheduledTask specifies one task rollout within a ScheduledStep.
type ScheduledTask struct {
	InstanceID string `json:"instance_id"`
	Image      string `json:"image"`
	Trajectory string `json:"trajectory,omitempty"`
	Rollout    int    `json:"rollout,omitempty"`
}

// Validate checks ScheduledTask invariants.
func (t ScheduledTask) Validate() error {
	if t.InstanceID == "" {
		return errors.New("instance_id is required")
	}
	if t.Image == "" {
		return errors.New("image is required")
	}
	if t.Rollout < 0 {
		return fmt.Errorf("rollout must be >= 0, got %d", t.Rollout)
	}
	return nil
}

// PlacementPlan is an optional separate artifact (placement.json) for scripting
// explicit sandbox-assignment decisions keyed by (step, instance_id, rollout).
type PlacementPlan struct {
	SchemaVersion string             `json:"schema_version"`
	ScenarioName  string             `json:"scenario_name"`
	Jobs          []JobPlacementPlan `json:"jobs"`
}

// Validate checks PlacementPlan invariants.
func (p PlacementPlan) Validate() error {
	if p.SchemaVersion != SchemaVersion {
		return fmt.Errorf("placement: unsupported schema_version %q (want %q)", p.SchemaVersion, SchemaVersion)
	}
	if p.ScenarioName == "" {
		return errors.New("placement: scenario_name is required")
	}
	if len(p.Jobs) == 0 {
		return errors.New("placement: at least one job placement plan is required")
	}
	for i, j := range p.Jobs {
		if err := j.Validate(); err != nil {
			return fmt.Errorf("placement job[%d]: %w", i, err)
		}
	}
	return nil
}

// JobPlacementPlan holds scripted sandbox assignments for one job.
type JobPlacementPlan struct {
	Job         string          `json:"job"`
	Assignments []TaskPlacement `json:"assignments"`
}

// Validate checks JobPlacementPlan invariants.
func (j JobPlacementPlan) Validate() error {
	if j.Job == "" {
		return errors.New("job name is required")
	}
	if len(j.Assignments) == 0 {
		return fmt.Errorf("job %q: at least one assignment is required", j.Job)
	}
	for i, a := range j.Assignments {
		if err := a.Validate(); err != nil {
			return fmt.Errorf("job %q assignment[%d]: %w", j.Job, i, err)
		}
	}
	return nil
}

// TaskPlacement maps a (step, instance_id, rollout) tuple to a scripted environment action.
type TaskPlacement struct {
	Step         int    `json:"step"`
	InstanceID   string `json:"instance_id"`
	Rollout      int    `json:"rollout,omitempty"`
	EnvID        string `json:"env_id"`
	ReuseEnvID   string `json:"reuse_env_id,omitempty"`
	FromSnapshot string `json:"from_snapshot,omitempty"`
	RetireAfter  bool   `json:"retire_after,omitempty"`
}

// Validate checks TaskPlacement invariants.
func (p TaskPlacement) Validate() error {
	if p.Step <= 0 {
		return fmt.Errorf("step must be >= 1, got %d", p.Step)
	}
	if p.InstanceID == "" {
		return errors.New("instance_id is required")
	}
	if p.Rollout < 0 {
		return fmt.Errorf("rollout must be >= 0, got %d", p.Rollout)
	}
	if p.EnvID == "" {
		return errors.New("env_id is required")
	}
	return nil
}

// RunWindow is the run_window.json artifact written by the job runner or external
// harness and read by bench measure, reap, and report.
type RunWindow struct {
	SchemaVersion string            `json:"schema_version"`
	RunID         string            `json:"run_id"`
	TestName      string            `json:"test_name,omitempty"`
	Tag           string            `json:"tag,omitempty"`
	StartTS       time.Time         `json:"start_ts"`
	EndTS         time.Time         `json:"end_ts"`
	SteadyStartTS *time.Time        `json:"steady_start_ts,omitempty"`
	SteadyEndTS   *time.Time        `json:"steady_end_ts,omitempty"`
	Cluster       string            `json:"cluster,omitempty"`
	SUTEndpoint   string            `json:"sut_endpoint,omitempty"`
	Metadata      map[string]string `json:"metadata,omitempty"`
}

// Validate checks RunWindow invariants.
func (w RunWindow) Validate() error {
	if w.SchemaVersion != SchemaVersion {
		return fmt.Errorf("run_window: unsupported schema_version %q (want %q)", w.SchemaVersion, SchemaVersion)
	}
	if w.RunID == "" {
		return errors.New("run_window: run_id is required")
	}
	if w.StartTS.IsZero() {
		return errors.New("run_window: start_ts is required")
	}
	if w.EndTS.IsZero() {
		return errors.New("run_window: end_ts is required")
	}
	if w.EndTS.Before(w.StartTS) {
		return fmt.Errorf("run_window: end_ts (%s) is before start_ts (%s)", w.EndTS.Format(time.RFC3339Nano), w.StartTS.Format(time.RFC3339Nano))
	}
	if w.SteadyStartTS != nil {
		if w.SteadyStartTS.Before(w.StartTS) || w.SteadyStartTS.After(w.EndTS) {
			return errors.New("run_window: steady_start_ts must lie within [start_ts, end_ts]")
		}
	}
	if w.SteadyEndTS != nil {
		if w.SteadyEndTS.Before(w.StartTS) || w.SteadyEndTS.After(w.EndTS) {
			return errors.New("run_window: steady_end_ts must lie within [start_ts, end_ts]")
		}
	}
	if w.SteadyStartTS != nil && w.SteadyEndTS != nil && w.SteadyEndTS.Before(*w.SteadyStartTS) {
		return errors.New("run_window: steady_end_ts is before steady_start_ts")
	}
	return nil
}

// TraceEvent records a single SDK call in verbose trace mode (trace.jsonl).
type TraceEvent struct {
	SchemaVersion string    `json:"schema_version,omitempty"`
	TS            time.Time `json:"ts"`
	Run           string    `json:"run,omitempty"`
	Job           string    `json:"job,omitempty"`
	Env           string    `json:"env"`
	Instance      string    `json:"instance,omitempty"`
	Image         string    `json:"image,omitempty"`
	Step          int       `json:"step,omitempty"`
	Op            string    `json:"op"`
	Command       []string  `json:"command,omitempty"`
	Path          string    `json:"path,omitempty"`
	Bytes         int64     `json:"bytes,omitempty"`
	DurMS         float64   `json:"dur_ms"`
	Exit          *int      `json:"exit,omitempty"`
	OK            bool      `json:"ok"`
	Err           string    `json:"err,omitempty"`
}

// Validate checks TraceEvent invariants.
func (t TraceEvent) Validate() error {
	if t.SchemaVersion != "" && t.SchemaVersion != SchemaVersion {
		return fmt.Errorf("trace_event: unsupported schema_version %q (want %q)", t.SchemaVersion, SchemaVersion)
	}
	if t.TS.IsZero() {
		return errors.New("trace_event: ts is required")
	}
	if t.Env == "" {
		return errors.New("trace_event: env is required")
	}
	switch t.Op {
	case "create", "exec", "write_file", "read_file", "suspend", "wake", "fork", "delete", "get", "start_process", "shell":
	case "":
		return errors.New("trace_event: op is required")
	default:
		return fmt.Errorf("trace_event: invalid op %q", t.Op)
	}
	if t.DurMS < 0 || t.Bytes < 0 || t.Step < 0 {
		return errors.New("trace_event: dur_ms, bytes, and step must be >= 0")
	}
	return nil
}

// Event is a 100% backward-compatible superset of swebench.Event emitted to events/*.jsonl.
//
// SchemaVersion is optional per line (defaulting to SchemaVersion "v1" when omitted)
// so high-volume event shards and legacy swebench events.jsonl files do not pay
// per-line versioning overhead.
//
// Task carries the task/rollout index within a step or evaluation run (matching
// swebench.Event.Task so there is a single canonical rollout index field).
type Event struct {
	SchemaVersion string             `json:"schema_version,omitempty"`
	TS            time.Time          `json:"ts"`
	Run           string             `json:"run"`
	Job           string             `json:"job,omitempty"`
	Worker        string             `json:"worker,omitempty"`
	Event         string             `json:"event"`
	Name          string             `json:"name,omitempty"`
	Phase         string             `json:"phase,omitempty"`
	Source        string             `json:"source,omitempty"`
	Env           string             `json:"env,omitempty"`
	Instance      string             `json:"instance,omitempty"`
	Image         string             `json:"image,omitempty"`
	Task          int                `json:"task,omitempty"`
	Step          int                `json:"step,omitempty"`
	DurMS         float64            `json:"dur_ms"`
	SlotWaitMS    *float64           `json:"slot_wait_ms,omitempty"`
	OK            bool               `json:"ok"`
	Timeout       *bool              `json:"timeout,omitempty"`
	Exit          *int               `json:"exit,omitempty"`
	Reward        *int               `json:"reward,omitempty"`
	Attempts      int                `json:"attempts,omitempty"`
	Count         int                `json:"count,omitempty"`
	Err           string             `json:"err,omitempty"`
	Tail          string             `json:"tail,omitempty"`
	Snapshot      string             `json:"snapshot,omitempty"`
	Node          string             `json:"node,omitempty"`
	Bytes         int64              `json:"bytes,omitempty"`
	CacheHit      *bool              `json:"cache_hit,omitempty"`
	Tags          map[string]string  `json:"tags,omitempty"`
	Metrics       map[string]float64 `json:"metrics,omitempty"`
}

// Validate checks Event invariants while remaining compatible with swebench.Event records.
func (e Event) Validate() error {
	if e.SchemaVersion != "" && e.SchemaVersion != SchemaVersion {
		return fmt.Errorf("event: unsupported schema_version %q (want %q)", e.SchemaVersion, SchemaVersion)
	}
	if e.TS.IsZero() {
		return errors.New("event: ts is required")
	}
	if e.Run == "" {
		return errors.New("event: run is required")
	}
	if e.Event == "" {
		return errors.New("event: event name is required")
	}
	if e.DurMS < 0 {
		return fmt.Errorf("event: dur_ms must be >= 0, got %f", e.DurMS)
	}
	if e.SlotWaitMS != nil && *e.SlotWaitMS < 0 {
		return fmt.Errorf("event: slot_wait_ms must be >= 0, got %f", *e.SlotWaitMS)
	}
	if e.Task < 0 || e.Step < 0 || e.Attempts < 0 || e.Count < 0 || e.Bytes < 0 {
		return errors.New("event: task, step, attempts, count, and bytes must be >= 0")
	}
	if e.Source != "" {
		switch e.Source {
		case "warm_pool", "cold_create", "golden_restore", "fork", "resume":
		default:
			return fmt.Errorf("event: invalid source %q (want \"warm_pool\", \"cold_create\", \"golden_restore\", \"fork\", or \"resume\")", e.Source)
		}
	}
	return nil
}

// MetricSummary represents pre-aggregated summary statistics for a server or custom metric.
type MetricSummary struct {
	Count int64    `json:"count"`
	Sum   *float64 `json:"sum,omitempty"`
	Min   *float64 `json:"min,omitempty"`
	Max   *float64 `json:"max,omitempty"`
	P50   *float64 `json:"p50,omitempty"`
	P90   *float64 `json:"p90,omitempty"`
	P95   *float64 `json:"p95,omitempty"`
	P99   *float64 `json:"p99,omitempty"`
	Unit  string   `json:"unit,omitempty"`
}

// Validate checks MetricSummary invariants.
func (m MetricSummary) Validate() error {
	if m.Count < 0 {
		return fmt.Errorf("count must be >= 0, got %d", m.Count)
	}
	return nil
}

// Measurements holds server-side telemetry, references or inline samples for node
// image-cache and restore timings, and typed custom_metrics extensions.
type Measurements struct {
	SchemaVersion     string                   `json:"schema_version"`
	RunID             string                   `json:"run_id"`
	CollectedAt       time.Time                `json:"collected_at"`
	StartTS           *time.Time               `json:"start_ts,omitempty"`
	EndTS             *time.Time               `json:"end_ts,omitempty"`
	CacheBeforeRef    string                   `json:"cache_before_ref,omitempty"`
	CacheAfterRef     string                   `json:"cache_after_ref,omitempty"`
	RestoreTimingsRef string                   `json:"restore_timings_ref,omitempty"`
	LogsArchive       string                   `json:"logs_archive,omitempty"`
	NodeCacheBefore   []NodeCacheSample        `json:"node_cache_before,omitempty"`
	NodeCacheAfter    []NodeCacheSample        `json:"node_cache_after,omitempty"`
	Restores          []RestoreSample          `json:"restores,omitempty"`
	PromMetrics       []PrometheusSeries       `json:"prom_metrics,omitempty"`
	ServerSummary     map[string]MetricSummary `json:"server_summary,omitempty"`
	CustomMetrics     map[string]MetricSummary `json:"custom_metrics,omitempty"`
}

// Validate checks Measurements invariants.
func (m Measurements) Validate() error {
	if m.SchemaVersion != SchemaVersion {
		return fmt.Errorf("measurements: unsupported schema_version %q (want %q)", m.SchemaVersion, SchemaVersion)
	}
	if m.RunID == "" {
		return errors.New("measurements: run_id is required")
	}
	if m.CollectedAt.IsZero() {
		return errors.New("measurements: collected_at is required")
	}
	if m.StartTS != nil && m.EndTS != nil && m.EndTS.Before(*m.StartTS) {
		return errors.New("measurements: end_ts is before start_ts")
	}
	for i, s := range m.NodeCacheBefore {
		if err := s.Validate(); err != nil {
			return fmt.Errorf("measurements node_cache_before[%d]: %w", i, err)
		}
	}
	for i, s := range m.NodeCacheAfter {
		if err := s.Validate(); err != nil {
			return fmt.Errorf("measurements node_cache_after[%d]: %w", i, err)
		}
	}
	for i, r := range m.Restores {
		if err := r.Validate(); err != nil {
			return fmt.Errorf("measurements restores[%d]: %w", i, err)
		}
	}
	for i, p := range m.PromMetrics {
		if err := p.Validate(); err != nil {
			return fmt.Errorf("measurements prom_metrics[%d]: %w", i, err)
		}
	}
	for k, sm := range m.ServerSummary {
		if err := sm.Validate(); err != nil {
			return fmt.Errorf("measurements server_summary[%q]: %w", k, err)
		}
	}
	for k, cm := range m.CustomMetrics {
		if err := cm.Validate(); err != nil {
			return fmt.Errorf("measurements custom_metrics[%q]: %w", k, err)
		}
	}
	return nil
}

// NodeCacheArtifact is the standalone cache_before.json / cache_after.json artifact.
type NodeCacheArtifact struct {
	SchemaVersion string            `json:"schema_version"`
	RunID         string            `json:"run_id"`
	CollectedAt   time.Time         `json:"collected_at"`
	Nodes         []NodeCacheSample `json:"nodes"`
}

// Validate checks NodeCacheArtifact invariants.
func (a NodeCacheArtifact) Validate() error {
	if a.SchemaVersion != SchemaVersion {
		return fmt.Errorf("node_cache: unsupported schema_version %q (want %q)", a.SchemaVersion, SchemaVersion)
	}
	if a.RunID == "" {
		return errors.New("node_cache: run_id is required")
	}
	if a.CollectedAt.IsZero() {
		return errors.New("node_cache: collected_at is required")
	}
	if len(a.Nodes) == 0 {
		return errors.New("node_cache: at least one node sample is required")
	}
	for i, n := range a.Nodes {
		if err := n.Validate(); err != nil {
			return fmt.Errorf("node_cache nodes[%d]: %w", i, err)
		}
	}
	return nil
}

// NodeCacheSample captures per-node image/snapshot cache counters at a point in time.
// Optional counters use pointers so measured 0 is preserved and distinguished from
// missing/uncollected counters (DESIGN §7.3).
type NodeCacheSample struct {
	Node      string    `json:"node"`
	TS        time.Time `json:"ts,omitempty"`
	Hits      int64     `json:"hits"`
	Misses    int64     `json:"misses"`
	Errors    *int64    `json:"errors,omitempty"`
	Evictions *int64    `json:"evictions,omitempty"`
	BytesUsed *int64    `json:"bytes_used,omitempty"`
}

// Validate checks NodeCacheSample invariants.
func (n NodeCacheSample) Validate() error {
	if n.Node == "" {
		return errors.New("node is required")
	}
	if n.Hits < 0 || n.Misses < 0 {
		return fmt.Errorf("node %q: hits and misses must be >= 0", n.Node)
	}
	if n.Errors != nil && *n.Errors < 0 {
		return fmt.Errorf("node %q: errors must be >= 0", n.Node)
	}
	if n.Evictions != nil && *n.Evictions < 0 {
		return fmt.Errorf("node %q: evictions must be >= 0", n.Node)
	}
	if n.BytesUsed != nil && *n.BytesUsed < 0 {
		return fmt.Errorf("node %q: bytes_used must be >= 0", n.Node)
	}
	return nil
}

// RestoreSample captures per-actor atelet restore timing sub-phases (restore_timings.jsonl).
type RestoreSample struct {
	SchemaVersion   string    `json:"schema_version,omitempty"`
	Node            string    `json:"node,omitempty"`
	Env             string    `json:"env"`
	Snapshot        string    `json:"snapshot,omitempty"`
	Kind            string    `json:"kind,omitempty"`
	TS              time.Time `json:"ts,omitempty"`
	TotalMS         float64   `json:"total_ms"`
	ManifestFetchMS float64   `json:"manifest_fetch_ms,omitempty"`
	DownloadMS      float64   `json:"download_ms,omitempty"`
	OCIUnpackMS     float64   `json:"oci_unpack_ms,omitempty"`
	WorkerRestoreMS float64   `json:"worker_restore_ms,omitempty"`
	PersistMS       float64   `json:"persist_ms,omitempty"`
	CacheHit        *bool     `json:"cache_hit,omitempty"`
}

// Validate checks RestoreSample invariants.
func (r RestoreSample) Validate() error {
	if r.SchemaVersion != "" && r.SchemaVersion != SchemaVersion {
		return fmt.Errorf("restore_sample: unsupported schema_version %q (want %q)", r.SchemaVersion, SchemaVersion)
	}
	if r.Env == "" {
		return errors.New("env is required")
	}
	if r.TotalMS < 0 || r.ManifestFetchMS < 0 || r.DownloadMS < 0 || r.OCIUnpackMS < 0 || r.WorkerRestoreMS < 0 || r.PersistMS < 0 {
		return fmt.Errorf("env %q: restore timings must be >= 0", r.Env)
	}
	return nil
}

// PrometheusSeries represents a scraped Prometheus metric series.
// Value is a pointer so measured 0 is distinguished from a missing/failed scrape (§7.3).
type PrometheusSeries struct {
	Name   string            `json:"name"`
	Labels map[string]string `json:"labels,omitempty"`
	Status string            `json:"status,omitempty"`
	Err    string            `json:"err,omitempty"`
	Value  *float64          `json:"value,omitempty"`
	Values []float64         `json:"values,omitempty"`
}

// Validate checks PrometheusSeries invariants.
func (p PrometheusSeries) Validate() error {
	if p.Name == "" {
		return errors.New("metric name is required")
	}
	switch p.Status {
	case "", "ok":
		if p.Err == "" && p.Value == nil && len(p.Values) == 0 {
			return fmt.Errorf("metric %q: value or values is required when status is ok", p.Name)
		}
	case "error":
		if p.Err == "" {
			return fmt.Errorf("metric %q: err is required when status is \"error\"", p.Name)
		}
	default:
		return fmt.Errorf("metric %q: invalid status %q (want \"ok\" or \"error\")", p.Name, p.Status)
	}
	return nil
}

// SystemDescription records cluster, runtime, workload, and overlay provenance for a benchmark run.
type SystemDescription struct {
	SchemaVersion          string                  `json:"schema_version"`
	Cluster                string                  `json:"cluster"`
	Backend                string                  `json:"backend,omitempty"`
	Access                 string                  `json:"access,omitempty"`
	KubernetesVersion      string                  `json:"kubernetes_version,omitempty"`
	SDKVersion             string                  `json:"sdk_version,omitempty"`
	SubstrateVersion       string                  `json:"substrate_version,omitempty"`
	GraderVersion          string                  `json:"grader_version,omitempty"`
	DatasetRevision        string                  `json:"dataset_revision,omitempty"`
	SWEBenchHarnessVersion string                  `json:"swebench_harness_version,omitempty"`
	ExclusionListRevision  string                  `json:"exclusion_list_revision,omitempty"`
	NodePools              map[string]int          `json:"node_pools,omitempty"`
	MachineTypes           map[string]NodePoolInfo `json:"machine_types,omitempty"`
	ZoneTopology           string                  `json:"zone_topology,omitempty"`
	BucketRegion           string                  `json:"bucket_region,omitempty"`
	RuntimeClass           string                  `json:"runtime_class,omitempty"`
	SandboxFootprint       *SandboxFootprint       `json:"sandbox_footprint,omitempty"`
	APIImage               string                  `json:"api_image,omitempty"`
	GuestImage             string                  `json:"guest_image,omitempty"`
	GitSHA                 string                  `json:"git_sha,omitempty"`
	DatasetDigest          string                  `json:"dataset_digest,omitempty"`
	ScenarioHash           string                  `json:"scenario_hash,omitempty"`
	Overlays               []OverlayRef            `json:"overlays,omitempty"`
	Load                   *LoadParams             `json:"load,omitempty"`
}

// NodePoolInfo records machine type, node count, and zone for a cluster node pool.
type NodePoolInfo struct {
	MachineType string `json:"machine_type"`
	Count       int    `json:"count"`
	Zone        string `json:"zone,omitempty"`
}

// SandboxFootprint records the per-sandbox CPU, memory, and disk resource request/limit.
type SandboxFootprint struct {
	CPU       float64 `json:"cpu"`
	MemoryGiB float64 `json:"memory_gib"`
	DiskGiB   float64 `json:"disk_gib,omitempty"`
}

// OverlayRef records an active FeatureOverlay's name, hash, and SUT overrides.
type OverlayRef struct {
	Name         string            `json:"name"`
	Hash         string            `json:"hash,omitempty"`
	SUTOverrides map[string]string `json:"sut_overrides,omitempty"`
}

// LoadParams records configured workload scale parameters in SystemDescription.
type LoadParams struct {
	Tasks               int `json:"tasks,omitempty"`
	GroupSize           int `json:"group_size,omitempty"`
	Steps               int `json:"steps,omitempty"`
	TasksPerStep        int `json:"tasks_per_step,omitempty"`
	TrajectoriesPerStep int `json:"trajectories_per_step,omitempty"`
	Repeat              int `json:"repeat,omitempty"`
	MaxConcurrent       int `json:"max_concurrent,omitempty"`
	DistinctImages      int `json:"distinct_images,omitempty"`
}

// Validate checks SystemDescription invariants.
func (s SystemDescription) Validate() error {
	if s.SchemaVersion != SchemaVersion {
		return fmt.Errorf("system_description: unsupported schema_version %q (want %q)", s.SchemaVersion, SchemaVersion)
	}
	if s.Cluster == "" {
		return errors.New("system_description: cluster is required")
	}
	if s.Backend != "" && s.Backend != "substrate" && s.Backend != "agent-sandbox" {
		return fmt.Errorf("system_description: invalid backend %q (want \"substrate\" or \"agent-sandbox\")", s.Backend)
	}
	if s.Access != "" && s.Access != "sdk" && s.Access != "ate-env-api" && s.Access != "direct" {
		return fmt.Errorf("system_description: invalid access %q (want \"sdk\", \"ate-env-api\", or \"direct\")", s.Access)
	}
	for pool, count := range s.NodePools {
		if count < 0 {
			return fmt.Errorf("system_description: node_pools[%q] count must be >= 0", pool)
		}
	}
	for pool, info := range s.MachineTypes {
		if info.MachineType == "" {
			return fmt.Errorf("system_description: machine_types[%q] machine_type is required", pool)
		}
		if info.Count < 0 {
			return fmt.Errorf("system_description: machine_types[%q] count must be >= 0", pool)
		}
	}
	if s.SandboxFootprint != nil {
		if s.SandboxFootprint.CPU < 0 || s.SandboxFootprint.MemoryGiB < 0 || s.SandboxFootprint.DiskGiB < 0 {
			return errors.New("system_description: sandbox_footprint values must be >= 0")
		}
	}
	for i, o := range s.Overlays {
		if o.Name == "" {
			return fmt.Errorf("system_description: overlays[%d] name is required", i)
		}
	}
	if s.Load != nil {
		if s.Load.Tasks < 0 || s.Load.GroupSize < 0 || s.Load.Steps < 0 || s.Load.TasksPerStep < 0 ||
			s.Load.TrajectoriesPerStep < 0 || s.Load.Repeat < 0 || s.Load.MaxConcurrent < 0 || s.Load.DistinctImages < 0 {
			return errors.New("system_description: load counts must be >= 0")
		}
	}
	return nil
}

// Report is the structured report.json artifact carrying system description,
// measurements, per-job summaries, custom_metrics, and validity status.
type Report struct {
	SchemaVersion     string                      `json:"schema_version"`
	RunID             string                      `json:"run_id"`
	ScenarioName      string                      `json:"scenario_name"`
	Tag               string                      `json:"tag,omitempty"`
	GeneratedAt       time.Time                   `json:"generated_at"`
	SystemDescription *SystemDescription          `json:"system_description,omitempty"`
	MeasurementsRef   string                      `json:"measurements_ref,omitempty"`
	Measurements      *Measurements               `json:"measurements,omitempty"`
	Validity          ReportValidity              `json:"validity"`
	Jobs              map[string]JobReportSummary `json:"jobs,omitempty"`
	CustomMetrics     map[string]MetricSummary    `json:"custom_metrics,omitempty"`
}

// ReportValidity records whether a benchmark run passed all §6 validity gates.
type ReportValidity struct {
	Valid   bool     `json:"valid"`
	Reasons []string `json:"reasons,omitempty"`
}

// Validate checks ReportValidity invariants.
func (v ReportValidity) Validate() error {
	if !v.Valid && len(v.Reasons) == 0 {
		return errors.New("validity: reasons is required when valid is false")
	}
	return nil
}

// JobReportSummary holds per-job task counts and aggregated metric summaries in Report.
type JobReportSummary struct {
	Tasks      int                      `json:"tasks"`
	Failed     int                      `json:"failed"`
	Rewarded   int                      `json:"rewarded"`
	RewardRate float64                  `json:"reward_rate"`
	Steps      int                      `json:"steps,omitempty"`
	Metrics    map[string]MetricSummary `json:"metrics,omitempty"`
}

// Validate checks JobReportSummary invariants.
func (j JobReportSummary) Validate() error {
	if j.Tasks < 0 || j.Failed < 0 || j.Rewarded < 0 || j.Steps < 0 || j.RewardRate < 0 {
		return errors.New("job report counts and reward_rate must be >= 0")
	}
	for k, m := range j.Metrics {
		if err := m.Validate(); err != nil {
			return fmt.Errorf("metric %q: %w", k, err)
		}
	}
	return nil
}

// Validate checks Report invariants.
func (r Report) Validate() error {
	if r.SchemaVersion != SchemaVersion {
		return fmt.Errorf("report: unsupported schema_version %q (want %q)", r.SchemaVersion, SchemaVersion)
	}
	if r.RunID == "" {
		return errors.New("report: run_id is required")
	}
	if r.ScenarioName == "" {
		return errors.New("report: scenario_name is required")
	}
	if r.GeneratedAt.IsZero() {
		return errors.New("report: generated_at is required")
	}
	if err := r.Validity.Validate(); err != nil {
		return fmt.Errorf("report: %w", err)
	}
	if r.SystemDescription != nil {
		if err := r.SystemDescription.Validate(); err != nil {
			return fmt.Errorf("report system_description: %w", err)
		}
	}
	if r.Measurements != nil {
		if err := r.Measurements.Validate(); err != nil {
			return fmt.Errorf("report measurements: %w", err)
		}
	}
	for jobName, jobSummary := range r.Jobs {
		if err := jobSummary.Validate(); err != nil {
			return fmt.Errorf("report jobs[%q]: %w", jobName, err)
		}
	}
	for k, cm := range r.CustomMetrics {
		if err := cm.Validate(); err != nil {
			return fmt.Errorf("report custom_metrics[%q]: %w", k, err)
		}
	}
	return nil
}

// StatsRow is a flat summary row in stats.jsonl following the DESIGN §3/§5.8
// {timestamp, tag, test_name, metric, measurements} contract.
type StatsRow struct {
	SchemaVersion string             `json:"schema_version"`
	Timestamp     time.Time          `json:"timestamp"`
	Tag           string             `json:"tag,omitempty"`
	TestName      string             `json:"test_name"`
	RunID         string             `json:"run_id,omitempty"`
	Job           string             `json:"job,omitempty"`
	Metric        string             `json:"metric"`
	Measurements  map[string]float64 `json:"measurements"`
	Labels        map[string]string  `json:"labels,omitempty"`
}

// Validate checks StatsRow invariants.
func (s StatsRow) Validate() error {
	if s.SchemaVersion != SchemaVersion {
		return fmt.Errorf("stats_row: unsupported schema_version %q (want %q)", s.SchemaVersion, SchemaVersion)
	}
	if s.Timestamp.IsZero() {
		return errors.New("stats_row: timestamp is required")
	}
	if s.TestName == "" {
		return errors.New("stats_row: test_name is required")
	}
	if s.Metric == "" {
		return errors.New("stats_row: metric is required")
	}
	if len(s.Measurements) == 0 {
		return errors.New("stats_row: measurements must be non-empty")
	}
	return nil
}

// LoadJSONSchema loads and resolves one of the embedded v1 JSON Schema files
// (for example, "scenario.schema.json" or "v1/scenario.schema.json").
func LoadJSONSchema(schemaFile string) (*jsonschema.Resolved, error) {
	rel := strings.TrimPrefix(schemaFile, "v1/")
	data, err := SchemaFS.ReadFile(path.Join("v1", rel))
	if err != nil {
		return nil, fmt.Errorf("read embedded schema %q: %w", schemaFile, err)
	}
	var s jsonschema.Schema
	if err := json.Unmarshal(data, &s); err != nil {
		return nil, fmt.Errorf("unmarshal schema %q: %w", schemaFile, err)
	}
	resolved, err := s.Resolve(nil)
	if err != nil {
		return nil, fmt.Errorf("resolve schema %q: %w", schemaFile, err)
	}
	return resolved, nil
}

// ValidateAgainstJSONSchema validates a raw JSON document against an embedded v1 JSON Schema.
func ValidateAgainstJSONSchema(schemaFile string, jsonData []byte) error {
	resolved, err := LoadJSONSchema(schemaFile)
	if err != nil {
		return err
	}
	var doc any
	dec := json.NewDecoder(bytes.NewReader(jsonData))
	if err := dec.Decode(&doc); err != nil {
		return fmt.Errorf("decode json for schema validation: %w", err)
	}
	if dec.More() {
		return errors.New("decode json for schema validation: unexpected trailing data")
	}
	if err := resolved.Validate(doc); err != nil {
		return fmt.Errorf("jsonschema %s: %w", schemaFile, err)
	}
	return nil
}

// ValidateJSONLAgainstJSONSchema validates each non-empty line in r against an embedded v1 JSON Schema.
func ValidateJSONLAgainstJSONSchema(schemaFile string, r io.Reader) error {
	resolved, err := LoadJSONSchema(schemaFile)
	if err != nil {
		return err
	}
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 1<<20), 16<<20)
	lineNum := 0
	for sc.Scan() {
		lineNum++
		b := bytes.TrimSpace(sc.Bytes())
		if len(b) == 0 {
			continue
		}
		var doc any
		if err := json.Unmarshal(b, &doc); err != nil {
			return fmt.Errorf("line %d: decode jsonl: %w", lineNum, err)
		}
		if err := resolved.Validate(doc); err != nil {
			return fmt.Errorf("line %d: jsonschema %s: %w", lineNum, schemaFile, err)
		}
	}
	if err := sc.Err(); err != nil {
		return fmt.Errorf("scan jsonl: %w", err)
	}
	return nil
}

// DecodeJSONStrict decodes a single JSON document from r with DisallowUnknownFields
// and validates the decoded value via its Validate method.
func DecodeJSONStrict[T Validator](r io.Reader) (*T, error) {
	dec := json.NewDecoder(r)
	dec.DisallowUnknownFields()
	var out T
	if err := dec.Decode(&out); err != nil {
		return nil, fmt.Errorf("decode json: %w", err)
	}
	if dec.More() {
		return nil, errors.New("decode json: unexpected trailing data")
	}
	if err := out.Validate(); err != nil {
		return nil, err
	}
	return &out, nil
}

// ForEachJSONL streams newline-delimited JSON records from r with DisallowUnknownFields,
// validating each record (if T implements Validator) and invoking fn(lineNum, item)
// without materialising the entire file in memory.
func ForEachJSONL[T any](r io.Reader, fn func(line int, v T) error) error {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 1<<20), 16<<20)
	lineNum := 0
	for sc.Scan() {
		lineNum++
		b := bytes.TrimSpace(sc.Bytes())
		if len(b) == 0 {
			continue
		}
		dec := json.NewDecoder(bytes.NewReader(b))
		dec.DisallowUnknownFields()
		var item T
		if err := dec.Decode(&item); err != nil {
			return fmt.Errorf("line %d: decode jsonl: %w", lineNum, err)
		}
		if dec.More() {
			return fmt.Errorf("line %d: decode jsonl: unexpected trailing data", lineNum)
		}
		if v, ok := any(item).(Validator); ok {
			if err := v.Validate(); err != nil {
				return fmt.Errorf("line %d: %w", lineNum, err)
			}
		}
		if err := fn(lineNum, item); err != nil {
			return err
		}
	}
	if err := sc.Err(); err != nil {
		return fmt.Errorf("scan jsonl: %w", err)
	}
	return nil
}

// DecodeJSONLStrict decodes newline-delimited JSON records from r with DisallowUnknownFields
// by streaming through ForEachJSONL.
func DecodeJSONLStrict[T any](r io.Reader) ([]T, error) {
	var out []T
	err := ForEachJSONL[T](r, func(_ int, item T) error {
		out = append(out, item)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// DecodeYAMLStrict decodes YAML bytes with sigs.k8s.io/yaml.UnmarshalStrict
// and validates the decoded value via its Validate method.
func DecodeYAMLStrict[T Validator](data []byte) (*T, error) {
	var out T
	if err := yaml.UnmarshalStrict(data, &out); err != nil {
		return nil, fmt.Errorf("decode yaml: %w", err)
	}
	if err := out.Validate(); err != nil {
		return nil, err
	}
	return &out, nil
}
