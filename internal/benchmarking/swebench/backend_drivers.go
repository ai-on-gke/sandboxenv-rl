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

package swebench

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/url"
	"strings"
	"time"

	env "github.com/agent-substrate/env/clients/go"
	"github.com/agent-substrate/env/internal/apiservice"
	"github.com/agent-substrate/env/internal/ate"
	ateenvv1alpha "github.com/agent-substrate/env/proto/ateenv/v1alpha"
	"go.ate.dev/substrate/pkg/proto/ateapipb"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
)

// BackendDriverOptions configures NewBackendDriver across the supported
// (-backend, -access) combinations:
//   - Backend: "substrate" (default) or "agent-sandbox"
//   - Access:  "ate-env-api" (default), "direct", or "sdk"
type BackendDriverOptions struct {
	Backend      string
	Access       string
	Endpoint     string
	ControlAddr  string
	RouterAddr   string
	Atespace     string
	BaseTemplate string
	WarmPool     string
	Connections  int
	AuthToken    string
}

type nopCloser struct{}

func (nopCloser) Close() error { return nil }

// ValidateBackendDriverFlags validates the (-backend, -access) flag values.
func ValidateBackendDriverFlags(backend, access string) error {
	switch backend {
	case "", "substrate":
		switch access {
		case "", "ate-env-api", "direct", "sdk":
			return nil
		default:
			return fmt.Errorf("unsupported -access %q for -backend %q (want ate-env-api, direct, or sdk)", access, backend)
		}
	case "agent-sandbox":
		switch access {
		case "", "ate-env-api", "direct", "sdk":
			return nil
		default:
			return fmt.Errorf("unsupported -access %q for -backend %q (want sdk, direct, or ate-env-api)", access, backend)
		}
	default:
		return fmt.Errorf("unsupported -backend %q (want substrate or agent-sandbox)", backend)
	}
}

// NewBackendDriver constructs the requested Backend implementation and an
// io.Closer for releasing its underlying connections.
func NewBackendDriver(opts BackendDriverOptions) (Backend, io.Closer, error) {
	if err := ValidateBackendDriverFlags(opts.Backend, opts.Access); err != nil {
		return nil, nil, err
	}
	switch opts.Backend {
	case "agent-sandbox":
		b, err := NewAgentSandboxBackend(AgentSandboxOptions{
			Endpoint:  opts.Endpoint,
			Namespace: opts.Atespace,
			WarmPool:  opts.WarmPool,
			AuthToken: opts.AuthToken,
		})
		if err != nil {
			return nil, nil, err
		}
		return b, nopCloser{}, nil

	default: // "substrate" or ""
		if opts.Access == "direct" {
			ctrl := opts.ControlAddr
			if ctrl == "" {
				ctrl = opts.Endpoint
			}
			b, err := NewDirectSubstrateBackend(DirectSubstrateOptions{
				ATEOptions: ate.Options{
					ControlAddr: ctrl,
					RouterAddr:  opts.RouterAddr,
					SkipVerify:  true,
				},
				Atespace:     opts.Atespace,
				BaseTemplate: opts.BaseTemplate,
			})
			if err != nil {
				return nil, nil, err
			}
			return b, b, nil
		}
		b, err := NewEnvBackend(opts.Endpoint, opts.Atespace, opts.BaseTemplate, opts.Connections, opts.AuthToken)
		if err != nil {
			return nil, nil, err
		}
		return b, b, nil
	}
}

// DirectSubstrateOptions configures a DirectSubstrateBackend that talks
// directly to the Substrate control plane and atenet router without routing
// through ate-env-api (isolating API-proxy overhead per DESIGN.md §4/§5.4).
type DirectSubstrateOptions struct {
	ATEOptions   ate.Options
	Atespace     string
	BaseTemplate string
	MinPoll      time.Duration
	MaxPoll      time.Duration
}

// DirectSubstrateBackend implements Backend, ForkBackend, and Prewarmer
// directly against Substrate's control plane and atenet router.
type DirectSubstrateBackend struct {
	client           *ate.Client
	controlConn      *grpc.ClientConn
	control          ateapipb.ControlClient
	ownsClient       bool
	atespace         string
	baseTemplate     string
	minPoll, maxPoll time.Duration
}

// NewDirectSubstrateBackend dials the Substrate control plane and router.
func NewDirectSubstrateBackend(opts DirectSubstrateOptions) (*DirectSubstrateBackend, error) {
	c, err := ate.New(opts.ATEOptions)
	if err != nil {
		return nil, err
	}
	var creds credentials.TransportCredentials
	if opts.ATEOptions.TLSConfig != nil {
		creds = credentials.NewTLS(opts.ATEOptions.TLSConfig)
	} else {
		creds = credentials.NewTLS(&tls.Config{InsecureSkipVerify: opts.ATEOptions.SkipVerify})
	}
	controlConn, err := grpc.NewClient(opts.ATEOptions.ControlAddr, grpc.WithTransportCredentials(creds))
	if err != nil {
		_ = c.Close()
		return nil, err
	}
	b := NewDirectSubstrateBackendFromClient(c, opts.Atespace, opts.BaseTemplate, opts.MinPoll, opts.MaxPoll)
	b.controlConn = controlConn
	b.control = ateapipb.NewControlClient(controlConn)
	b.ownsClient = true
	return b, nil
}

// NewDirectSubstrateBackendFromClient wraps an existing ate.Client as a
// DirectSubstrateBackend.
func NewDirectSubstrateBackendFromClient(client *ate.Client, atespace, baseTemplate string, minPoll, maxPoll time.Duration) *DirectSubstrateBackend {
	if atespace == "" {
		atespace = ate.DefaultAtespace
	}
	if baseTemplate == "" {
		baseTemplate = apiservice.DefaultTemplate
	}
	return &DirectSubstrateBackend{
		client:       client,
		atespace:     atespace,
		baseTemplate: baseTemplate,
		minPoll:      minPoll,
		maxPoll:      maxPoll,
	}
}

// Close releases the underlying control-plane and guest connections if owned.
func (b *DirectSubstrateBackend) Close() error {
	if b.controlConn != nil {
		_ = b.controlConn.Close()
	}
	if b.ownsClient && b.client != nil {
		return b.client.Close()
	}
	return nil
}

// ensureImageTemplate resolves or derives the per-image ActorTemplate on the
// Substrate control plane. If the base template has not been registered on the
// control plane (for example in minimal fake-control tests), it falls back to
// b.baseTemplate so actor creation still proceeds.
func (b *DirectSubstrateBackend) ensureImageTemplate(ctx context.Context, image string) (string, error) {
	if image == "" {
		return b.baseTemplate, nil
	}
	name, err := apiservice.ImageTemplateName(b.baseTemplate, image)
	if err != nil {
		return "", err
	}
	if _, err := b.client.GetActorTemplate(ctx, b.atespace, name); err == nil {
		return name, nil
	} else if !errors.Is(err, ate.ErrNotFound) {
		return "", err
	}
	baseTmpl, err := b.client.GetActorTemplate(ctx, b.atespace, b.baseTemplate)
	if err != nil {
		if errors.Is(err, ate.ErrNotFound) {
			return b.baseTemplate, nil
		}
		return "", err
	}
	derived, err := apiservice.DeriveImageTemplate(baseTmpl, name, image)
	if err != nil {
		return "", err
	}
	if _, err := b.client.CreateActorTemplate(ctx, derived); err != nil && !errors.Is(err, ate.ErrAlreadyExists) {
		return "", err
	}
	return name, nil
}

// Prewarm ensures derived ActorTemplates exist for each image in images.
func (b *DirectSubstrateBackend) Prewarm(ctx context.Context, images []string) error {
	for _, img := range images {
		if _, err := b.ensureImageTemplate(ctx, img); err != nil {
			return fmt.Errorf("prewarm %s: %w", img, err)
		}
	}
	return nil
}

// Create registers and starts an actor directly on the Substrate control plane.
func (b *DirectSubstrateBackend) Create(ctx context.Context, id, image string) (Sandbox, error) {
	tmplName, err := b.ensureImageTemplate(ctx, image)
	if err != nil {
		return nil, fmt.Errorf("create %s: %w", id, err)
	}
	if err := b.client.Create(ctx, ate.CreateOptions{
		ID:       id,
		Atespace: b.atespace,
		Template: tmplName,
	}); err != nil {
		return nil, fmt.Errorf("create %s: %w", id, err)
	}
	return &directSubstrateSandbox{b: b, id: id}, nil
}

func latestActorSnapshotID(actor *ateapipb.Actor) string {
	snaps := actor.GetStatus().GetSnapshots()
	if len(snaps) == 0 {
		return ""
	}
	latest := snaps[0]
	for _, s := range snaps[1:] {
		if s.GetGeneration() >= latest.GetGeneration() {
			latest = s
		}
	}
	return latest.GetUuid()
}

// Fork creates a new actor seeded from the latest snapshot of fromID.
func (b *DirectSubstrateBackend) Fork(ctx context.Context, fromID, newID string) (Sandbox, error) {
	if b.control == nil {
		return nil, fmt.Errorf("fork %s -> %s: direct control client is not configured", fromID, newID)
	}
	src, err := b.client.Get(ctx, b.atespace, fromID)
	if err != nil {
		return nil, fmt.Errorf("fork %s -> %s: %w", fromID, newID, err)
	}
	snapID := latestActorSnapshotID(src)
	if snapID == "" {
		if err := b.SuspendWithMode(ctx, fromID, true); err != nil {
			return nil, fmt.Errorf("fork %s -> %s: %w", fromID, newID, err)
		}
		src, err = b.client.Get(ctx, b.atespace, fromID)
		if err != nil {
			return nil, fmt.Errorf("fork %s -> %s: %w", fromID, newID, err)
		}
		snapID = latestActorSnapshotID(src)
	}
	tmplName := src.GetActorTemplate().GetName()
	if tmplName == "" {
		tmplName = b.baseTemplate
	}
	actor := &ateapipb.Actor{
		Metadata:      &ateapipb.ResourceMetadata{Atespace: b.atespace, Name: newID},
		ActorTemplate: &ateapipb.ObjectRef{Atespace: b.atespace, Name: tmplName},
	}
	if snapID != "" {
		actor.SourceTag = &ateapipb.ObjectRef{Atespace: b.atespace, Name: snapID}
	}
	if _, err := b.control.CreateActor(ctx, &ateapipb.CreateActorRequest{Actor: actor}); err != nil {
		return nil, fmt.Errorf("fork %s -> %s: %w", fromID, newID, err)
	}
	return &directSubstrateSandbox{b: b, id: newID}, nil
}

// SuspendWithMode suspends actor id using asynchronous or synchronous
// durability mode.
func (b *DirectSubstrateBackend) SuspendWithMode(ctx context.Context, id string, async bool) error {
	if async && b.control != nil {
		_, err := b.control.SuspendActor(ctx, &ateapipb.SuspendActorRequest{
			Actor:   &ateapipb.ObjectRef{Atespace: b.atespace, Name: id},
			Options: &ateapipb.SuspendOptions{Mode: ateapipb.DurabilityMode_DURABILITY_MODE_ASYNCHRONOUS},
		})
		return err
	}
	return b.client.Suspend(ctx, b.atespace, id)
}

// Delete suspends and deletes actor id directly on the Substrate control plane.
func (b *DirectSubstrateBackend) Delete(ctx context.Context, id string) error {
	return b.client.Delete(ctx, b.atespace, id)
}

type directSubstrateSandbox struct {
	b  *DirectSubstrateBackend
	id string
}

func (s *directSubstrateSandbox) Run(ctx context.Context, command string, timeout time.Duration) (Result, error) {
	conn, err := s.b.client.DialGuest(s.b.atespace, s.id)
	if err != nil {
		return Result{}, err
	}
	procClient := ateenvv1alpha.NewProcessServiceClient(conn)
	return runPolledProcess(ctx, command, timeout, polledProcessOps{
		minPoll: s.b.minPoll,
		maxPoll: s.b.maxPoll,
		start: func(ctx context.Context, req *ateenvv1alpha.StartProcessRequest) (string, error) {
			proc, err := procClient.StartProcess(ctx, req)
			if err != nil {
				return "", err
			}
			return proc.GetProcessId(), nil
		},
		poll: func(ctx context.Context, procID string) (*ateenvv1alpha.Process, error) {
			return procClient.GetProcess(ctx, &ateenvv1alpha.GetProcessRequest{ProcessId: procID})
		},
		kill: func(ctx context.Context, procID string) error {
			_, _ = procClient.SignalProcess(ctx, &ateenvv1alpha.SignalProcessRequest{
				ProcessId: procID,
				Signal:    ateenvv1alpha.Signal_SIGNAL_KILL,
			})
			return nil
		},
		output: func(ctx context.Context, procID string) (processOutputStream, error) {
			return procClient.StreamProcessOutput(ctx, &ateenvv1alpha.StreamProcessOutputRequest{ProcessId: procID})
		},
	})
}

func (s *directSubstrateSandbox) WriteFile(ctx context.Context, path string, content []byte, mode fs.FileMode) error {
	conn, err := s.b.client.DialGuest(s.b.atespace, s.id)
	if err != nil {
		return err
	}
	fsClient := ateenvv1alpha.NewFileSystemServiceClient(conn)
	stream, err := fsClient.WriteFile(ctx)
	if err != nil {
		return err
	}
	if err := stream.Send(&ateenvv1alpha.WriteFileRequest{
		Path:  path,
		Mode:  uint32(mode.Perm()),
		Chunk: content,
	}); err != nil {
		return err
	}
	_, err = stream.CloseAndRecv()
	return err
}

func (s *directSubstrateSandbox) Suspend(ctx context.Context) error {
	return s.SuspendWithMode(ctx, false)
}

func (s *directSubstrateSandbox) SuspendAsync(ctx context.Context) error {
	return s.SuspendWithMode(ctx, true)
}

func (s *directSubstrateSandbox) SuspendWithMode(ctx context.Context, async bool) error {
	return s.b.SuspendWithMode(ctx, s.id, async)
}

func (s *directSubstrateSandbox) Fork(ctx context.Context, newID string) (Sandbox, error) {
	return s.b.Fork(ctx, s.id, newID)
}

func (s *directSubstrateSandbox) Delete(ctx context.Context) error {
	return s.b.Delete(ctx, s.id)
}

// AgentSandboxOptions configures an AgentSandboxBackend driving Kubernetes
// Agent Sandbox CRDs / HTTP gateway.
type AgentSandboxOptions struct {
	Endpoint   string
	Namespace  string
	WarmPool   string
	AuthToken  string
	HTTPClient *http.Client
}

// AgentSandboxBackend implements Backend, ForkBackend, and Prewarmer against
// the Kubernetes Agent Sandbox HTTP gateway / controller API.
type AgentSandboxBackend struct {
	endpoint  string
	namespace string
	warmPool  string
	authToken string
	http      *http.Client
}

// NewAgentSandboxBackend constructs an AgentSandboxBackend targeting
// opts.Endpoint.
func NewAgentSandboxBackend(opts AgentSandboxOptions) (*AgentSandboxBackend, error) {
	endpoint := strings.TrimRight(strings.TrimSpace(opts.Endpoint), "/")
	if endpoint == "" {
		return nil, errors.New("swebench: AgentSandboxOptions.Endpoint is required")
	}
	if !strings.HasPrefix(endpoint, "http://") && !strings.HasPrefix(endpoint, "https://") {
		endpoint = "http://" + endpoint
	}
	ns := opts.Namespace
	if ns == "" {
		ns = "default"
	}
	hc := opts.HTTPClient
	if hc == nil {
		hc = &http.Client{Timeout: 5 * time.Minute}
	}
	return &AgentSandboxBackend{
		endpoint:  endpoint,
		namespace: ns,
		warmPool:  opts.WarmPool,
		authToken: opts.AuthToken,
		http:      hc,
	}, nil
}

func (b *AgentSandboxBackend) doJSON(ctx context.Context, method, path string, reqBody, respBody any) error {
	var bodyReader io.Reader
	if reqBody != nil {
		raw, err := json.Marshal(reqBody)
		if err != nil {
			return err
		}
		bodyReader = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, b.endpoint+path, bodyReader)
	if err != nil {
		return err
	}
	if reqBody != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if b.authToken != "" {
		req.Header.Set("Authorization", "Bearer "+b.authToken)
	}
	resp, err := b.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	if resp.StatusCode == http.StatusNotFound {
		return fmt.Errorf("%w: %s", env.ErrNotFound, strings.TrimSpace(string(data)))
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("agent-sandbox %s %s: status %d: %s", method, path, resp.StatusCode, strings.TrimSpace(string(data)))
	}
	if respBody != nil && len(data) > 0 {
		if err := json.Unmarshal(data, respBody); err != nil {
			return fmt.Errorf("agent-sandbox decoding %s response: %w", path, err)
		}
	}
	return nil
}

// Create claims or creates an Agent Sandbox instance.
func (b *AgentSandboxBackend) Create(ctx context.Context, id, image string) (Sandbox, error) {
	payload := map[string]any{
		"id":        id,
		"namespace": b.namespace,
		"image":     image,
		"warm_pool": b.warmPool,
	}
	if err := b.doJSON(ctx, http.MethodPost, "/v1/sandboxes", payload, nil); err != nil {
		return nil, fmt.Errorf("create %s: %w", id, err)
	}
	return &agentSandbox{b: b, id: id}, nil
}

// Delete removes an Agent Sandbox claim/instance by ID.
func (b *AgentSandboxBackend) Delete(ctx context.Context, id string) error {
	q := url.Values{"namespace": []string{b.namespace}}
	err := b.doJSON(ctx, http.MethodDelete, "/v1/sandboxes/"+url.PathEscape(id)+"?"+q.Encode(), nil, nil)
	if errors.Is(err, env.ErrNotFound) {
		return nil
	}
	return err
}

// SuspendWithMode suspends an Agent Sandbox instance with the requested
// durability mode.
func (b *AgentSandboxBackend) SuspendWithMode(ctx context.Context, id string, async bool) error {
	mode := "SYNCHRONOUS"
	if async {
		mode = "ASYNCHRONOUS"
	}
	payload := map[string]any{
		"namespace":       b.namespace,
		"async":           async,
		"durability_mode": mode,
	}
	return b.doJSON(ctx, http.MethodPost, "/v1/sandboxes/"+url.PathEscape(id)+"/suspend", payload, nil)
}

// Fork clones a new Agent Sandbox instance from fromID.
func (b *AgentSandboxBackend) Fork(ctx context.Context, fromID, newID string) (Sandbox, error) {
	payload := map[string]any{
		"new_id":    newID,
		"namespace": b.namespace,
	}
	if err := b.doJSON(ctx, http.MethodPost, "/v1/sandboxes/"+url.PathEscape(fromID)+"/fork", payload, nil); err != nil {
		return nil, fmt.Errorf("fork %s -> %s: %w", fromID, newID, err)
	}
	return &agentSandbox{b: b, id: newID}, nil
}

// Prewarm warms the configured SandboxWarmPool with images.
func (b *AgentSandboxBackend) Prewarm(ctx context.Context, images []string) error {
	payload := map[string]any{
		"namespace": b.namespace,
		"warm_pool": b.warmPool,
		"images":    images,
	}
	return b.doJSON(ctx, http.MethodPost, "/v1/warmpools/prewarm", payload, nil)
}

type agentSandbox struct {
	b  *AgentSandboxBackend
	id string
}

func (s *agentSandbox) Run(ctx context.Context, command string, timeout time.Duration) (Result, error) {
	payload := map[string]any{
		"namespace":  s.b.namespace,
		"command":    []string{"sh", "-c", command},
		"timeout_ms": timeout.Milliseconds(),
	}
	var resp struct {
		ExitCode int    `json:"exit_code"`
		Stdout   string `json:"stdout"`
		Stderr   string `json:"stderr"`
	}
	if err := s.b.doJSON(ctx, http.MethodPost, "/v1/sandboxes/"+url.PathEscape(s.id)+"/exec", payload, &resp); err != nil {
		return Result{}, err
	}
	return Result{ExitCode: resp.ExitCode, Stdout: resp.Stdout, Stderr: resp.Stderr}, nil
}

func (s *agentSandbox) WriteFile(ctx context.Context, path string, content []byte, mode fs.FileMode) error {
	payload := map[string]any{
		"namespace": s.b.namespace,
		"path":      path,
		"content":   string(content),
		"mode":      uint32(mode.Perm()),
	}
	return s.b.doJSON(ctx, http.MethodPut, "/v1/sandboxes/"+url.PathEscape(s.id)+"/files", payload, nil)
}

func (s *agentSandbox) Suspend(ctx context.Context) error {
	return s.SuspendWithMode(ctx, false)
}

func (s *agentSandbox) SuspendAsync(ctx context.Context) error {
	return s.SuspendWithMode(ctx, true)
}

func (s *agentSandbox) SuspendWithMode(ctx context.Context, async bool) error {
	return s.b.SuspendWithMode(ctx, s.id, async)
}

func (s *agentSandbox) Fork(ctx context.Context, newID string) (Sandbox, error) {
	return s.b.Fork(ctx, s.id, newID)
}

func (s *agentSandbox) Delete(ctx context.Context) error {
	return s.b.Delete(ctx, s.id)
}
