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

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/agent-substrate/env/internal/benchmarking/schema"
)

func mustTime(s string) time.Time {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		panic(err)
	}
	return t
}

// snapshotWith writes one measurements_snapshots sample with the given
// cumulative values for a ttfi histogram's buckets and an active_sandboxes
// scalar.
func snapshotWith(t *testing.T, dir string, ts time.Time, le map[string]float64) {
	t.Helper()
	var series []schema.PrometheusSeries
	for bound, v := range le {
		val := v
		series = append(series, schema.PrometheusSeries{
			Name:   "ate_env_client_ttfi_seconds_bucket",
			Labels: map[string]string{"le": bound, "status": "ok", "source": "cold_create"},
			Status: "ok",
			Value:  &val,
		})
	}
	active := 4.0
	m := schema.Measurements{
		SchemaVersion: schema.SchemaVersion,
		RunID:         "snap-only",
		CollectedAt:   ts,
		PromMetrics:   series,
		ServerSummary: map[string]schema.MetricSummary{
			"active_sandboxes": {Count: 1, Sum: active, Min: active, Max: active, P50: active, P90: active, P95: active, P99: active, Unit: "sandboxes"},
		},
	}
	raw, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "measurements_snapshots"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "measurements_snapshots", ts.Format("20060102T150405.000Z")+".json"), raw, 0o644); err != nil {
		t.Fatal(err)
	}
}

// measureFakeKube answers the kubectl calls the k8s collector makes with a
// one-node, one-worker cluster.
type measureFakeKube struct{}

func (measureFakeKube) Run(_ context.Context, args ...string) ([]byte, error) {
	cmd := strings.Join(args, " ")
	switch {
	case cmd == "config current-context":
		return []byte("gke_test_zone_cluster\n"), nil
	case cmd == "get nodes -o json":
		return []byte(`{"items":[{"metadata":{"name":"n1","labels":{"cloud.google.com/gke-nodepool":"sandbox-pool","node.kubernetes.io/instance-type":"n2-standard-32","topology.kubernetes.io/zone":"z"}},"status":{"capacity":{"cpu":"32","memory":"128Gi"},"allocatable":{"cpu":"30","memory":"120Gi"},"nodeInfo":{"kubeletVersion":"v1.34.0"}}}]}`), nil
	case cmd == "get pods -A -o json":
		return []byte(`{"items":[{"metadata":{"name":"wp-0","namespace":"ate-env","creationTimestamp":"2026-04-20T18:00:00Z","labels":{"ate-env/workerpool":"default-pool"}},"spec":{"nodeName":"n1","containers":[{"name":"worker","image":"w:1","resources":{"requests":{"cpu":"2","memory":"4Gi"},"limits":{"cpu":"2","memory":"4Gi"}}}]},"status":{"phase":"Running","conditions":[{"type":"Ready","status":"True","lastTransitionTime":"2026-04-20T18:00:05Z"}],"containerStatuses":[{"name":"worker","restartCount":0}]}}]}`), nil
	case strings.HasPrefix(cmd, "get --raw /apis/metrics.k8s.io/v1beta1/nodes"):
		return []byte(`{"items":[{"metadata":{"name":"n1"},"usage":{"cpu":"1500m","memory":"6Gi"}}]}`), nil
	case strings.HasPrefix(cmd, "get --raw /apis/metrics.k8s.io/v1beta1/pods"):
		return []byte(`{"items":[{"metadata":{"name":"wp-0","namespace":"ate-env"},"containers":[{"name":"worker","usage":{"cpu":"250m","memory":"512Mi"}}]}]}`), nil
	default:
		return []byte("{}"), nil
	}
}

// The sampler stands in for the single pass in the documented flow, so it
// must leave the same files behind: system_description.json and a
// measurements.json, or bench validate fails on the bundle.
func TestMeasureIntervalWritesSystemDescriptionAndMeasurements(t *testing.T) {
	prev := measureKubeRunner
	measureKubeRunner = measureFakeKube{}
	t.Cleanup(func() { measureKubeRunner = prev })

	dir := t.TempDir()
	var out, errBuf bytes.Buffer
	code := execute([]string{"measure", "-k8s", "-dest", dir, "-run-id", "smp", "-interval", "20ms", "-duration", "120ms", "-scopes", "k8s,substrate"}, &out, &errBuf)
	if code != 0 {
		t.Fatalf("bench measure -interval failed (code=%d): %s", code, errBuf.String())
	}
	if !strings.Contains(errBuf.String(), "snapshot(s)") {
		t.Fatalf("expected a snapshot summary line, got: %s", errBuf.String())
	}
	snaps, _ := filepath.Glob(filepath.Join(dir, "measurements_snapshots", "*.json"))
	if len(snaps) < 2 {
		t.Fatalf("want >= 2 snapshots, got %d", len(snaps))
	}
	rawSys, err := os.ReadFile(filepath.Join(dir, "system_description.json"))
	if err != nil {
		t.Fatalf("system_description.json must be written by the sampler: %v", err)
	}
	sys, err := schema.DecodeJSONStrict[schema.SystemDescription](bytes.NewReader(rawSys))
	if err != nil || sys.Cluster != "gke_test_zone_cluster" {
		t.Fatalf("system_description.json: err=%v cluster=%q", err, sys.Cluster)
	}
	rawMeas, err := os.ReadFile(filepath.Join(dir, "measurements.json"))
	if err != nil {
		t.Fatalf("measurements.json must be rolled up when the sampler stops: %v", err)
	}
	meas, err := schema.DecodeJSONStrict[schema.Measurements](bytes.NewReader(rawMeas))
	if err != nil {
		t.Fatal(err)
	}
	if meas.RunID != "smp" || meas.ServerSummary["worker_node_count"].Count < 2 {
		t.Fatalf("rolled-up measurements should carry the run id and fold every sample: %+v", meas.ServerSummary["worker_node_count"])
	}
}

// A bundle with snapshots but no measurements.json (sampler ran on its own,
// or an older bench measure) still reports: bench report builds the file.
func TestReportCreatesMeasurementsFromSnapshotsWhenAbsent(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "events"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "events", "run.jsonl"), []byte(`{"run_id":"snap-only","event":"task","env":"e1","dur_ms":10,"ok":true}`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	snapshotWith(t, dir, mustTime("2026-11-05T16:00:10Z"), map[string]float64{"0.5": 0, "1": 0, "5": 0, "+Inf": 0})
	snapshotWith(t, dir, mustTime("2026-11-05T16:00:40Z"), map[string]float64{"0.5": 3, "1": 4, "5": 4, "+Inf": 4})
	var out, errBuf bytes.Buffer
	if code := execute([]string{"report", "-dest", dir}, &out, &errBuf); code != 0 {
		t.Fatalf("bench report failed (code=%d): %s", code, errBuf.String())
	}
	rawMeas, err := os.ReadFile(filepath.Join(dir, "measurements.json"))
	if err != nil {
		t.Fatalf("bench report must create measurements.json from snapshots: %v", err)
	}
	meas, err := schema.DecodeJSONStrict[schema.Measurements](bytes.NewReader(rawMeas))
	if err != nil {
		t.Fatal(err)
	}
	if meas.RunID != "snap-only" || meas.ServerSummary["active_sandboxes"].Count != 2 {
		t.Fatalf("unexpected rolled-up measurements: run=%q active=%+v", meas.RunID, meas.ServerSummary["active_sandboxes"])
	}
	// Every bucket series survives the fold as its own series, keyed by
	// label set, with one value per sample in time order: that is what the
	// metrics-first report's windowed delta reads.
	byLE := map[string][]float64{}
	for _, ps := range meas.PromMetrics {
		if ps.Name == "ate_env_client_ttfi_seconds_bucket" {
			byLE[ps.Labels["le"]] = ps.Values
		}
	}
	want := map[string][]float64{"0.5": {0, 3}, "1": {0, 4}, "5": {0, 4}, "+Inf": {0, 4}}
	if len(byLE) != len(want) {
		t.Fatalf("folded bucket series = %v, want the 4 le buckets", byLE)
	}
	for le, vals := range want {
		if got := byLE[le]; len(got) != 2 || got[0] != vals[0] || got[1] != vals[1] {
			t.Fatalf("bucket le=%s values = %v, want %v", le, got, vals)
		}
	}
}

func TestMeasureIntervalRejectsSnapshotsDirOutsideDest(t *testing.T) {
	dir := t.TempDir()
	var out, errBuf bytes.Buffer
	code := execute([]string{"measure", "-dest", dir, "-run-id", "x", "-interval", "20ms", "-duration", "40ms",
		"-snapshots-dir", filepath.Join(t.TempDir(), "elsewhere"), "-scopes", "substrate"}, &out, &errBuf)
	if code == 0 || !strings.Contains(errBuf.String(), "-snapshots-dir") {
		t.Fatalf("want a refusal naming -snapshots-dir (code=%d): %s", code, errBuf.String())
	}
	if _, err := os.Stat(filepath.Join(dir, "measurements.json")); err == nil {
		t.Fatal("nothing must be written when the flags disagree")
	}
}

func TestRolledUpMeasurementsRefuseToOverwriteAnUndecodableFile(t *testing.T) {
	dir := t.TempDir()
	snapshotWith(t, dir, mustTime("2026-11-05T16:00:10Z"), map[string]float64{"+Inf": 0})
	outPath := filepath.Join(dir, "measurements.json")
	if err := os.WriteFile(outPath, []byte(`{"schema_version":"v1","run_id":"r","not_a_field":1}`), 0o644); err != nil {
		t.Fatal(err)
	}
	err := writeRolledUpMeasurements(dir, outPath, "r", nil)
	if err == nil || !strings.Contains(err.Error(), "does not decode") {
		t.Fatalf("err = %v, want a decode error, not a silent restart from empty", err)
	}
	raw, _ := os.ReadFile(outPath)
	if !strings.Contains(string(raw), "not_a_field") {
		t.Fatal("the undecodable file must be left as it was")
	}
}

const sampledRunScenario = `schema_version: v1
name: sampled-smoke
footing: warm
seed: 1
sut:
  backend: substrate
  access: ate-env-api
  endpoint: localhost:7777
jobs:
  - name: job-closed
    kind: simulated
    mode: closed-loop
    steps: 1
    batch_size: 1
    concurrency: 1
    park_policy: snapshot
    instances: 1
`

// A run whose telemetry never arrived is not a result: bench run exits
// non-zero when the sampler wrote no snapshot, but only after every artifact
// of the run is on disk, so the events and report can still be inspected.
func TestBenchRunFailsWhenTheSamplerWroteNoSnapshotButKeepsArtifacts(t *testing.T) {
	dir := t.TempDir()
	scenPath := filepath.Join(dir, "scen.yaml")
	if err := os.WriteFile(scenPath, []byte(sampledRunScenario), 0o644); err != nil {
		t.Fatal(err)
	}
	runDir := filepath.Join(dir, "out")
	var out, errBuf bytes.Buffer
	// Nothing listens on port 1: every scrape fails, so no snapshot is written.
	code := execute([]string{"run", "-scenario", scenPath, "-dest", runDir, "-run-id", "ns",
		"-snapshot-interval", "10ms", "-prom-url", "http://127.0.0.1:1"}, &out, &errBuf)
	if code == 0 || !strings.Contains(errBuf.String(), "no snapshot was written") {
		t.Fatalf("want a non-zero exit naming the missing snapshots (code=%d): %s", code, errBuf.String())
	}
	for _, rel := range []string{"events.jsonl", "report.json", "report.md", "measurements.json", "system_description.json"} {
		if _, err := os.Stat(filepath.Join(runDir, rel)); err != nil {
			t.Fatalf("artifact %s must still be written: %v", rel, err)
		}
	}
}

// bench spinup -dest records the WorkerPool spin-up into measurements.json
// before bench run starts; the run must fold its own numbers in, not start over.
func TestBenchRunKeepsMeasurementsRecordedBeforeTheRun(t *testing.T) {
	dir := t.TempDir()
	scenPath := filepath.Join(dir, "scen.yaml")
	if err := os.WriteFile(scenPath, []byte(sampledRunScenario), 0o644); err != nil {
		t.Fatal(err)
	}
	runDir := filepath.Join(dir, "out")
	if err := os.MkdirAll(runDir, 0o755); err != nil {
		t.Fatal(err)
	}
	pre := schema.Measurements{
		SchemaVersion: schema.SchemaVersion, RunID: "keep", CollectedAt: mustTime("2026-11-05T15:59:00Z"),
		ServerSummary: map[string]schema.MetricSummary{
			"workerpool_spinup_wall_ms": {Count: 1, Sum: 4200, Min: 4200, Max: 4200, P50: 4200, P90: 4200, P95: 4200, P99: 4200, Unit: "ms"},
		},
	}
	raw, _ := json.MarshalIndent(pre, "", "  ")
	if err := os.WriteFile(filepath.Join(runDir, "measurements.json"), raw, 0o644); err != nil {
		t.Fatal(err)
	}
	var out, errBuf bytes.Buffer
	if code := execute([]string{"run", "-scenario", scenPath, "-dest", runDir, "-run-id", "keep"}, &out, &errBuf); code != 0 {
		t.Fatalf("bench run failed (code=%d): %s", code, errBuf.String())
	}
	after, err := os.ReadFile(filepath.Join(runDir, "measurements.json"))
	if err != nil {
		t.Fatal(err)
	}
	meas, err := schema.DecodeJSONStrict[schema.Measurements](bytes.NewReader(after))
	if err != nil {
		t.Fatal(err)
	}
	if got := meas.ServerSummary["workerpool_spinup_wall_ms"]; got.P50 != 4200 {
		t.Fatalf("workerpool_spinup_wall_ms = %+v, want the pre-run value kept", got)
	}
	// And a file that is there but does not decode is an error, not a blank slate.
	if err := os.WriteFile(filepath.Join(runDir, "measurements.json"), []byte(`{"schema_version":"v1","bogus":true}`), 0o644); err != nil {
		t.Fatal(err)
	}
	errBuf.Reset()
	if code := execute([]string{"run", "-scenario", scenPath, "-dest", runDir, "-run-id", "keep"}, &out, &errBuf); code == 0 || !strings.Contains(errBuf.String(), "does not decode") {
		t.Fatalf("want a decode error (code=%d): %s", code, errBuf.String())
	}
}
