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
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	env "github.com/agent-substrate/env/clients/go"
	"github.com/agent-substrate/env/guest"
	"github.com/agent-substrate/env/internal/ate"
	"github.com/agent-substrate/env/internal/internaltest/fakecontrol"
	"github.com/agent-substrate/env/internal/internaltest/fakerouter"
	ateenvv1alpha "github.com/agent-substrate/env/proto/ateenv/v1alpha"
	ateapipb "go.ate.dev/substrate/api/ateapi/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"
)

// fakeAPI is an in-process ate-env API: enough of the environment, process
// and file services for the backend's start, poll and read path.
type fakeAPI struct {
	ateenvv1alpha.UnimplementedEnvironmentServiceServer
	ateenvv1alpha.UnimplementedProcessServiceServer
	ateenvv1alpha.UnimplementedFileSystemServiceServer

	mu sync.Mutex
	// pollsUntilExit is how many GetProcess calls a process stays running;
	// unavailablePolls is how many GetProcess calls answer Unavailable first.
	pollsUntilExit   int
	unavailablePolls int
	exitCode         int32
	stdout, stderr   string

	nextID    int
	polls     map[string]int
	exited    map[string]bool
	killed    []string
	created   []string
	deleted   []string
	commands  []string
	suspended []string
}

func (f *fakeAPI) CreateEnvironment(_ context.Context, req *ateenvv1alpha.CreateEnvironmentRequest) (*ateenvv1alpha.CreateEnvironmentResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.created = append(f.created, req.GetId())
	return &ateenvv1alpha.CreateEnvironmentResponse{Environment: &ateenvv1alpha.Environment{Id: req.GetId(), Atespace: req.GetAtespace()}}, nil
}

func (f *fakeAPI) SuspendEnvironment(_ context.Context, req *ateenvv1alpha.SuspendEnvironmentRequest) (*ateenvv1alpha.SuspendEnvironmentResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.suspended = append(f.suspended, req.GetId())
	return &ateenvv1alpha.SuspendEnvironmentResponse{}, nil
}

func (f *fakeAPI) DeleteEnvironment(_ context.Context, req *ateenvv1alpha.DeleteEnvironmentRequest) (*ateenvv1alpha.DeleteEnvironmentResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.deleted = append(f.deleted, req.GetId())
	return &ateenvv1alpha.DeleteEnvironmentResponse{}, nil
}

func (f *fakeAPI) StartProcess(_ context.Context, req *ateenvv1alpha.StartProcessRequest) (*ateenvv1alpha.Process, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.nextID++
	id := fmt.Sprintf("p%d", f.nextID)
	f.commands = append(f.commands, strings.Join(req.GetCommand(), " "))
	return &ateenvv1alpha.Process{ProcessId: id, State: ateenvv1alpha.ProcessState_PROCESS_STATE_RUNNING}, nil
}

func (f *fakeAPI) GetProcess(_ context.Context, req *ateenvv1alpha.GetProcessRequest) (*ateenvv1alpha.Process, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.unavailablePolls > 0 {
		f.unavailablePolls--
		return nil, status.Error(codes.Unavailable, "router: no healthy upstream")
	}
	f.polls[req.GetProcessId()]++
	p := &ateenvv1alpha.Process{ProcessId: req.GetProcessId(), State: ateenvv1alpha.ProcessState_PROCESS_STATE_RUNNING}
	if f.polls[req.GetProcessId()] > f.pollsUntilExit {
		f.exited[req.GetProcessId()] = true
		p.State = ateenvv1alpha.ProcessState_PROCESS_STATE_EXITED
		p.ExitCode = f.exitCode
	}
	return p, nil
}

func (f *fakeAPI) StreamProcessOutput(req *ateenvv1alpha.StreamProcessOutputRequest, stream grpc.ServerStreamingServer[ateenvv1alpha.ProcessOutput]) error {
	f.mu.Lock()
	stdout, stderr, exited, code := f.stdout, f.stderr, f.exited[req.GetProcessId()], f.exitCode
	f.mu.Unlock()
	if stdout != "" {
		if err := stream.Send(&ateenvv1alpha.ProcessOutput{Output: &ateenvv1alpha.ProcessOutput_Stdout{Stdout: []byte(stdout)}}); err != nil {
			return err
		}
	}
	if stderr != "" {
		if err := stream.Send(&ateenvv1alpha.ProcessOutput{Output: &ateenvv1alpha.ProcessOutput_Stderr{Stderr: []byte(stderr)}}); err != nil {
			return err
		}
	}
	if exited {
		return stream.Send(&ateenvv1alpha.ProcessOutput{Output: &ateenvv1alpha.ProcessOutput_Exit{Exit: &ateenvv1alpha.Process{
			ProcessId: req.GetProcessId(), State: ateenvv1alpha.ProcessState_PROCESS_STATE_EXITED, ExitCode: code}}})
	}
	return nil
}

func (f *fakeAPI) SignalProcess(_ context.Context, req *ateenvv1alpha.SignalProcessRequest) (*ateenvv1alpha.Process, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.killed = append(f.killed, req.GetProcessId())
	f.exited[req.GetProcessId()] = true
	return &ateenvv1alpha.Process{ProcessId: req.GetProcessId(), State: ateenvv1alpha.ProcessState_PROCESS_STATE_EXITED, ExitCode: 137}, nil
}

// newFakeBackend serves fakeAPI over an in-memory connection and returns an
// EnvBackend on it, the same path the binary uses with a real address.
func newFakeBackend(t *testing.T, api *fakeAPI) *EnvBackend {
	t.Helper()
	api.polls = map[string]int{}
	api.exited = map[string]bool{}
	lis := bufconn.Listen(1 << 20)
	srv := grpc.NewServer()
	ateenvv1alpha.RegisterEnvironmentServiceServer(srv, api)
	ateenvv1alpha.RegisterProcessServiceServer(srv, api)
	ateenvv1alpha.RegisterFileSystemServiceServer(srv, api)
	go func() { _ = srv.Serve(lis) }()
	conn, err := grpc.NewClient("passthrough:///bufconn",
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) { return lis.DialContext(ctx) }),
		grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	c, err := env.NewClient(env.ClientOptions{GRPCConn: conn})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close(); _ = conn.Close(); srv.Stop() })
	b := &EnvBackend{clients: []*env.Client{c}, atespace: "ate-env", baseTemplate: "default-template"}
	WithPollBounds(time.Millisecond, 5*time.Millisecond)(b)
	return b
}

func TestEnvBackendRunCollectsExitAndOutput(t *testing.T) {
	api := &fakeAPI{pollsUntilExit: 3, exitCode: 4, stdout: "out", stderr: "err"}
	b := newFakeBackend(t, api)
	sb, err := b.Create(context.Background(), "e1", "reg/img@"+goodDigest)
	if err != nil {
		t.Fatal(err)
	}
	res, err := sb.Run(context.Background(), "echo hi", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if res.ExitCode != 4 || res.Stdout != "out" || res.Stderr != "err" {
		t.Fatalf("result %+v", res)
	}
	if len(api.killed) != 0 {
		t.Fatalf("a process that exited must not be killed: %v", api.killed)
	}
	if len(api.created) != 1 || api.created[0] != "e1" {
		t.Fatalf("created %v", api.created)
	}
	if !strings.HasSuffix(api.commands[0], "echo hi") {
		t.Fatalf("command %q", api.commands[0])
	}
}

func TestEnvBackendRunToleratesTransientPolls(t *testing.T) {
	api := &fakeAPI{pollsUntilExit: 1, unavailablePolls: pollRetries}
	b := newFakeBackend(t, api)
	sb, _ := b.Create(context.Background(), "e1", "reg/img@"+goodDigest)
	if _, err := sb.Run(context.Background(), "true", time.Minute); err != nil {
		t.Fatalf("%d transient poll errors must be tolerated: %v", pollRetries, err)
	}
}

func TestEnvBackendRunGivesUpAndKills(t *testing.T) {
	api := &fakeAPI{pollsUntilExit: 100, unavailablePolls: pollRetries + 1}
	b := newFakeBackend(t, api)
	sb, _ := b.Create(context.Background(), "e1", "reg/img@"+goodDigest)
	_, err := sb.Run(context.Background(), "sleep 100", time.Minute)
	if err == nil || status.Code(err) != codes.Unavailable {
		t.Fatalf("expected the poll error after the retry limit, got %v", err)
	}
	if len(api.killed) != 1 {
		t.Fatalf("an abandoned process must be killed once, got %v", api.killed)
	}
}

func TestEnvBackendRunKillsOnCancel(t *testing.T) {
	api := &fakeAPI{pollsUntilExit: 1 << 30}
	b := newFakeBackend(t, api)
	sb, _ := b.Create(context.Background(), "e1", "reg/img@"+goodDigest)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	if _, err := sb.Run(ctx, "sleep 100", time.Minute); err == nil {
		t.Fatal("a cancelled Run must return an error")
	}
	if len(api.killed) != 1 {
		t.Fatalf("the process must be killed after a cancel, got %v", api.killed)
	}
}

func TestEnvBackendDeleteByName(t *testing.T) {
	api := &fakeAPI{}
	b := newFakeBackend(t, api)
	if err := b.Delete(context.Background(), "gone"); err != nil {
		t.Fatal(err)
	}
	if len(api.deleted) != 1 || api.deleted[0] != "gone" {
		t.Fatalf("deleted %v", api.deleted)
	}
}

func TestEnvBackendSpreadsConnections(t *testing.T) {
	// grpc.NewClient does not dial until the first call, so a backend over
	// an unreachable address is enough to check the pool and the rotation.
	b, err := NewEnvBackend("127.0.0.1:1", "ate-env", "default-template", 3, "")
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	if len(b.clients) != 3 {
		t.Fatalf("%d clients, want 3", len(b.clients))
	}
	seen := map[int]int{}
	for i := 0; i < 9; i++ {
		c := b.pick()
		for j, cc := range b.clients {
			if cc == c {
				seen[j]++
			}
		}
	}
	for j := 0; j < 3; j++ {
		if seen[j] != 3 {
			t.Fatalf("connection %d picked %d times, want 3: %v", j, seen[j], seen)
		}
	}
	one, err := NewEnvBackend("127.0.0.1:1", "ate-env", "default-template", 0, "", WithPollBounds(3*time.Millisecond, 15*time.Millisecond))
	if err != nil {
		t.Fatal(err)
	}
	defer one.Close()
	if len(one.clients) != 1 {
		t.Fatalf("connections 0 must mean 1, got %d", len(one.clients))
	}
	if one.minPoll != 3*time.Millisecond || one.maxPoll != 15*time.Millisecond {
		t.Fatalf("WithPollBounds: got (%v, %v), want (3ms, 15ms)", one.minPoll, one.maxPoll)
	}
}

// authRecorder answers DeleteEnvironment and records the authorization
// metadata of each call.
type authRecorder struct {
	ateenvv1alpha.UnimplementedEnvironmentServiceServer
	mu   sync.Mutex
	seen [][]string
}

func (a *authRecorder) DeleteEnvironment(ctx context.Context, _ *ateenvv1alpha.DeleteEnvironmentRequest) (*ateenvv1alpha.DeleteEnvironmentResponse, error) {
	md, _ := metadata.FromIncomingContext(ctx)
	a.mu.Lock()
	a.seen = append(a.seen, md.Get("authorization"))
	a.mu.Unlock()
	return &ateenvv1alpha.DeleteEnvironmentResponse{}, nil
}

func TestNewEnvBackendSendsTheAuthToken(t *testing.T) {
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	rec := &authRecorder{}
	srv := grpc.NewServer()
	ateenvv1alpha.RegisterEnvironmentServiceServer(srv, rec)
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(srv.Stop)

	for _, token := range []string{"s3cret", ""} {
		b, err := NewEnvBackend(lis.Addr().String(), "ate-env", "default-template", 2, token)
		if err != nil {
			t.Fatal(err)
		}
		// Two deletes reach both connections of the pool.
		for i := 0; i < 2; i++ {
			if err := b.Delete(context.Background(), fmt.Sprintf("e%d", i)); err != nil {
				t.Fatal(err)
			}
		}
		_ = b.Close()
	}
	rec.mu.Lock()
	defer rec.mu.Unlock()
	if len(rec.seen) != 4 {
		t.Fatalf("%d calls recorded, want 4", len(rec.seen))
	}
	for i, got := range rec.seen[:2] {
		if len(got) != 1 || got[0] != "Bearer s3cret" {
			t.Errorf("call %d with a token: authorization = %q, want one %q", i, got, "Bearer s3cret")
		}
	}
	for i, got := range rec.seen[2:] {
		if len(got) != 0 {
			t.Errorf("call %d without a token: authorization = %q, want none", i+2, got)
		}
	}
}

func TestEnvBackendSuspendAndPrewarm(t *testing.T) {
	api := &fakeAPI{}
	b := newFakeBackend(t, api)
	ctx := context.Background()

	sb, err := b.Create(ctx, "e1", "reg/img@"+goodDigest)
	if err != nil {
		t.Fatal(err)
	}
	if err := sb.Suspend(ctx); err != nil {
		t.Fatalf("Suspend: %v", err)
	}
	if err := sb.(AsyncSuspender).SuspendAsync(ctx); err != nil {
		t.Fatalf("SuspendAsync: %v", err)
	}
	if err := sb.(ModeSuspender).SuspendWithMode(ctx, false); err != nil {
		t.Fatalf("SuspendWithMode(false): %v", err)
	}
	if len(api.suspended) != 3 {
		t.Fatalf("suspended = %v, want 3 calls", api.suspended)
	}
	if err := b.Prewarm(ctx, []string{"reg/img@" + goodDigest}); err != nil {
		t.Fatalf("Prewarm: %v", err)
	}
}

func TestDirectSubstrateBackend(t *testing.T) {
	control := fakecontrol.New()
	controlAddr, stopControl, err := control.Serve()
	if err != nil {
		t.Fatalf("control.Serve: %v", err)
	}
	t.Cleanup(stopControl)

	router := fakerouter.New()
	routerAddr, stopRouter := router.Serve()
	t.Cleanup(stopRouter)

	workDir := t.TempDir()
	grpcGuestServer, cleanupGuest, err := guest.NewServer(guest.Config{
		Workspace:        workDir,
		EnableProcess:    true,
		EnableFileSystem: true,
	})
	if err != nil {
		t.Fatalf("guest.NewServer: %v", err)
	}
	t.Cleanup(cleanupGuest)
	router.Register("ds-1", grpcGuestServer)
	router.Register("ds-1-fork", grpcGuestServer)

	control.AddTemplate(&ateapipb.ActorTemplate{
		Metadata: &ateapipb.ResourceMetadata{Atespace: "ate-env", Name: "default-template"},
		Containers: []*ateapipb.Container{{
			Name:    "guest",
			Image:   "example.com/ate-env-guest@" + goodDigest,
			Command: []string{"/ko-app/ate-env-guest"},
		}},
	})

	ctx := context.Background()
	b, err := NewDirectSubstrateBackend(DirectSubstrateOptions{
		ATEOptions: ate.Options{
			ControlAddr: controlAddr,
			RouterAddr:  routerAddr,
			SkipVerify:  true,
		},
		Atespace:     "ate-env",
		BaseTemplate: "default-template",
		MinPoll:      time.Millisecond,
		MaxPoll:      5 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("NewDirectSubstrateBackend: %v", err)
	}
	t.Cleanup(func() { _ = b.Close() })

	img := "reg/img@" + goodDigest
	if err := b.Prewarm(ctx, []string{img}); err != nil {
		t.Fatalf("Prewarm: %v", err)
	}

	sb, err := b.Create(ctx, "ds-1", img)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	if err := sb.WriteFile(ctx, "greeting.txt", []byte("hello substrate"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	gotBytes, err := os.ReadFile(filepath.Join(workDir, "greeting.txt"))
	if err != nil || string(gotBytes) != "hello substrate" {
		t.Fatalf("ReadFile = %q, %v", gotBytes, err)
	}

	res, err := sb.Run(ctx, "printf direct-ok", 5*time.Second)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.ExitCode != 0 || res.Stdout != "direct-ok" {
		t.Fatalf("Run result = %+v", res)
	}

	if err := sb.(AsyncSuspender).SuspendAsync(ctx); err != nil {
		t.Fatalf("SuspendAsync: %v", err)
	}
	if n := len(control.SuspendModes); n == 0 || control.SuspendModes[n-1] != ateapipb.DurabilityMode_DURABILITY_MODE_ASYNCHRONOUS {
		t.Fatalf("SuspendModes = %v, want last ASYNCHRONOUS", control.SuspendModes)
	}

	if err := sb.Suspend(ctx); err != nil {
		t.Fatalf("Suspend: %v", err)
	}
	if n := len(control.SuspendModes); n == 0 || control.SuspendModes[n-1] != ateapipb.DurabilityMode_DURABILITY_MODE_SYNCHRONOUS {
		t.Fatalf("SuspendModes = %v, want last SYNCHRONOUS", control.SuspendModes)
	}

	forked, err := sb.(SandboxForker).Fork(ctx, "ds-1-fork")
	if err != nil {
		t.Fatalf("Fork: %v", err)
	}
	if err := forked.Delete(ctx); err != nil {
		t.Fatalf("forked.Delete: %v", err)
	}
	if err := sb.Delete(ctx); err != nil {
		t.Fatalf("sb.Delete: %v", err)
	}
}

func TestAgentSandboxBackend(t *testing.T) {
	type recordedReq struct {
		Method string
		Path   string
		Auth   string
		Body   map[string]any
	}
	var (
		mu   sync.Mutex
		reqs []recordedReq
	)
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var body map[string]any
		if len(raw) > 0 {
			_ = json.Unmarshal(raw, &body)
		}
		mu.Lock()
		reqs = append(reqs, recordedReq{
			Method: r.Method,
			Path:   r.URL.RequestURI(),
			Auth:   r.Header.Get("Authorization"),
			Body:   body,
		})
		mu.Unlock()

		if r.Method == http.MethodDelete && strings.Contains(r.URL.Path, "missing") {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		if strings.HasSuffix(r.URL.Path, "/exec") {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"exit_code":0,"stdout":"agent-sandbox-out","stderr":""}`))
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(ts.Close)

	b, err := NewAgentSandboxBackend(AgentSandboxOptions{
		Endpoint:  ts.URL,
		Namespace: "bench-ns",
		WarmPool:  "warm-pool-1",
		AuthToken: "secret-tok",
	})
	if err != nil {
		t.Fatalf("NewAgentSandboxBackend: %v", err)
	}

	ctx := context.Background()
	if err := b.Prewarm(ctx, []string{"img1"}); err != nil {
		t.Fatalf("Prewarm: %v", err)
	}
	sb, err := b.Create(ctx, "as-1", "img1")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := sb.WriteFile(ctx, "/tmp/a.txt", []byte("data"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	res, err := sb.Run(ctx, "echo hi", 5*time.Second)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.ExitCode != 0 || res.Stdout != "agent-sandbox-out" {
		t.Fatalf("Run result = %+v", res)
	}
	if err := sb.Suspend(ctx); err != nil {
		t.Fatalf("Suspend: %v", err)
	}
	if err := sb.(AsyncSuspender).SuspendAsync(ctx); err != nil {
		t.Fatalf("SuspendAsync: %v", err)
	}
	forked, err := sb.(SandboxForker).Fork(ctx, "as-1-fork")
	if err != nil {
		t.Fatalf("Fork: %v", err)
	}
	if err := forked.Delete(ctx); err != nil {
		t.Fatalf("forked.Delete: %v", err)
	}
	if err := sb.Delete(ctx); err != nil {
		t.Fatalf("sb.Delete: %v", err)
	}
	if err := b.Delete(ctx, "missing"); err != nil {
		t.Fatalf("Delete(missing) should ignore 404, got: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(reqs) < 8 {
		t.Fatalf("recorded %d requests, want >= 8", len(reqs))
	}
	for i, r := range reqs {
		if r.Auth != "Bearer secret-tok" {
			t.Errorf("req %d (%s %s): auth = %q, want Bearer secret-tok", i, r.Method, r.Path, r.Auth)
		}
	}
}

func TestNewBackendDriver(t *testing.T) {
	control := fakecontrol.New()
	controlAddr, stopControl, err := control.Serve()
	if err != nil {
		t.Fatalf("control.Serve: %v", err)
	}
	t.Cleanup(stopControl)

	router := fakerouter.New()
	routerAddr, stopRouter := router.Serve()
	t.Cleanup(stopRouter)

	// 1. substrate + ate-env-api (default)
	bEnv, closer, err := NewBackendDriver(BackendDriverOptions{
		Backend:     "substrate",
		Access:      "ate-env-api",
		Endpoint:    "127.0.0.1:1",
		Connections: 1,
	})
	if err != nil {
		t.Fatalf("NewBackendDriver(substrate, ate-env-api): %v", err)
	}
	_ = closer.Close()
	if _, ok := bEnv.(*EnvBackend); !ok {
		t.Fatalf("got %T, want *EnvBackend", bEnv)
	}

	// 2. substrate + direct
	bDir, closer, err := NewBackendDriver(BackendDriverOptions{
		Backend:     "substrate",
		Access:      "direct",
		ControlAddr: controlAddr,
		RouterAddr:  routerAddr,
	})
	if err != nil {
		t.Fatalf("NewBackendDriver(substrate, direct): %v", err)
	}
	_ = closer.Close()
	if _, ok := bDir.(*DirectSubstrateBackend); !ok {
		t.Fatalf("got %T, want *DirectSubstrateBackend", bDir)
	}

	// 3. agent-sandbox + sdk
	bAS, closer, err := NewBackendDriver(BackendDriverOptions{
		Backend:  "agent-sandbox",
		Access:   "sdk",
		Endpoint: "http://127.0.0.1:8080",
	})
	if err != nil {
		t.Fatalf("NewBackendDriver(agent-sandbox, sdk): %v", err)
	}
	_ = closer.Close()
	if _, ok := bAS.(*AgentSandboxBackend); !ok {
		t.Fatalf("got %T, want *AgentSandboxBackend", bAS)
	}

	// 4. invalid backend or access
	if _, _, err := NewBackendDriver(BackendDriverOptions{Backend: "bogus"}); err == nil {
		t.Fatal("expected error for unknown backend")
	}
	if _, _, err := NewBackendDriver(BackendDriverOptions{Backend: "substrate", Access: "bogus"}); err == nil {
		t.Fatal("expected error for unknown access")
	}
}
