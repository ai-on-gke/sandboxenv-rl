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
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/agent-substrate/env/internal/benchmarking/schema"
	"github.com/agent-substrate/env/internal/benchmarking/swebench"
)

// CommandExecutor abstracts command execution so `RealJobDriver` can be tested
// without spawning real cluster jobs or external trainers.
type CommandExecutor interface {
	Run(ctx context.Context, name string, env map[string]string, args ...string) ([]byte, error)
}

// OSCommandExecutor implements CommandExecutor using `os/exec`.
type OSCommandExecutor struct{}

// Run executes `name args...` with the provided environment variables merged
// onto `os.Environ()`.
func (OSCommandExecutor) Run(ctx context.Context, name string, env map[string]string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Env = os.Environ()
	for k, v := range env {
		cmd.Env = append(cmd.Env, fmt.Sprintf("%s=%s", k, v))
	}
	return cmd.CombinedOutput()
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

	switch {
	case len(r.Spec.TrainerCommand) > 0:
		logOutput, err = execRunner.Run(ctx, r.Spec.TrainerCommand[0], env, r.Spec.TrainerCommand[1:]...)
		if err != nil {
			return fmt.Errorf("execute trainer_command for %s: %w (output: %s)", r.Spec.Name, err, string(logOutput))
		}
	case r.Spec.TrainerManifest != "":
		logOutput, err = r.runManifestJob(ctx, execRunner, env)
		if err != nil {
			return err
		}
	default:
		return fmt.Errorf("RealJobDriver (%s): either trainer_manifest or trainer_command must be set", r.Spec.Name)
	}

	// Parse trainer step logs (VeRL, Slime, SkyRL, or MLLog) into EventStep records.
	stepEvents := ParseTrainerLog(r.Spec.Name, string(logOutput))
	for _, ev := range stepEvents {
		rec.Emit(ev)
	}

	// Ingest passive trace JSONL files emitted by ate_env_tool.py or ate-env wrapper.
	if traceDir := env["ATE_ENV_TRACE_DIR"]; traceDir != "" {
		traceEvents, err := IngestTraceDir(traceDir, r.Spec.Name)
		if err != nil {
			return fmt.Errorf("ingest trace dir %s: %w", traceDir, err)
		}
		for _, ev := range traceEvents {
			rec.Emit(ev)
		}
	}

	return nil
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
	jobSelector := fmt.Sprintf("job-name=%s", r.Spec.Name)
	if _, err := execRunner.Run(ctx, "kubectl", env, "-n", ns, "wait", "--for=condition=complete", "job/"+r.Spec.Name, "--timeout=3600s"); err != nil {
		return nil, fmt.Errorf("kubectl wait job/%s: %w", r.Spec.Name, err)
	}
	logs, err := execRunner.Run(ctx, "kubectl", env, "-n", ns, "logs", "-l", jobSelector, "--tail=-1")
	if err != nil {
		return nil, fmt.Errorf("kubectl logs %s: %w", jobSelector, err)
	}
	return logs, nil
}

// IngestTraceDir reads all `*.jsonl` trace files in `traceDir` and converts
// each `schema.TraceEvent` into canonical `swebench.Event` records (`phase` and
// `task`).
func IngestTraceDir(traceDir, defaultJob string) ([]swebench.Event, error) {
	entries, err := os.ReadDir(traceDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var out []swebench.Event
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".jsonl") {
			continue
		}
		evs, err := ParseTraceJSONLFile(filepath.Join(traceDir, entry.Name()), defaultJob)
		if err != nil {
			return nil, err
		}
		out = append(out, evs...)
	}
	return out, nil
}
