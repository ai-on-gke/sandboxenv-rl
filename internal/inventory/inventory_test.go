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
		{"crash without state", func(s *Snapshot) { s.Actors[0].Crash = &Crash{Message: "x", CrashTime: s.GeneratedAt} }, "crash is present"},
		{"crashed without record", func(s *Snapshot) { s.Actors[1].Crash = nil }, "crash is present"},
		{"counts", func(s *Snapshot) { s.Counts.ActorsByState[ActorStateRunning]++ }, "counts do not match"},
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

// TestEmptyClusterValidatesInGoAndSchema pins that a cluster with nothing in
// it is a valid snapshot in both validators: empty lists encode as [] and
// pass the schema, while nil lists are refused by Validate before they can
// encode as null.
func TestEmptyClusterValidatesInGoAndSchema(t *testing.T) {
	s := &Snapshot{
		SchemaVersion: SchemaVersion, GeneratedAt: time.Date(2026, 11, 4, 0, 0, 0, 0, time.UTC),
		Project: "p", Cluster: "c", Reporter: Reporter{Version: "v0.1.0", IntervalSeconds: 5},
		Atespaces: []Atespace{}, Templates: []Template{}, WorkerPools: []WorkerPool{}, Workers: []Worker{},
		Actors: []Actor{}, Deployments: []Deployment{},
	}
	s.Counts = ComputeCounts(s.Actors)
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
		for _, bad := range []string{"token", "password", "authorization", "secret", "credential"} {
			if strings.Contains(strings.ToLower(n), bad) {
				t.Errorf("schema property %q looks like a credential field", n)
			}
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
