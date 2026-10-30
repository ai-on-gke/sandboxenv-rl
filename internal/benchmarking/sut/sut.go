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

// Package sut provides System Under Test (SUT) manifest rendering, provisioning,
// system description recording, and server-authoritative resource reaping for
// RL benchmark runs.
package sut

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	"github.com/agent-substrate/env/internal/benchmarking/schema"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"sigs.k8s.io/yaml"
)

const (
	// DefaultAtespace is the default Substrate atespace and Kubernetes namespace for ate-env-api.
	DefaultAtespace = "ate-env"
	// DefaultReplicas is the default number of ate-env-api replicas when not specified.
	DefaultReplicas = 2
	// DefaultAPIPort is the default TCP port exposed by ate-env-api.
	DefaultAPIPort = 7777
	// DefaultAPIImage is the default container image used for ate-env-api in rendered manifests.
	DefaultAPIImage = "ate-env-api:latest"
	// DefaultManifestFilename is the file written inside ArtifactDir during RenderOnly provisioning.
	DefaultManifestFilename = "sut-manifest.yaml"

	apiName        = "ate-env-api"
	apiAccessLabel = "ate.dev/api-access"
	nodePoolLabel  = "cloud.google.com/gke-nodepool"
)

// RenderManifest renders a deterministic multi-document Kubernetes YAML manifest
// (Namespace, Deployment, and Service for ate-env-api) from spec.
func RenderManifest(spec schema.SUTSpec) ([]byte, error) {
	if err := spec.Validate(); err != nil {
		return nil, fmt.Errorf("invalid sut spec: %w", err)
	}
	ns := ""
	if spec.Substrate != nil {
		ns = spec.Substrate.Atespace
	}
	if ns == "" {
		ns = DefaultAtespace
	}
	replicas := int32(spec.Replicas)
	if replicas == 0 {
		replicas = DefaultReplicas
	}

	apiImage := DefaultAPIImage
	args := []string{
		"-listen", fmt.Sprintf("0.0.0.0:%d", DefaultAPIPort),
		"-ateapi-token-file=/var/run/secrets/ateapi/token",
	}
	if len(spec.ExtraArgs) > 0 {
		keys := make([]string, 0, len(spec.ExtraArgs))
		for k := range spec.ExtraArgs {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			v := spec.ExtraArgs[k]
			if k == "api_image" && v != "" {
				apiImage = v
				continue
			}
			flagName := k
			if !strings.HasPrefix(flagName, "-") {
				flagName = "-" + flagName
			}
			args = append(args, fmt.Sprintf("%s=%s", flagName, v))
		}
	}

	namespaceObj := &corev1.Namespace{
		TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Namespace"},
		ObjectMeta: metav1.ObjectMeta{
			Name:   ns,
			Labels: map[string]string{apiAccessLabel: "true"},
		},
	}

	labels := map[string]string{"app": apiName}
	automount := false
	tokenExpiration := int64(7200)

	podSpec := corev1.PodSpec{
		ServiceAccountName:           apiName,
		AutomountServiceAccountToken: &automount,
		Containers: []corev1.Container{{
			Name:  apiName,
			Image: apiImage,
			Args:  args,
			Ports: []corev1.ContainerPort{{ContainerPort: DefaultAPIPort}},
			VolumeMounts: []corev1.VolumeMount{{
				Name:      "ateapi-token",
				MountPath: "/var/run/secrets/ateapi",
				ReadOnly:  true,
			}},
			ReadinessProbe: &corev1.Probe{
				ProbeHandler: corev1.ProbeHandler{
					HTTPGet: &corev1.HTTPGetAction{
						Path: "/healthz",
						Port: intstr.FromInt32(DefaultAPIPort),
					},
				},
			},
		}},
		Volumes: []corev1.Volume{{
			Name: "ateapi-token",
			VolumeSource: corev1.VolumeSource{
				Projected: &corev1.ProjectedVolumeSource{
					Sources: []corev1.VolumeProjection{{
						ServiceAccountToken: &corev1.ServiceAccountTokenProjection{
							Audience:          "api.ate-system.svc",
							ExpirationSeconds: &tokenExpiration,
							Path:              "token",
						},
					}},
				},
			},
		}},
	}
	if spec.RuntimeClass != "" {
		rc := spec.RuntimeClass
		podSpec.RuntimeClassName = &rc
	}
	if spec.NodePool != "" {
		podSpec.NodeSelector = map[string]string{nodePoolLabel: spec.NodePool}
	}

	deploymentObj := &appsv1.Deployment{
		TypeMeta: metav1.TypeMeta{APIVersion: "apps/v1", Kind: "Deployment"},
		ObjectMeta: metav1.ObjectMeta{
			Name:      apiName,
			Namespace: ns,
			Labels:    labels,
		},
		Spec: appsv1.DeploymentSpec{
			Replicas: &replicas,
			Selector: &metav1.LabelSelector{MatchLabels: labels},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: labels},
				Spec:       podSpec,
			},
		},
	}

	serviceObj := &corev1.Service{
		TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Service"},
		ObjectMeta: metav1.ObjectMeta{
			Name:      apiName,
			Namespace: ns,
			Labels:    labels,
		},
		Spec: corev1.ServiceSpec{
			Selector: labels,
			Ports: []corev1.ServicePort{{
				Port:       DefaultAPIPort,
				TargetPort: intstr.FromInt32(DefaultAPIPort),
			}},
		},
	}

	docs := []any{namespaceObj, deploymentObj, serviceObj}
	var buf bytes.Buffer
	for i, doc := range docs {
		rawJSON, err := json.Marshal(doc)
		if err != nil {
			return nil, fmt.Errorf("marshal k8s object %d: %w", i, err)
		}
		var m map[string]any
		if err := json.Unmarshal(rawJSON, &m); err != nil {
			return nil, fmt.Errorf("unmarshal k8s object %d: %w", i, err)
		}
		delete(m, "status")
		pruneEmpty(m)
		yamlBytes, err := yaml.Marshal(m)
		if err != nil {
			return nil, fmt.Errorf("marshal yaml %d: %w", i, err)
		}
		if i > 0 {
			buf.WriteString("---\n")
		}
		buf.Write(yamlBytes)
	}
	return buf.Bytes(), nil
}

func pruneEmpty(v any) any {
	switch val := v.(type) {
	case map[string]any:
		for k, child := range val {
			cleaned := pruneEmpty(child)
			if cleaned == nil {
				delete(val, k)
				continue
			}
			if m, ok := cleaned.(map[string]any); ok && len(m) == 0 {
				delete(val, k)
				continue
			}
			val[k] = cleaned
		}
		if len(val) == 0 {
			return nil
		}
		return val
	case []any:
		for i, elem := range val {
			val[i] = pruneEmpty(elem)
		}
		return val
	default:
		return val
	}
}

// ManifestApplier applies rendered Kubernetes YAML manifests to a cluster.
type ManifestApplier interface {
	Apply(ctx context.Context, manifest []byte) error
}

// KubectlApplier implements ManifestApplier by piping the manifest to `kubectl apply -f -`.
type KubectlApplier struct {
	KubectlBin string
	Kubeconfig string
	Context    string
}

// Apply pipes manifest to `kubectl apply -f -`.
func (k KubectlApplier) Apply(ctx context.Context, manifest []byte) error {
	bin := k.KubectlBin
	if bin == "" {
		bin = "kubectl"
	}
	var args []string
	if k.Kubeconfig != "" {
		args = append(args, "--kubeconfig", k.Kubeconfig)
	}
	if k.Context != "" {
		args = append(args, "--context", k.Context)
	}
	args = append(args, "apply", "-f", "-")
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Stdin = bytes.NewReader(manifest)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("kubectl apply failed: %w: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// ProvisionOptions configures SUT provisioning or render-only output.
type ProvisionOptions struct {
	Spec        schema.SUTSpec
	ArtifactDir string
	RenderOnly  bool
}

// Provisioner renders and optionally applies SUT Kubernetes manifests.
type Provisioner interface {
	Provision(ctx context.Context, opts ProvisionOptions) ([]byte, error)
}

type defaultProvisioner struct {
	applier ManifestApplier
}

// NewProvisioner returns a Provisioner backed by applier.
func NewProvisioner(applier ManifestApplier) Provisioner {
	return &defaultProvisioner{applier: applier}
}

// Provision renders the SUT manifest for opts.Spec, writes it to
// $ARTIFACT_DIR/sut-manifest.yaml when ArtifactDir is set, and applies it via
// the configured ManifestApplier unless opts.RenderOnly is true.
func (p *defaultProvisioner) Provision(ctx context.Context, opts ProvisionOptions) ([]byte, error) {
	manifest, err := RenderManifest(opts.Spec)
	if err != nil {
		return nil, err
	}
	if opts.ArtifactDir != "" {
		if err := os.MkdirAll(opts.ArtifactDir, 0o755); err != nil {
			return nil, fmt.Errorf("mkdir artifact dir %q: %w", opts.ArtifactDir, err)
		}
		outPath := filepath.Join(opts.ArtifactDir, DefaultManifestFilename)
		if err := os.WriteFile(outPath, manifest, 0o644); err != nil {
			return nil, fmt.Errorf("write sut manifest %q: %w", outPath, err)
		}
	} else if opts.RenderOnly {
		return nil, fmt.Errorf("artifact_dir is required when render_only is true")
	}
	if opts.RenderOnly {
		return manifest, nil
	}
	if p.applier == nil {
		return nil, fmt.Errorf("manifest applier is required when render_only is false")
	}
	if err := p.applier.Apply(ctx, manifest); err != nil {
		return nil, err
	}
	return manifest, nil
}

// BuildSystemDescription constructs a schema.SystemDescription from the SUTSpec and cluster provenance.
func BuildSystemDescription(spec schema.SUTSpec, cluster, k8sVersion, gitSHA, datasetDigest string, nodePools map[string]int) schema.SystemDescription {
	var poolsCopy map[string]int
	if len(nodePools) > 0 {
		poolsCopy = make(map[string]int, len(nodePools))
		for k, v := range nodePools {
			poolsCopy[k] = v
		}
	}
	return schema.SystemDescription{
		SchemaVersion:     schema.SchemaVersion,
		Cluster:           cluster,
		KubernetesVersion: k8sVersion,
		NodePools:         poolsCopy,
		RuntimeClass:      spec.RuntimeClass,
		MaxActivePods:     spec.MaxActivePods,
		PoolSizePerImage:  spec.PoolSizePerImg,
		APIImage:          spec.ExtraArgs["api_image"],
		GuestImage:        spec.ExtraArgs["guest_image"],
		GitSHA:            gitSHA,
		DatasetDigest:     datasetDigest,
	}
}

// WriteSystemDescription validates desc and writes it as indented JSON to path.
func WriteSystemDescription(path string, desc schema.SystemDescription) error {
	if err := desc.Validate(); err != nil {
		return err
	}
	data, err := json.MarshalIndent(desc, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal system description: %w", err)
	}
	data = append(data, '\n')
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return fmt.Errorf("mkdir %q: %w", dir, err)
		}
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		return fmt.Errorf("write system description %q: %w", path, err)
	}
	return nil
}
