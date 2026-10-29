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
	"path/filepath"
	"strings"
)

// Canonical PR #112 (`examples/Qwen3-1.7B-verl`) resource names.
const (
	VeRLHeadJobName   = "verl-qwen3-1p7b-fully-async"
	VeRLWorkerJobName = "verl-qwen3-1p7b-fully-async-worker"
	VeRLConfigMapName = "verl-qwen3-1p7b-scripts"
)

// IsVeRLManifest returns true if `manifestPath` points to the PR #112
// `examples/Qwen3-1.7B-verl` directory, its `deploy.sh`, or `job-fully-async.yaml`.
func IsVeRLManifest(manifestPath string) bool {
	clean := filepath.ToSlash(manifestPath)
	return strings.Contains(clean, "Qwen3-1.7B-verl") ||
		strings.HasSuffix(clean, "job-fully-async.yaml")
}

// runVeRLManifest deploys the PR #112 Qwen3-1.7B VeRL fully-async RL workload
// (either via `deploy.sh up` or by creating the `verl-qwen3-1p7b-scripts`
// ConfigMap and applying `job-fully-async.yaml`), waits for
// `job/verl-qwen3-1p7b-fully-async` to complete, and collects its trainer logs.
func (r *RealJobDriver) runVeRLManifest(
	ctx context.Context,
	execRunner CommandExecutor,
	ns string,
	manifest string,
	env map[string]string,
) ([]byte, error) {
	if strings.HasSuffix(manifest, "deploy.sh") {
		if _, err := execRunner.Run(ctx, "bash", env, manifest, "up"); err != nil {
			return nil, fmt.Errorf("verl deploy.sh up: %w", err)
		}
	} else {
		dir := filepath.Dir(manifest)
		cmArgs := []string{
			"-n", ns, "create", "configmap", VeRLConfigMapName,
			"--from-file=ate_env_tool.py=" + filepath.Join(dir, "ate_env_tool.py"),
			"--from-file=tool_config.yaml=" + filepath.Join(dir, "tool_config.yaml"),
			"--from-file=run_fully_async.sh=" + filepath.Join(dir, "run_fully_async.sh"),
			"--dry-run=client", "-o", "yaml",
		}
		cmYAML, err := execRunner.Run(ctx, "kubectl", env, cmArgs...)
		if err != nil {
			return nil, fmt.Errorf("prepare configmap %s: %w", VeRLConfigMapName, err)
		}
		cmFile, err := os.CreateTemp("", VeRLConfigMapName+"-*.yaml")
		if err != nil {
			return nil, fmt.Errorf("create temp configmap %s: %w", VeRLConfigMapName, err)
		}
		cmPath := cmFile.Name()
		defer os.Remove(cmPath)
		if _, err := cmFile.Write(cmYAML); err != nil {
			_ = cmFile.Close()
			return nil, fmt.Errorf("write temp configmap %s: %w", VeRLConfigMapName, err)
		}
		if err := cmFile.Close(); err != nil {
			return nil, fmt.Errorf("close temp configmap %s: %w", VeRLConfigMapName, err)
		}
		if _, err := execRunner.Run(ctx, "kubectl", env, "-n", ns, "apply", "-f", cmPath); err != nil {
			return nil, fmt.Errorf("apply configmap %s: %w", VeRLConfigMapName, err)
		}
		if _, err := execRunner.Run(ctx, "kubectl", env, "-n", ns, "apply", "-f", manifest); err != nil {
			return nil, fmt.Errorf("kubectl apply %s: %w", manifest, err)
		}
	}

	if _, err := execRunner.Run(ctx, "kubectl", env, "-n", ns, "wait", "--for=condition=complete", "job/"+VeRLHeadJobName, "--timeout=3600s"); err != nil {
		return nil, fmt.Errorf("wait for %s: %w", VeRLHeadJobName, err)
	}
	logs, err := execRunner.Run(ctx, "kubectl", env, "-n", ns, "logs", "job/"+VeRLHeadJobName, "--tail=-1")
	if err != nil {
		return nil, fmt.Errorf("collect logs from %s: %w", VeRLHeadJobName, err)
	}
	return logs, nil
}
