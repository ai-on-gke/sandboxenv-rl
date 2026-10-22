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

// Package schema defines versioned wire contracts and strict decoders for
// RL benchmark scenarios, schedules, traces, events, measurements, and reports.
package schema

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"time"

	"sigs.k8s.io/yaml"
)

// SchemaVersion is the wire schema version for all W1 RL benchmark artifacts.
const SchemaVersion = "v1"

// Validator is implemented by schema types that enforce semantic invariants.
type Validator interface {
	Validate() error
}

// Scenario defines one benchmark run against a System Under Test (SUT).
type Scenario struct {
	SchemaVersion string           `json:"schema_version" yaml:"schema_version"`
	Name          string           `json:"name" yaml:"name"`
	SUT           SUTSpec          `json:"sut" yaml:"sut"`
	Jobs          []JobSpec        `json:"jobs" yaml:"jobs"`
	RunWindow     RunWindowSpec    `json:"run_window,omitempty" yaml:"run_window,omitempty"`
	Overlays      []FeatureOverlay `json:"overlays,omitempty" yaml:"overlays,omitempty"`
}

// Validate checks Scenario invariants.
func (s Scenario) Validate() error {
	if s.SchemaVersion != SchemaVersion {
		return fmt.Errorf("scenario: unsupported schema_version %q (want %q)", s.SchemaVersion, SchemaVersion)
	}
	if s.Name == "" {
		return errors.New("scenario: name is required")
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
	if err := s.RunWindow.Validate(); err != nil {
		return fmt.Errorf("scenario run_window: %w", err)
	}
	for i, o := range s.Overlays {
		if err := o.Validate(); err != nil {
			return fmt.Errorf("scenario overlay[%d]: %w", i, err)
		}
	}
	return nil
}

// SUTSpec describes the System Under Test configuration.
type SUTSpec struct {
	Endpoint     string            `json:"endpoint,omitempty" yaml:"endpoint,omitempty"`
	Atespace     string            `json:"atespace,omitempty" yaml:"atespace,omitempty"`
	BaseTemplate string            `json:"base_template,omitempty" yaml:"base_template,omitempty"`
	NodePool     string            `json:"node_pool,omitempty" yaml:"node_pool,omitempty"`
	RuntimeClass string            `json:"runtime_class,omitempty" yaml:"runtime_class,omitempty"`
	Replicas     int               `json:"replicas,omitempty" yaml:"replicas,omitempty"`
	ExtraArgs    map[string]string `json:"extra_args,omitempty" yaml:"extra_args,omitempty"`
}

// Validate checks SUTSpec invariants.
func (s SUTSpec) Validate() error {
	if s.Replicas < 0 {
		return fmt.Errorf("replicas must be >= 0, got %d", s.Replicas)
	}
	return nil
}

// JobSpec defines one simulated or real RL job loop within a Scenario.
type JobSpec struct {
	Name          string `json:"name" yaml:"name"`
	Kind          string `json:"kind" yaml:"kind"`
	Dataset       string `json:"dataset,omitempty" yaml:"dataset,omitempty"`
	Trajectory    string `json:"trajectory,omitempty" yaml:"trajectory,omitempty"`
	Mode          string `json:"mode,omitempty" yaml:"mode,omitempty"`
	Park          string `json:"park,omitempty" yaml:"park,omitempty"`
	Images        int    `json:"images,omitempty" yaml:"images,omitempty"`
	TasksPerImage int    `json:"tasks_per_image,omitempty" yaml:"tasks_per_image,omitempty"`
	Rollouts      int    `json:"rollouts,omitempty" yaml:"rollouts,omitempty"`
	Steps         int    `json:"steps,omitempty" yaml:"steps,omitempty"`
	Width         int    `json:"width,omitempty" yaml:"width,omitempty"`
	StepPause     string `json:"step_pause,omitempty" yaml:"step_pause,omitempty"`
	Prefetch      bool   `json:"prefetch,omitempty" yaml:"prefetch,omitempty"`
	Seed          int64  `json:"seed,omitempty" yaml:"seed,omitempty"`
	ScheduleMode  string `json:"schedule_mode,omitempty" yaml:"schedule_mode,omitempty"`
}

// Validate checks JobSpec invariants.
func (j JobSpec) Validate() error {
	if j.Name == "" {
		return errors.New("job name is required")
	}
	if j.Kind != "sim" && j.Kind != "trainer" {
		return fmt.Errorf("job %q: invalid kind %q (want \"sim\" or \"trainer\")", j.Name, j.Kind)
	}
	if j.ScheduleMode != "" && j.ScheduleMode != "fixed" && j.ScheduleMode != "sampled" && j.ScheduleMode != "explicit" {
		return fmt.Errorf("job %q: invalid schedule_mode %q (want \"fixed\", \"sampled\", or \"explicit\")", j.Name, j.ScheduleMode)
	}
	if j.Images < 0 || j.TasksPerImage < 0 || j.Rollouts < 0 || j.Steps < 0 || j.Width < 0 {
		return fmt.Errorf("job %q: numeric counts must be >= 0", j.Name)
	}
	if j.StepPause != "" {
		d, err := time.ParseDuration(j.StepPause)
		if err != nil || d < 0 {
			return fmt.Errorf("job %q: invalid step_pause %q", j.Name, j.StepPause)
		}
	}
	return nil
}

// RunWindowSpec configures steady-state step boundaries and run duration caps.
type RunWindowSpec struct {
	SteadyStateStartStep int    `json:"steady_state_start_step,omitempty" yaml:"steady_state_start_step,omitempty"`
	SteadyStateEndStep   int    `json:"steady_state_end_step,omitempty" yaml:"steady_state_end_step,omitempty"`
	WarmupSteps          int    `json:"warmup_steps,omitempty" yaml:"warmup_steps,omitempty"`
	MaxDuration          string `json:"max_duration,omitempty" yaml:"max_duration,omitempty"`
}

// Validate checks RunWindowSpec invariants.
func (w RunWindowSpec) Validate() error {
	if w.SteadyStateStartStep < 0 || w.SteadyStateEndStep < 0 || w.WarmupSteps < 0 {
		return errors.New("step bounds must be >= 0")
	}
	if w.SteadyStateEndStep > 0 && w.SteadyStateStartStep > w.SteadyStateEndStep {
		return fmt.Errorf("steady_state_start_step (%d) > steady_state_end_step (%d)", w.SteadyStateStartStep, w.SteadyStateEndStep)
	}
	if w.MaxDuration != "" {
		d, err := time.ParseDuration(w.MaxDuration)
		if err != nil || d <= 0 {
			return fmt.Errorf("invalid max_duration %q", w.MaxDuration)
		}
	}
	return nil
}

// FeatureOverlay bundles A/B feature overrides onto a base Scenario.
type FeatureOverlay struct {
	Name        string            `json:"name" yaml:"name"`
	Description string            `json:"description,omitempty" yaml:"description,omitempty"`
	Park        string            `json:"park,omitempty" yaml:"park,omitempty"`
	Prefetch    *bool             `json:"prefetch,omitempty" yaml:"prefetch,omitempty"`
	EnvVars     map[string]string `json:"env_vars,omitempty" yaml:"env_vars,omitempty"`
	Flags       map[string]string `json:"flags,omitempty" yaml:"flags,omitempty"`
}

// Validate checks FeatureOverlay invariants.
func (o FeatureOverlay) Validate() error {
	if o.Name == "" {
		return errors.New("feature overlay name is required")
	}
	return nil
}

// Schedule is the deterministic resolved per-step placement schedule.
type Schedule struct {
	SchemaVersion string         `json:"schema_version" yaml:"schema_version"`
	ScenarioName  string         `json:"scenario_name" yaml:"scenario_name"`
	Seed          int64          `json:"seed,omitempty" yaml:"seed,omitempty"`
	Jobs          []ScheduledJob `json:"jobs" yaml:"jobs"`
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
	Job   string          `json:"job" yaml:"job"`
	Steps []ScheduledStep `json:"steps" yaml:"steps"`
}

// Validate checks ScheduledJob invariants.
func (j ScheduledJob) Validate() error {
	if j.Job == "" {
		return errors.New("job name is required")
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

// ScheduledStep holds the placements and prefetch list for a single step.
type ScheduledStep struct {
	Step           int             `json:"step" yaml:"step"`
	StepPause      string          `json:"step_pause,omitempty" yaml:"step_pause,omitempty"`
	Placements     []StepPlacement `json:"placements" yaml:"placements"`
	PrefetchImages []string        `json:"prefetch_images,omitempty" yaml:"prefetch_images,omitempty"`
}

// Validate checks ScheduledStep invariants.
func (s ScheduledStep) Validate() error {
	if s.Step <= 0 {
		return fmt.Errorf("step must be >= 1, got %d", s.Step)
	}
	if s.StepPause != "" {
		d, err := time.ParseDuration(s.StepPause)
		if err != nil || d < 0 {
			return fmt.Errorf("invalid step_pause %q", s.StepPause)
		}
	}
	if len(s.Placements) == 0 {
		return fmt.Errorf("step %d: at least one placement is required", s.Step)
	}
	for i, p := range s.Placements {
		if err := p.Validate(); err != nil {
			return fmt.Errorf("step %d placement[%d]: %w", s.Step, i, err)
		}
	}
	return nil
}

// StepPlacement maps one task rollout to an environment in a ScheduledStep.
type StepPlacement struct {
	EnvID        string `json:"env_id" yaml:"env_id"`
	InstanceID   string `json:"instance_id" yaml:"instance_id"`
	Image        string `json:"image" yaml:"image"`
	Rollout      int    `json:"rollout,omitempty" yaml:"rollout,omitempty"`
	ReuseEnvID   string `json:"reuse_env_id,omitempty" yaml:"reuse_env_id,omitempty"`
	FromSnapshot string `json:"from_snapshot,omitempty" yaml:"from_snapshot,omitempty"`
	RetireAfter  bool   `json:"retire_after,omitempty" yaml:"retire_after,omitempty"`
}

// Validate checks StepPlacement invariants.
func (p StepPlacement) Validate() error {
	if p.EnvID == "" {
		return errors.New("env_id is required")
	}
	if p.InstanceID == "" {
		return errors.New("instance_id is required")
	}
	if p.Image == "" {
		return errors.New("image is required")
	}
	if p.Rollout < 0 {
		return fmt.Errorf("rollout must be >= 0, got %d", p.Rollout)
	}
	return nil
}

// TraceEvent records a single SDK call in verbose trace mode (trace.jsonl).
type TraceEvent struct {
	SchemaVersion string    `json:"schema_version,omitempty" yaml:"schema_version,omitempty"`
	TS            time.Time `json:"ts" yaml:"ts"`
	Job           string    `json:"job,omitempty" yaml:"job,omitempty"`
	Env           string    `json:"env" yaml:"env"`
	Step          int       `json:"step,omitempty" yaml:"step,omitempty"`
	Op            string    `json:"op" yaml:"op"`
	Command       []string  `json:"command,omitempty" yaml:"command,omitempty"`
	Path          string    `json:"path,omitempty" yaml:"path,omitempty"`
	Bytes         int64     `json:"bytes,omitempty" yaml:"bytes,omitempty"`
	DurMS         float64   `json:"dur_ms" yaml:"dur_ms"`
	Exit          *int      `json:"exit,omitempty" yaml:"exit,omitempty"`
	OK            bool      `json:"ok" yaml:"ok"`
	Err           string    `json:"err,omitempty" yaml:"err,omitempty"`
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
	if t.Op == "" {
		return errors.New("trace_event: op is required")
	}
	if t.DurMS < 0 || t.Bytes < 0 || t.Step < 0 {
		return errors.New("trace_event: dur_ms, bytes, and step must be >= 0")
	}
	return nil
}

// Event is a 100% backward-compatible superset of swebench.Event emitted to events/*.jsonl.
type Event struct {
	TS       time.Time          `json:"ts" yaml:"ts"`
	Run      string             `json:"run" yaml:"run"`
	Event    string             `json:"event" yaml:"event"`
	Name     string             `json:"name,omitempty" yaml:"name,omitempty"`
	Env      string             `json:"env,omitempty" yaml:"env,omitempty"`
	Instance string             `json:"instance,omitempty" yaml:"instance,omitempty"`
	Task     int                `json:"task,omitempty" yaml:"task,omitempty"`
	Rollout  int                `json:"rollout,omitempty" yaml:"rollout,omitempty"`
	Step     int                `json:"step,omitempty" yaml:"step,omitempty"`
	DurMS    float64            `json:"dur_ms" yaml:"dur_ms"`
	OK       bool               `json:"ok" yaml:"ok"`
	Exit     *int               `json:"exit,omitempty" yaml:"exit,omitempty"`
	Reward   *int               `json:"reward,omitempty" yaml:"reward,omitempty"`
	Attempts int                `json:"attempts,omitempty" yaml:"attempts,omitempty"`
	Count    int                `json:"count,omitempty" yaml:"count,omitempty"`
	Err      string             `json:"err,omitempty" yaml:"err,omitempty"`
	Tail     string             `json:"tail,omitempty" yaml:"tail,omitempty"`
	Job      string             `json:"job,omitempty" yaml:"job,omitempty"`
	Snapshot string             `json:"snapshot,omitempty" yaml:"snapshot,omitempty"`
	Node     string             `json:"node,omitempty" yaml:"node,omitempty"`
	Bytes    int64              `json:"bytes,omitempty" yaml:"bytes,omitempty"`
	CacheHit *bool              `json:"cache_hit,omitempty" yaml:"cache_hit,omitempty"`
	Tags     map[string]string  `json:"tags,omitempty" yaml:"tags,omitempty"`
	Metrics  map[string]float64 `json:"metrics,omitempty" yaml:"metrics,omitempty"`
}

// Validate checks Event invariants while remaining compatible with swebench.Event records.
func (e Event) Validate() error {
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
	return nil
}

// Measurements holds server-side telemetry, node image-cache samples, and restore timings.
type Measurements struct {
	SchemaVersion   string             `json:"schema_version" yaml:"schema_version"`
	RunID           string             `json:"run_id" yaml:"run_id"`
	CollectedAt     time.Time          `json:"collected_at" yaml:"collected_at"`
	NodeCacheBefore []NodeCacheSample  `json:"node_cache_before,omitempty" yaml:"node_cache_before,omitempty"`
	NodeCacheAfter  []NodeCacheSample  `json:"node_cache_after,omitempty" yaml:"node_cache_after,omitempty"`
	Restores        []RestoreSample    `json:"restores,omitempty" yaml:"restores,omitempty"`
	PromMetrics     []PrometheusSeries `json:"prom_metrics,omitempty" yaml:"prom_metrics,omitempty"`
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
	return nil
}

// NodeCacheSample captures per-node image/snapshot cache counters at a point in time.
type NodeCacheSample struct {
	Node      string    `json:"node" yaml:"node"`
	TS        time.Time `json:"ts,omitempty" yaml:"ts,omitempty"`
	Hits      int64     `json:"hits" yaml:"hits"`
	Misses    int64     `json:"misses" yaml:"misses"`
	Errors    int64     `json:"errors,omitempty" yaml:"errors,omitempty"`
	Evictions int64     `json:"evictions,omitempty" yaml:"evictions,omitempty"`
	BytesUsed int64     `json:"bytes_used,omitempty" yaml:"bytes_used,omitempty"`
}

// Validate checks NodeCacheSample invariants.
func (n NodeCacheSample) Validate() error {
	if n.Node == "" {
		return errors.New("node is required")
	}
	if n.Hits < 0 || n.Misses < 0 || n.Errors < 0 || n.Evictions < 0 || n.BytesUsed < 0 {
		return fmt.Errorf("node %q: cache counters must be >= 0", n.Node)
	}
	return nil
}

// RestoreSample captures per-actor atelet restore timing sub-phases.
type RestoreSample struct {
	Node            string    `json:"node,omitempty" yaml:"node,omitempty"`
	Env             string    `json:"env" yaml:"env"`
	Snapshot        string    `json:"snapshot,omitempty" yaml:"snapshot,omitempty"`
	Kind            string    `json:"kind,omitempty" yaml:"kind,omitempty"`
	TS              time.Time `json:"ts,omitempty" yaml:"ts,omitempty"`
	TotalMS         float64   `json:"total_ms" yaml:"total_ms"`
	ManifestFetchMS float64   `json:"manifest_fetch_ms,omitempty" yaml:"manifest_fetch_ms,omitempty"`
	DownloadMS      float64   `json:"download_ms,omitempty" yaml:"download_ms,omitempty"`
	OCIUnpackMS     float64   `json:"oci_unpack_ms,omitempty" yaml:"oci_unpack_ms,omitempty"`
	WorkerRestoreMS float64   `json:"worker_restore_ms,omitempty" yaml:"worker_restore_ms,omitempty"`
	PersistMS       float64   `json:"persist_ms,omitempty" yaml:"persist_ms,omitempty"`
	CacheHit        bool      `json:"cache_hit,omitempty" yaml:"cache_hit,omitempty"`
}

// Validate checks RestoreSample invariants.
func (r RestoreSample) Validate() error {
	if r.Env == "" {
		return errors.New("env is required")
	}
	if r.TotalMS < 0 {
		return fmt.Errorf("env %q: total_ms must be >= 0", r.Env)
	}
	return nil
}

// PrometheusSeries represents a scraped Prometheus metric series.
type PrometheusSeries struct {
	Name   string            `json:"name" yaml:"name"`
	Labels map[string]string `json:"labels,omitempty" yaml:"labels,omitempty"`
	Value  float64           `json:"value,omitempty" yaml:"value,omitempty"`
	Values []float64         `json:"values,omitempty" yaml:"values,omitempty"`
}

// Validate checks PrometheusSeries invariants.
func (p PrometheusSeries) Validate() error {
	if p.Name == "" {
		return errors.New("metric name is required")
	}
	return nil
}

// SystemDescription records cluster, runtime, and image provenance for a benchmark run.
type SystemDescription struct {
	SchemaVersion     string         `json:"schema_version" yaml:"schema_version"`
	Cluster           string         `json:"cluster" yaml:"cluster"`
	KubernetesVersion string         `json:"kubernetes_version,omitempty" yaml:"kubernetes_version,omitempty"`
	NodePools         map[string]int `json:"node_pools,omitempty" yaml:"node_pools,omitempty"`
	RuntimeClass      string         `json:"runtime_class,omitempty" yaml:"runtime_class,omitempty"`
	APIImage          string         `json:"api_image,omitempty" yaml:"api_image,omitempty"`
	GuestImage        string         `json:"guest_image,omitempty" yaml:"guest_image,omitempty"`
	GitSHA            string         `json:"git_sha,omitempty" yaml:"git_sha,omitempty"`
	DatasetDigest     string         `json:"dataset_digest,omitempty" yaml:"dataset_digest,omitempty"`
}

// Validate checks SystemDescription invariants.
func (s SystemDescription) Validate() error {
	if s.SchemaVersion != SchemaVersion {
		return fmt.Errorf("system_description: unsupported schema_version %q (want %q)", s.SchemaVersion, SchemaVersion)
	}
	if s.Cluster == "" {
		return errors.New("system_description: cluster is required")
	}
	return nil
}

// StatsRecord is a flat summary row in stats.jsonl covering M1-M6 benchmark metrics.
type StatsRecord struct {
	SchemaVersion  string   `json:"schema_version" yaml:"schema_version"`
	RunID          string   `json:"run_id" yaml:"run_id"`
	Scenario       string   `json:"scenario" yaml:"scenario"`
	Overlay        string   `json:"overlay,omitempty" yaml:"overlay,omitempty"`
	Mode           string   `json:"mode" yaml:"mode"`
	Valid          bool     `json:"valid" yaml:"valid"`
	InvalidReasons []string `json:"invalid_reasons,omitempty" yaml:"invalid_reasons,omitempty"`
	Tasks          int      `json:"tasks" yaml:"tasks"`
	Failed         int      `json:"failed" yaml:"failed"`
	Rewarded       int      `json:"rewarded" yaml:"rewarded"`
	RewardRate     float64  `json:"reward_rate" yaml:"reward_rate"`
	StepWallP50MS  float64  `json:"step_wall_p50_ms" yaml:"step_wall_p50_ms"`
	StepWallP95MS  float64  `json:"step_wall_p95_ms" yaml:"step_wall_p95_ms"`
	TTFIP50MS      float64  `json:"ttfi_p50_ms" yaml:"ttfi_p50_ms"`
	TTFIP95MS      float64  `json:"ttfi_p95_ms" yaml:"ttfi_p95_ms"`
	WakeP50MS      float64  `json:"wake_p50_ms" yaml:"wake_p50_ms"`
	WakeP95MS      float64  `json:"wake_p95_ms" yaml:"wake_p95_ms"`
	SuspendP50MS   float64  `json:"suspend_p50_ms" yaml:"suspend_p50_ms"`
	SuspendP95MS   float64  `json:"suspend_p95_ms" yaml:"suspend_p95_ms"`
	CacheHitRate   float64  `json:"cache_hit_rate" yaml:"cache_hit_rate"`
	OrphanEnvs     int      `json:"orphan_envs" yaml:"orphan_envs"`
	CPUCoreSeconds float64  `json:"cpu_core_seconds" yaml:"cpu_core_seconds"`
	GiBSeconds     float64  `json:"gib_seconds" yaml:"gib_seconds"`
}

// Validate checks StatsRecord invariants.
func (s StatsRecord) Validate() error {
	if s.SchemaVersion != SchemaVersion {
		return fmt.Errorf("stats_record: unsupported schema_version %q (want %q)", s.SchemaVersion, SchemaVersion)
	}
	if s.RunID == "" {
		return errors.New("stats_record: run_id is required")
	}
	if s.Scenario == "" {
		return errors.New("stats_record: scenario is required")
	}
	if s.Mode == "" {
		return errors.New("stats_record: mode is required")
	}
	if !s.Valid && len(s.InvalidReasons) == 0 {
		return errors.New("stats_record: invalid_reasons is required when valid is false")
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

// DecodeJSONLStrict decodes newline-delimited JSON records from r with DisallowUnknownFields.
// If T implements Validator, each decoded item is also validated.
func DecodeJSONLStrict[T any](r io.Reader) ([]T, error) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 1<<20), 16<<20)
	var out []T
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
			return nil, fmt.Errorf("line %d: decode jsonl: %w", lineNum, err)
		}
		if v, ok := any(item).(Validator); ok {
			if err := v.Validate(); err != nil {
				return nil, fmt.Errorf("line %d: %w", lineNum, err)
			}
		}
		out = append(out, item)
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("scan jsonl: %w", err)
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
