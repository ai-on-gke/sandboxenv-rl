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
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
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

// runVeRLManifest deploys a VeRL fully-async RL workload (either via
// `deploy.sh up` or by creating the scripts ConfigMap and applying the job
// manifest), waits for the job's head Job to complete, and collects its
// trainer logs.
//
// The head Job defaults to the PR #112 example name
// (`verl-qwen3-1p7b-fully-async`) only when the scenario leaves `head_job`
// unset; multi-job scenarios must set `head_job` per job, or every driver
// would wait on the same Job and return as soon as the first one finishes.
func (r *RealJobDriver) runVeRLManifest(
	ctx context.Context,
	execRunner CommandExecutor,
	ns string,
	manifest string,
	env map[string]string,
) ([]byte, error) {
	headJob := strings.TrimSpace(r.Spec.HeadJob)
	if headJob == "" {
		headJob = VeRLHeadJobName
	}
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
		}
		clientsPythonDir := filepath.Join(dir, "..", "..", "clients", "python")
		if _, err := os.Stat(filepath.Join(clientsPythonDir, "pyproject.toml")); err == nil {
			tgzPath, err := archiveClientsPython(clientsPythonDir)
			if err != nil {
				return nil, fmt.Errorf("archive %s: %w", clientsPythonDir, err)
			}
			defer os.Remove(tgzPath)
			cmArgs = append(cmArgs, "--from-file=ate-env-rl.tgz="+tgzPath)
		}
		cmArgs = append(cmArgs, "--dry-run=client", "-o", "yaml")
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

	if r.Spec.PodSelector == "" && r.trainerContainer() == "" {
		// The example's pods carry no job-name label bench can predict, so
		// without a pod_selector wait on the Job and read its logs by name.
		if err := waitForJobTerminal(ctx, execRunner, env, ns, headJob, r.waitTimeout()); err != nil {
			if !errors.Is(err, ErrJobFailed) {
				return nil, fmt.Errorf("wait for %s: %w", headJob, err)
			}
			logs, _ := execRunner.Run(ctx, "kubectl", env, "-n", ns, "logs", "job/"+headJob, "--tail=-1")
			return logs, err
		}
		logs, err := execRunner.Run(ctx, "kubectl", env, "-n", ns, "logs", "job/"+headJob, "--tail=-1")
		if err != nil {
			return nil, fmt.Errorf("collect logs from %s: %w", headJob, err)
		}
		return logs, nil
	}
	return r.awaitJobAndLogs(ctx, execRunner, env, ns, headJob)
}

// archiveClientsPython packs pyproject.toml, README.md, and src/ from
// clientsPythonDir into a temporary .tgz so run_fully_async.sh can install
// ate-env-rl from /workspace/cfg/ate-env-rl.tgz without pinning a pre-merge
// commit hash.
func archiveClientsPython(clientsPythonDir string) (string, error) {
	f, err := os.CreateTemp("", "ate-env-rl-*.tgz")
	if err != nil {
		return "", err
	}
	tgzPath := f.Name()
	ok := false
	defer func() {
		if !ok {
			_ = os.Remove(tgzPath)
		}
	}()

	gzw := gzip.NewWriter(f)
	tw := tar.NewWriter(gzw)

	addFile := func(relPath, fullPath string, info fs.FileInfo) error {
		hdr, err := tar.FileInfoHeader(info, "")
		if err != nil {
			return err
		}
		hdr.Name = filepath.ToSlash(relPath)
		if err := tw.WriteHeader(hdr); err != nil {
			return err
		}
		src, err := os.Open(fullPath)
		if err != nil {
			return err
		}
		defer src.Close()
		_, err = io.Copy(tw, src)
		return err
	}

	for _, name := range []string{"pyproject.toml", "README.md"} {
		full := filepath.Join(clientsPythonDir, name)
		info, err := os.Stat(full)
		if err != nil {
			_ = tw.Close()
			_ = gzw.Close()
			_ = f.Close()
			return "", err
		}
		if err := addFile(name, full, info); err != nil {
			_ = tw.Close()
			_ = gzw.Close()
			_ = f.Close()
			return "", err
		}
	}

	srcRoot := filepath.Join(clientsPythonDir, "src")
	if err := filepath.WalkDir(srcRoot, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if d.IsDir() {
			if d.Name() == "__pycache__" || strings.HasSuffix(d.Name(), ".egg-info") {
				return filepath.SkipDir
			}
			return nil
		}
		if strings.HasSuffix(d.Name(), ".pyc") {
			return nil
		}
		rel, err := filepath.Rel(clientsPythonDir, path)
		if err != nil {
			return err
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		return addFile(rel, path, info)
	}); err != nil {
		_ = tw.Close()
		_ = gzw.Close()
		_ = f.Close()
		return "", err
	}

	if err := tw.Close(); err != nil {
		_ = gzw.Close()
		_ = f.Close()
		return "", err
	}
	if err := gzw.Close(); err != nil {
		_ = f.Close()
		return "", err
	}
	if err := f.Close(); err != nil {
		return "", err
	}
	ok = true
	return tgzPath, nil
}
