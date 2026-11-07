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
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/agent-substrate/env/internal/benchmarking/schema"
	"github.com/agent-substrate/env/internal/benchmarking/swebench"
)

// CommandExecutor abstracts command execution so `RealJobDriver` can be tested
// without spawning real cluster jobs or external trainers.
type CommandExecutor interface {
	Run(ctx context.Context, name string, env map[string]string, args ...string) ([]byte, error)
}

// StdoutExecutor is an optional CommandExecutor extension that returns only
// stdout, for commands whose output is a byte stream (a tar archive) that
// interleaved stderr would corrupt.
type StdoutExecutor interface {
	Output(ctx context.Context, name string, env map[string]string, args ...string) ([]byte, error)
}

// OSCommandExecutor implements CommandExecutor using `os/exec`.
type OSCommandExecutor struct{}

// Run executes `name args...` with the provided environment variables merged
// onto `os.Environ()`.
func (OSCommandExecutor) Run(ctx context.Context, name string, env map[string]string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Env = mergedEnv(env)
	return cmd.CombinedOutput()
}

// Output executes `name args...` and returns stdout only; stderr is folded
// into the returned error.
func (OSCommandExecutor) Output(ctx context.Context, name string, env map[string]string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Env = mergedEnv(env)
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return out, fmt.Errorf("%w: %s", err, strings.TrimSpace(stderr.String()))
	}
	return out, nil
}

func mergedEnv(env map[string]string) []string {
	merged := os.Environ()
	for k, v := range env {
		merged = append(merged, fmt.Sprintf("%s=%s", k, v))
	}
	return merged
}

// RealJobDriver orchestrates an actual RL training job (either an in-cluster
// Kubernetes Job/RayJob manifest such as `examples/Qwen3-1.7B-verl/job-fully-async.yaml`
// or an external trainer command) and ingests its trace events and trainer step
// telemetry into the canonical `swebench.Recorder`.
type RealJobDriver struct {
	RunID      string
	Namespace  string
	ServerAddr string
	Spec       schema.JobSpec
	OutDir     string
	Executor   CommandExecutor
	// Warnf receives non-fatal notes (a torn trace line dropped); nil discards.
	Warnf func(format string, args ...any)
}

// Name returns the job's unique name.
func (r *RealJobDriver) Name() string {
	return r.Spec.Name
}

// BuildEnv constructs the environment variable map injected into the real RL
// job or trainer process, including `ATE_ENV_TRACE_DIR` when `trace_mode` is
// `"trace"` (or by default for `ate_env_tool.py`).
func (r *RealJobDriver) BuildEnv() map[string]string {
	env := make(map[string]string, len(r.Spec.EnvVars)+6)
	for k, v := range r.Spec.EnvVars {
		env[k] = v
	}
	if r.ServerAddr != "" && env["ATE_ENV_SERVER"] == "" {
		env["ATE_ENV_SERVER"] = r.ServerAddr
	}
	env["ATE_BENCH_RUN_ID"] = r.RunID
	env["ATE_BENCH_JOB_NAME"] = r.Spec.Name
	if r.Spec.TraceMode == "" || r.Spec.TraceMode == "trace" {
		if r.OutDir != "" && env["ATE_ENV_TRACE_DIR"] == "" {
			env["ATE_ENV_TRACE_DIR"] = filepath.Join(r.OutDir, "traces", r.Spec.Name)
		}
	}
	return env
}

// Run launches the real RL job, collects its stdout/stderr trainer logs and any
// passive `ate-env` trace JSONL files, and emits normalized `step`, `phase`,
// and `task` events into `rec`.
func (r *RealJobDriver) Run(ctx context.Context, rec *swebench.Recorder) error {
	if rec == nil {
		return fmt.Errorf("RealJobDriver (%s): Recorder is nil", r.Spec.Name)
	}
	execRunner := r.Executor
	if execRunner == nil {
		execRunner = OSCommandExecutor{}
	}
	env := r.BuildEnv()
	if traceDir := env["ATE_ENV_TRACE_DIR"]; traceDir != "" {
		_ = os.MkdirAll(traceDir, 0o755)
	}

	var logOutput []byte
	var err error
	var jobErr error // the run ended unsuccessfully; traces are still harvested first

	switch {
	case len(r.Spec.TrainerCommand) > 0:
		logOutput, err = execRunner.Run(ctx, r.Spec.TrainerCommand[0], env, r.Spec.TrainerCommand[1:]...)
		if err != nil {
			return fmt.Errorf("execute trainer_command for %s: %w (output: %s)", r.Spec.Name, err, string(logOutput))
		}
	case r.Spec.TrainerManifest != "":
		if r.trainerContainer() != "" {
			// Sidecar mode: the keepalive container holds the pod, and so
			// the Job, open for ever. Delete it on every way out of this run
			// (harvested, wait timed out, context cancelled, logs failed),
			// after the harvest below, which the deferred order gives.
			ns := r.Namespace
			if ns == "" {
				ns = "ate-bench"
			}
			defer r.deleteHeadJob(ctx, execRunner, env, ns)
		}
		logOutput, err = r.runManifestJob(ctx, execRunner, env)
		if err != nil {
			if !errors.Is(err, ErrJobFailed) {
				return err
			}
			jobErr = fmt.Errorf("job %s: %w", r.Spec.Name, err)
		}
	default:
		return fmt.Errorf("RealJobDriver (%s): either trainer_manifest or trainer_command must be set", r.Spec.Name)
	}

	// Parse trainer step logs (VeRL, Slime, SkyRL, or MLLog) into EventStep records.
	stepEvents := ParseTrainerLog(r.Spec.Name, string(logOutput))
	for _, ev := range stepEvents {
		rec.Emit(ev)
	}

	// Pull trace files out of every pod of the job (Ray workers included) when
	// the scenario asks for it; in-cluster trainers have no shared filesystem
	// with bench, so ATE_ENV_TRACE_DIR alone is only enough for trainer_command.
	traceDir := env["ATE_ENV_TRACE_DIR"]
	if r.Spec.TraceCollection != nil && r.Spec.TrainerManifest != "" {
		if traceDir == "" {
			traceDir = filepath.Join(r.OutDir, "traces", r.Spec.Name)
		}
		_, cerr := r.collectPodTraces(ctx, execRunner, env, traceDir)
		if cerr != nil {
			if jobErr != nil {
				return fmt.Errorf("%w (and collecting traces: %v)", jobErr, cerr)
			}
			return fmt.Errorf("collect traces for %s: %w", r.Spec.Name, cerr)
		}
	}

	// Ingest passive trace JSONL files emitted by ate_env_tool.py or ate-env wrapper.
	if traceDir != "" {
		traceEvents, st, err := IngestTraceDir(traceDir, r.Spec.Name)
		if err != nil {
			return fmt.Errorf("ingest trace dir %s: %w", traceDir, err)
		}
		if st.TruncatedTail > 0 && r.Warnf != nil {
			r.Warnf("job %s: dropped the torn last line of %d trace file(s) under %s (copied mid-write); %d records kept",
				r.Spec.Name, st.TruncatedTail, traceDir, st.Records)
		}
		for _, ev := range traceEvents {
			rec.Emit(ev)
		}
	}

	return jobErr
}

func (r *RealJobDriver) runManifestJob(ctx context.Context, execRunner CommandExecutor, env map[string]string) ([]byte, error) {
	ns := r.Namespace
	if ns == "" {
		ns = "ate-bench"
	}
	manifest := r.Spec.TrainerManifest

	// First-class support for PR #112 Qwen3-1.7B VeRL deploy.sh or job-fully-async.yaml.
	if IsVeRLManifest(manifest) {
		return r.runVeRLManifest(ctx, execRunner, ns, manifest, env)
	}

	if _, err := execRunner.Run(ctx, "kubectl", env, "-n", ns, "apply", "-f", manifest); err != nil {
		return nil, fmt.Errorf("kubectl apply %s: %w", manifest, err)
	}
	return r.awaitJobAndLogs(ctx, execRunner, env, ns, r.Spec.EffectiveHeadJob())
}

// awaitJobAndLogs waits for the job to finish and returns its trainer logs.
//
// Without trace_collection.trainer_container it waits for the head Job to
// reach Complete or Failed (Failed fails fast) and reads the logs of the
// job's pod_selector. With it, the manifest is expected to hold the pod open
// after the trainer exits (a keepalive sidecar sharing the trace volume), so
// it waits for that container to terminate in every pod of the trace
// selector (trace_collection.pod_selector, else pod_selector, else
// `job-name=<head_job>`, the same pods the harvest reads) and reads the
// trainer container's logs pod by pod: the pod is then still exec-able for
// trace harvesting, which a Succeeded pod is not. A non-zero trainer exit,
// or a Failed pod without one, is reported through the returned error
// wrapping ErrJobFailed, after the logs were read, so the caller can still
// harvest traces before failing.
func (r *RealJobDriver) awaitJobAndLogs(ctx context.Context, execRunner CommandExecutor, env map[string]string, ns, headJob string) ([]byte, error) {
	trainer := r.trainerContainer()
	if trainer == "" {
		podSelector := r.Spec.EffectivePodSelector()
		waitErr := waitForJobTerminal(ctx, execRunner, env, ns, headJob, r.waitTimeout())
		if waitErr != nil && !errors.Is(waitErr, ErrJobFailed) {
			return nil, waitErr
		}
		logs, err := execRunner.Run(ctx, "kubectl", env, "-n", ns, "logs", "-l", podSelector, "--tail=-1")
		if err != nil {
			if waitErr != nil {
				return nil, fmt.Errorf("%w (and kubectl logs %s: %v)", waitErr, podSelector, err)
			}
			return nil, fmt.Errorf("kubectl logs %s: %w", podSelector, err)
		}
		return logs, waitErr
	}

	selector := r.traceSelector()
	exits, err := waitForContainerExit(ctx, execRunner, env, ns, selector, trainer, r.waitTimeout())
	if err != nil {
		return nil, err
	}
	var waitErr error
	for _, e := range exits {
		if e.PodFailed {
			waitErr = fmt.Errorf("%w: pod %s is Failed and container %s recorded no exit", ErrJobFailed, e.Pod, trainer)
			break
		}
		if e.ExitCode != 0 {
			waitErr = fmt.Errorf("%w: container %s of pod %s exited %d (%s)", ErrJobFailed, trainer, e.Pod, e.ExitCode, e.Reason)
			break
		}
	}
	// Per pod rather than `logs -l`: the trace selector may cover pods
	// without the trainer container, which `-l ... -c` refuses as a whole.
	var logs []byte
	for _, e := range exits {
		out, lerr := execRunner.Run(ctx, "kubectl", env, "-n", ns, "logs", e.Pod, "-c", trainer, "--tail=-1")
		if lerr != nil {
			if waitErr != nil {
				return nil, fmt.Errorf("%w (and kubectl logs %s -c %s: %v)", waitErr, e.Pod, trainer, lerr)
			}
			return nil, fmt.Errorf("kubectl logs %s -c %s: %w", e.Pod, trainer, lerr)
		}
		logs = append(logs, out...)
		if len(out) > 0 && out[len(out)-1] != '\n' {
			logs = append(logs, '\n')
		}
	}
	return logs, waitErr
}

// waitTimeout is the job's declared timeout, or the package default when the
// spec gives none (Validate rejects an invalid one earlier).
func (r *RealJobDriver) waitTimeout() time.Duration {
	if d, err := r.Spec.WaitTimeout(); err == nil {
		return d
	}
	return 0
}

// trainerContainer is trace_collection.trainer_container, or "".
func (r *RealJobDriver) trainerContainer() string {
	if r.Spec.TraceCollection == nil {
		return ""
	}
	return strings.TrimSpace(r.Spec.TraceCollection.TrainerContainer)
}

// traceSelector is the label selector of the pods traces are harvested
// from, and in sidecar mode waited on: trace_collection.pod_selector, else
// the job's pod_selector, else `job-name=<head_job>`.
func (r *RealJobDriver) traceSelector() string {
	if r.Spec.TraceCollection != nil {
		if s := strings.TrimSpace(r.Spec.TraceCollection.PodSelector); s != "" {
			return s
		}
	}
	return r.Spec.EffectivePodSelector()
}

// deleteHeadJob removes the head Job once a keepalive sidecar has served its
// purpose; without it the Job never completes on its own. It runs on every
// exit of a sidecar-mode run, a cancelled context included, so the delete
// gets its own short deadline. A failure is logged, not returned: the run's
// own outcome is already decided by then.
func (r *RealJobDriver) deleteHeadJob(ctx context.Context, execRunner CommandExecutor, env map[string]string, ns string) {
	dctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer cancel()
	name := r.Spec.EffectiveHeadJob()
	if out, err := execRunner.Run(dctx, "kubectl", env, "-n", ns, "delete", "job", name, "--ignore-not-found", "--wait=false"); err != nil && r.Warnf != nil {
		r.Warnf("job %s: delete head job/%s in %s failed (the keepalive sidecar holds it open; delete it by hand): %v %s",
			r.Spec.Name, name, ns, err, strings.TrimSpace(string(out)))
	}
}

// IngestTraceDir reads all `*.jsonl` trace files under `traceDir` (including
// per-pod subdirectories written by trace collection) and converts each
// `schema.TraceEvent` into canonical `swebench.Event` records (`phase` and
// `task`). Torn trailing lines are dropped and reported in the returned
// TraceParseStats (TruncatedTail is the number of files affected).
func IngestTraceDir(traceDir, defaultJob string) ([]swebench.Event, TraceParseStats, error) {
	var total TraceParseStats
	var files []string
	err := filepath.WalkDir(traceDir, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if !d.IsDir() && strings.HasSuffix(d.Name(), ".jsonl") {
			files = append(files, path)
		}
		return nil
	})
	if err != nil {
		if os.IsNotExist(err) {
			return nil, total, nil
		}
		return nil, total, err
	}
	sort.Strings(files)
	var out []swebench.Event
	for _, path := range files {
		evs, st, err := ParseTraceJSONLFileWithStats(path, defaultJob)
		if err != nil {
			return nil, total, err
		}
		total.Records += st.Records
		total.TruncatedTail += st.TruncatedTail
		out = append(out, evs...)
	}
	return out, total, nil
}
