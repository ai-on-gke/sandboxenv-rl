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

package measure

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/agent-substrate/env/internal/benchmarking/schema"
)

type fakeKubeRunner struct {
	nodesJSON       string
	podsJSON        string
	nodeMetricsJSON string
	podMetricsJSON  string
	logsOutput      string
}

func (f *fakeKubeRunner) Run(_ context.Context, args ...string) ([]byte, error) {
	cmd := strings.Join(args, " ")
	switch {
	case cmd == "config current-context":
		return []byte("gke_test_us-east7-c_gpu-cluster\n"), nil
	case cmd == "get nodes -o json":
		return []byte(f.nodesJSON), nil
	case cmd == "get pods -A -o json":
		return []byte(f.podsJSON), nil
	case cmd == "get --raw /apis/metrics.k8s.io/v1beta1/nodes":
		return []byte(f.nodeMetricsJSON), nil
	case cmd == "get --raw /apis/metrics.k8s.io/v1beta1/pods":
		return []byte(f.podMetricsJSON), nil
	case strings.HasPrefix(cmd, "logs "):
		return []byte(f.logsOutput), nil
	default:
		return []byte("{}"), nil
	}
}

func TestCollectK8sClusterMeasurements(t *testing.T) {
	fk := &fakeKubeRunner{
		nodesJSON: `{
			"items": [
				{
					"metadata": {
						"name": "node-sandbox-1",
						"labels": {
							"cloud.google.com/gke-nodepool": "sandbox-pool",
							"node.kubernetes.io/instance-type": "n2-standard-32",
							"topology.kubernetes.io/zone": "us-east7-c"
						}
					},
					"status": {
						"capacity": {"cpu": "32", "memory": "128Gi"},
						"allocatable": {"cpu": "30", "memory": "120Gi"},
						"nodeInfo": {"kubeletVersion": "v1.34.3-gke.100"}
					}
				},
				{
					"metadata": {
						"name": "node-gpu-1",
						"labels": {
							"cloud.google.com/gke-nodepool": "gpu-pool",
							"node.kubernetes.io/instance-type": "a3-ultragpu-8g",
							"topology.kubernetes.io/zone": "us-east7-c"
						}
					},
					"status": {
						"capacity": {"cpu": "224", "memory": "1872Gi", "nvidia.com/gpu": "8"},
						"allocatable": {"cpu": "220", "memory": "1800Gi", "nvidia.com/gpu": "8"},
						"nodeInfo": {"kubeletVersion": "v1.34.3-gke.100"}
					}
				}
			]
		}`,
		podsJSON: `{
			"items": [
				{
					"metadata": {
						"name": "default-pool-worker-0",
						"namespace": "ate-env",
						"creationTimestamp": "2026-04-20T18:00:00Z",
						"labels": {"ate-env/workerpool": "default-pool"}
					},
					"spec": {
						"nodeName": "node-sandbox-1",
						"runtimeClassName": "gvisor",
						"containers": [{
							"name": "worker",
							"image": "worker:latest",
							"resources": {
								"requests": {"cpu": "2", "memory": "4Gi"},
								"limits": {"cpu": "2", "memory": "4Gi"}
							}
						}]
					},
					"status": {
						"phase": "Running",
						"conditions": [
							{"type": "PodScheduled", "status": "True", "lastTransitionTime": "2026-04-20T18:00:01Z"},
							{"type": "Initialized", "status": "True", "lastTransitionTime": "2026-04-20T18:00:02Z"},
							{"type": "ContainersReady", "status": "True", "lastTransitionTime": "2026-04-20T18:00:04Z"},
							{"type": "Ready", "status": "True", "lastTransitionTime": "2026-04-20T18:00:05Z"}
						],
						"containerStatuses": [{"name": "worker", "restartCount": 0}]
					}
				}
			]
		}`,
		nodeMetricsJSON: `{
			"items": [
				{
					"metadata": {"name": "node-sandbox-1"},
					"usage": {"cpu": "1500m", "memory": "6Gi"}
				}
			]
		}`,
		podMetricsJSON: `{
			"items": [
				{
					"metadata": {"name": "default-pool-worker-0", "namespace": "ate-env"},
					"containers": [{"name": "worker", "usage": {"cpu": "250m", "memory": "512Mi"}}]
				}
			]
		}`,
	}

	sysDesc, summary, err := CollectK8sClusterMeasurements(context.Background(), K8sCollectOptions{
		Kube: fk,
	})
	if err != nil {
		t.Fatalf("CollectK8sClusterMeasurements: %v", err)
	}
	if err := sysDesc.Validate(); err != nil {
		t.Fatalf("sysDesc.Validate: %v", err)
	}
	if sysDesc.Cluster != "gke_test_us-east7-c_gpu-cluster" {
		t.Errorf("Cluster = %q", sysDesc.Cluster)
	}
	if sysDesc.SandboxFootprint == nil || sysDesc.SandboxFootprint.CPU != 2 || sysDesc.SandboxFootprint.MemoryGiB != 4 {
		t.Errorf("unexpected SandboxFootprint: %+v", sysDesc.SandboxFootprint)
	}
	if summary["worker_node_count"].P50 != 1 || summary["gpu_node_count"].P50 != 1 || summary["total_gpus"].P50 != 8 {
		t.Errorf("unexpected node/gpu counts: %+v", summary)
	}
	if summary["pod_schedule_ms"].P50 != 1000 || summary["pod_total_ready_ms"].P50 != 5000 {
		t.Errorf("unexpected pod startup timings: schedule=%+v ready=%+v", summary["pod_schedule_ms"], summary["pod_total_ready_ms"])
	}
	if summary["sandbox_pod_cpu_mcores"].P50 != 250 || summary["sandbox_pod_wss_mib"].P50 != 512 {
		t.Errorf("unexpected pod metrics: cpu=%+v wss=%+v", summary["sandbox_pod_cpu_mcores"], summary["sandbox_pod_wss_mib"])
	}
}

func TestCollectorsForScopesAndPlugins(t *testing.T) {
	// k8s scope should select only k8s-health, not substrate collectors.
	k8sCols := CollectorsForScopes([]schema.TelemetryScope{schema.ScopeK8s}, nil)
	if len(k8sCols) != 1 || k8sCols[0].Name() != "k8s-health" {
		t.Fatalf("expected only k8s-health for ScopeK8s, got %+v", k8sCols)
	}

	// substrate scope should select substrate collectors, not k8s-health.
	subCols := CollectorsForScopes([]schema.TelemetryScope{schema.ScopeSubstrate}, nil)
	for _, c := range subCols {
		if c.Scope() != schema.ScopeSubstrate {
			t.Fatalf("unexpected scope %q for collector %q", c.Scope(), c.Name())
		}
	}

	dir := t.TempDir()
	sampleLog := filepath.Join(dir, "feature.log")
	if err := os.WriteFile(sampleLog, []byte("op=restore snapshot_upload_ms=12.5\nop=restore snapshot_upload_ms=27.5\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	fk := &fakeKubeRunner{
		logsOutput: "INFO normal startup\nWARN uffd_page_fault timeout=1\nWARN uffd_page_fault timeout=2\n",
	}

	res, err := RunCollectors(context.Background(), CollectRequest{
		RunID:   "test-plugins",
		DestDir: dir,
		Scopes:  []schema.TelemetryScope{schema.ScopeSubstrate, schema.ScopeK8s},
		K8s:     K8sCollectOptions{Kube: fk},
		Plugins: []schema.TelemetryPluginSpec{
			{
				Name:          "uffd_warns",
				Kind:          schema.PluginKindK8sLogs,
				Scope:         schema.ScopeSubstrate,
				Namespace:     "ate-system",
				LabelSelector: "app=atelet",
				Grep:          `WARN.*uffd_page_fault`,
			},
			{
				Name:  "snapshot_upload_ms",
				Kind:  schema.PluginKindRegexMetric,
				Scope: schema.ScopeSubstrate,
				Glob:  "*.log",
				Regex: `snapshot_upload_ms=(?P<value>[0-9.]+)`,
				Unit:  "ms",
			},
		},
	})
	if err != nil {
		t.Fatalf("RunCollectors: %v", err)
	}
	if got := res.Measurements.CustomMetrics["uffd_warns"]; got.Count != 1 || got.P50 != 2 {
		t.Fatalf("unexpected uffd_warns summary: %+v", got)
	}
	if _, err := os.Stat(filepath.Join(dir, "plugins", "uffd_warns.log")); err != nil {
		t.Fatalf("expected plugins/uffd_warns.log to be written: %v", err)
	}
	if got := res.Measurements.CustomMetrics["snapshot_upload_ms"]; got.Count != 2 || got.P50 != 12.5 || got.Max != 27.5 {
		t.Fatalf("unexpected snapshot_upload_ms summary: %+v", got)
	}
}
