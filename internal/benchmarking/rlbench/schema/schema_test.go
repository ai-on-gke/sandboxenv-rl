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
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"sigs.k8s.io/yaml"
)

func openTestdataFile(t *testing.T, relName string) *os.File {
	t.Helper()
	p := filepath.Join("testdata", relName)
	f, err := os.Open(p)
	if err != nil {
		t.Fatalf("open testdata file %s: %v", p, err)
	}
	return f
}

func readTestdataBytes(t *testing.T, relName string) []byte {
	t.Helper()
	p := filepath.Join("testdata", relName)
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("read testdata file %s: %v", p, err)
	}
	return b
}

func assertRoundTripJSON[T Validator](t *testing.T, fixtureFile, schemaFile string) *T {
	t.Helper()
	raw := readTestdataBytes(t, fixtureFile)
	if err := ValidateAgainstJSONSchema(schemaFile, raw); err != nil {
		t.Fatalf("%s failed JSON Schema (%s): %v", fixtureFile, schemaFile, err)
	}
	decoded, err := DecodeJSONStrict[T](bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("%s failed DecodeJSONStrict: %v", fixtureFile, err)
	}
	marshaled, err := json.Marshal(decoded)
	if err != nil {
		t.Fatalf("%s failed json.Marshal: %v", fixtureFile, err)
	}
	if err := ValidateAgainstJSONSchema(schemaFile, marshaled); err != nil {
		t.Fatalf("%s marshaled output failed JSON Schema (%s): %v", fixtureFile, schemaFile, err)
	}
	roundTripped, err := DecodeJSONStrict[T](bytes.NewReader(marshaled))
	if err != nil {
		t.Fatalf("%s round-trip DecodeJSONStrict failed: %v", fixtureFile, err)
	}
	if !reflect.DeepEqual(decoded, roundTripped) {
		t.Fatalf("%s round-trip mismatch:\ngot:  %+v\nwant: %+v", fixtureFile, roundTripped, decoded)
	}
	return decoded
}

func assertRoundTripJSONL[T any](t *testing.T, fixtureFile, schemaFile string) []T {
	t.Helper()
	raw := readTestdataBytes(t, fixtureFile)
	if err := ValidateJSONLAgainstJSONSchema(schemaFile, bytes.NewReader(raw)); err != nil {
		t.Fatalf("%s failed JSONL Schema (%s): %v", fixtureFile, schemaFile, err)
	}
	items, err := DecodeJSONLStrict[T](bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("%s failed DecodeJSONLStrict: %v", fixtureFile, err)
	}
	if len(items) == 0 {
		t.Fatalf("%s decoded 0 items", fixtureFile)
	}
	var buf bytes.Buffer
	for i, item := range items {
		b, err := json.Marshal(item)
		if err != nil {
			t.Fatalf("%s item[%d] Marshal failed: %v", fixtureFile, i, err)
		}
		buf.Write(b)
		buf.WriteByte('\n')
	}
	if err := ValidateJSONLAgainstJSONSchema(schemaFile, bytes.NewReader(buf.Bytes())); err != nil {
		t.Fatalf("%s marshaled JSONL failed Schema (%s): %v", fixtureFile, schemaFile, err)
	}
	roundTripped, err := DecodeJSONLStrict[T](bytes.NewReader(buf.Bytes()))
	if err != nil {
		t.Fatalf("%s round-trip DecodeJSONLStrict failed: %v", fixtureFile, err)
	}
	if !reflect.DeepEqual(items, roundTripped) {
		t.Fatalf("%s round-trip mismatch:\ngot:  %+v\nwant: %+v", fixtureFile, roundTripped, items)
	}
	return items
}

func TestGoldenFixturesValidateAndRoundTrip(t *testing.T) {
	t.Run("scenario.yaml", func(t *testing.T) {
		rawYAML := readTestdataBytes(t, "scenario.yaml")
		jsonBytes, err := yaml.YAMLToJSON(rawYAML)
		if err != nil {
			t.Fatalf("yaml.YAMLToJSON(scenario.yaml): %v", err)
		}
		if err := ValidateAgainstJSONSchema("scenario.schema.json", jsonBytes); err != nil {
			t.Fatalf("scenario.yaml failed scenario.schema.json: %v", err)
		}
		sc, err := DecodeYAMLStrict[Scenario](rawYAML)
		if err != nil {
			t.Fatalf("DecodeYAMLStrict(scenario.yaml): %v", err)
		}
		if sc.Repeat != 2 || sc.SUT.Backend != "substrate" || sc.SUT.Access != "ate-env-api" {
			t.Fatalf("unexpected Scenario header/SUT: %+v", sc)
		}
		if sc.SUT.Substrate == nil || sc.SUT.Substrate.Atespace != "default" {
			t.Fatalf("expected SUT.Substrate.Atespace=default, got %+v", sc.SUT.Substrate)
		}
		if len(sc.Variants) != 1 || sc.Variants[0].Park != ParkSuspendAsync {
			t.Fatalf("unexpected Scenario.Variants: %+v", sc.Variants)
		}
		marshaledYAML, err := yaml.Marshal(sc)
		if err != nil {
			t.Fatalf("yaml.Marshal(Scenario): %v", err)
		}
		rt, err := DecodeYAMLStrict[Scenario](marshaledYAML)
		if err != nil {
			t.Fatalf("round-trip DecodeYAMLStrict(Scenario): %v", err)
		}
		if !reflect.DeepEqual(sc, rt) {
			t.Fatalf("Scenario round-trip mismatch:\ngot:  %+v\nwant: %+v", rt, sc)
		}
	})

	t.Run("schedule.json", func(t *testing.T) {
		sched := assertRoundTripJSON[Schedule](t, "schedule.json", "schedule.schema.json")
		if len(sched.Jobs) != 1 || sched.Jobs[0].StartOffsetMS != 500 || len(sched.Jobs[0].Steps[0].Tasks) != 2 {
			t.Fatalf("unexpected Schedule: %+v", sched)
		}
	})

	t.Run("placement.json", func(t *testing.T) {
		pl := assertRoundTripJSON[PlacementPlan](t, "placement.json", "placement.schema.json")
		if len(pl.Jobs) != 1 || len(pl.Jobs[0].Assignments) != 2 || pl.Jobs[0].Assignments[1].ReuseEnvID != "env-0" {
			t.Fatalf("unexpected PlacementPlan: %+v", pl)
		}
	})

	t.Run("run_window.json", func(t *testing.T) {
		rw := assertRoundTripJSON[RunWindow](t, "run_window.json", "run_window.schema.json")
		if rw.RunID != "run-123" || rw.SteadyStartTS == nil || rw.Metadata["harness"] != "swe-perf" {
			t.Fatalf("unexpected RunWindow: %+v", rw)
		}
	})

	t.Run("trace_event.jsonl", func(t *testing.T) {
		evs := assertRoundTripJSONL[TraceEvent](t, "trace_event.jsonl", "trace_event.schema.json")
		if len(evs) != 3 || evs[0].Image != "gcr.io/test/astropy:latest" || evs[1].Op != "exec" {
			t.Fatalf("unexpected TraceEvents: %+v", evs)
		}
	})

	t.Run("event.jsonl", func(t *testing.T) {
		evs := assertRoundTripJSONL[Event](t, "event.jsonl", "event.schema.json")
		if len(evs) != 2 || evs[0].Source != "golden_restore" || evs[1].Phase != "wire_claim_to_ready" {
			t.Fatalf("unexpected Events: %+v", evs)
		}
	})

	t.Run("measurements.json", func(t *testing.T) {
		m := assertRoundTripJSON[Measurements](t, "measurements.json", "measurements.schema.json")
		if m.CacheBeforeRef != "cache_before.json" || len(m.CustomMetrics) != 1 || m.PromMetrics[0].Value == nil || *m.PromMetrics[0].Value != 0 {
			t.Fatalf("unexpected Measurements: %+v", m)
		}
		if m.NodeCacheBefore[0].Errors == nil || *m.NodeCacheBefore[0].Errors != 0 {
			t.Fatalf("expected measured 0 errors pointer to be preserved, got %+v", m.NodeCacheBefore[0])
		}
	})

	t.Run("cache_before.json", func(t *testing.T) {
		nc := assertRoundTripJSON[NodeCacheArtifact](t, "cache_before.json", "node_cache.schema.json")
		if len(nc.Nodes) != 1 || nc.Nodes[0].Errors == nil || *nc.Nodes[0].Errors != 0 {
			t.Fatalf("unexpected NodeCacheArtifact: %+v", nc)
		}
	})

	t.Run("restore_timings.jsonl", func(t *testing.T) {
		rs := assertRoundTripJSONL[RestoreSample](t, "restore_timings.jsonl", "restore_sample.schema.json")
		if len(rs) != 2 || rs[0].CacheHit == nil || !*rs[0].CacheHit || rs[1].CacheHit == nil || *rs[1].CacheHit {
			t.Fatalf("unexpected RestoreSamples: %+v", rs)
		}
	})

	t.Run("system_description.json", func(t *testing.T) {
		sd := assertRoundTripJSON[SystemDescription](t, "system_description.json", "system_description.schema.json")
		if sd.GraderVersion != "v1.2.0" || sd.MachineTypes["worker-pool"].MachineType != "n2-standard-32" || sd.SandboxFootprint == nil {
			t.Fatalf("unexpected SystemDescription: %+v", sd)
		}
	})

	t.Run("report.json", func(t *testing.T) {
		rep := assertRoundTripJSON[Report](t, "report.json", "report.schema.json")
		if !rep.Validity.Valid || len(rep.CustomMetrics) != 1 || rep.Jobs["sim-loop-1"].Tasks != 4 {
			t.Fatalf("unexpected Report: %+v", rep)
		}
	})

	t.Run("stats.jsonl", func(t *testing.T) {
		rows := assertRoundTripJSONL[StatsRow](t, "stats.jsonl", "stats_row.schema.json")
		if len(rows) != 2 || rows[0].Metric != "trial_summary" || rows[1].Measurements["cache_errors"] != 0 {
			t.Fatalf("unexpected StatsRows: %+v", rows)
		}
	})
}

func injectBogusField(t *testing.T, rawJSON []byte) []byte {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(rawJSON, &m); err != nil {
		t.Fatalf("unmarshal for bogus injection: %v", err)
	}
	m["bogus"] = 123
	out, err := json.Marshal(m)
	if err != nil {
		t.Fatalf("marshal after bogus injection: %v", err)
	}
	return out
}

func TestUnknownFieldsRejectedAcrossAllTypes(t *testing.T) {
	t.Run("ScenarioYAML", func(t *testing.T) {
		raw := append(readTestdataBytes(t, "scenario.yaml"), []byte("\nbogus: 123\n")...)
		_, err := DecodeYAMLStrict[Scenario](raw)
		if err == nil || !strings.Contains(err.Error(), "bogus") {
			t.Fatalf("expected unknown field 'bogus' error for Scenario YAML, got: %v", err)
		}
		jsonBytes, err := yaml.YAMLToJSON(raw)
		if err != nil {
			t.Fatalf("YAMLToJSON: %v", err)
		}
		if err := ValidateAgainstJSONSchema("scenario.schema.json", jsonBytes); err == nil {
			t.Fatal("expected scenario.schema.json to reject unknown field 'bogus'")
		}
	})

	jsonCases := []struct {
		name       string
		fixture    string
		schemaFile string
		decode     func(r *bytes.Reader) error
	}{
		{
			name:       "Schedule",
			fixture:    "schedule.json",
			schemaFile: "schedule.schema.json",
			decode: func(r *bytes.Reader) error {
				_, err := DecodeJSONStrict[Schedule](r)
				return err
			},
		},
		{
			name:       "PlacementPlan",
			fixture:    "placement.json",
			schemaFile: "placement.schema.json",
			decode: func(r *bytes.Reader) error {
				_, err := DecodeJSONStrict[PlacementPlan](r)
				return err
			},
		},
		{
			name:       "RunWindow",
			fixture:    "run_window.json",
			schemaFile: "run_window.schema.json",
			decode: func(r *bytes.Reader) error {
				_, err := DecodeJSONStrict[RunWindow](r)
				return err
			},
		},
		{
			name:       "Measurements",
			fixture:    "measurements.json",
			schemaFile: "measurements.schema.json",
			decode: func(r *bytes.Reader) error {
				_, err := DecodeJSONStrict[Measurements](r)
				return err
			},
		},
		{
			name:       "NodeCacheArtifact",
			fixture:    "cache_before.json",
			schemaFile: "node_cache.schema.json",
			decode: func(r *bytes.Reader) error {
				_, err := DecodeJSONStrict[NodeCacheArtifact](r)
				return err
			},
		},
		{
			name:       "SystemDescription",
			fixture:    "system_description.json",
			schemaFile: "system_description.schema.json",
			decode: func(r *bytes.Reader) error {
				_, err := DecodeJSONStrict[SystemDescription](r)
				return err
			},
		},
		{
			name:       "Report",
			fixture:    "report.json",
			schemaFile: "report.schema.json",
			decode: func(r *bytes.Reader) error {
				_, err := DecodeJSONStrict[Report](r)
				return err
			},
		},
	}

	for _, tc := range jsonCases {
		t.Run(tc.name, func(t *testing.T) {
			bogusJSON := injectBogusField(t, readTestdataBytes(t, tc.fixture))
			err := tc.decode(bytes.NewReader(bogusJSON))
			if err == nil || !strings.Contains(err.Error(), `unknown field "bogus"`) {
				t.Fatalf("%s: expected error containing 'unknown field \"bogus\"', got: %v", tc.name, err)
			}
			if err := ValidateAgainstJSONSchema(tc.schemaFile, bogusJSON); err == nil {
				t.Fatalf("%s: expected JSON Schema %s to reject bogus field", tc.name, tc.schemaFile)
			}
		})
	}

	jsonlCases := []struct {
		name       string
		fixture    string
		schemaFile string
		decode     func(r *bytes.Reader) error
	}{
		{
			name:       "TraceEvent",
			fixture:    "trace_event.jsonl",
			schemaFile: "trace_event.schema.json",
			decode: func(r *bytes.Reader) error {
				_, err := DecodeJSONLStrict[TraceEvent](r)
				return err
			},
		},
		{
			name:       "Event",
			fixture:    "event.jsonl",
			schemaFile: "event.schema.json",
			decode: func(r *bytes.Reader) error {
				_, err := DecodeJSONLStrict[Event](r)
				return err
			},
		},
		{
			name:       "RestoreSample",
			fixture:    "restore_timings.jsonl",
			schemaFile: "restore_sample.schema.json",
			decode: func(r *bytes.Reader) error {
				_, err := DecodeJSONLStrict[RestoreSample](r)
				return err
			},
		},
		{
			name:       "StatsRow",
			fixture:    "stats.jsonl",
			schemaFile: "stats_row.schema.json",
			decode: func(r *bytes.Reader) error {
				_, err := DecodeJSONLStrict[StatsRow](r)
				return err
			},
		},
	}

	for _, tc := range jsonlCases {
		t.Run(tc.name, func(t *testing.T) {
			lines := bytes.Split(bytes.TrimSpace(readTestdataBytes(t, tc.fixture)), []byte("\n"))
			bogusLine := append(injectBogusField(t, lines[0]), '\n')
			err := tc.decode(bytes.NewReader(bogusLine))
			if err == nil || !strings.Contains(err.Error(), `unknown field "bogus"`) {
				t.Fatalf("%s: expected error containing 'unknown field \"bogus\"', got: %v", tc.name, err)
			}
			if err := ValidateJSONLAgainstJSONSchema(tc.schemaFile, bytes.NewReader(bogusLine)); err == nil {
				t.Fatalf("%s: expected JSONL Schema %s to reject bogus field", tc.name, tc.schemaFile)
			}
		})
	}
}

func TestParkPolicyValidationAndNormalization(t *testing.T) {
	for _, p := range []ParkPolicy{ParkSuspendAsync, ParkSuspendSync, ParkFork, ParkDelete, ParkNone} {
		got, err := NormalizeParkPolicy(p)
		if err != nil || got != p {
			t.Fatalf("NormalizeParkPolicy(%q) = (%q, %v), want (%q, nil)", p, got, err, p)
		}
	}
	norm, err := NormalizeParkPolicy(ParkLegacySuspend)
	if err != nil || norm != ParkSuspendSync {
		t.Fatalf("NormalizeParkPolicy(\"suspend\") = (%q, %v), want (%q, nil)", norm, err, ParkSuspendSync)
	}
	if _, err := NormalizeParkPolicy("banana"); err == nil {
		t.Fatal("expected NormalizeParkPolicy(\"banana\") to fail")
	}

	job := JobSpec{
		Name:       "sim-1",
		Kind:       "simulated",
		Dataset:    "d.jsonl",
		Trajectory: "t.yaml",
		Park:       "banana",
	}
	if err := job.Validate(); err == nil || !strings.Contains(err.Error(), "invalid park policy") {
		t.Fatalf("expected JobSpec.Validate to reject park=\"banana\", got: %v", err)
	}

	overlay := FeatureOverlay{
		Name: "bad-park-overlay",
		Park: "banana",
	}
	if err := overlay.Validate(); err == nil || !strings.Contains(err.Error(), "invalid park policy") {
		t.Fatalf("expected FeatureOverlay.Validate to reject park=\"banana\", got: %v", err)
	}
}

func TestJobAndRunWindowSemanticValidation(t *testing.T) {
	// Simulated job in steps mode requires steps >= 1.
	simSteps := JobSpec{
		Name:       "sim-steps",
		Kind:       "simulated",
		LoaderMode: "steps",
		Dataset:    "d.jsonl",
		Trajectory: "t.yaml",
		Steps:      0,
	}
	if err := simSteps.Validate(); err == nil {
		t.Fatal("expected simulated steps job with steps=0 to fail validation")
	}

	// Real job requires real_mode = measure|trace and cannot set loader_mode.
	realJob := JobSpec{
		Name:     "real-1",
		Kind:     "real",
		RealMode: "measure",
	}
	if err := realJob.Validate(); err != nil {
		t.Fatalf("valid real job failed validation: %v", err)
	}
	badReal := JobSpec{
		Name:     "real-bad",
		Kind:     "real",
		RealMode: "steps",
	}
	if err := badReal.Validate(); err == nil {
		t.Fatal("expected real job with real_mode=steps to fail validation")
	}

	// RunWindow validation checks end_ts >= start_ts and steady window within [start_ts, end_ts].
	badWindow := `{"schema_version":"v1","run_id":"r1","start_ts":"2026-10-22T01:25:00Z","end_ts":"2026-10-22T01:20:00Z"}`
	if _, err := DecodeJSONStrict[RunWindow](strings.NewReader(badWindow)); err == nil {
		t.Fatal("expected RunWindow with end_ts < start_ts to fail")
	}
	badSteady := `{"schema_version":"v1","run_id":"r1","start_ts":"2026-10-22T01:20:00Z","end_ts":"2026-10-22T01:25:00Z","steady_start_ts":"2026-10-22T01:19:00Z"}`
	if _, err := DecodeJSONStrict[RunWindow](strings.NewReader(badSteady)); err == nil {
		t.Fatal("expected RunWindow with steady_start_ts < start_ts to fail")
	}

	// PrometheusSeries requires value/values when status is ok, and err when status is error.
	missingVal := `{"name":"m1","status":"ok"}`
	if _, err := DecodeJSONStrict[PrometheusSeries](strings.NewReader(missingVal)); err == nil {
		t.Fatal("expected PrometheusSeries with status=ok and no value to fail")
	}
	errSeries := `{"name":"m1","status":"error","err":"connection refused"}`
	if _, err := DecodeJSONStrict[PrometheusSeries](strings.NewReader(errSeries)); err != nil {
		t.Fatalf("expected PrometheusSeries with status=error and err message to pass, got: %v", err)
	}

	// Report requires reasons when valid=false.
	invalidRep := `{"schema_version":"v1","run_id":"r1","scenario_name":"s1","generated_at":"2026-10-22T01:26:00Z","validity":{"valid":false}}`
	if _, err := DecodeJSONStrict[Report](strings.NewReader(invalidRep)); err == nil {
		t.Fatal("expected Report with valid=false and empty reasons to fail")
	}
}

func TestBaselineLiveClusterFilesDecodeCleanly(t *testing.T) {
	evalFile := openTestdataFile(t, "baseline-eval.jsonl")
	defer evalFile.Close()

	evalEvents, err := DecodeJSONLStrict[Event](evalFile)
	if err != nil {
		t.Fatalf("DecodeJSONLStrict on baseline-eval.jsonl failed: %v", err)
	}
	if len(evalEvents) != 29 {
		t.Fatalf("baseline-eval.jsonl event count = %d, want 29", len(evalEvents))
	}
	if err := ValidateJSONLAgainstJSONSchema("event.schema.json", bytes.NewReader(readTestdataBytes(t, "baseline-eval.jsonl"))); err != nil {
		t.Fatalf("baseline-eval.jsonl failed event.schema.json: %v", err)
	}

	first := evalEvents[0]
	if first.Run != "swe-base-eval3" || first.Event != "create" || first.Env != "swe-base-eval3-astropy--astropy-12907-0" || !first.OK {
		t.Fatalf("unexpected first eval event: %+v", first)
	}
	gradeEv := evalEvents[19]
	if gradeEv.Event != "step" || gradeEv.Name != "grade" || gradeEv.Reward == nil || *gradeEv.Reward != 1 || gradeEv.Exit == nil || *gradeEv.Exit != 0 || gradeEv.Tail == "" {
		t.Fatalf("grade event fields not preserved: %+v", gradeEv)
	}
	lastEval := evalEvents[len(evalEvents)-1]
	if lastEval.Event != "run" || lastEval.Name != "eval" || lastEval.Count != 2 {
		t.Fatalf("run event fields not preserved: %+v", lastEval)
	}

	stepsFile := openTestdataFile(t, "baseline-steps.jsonl")
	defer stepsFile.Close()

	stepEvents, err := DecodeJSONLStrict[Event](stepsFile)
	if err != nil {
		t.Fatalf("DecodeJSONLStrict on baseline-steps.jsonl failed: %v", err)
	}
	if len(stepEvents) != 58 {
		t.Fatalf("baseline-steps.jsonl event count = %d, want 58", len(stepEvents))
	}
	if err := ValidateJSONLAgainstJSONSchema("event.schema.json", bytes.NewReader(readTestdataBytes(t, "baseline-steps.jsonl"))); err != nil {
		t.Fatalf("baseline-steps.jsonl failed event.schema.json: %v", err)
	}

	barrierEv := stepEvents[26]
	if barrierEv.Event != "step_barrier" || barrierEv.Step != 1 || barrierEv.Count != 2 {
		t.Fatalf("step_barrier event fields not preserved: %+v", barrierEv)
	}
	wakeEv := stepEvents[30]
	if wakeEv.Event != "wake" || wakeEv.Step != 2 || wakeEv.Task != 1 || wakeEv.Attempts != 1 {
		t.Fatalf("wake event fields not preserved: %+v", wakeEv)
	}
}

func TestPythonSchemaValidatorHelper(t *testing.T) {
	pyBin, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 not found on PATH")
	}
	pairs := [][2]string{
		{"schedule", filepath.Join("testdata", "schedule.json")},
		{"placement", filepath.Join("testdata", "placement.json")},
		{"run_window", filepath.Join("testdata", "run_window.json")},
		{"trace_event", filepath.Join("testdata", "trace_event.jsonl")},
		{"event", filepath.Join("testdata", "event.jsonl")},
		{"event", filepath.Join("testdata", "baseline-eval.jsonl")},
		{"event", filepath.Join("testdata", "baseline-steps.jsonl")},
		{"measurements", filepath.Join("testdata", "measurements.json")},
		{"node_cache", filepath.Join("testdata", "cache_before.json")},
		{"restore_sample", filepath.Join("testdata", "restore_timings.jsonl")},
		{"system_description", filepath.Join("testdata", "system_description.json")},
		{"report", filepath.Join("testdata", "report.json")},
		{"stats_row", filepath.Join("testdata", "stats.jsonl")},
	}
	for _, p := range pairs {
		cmd := exec.Command(pyBin, "-B", "validate_schema.py", p[0], p[1])
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("python3 validate_schema.py %s %s failed: %v\n%s", p[0], p[1], err, string(out))
		}
	}

	bogusPath := filepath.Join(t.TempDir(), "bogus.json")
	if err := os.WriteFile(bogusPath, injectBogusField(t, readTestdataBytes(t, "run_window.json")), 0o600); err != nil {
		t.Fatalf("write bogus.json: %v", err)
	}
	cmd := exec.Command(pyBin, "-B", "validate_schema.py", "run_window", bogusPath)
	out, err := cmd.CombinedOutput()
	if err == nil || !strings.Contains(string(out), `unknown field "bogus"`) {
		t.Fatalf("expected validate_schema.py to reject unknown field \"bogus\", err=%v out=%s", err, string(out))
	}
}
