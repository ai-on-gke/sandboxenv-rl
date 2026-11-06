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
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/agent-substrate/env/internal/benchmarking/schema"
	"github.com/agent-substrate/env/internal/benchmarking/swebench"
)

// snapshotWith writes one measurements_snapshots sample holding the given
// cumulative value for every bucket of one ttfi histogram plus its _count.
func snapshotWith(t *testing.T, dir string, ts time.Time, le map[string]float64) {
	t.Helper()
	var series []schema.PrometheusSeries
	var total float64
	for bound, v := range le {
		if bound == "+Inf" {
			total = v
		}
		val := v
		series = append(series, schema.PrometheusSeries{
			Name:   "ate_env_client_ttfi_seconds_bucket",
			Labels: map[string]string{"le": bound, "status": "ok", "source": "cold_create"},
			Status: "ok",
			Value:  &val,
		})
	}
	cnt := total
	series = append(series, schema.PrometheusSeries{
		Name:   "ate_env_client_ttfi_seconds_count",
		Labels: map[string]string{"status": "ok", "source": "cold_create"},
		Status: "ok",
		Value:  &cnt,
	})
	active := 4.0
	m := schema.Measurements{
		SchemaVersion: schema.SchemaVersion,
		RunID:         "snap-run",
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

func TestReportRollsUpSnapshotBucketSeriesByLabels(t *testing.T) {
	dir := t.TempDir()
	// A trace-derived ttfi of 500 ms that the metrics must replace.
	events := `{"run_id":"snap-run","event":"ttfi","env":"e1","dur_ms":500,"ok":true}` + "\n"
	if err := os.MkdirAll(filepath.Join(dir, "events"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "events", "trace.jsonl"), []byte(events), 0o644); err != nil {
		t.Fatal(err)
	}
	meas := schema.Measurements{SchemaVersion: schema.SchemaVersion, RunID: "snap-run", CollectedAt: time.Date(2026, 11, 5, 16, 0, 0, 0, time.UTC)}
	raw, _ := json.MarshalIndent(meas, "", "  ")
	if err := os.WriteFile(filepath.Join(dir, "measurements.json"), raw, 0o644); err != nil {
		t.Fatal(err)
	}
	// Two samples: all four le series grow, so the window holds 10 observations
	// with p50 at the 0.5 bucket boundary... 2 in <=0.25, 5 in <=0.5, 10 in <=1.
	t0 := time.Date(2026, 11, 5, 16, 0, 10, 0, time.UTC)
	snapshotWith(t, dir, t0, map[string]float64{"0.25": 0, "0.5": 0, "1": 0, "+Inf": 0})
	snapshotWith(t, dir, t0.Add(30*time.Second), map[string]float64{"0.25": 2, "0.5": 5, "1": 10, "+Inf": 10})

	var out, errBuf bytes.Buffer
	if code := execute([]string{"report", "-dest", dir}, &out, &errBuf); code != 0 {
		t.Fatalf("bench report failed (code=%d): %s", code, errBuf.String())
	}
	rawMeas, err := os.ReadFile(filepath.Join(dir, "measurements.json"))
	if err != nil {
		t.Fatal(err)
	}
	rolled, err := schema.DecodeJSONStrict[schema.Measurements](bytes.NewReader(rawMeas))
	if err != nil {
		t.Fatal(err)
	}
	buckets := 0
	for _, s := range rolled.PromMetrics {
		if s.Name == "ate_env_client_ttfi_seconds_bucket" {
			buckets++
			if len(s.Values) != 2 {
				t.Fatalf("bucket %s must carry both samples, got %v", s.Labels["le"], s.Values)
			}
		}
	}
	if buckets != 4 {
		t.Fatalf("rolled-up measurements hold %d ttfi bucket series, want 4 (one per le)", buckets)
	}
	rep, err := swebench.LoadOrSummarizeReport(filepath.Join(dir, "report.json"))
	if err != nil {
		t.Fatal(err)
	}
	st, ok := rep.StatFor(swebench.EventTTFI, "")
	if !ok || st.Count != 10 {
		t.Fatalf("ttfi stat = %+v ok=%v, want the 10 metric observations, not the single trace event", st, ok)
	}
	// p50 = rank 5 of 10 lands exactly on the 0.5 bucket boundary.
	if st.P50MS != 500 {
		t.Fatalf("ttfi p50 = %v, want 500 from the bucket quantile", st.P50MS)
	}
	if st.P95MS <= 500 || st.P95MS > 1000 {
		t.Fatalf("ttfi p95 = %v, want within the (0.5, 1] bucket", st.P95MS)
	}
}
