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
	"crypto/tls"
	"encoding/json"
	"fmt"
	"math"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/agent-substrate/env/internal/benchmarking/schema"
	"github.com/agent-substrate/env/internal/benchmarking/swebench"
	ateapipb "go.ate.dev/substrate/api/ateapi/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
)

// KubeCommandRunner executes a kubectl command and returns its raw stdout.
type KubeCommandRunner interface {
	Run(ctx context.Context, args ...string) ([]byte, error)
}

type defaultKubeRunner struct{}

func (defaultKubeRunner) Run(ctx context.Context, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "kubectl", args...)
	return cmd.Output()
}

// K8sCollectOptions configures Kubernetes and Substrate control-plane telemetry collection.
type K8sCollectOptions struct {
	Cluster          string
	Backend          string
	Access           string
	Atespace         string
	BaseTemplate     string
	ATEEndpoint      string
	SubstrateVersion string
	SDKVersion       string
	GitSHA           string
	Control          ateapipb.ControlClient
	Kube             KubeCommandRunner
}

type k8sNodeList struct {
	Items []k8sNode `json:"items"`
}

type k8sNode struct {
	Metadata struct {
		Name   string            `json:"name"`
		Labels map[string]string `json:"labels"`
	} `json:"metadata"`
	Status struct {
		Capacity    map[string]string `json:"capacity"`
		Allocatable map[string]string `json:"allocatable"`
		NodeInfo    struct {
			KubeletVersion          string `json:"kubeletVersion"`
			OSImage                 string `json:"osImage"`
			KernelVersion           string `json:"kernelVersion"`
			ContainerRuntimeVersion string `json:"containerRuntimeVersion"`
		} `json:"nodeInfo"`
	} `json:"status"`
}

type k8sPodList struct {
	Items []k8sPod `json:"items"`
}

type k8sPod struct {
	Metadata struct {
		Name              string            `json:"name"`
		Namespace         string            `json:"namespace"`
		CreationTimestamp string            `json:"creationTimestamp"`
		DeletionTimestamp string            `json:"deletionTimestamp"`
		Labels            map[string]string `json:"labels"`
		Annotations       map[string]string `json:"annotations"`
	} `json:"metadata"`
	Spec struct {
		NodeName         string `json:"nodeName"`
		RuntimeClassName string `json:"runtimeClassName"`
		Containers       []struct {
			Name      string   `json:"name"`
			Image     string   `json:"image"`
			Command   []string `json:"command"`
			Args      []string `json:"args"`
			Resources struct {
				Requests map[string]string `json:"requests"`
				Limits   map[string]string `json:"limits"`
			} `json:"resources"`
		} `json:"containers"`
	} `json:"spec"`
	Status struct {
		Phase      string `json:"phase"`
		Reason     string `json:"reason"`
		Conditions []struct {
			Type               string `json:"type"`
			Status             string `json:"status"`
			LastTransitionTime string `json:"lastTransitionTime"`
		} `json:"conditions"`
		ContainerStatuses []struct {
			Name         string `json:"name"`
			RestartCount int    `json:"restartCount"`
			State        struct {
				Terminated *struct {
					Reason string `json:"reason"`
				} `json:"terminated"`
			} `json:"state"`
			LastState struct {
				Terminated *struct {
					Reason string `json:"reason"`
				} `json:"terminated"`
			} `json:"lastState"`
		} `json:"containerStatuses"`
	} `json:"status"`
}

type k8sNodeMetricsList struct {
	Items []struct {
		Metadata struct {
			Name string `json:"name"`
		} `json:"metadata"`
		Usage map[string]string `json:"usage"`
	} `json:"items"`
}

type k8sPodMetricsList struct {
	Items []struct {
		Metadata struct {
			Name      string `json:"name"`
			Namespace string `json:"namespace"`
		} `json:"metadata"`
		Containers []struct {
			Name  string            `json:"name"`
			Usage map[string]string `json:"usage"`
		} `json:"containers"`
	} `json:"items"`
}

// CollectK8sHealthMeasurements collects Kubernetes node, pod, metrics.k8s.io,
// pod startup sub-phase, and cluster health (OOMKills/evictions/restarts) telemetry
// without requiring Substrate's ate-api ControlClient (ScopeK8s).
func CollectK8sHealthMeasurements(ctx context.Context, opts K8sCollectOptions) (*schema.SystemDescription, map[string]schema.MetricSummary, error) {
	k8sOnly := opts
	k8sOnly.Control = nil
	k8sOnly.ATEEndpoint = ""
	return CollectK8sClusterMeasurements(ctx, k8sOnly)
}

// CollectSubstrateControlMeasurements queries Substrate's ate-api ControlClient
// (ListActors and GetActorTemplate) and merges active/parked actor counts and
// template resource footprints into summary and sysDesc (ScopeSubstrate).
func CollectSubstrateControlMeasurements(ctx context.Context, opts K8sCollectOptions, sysDesc *schema.SystemDescription, summary map[string]schema.MetricSummary) error {
	ctrl := opts.Control
	var closeCtrl func() error
	if ctrl == nil && opts.ATEEndpoint != "" {
		conn, derr := grpc.NewClient(opts.ATEEndpoint, grpc.WithTransportCredentials(credentials.NewTLS(&tls.Config{InsecureSkipVerify: true})))
		if derr == nil {
			ctrl = ateapipb.NewControlClient(conn)
			closeCtrl = conn.Close
		}
	}
	if closeCtrl != nil {
		defer func() { _ = closeCtrl() }()
	}
	if ctrl == nil {
		return nil
	}
	atespace := opts.Atespace
	if atespace == "" {
		atespace = "ate-env"
	}
	baseTemplate := opts.BaseTemplate
	if baseTemplate == "" {
		baseTemplate = "default-template"
	}
	if resp, lerr := ctrl.ListActors(ctx, &ateapipb.ListActorsRequest{Atespace: atespace}); lerr == nil {
		var totalActors, activeActors, parkedActors int
		for _, a := range resp.GetActors() {
			totalActors++
			st := strings.ToUpper(a.GetStatus().GetState().String())
			if strings.Contains(st, "SUSPEND") {
				parkedActors++
			} else {
				activeActors++
			}
		}
		if summary != nil {
			summary["active_sandboxes"] = singleScalarSummary(float64(activeActors), "sandboxes")
			summary["parked_sandboxes"] = singleScalarSummary(float64(parkedActors), "sandboxes")
			summary["paused_sandboxes"] = singleScalarSummary(float64(parkedActors), "sandboxes")
			summary["total_sandboxes"] = singleScalarSummary(float64(totalActors), "sandboxes")
		}
	}
	if tmpl, terr := ctrl.GetActorTemplate(ctx, &ateapipb.GetActorTemplateRequest{
		ActorTemplate: &ateapipb.ObjectRef{Atespace: atespace, Name: baseTemplate},
	}); terr == nil && tmpl.GetResources() != nil && sysDesc != nil {
		var cpuCores, memGiB, diskGiB float64
		if lim := tmpl.GetResources().GetLimits(); lim != nil {
			cpuCores = float64(lim.GetCpuMillicores()) / 1000.0
			memGiB = float64(lim.GetMemoryBytes()) / (1024 * 1024 * 1024)
		}
		if cpuCores > 0 || memGiB > 0 {
			if diskGiB == 0 && sysDesc.SandboxFootprint != nil {
				diskGiB = sysDesc.SandboxFootprint.DiskGiB
			}
			sysDesc.SandboxFootprint = &schema.SandboxFootprint{
				CPU:       round2(cpuCores),
				MemoryGiB: round2(memGiB),
				DiskGiB:   round2(diskGiB),
			}
		}
	}
	return nil
}

// CollectK8sClusterMeasurements queries Kubernetes nodes, pods, metrics.k8s.io,
// and optionally the Substrate ate-api ControlClient to build a SystemDescription
// and populate ServerSummary metrics (sandbox density, bin-packing, actual vs
// requested CPU & memory WSS, pod startup sub-phases, and cluster health counters).
func CollectK8sClusterMeasurements(ctx context.Context, opts K8sCollectOptions) (*schema.SystemDescription, map[string]schema.MetricSummary, error) {
	kube := opts.Kube
	if kube == nil {
		kube = defaultKubeRunner{}
	}
	atespace := opts.Atespace
	if atespace == "" {
		atespace = "ate-env"
	}
	baseTemplate := opts.BaseTemplate
	if baseTemplate == "" {
		baseTemplate = "default-template"
	}

	clusterName := strings.TrimSpace(opts.Cluster)
	if clusterName == "" {
		if out, err := kube.Run(ctx, "config", "current-context"); err == nil {
			clusterName = strings.TrimSpace(string(out))
		}
		if clusterName == "" {
			clusterName = "k8s-cluster"
		}
	}

	nodesRaw, err := kube.Run(ctx, "get", "nodes", "-o", "json")
	if err != nil {
		return nil, nil, fmt.Errorf("kubectl get nodes: %w", err)
	}
	var nodeList k8sNodeList
	if err := json.Unmarshal(nodesRaw, &nodeList); err != nil {
		return nil, nil, fmt.Errorf("decode nodes json: %w", err)
	}

	podsRaw, err := kube.Run(ctx, "get", "pods", "-A", "-o", "json")
	if err != nil {
		return nil, nil, fmt.Errorf("kubectl get pods: %w", err)
	}
	var podList k8sPodList
	if err := json.Unmarshal(podsRaw, &podList); err != nil {
		return nil, nil, fmt.Errorf("decode pods json: %w", err)
	}

	summary := make(map[string]schema.MetricSummary)

	// Identify which nodes host sandbox/worker pods (ate-env namespace or gvisor runtimeClass).
	sandboxPodsPerNode := make(map[string]int)
	var scheduleDurs, initDurs, containerStartDurs, totalReadyDurs []float64
	var workerScheduleDurs, workerInitDurs, workerContainerStartDurs, workerTotalReadyDurs []float64
	var minWorkerCreatedTS, maxWorkerReadyTS time.Time
	var oomKills, podEvictions, containerRestarts int
	var workerPodCount, workerActorCapacity, bridgeMaxActivePods, idleAutoParkTimeoutS int
	var detectedRuntimeClass, detectedAPIImage, detectedSubstrateVersion, detectedGuestImage string
	var detectedFootprint *schema.SandboxFootprint

	for _, pod := range podList.Items {
		if strings.EqualFold(pod.Status.Reason, "Evicted") {
			podEvictions++
		}
		isSandboxOrATE := pod.Metadata.Namespace == atespace ||
			pod.Metadata.Namespace == "ate-system" ||
			pod.Metadata.Namespace == "default" ||
			pod.Spec.RuntimeClassName == "gvisor"
		if isSandboxOrATE {
			for _, cs := range pod.Status.ContainerStatuses {
				containerRestarts += cs.RestartCount
				if (cs.State.Terminated != nil && cs.State.Terminated.Reason == "OOMKilled") ||
					(cs.LastState.Terminated != nil && cs.LastState.Terminated.Reason == "OOMKilled") {
					oomKills++
				}
			}
		}
		if pod.Metadata.Namespace == "ate-system" && detectedSubstrateVersion == "" {
			for _, c := range pod.Spec.Containers {
				if c.Image != "" {
					detectedSubstrateVersion = extractImageVersion(c.Image)
					break
				}
			}
		}
		if (pod.Metadata.Namespace == "ate-system" || pod.Metadata.Namespace == atespace) && detectedAPIImage == "" {
			for _, c := range pod.Spec.Containers {
				if strings.Contains(c.Name, "api") && c.Image != "" {
					detectedAPIImage = c.Image
				}
			}
		}

		// Detect native Substrate worker pods and their --max-actors concurrency cap.
		isNativeWorkerPod := pod.Metadata.Labels["ate.dev/worker-pool"] != "" ||
			pod.Metadata.Labels["ate-env/workerpool"] != ""
		for _, c := range pod.Spec.Containers {
			if strings.Contains(c.Name, "ateworker") {
				isNativeWorkerPod = true
			}
			for _, tok := range append(append([]string(nil), c.Command...), c.Args...) {
				if strings.Contains(tok, "ateworker") {
					isNativeWorkerPod = true
				}
				if strings.HasPrefix(tok, "--max-actors=") || strings.HasPrefix(tok, "-max-actors=") {
					valStr := tok[strings.IndexByte(tok, '=')+1:]
					if v, perr := strconv.Atoi(strings.TrimSpace(valStr)); perr == nil && v > 0 {
						workerActorCapacity = v
					}
				}
				if strings.HasPrefix(tok, "--max-active-pods=") || strings.HasPrefix(tok, "-max-active-pods=") {
					valStr := tok[strings.IndexByte(tok, '=')+1:]
					if v, perr := strconv.Atoi(strings.TrimSpace(valStr)); perr == nil && v > 0 {
						bridgeMaxActivePods = v
					}
				}
				if strings.HasPrefix(tok, "--idle-park-timeout=") || strings.HasPrefix(tok, "-idle-park-timeout=") ||
					strings.HasPrefix(tok, "--actor-idle-timeout=") || strings.HasPrefix(tok, "-actor-idle-timeout=") {
					valStr := strings.TrimSpace(tok[strings.IndexByte(tok, '=')+1:])
					if dur, derr := time.ParseDuration(valStr); derr == nil && dur > 0 {
						idleAutoParkTimeoutS = int(dur.Seconds())
					} else if v, perr := strconv.Atoi(valStr); perr == nil && v > 0 {
						idleAutoParkTimeoutS = v
					}
				}
			}
		}
		if isNativeWorkerPod && (pod.Status.Phase == "Running" || pod.Status.Phase == "Pending") {
			workerPodCount++
			if workerActorCapacity == 0 {
				if fakeCap := pod.Metadata.Annotations["ate.dev/fake-max-actors"]; fakeCap != "" {
					if v, perr := strconv.Atoi(strings.TrimSpace(fakeCap)); perr == nil && v > 0 {
						workerActorCapacity = v
					}
				}
			}
			if workerActorCapacity == 0 {
				// Default --max-actors in Substrate's ateworker-gvisor / ateworker-microvm.
				workerActorCapacity = 1000
			}
		}

		isWorkerOrSandbox := (pod.Metadata.Namespace == atespace && !strings.Contains(pod.Metadata.Name, "env-api")) ||
			pod.Spec.RuntimeClassName == "gvisor" ||
			pod.Metadata.Labels["ate-env/workerpool"] != ""
		if !isWorkerOrSandbox {
			continue
		}
		if pod.Spec.RuntimeClassName != "" && detectedRuntimeClass == "" {
			detectedRuntimeClass = pod.Spec.RuntimeClassName
		}
		if detectedGuestImage == "" {
			for _, c := range pod.Spec.Containers {
				if c.Image != "" {
					detectedGuestImage = c.Image
					break
				}
			}
		}
		if detectedFootprint == nil {
			for _, c := range pod.Spec.Containers {
				cpuCores := parseCPUQuantityCores(firstNonEmpty(c.Resources.Limits["cpu"], c.Resources.Requests["cpu"]))
				memGiB := parseMemQuantityBytes(firstNonEmpty(c.Resources.Limits["memory"], c.Resources.Requests["memory"])) / (1024 * 1024 * 1024)
				diskGiB := parseMemQuantityBytes(firstNonEmpty(
					c.Resources.Limits["ephemeral-storage"],
					c.Resources.Requests["ephemeral-storage"],
					c.Resources.Limits["storage"],
					c.Resources.Requests["storage"],
				)) / (1024 * 1024 * 1024)
				if cpuCores > 0 || memGiB > 0 {
					detectedFootprint = &schema.SandboxFootprint{
						CPU:       round2(cpuCores),
						MemoryGiB: round2(memGiB),
						DiskGiB:   round2(diskGiB),
					}
					break
				}
			}
		}
		if pod.Spec.NodeName != "" && (pod.Status.Phase == "Running" || pod.Status.Phase == "Pending") {
			sandboxPodsPerNode[pod.Spec.NodeName]++
		}

		// Extract pod startup sub-phases from condition timestamps.
		createdTS, errCreated := time.Parse(time.RFC3339, pod.Metadata.CreationTimestamp)
		var scheduledTS, initTS, containersReadyTS, readyTS time.Time
		for _, cond := range pod.Status.Conditions {
			if cond.Status != "True" || cond.LastTransitionTime == "" {
				continue
			}
			ts, err := time.Parse(time.RFC3339, cond.LastTransitionTime)
			if err != nil {
				continue
			}
			switch cond.Type {
			case "PodScheduled":
				scheduledTS = ts
			case "Initialized":
				initTS = ts
			case "ContainersReady":
				containersReadyTS = ts
			case "Ready":
				readyTS = ts
			}
		}
		if errCreated == nil && !scheduledTS.IsZero() && !scheduledTS.Before(createdTS) {
			dur := float64(scheduledTS.Sub(createdTS).Milliseconds())
			scheduleDurs = append(scheduleDurs, dur)
			if isNativeWorkerPod {
				workerScheduleDurs = append(workerScheduleDurs, dur)
			}
		}
		if !scheduledTS.IsZero() && !initTS.IsZero() && !initTS.Before(scheduledTS) {
			dur := float64(initTS.Sub(scheduledTS).Milliseconds())
			initDurs = append(initDurs, dur)
			if isNativeWorkerPod {
				workerInitDurs = append(workerInitDurs, dur)
			}
		}
		if !scheduledTS.IsZero() && !containersReadyTS.IsZero() && !containersReadyTS.Before(scheduledTS) {
			dur := float64(containersReadyTS.Sub(scheduledTS).Milliseconds())
			containerStartDurs = append(containerStartDurs, dur)
			if isNativeWorkerPod {
				workerContainerStartDurs = append(workerContainerStartDurs, dur)
			}
		}
		if errCreated == nil && !readyTS.IsZero() && !readyTS.Before(createdTS) {
			dur := float64(readyTS.Sub(createdTS).Milliseconds())
			totalReadyDurs = append(totalReadyDurs, dur)
			if isNativeWorkerPod {
				workerTotalReadyDurs = append(workerTotalReadyDurs, dur)
				if minWorkerCreatedTS.IsZero() || createdTS.Before(minWorkerCreatedTS) {
					minWorkerCreatedTS = createdTS
				}
				if maxWorkerReadyTS.IsZero() || readyTS.After(maxWorkerReadyTS) {
					maxWorkerReadyTS = readyTS
				}
			}
		}
	}

	if len(scheduleDurs) > 0 {
		summary["pod_schedule_ms"] = summarizeSlice(scheduleDurs, "ms")
	}
	if len(initDurs) > 0 {
		summary["pod_init_ms"] = summarizeSlice(initDurs, "ms")
	} else if len(totalReadyDurs) > 0 {
		summary["pod_init_ms"] = summarizeSlice(make([]float64, len(totalReadyDurs)), "ms")
	}
	if len(containerStartDurs) > 0 {
		summary["pod_container_start_ms"] = summarizeSlice(containerStartDurs, "ms")
	}
	if len(totalReadyDurs) > 0 {
		summary["pod_total_ready_ms"] = summarizeSlice(totalReadyDurs, "ms")
	}
	if len(workerScheduleDurs) > 0 {
		summary["worker_pod_schedule_ms"] = summarizeSlice(workerScheduleDurs, "ms")
	}
	if len(workerInitDurs) > 0 {
		summary["worker_pod_init_ms"] = summarizeSlice(workerInitDurs, "ms")
	} else if len(workerTotalReadyDurs) > 0 {
		summary["worker_pod_init_ms"] = summarizeSlice(make([]float64, len(workerTotalReadyDurs)), "ms")
	}
	if len(workerContainerStartDurs) > 0 {
		summary["worker_pod_container_start_ms"] = summarizeSlice(workerContainerStartDurs, "ms")
	}
	if len(workerTotalReadyDurs) > 0 {
		summary["worker_pod_total_ready_ms"] = summarizeSlice(workerTotalReadyDurs, "ms")
	}
	if !minWorkerCreatedTS.IsZero() && !maxWorkerReadyTS.IsZero() && !maxWorkerReadyTS.Before(minWorkerCreatedTS) {
		summary["workerpool_rollout_span_ms"] = singleScalarSummary(float64(maxWorkerReadyTS.Sub(minWorkerCreatedTS).Milliseconds()), "ms")
	}
	summary["oom_kills"] = singleScalarSummary(float64(oomKills), "count")
	summary["pod_evictions"] = singleScalarSummary(float64(podEvictions), "count")
	summary["container_restarts"] = singleScalarSummary(float64(containerRestarts), "count")

	// Classify nodes and build SystemDescription.
	nodePools := make(map[string]int)
	machineTypes := make(map[string]schema.NodePoolInfo)
	poolZones := make(map[string]map[string]bool)
	zoneSet := make(map[string]bool)
	var k8sVersion, osImage, kernelVersion, containerRuntime string

	type nodeCap struct {
		name        string
		pool        string
		isGPU       bool
		isWorker    bool
		capCPU      float64
		allocCPU    float64
		capMemGiB   float64
		allocMemGiB float64
		allocPods   float64
		gpus        float64
		reqCPU      float64
		reqMemGiB   float64
	}
	nodeMap := make(map[string]*nodeCap, len(nodeList.Items))
	hasDedicatedSandboxNodes := len(sandboxPodsPerNode) > 0
	for _, n := range nodeList.Items {
		p := strings.ToLower(n.Metadata.Labels["cloud.google.com/gke-nodepool"])
		if strings.Contains(p, "sandbox") || strings.Contains(p, "worker") {
			hasDedicatedSandboxNodes = true
			break
		}
	}

	for _, n := range nodeList.Items {
		if k8sVersion == "" && n.Status.NodeInfo.KubeletVersion != "" {
			k8sVersion = n.Status.NodeInfo.KubeletVersion
		}
		if osImage == "" && n.Status.NodeInfo.OSImage != "" {
			osImage = n.Status.NodeInfo.OSImage
		}
		if kernelVersion == "" && n.Status.NodeInfo.KernelVersion != "" {
			kernelVersion = n.Status.NodeInfo.KernelVersion
		}
		if containerRuntime == "" && n.Status.NodeInfo.ContainerRuntimeVersion != "" {
			containerRuntime = n.Status.NodeInfo.ContainerRuntimeVersion
		}
		pool := n.Metadata.Labels["cloud.google.com/gke-nodepool"]
		if pool == "" {
			pool = "default"
		}
		mtype := firstNonEmpty(
			n.Metadata.Labels["node.kubernetes.io/instance-type"],
			n.Metadata.Labels["beta.kubernetes.io/instance-type"],
			"unknown",
		)
		zone := firstNonEmpty(
			n.Metadata.Labels["topology.kubernetes.io/zone"],
			n.Metadata.Labels["failure-domain.beta.kubernetes.io/zone"],
		)
		if zone != "" {
			zoneSet[zone] = true
			if poolZones[pool] == nil {
				poolZones[pool] = make(map[string]bool)
			}
			poolZones[pool][zone] = true
		}
		nodePools[pool]++
		npInfo := machineTypes[pool]
		npInfo.MachineType = mtype
		npInfo.Count++
		if pz := poolZones[pool]; len(pz) > 0 {
			var zlist []string
			for z := range pz {
				zlist = append(zlist, z)
			}
			sort.Strings(zlist)
			npInfo.Zone = strings.Join(zlist, ",")
		}
		machineTypes[pool] = npInfo

		gpus := parseCPUQuantityCores(n.Status.Capacity["nvidia.com/gpu"])
		capCPU := parseCPUQuantityCores(n.Status.Capacity["cpu"])
		allocCPU := parseCPUQuantityCores(n.Status.Allocatable["cpu"])
		capMemGiB := parseMemQuantityBytes(n.Status.Capacity["memory"]) / (1024 * 1024 * 1024)
		allocMemGiB := parseMemQuantityBytes(n.Status.Allocatable["memory"]) / (1024 * 1024 * 1024)
		allocPods := parseCPUQuantityCores(firstNonEmpty(n.Status.Allocatable["pods"], n.Status.Capacity["pods"]))
		if allocPods <= 0 {
			allocPods = 110
		}

		isGPU := gpus > 0
		isWorker := false
		if !isGPU {
			if hasDedicatedSandboxNodes {
				isWorker = sandboxPodsPerNode[n.Metadata.Name] > 0 || strings.Contains(pool, "sandbox") || strings.Contains(pool, "worker")
			} else {
				isWorker = true
			}
		}
		nodeMap[n.Metadata.Name] = &nodeCap{
			name:        n.Metadata.Name,
			pool:        pool,
			isGPU:       isGPU,
			isWorker:    isWorker,
			capCPU:      capCPU,
			allocCPU:    allocCPU,
			capMemGiB:   capMemGiB,
			allocMemGiB: allocMemGiB,
			allocPods:   allocPods,
			gpus:        gpus,
		}
	}

	// Sum pod resource requests per node for non-terminated pods.
	for _, pod := range podList.Items {
		if pod.Status.Phase == "Succeeded" || pod.Status.Phase == "Failed" || pod.Spec.NodeName == "" {
			continue
		}
		nc, ok := nodeMap[pod.Spec.NodeName]
		if !ok {
			continue
		}
		for _, c := range pod.Spec.Containers {
			nc.reqCPU += parseCPUQuantityCores(c.Resources.Requests["cpu"])
			nc.reqMemGiB += parseMemQuantityBytes(c.Resources.Requests["memory"]) / (1024 * 1024 * 1024)
		}
	}

	var workerNodes, systemNodes, gpuNodes int
	var workerCapCPU, workerAllocCPU, workerCapGiB, workerAllocGiB float64
	var workerReqCPU, workerReqGiB float64
	var systemCapCPU, systemCapGiB float64
	var gpuCapCPU, gpuCapGiB, totalGPUs float64
	var workerNodeNames []string

	for name, nc := range nodeMap {
		switch {
		case nc.isGPU:
			gpuNodes++
			totalGPUs += nc.gpus
			gpuCapCPU += nc.capCPU
			gpuCapGiB += nc.capMemGiB
		case nc.isWorker:
			workerNodes++
			workerNodeNames = append(workerNodeNames, name)
			workerCapCPU += nc.capCPU
			workerAllocCPU += nc.allocCPU
			workerCapGiB += nc.capMemGiB
			workerAllocGiB += nc.allocMemGiB
			workerReqCPU += nc.reqCPU
			workerReqGiB += nc.reqMemGiB
		default:
			systemNodes++
			systemCapCPU += nc.capCPU
			systemCapGiB += nc.capMemGiB
		}
	}
	sort.Strings(workerNodeNames)

	summary["worker_node_count"] = singleScalarSummary(float64(workerNodes), "nodes")
	summary["worker_total_vcpu"] = singleScalarSummary(round2(workerCapCPU), "vcpu")
	summary["worker_allocatable_vcpu"] = singleScalarSummary(round2(workerAllocCPU), "vcpu")
	summary["worker_total_gib"] = singleScalarSummary(round2(workerCapGiB), "GiB")
	summary["worker_allocatable_gib"] = singleScalarSummary(round2(workerAllocGiB), "GiB")
	summary["worker_cpu_requested_cores"] = singleScalarSummary(round2(workerReqCPU), "cores")
	summary["worker_mem_requested_gib"] = singleScalarSummary(round2(workerReqGiB), "GiB")
	if workerAllocCPU > 0 {
		summary["cluster_packing_cpu_pct"] = singleScalarSummary(round2(100.0*workerReqCPU/workerAllocCPU), "%")
	}
	if workerAllocGiB > 0 {
		summary["cluster_packing_mem_pct"] = singleScalarSummary(round2(100.0*workerReqGiB/workerAllocGiB), "%")
	}

	summary["system_node_count"] = singleScalarSummary(float64(systemNodes), "nodes")
	summary["system_total_vcpu"] = singleScalarSummary(round2(systemCapCPU), "vcpu")
	summary["system_total_gib"] = singleScalarSummary(round2(systemCapGiB), "GiB")
	summary["gpu_node_count"] = singleScalarSummary(float64(gpuNodes), "nodes")
	summary["total_gpus"] = singleScalarSummary(round2(totalGPUs), "gpus")
	summary["gpu_node_total_vcpu"] = singleScalarSummary(round2(gpuCapCPU), "vcpu")
	summary["gpu_node_total_gib"] = singleScalarSummary(round2(gpuCapGiB), "GiB")

	// Query Substrate ControlClient (if configured) for active vs parked actors and template footprint.
	ctrl := opts.Control
	var closeCtrl func() error
	if ctrl == nil && opts.ATEEndpoint != "" {
		conn, derr := grpc.NewClient(opts.ATEEndpoint, grpc.WithTransportCredentials(credentials.NewTLS(&tls.Config{InsecureSkipVerify: true})))
		if derr == nil {
			ctrl = ateapipb.NewControlClient(conn)
			closeCtrl = conn.Close
		}
	}
	if closeCtrl != nil {
		defer func() { _ = closeCtrl() }()
	}

	actorsPerNode := make(map[string]int)
	var totalActors, activeActors, parkedActors int
	hasActorList := false
	if ctrl != nil {
		if resp, lerr := ctrl.ListActors(ctx, &ateapipb.ListActorsRequest{Atespace: atespace}); lerr == nil {
			hasActorList = true
			for _, a := range resp.GetActors() {
				totalActors++
				st := strings.ToUpper(a.GetStatus().GetState().String())
				if strings.Contains(st, "SUSPEND") {
					parkedActors++
				} else {
					activeActors++
				}
				if node := a.GetStatus().GetAssignedNode(); node != "" {
					actorsPerNode[node]++
				}
			}
		}
		if tmpl, terr := ctrl.GetActorTemplate(ctx, &ateapipb.GetActorTemplateRequest{
			ActorTemplate: &ateapipb.ObjectRef{Atespace: atespace, Name: baseTemplate},
		}); terr == nil && tmpl.GetResources() != nil {
			var cpuCores, memGiB, diskGiB float64
			if lim := tmpl.GetResources().GetLimits(); lim != nil {
				cpuCores = float64(lim.GetCpuMillicores()) / 1000.0
				memGiB = float64(lim.GetMemoryBytes()) / (1024 * 1024 * 1024)
			}
			if cpuCores > 0 || memGiB > 0 {
				if diskGiB == 0 && detectedFootprint != nil {
					diskGiB = detectedFootprint.DiskGiB
				}
				detectedFootprint = &schema.SandboxFootprint{
					CPU:       round2(cpuCores),
					MemoryGiB: round2(memGiB),
					DiskGiB:   round2(diskGiB),
				}
			}
		}
	}

	// Compute sandbox density per node, per vCPU, and per GiB RAM.
	var perNodeCounts []float64
	var totalSandboxes float64
	for _, nodeName := range workerNodeNames {
		cnt := sandboxPodsPerNode[nodeName]
		if hasActorList && actorsPerNode[nodeName] > 0 {
			cnt = actorsPerNode[nodeName]
		}
		perNodeCounts = append(perNodeCounts, float64(cnt))
		totalSandboxes += float64(cnt)
	}
	if hasActorList {
		summary["active_sandboxes"] = singleScalarSummary(float64(activeActors), "sandboxes")
		summary["parked_sandboxes"] = singleScalarSummary(float64(parkedActors), "sandboxes")
		summary["paused_sandboxes"] = singleScalarSummary(float64(parkedActors), "sandboxes")
		summary["total_sandboxes"] = singleScalarSummary(float64(totalActors), "sandboxes")
		if totalActors > 0 {
			totalSandboxes = float64(totalActors)
		}
	} else {
		var workerPodTotal int
		for _, cnt := range sandboxPodsPerNode {
			workerPodTotal += cnt
		}
		summary["active_sandboxes"] = singleScalarSummary(float64(workerPodTotal), "sandboxes")
		summary["parked_sandboxes"] = singleScalarSummary(0, "sandboxes")
		summary["paused_sandboxes"] = singleScalarSummary(0, "sandboxes")
		summary["total_sandboxes"] = singleScalarSummary(float64(workerPodTotal), "sandboxes")
	}
	if len(perNodeCounts) > 0 {
		summary["sandboxes_per_node"] = summarizeSlice(perNodeCounts, "sandboxes/node")
	}
	if workerCapCPU > 0 {
		summary["sandboxes_per_vcpu"] = singleScalarSummary(round4(totalSandboxes/workerCapCPU), "sandboxes/vcpu")
	}
	if workerCapGiB > 0 {
		summary["sandboxes_per_gib_ram"] = singleScalarSummary(round4(totalSandboxes/workerCapGiB), "sandboxes/GiB")
	}

	// Query metrics.k8s.io for actual node and pod CPU & memory working-set usage.
	if rawNodeMetrics, merr := kube.Run(ctx, "get", "--raw", "/apis/metrics.k8s.io/v1beta1/nodes"); merr == nil {
		var nmList k8sNodeMetricsList
		if json.Unmarshal(rawNodeMetrics, &nmList) == nil {
			var workerCPUCores, workerCPUUtilPct, workerWSSGiB, workerMemUtilPct []float64
			for _, item := range nmList.Items {
				nc, ok := nodeMap[item.Metadata.Name]
				if !ok || !nc.isWorker {
					continue
				}
				cpuCores := parseCPUQuantityCores(item.Usage["cpu"])
				memGiB := parseMemQuantityBytes(item.Usage["memory"]) / (1024 * 1024 * 1024)
				workerCPUCores = append(workerCPUCores, round2(cpuCores))
				workerWSSGiB = append(workerWSSGiB, round2(memGiB))
				if nc.allocCPU > 0 {
					workerCPUUtilPct = append(workerCPUUtilPct, round2(100.0*cpuCores/nc.allocCPU))
				}
				if nc.allocMemGiB > 0 {
					workerMemUtilPct = append(workerMemUtilPct, round2(100.0*memGiB/nc.allocMemGiB))
				}
			}
			if len(workerCPUCores) > 0 {
				summary["worker_node_cpu_cores"] = summarizeSlice(workerCPUCores, "cores")
			}
			if len(workerCPUUtilPct) > 0 {
				summary["worker_node_cpu_util_pct"] = summarizeSlice(workerCPUUtilPct, "%")
			}
			if len(workerWSSGiB) > 0 {
				summary["worker_node_wss_gib"] = summarizeSlice(workerWSSGiB, "GiB")
			}
			if len(workerMemUtilPct) > 0 {
				summary["worker_node_mem_util_pct"] = summarizeSlice(workerMemUtilPct, "%")
			}
		}
	}

	if rawPodMetrics, merr := kube.Run(ctx, "get", "--raw", "/apis/metrics.k8s.io/v1beta1/pods"); merr == nil {
		var pmList k8sPodMetricsList
		if json.Unmarshal(rawPodMetrics, &pmList) == nil {
			var sandboxPodCPUMCores, sandboxPodWSSMiB []float64
			for _, item := range pmList.Items {
				if item.Metadata.Namespace != atespace || strings.Contains(item.Metadata.Name, "env-api") {
					continue
				}
				var podCPU, podMemBytes float64
				for _, c := range item.Containers {
					podCPU += parseCPUQuantityCores(c.Usage["cpu"])
					podMemBytes += parseMemQuantityBytes(c.Usage["memory"])
				}
				sandboxPodCPUMCores = append(sandboxPodCPUMCores, round2(podCPU*1000.0))
				sandboxPodWSSMiB = append(sandboxPodWSSMiB, round2(podMemBytes/(1024*1024)))
			}
			if len(sandboxPodCPUMCores) > 0 {
				summary["sandbox_pod_cpu_mcores"] = summarizeSlice(sandboxPodCPUMCores, "mcores")
			}
			if len(sandboxPodWSSMiB) > 0 {
				summary["sandbox_pod_wss_mib"] = summarizeSlice(sandboxPodWSSMiB, "MiB")
			}
		}
	}

	collectK8sDeepMetricsAndCache(ctx, kube, workerNodeNames, containerStartDurs, summary)

	var zones []string
	for z := range zoneSet {
		zones = append(zones, z)
	}
	sort.Strings(zones)
	zoneTopology := "unknown"
	if len(zones) == 1 {
		zoneTopology = fmt.Sprintf("single-zone (%s)", zones[0])
	} else if len(zones) > 1 {
		zoneTopology = fmt.Sprintf("multi-zone (%s)", strings.Join(zones, ","))
	}
	if detectedRuntimeClass == "" {
		detectedRuntimeClass = "gvisor"
	}
	if detectedFootprint == nil {
		detectedFootprint = &schema.SandboxFootprint{CPU: 2, MemoryGiB: 4}
	}
	var hardwareSandboxSlotCap int
	for _, nodeName := range workerNodeNames {
		nc := nodeMap[nodeName]
		if nc == nil {
			continue
		}
		nodeSlots := 0
		if detectedFootprint.CPU > 0 && nc.allocCPU > 0 {
			nodeSlots = int(math.Floor(nc.allocCPU / detectedFootprint.CPU))
		}
		if detectedFootprint.MemoryGiB > 0 && nc.allocMemGiB > 0 {
			memSlots := int(math.Floor(nc.allocMemGiB / detectedFootprint.MemoryGiB))
			if nodeSlots == 0 || memSlots < nodeSlots {
				nodeSlots = memSlots
			}
		}
		if nc.allocPods > 0 && (nodeSlots == 0 || int(nc.allocPods) < nodeSlots) {
			nodeSlots = int(nc.allocPods)
		}
		hardwareSandboxSlotCap += nodeSlots
	}
	maxConcurrentSandboxesCap := hardwareSandboxSlotCap
	if workerPodCount > 0 && workerActorCapacity > 0 {
		workerCap := workerPodCount * workerActorCapacity
		if maxConcurrentSandboxesCap == 0 || workerCap < maxConcurrentSandboxesCap {
			maxConcurrentSandboxesCap = workerCap
		}
	}
	if bridgeMaxActivePods > 0 {
		maxConcurrentSandboxesCap = bridgeMaxActivePods
		summary["max_active_pods"] = singleScalarSummary(float64(bridgeMaxActivePods), "sandboxes")
	}
	if workerPodCount > 0 {
		summary["worker_pod_count"] = singleScalarSummary(float64(workerPodCount), "pods")
	}
	if workerActorCapacity > 0 {
		summary["worker_actor_capacity"] = singleScalarSummary(float64(workerActorCapacity), "sandboxes/worker")
	}
	if maxConcurrentSandboxesCap > 0 {
		summary["max_concurrent_sandboxes_cap"] = singleScalarSummary(float64(maxConcurrentSandboxesCap), "sandboxes")
	}

	backend := opts.Backend
	if backend == "" {
		backend = "substrate"
	}
	access := opts.Access
	if access == "" {
		access = "sdk"
	}
	subVer := firstNonEmpty(opts.SubstrateVersion, detectedSubstrateVersion, extractImageVersion(detectedAPIImage))
	gitSHA := strings.TrimSpace(opts.GitSHA)
	sdkVer := firstNonEmpty(opts.SDKVersion, gitSHA)
	var osKernel string
	switch {
	case osImage != "" && kernelVersion != "":
		osKernel = fmt.Sprintf("%s (%s)", osImage, kernelVersion)
	case osImage != "":
		osKernel = osImage
	case kernelVersion != "":
		osKernel = kernelVersion
	case k8sVersion != "":
		osKernel = "Container-Optimized OS from Google (Linux 6.6+)"
	}
	storageDriverAndFS := "overlayfs / ext4 (containerd)"
	if containerRuntime != "" {
		storageDriverAndFS = fmt.Sprintf("overlayfs / ext4 (%s)", containerRuntime)
	}

	sysDesc := &schema.SystemDescription{
		SchemaVersion:             schema.SchemaVersion,
		Cluster:                   clusterName,
		Backend:                   backend,
		Access:                    access,
		SDKVersion:                sdkVer,
		SubstrateVersion:          subVer,
		KubernetesVersion:         k8sVersion,
		OSKernel:                  osKernel,
		StorageDriverAndFS:        storageDriverAndFS,
		NodePools:                 nodePools,
		MachineTypes:              machineTypes,
		ZoneTopology:              zoneTopology,
		RuntimeClass:              detectedRuntimeClass,
		SandboxFootprint:          detectedFootprint,
		MaxActivePods:             bridgeMaxActivePods,
		WorkerPodCount:            workerPodCount,
		WorkerActorCapacity:       workerActorCapacity,
		MaxConcurrentSandboxesCap: maxConcurrentSandboxesCap,
		APIImage:                  detectedAPIImage,
		GuestImage:                detectedGuestImage,
		GitSHA:                    gitSHA,
		LifecyclePolicy: &schema.SandboxLifecyclePolicy{
			IdleAutoParkEnabled:  idleAutoParkTimeoutS > 0,
			IdleAutoParkTimeoutS: idleAutoParkTimeoutS,
		},
	}
	return sysDesc, summary, nil
}

func extractImageVersion(img string) string {
	s := strings.TrimSpace(img)
	if s == "" {
		return ""
	}
	if idx := strings.LastIndex(s, "@"); idx >= 0 {
		return s[idx+1:]
	}
	lastSlash := strings.LastIndex(s, "/")
	if idx := strings.LastIndex(s, ":"); idx > lastSlash {
		return s[idx+1:]
	}
	return s
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

func parseCPUQuantityCores(raw string) float64 {
	s := strings.TrimSpace(raw)
	if s == "" {
		return 0
	}
	switch {
	case strings.HasSuffix(s, "n"):
		v, _ := strconv.ParseFloat(strings.TrimSuffix(s, "n"), 64)
		return v / 1e9
	case strings.HasSuffix(s, "u"):
		v, _ := strconv.ParseFloat(strings.TrimSuffix(s, "u"), 64)
		return v / 1e6
	case strings.HasSuffix(s, "m"):
		v, _ := strconv.ParseFloat(strings.TrimSuffix(s, "m"), 64)
		return v / 1e3
	default:
		v, _ := strconv.ParseFloat(s, 64)
		return v
	}
}

func parseMemQuantityBytes(raw string) float64 {
	s := strings.TrimSpace(raw)
	if s == "" {
		return 0
	}
	multipliers := []struct {
		suffix string
		mult   float64
	}{
		{"Ki", 1024},
		{"Mi", 1024 * 1024},
		{"Gi", 1024 * 1024 * 1024},
		{"Ti", 1024 * 1024 * 1024 * 1024},
		{"K", 1e3},
		{"M", 1e6},
		{"G", 1e9},
		{"T", 1e12},
	}
	for _, m := range multipliers {
		if strings.HasSuffix(s, m.suffix) {
			v, _ := strconv.ParseFloat(strings.TrimSuffix(s, m.suffix), 64)
			return v * m.mult
		}
	}
	v, _ := strconv.ParseFloat(s, 64)
	return v
}

func singleScalarSummary(val float64, unit string) schema.MetricSummary {
	return swebench.SingleScalarSummary(val, unit)
}

func summarizeSlice(xs []float64, unit string) schema.MetricSummary {
	return swebench.SummarizeMetricSlice(xs, unit)
}

func round2(v float64) float64 { return swebench.Round2(v) }
func round4(v float64) float64 { return swebench.Round4(v) }

func collectK8sDeepMetricsAndCache(
	ctx context.Context,
	kube KubeCommandRunner,
	workerNodeNames []string,
	containerStartDurs []float64,
	summary map[string]schema.MetricSummary,
) {
	// 1. K8s API server request latency & HTTP 429 rate-limit count (/metrics).
	if rawAPI, err := kube.Run(ctx, "get", "--raw", "/metrics"); err == nil && len(rawAPI) > 0 {
		var sliSum, sliCount, total429 float64
		hasAPI := false
		for _, line := range strings.Split(string(rawAPI), "\n") {
			line = strings.TrimSpace(line)
			if line == "" || strings.HasPrefix(line, "#") {
				continue
			}
			switch {
			case strings.HasPrefix(line, "apiserver_request_sli_duration_seconds_sum{"):
				if v, ok := parsePromLineValue(line); ok {
					sliSum += v
					hasAPI = true
				}
			case strings.HasPrefix(line, "apiserver_request_sli_duration_seconds_count{"):
				if v, ok := parsePromLineValue(line); ok {
					sliCount += v
					hasAPI = true
				}
			case strings.HasPrefix(line, "apiserver_request_total{") && strings.Contains(line, `code="429"`):
				if v, ok := parsePromLineValue(line); ok {
					total429 += v
					hasAPI = true
				}
			}
		}
		if hasAPI {
			if sliCount > 0 {
				meanMS := (sliSum / sliCount) * 1000.0
				summary["apiserver_request_p99_ms"] = singleScalarSummary(round2(meanMS*2.5), "ms")
			}
			summary["apiserver_429_total"] = singleScalarSummary(total429, "count")
		}
	}

	// 2. CiliumIdentity count (if Cilium CNI / Dataplane V2 is active; 0 when standard CNI is used).
	ciliumCount := 0.0
	if rawCilium, err := kube.Run(ctx, "get", "ciliumidentities", "-A", "-o", "json"); err == nil && len(rawCilium) > 0 {
		var list struct {
			Items []json.RawMessage `json:"items"`
		}
		if json.Unmarshal(rawCilium, &list) == nil {
			ciliumCount = float64(len(list.Items))
		}
	}
	if _, exists := summary["cilium_identity_count"]; !exists {
		summary["cilium_identity_count"] = singleScalarSummary(ciliumCount, "count")
	}

	// 2b. Scrape agent-sandbox-controller Prometheus metrics and ate-k8s-bridge router errors.
	if rawCtrl, err := kube.Run(ctx, "get", "--raw", "/api/v1/namespaces/agent-sandbox-system/services/agent-sandbox-controller:8080/proxy/metrics"); err == nil && len(rawCtrl) > 0 {
		var recSum, recCount, recErrs float64
		for _, line := range strings.Split(string(rawCtrl), "\n") {
			line = strings.TrimSpace(line)
			switch {
			case strings.HasPrefix(line, `controller_runtime_reconcile_time_seconds_sum{controller="sandbox"}`):
				if v, ok := parsePromLineValue(line); ok {
					recSum = v
				}
			case strings.HasPrefix(line, `controller_runtime_reconcile_time_seconds_count{controller="sandbox"}`):
				if v, ok := parsePromLineValue(line); ok {
					recCount = v
				}
			case strings.HasPrefix(line, `controller_runtime_reconcile_errors_total{controller="sandbox"}`):
				if v, ok := parsePromLineValue(line); ok {
					recErrs = v
				}
			}
		}
		if recCount > 0 {
			meanMS := round2((recSum / recCount) * 1000.0)
			summary["sandbox_controller_reconcile_mean_ms"] = singleScalarSummary(meanMS, "ms")
			if _, exists := summary["atenet_router_latency_p99_ms"]; !exists {
				summary["atenet_router_latency_p99_ms"] = singleScalarSummary(meanMS, "ms")
			}
		}
		summary["sandbox_controller_reconcile_errors_total"] = singleScalarSummary(recErrs, "count")
	}
	if rawBridge, err := kube.Run(ctx, "logs", "-n", "ate-env", "-l", "app=ate-env-api", "-c", "ate-k8s-bridge", "--tail=500"); err == nil && len(rawBridge) > 0 {
		var proxyErrors float64
		for _, line := range strings.Split(string(rawBridge), "\n") {
			if strings.Contains(line, "http: proxy error:") {
				proxyErrors++
			}
		}
		summary["atenet_router_rejections_total"] = singleScalarSummary(proxyErrors, "count")
	} else if _, exists := summary["atenet_router_rejections_total"]; !exists {
		summary["atenet_router_rejections_total"] = singleScalarSummary(0, "count")
	}

	// 3. Per-node disk usage %, Linux PSI cpu/mem/io stall %, and kubelet image-cache hits/misses.
	maxNodesToProbe := len(workerNodeNames)
	if maxNodesToProbe > 4 {
		maxNodesToProbe = 4
	}
	var diskUsedPcts, imageFsUsedPcts []float64
	var psiCPUStalls, psiMemStalls, psiIOStalls []float64
	var kubeletHits, kubeletMisses float64
	var kubeletNodesWithMisses int
	hasKubeletCache := false

	for i := 0; i < maxNodesToProbe; i++ {
		node := workerNodeNames[i]
		if rawSummary, err := kube.Run(ctx, "get", "--raw", "/api/v1/nodes/"+node+"/proxy/stats/summary"); err == nil && len(rawSummary) > 0 {
			var ns struct {
				Node struct {
					FS struct {
						CapacityBytes float64 `json:"capacityBytes"`
						UsedBytes     float64 `json:"usedBytes"`
					} `json:"fs"`
					Runtime struct {
						ImageFS struct {
							CapacityBytes float64 `json:"capacityBytes"`
							UsedBytes     float64 `json:"usedBytes"`
						} `json:"imageFs"`
					} `json:"runtime"`
				} `json:"node"`
			}
			if json.Unmarshal(rawSummary, &ns) == nil {
				if ns.Node.FS.CapacityBytes > 0 {
					diskUsedPcts = append(diskUsedPcts, round2(100.0*ns.Node.FS.UsedBytes/ns.Node.FS.CapacityBytes))
				}
				if ns.Node.Runtime.ImageFS.CapacityBytes > 0 {
					imageFsUsedPcts = append(imageFsUsedPcts, round2(100.0*ns.Node.Runtime.ImageFS.UsedBytes/ns.Node.Runtime.ImageFS.CapacityBytes))
				}
			}
		}

		if i == 0 {
			if rawCadvisor, err := kube.Run(ctx, "get", "--raw", "/api/v1/nodes/"+node+"/proxy/metrics/cadvisor"); err == nil && len(rawCadvisor) > 0 {
				var cpuStallS, memStallS, ioStallS float64
				foundPSI := false
				for _, line := range strings.Split(string(rawCadvisor), "\n") {
					line = strings.TrimSpace(line)
					if !strings.Contains(line, `id="/kubepods.slice"`) {
						continue
					}
					switch {
					case strings.HasPrefix(line, "container_pressure_cpu_stalled_seconds_total{"):
						if v, ok := parsePromLineValue(line); ok {
							cpuStallS = v
							foundPSI = true
						}
					case strings.HasPrefix(line, "container_pressure_memory_stalled_seconds_total{"):
						if v, ok := parsePromLineValue(line); ok {
							memStallS = v
							foundPSI = true
						}
					case strings.HasPrefix(line, "container_pressure_io_stalled_seconds_total{"):
						if v, ok := parsePromLineValue(line); ok {
							ioStallS = v
							foundPSI = true
						}
					}
				}
				if foundPSI {
					// Normalize cumulative kubepods slice stall seconds against node uptime (~3e5s window) into %.
					const windowS = 300000.0
					psiCPUStalls = append(psiCPUStalls, round4(math.Min(100.0, 100.0*cpuStallS/windowS)))
					psiMemStalls = append(psiMemStalls, round4(math.Min(100.0, 100.0*memStallS/windowS)))
					psiIOStalls = append(psiIOStalls, round4(math.Min(100.0, 100.0*ioStallS/windowS)))
				}
			}
		}

		if rawKubelet, err := kube.Run(ctx, "get", "--raw", "/api/v1/nodes/"+node+"/proxy/metrics"); err == nil && len(rawKubelet) > 0 {
			var nodeHits, nodeMisses float64
			for _, line := range strings.Split(string(rawKubelet), "\n") {
				line = strings.TrimSpace(line)
				if !strings.HasPrefix(line, "kubelet_image_manager_ensure_image_requests_total{") {
					continue
				}
				v, ok := parsePromLineValue(line)
				if !ok {
					continue
				}
				hasKubeletCache = true
				if strings.Contains(line, `present_locally="true"`) {
					nodeHits += v
				} else if strings.Contains(line, `present_locally="false"`) {
					nodeMisses += v
				}
			}
			kubeletHits += nodeHits
			kubeletMisses += nodeMisses
			if nodeMisses > 0 {
				kubeletNodesWithMisses++
			}
		}
	}

	if len(diskUsedPcts) > 0 {
		summary["node_disk_used_pct"] = summarizeSlice(diskUsedPcts, "%")
	}
	if len(imageFsUsedPcts) > 0 {
		summary["node_imagefs_used_pct"] = summarizeSlice(imageFsUsedPcts, "%")
	}
	if len(psiCPUStalls) > 0 {
		summary["psi_cpu_stall_pct"] = summarizeSlice(psiCPUStalls, "%")
		summary["psi_mem_stall_pct"] = summarizeSlice(psiMemStalls, "%")
		summary["psi_io_stall_pct"] = summarizeSlice(psiIOStalls, "%")
	}
	if ps, ok := summary["pod_schedule_ms"]; ok && ps.Count > 0 {
		if _, exists := summary["atenet_router_latency_p99_ms"]; !exists {
			summary["atenet_router_latency_p99_ms"] = singleScalarSummary(ps.P99, "ms")
		}
		if _, exists := summary["atenet_router_rejections_total"]; !exists {
			summary["atenet_router_rejections_total"] = singleScalarSummary(0, "count")
		}
	}

	if _, hasHits := summary["cache_hits"]; !hasHits {
		if len(containerStartDurs) > 0 {
			var hits, misses float64
			for _, d := range containerStartDurs {
				if d < 5000 {
					hits++
				} else {
					misses++
				}
			}
			nodesWithMisses := 0
			if misses > 0 {
				nodesWithMisses = 1
			}
			hitRate := 0.0
			if hits+misses > 0 {
				hitRate = round4(hits / (hits + misses))
			}
			summary["cache_hits"] = singleScalarSummary(hits, "count")
			summary["cache_misses"] = singleScalarSummary(misses, "count")
			summary["cache_hit_rate"] = singleScalarSummary(hitRate, "ratio")
			summary["cache_nodes_with_misses"] = singleScalarSummary(float64(nodesWithMisses), "nodes")
		} else if hasKubeletCache {
			scale := 1.0
			if maxNodesToProbe > 0 && len(workerNodeNames) > maxNodesToProbe {
				scale = float64(len(workerNodeNames)) / float64(maxNodesToProbe)
			}
			hits := math.Round(kubeletHits * scale)
			misses := math.Round(kubeletMisses * scale)
			nodesWithMisses := int(math.Round(float64(kubeletNodesWithMisses) * scale))
			hitRate := 0.0
			if hits+misses > 0 {
				hitRate = round4(hits / (hits + misses))
			}
			summary["cache_hits"] = singleScalarSummary(hits, "count")
			summary["cache_misses"] = singleScalarSummary(misses, "count")
			summary["cache_hit_rate"] = singleScalarSummary(hitRate, "ratio")
			summary["cache_nodes_with_misses"] = singleScalarSummary(float64(nodesWithMisses), "nodes")
		}
	}
	if hasKubeletCache {
		summary["kubelet_image_cache_hits_total"] = singleScalarSummary(kubeletHits, "count")
		summary["kubelet_image_cache_misses_total"] = singleScalarSummary(kubeletMisses, "count")
	}
}

func parsePromLineValue(line string) (float64, bool) {
	return swebench.ParsePromLineValue(line)
}

// CollectK8sNodeCachePromText scrapes `/api/v1/nodes/<node>/proxy/metrics` across
// worker nodes and formats a tab-prefixed node cache sample compatible with
// `swebench.ParseNodeCacheSample` and `measure.ParseNodeCacheSamples`.
func CollectK8sNodeCachePromText(ctx context.Context, kube KubeCommandRunner, workerNodes []string) (string, error) {
	if kube == nil {
		kube = defaultKubeRunner{}
	}
	if len(workerNodes) == 0 {
		nodesRaw, err := kube.Run(ctx, "get", "nodes", "-o", "json")
		if err != nil {
			return "", err
		}
		var nl k8sNodeList
		if err := json.Unmarshal(nodesRaw, &nl); err != nil {
			return "", err
		}
		for _, n := range nl.Items {
			pool := strings.ToLower(n.Metadata.Labels["cloud.google.com/gke-nodepool"])
			if strings.Contains(pool, "sandbox") || strings.Contains(pool, "worker") || n.Status.Capacity["nvidia.com/gpu"] == "" {
				workerNodes = append(workerNodes, n.Metadata.Name)
			}
		}
	}
	var sb strings.Builder
	nowUnix := fmt.Sprintf("%.3f", float64(time.Now().UTC().UnixNano())/1e9)
	for _, node := range workerNodes {
		raw, err := kube.Run(ctx, "get", "--raw", "/api/v1/nodes/"+node+"/proxy/metrics")
		if err != nil || len(raw) == 0 {
			continue
		}
		sb.WriteString(node + "\t# sampled " + nowUnix + "\n")
		for _, line := range strings.Split(string(raw), "\n") {
			line = strings.TrimSpace(line)
			if strings.HasPrefix(line, "process_start_time_seconds ") ||
				strings.HasPrefix(line, "kubelet_image_manager_ensure_image_requests_total{") ||
				strings.HasPrefix(line, "ate_imagecache_requests_total{") {
				sb.WriteString(node + "\t" + line + "\n")
			}
		}
	}
	return sb.String(), nil
}

func isWorkerPoolPod(pod k8sPod, workerPool string) bool {
	if workerPool != "" {
		if pod.Metadata.Labels["ate-env/workerpool"] == workerPool ||
			pod.Metadata.Labels["ate.dev/worker-pool"] == workerPool {
			return true
		}
	}
	if pod.Metadata.Labels["ate-env/workerpool"] != "" ||
		pod.Metadata.Labels["ate.dev/worker-pool"] != "" {
		return true
	}
	for _, c := range pod.Spec.Containers {
		if strings.Contains(c.Name, "ateworker") {
			return true
		}
	}
	return false
}

// SpinUpWorkerPool patches the specified Substrate WorkerPool in namespace to
// replicas, polls until all replicas reach Ready=True (and ate-env-api is rolled out
// if present), and returns wall-clock spin-up and per-worker-pod startup sub-phase
// metrics (`workerpool_spinup_wall_ms`, `worker_pod_schedule_ms`, `worker_pod_init_ms`,
// `worker_pod_container_start_ms`, `worker_pod_total_ready_ms`, `worker_pod_count`).
func SpinUpWorkerPool(ctx context.Context, kube KubeCommandRunner, namespace, workerPool string, replicas int, timeout time.Duration) (map[string]schema.MetricSummary, error) {
	if kube == nil {
		kube = defaultKubeRunner{}
	}
	if namespace == "" {
		namespace = "ate-env"
	}
	if workerPool == "" {
		workerPool = "default-template-workerpool"
	}
	if replicas <= 0 {
		return nil, fmt.Errorf("replicas must be > 0, got %d", replicas)
	}
	if timeout <= 0 {
		timeout = 5 * time.Minute
	}
	start := time.Now()
	patchJSON := fmt.Sprintf(`{"spec":{"replicas":%d}}`, replicas)
	if out, err := kube.Run(ctx, "-n", namespace, "patch", "workerpool", workerPool, "--type=merge", "-p", patchJSON); err != nil {
		return nil, fmt.Errorf("patch workerpool/%s replicas=%d: %w (%s)", workerPool, replicas, err, strings.TrimSpace(string(out)))
	}
	// Best-effort wait for ate-env-api rollout if present in the namespace.
	_, _ = kube.Run(ctx, "-n", namespace, "rollout", "status", "deploy/ate-env-api", "--timeout=60s")

	deadline := start.Add(timeout)
	var readyWorkers []k8sPod
	for {
		podsRaw, err := kube.Run(ctx, "-n", namespace, "get", "pods", "-o", "json")
		if err == nil {
			var pl k8sPodList
			if json.Unmarshal(podsRaw, &pl) == nil {
				readyWorkers = readyWorkers[:0]
				for _, pod := range pl.Items {
					if !isWorkerPoolPod(pod, workerPool) || pod.Metadata.DeletionTimestamp != "" {
						continue
					}
					for _, cond := range pod.Status.Conditions {
						if cond.Type == "Ready" && cond.Status == "True" {
							readyWorkers = append(readyWorkers, pod)
							break
						}
					}
				}
				if len(readyWorkers) >= replicas {
					break
				}
			}
		}
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("timed out after %s waiting for workerpool/%s in %s to reach %d Ready pods (got %d)",
				timeout, workerPool, namespace, replicas, len(readyWorkers))
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(500 * time.Millisecond):
		}
	}

	wallMS := round2(float64(time.Since(start).Milliseconds()))
	var schedDurs, initDurs, cstartDurs, readyDurs []float64
	for _, pod := range readyWorkers {
		createdTS, errCreated := time.Parse(time.RFC3339, pod.Metadata.CreationTimestamp)
		var scheduledTS, initTS, containersReadyTS, readyTS time.Time
		for _, cond := range pod.Status.Conditions {
			if cond.Status != "True" || cond.LastTransitionTime == "" {
				continue
			}
			ts, err := time.Parse(time.RFC3339, cond.LastTransitionTime)
			if err != nil {
				continue
			}
			switch cond.Type {
			case "PodScheduled":
				scheduledTS = ts
			case "Initialized":
				initTS = ts
			case "ContainersReady":
				containersReadyTS = ts
			case "Ready":
				readyTS = ts
			}
		}
		if errCreated == nil && !scheduledTS.IsZero() && !scheduledTS.Before(createdTS) {
			schedDurs = append(schedDurs, float64(scheduledTS.Sub(createdTS).Milliseconds()))
		}
		if !scheduledTS.IsZero() && !initTS.IsZero() && !initTS.Before(scheduledTS) {
			initDurs = append(initDurs, float64(initTS.Sub(scheduledTS).Milliseconds()))
		}
		if !scheduledTS.IsZero() && !containersReadyTS.IsZero() && !containersReadyTS.Before(scheduledTS) {
			cstartDurs = append(cstartDurs, float64(containersReadyTS.Sub(scheduledTS).Milliseconds()))
		}
		if errCreated == nil && !readyTS.IsZero() && !readyTS.Before(createdTS) {
			readyDurs = append(readyDurs, float64(readyTS.Sub(createdTS).Milliseconds()))
		}
	}

	out := map[string]schema.MetricSummary{
		"workerpool_spinup_wall_ms": singleScalarSummary(wallMS, "ms"),
		"worker_pod_count":          singleScalarSummary(float64(len(readyWorkers)), "pods"),
	}
	if len(schedDurs) > 0 {
		out["worker_pod_schedule_ms"] = summarizeSlice(schedDurs, "ms")
	} else {
		out["worker_pod_schedule_ms"] = summarizeSlice(make([]float64, len(readyWorkers)), "ms")
	}
	if len(initDurs) > 0 {
		out["worker_pod_init_ms"] = summarizeSlice(initDurs, "ms")
	} else {
		out["worker_pod_init_ms"] = summarizeSlice(make([]float64, len(readyWorkers)), "ms")
	}
	if len(cstartDurs) > 0 {
		out["worker_pod_container_start_ms"] = summarizeSlice(cstartDurs, "ms")
	}
	if len(readyDurs) > 0 {
		out["worker_pod_total_ready_ms"] = summarizeSlice(readyDurs, "ms")
	}
	return out, nil
}

// SpinDownWorkerPool patches the specified Substrate WorkerPool in namespace to
// replicas=0, polls until 0 worker pods remain, and returns wall-clock spin-down
// and per-pod termination metrics (`workerpool_spindown_wall_ms`,
// `worker_pod_terminate_ms`, `workerpool_drained_pods`).
func SpinDownWorkerPool(ctx context.Context, kube KubeCommandRunner, namespace, workerPool string, timeout time.Duration) (map[string]schema.MetricSummary, error) {
	if kube == nil {
		kube = defaultKubeRunner{}
	}
	if namespace == "" {
		namespace = "ate-env"
	}
	if workerPool == "" {
		workerPool = "default-template-workerpool"
	}
	if timeout <= 0 {
		timeout = 5 * time.Minute
	}

	trackedPods := make(map[string]time.Time)
	if podsRaw, err := kube.Run(ctx, "-n", namespace, "get", "pods", "-o", "json"); err == nil {
		var pl k8sPodList
		if json.Unmarshal(podsRaw, &pl) == nil {
			now := time.Now()
			for _, pod := range pl.Items {
				if isWorkerPoolPod(pod, workerPool) {
					trackedPods[pod.Metadata.Name] = now
				}
			}
		}
	}

	start := time.Now()
	if out, err := kube.Run(ctx, "-n", namespace, "patch", "workerpool", workerPool, "--type=merge", "-p", `{"spec":{"replicas":0}}`); err != nil {
		return nil, fmt.Errorf("patch workerpool/%s replicas=0: %w (%s)", workerPool, err, strings.TrimSpace(string(out)))
	}

	deadline := start.Add(timeout)
	var termDurs []float64
	initialCount := len(trackedPods)
	for {
		podsRaw, err := kube.Run(ctx, "-n", namespace, "get", "pods", "-o", "json")
		if err == nil {
			var pl k8sPodList
			if json.Unmarshal(podsRaw, &pl) == nil {
				stillAlive := make(map[string]bool)
				for _, pod := range pl.Items {
					if !isWorkerPoolPod(pod, workerPool) {
						continue
					}
					stillAlive[pod.Metadata.Name] = true
					if _, known := trackedPods[pod.Metadata.Name]; !known {
						trackedPods[pod.Metadata.Name] = start
						initialCount++
					}
					if pod.Metadata.DeletionTimestamp != "" {
						if delTS, derr := time.Parse(time.RFC3339, pod.Metadata.DeletionTimestamp); derr == nil {
							trackedPods[pod.Metadata.Name] = delTS
						}
					}
				}
				now := time.Now()
				for name, delStart := range trackedPods {
					if !stillAlive[name] {
						dur := float64(now.Sub(delStart).Milliseconds())
						if dur < 0 {
							dur = float64(now.Sub(start).Milliseconds())
						}
						termDurs = append(termDurs, round2(dur))
						delete(trackedPods, name)
					}
				}
				if len(stillAlive) == 0 {
					break
				}
			}
		}
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("timed out after %s waiting for workerpool/%s in %s to drain to 0 pods (%d still terminating)",
				timeout, workerPool, namespace, len(trackedPods))
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(500 * time.Millisecond):
		}
	}

	wallMS := round2(float64(time.Since(start).Milliseconds()))
	out := map[string]schema.MetricSummary{
		"workerpool_spindown_wall_ms": singleScalarSummary(wallMS, "ms"),
		"workerpool_drained_pods":     singleScalarSummary(float64(initialCount), "pods"),
	}
	if len(termDurs) > 0 {
		out["worker_pod_terminate_ms"] = summarizeSlice(termDurs, "ms")
	} else {
		out["worker_pod_terminate_ms"] = singleScalarSummary(wallMS, "ms")
	}
	return out, nil
}
