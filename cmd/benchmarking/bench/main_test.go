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
	"crypto/tls"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/agent-substrate/env/internal/apiservice"
	"github.com/agent-substrate/env/internal/ate"
	"github.com/agent-substrate/env/internal/benchmarking/measure"
	"github.com/agent-substrate/env/internal/benchmarking/schema"
	"github.com/agent-substrate/env/internal/internaltest/fakecontrol"
	"github.com/agent-substrate/env/internal/internaltest/fakerouter"
	ateenvpb "github.com/agent-substrate/env/proto/ateenv/v1alpha"
	ateapipb "go.ate.dev/substrate/api/ateapi/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
)

func startFakeAPIAndControl(t *testing.T) (apiAddr, ctrlAddr string) {
	t.Helper()

	ctrl := fakecontrol.New()
	ctrl.AddTemplate(&ateapipb.ActorTemplate{
		Metadata: &ateapipb.ResourceMetadata{Atespace: "ate-env", Name: "default-template"},
		Containers: []*ateapipb.Container{{
			Name:    "guest",
			Image:   "example.com/ate-env-guest@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
			Command: []string{"/ko-app/ate-env-guest"},
		}},
	})
	cAddr, stopCtrl, err := ctrl.Serve()
	if err != nil {
		t.Fatalf("fakecontrol.Serve: %v", err)
	}
	t.Cleanup(stopCtrl)

	rtr := fakerouter.New()
	rtr.Running = func(id string) bool {
		return ctrl.Status(id) == ateapipb.ActorState_ACTOR_STATE_RUNNING
	}
	rtrAddr, stopRtr := rtr.Serve()
	t.Cleanup(stopRtr)

	ateClient, err := ate.New(ate.Options{
		ControlAddr: cAddr,
		RouterAddr:  rtrAddr,
		SkipVerify:  true,
	})
	if err != nil {
		t.Fatalf("ate.New: %v", err)
	}
	t.Cleanup(func() { ateClient.Close() })

	srv := apiservice.New(ateClient, rtrAddr, "")
	t.Cleanup(srv.Close)
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("net.Listen: %v", err)
	}
	grpcSrv := grpc.NewServer()
	ateenvpb.RegisterEnvironmentServiceServer(grpcSrv, srv)
	go func() { _ = grpcSrv.Serve(lis) }()
	t.Cleanup(grpcSrv.Stop)

	return lis.Addr().String(), cAddr
}

func TestBenchComposableSubcommandsEndToEnd(t *testing.T) {
	dir := t.TempDir()
	artDir := filepath.Join(dir, "artifacts")

	// 1. Write smoke dataset JSONL.
	dsPath := filepath.Join(dir, "dataset.jsonl")
	dsContent := `{"instance_id":"astropy__astropy-12907","image":"us-docker.pkg.dev/proj/swe/astropy@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef","patch":"diff","eval_script":"pytest","fail_to_pass":["t1"],"log_parser":"parse_log_pytest"}` + "\n"
	if err := os.WriteFile(dsPath, []byte(dsContent), 0o644); err != nil {
		t.Fatal(err)
	}

	// 2. Write Scenario YAML.
	scenPath := filepath.Join(dir, "scenario.yaml")
	scenYAML := `schema_version: v1
name: bench-pr6-smoke
sut:
  backend: substrate
  access: ate-env-api
  endpoint: localhost:7777
  substrate:
    atespace: ate-env
    base_template: default-template
  replicas: 2
jobs:
  - name: sim-eval
    kind: simulated
    loader_mode: steps
    dataset: ` + dsPath + `
    trajectory: benchmark/traj.yaml
    park: suspend-async
    images: 1
    tasks_per_step: 1
    group_size: 2
    steps: 2
    max_concurrent: 2
    prefetch: true
    seed: 42
    schedule_mode: sampled
`
	if err := os.WriteFile(scenPath, []byte(scenYAML), 0o644); err != nil {
		t.Fatal(err)
	}

	// 3. Test `bench resolve`.
	var out, errBuf bytes.Buffer
	if code := execute([]string{"resolve", "-scenario", scenPath, "-dest", artDir}, &out, &errBuf); code != 0 {
		t.Fatalf("resolve failed (code=%d): %s", code, errBuf.String())
	}
	schedBytes, err := os.ReadFile(filepath.Join(artDir, "schedule.json"))
	if err != nil {
		t.Fatalf("missing schedule.json: %v", err)
	}
	if err := schema.ValidateAgainstJSONSchema("schedule.schema.json", schedBytes); err != nil {
		t.Fatalf("schedule.json failed schema validation: %v", err)
	}

	// 4. Test `bench render`.
	out.Reset()
	errBuf.Reset()
	if code := execute([]string{"render", "-scenario", scenPath, "-dest", artDir}, &out, &errBuf); code != 0 {
		t.Fatalf("render failed (code=%d): %s", code, errBuf.String())
	}
	if _, err := os.Stat(filepath.Join(artDir, "sut-manifest.yaml")); err != nil {
		t.Fatalf("missing sut-manifest.yaml: %v", err)
	}
	sysBytes, err := os.ReadFile(filepath.Join(artDir, "system_description.json"))
	if err != nil {
		t.Fatalf("missing system_description.json: %v", err)
	}
	if err := schema.ValidateAgainstJSONSchema("system_description.schema.json", sysBytes); err != nil {
		t.Fatalf("system_description.json failed schema validation: %v", err)
	}

	// 5. Test `bench prewarm` against fake ate-env-api + fakecontrol.
	apiAddr, ctrlAddr := startFakeAPIAndControl(t)
	out.Reset()
	errBuf.Reset()
	if code := execute([]string{"prewarm", "-endpoint", apiAddr, "-atespace", "ate-env", "-dataset", dsPath, "-images", "1", "-dest", artDir}, &out, &errBuf); code != 0 {
		t.Fatalf("prewarm failed (code=%d): %s", code, errBuf.String())
	}
	if _, err := os.Stat(filepath.Join(artDir, "events", "prewarm.jsonl")); err != nil {
		t.Fatalf("missing events/prewarm.jsonl: %v", err)
	}

	// 6. Test `bench measure` (both valid with 0-value metric and failed-scrape rejection).
	promMux := http.NewServeMux()
	promMux.HandleFunc("/api/v1/query", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query().Get("query")
		switch q {
		case "zero_oom":
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"status":"success","data":{"resultType":"vector","result":[{"metric":{"node":"n1"},"value":[1700000000,"0"]}]}}`))
		case "missing_series":
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"status":"success","data":{"resultType":"vector","result":[]}}`))
		default:
			w.WriteHeader(http.StatusInternalServerError)
		}
	})
	promSrv := httptest.NewServer(promMux)
	t.Cleanup(promSrv.Close)

	if err := measure.WriteNodeCacheSamples(filepath.Join(artDir, "cache_before.json"), []schema.NodeCacheSample{{Node: "n1", Hits: 2, Misses: 1}}); err != nil {
		t.Fatal(err)
	}
	if err := measure.WriteNodeCacheSamples(filepath.Join(artDir, "cache_after.json"), []schema.NodeCacheSample{{Node: "n1", Hits: 10, Misses: 3}}); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(artDir, "restore_timings.jsonl"), []byte(`{"env":"e1","total_ms":25.0,"cache_hit":true}`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Failed-scrape / missing required metric must fail with non-zero exit code.
	out.Reset()
	errBuf.Reset()
	if code := execute([]string{"measure", "-run-id", "run-pr6", "-prom-url", promSrv.URL, "-query", "missing=missing_series", "-dest", artDir}, &out, &errBuf); code == 0 {
		t.Fatal("expected bench measure to fail when required query returns empty series")
	}

	// Valid scrape with measured 0 must succeed and validate against measurements.schema.json.
	out.Reset()
	errBuf.Reset()
	if code := execute([]string{"measure", "-run-id", "run-pr6", "-prom-url", promSrv.URL, "-query", "oom_kills=zero_oom", "-min-restores", "1", "-dest", artDir}, &out, &errBuf); code != 0 {
		t.Fatalf("measure failed (code=%d): %s", code, errBuf.String())
	}
	measBytes, err := os.ReadFile(filepath.Join(artDir, "measurements.json"))
	if err != nil {
		t.Fatalf("missing measurements.json: %v", err)
	}
	if err := schema.ValidateAgainstJSONSchema("measurements.schema.json", measBytes); err != nil {
		t.Fatalf("measurements.json failed schema validation: %v", err)
	}

	// 7. Seed orphan actors in fakecontrol and test `bench reap`.
	conn, err := grpc.NewClient(ctrlAddr, grpc.WithTransportCredentials(credentials.NewTLS(&tls.Config{InsecureSkipVerify: true})))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	ctrlClient := ateapipb.NewControlClient(conn)
	for _, id := range []string{"orphan-run-1", "orphan-run-2"} {
		if _, err := ctrlClient.CreateActor(context.Background(), &ateapipb.CreateActorRequest{
			Actor: &ateapipb.Actor{Metadata: &ateapipb.ResourceMetadata{Atespace: "ate-env", Name: id}},
		}); err != nil {
			t.Fatal(err)
		}
	}

	out.Reset()
	errBuf.Reset()
	if code := execute([]string{"reap", "-ate-endpoint", ctrlAddr, "-atespace", "ate-env", "-prefix", "orphan-run-", "-poll-interval", "10ms", "-dest", artDir}, &out, &errBuf); code != 0 {
		t.Fatalf("reap failed (code=%d): %s", code, errBuf.String())
	}
	if !strings.Contains(out.String(), "reaped 2 environments (remaining=0") {
		t.Fatalf("unexpected reap output: %s", out.String())
	}
	if _, err := os.Stat(filepath.Join(artDir, "events", "reap.jsonl")); err != nil {
		t.Fatalf("missing events/reap.jsonl: %v", err)
	}

	// 8. Test `bench report -report-only -dest DIR` attaches measurements & system_description.
	workerEvents := strings.Join([]string{
		`{"ts":"2026-10-22T01:00:00Z","run":"run-pr6","task":0,"instance":"astropy__astropy-12907","image":"img","env":"e0","step":1,"event":"create","dur_ms":40,"ok":true}`,
		`{"ts":"2026-10-22T01:00:01Z","run":"run-pr6","task":0,"instance":"astropy__astropy-12907","image":"img","env":"e0","step":1,"event":"step","name":"test","dur_ms":100,"exit":0,"ok":true}`,
		`{"ts":"2026-10-22T01:00:02Z","run":"run-pr6","task":0,"instance":"astropy__astropy-12907","image":"img","env":"e0","step":1,"event":"task","dur_ms":140,"ok":true}`,
	}, "\n") + "\n"
	if err := os.WriteFile(filepath.Join(artDir, "events", "worker-0.jsonl"), []byte(workerEvents), 0o644); err != nil {
		t.Fatal(err)
	}

	out.Reset()
	errBuf.Reset()
	if code := execute([]string{"report", "-report-only", "-dest", artDir}, &out, &errBuf); code != 0 {
		t.Fatalf("report -report-only failed (code=%d): %s", code, errBuf.String())
	}
	repBytes, err := os.ReadFile(filepath.Join(artDir, "report.json"))
	if err != nil {
		t.Fatalf("missing report.json: %v", err)
	}
	var parsedRep map[string]any
	if err := json.Unmarshal(repBytes, &parsedRep); err != nil {
		t.Fatalf("unmarshal report.json: %v", err)
	}
	if parsedRep["measurements"] == nil || parsedRep["system_description"] == nil {
		t.Fatalf("expected report.json to include measurements and system_description, got: %s", string(repBytes))
	}
}

func TestBenchRunMultiJobScenario(t *testing.T) {
	dir := t.TempDir()
	scenPath := filepath.Join(dir, "multi-job.yaml")
	scenYAML := `schema_version: v1
name: multi-job-smoke
footing: warm
seed: 42
sut:
  backend: substrate
  access: ate-env-api
  endpoint: localhost:7777
jobs:
  - name: job-closed
    kind: simulated
    mode: closed-loop
    steps: 2
    batch_size: 2
    concurrency: 2
    park_policy: snapshot
    instances: 2
  - name: job-open
    kind: simulated
    mode: open-loop
    start_offset_ms: 5
    concurrency: 2
    arrival_rate: 40.0
    duration_sec: 0.05
    instances: 2
`
	if err := os.WriteFile(scenPath, []byte(scenYAML), 0o644); err != nil {
		t.Fatal(err)
	}
	runDir := filepath.Join(dir, "run-out")
	var out, errBuf bytes.Buffer
	if code := execute([]string{"run", "-scenario", scenPath, "-dest", runDir}, &out, &errBuf); code != 0 {
		t.Fatalf("bench run failed (code=%d): %s", code, errBuf.String())
	}
	for _, rel := range []string{"schedule.json", "events.jsonl", "report.json", "measurements.json", "system_description.json"} {
		if _, err := os.Stat(filepath.Join(runDir, rel)); err != nil {
			t.Fatalf("missing artifact %s: %v", rel, err)
		}
	}
	out.Reset()
	errBuf.Reset()
	if code := execute([]string{"validate", "-report", filepath.Join(runDir, "report.json"), "-schedule", filepath.Join(runDir, "schedule.json"), "-footing", "warm"}, &out, &errBuf); code != 0 {
		t.Fatalf("bench validate failed (code=%d): %s", code, errBuf.String())
	}

	// Test A/B overlay comparison (`bench run -overlay pool-off,pool-on`)
	abDir := filepath.Join(dir, "ab-out")
	out.Reset()
	errBuf.Reset()
	if code := execute([]string{"run", "-scenario", scenPath, "-overlay", "pool-off,pool-on", "-dest", abDir}, &out, &errBuf); code != 0 {
		t.Fatalf("bench run -overlay pool-off,pool-on failed (code=%d): %s", code, errBuf.String())
	}
	for _, rel := range []string{
		"pool-off/schedule.json",
		"pool-off/events.jsonl",
		"pool-off/report.json",
		"pool-on/schedule.json",
		"pool-on/events.jsonl",
		"pool-on/report.json",
		"comparison.json",
		"comparison.md",
	} {
		if _, err := os.Stat(filepath.Join(abDir, rel)); err != nil {
			t.Fatalf("missing overlay artifact %s: %v", rel, err)
		}
	}
	if !strings.Contains(out.String(), "Overlay Comparison") {
		t.Fatalf("expected Overlay Comparison in stdout, got:\n%s", out.String())
	}

	// Test scoped reporting (`-scopes user-sandbox`) and `-plugin-metric` on `bench report`
	scopedDir := filepath.Join(dir, "scoped-report")
	if err := os.MkdirAll(scopedDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(scopedDir, "verl-head.log"), []byte("(TaskRunner pid=1) step:1 - timing_s/step:12.0 - timing_s/gen:8.0 - critic/score/mean:0.5\ncustom_debug_latency_ms=42.5\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(scopedDir, "trace.jsonl"), []byte(`{"ts":"2026-04-20T18:00:00Z","run":"rlb-s","job":"verl-job","op":"create","env":"sb-1","instance":"inst-1","dur_ms":110,"ok":true}`+"\n"+`{"ts":"2026-04-20T18:00:01Z","run":"rlb-s","job":"verl-job","op":"exec","env":"sb-1","instance":"inst-1","dur_ms":210,"ok":true,"exit":0}`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	errBuf.Reset()
	if code := execute([]string{
		"report",
		"-dest", scopedDir,
		"-scopes", "user-sandbox",
		"-plugin-metric", "custom_debug_latency_ms=verl-head.log:custom_debug_latency_ms=(?P<value>[0-9.]+)",
	}, &out, &errBuf); code != 0 {
		t.Fatalf("bench report -scopes user-sandbox failed (code=%d): %s", code, errBuf.String())
	}
	mdBytes, err := os.ReadFile(filepath.Join(scopedDir, "report.md"))
	if err != nil {
		t.Fatal(err)
	}
	mdStr := string(mdBytes)
	if !strings.Contains(mdStr, "## 1. Published Baseline") || !strings.Contains(mdStr, "custom_debug_latency_ms") {
		t.Fatalf("expected Published Baseline and custom_debug_latency_ms in report.md, got:\n%s", mdStr)
	}
	if strings.Contains(mdStr, "## 4. Convergence Info") {
		t.Fatalf("expected RL Convergence Info section to be omitted when -scopes=user-sandbox, got:\n%s", mdStr)
	}
}

type fakeLifecycleKubeRunner struct {
	replicas int
}

func (f *fakeLifecycleKubeRunner) Run(_ context.Context, args ...string) ([]byte, error) {
	joined := strings.Join(args, " ")
	switch {
	case strings.Contains(joined, "patch workerpool") && strings.Contains(joined, `"replicas":2`):
		f.replicas = 2
		return []byte("workerpool.ate.dev/default-template-workerpool patched\n"), nil
	case strings.Contains(joined, "patch workerpool") && strings.Contains(joined, `"replicas":0`):
		f.replicas = 0
		return []byte("workerpool.ate.dev/default-template-workerpool patched\n"), nil
	case strings.Contains(joined, "rollout status"):
		return []byte("deployment \"ate-env-api\" successfully rolled out\n"), nil
	case strings.Contains(joined, "get pods -o json"):
		if f.replicas == 0 {
			return []byte(`{"items":[]}`), nil
		}
		return []byte(`{"items":[
			{"metadata":{"name":"wp-0","namespace":"ate-env","creationTimestamp":"2026-10-27T00:00:00Z","labels":{"ate-env/workerpool":"default-template-workerpool"}},"spec":{"containers":[{"name":"ateworker"}]},"status":{"phase":"Running","conditions":[
				{"type":"PodScheduled","status":"True","lastTransitionTime":"2026-10-27T00:00:01Z"},
				{"type":"Initialized","status":"True","lastTransitionTime":"2026-10-27T00:00:02Z"},
				{"type":"ContainersReady","status":"True","lastTransitionTime":"2026-10-27T00:00:05Z"},
				{"type":"Ready","status":"True","lastTransitionTime":"2026-10-27T00:00:05Z"}
			]}},
			{"metadata":{"name":"wp-1","namespace":"ate-env","creationTimestamp":"2026-10-27T00:00:00Z","labels":{"ate-env/workerpool":"default-template-workerpool"}},"spec":{"containers":[{"name":"ateworker"}]},"status":{"phase":"Running","conditions":[
				{"type":"PodScheduled","status":"True","lastTransitionTime":"2026-10-27T00:00:01Z"},
				{"type":"Initialized","status":"True","lastTransitionTime":"2026-10-27T00:00:02Z"},
				{"type":"ContainersReady","status":"True","lastTransitionTime":"2026-10-27T00:00:06Z"},
				{"type":"Ready","status":"True","lastTransitionTime":"2026-10-27T00:00:06Z"}
			]}}
		]}`), nil
	default:
		return []byte(`{}`), nil
	}
}

func TestBenchSpinUpAndSpinDownWorkerPool(t *testing.T) {
	fakeKube := &fakeLifecycleKubeRunner{}
	prev := lifecycleKubeRunner
	lifecycleKubeRunner = fakeKube
	t.Cleanup(func() { lifecycleKubeRunner = prev })

	dir := t.TempDir()
	var out, errBuf bytes.Buffer
	if code := execute([]string{"spinup", "-atespace", "ate-env", "-workerpool", "default-template-workerpool", "-replicas", "2", "-run-id", "wp-test", "-dest", dir}, &out, &errBuf); code != 0 {
		t.Fatalf("bench spinup failed (code=%d): %s", code, errBuf.String())
	}
	if !strings.Contains(out.String(), "spun up workerpool/default-template-workerpool in ate-env (replicas=2") {
		t.Fatalf("unexpected spinup output: %s", out.String())
	}

	out.Reset()
	errBuf.Reset()
	if code := execute([]string{"spindown", "-atespace", "ate-env", "-workerpool", "default-template-workerpool", "-run-id", "wp-test", "-dest", dir}, &out, &errBuf); code != 0 {
		t.Fatalf("bench spindown failed (code=%d): %s", code, errBuf.String())
	}
	if !strings.Contains(out.String(), "spun down workerpool/default-template-workerpool in ate-env") {
		t.Fatalf("unexpected spindown output: %s", out.String())
	}

	rawMeas, err := os.ReadFile(filepath.Join(dir, "measurements.json"))
	if err != nil {
		t.Fatalf("missing measurements.json: %v", err)
	}
	meas, err := schema.DecodeJSONStrict[schema.Measurements](bytes.NewReader(rawMeas))
	if err != nil {
		t.Fatalf("invalid measurements.json: %v", err)
	}
	for _, k := range []string{
		"workerpool_spinup_wall_ms",
		"worker_pod_schedule_ms",
		"worker_pod_init_ms",
		"worker_pod_container_start_ms",
		"worker_pod_total_ready_ms",
		"workerpool_spindown_wall_ms",
		"worker_pod_terminate_ms",
	} {
		if _, ok := meas.ServerSummary[k]; !ok {
			t.Fatalf("expected %s in measurements.json server_summary, got: %v", k, meas.ServerSummary)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "events", "substrate_lifecycle.jsonl")); err != nil {
		t.Fatalf("missing events/substrate_lifecycle.jsonl: %v", err)
	}
}

type fakeUploadRunner struct {
	calls [][2]string
}

func (f *fakeUploadRunner) UploadDir(_ context.Context, srcDir, destGCSURI string) ([]byte, error) {
	f.calls = append(f.calls, [2]string{srcDir, destGCSURI})
	return []byte("uploaded\n"), nil
}

func TestBenchValidateBundleAndUpload(t *testing.T) {
	dir := t.TempDir()
	bundleDir := filepath.Join(dir, "2026-10-28_qwen3.5-35b_r2e-gym-easy_prewarmed")
	if err := os.MkdirAll(filepath.Join(bundleDir, "overlay"), 0o755); err != nil {
		t.Fatal(err)
	}

	// Seed raw trace, verl log, K8s manifests, node cache snapshots, and restore timings for a Mode 2 warm cluster RL run.
	if err := os.WriteFile(filepath.Join(bundleDir, "verl-head.log"), []byte("(TaskRunner pid=1) step:1 - timing_s/step:12.0 - timing_s/gen:8.0 - critic/score/mean:0.5\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bundleDir, "configmap.yaml"), []byte("apiVersion: v1\nkind: ConfigMap\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bundleDir, "job.yaml"), []byte("apiVersion: batch/v1\nkind: Job\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := measure.WriteNodeCacheSamples(filepath.Join(bundleDir, "cache_before.json"), []schema.NodeCacheSample{{Node: "n1", Hits: 2, Misses: 1}}); err != nil {
		t.Fatal(err)
	}
	if err := measure.WriteNodeCacheSamples(filepath.Join(bundleDir, "cache_after.json"), []schema.NodeCacheSample{{Node: "n1", Hits: 10, Misses: 1}}); err != nil {
		t.Fatal(err)
	}
	traceContent := strings.Join([]string{
		`{"ts":"2026-10-28T18:00:00Z","run":"rlb-m2","job":"verl-job","op":"wake","env":"sb-1","instance":"inst-1","dur_ms":245,"ok":true}`,
		`{"ts":"2026-10-28T18:00:01Z","run":"rlb-m2","job":"verl-job","op":"exec","env":"sb-1","instance":"inst-1","dur_ms":210,"ok":true,"exit":0}`,
		`{"ts":"2026-10-28T18:00:02Z","run":"rlb-m2","job":"verl-job","op":"suspend","env":"sb-1","instance":"inst-1","dur_ms":650,"ok":true}`,
	}, "\n") + "\n"
	if err := os.WriteFile(filepath.Join(bundleDir, "trace.jsonl"), []byte(traceContent), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bundleDir, "restore_timings.jsonl"), []byte(`{"env":"sb-1","total_ms":245.0,"cache_hit":true}`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Seed overlay files + overlay.json provenance manifest.
	if err := os.WriteFile(filepath.Join(bundleDir, "overlay", "README.md"), []byte("# Mode 2 Overlay\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bundleDir, "overlay", "mode0_to_mode2.patch"), []byte("--- a/run.sh\n+++ b/run.sh\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	overlayManifest := `{
  "schema_version": "v1",
  "name": "mode2-prewarm-suspend-restore",
  "hash": "sha256:1111111111111111111111111111111111111111111111111111111111111111",
  "base_recipe_path": "examples/Qwen3.5-35B-A3B-verl",
  "base_recipe_hash": "sha256:2222222222222222222222222222222222222222222222222222222222222222",
  "base_recipe_commit": "2eccd89617ff704bb74b9ca356a24548987dd380",
  "patch_file": "overlay/mode0_to_mode2.patch"
}`
	if err := os.WriteFile(filepath.Join(bundleDir, "overlay", "overlay.json"), []byte(overlayManifest), 0o644); err != nil {
		t.Fatal(err)
	}

	// Seed measurements.json and system_description.json.
	meas := schema.Measurements{
		SchemaVersion: schema.SchemaVersion,
		RunID:         "rlb-m2",
		CollectedAt:   time.Date(2026, 10, 28, 18, 5, 0, 0, time.UTC),
		ServerSummary: map[string]schema.MetricSummary{"sandbox_restore_ms": {Count: 1, Sum: 245, Min: 245, Max: 245, P50: 245, P90: 245, P95: 245, P99: 245, Unit: "ms"}},
	}
	measBytes, err := json.MarshalIndent(meas, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bundleDir, "measurements.json"), measBytes, 0o644); err != nil {
		t.Fatal(err)
	}
	sysDesc := schema.SystemDescription{
		SchemaVersion:             schema.SchemaVersion,
		Cluster:                   "rl-bench-cluster",
		Backend:                   "substrate",
		Access:                    "ate-env-api",
		MaxConcurrentSandboxesCap: 128,
		Load: &schema.LoadParams{
			MaxConcurrent:       128,
			TrajectoriesPerStep: 128,
		},
		LifecyclePolicy: &schema.SandboxLifecyclePolicy{
			Mode:           "prewarmed",
			ParkMode:       "suspend",
			PrewarmAtStart: true,
			PrewarmCount:   64,
		},
	}
	sysBytes, err := json.MarshalIndent(sysDesc, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bundleDir, "system_description.json"), sysBytes, 0o644); err != nil {
		t.Fatal(err)
	}

	// Run `bench report -dest` to generate report.json, report.md, run_window.json, and ingest overlay/overlay.json into system_description.json.
	var out, errBuf bytes.Buffer
	if code := execute([]string{"report", "-dest", bundleDir}, &out, &errBuf); code != 0 {
		t.Fatalf("bench report failed (code=%d): %s", code, errBuf.String())
	}

	// Validate the bundle with `bench validate -dest`.
	out.Reset()
	errBuf.Reset()
	if code := execute([]string{"validate", "-dest", bundleDir}, &out, &errBuf); code != 0 {
		t.Fatalf("bench validate -dest failed (code=%d): %s\nstdout: %s", code, errBuf.String(), out.String())
	}
	if !strings.Contains(out.String(), `"valid": true`) {
		t.Fatalf("expected '\"valid\": true' in stdout, got: %s", out.String())
	}

	// Verify that missing base_recipe_hash in overlay/overlay.json causes validation to fail.
	badManifest := strings.ReplaceAll(overlayManifest, `"base_recipe_hash": "sha256:2222222222222222222222222222222222222222222222222222222222222222",`, `"base_recipe_hash": "",`)
	if err := os.WriteFile(filepath.Join(bundleDir, "overlay", "overlay.json"), []byte(badManifest), 0o644); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	errBuf.Reset()
	if code := execute([]string{"validate", "-dest", bundleDir}, &out, &errBuf); code == 0 {
		t.Fatal("expected bench validate -dest to fail when overlay/overlay.json is missing base_recipe_hash")
	}
	// Restore valid overlay.json.
	if err := os.WriteFile(filepath.Join(bundleDir, "overlay", "overlay.json"), []byte(overlayManifest), 0o644); err != nil {
		t.Fatal(err)
	}

	// Test `bench upload`: must fail when -bucket is omitted (no hardcoded bucket).
	out.Reset()
	errBuf.Reset()
	if code := execute([]string{"upload", "-dest", bundleDir}, &out, &errBuf); code == 0 {
		t.Fatal("expected bench upload without -bucket to fail")
	}

	// Test `bench upload` with invalid folder name fails.
	out.Reset()
	errBuf.Reset()
	if code := execute([]string{"upload", "-dest", bundleDir, "-bucket", "gs://example-bench-bucket", "-folder", "BadFolderName"}, &out, &errBuf); code == 0 {
		t.Fatal("expected bench upload with invalid -folder name to fail")
	}

	// Test `bench upload` with valid -bucket and mock runner succeeds.
	fakeUp := &fakeUploadRunner{}
	prevUp := uploadRunner
	uploadRunner = fakeUp
	t.Cleanup(func() { uploadRunner = prevUp })

	out.Reset()
	errBuf.Reset()
	if code := execute([]string{"upload", "-dest", bundleDir, "-bucket", "gs://example-bench-bucket"}, &out, &errBuf); code != 0 {
		t.Fatalf("bench upload failed (code=%d): %s", code, errBuf.String())
	}
	if len(fakeUp.calls) != 1 {
		t.Fatalf("expected 1 UploadDir call, got %d", len(fakeUp.calls))
	}
	wantTarget := "gs://example-bench-bucket/2026-10-28_qwen3.5-35b_r2e-gym-easy_prewarmed/"
	if fakeUp.calls[0][1] != wantTarget {
		t.Fatalf("expected upload target %q, got %q", wantTarget, fakeUp.calls[0][1])
	}
}
