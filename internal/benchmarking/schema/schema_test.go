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
	"bytes"
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestEmbeddedSchemasResolve(t *testing.T) {
	schemas := []string{
		"schedule.schema.json",
		"placement.schema.json",
		"event.schema.json",
		"run_window.schema.json",
		"report.schema.json",
		"stats_row.schema.json",
		"system_description.schema.json",
	}
	for _, name := range schemas {
		t.Run(name, func(t *testing.T) {
			if _, err := LoadJSONSchema(name); err != nil {
				t.Fatalf("LoadJSONSchema(%q) failed: %v", name, err)
			}
		})
	}
}

func TestFixturesValidateAndRoundTrip(t *testing.T) {
	t.Run("schedule.json", func(t *testing.T) {
		raw := mustRead(t, "testdata/schedule.json")
		if err := ValidateAgainstJSONSchema("schedule.schema.json", raw); err != nil {
			t.Fatalf("schema validate: %v", err)
		}
		v, err := DecodeJSONStrict[Schedule](bytes.NewReader(raw))
		if err != nil {
			t.Fatalf("DecodeJSONStrict: %v", err)
		}
		roundTripJSON(t, "schedule.schema.json", v)
	})

	t.Run("placement.json", func(t *testing.T) {
		raw := mustRead(t, "testdata/placement.json")
		if err := ValidateAgainstJSONSchema("placement.schema.json", raw); err != nil {
			t.Fatalf("schema validate: %v", err)
		}
		v, err := DecodeJSONStrict[PlacementPlan](bytes.NewReader(raw))
		if err != nil {
			t.Fatalf("DecodeJSONStrict: %v", err)
		}
		roundTripJSON(t, "placement.schema.json", v)
	})

	t.Run("event.jsonl", func(t *testing.T) {
		raw := mustRead(t, "testdata/event.jsonl")
		if err := ValidateJSONLAgainstJSONSchema("event.schema.json", bytes.NewReader(raw)); err != nil {
			t.Fatalf("schema validate jsonl: %v", err)
		}
		items, err := DecodeJSONLStrict[Event](bytes.NewReader(raw))
		if err != nil {
			t.Fatalf("DecodeJSONLStrict: %v", err)
		}
		if len(items) != 2 {
			t.Fatalf("len(items) = %d, want 2", len(items))
		}
		for _, item := range items {
			roundTripJSON(t, "event.schema.json", item)
		}
	})

	t.Run("run_window.json", func(t *testing.T) {
		raw := mustRead(t, "testdata/run_window.json")
		if err := ValidateAgainstJSONSchema("run_window.schema.json", raw); err != nil {
			t.Fatalf("schema validate: %v", err)
		}
		v, err := DecodeJSONStrict[RunWindow](bytes.NewReader(raw))
		if err != nil {
			t.Fatalf("DecodeJSONStrict: %v", err)
		}
		roundTripJSON(t, "run_window.schema.json", v)
	})

	t.Run("report.json", func(t *testing.T) {
		raw := mustRead(t, "testdata/report.json")
		if err := ValidateAgainstJSONSchema("report.schema.json", raw); err != nil {
			t.Fatalf("schema validate: %v", err)
		}
		v, err := DecodeJSONStrict[Report](bytes.NewReader(raw))
		if err != nil {
			t.Fatalf("DecodeJSONStrict: %v", err)
		}
		roundTripJSON(t, "report.schema.json", v)
	})

	t.Run("stats.jsonl", func(t *testing.T) {
		raw := mustRead(t, "testdata/stats.jsonl")
		if err := ValidateJSONLAgainstJSONSchema("stats_row.schema.json", bytes.NewReader(raw)); err != nil {
			t.Fatalf("schema validate jsonl: %v", err)
		}
		items, err := DecodeJSONLStrict[StatsRow](bytes.NewReader(raw))
		if err != nil {
			t.Fatalf("DecodeJSONLStrict: %v", err)
		}
		if len(items) != 2 {
			t.Fatalf("len(items) = %d, want 2", len(items))
		}
		for _, item := range items {
			roundTripJSON(t, "stats_row.schema.json", item)
		}
	})

	t.Run("system_description.json", func(t *testing.T) {
		raw := mustRead(t, "testdata/system_description.json")
		if err := ValidateAgainstJSONSchema("system_description.schema.json", raw); err != nil {
			t.Fatalf("schema validate: %v", err)
		}
		v, err := DecodeJSONStrict[SystemDescription](bytes.NewReader(raw))
		if err != nil {
			t.Fatalf("DecodeJSONStrict: %v", err)
		}
		roundTripJSON(t, "system_description.schema.json", v)
	})
}

func TestBaselineEventStreamsDecode(t *testing.T) {
	for _, tc := range []struct {
		file      string
		wantCount int
	}{
		{file: "testdata/baseline-eval.jsonl", wantCount: 29},
		{file: "testdata/baseline-steps.jsonl", wantCount: 58},
	} {
		t.Run(tc.file, func(t *testing.T) {
			raw := mustRead(t, tc.file)
			if err := ValidateJSONLAgainstJSONSchema("event.schema.json", bytes.NewReader(raw)); err != nil {
				t.Fatalf("ValidateJSONLAgainstJSONSchema(%s): %v", tc.file, err)
			}
			events, err := DecodeJSONLStrict[Event](bytes.NewReader(raw))
			if err != nil {
				t.Fatalf("DecodeJSONLStrict(%s): %v", tc.file, err)
			}
			if len(events) != tc.wantCount {
				t.Fatalf("len(events) = %d, want %d", len(events), tc.wantCount)
			}
			for i, ev := range events {
				data, err := json.Marshal(ev)
				if err != nil {
					t.Fatalf("event %d marshal: %v", i, err)
				}
				if err := ValidateAgainstJSONSchema("event.schema.json", data); err != nil {
					t.Fatalf("event %d re-validate: %v", i, err)
				}
			}
		})
	}
}

func TestStrictDecodeRejectsUnknownField(t *testing.T) {
	t.Run("JSON unknown field", func(t *testing.T) {
		payload := `{"schema_version":"v1","run_id":"r1","start_ts":"2026-10-22T01:20:00Z","end_ts":"2026-10-22T01:25:00Z","bogus":123}`
		if _, err := DecodeJSONStrict[RunWindow](strings.NewReader(payload)); err == nil {
			t.Fatal("expected error for unknown JSON field, got nil")
		}
		if err := ValidateAgainstJSONSchema("run_window.schema.json", []byte(payload)); err == nil {
			t.Fatal("expected JSON Schema error for additional property, got nil")
		}
	})

	t.Run("YAML unknown field", func(t *testing.T) {
		payload := []byte(`
schema_version: v1
scenario_name: test
unknown_top_level: true
jobs: []
`)
		if _, err := DecodeYAMLStrict[Schedule](payload); err == nil {
			t.Fatal("expected error for unknown YAML field, got nil")
		}
	})
}

func TestStrictDecodeRejectsTrailingPayload(t *testing.T) {
	payload := `{"schema_version":"v1","run_id":"r1","start_ts":"2026-10-22T01:20:00Z","end_ts":"2026-10-22T01:25:00Z"} {"extra":true}`
	if _, err := DecodeJSONStrict[RunWindow](strings.NewReader(payload)); err == nil {
		t.Fatal("expected error for trailing JSON token, got nil")
	}
	if err := ValidateAgainstJSONSchema("run_window.schema.json", []byte(payload)); err == nil {
		t.Fatal("expected ValidateAgainstJSONSchema error for trailing JSON token, got nil")
	}
}

func TestJSONSchemaRejectsMissingRequiredAndUnknownField(t *testing.T) {
	cases := []struct {
		name       string
		schemaName string
		payload    string
	}{
		{
			name:       "schedule missing jobs",
			schemaName: "schedule.schema.json",
			payload:    `{"schema_version":"v1","scenario_name":"s"}`,
		},
		{
			name:       "placement missing env_id",
			schemaName: "placement.schema.json",
			payload:    `{"schema_version":"v1","scenario_name":"s","jobs":[{"job":"j","assignments":[{"step":1,"instance_id":"i"}]}]}`,
		},
		{
			name:       "event missing dur_ms",
			schemaName: "event.schema.json",
			payload:    `{"ts":"2026-10-22T01:23:23Z","run":"r","event":"create","ok":true}`,
		},
		{
			name:       "event invalid source enum",
			schemaName: "event.schema.json",
			payload:    `{"ts":"2026-10-22T01:23:23Z","run":"r","event":"create","dur_ms":1,"ok":true,"source":"invalid_source"}`,
		},
		{
			name:       "run_window missing end_ts",
			schemaName: "run_window.schema.json",
			payload:    `{"schema_version":"v1","run_id":"r","start_ts":"2026-10-22T01:20:00Z"}`,
		},
		{
			name:       "report missing validity",
			schemaName: "report.schema.json",
			payload:    `{"schema_version":"v1","run_id":"r","scenario_name":"s","generated_at":"2026-10-22T01:20:00Z"}`,
		},
		{
			name:       "stats_row empty measurements",
			schemaName: "stats_row.schema.json",
			payload:    `{"schema_version":"v1","timestamp":"2026-10-22T01:20:00Z","test_name":"t","metric":"m","measurements":{}}`,
		},
		{
			name:       "system_description missing cluster",
			schemaName: "system_description.schema.json",
			payload:    `{"schema_version":"v1"}`,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := ValidateAgainstJSONSchema(tc.schemaName, []byte(tc.payload)); err == nil {
				t.Fatalf("expected JSON Schema error for %s, got nil", tc.name)
			}
		})
	}
}

func TestJSONLRejectsMalformedLineWithLineNumber(t *testing.T) {
	input := strings.Join([]string{
		`{"ts":"2026-10-22T01:23:23Z","run":"r","event":"create","dur_ms":10,"ok":true}`,
		`{"ts":"not-a-timestamp","run":"r","event":"create","dur_ms":10,"ok":true}`,
	}, "\n")
	_, err := DecodeJSONLStrict[Event](strings.NewReader(input))
	if err == nil {
		t.Fatal("expected error on line 2, got nil")
	}
	if !strings.Contains(err.Error(), "line 2:") {
		t.Fatalf("expected error to mention 'line 2:', got %v", err)
	}
}

func TestParkPolicyNormalization(t *testing.T) {
	for _, tc := range []struct {
		in      ParkPolicy
		want    ParkPolicy
		wantErr bool
	}{
		{in: "", want: ParkSuspendSync},
		{in: ParkSuspend, want: ParkSuspendSync},
		{in: ParkSuspendSync, want: ParkSuspendSync},
		{in: ParkSuspendAsync, want: ParkSuspendAsync},
		{in: ParkFork, want: ParkFork},
		{in: ParkDelete, want: ParkDelete},
		{in: ParkNone, want: ParkNone},
		{in: "bogus", wantErr: true},
	} {
		got, err := NormalizeParkPolicy(tc.in)
		if tc.wantErr {
			if err == nil {
				t.Fatalf("NormalizeParkPolicy(%q) succeeded, want error", tc.in)
			}
			continue
		}
		if err != nil {
			t.Fatalf("NormalizeParkPolicy(%q) failed: %v", tc.in, err)
		}
		if got != tc.want {
			t.Fatalf("NormalizeParkPolicy(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestValidationRules(t *testing.T) {
	t.Run("RunWindow end before start", func(t *testing.T) {
		rw := RunWindow{
			SchemaVersion: SchemaVersion,
			RunID:         "r1",
			StartTS:       "2026-10-22T01:25:00Z",
			EndTS:         "2026-10-22T01:20:00Z",
		}
		if err := rw.Validate(); err == nil {
			t.Fatal("expected error when end_ts < start_ts")
		}
	})

	t.Run("Report invalid without reasons", func(t *testing.T) {
		rep := Report{
			SchemaVersion: SchemaVersion,
			RunID:         "r1",
			ScenarioName:  "s1",
			GeneratedAt:   "2026-10-22T01:25:00Z",
			Validity:      ReportValidity{Valid: false},
		}
		if err := rep.Validate(); err == nil {
			t.Fatal("expected error when validity.valid=false and reasons is empty")
		}
	})

	t.Run("StatsRow NaN measurement", func(t *testing.T) {
		row := StatsRow{
			SchemaVersion: SchemaVersion,
			Timestamp:     "2026-10-22T01:25:00Z",
			TestName:      "t",
			Metric:        "m",
			Measurements:  map[string]float64{"x": math.NaN()},
		}
		if err := row.Validate(); err == nil {
			t.Fatal("expected error for NaN measurement")
		}
	})
}

func mustRead(t *testing.T, rel string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.FromSlash(rel))
	if err != nil {
		t.Fatalf("read %s: %v", rel, err)
	}
	return b
}

func roundTripJSON[T any](t *testing.T, schemaName string, val T) {
	t.Helper()
	encoded, err := json.Marshal(val)
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}
	if err := ValidateAgainstJSONSchema(schemaName, encoded); err != nil {
		t.Fatalf("round-trip ValidateAgainstJSONSchema(%s): %v\npayload: %s", schemaName, err, string(encoded))
	}
	if _, err := DecodeJSONStrict[T](bytes.NewReader(encoded)); err != nil {
		t.Fatalf("round-trip DecodeJSONStrict: %v", err)
	}
}
