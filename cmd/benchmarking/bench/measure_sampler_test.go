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
	snapshotWith(t, dir, mustTime("2026-11-05T16:00:10Z"), map[string]float64{"0.5": 0, "+Inf": 0})
	snapshotWith(t, dir, mustTime("2026-11-05T16:00:40Z"), map[string]float64{"0.5": 3, "+Inf": 4})
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
}
