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

package inventory

import (
	"bytes"
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	ateapipb "go.ate.dev/substrate/api/ateapi/v1"
)

var updateGolden = flag.Bool("update", false, "rewrite testdata/snapshot.golden.json from goldenSnapshot")

const goldenPath = "testdata/snapshot.golden.json"

// goldenSnapshot covers every object kind and every optional field once: a
// base and a derived template, a golden tag, one pool with one worker, a
// running and a crashed actor, and one ate-env-api Deployment.
func goldenSnapshot() *Snapshot {
	at := func(s int) time.Time { return time.Date(2026, 11, 3, 12, 0, s, 0, time.UTC) }
	s := &Snapshot{
		SchemaVersion: SchemaVersion,
		GeneratedAt:   at(5),
		Project:       "example-project",
		Cluster:       "bench-1",
		Location:      "us-central1",
		Reporter:      Reporter{Version: "v0.1.0", Commit: "abc1234", IntervalSeconds: 5},
		Atespaces:     []Atespace{{Name: "ate-env", CreateTime: at(0)}},
		Templates: []Template{
			{Atespace: "ate-env", Name: "default-template", GuestImage: "example.com/ate-env-guest@sha256:" + strings.Repeat("a", 64), WorkerPool: "default-template-workerpool", GoldenState: GoldenNone, CreateTime: at(0)},
			{Atespace: "ate-env", Name: "default-template-1a2b3c4d5e6f", Base: "default-template", RootfsImage: "example.com/python@sha256:1a2b3c4d5e6f" + strings.Repeat("0", 52), GuestImage: "example.com/ate-env-guest@sha256:" + strings.Repeat("a", 64), WorkerPool: "default-template-workerpool", GoldenState: GoldenReady, CreateTime: at(1)},
		},
		Tags: []Tag{{Atespace: "ate-env", Name: "golden-default-template-1a2b3c4d5e6f", SourceActor: "golden-7f3e", CreateTime: at(2)}},
		WorkerPools: []WorkerPool{{
			Namespace: "ate-env", Name: "default-template-workerpool", Replicas: 4, ReadyReplicas: 4,
			SandboxClass: "gvisor", SandboxConfig: "gvisor-default", WorkerImage: "example.com/ateworker-gvisor@sha256:" + strings.Repeat("b", 64),
		}},
		Workers: []Worker{{
			Name: "w-0a1b2c", Namespace: "ate-env", Pool: "default-template-workerpool", Pod: "default-template-workerpool-7c9d8-x2k4q", Node: "gke-bench-1-pool-1-abcd",
			State: WorkerStateActive, Capacity: Resources{CPUMillicores: 8000, MemoryBytes: 32 << 30}, Allocated: Resources{CPUMillicores: 2000, MemoryBytes: 4 << 30},
		}},
		Actors: []Actor{
			{
				UID: "8f2a0001", Atespace: "ate-env", Name: "env-001", Template: "default-template-1a2b3c4d5e6f", State: ActorStateRunning,
				AssignedNode: "gke-bench-1-pool-1-abcd", Worker: "w-0a1b2c", CreateTime: at(3), UpdateTime: at(4),
				Labels: map[string]string{"example.com/run-id": "r-1"},
			},
			{
				UID: "8f2a0002", Atespace: "ate-env", Name: "env-002", Template: "default-template-1a2b3c4d5e6f", SourceTag: "golden-default-template-1a2b3c4d5e6f",
				State: ActorStateCrashed, CreateTime: at(3), UpdateTime: at(5),
				Crash: &Crash{Message: "atelet: restore failed: snapshot download timed out", CrashTime: at(5)},
			},
		},
		Deployments: []Deployment{{
			Namespace: "ate-env", Name: "ate-env-api", Image: "example.com/ate-env-api@sha256:" + strings.Repeat("c", 64), Version: "v0.1.0",
			Ready: 1, Tracing: true, MetricsPort: 9090,
		}},
	}
	s.Counts = ComputeCounts(s.Actors)
	return s
}

func TestGoldenSnapshotJSON(t *testing.T) {
	var buf bytes.Buffer
	if err := goldenSnapshot().Encode(&buf); err != nil {
		t.Fatal(err)
	}
	if *updateGolden {
		if err := os.MkdirAll(filepath.Dir(goldenPath), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(goldenPath, buf.Bytes(), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(goldenPath)
	if err != nil {
		t.Fatalf("read golden: %v (run with -update to write it)", err)
	}
	if !bytes.Equal(want, buf.Bytes()) {
		t.Fatalf("golden snapshot differs from %s; run with -update if the format is meant to change\n--- got ---\n%s", goldenPath, buf.String())
	}
}

func TestGoldenValidatesDecodesAndRoundTrips(t *testing.T) {
	raw, err := os.ReadFile(goldenPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := ValidateAgainstJSONSchema(SnapshotSchema, raw); err != nil {
		t.Fatalf("golden does not validate against the JSON Schema: %v", err)
	}
	got, err := DecodeSnapshot(bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	if want := goldenSnapshot(); !reflect.DeepEqual(got, want) {
		t.Fatalf("decoded snapshot differs from goldenSnapshot:\n got %+v\nwant %+v", got, want)
	}
	var again bytes.Buffer
	if err := got.Encode(&again); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(again.Bytes(), raw) {
		t.Fatal("encode(decode(golden)) is not the golden bytes")
	}
}

func TestEmbeddedSchemaResolves(t *testing.T) {
	for _, name := range []string{SnapshotSchema, "v1/" + SnapshotSchema} {
		if _, err := LoadJSONSchema(name); err != nil {
			t.Fatalf("LoadJSONSchema(%q): %v", name, err)
		}
	}
}

func TestDecodeRejectsUnknownFieldAndTrailingData(t *testing.T) {
	var buf bytes.Buffer
	if err := goldenSnapshot().Encode(&buf); err != nil {
		t.Fatal(err)
	}
	valid := buf.String()

	unknown := strings.Replace(valid, `"project":`, `"token": "x", "project":`, 1)
	if _, err := DecodeSnapshot(strings.NewReader(unknown)); err == nil || !strings.Contains(err.Error(), "unknown field") {
		t.Fatalf("unknown field: got %v, want an unknown field error", err)
	}
	if _, err := DecodeSnapshot(strings.NewReader(valid + "{}")); err == nil || !strings.Contains(err.Error(), "trailing data") {
		t.Fatalf("trailing data: got %v, want a trailing data error", err)
	}
}

func TestValidateRejects(t *testing.T) {
	cases := []struct {
		name string
		edit func(*Snapshot)
		want string
	}{
		{"version", func(s *Snapshot) { s.SchemaVersion = "v2" }, "schema_version"},
		{"generated_at", func(s *Snapshot) { s.GeneratedAt = time.Time{} }, "generated_at"},
		{"cluster", func(s *Snapshot) { s.Cluster = "" }, "cluster"},
		{"interval", func(s *Snapshot) { s.Reporter.IntervalSeconds = 0 }, "interval_s"},
		{"actor state", func(s *Snapshot) { s.Actors[0].State = "paused"; s.Counts = ComputeCounts(s.Actors) }, "unknown state"},
		{"worker state", func(s *Snapshot) { s.Workers[0].State = "idle" }, "unknown state"},
		{"golden state", func(s *Snapshot) { s.Templates[1].GoldenState = "baking" }, "golden_state"},
		{"duplicate uid", func(s *Snapshot) { s.Actors[1].UID = s.Actors[0].UID; s.Counts = ComputeCounts(s.Actors) }, "repeats"},
		{"counts", func(s *Snapshot) { s.Counts.ActorsByState[ActorStateRunning]++ }, "counts do not match"},
		{"nil counts", func(s *Snapshot) { s.Counts = Counts{} }, "not null"},
		{"nil counts by atespace", func(s *Snapshot) { s.Counts.ActorsByAtespace = nil }, "not null"},
		{"credential label key", func(s *Snapshot) { s.Actors[0].Labels = map[string]string{"example.com/api-token": "x"} }, "names a credential"},
		{"credential label key, upper case", func(s *Snapshot) { s.Actors[0].Labels = map[string]string{"EXAMPLE.COM/CLIENT_SECRET": "x"} }, "names a credential"},
		{"negative capacity", func(s *Snapshot) { s.Workers[0].Capacity.CPUMillicores = -1 }, "negative"},
		{"nil actors", func(s *Snapshot) { s.Actors = nil; s.Counts = ComputeCounts(nil) }, "not null"},
		{"nil deployments", func(s *Snapshot) { s.Deployments = nil }, "not null"},
		{"atespace create_time", func(s *Snapshot) { s.Atespaces[0].CreateTime = time.Time{} }, "create_time"},
		{"template create_time", func(s *Snapshot) { s.Templates[0].CreateTime = time.Time{} }, "create_time"},
		{"tag create_time", func(s *Snapshot) { s.Tags[0].CreateTime = time.Time{} }, "create_time"},
		{"crash_time", func(s *Snapshot) { s.Actors[1].Crash.CrashTime = time.Time{} }, "crash_time"},
		{"negative ready", func(s *Snapshot) { s.Deployments[0].Ready = -1 }, "ready"},
		{"metrics_port too large", func(s *Snapshot) { s.Deployments[0].MetricsPort = 70000 }, "metrics_port"},
		{"metrics_port negative", func(s *Snapshot) { s.Deployments[0].MetricsPort = -1 }, "metrics_port"},
		{"duplicate atespace", func(s *Snapshot) { s.Atespaces = append(s.Atespaces, s.Atespaces[0]) }, "repeats"},
		{"duplicate template", func(s *Snapshot) { s.Templates[1].Name = s.Templates[0].Name }, "repeats"},
		{"duplicate tag", func(s *Snapshot) { s.Tags = append(s.Tags, s.Tags[0]) }, "repeats"},
		{"duplicate workerpool", func(s *Snapshot) { s.WorkerPools = append(s.WorkerPools, s.WorkerPools[0]) }, "repeats"},
		{"duplicate worker", func(s *Snapshot) { s.Workers = append(s.Workers, s.Workers[0]) }, "repeats"},
		{"duplicate deployment", func(s *Snapshot) { s.Deployments = append(s.Deployments, s.Deployments[0]) }, "repeats"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := goldenSnapshot()
			tc.edit(s)
			err := s.Validate()
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Validate() = %v, want an error containing %q", err, tc.want)
			}
		})
	}
	if err := goldenSnapshot().Validate(); err != nil {
		t.Fatalf("golden must validate: %v", err)
	}
}

// emptySnapshot is a cluster with nothing in it: every required list empty
// and Counts computed from the empty actor list.
func emptySnapshot() *Snapshot {
	s := &Snapshot{
		SchemaVersion: SchemaVersion, GeneratedAt: time.Date(2026, 11, 4, 0, 0, 0, 0, time.UTC),
		Project: "p", Cluster: "c", Reporter: Reporter{Version: "v0.1.0", IntervalSeconds: 5},
		Atespaces: []Atespace{}, Templates: []Template{}, WorkerPools: []WorkerPool{}, Workers: []Worker{},
		Actors: []Actor{}, Deployments: []Deployment{},
	}
	s.Counts = ComputeCounts(s.Actors)
	return s
}

// TestEmptyClusterValidatesInGoAndSchema pins that a cluster with nothing in
// it is a valid snapshot in both validators: empty lists encode as [] and
// pass the schema, while nil lists are refused by Validate before they can
// encode as null.
func TestEmptyClusterValidatesInGoAndSchema(t *testing.T) {
	s := emptySnapshot()
	if err := s.Validate(); err != nil {
		t.Fatalf("empty cluster: %v", err)
	}
	var buf bytes.Buffer
	if err := s.Encode(&buf); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(buf.String(), "null") {
		t.Fatalf("empty cluster encoded a null:\n%s", buf.String())
	}
	if err := ValidateAgainstJSONSchema(SnapshotSchema, buf.Bytes()); err != nil {
		t.Fatalf("empty cluster does not validate against the schema: %v", err)
	}
	if _, err := DecodeSnapshot(bytes.NewReader(buf.Bytes())); err != nil {
		t.Fatalf("empty cluster does not decode: %v", err)
	}
}

// TestNilCountsAreRejectedBeforeTheyEncodeAsNull pins the gap a zero Counts
// opened: with no actors, nil maps compare equal to the computed empty maps,
// yet they encode as null, which the schema refuses. Validate now refuses
// them first, so the two validators agree.
func TestNilCountsAreRejectedBeforeTheyEncodeAsNull(t *testing.T) {
	s := emptySnapshot()
	s.Counts = Counts{}
	if err := s.Validate(); err == nil || !strings.Contains(err.Error(), "not null") {
		t.Fatalf("Validate() = %v, want a not-null error for counts", err)
	}
	var buf bytes.Buffer
	if err := s.Encode(&buf); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), `"actors_by_state": null`) {
		t.Fatalf("expected nil counts to encode as null:\n%s", buf.String())
	}
	if err := ValidateAgainstJSONSchema(SnapshotSchema, buf.Bytes()); err == nil {
		t.Fatal("schema accepted null counts; Validate and the schema disagree")
	}
}

// TestCrashRecordIsIndependentOfState pins what Substrate does with
// ActorStatus.crash. The control plane sets it when an actor enters crashed
// and leaves it on the record while a crashed actor is deleting or reverting
// (controlapi ensureMarkedDeleting and ensureMarkedReverting change only the
// state); only the step that returns a reverted actor to suspended clears it.
// A crashed actor without a record is legal too: fakecontrol.SetState writes
// the state alone. Validate, the schema and DecodeSnapshot accept each
// combination.
func TestCrashRecordIsIndependentOfState(t *testing.T) {
	rec := &Crash{Message: "resume failed: snapshot download timed out", CrashTime: time.Date(2026, 11, 3, 12, 0, 5, 0, time.UTC)}
	cases := []struct {
		name  string
		state string
		crash *Crash
	}{
		{"crashed without record", ActorStateCrashed, nil},
		{"deleting keeps the record", ActorStateDeleting, rec},
		{"reverting keeps the record", ActorStateReverting, rec},
		{"running with a record", ActorStateRunning, rec},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := goldenSnapshot()
			s.Actors[1].State = tc.state
			s.Actors[1].Crash = tc.crash
			s.Counts = ComputeCounts(s.Actors)
			if err := s.Validate(); err != nil {
				t.Fatalf("Validate: %v", err)
			}
			var buf bytes.Buffer
			if err := s.Encode(&buf); err != nil {
				t.Fatal(err)
			}
			if err := ValidateAgainstJSONSchema(SnapshotSchema, buf.Bytes()); err != nil {
				t.Fatalf("schema: %v", err)
			}
			got, err := DecodeSnapshot(bytes.NewReader(buf.Bytes()))
			if err != nil {
				t.Fatalf("DecodeSnapshot: %v", err)
			}
			if !reflect.DeepEqual(got.Actors[1].Crash, tc.crash) || got.Actors[1].State != tc.state {
				t.Fatalf("round trip changed the actor: got state %q crash %+v", got.Actors[1].State, got.Actors[1].Crash)
			}
		})
	}
}

// TestDecodeRejectsWhatTheSchemaRejects pins that DecodeSnapshot is no looser
// than snapshot.schema.json. Each payload decodes into the Go types without
// error, because a nil slice, map or pointer and a zero int or string are
// legal Go values; the schema refuses each, and so DecodeSnapshot must too.
func TestDecodeRejectsWhatTheSchemaRejects(t *testing.T) {
	raw, err := os.ReadFile(goldenPath)
	if err != nil {
		t.Fatal(err)
	}
	item := func(doc map[string]any, list string, i int) map[string]any {
		return doc[list].([]any)[i].(map[string]any)
	}
	cases := []struct {
		name string
		edit func(doc map[string]any)
	}{
		{"tags null", func(d map[string]any) { d["tags"] = nil }},
		{"labels null", func(d map[string]any) { item(d, "actors", 0)["labels"] = nil }},
		{"crash null", func(d map[string]any) { item(d, "actors", 1)["crash"] = nil }},
		{"metrics_port 0", func(d map[string]any) { item(d, "deployments", 0)["metrics_port"] = 0 }},
		{"golden_state empty", func(d map[string]any) { item(d, "templates", 0)["golden_state"] = "" }},
		{"counts null", func(d map[string]any) { d["counts"].(map[string]any)["actors_by_state"] = nil }},
		{"credential label key", func(d map[string]any) {
			item(d, "actors", 0)["labels"] = map[string]any{"example.com/Access-Token": "x"}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var doc map[string]any
			if err := json.Unmarshal(raw, &doc); err != nil {
				t.Fatal(err)
			}
			tc.edit(doc)
			payload, err := json.Marshal(doc)
			if err != nil {
				t.Fatal(err)
			}
			var plain Snapshot
			if err := json.Unmarshal(payload, &plain); err != nil {
				t.Fatalf("the Go types alone refused the payload, so this case does not test the schema pass: %v", err)
			}
			_, err = DecodeSnapshot(bytes.NewReader(payload))
			if err == nil || !strings.Contains(err.Error(), SnapshotSchema) {
				t.Fatalf("DecodeSnapshot = %v, want a %s validation error", err, SnapshotSchema)
			}
		})
	}
}

func TestJSONSchemaRejectsMissingRequiredAndUnknownField(t *testing.T) {
	var buf bytes.Buffer
	if err := goldenSnapshot().Encode(&buf); err != nil {
		t.Fatal(err)
	}
	valid := buf.String()
	cases := []struct{ name, payload string }{
		{"missing cluster", strings.Replace(valid, `"cluster": "bench-1",`, ``, 1)},
		{"unknown top-level field", strings.Replace(valid, `"project":`, `"password": "x", "project":`, 1)},
		{"unknown actor field", strings.Replace(valid, `"uid": "8f2a0001",`, `"uid": "8f2a0001", "authorization": "x",`, 1)},
		{"bad actor state", strings.Replace(valid, `"state": "running"`, `"state": "paused"`, 1)},
		{"bad metrics port", strings.Replace(valid, `"metrics_port": 9090`, `"metrics_port": 0`, 1)},
		{"credential label key", strings.Replace(valid, `"example.com/run-id": "r-1"`, `"example.com/api-token": "r-1"`, 1)},
		{"upper-case credential label key", strings.Replace(valid, `"example.com/run-id": "r-1"`, `"EXAMPLE.COM/PASSWORD": "r-1"`, 1)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.payload == valid {
				t.Fatal("the edit did not apply; the fixture text changed")
			}
			if err := ValidateAgainstJSONSchema(SnapshotSchema, []byte(tc.payload)); err == nil {
				t.Fatal("schema accepted an invalid document")
			}
		})
	}
}

// TestStateValuesMatchSubstrateVocabulary pins the rule Substrate's
// ate.actor.state follows: the enum name lowercased without its prefix. Every
// enum value the vendored module knows must map, so a newer value shows up
// here before a reporter writes "unspecified" for it.
func TestStateValuesMatchSubstrateVocabulary(t *testing.T) {
	for v, name := range ateapipb.ActorState_name {
		got := ActorStateValue(ateapipb.ActorState(v))
		want := strings.ToLower(strings.TrimPrefix(name, "ACTOR_STATE_"))
		if got != want {
			t.Errorf("ActorStateValue(%s) = %q, want %q", name, got, want)
		}
		if !actorStates[got] {
			t.Errorf("ActorStateValue(%s) = %q is not in the snapshot vocabulary", name, got)
		}
	}
	for v, name := range ateapipb.WorkerState_name {
		got := WorkerStateValue(ateapipb.WorkerState(v))
		want := strings.ToLower(strings.TrimPrefix(name, "WORKER_STATE_"))
		if got != want {
			t.Errorf("WorkerStateValue(%s) = %q, want %q", name, got, want)
		}
		if !workerStates[got] {
			t.Errorf("WorkerStateValue(%s) = %q is not in the snapshot vocabulary", name, got)
		}
	}
}

func TestGoldenState(t *testing.T) {
	tag := &ateapipb.GoldenSnapshotStatus{GoldenTag: &ateapipb.ObjectRef{Name: "golden-x"}}
	failed := &ateapipb.GoldenSnapshotStatus{ErrorMessage: "bake failed"}
	cases := []struct {
		name       string
		status     *ateapipb.GoldenSnapshotStatus
		configured bool
		want       string
	}{
		{"not configured", nil, false, GoldenNone},
		{"not configured with stale status", tag, false, GoldenNone},
		{"configured, nothing yet", nil, true, GoldenPending},
		{"configured, empty status", &ateapipb.GoldenSnapshotStatus{}, true, GoldenPending},
		{"ready", tag, true, GoldenReady},
		{"failed", failed, true, GoldenFailed},
	}
	for _, tc := range cases {
		if got := GoldenState(tc.status, tc.configured); got != tc.want {
			t.Errorf("%s: GoldenState = %q, want %q", tc.name, got, tc.want)
		}
	}
}

// TestSchemaHasNoCredentialFields walks every property name in the JSON
// Schema. A snapshot is published to a shared bucket, so no field may carry a
// token, password or authorization value; the schema's additionalProperties
// false keeps an encoder from adding one by accident.
func TestSchemaHasNoCredentialFields(t *testing.T) {
	raw, err := SchemaFS.ReadFile("v1/" + SnapshotSchema)
	if err != nil {
		t.Fatal(err)
	}
	var doc any
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	var names []string
	var walk func(v any, underProperties bool)
	walk = func(v any, underProperties bool) {
		switch x := v.(type) {
		case map[string]any:
			for k, child := range x {
				if underProperties {
					names = append(names, k)
				}
				walk(child, k == "properties")
			}
		case []any:
			for _, child := range x {
				walk(child, false)
			}
		}
	}
	walk(doc, false)
	if len(names) == 0 {
		t.Fatal("walked no property names")
	}
	for _, n := range names {
		if labelKeyPattern.MatchString(n) {
			t.Errorf("schema property %q looks like a credential field", n)
		}
	}
}

// TestLabelKeyPatternMatchesSchema keeps the Go label-key rule and the
// schema's propertyNames rule on Actor.labels the same text, so a reader in
// another language refuses exactly the keys Validate refuses. It also pins
// the pattern's reach: every credential word in any letter case, and none of
// the ordinary keys.
func TestLabelKeyPatternMatchesSchema(t *testing.T) {
	raw, err := SchemaFS.ReadFile("v1/" + SnapshotSchema)
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Defs struct {
			Actor struct {
				Properties struct {
					Labels struct {
						PropertyNames struct {
							Not struct {
								Pattern string `json:"pattern"`
							} `json:"not"`
						} `json:"propertyNames"`
					} `json:"labels"`
				} `json:"properties"`
			} `json:"Actor"`
		} `json:"$defs"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	if got, want := doc.Defs.Actor.Properties.Labels.PropertyNames.Not.Pattern, labelKeyPattern.String(); got != want {
		t.Fatalf("schema propertyNames pattern\n got %q\nwant %q", got, want)
	}
	for _, w := range credentialWords {
		for _, k := range []string{w, strings.ToUpper(w), "example.com/" + w, "x-" + strings.ToUpper(w[:1]) + w[1:] + "-id"} {
			if !labelKeyPattern.MatchString(k) {
				t.Errorf("pattern does not match %q", k)
			}
		}
	}
	for _, k := range []string{"example.com/run-id", "app.kubernetes.io/name", "topology.kubernetes.io/zone", "ate.actor.state"} {
		if labelKeyPattern.MatchString(k) {
			t.Errorf("pattern matches ordinary key %q", k)
		}
	}
}

func TestSortIsStableByIdentity(t *testing.T) {
	a := goldenSnapshot()
	b := goldenSnapshot()
	b.Actors[0], b.Actors[1] = b.Actors[1], b.Actors[0]
	b.Templates[0], b.Templates[1] = b.Templates[1], b.Templates[0]
	a.Sort()
	b.Sort()
	var ba, bb bytes.Buffer
	if err := a.Encode(&ba); err != nil {
		t.Fatal(err)
	}
	if err := b.Encode(&bb); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(ba.Bytes(), bb.Bytes()) {
		t.Fatal("Sort did not make the two orderings encode identically")
	}
}
