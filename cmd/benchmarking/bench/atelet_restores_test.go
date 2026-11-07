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
	"github.com/agent-substrate/env/internal/benchmarking/swebench"
)

type recordingGCSUploadRunner struct {
	calls [][2]string
}

func (r *recordingGCSUploadRunner) UploadDir(_ context.Context, srcDir, destGCSURI string) ([]byte, error) {
	r.calls = append(r.calls, [2]string{srcDir, destGCSURI})
	return []byte("copied\n"), nil
}

func TestReportValidateAndUploadWithAteletRestoresAndSnapshots(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "events"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "measurements_snapshots"), 0o755); err != nil {
		t.Fatal(err)
	}

	baseT := time.Date(2026, 11, 6, 20, 0, 0, 0, time.UTC)
	r2eImg := "us-central1-docker.pkg.dev/p/r2e/img1@sha256:" + strings.Repeat("1", 64)
	sweImg := "us-central1-docker.pkg.dev/p/swe/img1@sha256:" + strings.Repeat("2", 64)

	writeJSONL := func(path string, rows []any) {
		t.Helper()
		var buf bytes.Buffer
		for _, row := range rows {
			b, err := json.Marshal(row)
			if err != nil {
				t.Fatal(err)
			}
			buf.Write(b)
			buf.WriteByte('\n')
		}
		if err := os.WriteFile(path, buf.Bytes(), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	rewardOne := 1
	r2eEvents := []any{
		swebench.Event{Time: baseT.Add(100 * time.Millisecond), RunID: "bkl-a1-r2e", Job: "r2e", Worker: "r2e", Task: 0, Step: 1, Instance: "r2e-1", Image: r2eImg, Env: "bkl-a1-r2e-0", Event: swebench.EventCreate, DurMS: 50, OK: true},
		swebench.Event{Time: baseT.Add(300 * time.Millisecond), RunID: "bkl-a1-r2e", Job: "r2e", Worker: "r2e", Task: 0, Step: 1, Instance: "r2e-1", Image: r2eImg, Env: "bkl-a1-r2e-0", Event: swebench.EventTTFI, DurMS: 200, OK: true},
		swebench.Event{Time: baseT.Add(900 * time.Millisecond), RunID: "bkl-a1-r2e", Job: "r2e", Worker: "r2e", Task: 0, Step: 1, Instance: "r2e-1", Image: r2eImg, Env: "bkl-a1-r2e-0", Event: swebench.EventTask, DurMS: 800, OK: true, Reward: &rewardOne},
		swebench.Event{Time: baseT.Add(950 * time.Millisecond), RunID: "bkl-a1-r2e", Job: "r2e", Worker: "r2e", Step: 1, Event: swebench.EventBarrier, DurMS: 850, OK: true, Count: 1, WorkingSetMisses: 1, Created: 1, Retired: 1},
		swebench.Event{
			Time: baseT.Add(1000 * time.Millisecond), RunID: "bkl-a1-r2e", Job: "r2e", Worker: "r2e",
			Event: swebench.EventRun, Name: "steps", DurMS: 1000, OK: true, Count: 1,
			WorkingSetMisses: 1, Created: 1, Retired: 1,
			Labels: map[string]string{"mode": "steps", "strategy": "none", "park": "delete", "width": "4", "images": "1", "rollouts": "1", "steps": "1", "client": "go", "arm": "go-none"},
			Tags: map[string]string{
				"dataset":                    "/var/run/ko/r2e-dataset.jsonl",
				"guest_image":                r2eImg,
				"load_tasks":                 "1",
				"load_group_size":            "1",
				"load_steps":                 "1",
				"load_tasks_per_step":        "1",
				"load_trajectories_per_step": "1",
				"load_max_concurrent":        "4",
				"load_distinct_images":       "1",
				"load_repeat":                "1",
			},
		},
	}
	sweEvents := []any{
		swebench.Event{Time: baseT.Add(120 * time.Millisecond), RunID: "bkl-a1-swe", Job: "swe", Worker: "swe", Task: 0, Step: 1, Instance: "swe-1", Image: sweImg, Env: "bkl-a1-swe-0", Event: swebench.EventCreate, DurMS: 60, OK: true},
		swebench.Event{Time: baseT.Add(340 * time.Millisecond), RunID: "bkl-a1-swe", Job: "swe", Worker: "swe", Task: 0, Step: 1, Instance: "swe-1", Image: sweImg, Env: "bkl-a1-swe-0", Event: swebench.EventTTFI, DurMS: 220, OK: true},
		swebench.Event{Time: baseT.Add(1100 * time.Millisecond), RunID: "bkl-a1-swe", Job: "swe", Worker: "swe", Task: 0, Step: 1, Instance: "swe-1", Image: sweImg, Env: "bkl-a1-swe-0", Event: swebench.EventTask, DurMS: 980, OK: true, Reward: &rewardOne},
		swebench.Event{Time: baseT.Add(1150 * time.Millisecond), RunID: "bkl-a1-swe", Job: "swe", Worker: "swe", Step: 1, Event: swebench.EventBarrier, DurMS: 1030, OK: true, Count: 1, WorkingSetMisses: 1, Created: 1, Retired: 1},
		swebench.Event{
			Time: baseT.Add(1200 * time.Millisecond), RunID: "bkl-a1-swe", Job: "swe", Worker: "swe",
			Event: swebench.EventRun, Name: "steps", DurMS: 1200, OK: true, Count: 1,
			WorkingSetMisses: 1, Created: 1, Retired: 1,
			Labels: map[string]string{"mode": "steps", "strategy": "none", "park": "delete", "width": "4", "images": "1", "rollouts": "1", "steps": "1", "client": "go", "arm": "go-none"},
			Tags: map[string]string{
				"dataset":                    "/var/run/ko/swe-dataset.jsonl",
				"guest_image":                sweImg,
				"load_tasks":                 "1",
				"load_group_size":            "1",
				"load_steps":                 "1",
				"load_tasks_per_step":        "1",
				"load_trajectories_per_step": "1",
				"load_max_concurrent":        "4",
				"load_distinct_images":       "1",
				"load_repeat":                "1",
			},
		},
	}
	writeJSONL(filepath.Join(dir, "events", "r2e.jsonl"), r2eEvents)
	writeJSONL(filepath.Join(dir, "events", "swe.jsonl"), sweEvents)
	writeJSONL(filepath.Join(dir, "events", "reap.jsonl"), []any{
		swebench.Event{
			Time:    baseT.Add(1500 * time.Millisecond),
			RunID:   "bkl-a1",
			Task:    0,
			Env:     "bkl-a1",
			Event:   swebench.EventReap,
			DurMS:   18.0,
			OK:      true,
			Metrics: map[string]float64{"orphan_count": 0, "deleted_count": 0},
		},
	})

	// Write raw atelet restore logs under atelet_restores.jsonl (no restore_timings.jsonl).
	ateletLines := []any{
		map[string]any{
			"msg":                              "Restore timing breakdown",
			"ate.actor.name":                   "bkl-a1-r2e-0",
			"time":                             baseT.Add(260 * time.Millisecond).Format(time.RFC3339Nano),
			"ate.snapshot.fidelity":            "MEMORY",
			"ate.actor.restore.duration.total": 0.145,
			"ate.actor.restore.duration.worker_restore": 0.110,
			"ate.actor.restore.duration.oci_unpack":     0.002,
		},
		map[string]any{
			"msg":                              "Restore timing breakdown",
			"ate.actor.name":                   "bkl-a1-swe-0",
			"time":                             baseT.Add(290 * time.Millisecond).Format(time.RFC3339Nano),
			"ate.snapshot.fidelity":            "MEMORY",
			"ate.actor.restore.duration.total": 0.160,
			"ate.actor.restore.duration.worker_restore": 0.120,
			"ate.actor.restore.duration.oci_unpack":     0.003,
		},
	}
	writeJSONL(filepath.Join(dir, "atelet_restores.jsonl"), ateletLines)

	// Node cache snapshots (warm hit delta = 2, misses = 0) + complete preflight-coverage.json.
	promBefore := "node-a\t# sampled 1793995200\nnode-a\tprocess_start_time_seconds 1793990000\nnode-a\tate_imagecache_requests_total{ate_imagecache_outcome=\"hit\"} 10\nnode-a\tate_imagecache_requests_total{ate_imagecache_outcome=\"miss\"} 0\n"
	promAfter := "node-a\t# sampled 1793995210\nnode-a\tprocess_start_time_seconds 1793990000\nnode-a\tate_imagecache_requests_total{ate_imagecache_outcome=\"hit\"} 12\nnode-a\tate_imagecache_requests_total{ate_imagecache_outcome=\"miss\"} 0\n"
	if err := os.WriteFile(filepath.Join(dir, "cache_before.prom"), []byte(promBefore), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "cache_after.prom"), []byte(promAfter), 0o644); err != nil {
		t.Fatal(err)
	}
	cov := schema.PreflightCoverage{
		SchemaVersion: schema.SchemaVersion,
		RunID:         "pf-1",
		StartedAt:     baseT.Add(-2 * time.Minute),
		FinishedAt:    baseT.Add(-1 * time.Minute),
		Complete:      true,
		Goldens: []schema.GoldenRecord{
			{Image: r2eImg, State: "ready", WaitMS: 100},
			{Image: sweImg, State: "ready", WaitMS: 100},
		},
		Domains: []schema.DomainCoverage{{
			Name:      "cos7-workerpool",
			Nodes:     1,
			NodesSeen: []string{"node-a"},
			Complete:  true,
			Images: []schema.ImageCoverage{
				{Image: r2eImg, Wanted: 1, Nodes: []string{"node-a"}, Probes: 1, Rounds: 1, Complete: true},
				{Image: sweImg, Wanted: 1, Nodes: []string{"node-a"}, Probes: 1, Rounds: 1, Complete: true},
			},
		}},
	}
	covBytes, _ := json.MarshalIndent(cov, "", "  ")
	if err := os.WriteFile(filepath.Join(dir, "preflight-coverage.json"), covBytes, 0o644); err != nil {
		t.Fatal(err)
	}

	// Periodic snapshot + final measurements.json + system_description.json.
	snapT := baseT.Add(500 * time.Millisecond)
	snap := schema.Measurements{
		SchemaVersion: schema.SchemaVersion,
		RunID:         "bkl-a1",
		CollectedAt:   snapT,
		StartTS:       &baseT,
		EndTS:         &snapT,
		ServerSummary: map[string]schema.MetricSummary{
			"active_sandboxes": {Count: 1, Sum: 2, Min: 2, P50: 2, P90: 2, P95: 2, P99: 2, Max: 2, Unit: "actors"},
		},
	}
	snapBytes, _ := json.MarshalIndent(snap, "", "  ")
	if err := os.WriteFile(filepath.Join(dir, "measurements_snapshots", "meas_20261106T200000Z_1.json"), snapBytes, 0o644); err != nil {
		t.Fatal(err)
	}
	endT := baseT.Add(2 * time.Second)
	meas := schema.Measurements{
		SchemaVersion: schema.SchemaVersion,
		RunID:         "bkl-a1",
		CollectedAt:   endT,
		StartTS:       &baseT,
		EndTS:         &endT,
		ServerSummary: map[string]schema.MetricSummary{
			"active_sandboxes": {Count: 1, Sum: 0, Min: 0, P50: 0, P90: 0, P95: 0, P99: 0, Max: 0, Unit: "actors"},
		},
	}
	measBytes, _ := json.MarshalIndent(meas, "", "  ")
	if err := os.WriteFile(filepath.Join(dir, "measurements.json"), measBytes, 0o644); err != nil {
		t.Fatal(err)
	}
	sysDesc := schema.SystemDescription{
		SchemaVersion:             schema.SchemaVersion,
		Cluster:                   "unit-cluster",
		Backend:                   "substrate",
		Access:                    "ate-env-api",
		KubernetesVersion:         "v1.32.2-gke.100",
		WorkerPodCount:            2,
		WorkerActorCapacity:       14,
		MaxConcurrentSandboxesCap: 28,
	}
	sysBytes, _ := json.MarshalIndent(sysDesc, "", "  ")
	if err := os.WriteFile(filepath.Join(dir, "system_description.json"), sysBytes, 0o644); err != nil {
		t.Fatal(err)
	}

	var outBuf, errBuf bytes.Buffer
	if err := reportCmd([]string{"-dest", dir}, &outBuf, &errBuf); err != nil {
		t.Fatalf("reportCmd failed: %v (stderr=%s)", err, errBuf.String())
	}

	rawRep, err := os.ReadFile(filepath.Join(dir, "report.json"))
	if err != nil {
		t.Fatal(err)
	}
	var parsed benchArtifactReport
	if err := json.Unmarshal(rawRep, &parsed); err != nil {
		t.Fatalf("unmarshal report.json: %v", err)
	}
	if parsed.Restores == nil || parsed.Restores.First.Restores != 2 || len(parsed.Restores.First.Phases) == 0 {
		t.Fatalf("expected atelet_restores.jsonl to populate rep.Restores.First, got %+v", parsed.Restores)
	}
	if parsed.Cache == nil || parsed.Cache.Hits != 2 || parsed.Cache.Misses != 0 {
		t.Fatalf("expected cache delta hits=2 misses=0, got %+v", parsed.Cache)
	}
	if parsed.Measurements == nil || parsed.Measurements.ServerSummary["active_sandboxes"].Max != 2 {
		t.Fatalf("expected measurements_snapshots rollup max=2, got %+v", parsed.Measurements)
	}
	if !parsed.ReapSeen || strings.HasSuffix(parsed.RunID, "-") {
		t.Fatalf("expected ReapSeen=true and clean RunID, got ReapSeen=%v RunID=%q", parsed.ReapSeen, parsed.RunID)
	}

	rawSysAfter, err := os.ReadFile(filepath.Join(dir, "system_description.json"))
	if err != nil {
		t.Fatal(err)
	}
	sysAfter, err := schema.DecodeJSONStrict[schema.SystemDescription](bytes.NewReader(rawSysAfter))
	if err != nil {
		t.Fatalf("decode enriched system_description.json: %v", err)
	}
	if sysAfter.DatasetRevision != "r2e-dataset.jsonl+swe-dataset.jsonl" {
		t.Fatalf("DatasetRevision = %q, want r2e-dataset.jsonl+swe-dataset.jsonl", sysAfter.DatasetRevision)
	}
	if sysAfter.Load == nil || sysAfter.Load.MaxConcurrent != 8 || sysAfter.Load.TrajectoriesPerStep != 2 || sysAfter.Load.DistinctImages != 2 {
		t.Fatalf("unexpected merged sysAfter.Load: %+v", sysAfter.Load)
	}

	untracedFolder := "2026-11-06_simulated-w8_r2e1-swe1_smoke-go-none"
	var valOut, valErr bytes.Buffer
	if err := validateCmd([]string{"-dest", dir, "-footing", "warm", "-folder", untracedFolder}, &valOut, &valErr); err != nil {
		t.Fatalf("validateCmd failed: %v\nstdout=%s\nstderr=%s", err, valOut.String(), valErr.String())
	}

	origRunner := uploadRunner
	fakeUp := &recordingGCSUploadRunner{}
	uploadRunner = fakeUp
	t.Cleanup(func() { uploadRunner = origRunner })

	var upOut, upErr bytes.Buffer
	if err := uploadCmd([]string{"-dest", dir, "-bucket", "gs://unit-test-bucket/", "-folder", untracedFolder, "-footing", "warm", "-dry-run"}, &upOut, &upErr); err != nil {
		t.Fatalf("uploadCmd -dry-run failed: %v", err)
	}
	if len(fakeUp.calls) != 0 {
		t.Fatalf("expected 0 upload calls on -dry-run, got %d", len(fakeUp.calls))
	}
	if err := uploadCmd([]string{"-dest", dir, "-bucket", "gs://unit-test-bucket/", "-folder", untracedFolder, "-footing", "warm"}, &upOut, &upErr); err != nil {
		t.Fatalf("uploadCmd failed: %v", err)
	}
	if len(fakeUp.calls) != 1 || fakeUp.calls[0][1] != "gs://unit-test-bucket/"+untracedFolder+"/" {
		t.Fatalf("unexpected upload calls: %+v", fakeUp.calls)
	}

	// Add a v1 TraceEvent under traces/swe.jsonl and verify _traced folder enforcement.
	if err := os.MkdirAll(filepath.Join(dir, "traces"), 0o755); err != nil {
		t.Fatal(err)
	}
	traceEv := schema.TraceEvent{
		SchemaVersion: schema.SchemaVersion,
		TS:            baseT.Add(150 * time.Millisecond),
		Op:            "exec",
		DurMS:         12.5,
		OK:            true,
		Run:           "bkl-a1-swe",
		Job:           "swe",
		Env:           "bkl-a1-swe-0",
		Instance:      "swe-1",
		Image:         sweImg,
	}
	writeJSONL(filepath.Join(dir, "traces", "swe.jsonl"), []any{traceEv})
	if err := reportCmd([]string{"-dest", dir}, &outBuf, &errBuf); err != nil {
		t.Fatalf("reportCmd with traces failed: %v", err)
	}
	if err := uploadCmd([]string{"-dest", dir, "-bucket", "unit-test-bucket", "-folder", untracedFolder, "-dry-run"}, &upOut, &upErr); err == nil || !strings.Contains(err.Error(), "_traced") {
		t.Fatalf("expected untraced folder rejection when traces exist, got %v", err)
	}
	tracedFolder := untracedFolder + "_traced"
	if err := uploadCmd([]string{"-dest", dir, "-bucket", "unit-test-bucket", "-folder", tracedFolder, "-footing", "warm"}, &upOut, &upErr); err != nil {
		t.Fatalf("uploadCmd with _traced folder failed: %v", err)
	}
	if len(fakeUp.calls) != 2 || fakeUp.calls[1][1] != "gs://unit-test-bucket/"+tracedFolder+"/" {
		t.Fatalf("unexpected traced upload calls: %+v", fakeUp.calls)
	}
}
