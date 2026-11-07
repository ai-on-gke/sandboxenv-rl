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

package job

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/agent-substrate/env/internal/benchmarking/schema"
	"github.com/agent-substrate/env/internal/benchmarking/swebench"
)

// clusterFakeExecutor answers kubectl calls per pod so trace collection can
// be exercised without a cluster. It implements StdoutExecutor so tar bytes
// are returned unmixed with stderr.
type clusterFakeExecutor struct {
	calls     []string
	logs      string
	pods      []string
	podTraces map[string]map[string]string // pod -> relative path -> content
	// jobFailed makes `get job` report Failed instead of Complete.
	jobFailed bool
	// trainerExit is the trainer container's exit code when a pod listing in
	// JSON is requested (sidecar mode); trainerRunningPolls delays the exit
	// by that many polls so the wait loop is exercised.
	trainerExit         int
	trainerRunningPolls int
	podJSONPolls        int
	// podFailedNoExit makes every pod report phase Failed with the trainer
	// container still `running` (evicted before its status landed).
	podFailedNoExit bool
	// trainerless names pods in the JSON listing that do not run the
	// trainer container at all (Ray workers under a shared selector).
	trainerless map[string]bool
	// tarFails names pods whose tar exits 1 (a live writer changed a file).
	tarFails map[string]bool
}

func TestMain(m *testing.M) {
	jobPollInterval = time.Millisecond
	jobWaitTimeout = 10 * time.Second
	os.Exit(m.Run())
}

func (f *clusterFakeExecutor) Run(ctx context.Context, name string, env map[string]string, args ...string) ([]byte, error) {
	f.calls = append(f.calls, name+" "+strings.Join(args, " "))
	joined := strings.Join(args, " ")
	switch {
	case strings.Contains(joined, " logs "):
		return []byte(f.logs), nil
	default:
		return nil, nil
	}
}

func (f *clusterFakeExecutor) podJSON() []byte {
	f.podJSONPolls++
	var items []string
	for _, p := range f.pods {
		if f.trainerless[p] {
			items = append(items, fmt.Sprintf(`{"metadata":{"name":%q},"status":{"phase":"Running","containerStatuses":[{"name":"ray-worker","state":{"running":{}}}]}}`, p))
			continue
		}
		phase := "Running"
		state := `{"running":{}}`
		switch {
		case f.podFailedNoExit:
			phase = "Failed"
		case f.podJSONPolls > f.trainerRunningPolls:
			state = fmt.Sprintf(`{"terminated":{"exitCode":%d,"reason":"Completed"}}`, f.trainerExit)
		}
		items = append(items, fmt.Sprintf(`{"metadata":{"name":%q},"status":{"phase":%q,"containerStatuses":[{"name":"workload","state":%s},{"name":"keepalive","state":{"running":{}}}]}}`, p, phase, state))
	}
	return []byte(`{"items":[` + strings.Join(items, ",") + `]}`)
}

func (f *clusterFakeExecutor) Output(ctx context.Context, name string, env map[string]string, args ...string) ([]byte, error) {
	f.calls = append(f.calls, name+" "+strings.Join(args, " "))
	joined := strings.Join(args, " ")
	switch {
	case strings.Contains(joined, "get job"):
		if f.jobFailed {
			return []byte(`{"status":{"conditions":[{"type":"Failed","status":"True","reason":"BackoffLimitExceeded","message":"Job has reached the specified backoff limit"}]}}`), nil
		}
		return []byte(completeJobJSON), nil
	case strings.Contains(joined, "get pods") && args[len(args)-1] == "json":
		return f.podJSON(), nil
	case strings.Contains(joined, "get pods"):
		return []byte(strings.Join(f.pods, " ")), nil
	case strings.Contains(joined, " exec "):
		pod := args[3]
		if f.tarFails[pod] {
			return nil, errors.New("tar: ./trace.jsonl: file changed as we read it\ncommand terminated with exit code 1: exit status 1")
		}
		files, ok := f.podTraces[pod]
		if !ok {
			return nil, errors.New("command terminated with exit code 42: exit status 42")
		}
		var buf bytes.Buffer
		tw := tar.NewWriter(&buf)
		// A directory entry and a non-jsonl file must be ignored by the extractor.
		_ = tw.WriteHeader(&tar.Header{Name: "./", Typeflag: tar.TypeDir, Mode: 0o755})
		_ = tw.WriteHeader(&tar.Header{Name: "./notes.txt", Typeflag: tar.TypeReg, Mode: 0o644, Size: 2})
		_, _ = tw.Write([]byte("hi"))
		for rel, content := range files {
			_ = tw.WriteHeader(&tar.Header{Name: "./" + rel, Typeflag: tar.TypeReg, Mode: 0o644, Size: int64(len(content))})
			_, _ = tw.Write([]byte(content))
		}
		_ = tw.Close()
		return buf.Bytes(), nil
	}
	return nil, nil
}

const traceLineA = `{"schema_version":"v1","ts":"2026-04-19T12:00:00Z","run":"run-mj","job":"j","step":1,"op":"create","env":"env-a","dur_ms":100.0,"ok":true}` + "\n"
const traceLineB = `{"schema_version":"v1","ts":"2026-04-19T12:00:01Z","run":"run-mj","job":"j","step":1,"op":"exec","command":["true"],"env":"env-a","dur_ms":50.0,"ok":true,"exit":0}` + "\n"

func TestRealJobDriverUsesPerJobHeadJobAndSelector(t *testing.T) {
	dir := t.TempDir()
	rec, err := swebench.NewFileRecorder(filepath.Join(dir, "events.jsonl"), "run-mj")
	if err != nil {
		t.Fatal(err)
	}
	defer rec.Close()

	fake := &clusterFakeExecutor{logs: "step:1 - timing_s/step:1.0\n"}
	drv := &RealJobDriver{
		RunID:     "run-mj",
		Namespace: "bench-ns",
		OutDir:    dir,
		Executor:  fake,
		Spec: schema.JobSpec{
			Name:            "job-3",
			Mode:            schema.JobModeReal,
			TrainerManifest: "manifests/job-3.yaml",
			TraceMode:       "measure",
			HeadJob:         "trainer-3-head",
			PodSelector:     "app.kubernetes.io/instance=trainer-3",
		},
	}
	if err := drv.Run(context.Background(), rec); err != nil {
		t.Fatalf("Run: %v", err)
	}
	joined := strings.Join(fake.calls, "\n")
	if !strings.Contains(joined, "get job trainer-3-head -o json") {
		t.Fatalf("expected polling of head_job, got calls:\n%s", joined)
	}
	if !strings.Contains(joined, "logs -l app.kubernetes.io/instance=trainer-3") {
		t.Fatalf("expected logs via pod_selector, got calls:\n%s", joined)
	}
	if strings.Contains(joined, "job-name=job-3") {
		t.Fatalf("default selector must not be used when pod_selector is set:\n%s", joined)
	}
}

func TestVeRLManifestWaitsOnPerJobHeadJob(t *testing.T) {
	dir := t.TempDir()
	rec, err := swebench.NewFileRecorder(filepath.Join(dir, "events.jsonl"), "run-verl")
	if err != nil {
		t.Fatal(err)
	}
	defer rec.Close()

	for _, tc := range []struct {
		name     string
		headJob  string
		wantWait string
	}{
		{"default keeps example name", "", VeRLHeadJobName},
		{"per-job override", "verl-job-7", "verl-job-7"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fake := &clusterFakeExecutor{logs: "step:1 - timing_s/step:1.0\n"}
			drv := &RealJobDriver{
				RunID: "run-verl", Namespace: "ns", OutDir: dir, Executor: fake,
				Spec: schema.JobSpec{
					Name: "job-7", Mode: schema.JobModeReal,
					TrainerManifest: "examples/Qwen3-1.7B-verl/job-fully-async.yaml",
					TraceMode:       "measure", HeadJob: tc.headJob,
				},
			}
			if err := drv.Run(context.Background(), rec); err != nil {
				t.Fatalf("Run: %v", err)
			}
			joined := strings.Join(fake.calls, "\n")
			if !strings.Contains(joined, "get job "+tc.wantWait+" -o json") {
				t.Fatalf("want polling of %s, got:\n%s", tc.wantWait, joined)
			}
		})
	}
}

func TestRealJobDriverFailsFastOnFailedJob(t *testing.T) {
	dir := t.TempDir()
	rec, err := swebench.NewFileRecorder(filepath.Join(dir, "events.jsonl"), "run-f")
	if err != nil {
		t.Fatal(err)
	}
	defer rec.Close()
	fake := &clusterFakeExecutor{logs: "step:1 - timing_s/step:1.0\n", jobFailed: true}
	drv := &RealJobDriver{
		RunID: "run-f", Namespace: "ns", OutDir: dir, Executor: fake,
		Spec: schema.JobSpec{Name: "j", Mode: schema.JobModeReal, TrainerManifest: "m.yaml", TraceMode: "measure"},
	}
	start := time.Now()
	err = drv.Run(context.Background(), rec)
	if !errors.Is(err, ErrJobFailed) {
		t.Fatalf("err = %v, want ErrJobFailed", err)
	}
	if !strings.Contains(err.Error(), "BackoffLimitExceeded") {
		t.Fatalf("error should carry the Job's reason: %v", err)
	}
	if time.Since(start) > 5*time.Second {
		t.Fatal("a Failed job must not be waited on until the timeout")
	}
	// Logs are still read, so the trainer's step lines reach the report.
	if !strings.Contains(strings.Join(fake.calls, "\n"), " logs ") {
		t.Fatal("expected logs to be collected even for a failed job")
	}
}

func TestRealJobDriverSidecarModeWaitsForTrainerContainerAndDeletesJob(t *testing.T) {
	dir := t.TempDir()
	eventsPath := filepath.Join(dir, "events.jsonl")
	rec, err := swebench.NewFileRecorder(eventsPath, "run-sc")
	if err != nil {
		t.Fatal(err)
	}
	fake := &clusterFakeExecutor{
		logs:                "step:1 - timing_s/step:1.0\n",
		pods:                []string{"trainer-0"},
		podTraces:           map[string]map[string]string{"trainer-0": {"trace.jsonl": traceLineA}},
		trainerRunningPolls: 3,
	}
	drv := &RealJobDriver{
		RunID: "run-sc", Namespace: "ns", OutDir: dir, Executor: fake,
		Spec: schema.JobSpec{
			Name: "j", Mode: schema.JobModeReal, TrainerManifest: "m.yaml", TraceMode: "trace",
			HeadJob: "j-head", PodSelector: "app=j",
			TraceCollection: &schema.TraceCollection{
				Container: "keepalive", TrainerContainer: "workload", RemoteDir: "/tmp/ate-traces",
			},
		},
	}
	if err := drv.Run(context.Background(), rec); err != nil {
		t.Fatalf("Run: %v", err)
	}
	_ = rec.Close()
	joined := strings.Join(fake.calls, "\n")
	if strings.Contains(joined, "get job") {
		t.Fatalf("sidecar mode must wait on the container, not the Job:\n%s", joined)
	}
	if fake.podJSONPolls < 4 {
		t.Fatalf("expected the wait loop to poll until the trainer exited, polls=%d", fake.podJSONPolls)
	}
	if !strings.Contains(joined, "logs trainer-0 -c workload --tail=-1") {
		t.Fatalf("logs must come from the trainer container, pod by pod:\n%s", joined)
	}
	if !strings.Contains(joined, "exec trainer-0 -c keepalive -- sh -c") {
		t.Fatalf("traces must be harvested from the keepalive container:\n%s", joined)
	}
	execIdx := strings.Index(joined, "exec trainer-0")
	delIdx := strings.Index(joined, "delete job j-head --ignore-not-found --wait=false")
	if delIdx < 0 || delIdx < execIdx {
		t.Fatalf("the head Job must be deleted after harvesting:\n%s", joined)
	}
	if _, err := os.Stat(filepath.Join(dir, "traces", "j", "trainer-0", "trace.jsonl")); err != nil {
		t.Fatalf("harvested trace missing: %v", err)
	}
}

func TestRealJobDriverSidecarModeReportsTrainerExitCodeAfterHarvest(t *testing.T) {
	dir := t.TempDir()
	rec, err := swebench.NewFileRecorder(filepath.Join(dir, "events.jsonl"), "run-sc2")
	if err != nil {
		t.Fatal(err)
	}
	defer rec.Close()
	fake := &clusterFakeExecutor{
		logs:        "step:1 - timing_s/step:1.0\n",
		pods:        []string{"trainer-0"},
		podTraces:   map[string]map[string]string{"trainer-0": {"trace.jsonl": traceLineA}},
		trainerExit: 3,
	}
	drv := &RealJobDriver{
		RunID: "run-sc2", Namespace: "ns", OutDir: dir, Executor: fake,
		Spec: schema.JobSpec{
			Name: "j", Mode: schema.JobModeReal, TrainerManifest: "m.yaml", TraceMode: "trace",
			TraceCollection: &schema.TraceCollection{Container: "keepalive", TrainerContainer: "workload", RemoteDir: "/tmp/ate-traces"},
		},
	}
	err = drv.Run(context.Background(), rec)
	if !errors.Is(err, ErrJobFailed) || !strings.Contains(err.Error(), "exited 3") {
		t.Fatalf("err = %v, want ErrJobFailed with exit code", err)
	}
	if _, serr := os.Stat(filepath.Join(dir, "traces", "j", "trainer-0", "trace.jsonl")); serr != nil {
		t.Fatalf("traces must be harvested even when the trainer failed: %v", serr)
	}
	if !strings.Contains(strings.Join(fake.calls, "\n"), "delete job j --ignore-not-found") {
		t.Fatal("the head Job must be deleted after a failed trainer too")
	}
}

func sidecarDriver(t *testing.T, fake *clusterFakeExecutor, spec schema.JobSpec) (*RealJobDriver, *swebench.Recorder) {
	t.Helper()
	dir := t.TempDir()
	rec, err := swebench.NewFileRecorder(filepath.Join(dir, "events.jsonl"), "run-sc")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = rec.Close() })
	spec.Mode, spec.TrainerManifest, spec.TraceMode = schema.JobModeReal, "m.yaml", "trace"
	return &RealJobDriver{RunID: "run-sc", Namespace: "ns", OutDir: dir, Executor: fake, Spec: spec}, rec
}

func TestRealJobDriverSidecarModeFailsOnFailedPodWithoutContainerExit(t *testing.T) {
	// A pod evicted or OOM-killed before the trainer's exit status landed is
	// phase Failed with the container still "running": a failure, not exit 0.
	fake := &clusterFakeExecutor{logs: "step:1 - timing_s/step:1.0\n", pods: []string{"trainer-0"}, podFailedNoExit: true}
	drv, rec := sidecarDriver(t, fake, schema.JobSpec{
		Name: "j", HeadJob: "j-head", PodSelector: "app=j",
		TraceCollection: &schema.TraceCollection{Container: "keepalive", TrainerContainer: "workload", RemoteDir: "/tmp/ate-traces"},
	})
	start := time.Now()
	err := drv.Run(context.Background(), rec)
	if !errors.Is(err, ErrJobFailed) || !strings.Contains(err.Error(), "pod trainer-0 is Failed") {
		t.Fatalf("err = %v, want ErrJobFailed naming the Failed pod", err)
	}
	if time.Since(start) > 5*time.Second {
		t.Fatal("a Failed pod must not be waited on until the timeout")
	}
	if !strings.Contains(strings.Join(fake.calls, "\n"), "delete job j-head --ignore-not-found") {
		t.Fatal("the head Job must be deleted after a Failed pod too")
	}
}

func TestRealJobDriverSidecarModeDeletesHeadJobWhenTheWaitDoesNotEnd(t *testing.T) {
	// Without the delete, a wait that times out or is cancelled leaves the
	// keepalive sidecar holding the Job open for ever.
	t.Run("timeout", func(t *testing.T) {
		fake := &clusterFakeExecutor{pods: []string{"trainer-0"}, trainerRunningPolls: 1 << 30}
		drv, rec := sidecarDriver(t, fake, schema.JobSpec{
			Name: "j", HeadJob: "j-head", PodSelector: "app=j", Timeout: "20ms",
			TraceCollection: &schema.TraceCollection{Container: "keepalive", TrainerContainer: "workload", RemoteDir: "/tmp/ate-traces"},
		})
		err := drv.Run(context.Background(), rec)
		if err == nil || !strings.Contains(err.Error(), "timed out") {
			t.Fatalf("err = %v, want a timeout", err)
		}
		if !strings.Contains(strings.Join(fake.calls, "\n"), "delete job j-head --ignore-not-found") {
			t.Fatalf("the head Job must be deleted after a timed-out wait:\n%s", strings.Join(fake.calls, "\n"))
		}
	})
	t.Run("cancelled context", func(t *testing.T) {
		fake := &clusterFakeExecutor{pods: []string{"trainer-0"}, trainerRunningPolls: 1 << 30}
		drv, rec := sidecarDriver(t, fake, schema.JobSpec{
			Name: "j", HeadJob: "j-head", PodSelector: "app=j",
			TraceCollection: &schema.TraceCollection{Container: "keepalive", TrainerContainer: "workload", RemoteDir: "/tmp/ate-traces"},
		})
		ctx, cancel := context.WithCancel(context.Background())
		go func() { time.Sleep(10 * time.Millisecond); cancel() }()
		if err := drv.Run(ctx, rec); !errors.Is(err, context.Canceled) {
			t.Fatalf("err = %v, want context.Canceled", err)
		}
		if !strings.Contains(strings.Join(fake.calls, "\n"), "delete job j-head --ignore-not-found") {
			t.Fatalf("the head Job must be deleted after a cancelled wait:\n%s", strings.Join(fake.calls, "\n"))
		}
	})
}

func TestRealJobDriverSidecarModeWaitsOnTheTraceSelector(t *testing.T) {
	// The wait, the logs and the harvest read the same pods:
	// trace_collection.pod_selector first. Pods under it that do not run the
	// trainer container (Ray workers) neither block the wait nor get a logs call.
	fake := &clusterFakeExecutor{
		logs:        "step:1 - timing_s/step:1.0\n",
		pods:        []string{"ray-head-0", "ray-worker-a"},
		podTraces:   map[string]map[string]string{"ray-worker-a": {"trace.jsonl": traceLineA}},
		trainerless: map[string]bool{"ray-worker-a": true},
	}
	drv, rec := sidecarDriver(t, fake, schema.JobSpec{
		Name: "j", HeadJob: "j-head",
		TraceCollection: &schema.TraceCollection{
			PodSelector: "ray.io/cluster=j", Container: "keepalive", TrainerContainer: "workload", RemoteDir: "/tmp/ate-traces",
		},
	})
	if err := drv.Run(context.Background(), rec); err != nil {
		t.Fatalf("Run: %v", err)
	}
	joined := strings.Join(fake.calls, "\n")
	if !strings.Contains(joined, "get pods -l ray.io/cluster=j -o json") {
		t.Fatalf("the wait must use trace_collection.pod_selector:\n%s", joined)
	}
	if strings.Contains(joined, "job-name=j-head") {
		t.Fatalf("the default job-name selector must not be used when trace_collection names one:\n%s", joined)
	}
	if !strings.Contains(joined, "logs ray-head-0 -c workload") || strings.Contains(joined, "logs ray-worker-a") {
		t.Fatalf("logs must be read from the trainer pods only:\n%s", joined)
	}
	if _, err := os.Stat(filepath.Join(drv.OutDir, "traces", "j", "ray-worker-a", "trace.jsonl")); err != nil {
		t.Fatalf("harvested trace missing: %v", err)
	}
}

func TestVeRLSidecarModeRefusesTheDefaultSelector(t *testing.T) {
	fake := &clusterFakeExecutor{logs: "step:1 - timing_s/step:1.0\n", pods: []string{"p"}}
	drv, rec := sidecarDriver(t, fake, schema.JobSpec{
		Name:            "j",
		TraceCollection: &schema.TraceCollection{Container: "keepalive", TrainerContainer: "workload", RemoteDir: "/tmp/ate-traces"},
	})
	drv.Spec.TrainerManifest = "examples/Qwen3-1.7B-verl/job-fully-async.yaml"
	err := drv.Run(context.Background(), rec)
	if err == nil || !strings.Contains(err.Error(), "needs pod_selector") {
		t.Fatalf("err = %v, want a refusal naming pod_selector", err)
	}
	if strings.Contains(strings.Join(fake.calls, "\n"), "apply") {
		t.Fatal("nothing must be applied before the refusal")
	}
}

func TestRealJobDriverCollectsTracesFromEveryPod(t *testing.T) {
	dir := t.TempDir()
	eventsPath := filepath.Join(dir, "events.jsonl")
	rec, err := swebench.NewFileRecorder(eventsPath, "run-mj")
	if err != nil {
		t.Fatal(err)
	}

	fake := &clusterFakeExecutor{
		logs: "step:1 - timing_s/step:1.0\n",
		pods: []string{"trainer-head-0", "ray-worker-a", "ray-worker-b"},
		podTraces: map[string]map[string]string{
			// Head pod has no trace dir (exit 42) -> skipped, not fatal.
			"ray-worker-a": {"trace.jsonl": traceLineA},
			"ray-worker-b": {"nested/dir/trace.jsonl": traceLineB},
		},
	}
	drv := &RealJobDriver{
		RunID: "run-mj", Namespace: "ns", OutDir: dir, Executor: fake,
		Spec: schema.JobSpec{
			Name: "j", Mode: schema.JobModeReal,
			TrainerManifest: "manifests/j.yaml", TraceMode: "trace",
			TraceCollection: &schema.TraceCollection{
				PodSelector: "ray.io/cluster=j",
				Container:   "trainer",
				RemoteDir:   "/tmp/ate-traces",
			},
		},
	}
	if err := drv.Run(context.Background(), rec); err != nil {
		t.Fatalf("Run: %v", err)
	}
	_ = rec.Close()

	joined := strings.Join(fake.calls, "\n")
	if !strings.Contains(joined, "get pods -l ray.io/cluster=j") {
		t.Fatalf("expected pod listing with trace selector:\n%s", joined)
	}
	for _, pod := range fake.pods {
		if !strings.Contains(joined, "exec "+pod+" -c trainer -- sh -c") {
			t.Fatalf("expected exec into %s:\n%s", pod, joined)
		}
	}
	if !strings.Contains(joined, "tar -chf -") {
		t.Fatalf("tar must dereference symlinks (-h):\n%s", joined)
	}

	// Files land flattened under traces/<job>/<pod>/ and only *.jsonl survive.
	gotA, err := os.ReadFile(filepath.Join(dir, "traces", "j", "ray-worker-a", "trace.jsonl"))
	if err != nil || string(gotA) != traceLineA {
		t.Fatalf("worker-a trace: err=%v content=%q", err, gotA)
	}
	gotB, err := os.ReadFile(filepath.Join(dir, "traces", "j", "ray-worker-b", "nested__dir__trace.jsonl"))
	if err != nil || string(gotB) != traceLineB {
		t.Fatalf("worker-b trace: err=%v content=%q", err, gotB)
	}
	if _, err := os.Stat(filepath.Join(dir, "traces", "j", "ray-worker-a", "notes.txt")); err == nil {
		t.Fatal("non-jsonl entries must not be extracted")
	}
	if _, err := os.Stat(filepath.Join(dir, "traces", "j", "trainer-head-0")); err == nil {
		t.Fatal("pod without traces must not create a directory")
	}

	// Both pods' events were ingested, and validate's patterns find the files.
	rep, err := swebench.BuildReport(eventsPath)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Tasks != 1 {
		t.Fatalf("Tasks = %d, want 1 (env-a from two pods)", rep.Tasks)
	}
	if got := swebench.FindTraceFiles(dir); len(got) != 2 {
		t.Fatalf("FindTraceFiles = %v, want 2 per-pod files", got)
	}
}

func TestExtractTraceTarRejectsPathEscapes(t *testing.T) {
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	_ = tw.WriteHeader(&tar.Header{Name: "../../escape.jsonl", Typeflag: tar.TypeReg, Mode: 0o644, Size: 1})
	_, _ = tw.Write([]byte("x"))
	_ = tw.Close()
	dest := filepath.Join(t.TempDir(), "out")
	n, err := extractTraceTar(&buf, dest)
	if err != nil || n != 0 {
		t.Fatalf("n=%d err=%v, want 0 files and no error", n, err)
	}
	if _, err := os.Stat(dest); err == nil {
		t.Fatal("dest must not be created when nothing is extracted")
	}
}

func TestCollectPodTracesReportsTarFailureInsteadOfSkipping(t *testing.T) {
	dir := t.TempDir()
	rec, err := swebench.NewFileRecorder(filepath.Join(dir, "events.jsonl"), "run-tar")
	if err != nil {
		t.Fatal(err)
	}
	defer rec.Close()
	fake := &clusterFakeExecutor{
		logs:      "step:1 - timing_s/step:1.0\n",
		pods:      []string{"worker-a", "worker-b"},
		podTraces: map[string]map[string]string{"worker-a": {"trace.jsonl": traceLineA}, "worker-b": {"trace.jsonl": traceLineB}},
		tarFails:  map[string]bool{"worker-b": true},
	}
	drv := &RealJobDriver{
		RunID: "run-tar", Namespace: "ns", OutDir: dir, Executor: fake,
		Spec: schema.JobSpec{
			Name: "j", Mode: schema.JobModeReal, TrainerManifest: "m.yaml", TraceMode: "trace",
			TraceCollection: &schema.TraceCollection{PodSelector: "app=j", RemoteDir: "/tmp/ate-traces"},
		},
	}
	err = drv.Run(context.Background(), rec)
	if err == nil || !strings.Contains(err.Error(), "worker-b") || !strings.Contains(err.Error(), "file changed") {
		t.Fatalf("a tar failure must fail the harvest naming the pod, got: %v", err)
	}
	joined := strings.Join(fake.calls, "\n")
	if !strings.Contains(joined, `[ -d "/tmp/ate-traces" ] || exit 42; exec tar -chf -`) {
		t.Fatalf("the missing-dir check and tar must be separate commands:\n%s", joined)
	}
}
