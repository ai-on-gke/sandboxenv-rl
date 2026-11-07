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
	"io"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
)

// maxTraceFileBytes bounds a single extracted trace file (256 MiB).
const maxTraceFileBytes = 256 << 20

// collectPodTraces copies every `*.jsonl` under the job's trace_collection
// remote_dir out of each pod matched by the selector into
// `<traceDir>/<pod>/`, flattening the in-pod directory layout into file
// names. Symlinks are dereferenced in the pod (`tar -h`), so what lands on
// disk are regular files that `bench validate` can read. Returns the number
// of files written. Pods without the directory are skipped, not failed: Ray
// head pods often have no traces of their own.
func (r *RealJobDriver) collectPodTraces(
	ctx context.Context,
	execRunner CommandExecutor,
	env map[string]string,
	traceDir string,
) (int, error) {
	tc := r.Spec.TraceCollection
	if tc == nil {
		return 0, nil
	}
	ns := r.Namespace
	if ns == "" {
		ns = "ate-bench"
	}
	selector := strings.TrimSpace(tc.PodSelector)
	if selector == "" {
		selector = r.Spec.EffectivePodSelector()
	}
	remoteDir := path.Clean(strings.TrimSpace(tc.RemoteDir))

	pods, err := listPods(ctx, execRunner, env, ns, selector)
	if err != nil {
		return 0, err
	}
	if len(pods) == 0 {
		return 0, fmt.Errorf("no pods matched selector %q in namespace %s", selector, ns)
	}

	stdout, hasStdout := execRunner.(StdoutExecutor)
	written := 0
	for _, pod := range pods {
		args := []string{"-n", ns, "exec", pod}
		if c := strings.TrimSpace(tc.Container); c != "" {
			args = append(args, "-c", c)
		}
		// `-h` follows symlinks so the archive holds file contents, not links.
		// A missing directory exits 42 and is treated as "no traces here";
		// any other failure (tar itself, for example "file changed as we
		// read it" on a live writer) is reported, not skipped.
		args = append(args, "--", "sh", "-c",
			fmt.Sprintf("[ -d %q ] || exit 42; exec tar -chf - -C %q .", remoteDir, remoteDir))
		var raw []byte
		if hasStdout {
			raw, err = stdout.Output(ctx, "kubectl", env, args...)
		} else {
			raw, err = execRunner.Run(ctx, "kubectl", env, args...)
		}
		if err != nil {
			if strings.Contains(err.Error(), "exit status 42") {
				continue
			}
			return written, fmt.Errorf("kubectl exec %s tar: %w", pod, err)
		}
		n, err := extractTraceTar(bytes.NewReader(raw), filepath.Join(traceDir, pod))
		if err != nil {
			return written, fmt.Errorf("extract traces from %s: %w", pod, err)
		}
		written += n
	}
	return written, nil
}

// listPods returns the sorted pod names matching selector in ns.
func listPods(ctx context.Context, execRunner CommandExecutor, env map[string]string, ns, selector string) ([]string, error) {
	args := []string{"-n", ns, "get", "pods", "-l", selector, "-o", "jsonpath={.items[*].metadata.name}"}
	var (
		raw []byte
		err error
	)
	if so, ok := execRunner.(StdoutExecutor); ok {
		raw, err = so.Output(ctx, "kubectl", env, args...)
	} else {
		raw, err = execRunner.Run(ctx, "kubectl", env, args...)
	}
	if err != nil {
		return nil, fmt.Errorf("kubectl get pods -l %s: %w", selector, err)
	}
	pods := strings.Fields(string(raw))
	sort.Strings(pods)
	return pods, nil
}

// extractTraceTar writes every regular `*.jsonl` entry of the tar stream into
// destDir, replacing path separators with `__` so nested in-pod layouts stay
// one level deep. Non-regular entries (directories, symlinks that `tar -h`
// could not resolve, devices) are skipped.
func extractTraceTar(src io.Reader, destDir string) (int, error) {
	tr := tar.NewReader(src)
	written := 0
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return written, nil
		}
		if err != nil {
			return written, err
		}
		if hdr.Typeflag != tar.TypeReg && hdr.Typeflag != tar.TypeRegA { //nolint:staticcheck // TypeRegA for old tars
			continue
		}
		name := path.Clean(strings.TrimPrefix(hdr.Name, "./"))
		if name == "." || name == "" || strings.HasPrefix(name, "..") || !strings.HasSuffix(name, ".jsonl") {
			continue
		}
		if written == 0 {
			if err := os.MkdirAll(destDir, 0o755); err != nil {
				return written, err
			}
		}
		flat := strings.ReplaceAll(name, "/", "__")
		f, err := os.OpenFile(filepath.Join(destDir, flat), os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
		if err != nil {
			return written, err
		}
		n, err := io.Copy(f, io.LimitReader(tr, maxTraceFileBytes+1))
		_ = f.Close()
		if err != nil {
			return written, err
		}
		if n > maxTraceFileBytes {
			return written, fmt.Errorf("trace file %s exceeds %d bytes", name, maxTraceFileBytes)
		}
		written++
	}
}
