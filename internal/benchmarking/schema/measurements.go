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
	"time"
)

// Measurements holds server-side telemetry, artifact references for node
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
	for i, p := range m.PromMetrics {
		if err := p.Validate(); err != nil {
			return fmt.Errorf("measurements prom_metrics[%d]: %w", i, err)
		}
	}
	for k, sm := range m.ServerSummary {
		if err := sm.Validate(k); err != nil {
			return fmt.Errorf("measurements server_summary[%q]: %w", k, err)
		}
	}
	for k, cm := range m.CustomMetrics {
		if err := cm.Validate(k); err != nil {
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
	Node      string     `json:"node"`
	TS        *time.Time `json:"ts,omitempty"`
	Hits      int64      `json:"hits"`
	Misses    int64      `json:"misses"`
	Errors    *int64     `json:"errors,omitempty"`
	Evictions *int64     `json:"evictions,omitempty"`
	BytesUsed *int64     `json:"bytes_used,omitempty"`
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
	SchemaVersion   string     `json:"schema_version,omitempty"`
	Node            string     `json:"node,omitempty"`
	Env             string     `json:"env"`
	Snapshot        string     `json:"snapshot,omitempty"`
	Kind            string     `json:"kind,omitempty"`
	TS              *time.Time `json:"ts,omitempty"`
	TotalMS         float64    `json:"total_ms"`
	ManifestFetchMS float64    `json:"manifest_fetch_ms,omitempty"`
	DownloadMS      float64    `json:"download_ms,omitempty"`
	OCIUnpackMS     float64    `json:"oci_unpack_ms,omitempty"`
	WorkerRestoreMS float64    `json:"worker_restore_ms,omitempty"`
	PersistMS       float64    `json:"persist_ms,omitempty"`
	CacheHit        *bool      `json:"cache_hit,omitempty"`
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
	if p.Err != "" && p.Status != "error" {
		return fmt.Errorf("metric %q: err is set but status is not \"error\"", p.Name)
	}
	switch p.Status {
	case "", "ok":
		if p.Value == nil && len(p.Values) == 0 {
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
