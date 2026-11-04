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
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/agent-substrate/env/internal/benchmarking/swebench"
)

type recipeFakeBackend struct {
	mu         sync.Mutex
	creates    int
	awake      int
	maxAwake   int
	parkedByID map[string]bool
}

type recipeFakeSandbox struct {
	backend *recipeFakeBackend
	id      string
}

func (b *recipeFakeBackend) Create(_ context.Context, id, _ string) (swebench.Sandbox, error) {
	b.mu.Lock()
	if b.parkedByID == nil {
		b.parkedByID = make(map[string]bool)
	}
	b.creates++
	b.awake++
	if b.awake > b.maxAwake {
		b.maxAwake = b.awake
	}
	b.mu.Unlock()
	return &recipeFakeSandbox{backend: b, id: id}, nil
}

func (b *recipeFakeBackend) Delete(_ context.Context, id string) error {
	b.onDelete(id)
	return nil
}

func (b *recipeFakeBackend) onDelete(id string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.parkedByID[id] {
		delete(b.parkedByID, id)
		return
	}
	if b.awake > 0 {
		b.awake--
	}
}

func (s *recipeFakeSandbox) Run(_ context.Context, command string, _ time.Duration) (swebench.Result, error) {
	s.backend.mu.Lock()
	if s.backend.parkedByID[s.id] {
		delete(s.backend.parkedByID, s.id)
		s.backend.awake++
		if s.backend.awake > s.backend.maxAwake {
			s.backend.maxAwake = s.backend.awake
		}
	}
	s.backend.mu.Unlock()

	switch {
	case strings.Contains(command, "/tmp/grade.py"):
		return swebench.Result{
			ExitCode: 0,
			Stdout:   `{"valid":true,"resolved":true,"f2p":"1/1","p2p":"2/2"}` + "\n",
		}, nil
	case strings.Contains(command, "run_tests.sh"):
		return swebench.Result{
			ExitCode: 0,
			Stdout:   `{"valid":true,"reward":1,"detail":"matched 2/2 tests"}` + "\n",
		}, nil
	default:
		return swebench.Result{ExitCode: 0, Stdout: "42\n"}, nil
	}
}

func (s *recipeFakeSandbox) WriteFile(context.Context, string, []byte, fs.FileMode) error {
	return nil
}

func (s *recipeFakeSandbox) Suspend(context.Context) error {
	s.backend.mu.Lock()
	defer s.backend.mu.Unlock()
	if !s.backend.parkedByID[s.id] {
		s.backend.parkedByID[s.id] = true
		if s.backend.awake > 0 {
			s.backend.awake--
		}
	}
	return nil
}

func (s *recipeFakeSandbox) Delete(context.Context) error {
	s.backend.onDelete(s.id)
	return nil
}

func TestMixedR2ESWEBench200ConcurrencyShards(t *testing.T) {
	r2eTrajPath := filepath.Join("..", "..", "..", "benchmarking", "swebench", "r2e-gold.yaml")
	var assetsOut bytes.Buffer
	if err := assetsCmd([]string{r2eTrajPath}, &assetsOut); err != nil {
		t.Fatalf("trajectory-assets r2e-gold.yaml: %v", err)
	}
	if assetsOut.String() != "" {
		t.Fatalf("expected no external write.file assets in r2e-gold.yaml, got %q", assetsOut.String())
	}

	// Load and substitute zero think times so a 200-actor, 2-step simulation finishes in milliseconds.
	rawR2ETraj, err := os.ReadFile(r2eTrajPath)
	if err != nil {
		t.Fatalf("read r2e-gold.yaml: %v", err)
	}
	dir := t.TempDir()
	fastR2ETraj := filepath.Join(dir, "r2e-gold-fast.yaml")
	fastYAML := strings.NewReplacer("think: 1s", "think: 0s", "think: 2s", "think: 0s", "think: 3s", "think: 0s").Replace(string(rawR2ETraj))
	if err := os.WriteFile(fastR2ETraj, []byte(fastYAML), 0o644); err != nil {
		t.Fatal(err)
	}

	rawSWETraj, err := os.ReadFile(filepath.Join("..", "..", "..", "internal", "benchmarking", "swebench", "trajectories", "swebench-gold.yaml"))
	if err != nil {
		t.Fatalf("read swebench-gold.yaml: %v", err)
	}
	fastSWETraj := filepath.Join(dir, "swebench-gold-fast.yaml")
	fastSWEYAML := strings.NewReplacer("think: 1s", "think: 0s", "think: 2s", "think: 0s", "think: 3s", "think: 0s").Replace(string(rawSWETraj))
	if err := os.WriteFile(fastSWETraj, []byte(fastSWEYAML), 0o644); err != nil {
		t.Fatal(err)
	}
	graderPy, err := os.ReadFile(filepath.Join("..", "..", "..", "internal", "benchmarking", "swebench", "trajectories", "swebench-grade.py"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "swebench-grade.py"), graderPy, 0o644); err != nil {
		t.Fatal(err)
	}

	var r2eBuf, sweBuf bytes.Buffer
	for i := 0; i < 20; i++ {
		r2eRec := map[string]any{
			"kind":               "command",
			"instance_id":        fmt.Sprintf("r2e-inst-%02d", i),
			"repo":               "demo/r2e",
			"image":              fmt.Sprintf("us-docker.pkg.dev/p/r2e/img-%02d@sha256:%064x", i, 100+i),
			"setup_command":      "test -d /testbed",
			"reset_command":      "cd /testbed && git reset -q --hard",
			"command":            "/bin/bash run_tests.sh",
			"expected_exit_code": 0,
		}
		b, _ := json.Marshal(r2eRec)
		r2eBuf.Write(b)
		r2eBuf.WriteByte('\n')

		sweRec := map[string]any{
			"instance_id":  fmt.Sprintf("django__django-%d", 11000+i),
			"repo":         "django/django",
			"image":        fmt.Sprintf("us-docker.pkg.dev/p/swe/img-%02d@sha256:%064x", i, 200+i),
			"patch":        "diff --git a/a.py b/a.py\n",
			"test_patch":   "diff --git a/t.py b/t.py\n",
			"eval_script":  "#!/bin/bash\npytest\n",
			"fail_to_pass": []string{"test_a"},
			"pass_to_pass": []string{"test_b"},
			"log_parser":   "parse_log_pytest",
			"eval_type":    "pass_and_fail",
		}
		sb, _ := json.Marshal(sweRec)
		sweBuf.Write(sb)
		sweBuf.WriteByte('\n')
	}
	r2eManifest := filepath.Join(dir, "r2e-20.jsonl")
	sweManifest := filepath.Join(dir, "swe-20.jsonl")
	if err := os.WriteFile(r2eManifest, r2eBuf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(sweManifest, sweBuf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}

	fake := &recipeFakeBackend{}
	prev := testBackendOverride
	testBackendOverride = fake
	t.Cleanup(func() { testBackendOverride = prev })

	artifactDir := filepath.Join(dir, "artifacts")
	var wg sync.WaitGroup
	var sweCode, r2eCode int
	var sweErr, r2eErr bytes.Buffer

	wg.Add(2)
	go func() {
		defer wg.Done()
		var out bytes.Buffer
		sweCode = runMain([]string{
			"run",
			"-dataset", sweManifest,
			"-run-id", "r2eswe20-swe",
			"-images", "20",
			"-rollouts", "5",
			"-width", "100",
			"-mode", "steps",
			"-steps", "2",
			"-park", "suspend",
			"-trajectory", fastSWETraj,
			"-events-dir", artifactDir,
			"-worker", "swe",
			"-job", "swe",
		}, &out, &sweErr)
	}()
	go func() {
		defer wg.Done()
		var out bytes.Buffer
		r2eCode = runMain([]string{
			"run",
			"-dataset", r2eManifest,
			"-run-id", "r2eswe20-r2e",
			"-images", "20",
			"-rollouts", "5",
			"-width", "100",
			"-mode", "steps",
			"-steps", "2",
			"-park", "suspend",
			"-trajectory", fastR2ETraj,
			"-events-dir", artifactDir,
			"-worker", "r2e",
			"-job", "r2e",
		}, &out, &r2eErr)
	}()
	wg.Wait()

	if sweCode != 0 {
		t.Fatalf("swe shard exited %d: %s", sweCode, sweErr.String())
	}
	if r2eCode != 0 {
		t.Fatalf("r2e shard exited %d: %s", r2eCode, r2eErr.String())
	}
	if fake.creates != 200 {
		t.Fatalf("expected 200 created sandboxes (100 swe + 100 r2e), got %d", fake.creates)
	}

	var combined bytes.Buffer
	for _, shard := range []string{"r2e.jsonl", "swe.jsonl"} {
		raw, err := os.ReadFile(filepath.Join(artifactDir, "events", shard))
		if err != nil {
			t.Fatalf("read %s: %v", shard, err)
		}
		combined.Write(raw)
	}
	rep, err := swebench.Summarize(&combined)
	if err != nil {
		t.Fatalf("Summarize combined shards: %v", err)
	}
	if rep.Tasks != 400 || rep.Rewarded != 400 || rep.RewardRate != 1.0 {
		t.Fatalf("expected 400/400 rewarded tasks across 2 steps, got tasks=%d rewarded=%d rate=%v", rep.Tasks, rep.Rewarded, rep.RewardRate)
	}
	if rep.DistinctInstances != 40 || rep.DistinctEnvs != 200 || rep.PeakActiveSandboxes != 200 {
		t.Fatalf("expected 40 instances, 200 envs, 200 peak active sandboxes; got instances=%d envs=%d peak=%d",
			rep.DistinctInstances, rep.DistinctEnvs, rep.PeakActiveSandboxes)
	}
	if rep.Jobs["swe"].Tasks != 200 || rep.Jobs["r2e"].Tasks != 200 {
		t.Fatalf("expected 200 tasks per job, got swe=%+v r2e=%+v", rep.Jobs["swe"], rep.Jobs["r2e"])
	}
	if rep.Interference == nil || rep.Interference.ActiveJobs != 2 {
		t.Fatalf("expected interference across 2 active jobs, got %+v", rep.Interference)
	}

	// Verify oversubscribed replica rollouts (10 images * 4 rollouts = 40 per shard = 80 total envs)
	// capped at concurrency 30 (width 15 per shard) via -park-each never exceed 30 awake sandboxes.
	fakeOver := &recipeFakeBackend{}
	testBackendOverride = fakeOver
	overDir := filepath.Join(dir, "artifacts-over")
	var overWG sync.WaitGroup
	var sweOverCode, r2eOverCode int
	overWG.Add(2)
	go func() {
		defer overWG.Done()
		var out, errBuf bytes.Buffer
		sweOverCode = runMain([]string{
			"run", "-dataset", sweManifest, "-run-id", "r2eswe-ov-swe",
			"-images", "10", "-rollouts", "4", "-width", "15",
			"-mode", "steps", "-steps", "2", "-park", "suspend", "-park-each",
			"-trajectory", fastSWETraj, "-events-dir", overDir, "-worker", "swe", "-job", "swe",
		}, &out, &errBuf)
	}()
	go func() {
		defer overWG.Done()
		var out, errBuf bytes.Buffer
		r2eOverCode = runMain([]string{
			"run", "-dataset", r2eManifest, "-run-id", "r2eswe-ov-r2e",
			"-images", "10", "-rollouts", "4", "-width", "15",
			"-mode", "steps", "-steps", "2", "-park", "suspend", "-park-each",
			"-trajectory", fastR2ETraj, "-events-dir", overDir, "-worker", "r2e", "-job", "r2e",
		}, &out, &errBuf)
	}()
	overWG.Wait()
	if sweOverCode != 0 || r2eOverCode != 0 {
		t.Fatalf("oversubscribed shards failed: swe=%d r2e=%d", sweOverCode, r2eOverCode)
	}
	if fakeOver.creates != 80 {
		t.Fatalf("expected 80 created replica sandboxes, got %d", fakeOver.creates)
	}
	if fakeOver.maxAwake > 30 {
		t.Fatalf("expected max awake sandboxes <= 30 with -park-each, got %d", fakeOver.maxAwake)
	}
}
