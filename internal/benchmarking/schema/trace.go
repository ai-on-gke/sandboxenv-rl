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

// TraceEvent is a single raw SDK call record in trace.jsonl (emitted when
// ENV_BENCH_MODE=trace).
type TraceEvent struct {
	SchemaVersion string    `json:"schema_version"`
	TS            time.Time `json:"ts"`
	Run           string    `json:"run,omitempty"`
	Job           string    `json:"job,omitempty"`
	Step          int       `json:"step,omitempty"`
	Env           string    `json:"env"`
	Instance      string    `json:"instance,omitempty"`
	Image         string    `json:"image,omitempty"`
	Op            string    `json:"op"`
	Command       []string  `json:"command,omitempty"`
	Path          string    `json:"path,omitempty"`
	Bytes         int64     `json:"bytes,omitempty"`
	DurMS         float64   `json:"dur_ms"`
	Exit          *int      `json:"exit,omitempty"`
	OK            bool      `json:"ok"`
	Err           string    `json:"err,omitempty"`
}

// ValidTraceOps enumerates the allowed SDK operation names in TraceEvent.Op.
var ValidTraceOps = map[string]bool{
	"create":        true,
	"get":           true,
	"suspend":       true,
	"delete":        true,
	"wake":          true,
	"exec":          true,
	"shell":         true,
	"start_process": true,
	"write_file":    true,
	"read_file":     true,
}

// Validate checks TraceEvent invariants.
func (t TraceEvent) Validate() error {
	if t.SchemaVersion != SchemaVersion {
		return fmt.Errorf("trace_event: schema_version must be %q, got %q", SchemaVersion, t.SchemaVersion)
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
	if !ValidTraceOps[t.Op] {
		return fmt.Errorf("trace_event: invalid op %q", t.Op)
	}
	if t.Step < 0 {
		return fmt.Errorf("trace_event: step must be >= 0, got %d", t.Step)
	}
	if t.Bytes < 0 {
		return fmt.Errorf("trace_event: bytes must be >= 0, got %d", t.Bytes)
	}
	if t.DurMS < 0 {
		return fmt.Errorf("trace_event: dur_ms must be >= 0, got %v", t.DurMS)
	}
	return nil
}
