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

package sut

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/agent-substrate/env/internal/benchmarking/schema"
	"github.com/agent-substrate/env/internal/internaltest/fakecontrol"
	"go.ate.dev/substrate/pkg/proto/ateapipb"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
)

type fakeApplier struct {
	applied [][]byte
	err     error
}

func (f *fakeApplier) Apply(_ context.Context, manifest []byte) error {
	if f.err != nil {
		return f.err
	}
	f.applied = append(f.applied, append([]byte(nil), manifest...))
	return nil
}

func TestRenderManifestAndProvisioner(t *testing.T) {
	spec := schema.SUTSpec{
		Substrate:    &schema.SubstrateSUTSpec{Atespace: "custom-atespace"},
		Replicas:     4,
		RuntimeClass: "gvisor",
		NodePool:     "rl-worker-pool",
		ExtraArgs: map[string]string{
			"wake-budget": "5m",
			"api_image":   "gcr.io/test/ate-env-api:v1",
		},
	}

	m1, err := RenderManifest(spec)
	if err != nil {
		t.Fatalf("RenderManifest failed: %v", err)
	}
	m2, err := RenderManifest(spec)
	if err != nil {
		t.Fatalf("RenderManifest second call failed: %v", err)
	}
	if !bytes.Equal(m1, m2) {
		t.Fatal("RenderManifest is not deterministic across calls")
	}

	s := string(m1)
	for _, want := range []string{
		"kind: Namespace",
		"name: custom-atespace",
		"kind: Deployment",
		"replicas: 4",
		"runtimeClassName: gvisor",
		"cloud.google.com/gke-nodepool: rl-worker-pool",
		"image: gcr.io/test/ate-env-api:v1",
		"-wake-budget=5m",
		"kind: Service",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("rendered manifest missing %q:\n%s", want, s)
		}
	}

	// Verify defaults when Atespace and Replicas are omitted.
	defBytes, err := RenderManifest(schema.SUTSpec{})
	if err != nil {
		t.Fatalf("RenderManifest default spec failed: %v", err)
	}
	defStr := string(defBytes)
	if !strings.Contains(defStr, "name: ate-env") || !strings.Contains(defStr, "replicas: 2") {
		t.Fatalf("default manifest did not honor defaults (ate-env, 2 replicas):\n%s", defStr)
	}

	// Test Provisioner with RenderOnly = true.
	artDir := t.TempDir()
	applier := &fakeApplier{}
	prov := NewProvisioner(applier)

	rendered, err := prov.Provision(context.Background(), ProvisionOptions{
		Spec:        spec,
		ArtifactDir: artDir,
		RenderOnly:  true,
	})
	if err != nil {
		t.Fatalf("Provision RenderOnly failed: %v", err)
	}
	if len(applier.applied) != 0 {
		t.Fatalf("expected 0 apply calls in RenderOnly mode, got %d", len(applier.applied))
	}
	diskBytes, err := os.ReadFile(filepath.Join(artDir, DefaultManifestFilename))
	if err != nil {
		t.Fatalf("read sut-manifest.yaml: %v", err)
	}
	if !bytes.Equal(diskBytes, rendered) {
		t.Fatal("sut-manifest.yaml on disk does not match returned manifest")
	}

	// Test Provisioner with RenderOnly = false (live apply via fakeApplier).
	artDir2 := t.TempDir()
	_, err = prov.Provision(context.Background(), ProvisionOptions{
		Spec:        spec,
		ArtifactDir: artDir2,
		RenderOnly:  false,
	})
	if err != nil {
		t.Fatalf("Provision apply failed: %v", err)
	}
	if len(applier.applied) != 1 || !bytes.Equal(applier.applied[0], rendered) {
		t.Fatalf("expected 1 matching apply call, got %d", len(applier.applied))
	}
}

type statefulFakeReapBackend struct {
	mu           sync.Mutex
	envs         map[string]int // env ID -> remaining delete attempts needed before removal; -1 means immortal/stuck
	deleteCalled []string
}

func newStatefulFakeReapBackend(initial map[string]int) *statefulFakeReapBackend {
	cp := make(map[string]int, len(initial))
	for k, v := range initial {
		cp[k] = v
	}
	return &statefulFakeReapBackend{envs: cp}
}

func (b *statefulFakeReapBackend) ListEnvironments(_ context.Context, _ string) ([]string, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := make([]string, 0, len(b.envs))
	for id := range b.envs {
		out = append(out, id)
	}
	sort.Strings(out)
	return out, nil
}

func (b *statefulFakeReapBackend) DeleteEnvironment(_ context.Context, _, id string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.deleteCalled = append(b.deleteCalled, id)
	rem, ok := b.envs[id]
	if !ok {
		return nil
	}
	if rem < 0 {
		return errors.New("resource stuck in terminating state")
	}
	rem--
	if rem <= 0 {
		delete(b.envs, id)
	} else {
		b.envs[id] = rem
	}
	return nil
}

func TestReapDiscoversOrphansAndPreservesOtherPrefixes(t *testing.T) {
	// Include unremembered server-side orphan environments requiring 1 or 2 delete passes,
	// plus an environment belonging to a different run prefix.
	backend := newStatefulFakeReapBackend(map[string]int{
		"run-42-env-0":       1,
		"run-42-orphan-leak": 2, // requires two poll cycles to disappear
		"other-run-env-0":    1,
	})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	res, err := Reap(ctx, backend, "ate-env", "run-42-", 5*time.Millisecond)
	if err != nil {
		t.Fatalf("Reap failed: %v", err)
	}
	if res.Remaining != 0 || len(res.RemainingIDs) != 0 {
		t.Fatalf("expected Remaining=0, got %+v", res)
	}
	if res.Deleted != 2 {
		t.Fatalf("expected Deleted=2, got %d", res.Deleted)
	}
	if res.DeletionRatePerSec <= 0 {
		t.Fatalf("expected positive DeletionRatePerSec, got %f", res.DeletionRatePerSec)
	}

	// Ensure other-run-env-0 was never deleted.
	remainingAll, _ := backend.ListEnvironments(ctx, "ate-env")
	if len(remainingAll) != 1 || remainingAll[0] != "other-run-env-0" {
		t.Fatalf("expected other-run-env-0 to remain untouched, got %v", remainingAll)
	}
}

func TestReapStuckEnvironmentFailsGuardrail3(t *testing.T) {
	backend := newStatefulFakeReapBackend(map[string]int{
		"run-99-ok":    1,
		"run-99-stuck": -1, // refuses to disappear
	})

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()

	res, err := Reap(ctx, backend, "ate-env", "run-99-", 5*time.Millisecond)
	if err == nil {
		t.Fatal("expected Reap to return an error when an environment refuses to disappear")
	}
	if res == nil || res.Remaining != 1 || len(res.RemainingIDs) != 1 || res.RemainingIDs[0] != "run-99-stuck" {
		t.Fatalf("expected Remaining=1 (run-99-stuck), got %+v", res)
	}
	if res.Deleted != 1 {
		t.Fatalf("expected Deleted=1 for run-99-ok, got %d", res.Deleted)
	}
}

func TestATEClusterReaperAgainstFakeControl(t *testing.T) {
	ctrl := fakecontrol.New()
	addr, stop, err := ctrl.Serve()
	if err != nil {
		t.Fatalf("fakecontrol.Serve: %v", err)
	}
	t.Cleanup(stop)

	conn, err := grpc.NewClient(addr, grpc.WithTransportCredentials(credentials.NewTLS(&tls.Config{InsecureSkipVerify: true})))
	if err != nil {
		t.Fatalf("grpc.NewClient: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })

	actorsClient := ateapipb.NewControlClient(conn)
	ctx := context.Background()

	for _, id := range []string{"bench-1-a", "bench-1-b", "keep-other"} {
		_, err := actorsClient.CreateActor(ctx, &ateapipb.CreateActorRequest{
			Actor: &ateapipb.Actor{
				Metadata: &ateapipb.ResourceMetadata{Atespace: "ate-env", Name: id},
			},
		})
		if err != nil {
			t.Fatalf("CreateActor %s: %v", id, err)
		}
	}

	reaper := NewATEClusterReaper(actorsClient)
	res, err := Reap(ctx, reaper, "ate-env", "bench-1-", 5*time.Millisecond)
	if err != nil {
		t.Fatalf("Reap with ATEClusterReaper failed: %v", err)
	}
	if res.Deleted != 2 || res.Remaining != 0 {
		t.Fatalf("unexpected ATEClusterReaper result: %+v", res)
	}

	left, err := reaper.ListEnvironments(ctx, "ate-env")
	if err != nil {
		t.Fatalf("ListEnvironments after reap: %v", err)
	}
	if len(left) != 1 || left[0] != "keep-other" {
		t.Fatalf("expected [keep-other] left on server, got %v", left)
	}
}

func TestSystemDescriptionRoundTrip(t *testing.T) {
	spec := schema.SUTSpec{
		RuntimeClass: "gvisor",
		ExtraArgs: map[string]string{
			"api_image":   "gcr.io/ate/api@sha256:1111",
			"guest_image": "gcr.io/ate/guest@sha256:2222",
		},
	}
	desc := BuildSystemDescription(
		spec,
		"gke-us-central1-rlbench",
		"v1.32.2-gke.100",
		"deadbeef1234",
		"sha256:abcdef0123456789",
		map[string]int{"default-pool": 2, "gvisor-pool": 8},
	)

	outPath := filepath.Join(t.TempDir(), "system_description.json")
	if err := WriteSystemDescription(outPath, desc); err != nil {
		t.Fatalf("WriteSystemDescription failed: %v", err)
	}

	f, err := os.Open(outPath)
	if err != nil {
		t.Fatalf("open system_description.json: %v", err)
	}
	defer f.Close()

	decoded, err := schema.DecodeJSONStrict[schema.SystemDescription](f)
	if err != nil {
		t.Fatalf("DecodeJSONStrict[SystemDescription] failed: %v", err)
	}
	if decoded.Cluster != "gke-us-central1-rlbench" ||
		decoded.KubernetesVersion != "v1.32.2-gke.100" ||
		decoded.RuntimeClass != "gvisor" ||
		decoded.APIImage != "gcr.io/ate/api@sha256:1111" ||
		decoded.GuestImage != "gcr.io/ate/guest@sha256:2222" ||
		decoded.GitSHA != "deadbeef1234" ||
		decoded.DatasetDigest != "sha256:abcdef0123456789" ||
		decoded.NodePools["gvisor-pool"] != 8 {
		t.Fatalf("unexpected decoded SystemDescription: %+v", decoded)
	}
}
