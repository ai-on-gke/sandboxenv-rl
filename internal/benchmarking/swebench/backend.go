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
	"errors"
	"fmt"
	"io"
	"io/fs"
	"strings"
	"sync/atomic"
	"time"

	env "github.com/agent-substrate/env/clients/go"
	ateenvv1alpha "github.com/agent-substrate/env/proto/ateenv/v1alpha"
	"golang.org/x/time/rate"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/durationpb"
)

// Result is what a Run step returned.
type Result struct {
	ExitCode int
	Stdout   string
	Stderr   string
}

// Sandbox is the subset of an environment the loader drives.
type Sandbox interface {
	Run(ctx context.Context, command string, timeout time.Duration) (Result, error)
	WriteFile(ctx context.Context, path string, content []byte, mode fs.FileMode) error
	Suspend(ctx context.Context) error
	Delete(ctx context.Context) error
}

// ModeSuspender is an optional interface implemented by Sandboxes that can
// choose between synchronous and asynchronous checkpoint durability.
type ModeSuspender interface {
	SuspendWithMode(ctx context.Context, async bool) error
}

// AsyncSuspender is an optional interface implemented by Sandboxes that support
// non-blocking asynchronous checkpointing (DURABILITY_MODE_ASYNCHRONOUS).
type AsyncSuspender interface {
	SuspendAsync(ctx context.Context) error
}

// SandboxForker is an optional interface implemented by Sandboxes that can
// fork a new Sandbox from their latest checkpoint.
type SandboxForker interface {
	Fork(ctx context.Context, newID string) (Sandbox, error)
}

// ForkBackend is an optional interface implemented by Backends that can fork a
// new Sandbox from an existing suspended environment ID.
type ForkBackend interface {
	Fork(ctx context.Context, fromID, newID string) (Sandbox, error)
}

// Prewarmer is an optional interface implemented by Backends that can prewarm
// image templates or warm pools ahead of a training step.
type Prewarmer interface {
	Prewarm(ctx context.Context, images []string) error
}

// Backend creates sandboxes. The loader is written against this interface so
// tests run it against an in-memory fake and a second SDK or backend can be
// compared with the same workload.
type Backend interface {
	Create(ctx context.Context, id, image string) (Sandbox, error)
	// Delete removes an environment by name without a Sandbox handle, for
	// one whose create call failed on the client side but may have
	// succeeded on the server.
	Delete(ctx context.Context, id string) error
}

// EnvBackendOption configures an EnvBackend.
type EnvBackendOption func(*EnvBackend)

// WithPollBounds overrides the initial and maximum process-polling intervals.
func WithPollBounds(minPoll, maxPoll time.Duration) EnvBackendOption {
	return func(b *EnvBackend) {
		b.minPoll = minPoll
		b.maxPoll = maxPoll
	}
}

// EnvBackend is the Backend over the ate-env API through the Go SDK.
//
// It holds several connections and hands them out round-robin per
// environment: a Kubernetes Service balances per connection, so one
// connection would put every task on one API replica, and gRPC caps the
// concurrent streams on a connection.
type EnvBackend struct {
	clients          []*env.Client
	next             atomic.Uint64
	atespace         string
	baseTemplate     string
	minPoll, maxPoll time.Duration
	deletes          deleteLimits
}

// DefaultDeleteRate is the loader's default cap on the environment deletes an
// ate-env-api backend issues per second, across all its connections. A
// delete removes the environment's snapshots from the bucket, and the bucket
// rate-limits deletes: a cleanup of 37,000 environments at about 1,850
// deletes per second lost 15,031 of them to GCS 429s. 200 per second clears
// that cleanup in about three minutes, inside the loader's ten-minute cleanup
// budget, and leaves room under the bucket's limit for Substrate's own
// snapshot traffic.
const DefaultDeleteRate = 200

// WithDeleteRate caps the environment deletes the backend issues at perSecond
// across all its connections, with a burst of one second's worth; 0 removes
// the cap. See DefaultDeleteRate.
func WithDeleteRate(perSecond float64) EnvBackendOption {
	return func(b *EnvBackend) {
		if perSecond <= 0 {
			b.deletes.limiter = nil
			return
		}
		b.deletes.limiter = rate.NewLimiter(rate.Limit(perSecond), max(1, int(perSecond)))
	}
}

// deleteLimits is how an EnvBackend paces and retries its deletes. The zero
// value neither paces nor retries.
type deleteLimits struct {
	limiter *rate.Limiter // nil: no cap
	// budget bounds the retries of one delete; minWait is the first pause
	// between attempts, doubling up to maxWait.
	budget, minWait, maxWait time.Duration
}

const (
	deleteRetryBudget = 2 * time.Minute
	deleteBackoffMin  = time.Second
	deleteBackoffMax  = 30 * time.Second
)

func defaultDeleteLimits() deleteLimits {
	return deleteLimits{budget: deleteRetryBudget, minWait: deleteBackoffMin, maxWait: deleteBackoffMax}
}

// do runs one delete under the cap and retries it on the errors a
// rate-limited bucket surfaces (isRetryableDelete) until the budget or ctx
// ends. The budget starts with the first attempt, after the cap's queue. A
// NotFound after a retry is success: an earlier attempt deleted the
// environment and its answer was lost. A NotFound on the first attempt is
// returned as it is, so a caller still learns of a name that was never there.
func (l deleteLimits) do(ctx context.Context, del func(context.Context) error) error {
	var deadline time.Time
	wait := l.minWait
	for attempt := 0; ; attempt++ {
		if l.limiter != nil {
			if err := l.limiter.Wait(ctx); err != nil {
				if ctx.Err() != nil {
					return ctx.Err()
				}
				return fmt.Errorf("delete rate cap: %w", err)
			}
		}
		if attempt == 0 {
			deadline = time.Now().Add(l.budget)
		}
		err := del(ctx)
		if err == nil || (attempt > 0 && isNotFound(err)) {
			return nil
		}
		if l.budget <= 0 || !isRetryableDelete(err) || ctx.Err() != nil || time.Now().Add(wait).After(deadline) {
			return err
		}
		select {
		case <-ctx.Done():
			return err
		case <-time.After(wait):
		}
		wait = min(wait*2, l.maxWait)
	}
}

// isRetryableDelete reports whether a failed delete is worth another try:
// ResourceExhausted, which is how ateapi reports the bucket rate-limiting its
// snapshot deletes, the codes isTransient retries, and a message that carries
// the bucket's own rate-limit error under another code. The message carries
// the environment id too, so a bare "429" is not matched: a run id such as
// swe-1794290000 contains one.
func isRetryableDelete(err error) bool {
	st, ok := status.FromError(err)
	if !ok {
		return false
	}
	if st.Code() == codes.ResourceExhausted || isTransient(err) {
		return true
	}
	msg := st.Message()
	return strings.Contains(msg, "Error 429") || strings.Contains(msg, "rateLimitExceeded")
}

// NewEnvBackend dials the ate-env API over connections connections (at
// least one). Environments are created in atespace from baseTemplate with
// the task image as rootfs; the API derives and bakes one template per
// distinct image on first use. A non-empty authToken is sent as the bearer
// token on each call, for an ate-env-api that runs with -auth-token-file.
// Deletes retry the errors a rate-limited bucket surfaces and, with
// WithDeleteRate, are capped per second.
func NewEnvBackend(endpoint, atespace, baseTemplate string, connections int, authToken string, opts ...EnvBackendOption) (*EnvBackend, error) {
	if connections < 1 {
		connections = 1
	}
	b := &EnvBackend{atespace: atespace, baseTemplate: baseTemplate, deletes: defaultDeleteLimits()}
	for _, opt := range opts {
		opt(b)
	}
	for i := 0; i < connections; i++ {
		c, err := env.NewClient(env.ClientOptions{Endpoint: endpoint, AuthToken: authToken})
		if err != nil {
			b.Close()
			return nil, err
		}
		b.clients = append(b.clients, c)
	}
	return b, nil
}

// Close releases the API connections.
func (b *EnvBackend) Close() error {
	var first error
	for _, c := range b.clients {
		if err := c.Close(); err != nil && first == nil {
			first = err
		}
	}
	return first
}

// pick returns the next connection round-robin.
func (b *EnvBackend) pick() *env.Client {
	return b.clients[int(b.next.Add(1)-1)%len(b.clients)]
}

// Delete implements Backend, under the backend's delete cap and retries.
func (b *EnvBackend) Delete(ctx context.Context, id string) error {
	return b.deletes.do(ctx, func(ctx context.Context) error {
		return b.pick().Delete(ctx, b.atespace, id)
	})
}

// SuspendWithMode suspends the environment with id via the ate-env API.
func (b *EnvBackend) SuspendWithMode(ctx context.Context, id string, _ bool) error {
	return b.pick().Suspend(ctx, b.atespace, id)
}

// Prewarm ensures derived ActorTemplates exist for each image by creating and
// immediately deleting a probe environment per image.
func (b *EnvBackend) Prewarm(ctx context.Context, images []string) error {
	for i, img := range images {
		id := fmt.Sprintf("prewarm-%d-%d", time.Now().UnixNano()%1_000_000, i)
		sb, err := b.Create(ctx, id, img)
		if err != nil {
			_ = b.Delete(ctx, id)
			return fmt.Errorf("prewarm %s: %w", img, err)
		}
		if err := sb.Delete(ctx); err != nil {
			return fmt.Errorf("prewarm cleanup %s: %w", id, err)
		}
	}
	return nil
}

// Create implements Backend. The environment keeps the connection it was
// created on for the rest of its life.
func (b *EnvBackend) Create(ctx context.Context, id, image string) (Sandbox, error) {
	e, err := b.pick().Create(ctx, &ateenvv1alpha.CreateEnvironmentRequest{
		Id:          id,
		Atespace:    b.atespace,
		Template:    &ateenvv1alpha.Template{Name: b.baseTemplate, Atespace: b.atespace},
		RootfsImage: image,
	})
	if err != nil {
		return nil, fmt.Errorf("create %s: %w", id, err)
	}
	return envSandbox{e: e, minPoll: b.minPoll, maxPoll: b.maxPoll, deletes: b.deletes}, nil
}

type envSandbox struct {
	e                *env.Env
	minPoll, maxPoll time.Duration
	deletes          deleteLimits
}

const (
	// Default polling bounds for a running process: short at first so quick
	// commands return quickly, backing off so a long test run does not hammer
	// the API.
	defaultPollMin = 50 * time.Millisecond
	defaultPollMax = 2 * time.Second
	// pollRetries is how many consecutive transient poll errors are
	// tolerated before the step fails; a wake or an API pod restart costs a
	// few.
	pollRetries = 10
	// killTimeout bounds the kill of a process Run abandons.
	killTimeout = 10 * time.Second
)

type processOutputStream interface {
	Recv() (*ateenvv1alpha.ProcessOutput, error)
}

type polledProcessOps struct {
	minPoll time.Duration
	maxPoll time.Duration
	start   func(ctx context.Context, req *ateenvv1alpha.StartProcessRequest) (string, error)
	poll    func(ctx context.Context, procID string) (*ateenvv1alpha.Process, error)
	kill    func(ctx context.Context, procID string) error
	output  func(ctx context.Context, procID string) (processOutputStream, error)
}

// runPolledProcess starts command via ops.start, polls ops.poll with exponential
// backoff until the process exits, and reads buffered stdout/stderr via
// ops.output. If runPolledProcess returns before the process has exited, it
// kills the process via ops.kill.
func runPolledProcess(ctx context.Context, command string, timeout time.Duration, ops polledProcessOps) (res Result, err error) {
	start := &ateenvv1alpha.StartProcessRequest{Command: []string{"sh", "-c", command}}
	if timeout > 0 {
		start.Timeout = durationpb.New(timeout)
	}
	procID, err := ops.start(ctx, start)
	if err != nil {
		return Result{}, err
	}
	exited := false
	defer func() {
		if exited {
			return
		}
		kctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), killTimeout)
		defer cancel()
		if kerr := ops.kill(kctx, procID); kerr != nil && !errors.Is(kerr, env.ErrProcessExited) && err != nil {
			err = fmt.Errorf("%w (and killing the process failed: %v)", err, kerr)
		}
	}()

	pMin, pMax := ops.minPoll, ops.maxPoll
	if pMin <= 0 {
		pMin = defaultPollMin
	}
	if pMax <= 0 {
		pMax = defaultPollMax
	}
	wait := pMin
	failures := 0
	for {
		select {
		case <-ctx.Done():
			return Result{}, ctx.Err()
		case <-time.After(wait):
		}
		p, perr := ops.poll(ctx, procID)
		switch {
		case perr == nil:
			failures = 0
			if p.GetState() == ateenvv1alpha.ProcessState_PROCESS_STATE_EXITED {
				exited = true
				stream, oerr := ops.output(ctx, procID)
				if oerr != nil {
					return Result{}, oerr
				}
				return collectProcessOutput(stream, int(p.GetExitCode()))
			}
		case status.Code(perr) == codes.Unavailable || status.Code(perr) == codes.DeadlineExceeded:
			failures++
			if failures > pollRetries {
				return Result{}, fmt.Errorf("poll %s: %w", procID, perr)
			}
		default:
			return Result{}, perr
		}
		if wait < pMax {
			wait = min(wait*2, pMax)
		}
	}
}

// collectProcessOutput reads an exited process's buffered output in one
// non-following stream until EOF.
func collectProcessOutput(stream processOutputStream, exitCode int) (Result, error) {
	var stdout, stderr bytes.Buffer
	for {
		msg, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return Result{}, err
		}
		switch out := msg.GetOutput().(type) {
		case *ateenvv1alpha.ProcessOutput_Stdout:
			stdout.Write(out.Stdout)
		case *ateenvv1alpha.ProcessOutput_Stderr:
			stderr.Write(out.Stderr)
		case *ateenvv1alpha.ProcessOutput_Exit:
			exitCode = int(out.Exit.GetExitCode())
		}
	}
	return Result{ExitCode: exitCode, Stdout: stdout.String(), Stderr: stderr.String()}, nil
}

// Run starts the command, polls until it exits and then reads its output.
//
// The SDK's streaming Shell holds one request open through the router for
// the whole command, and the router's route timeout (five minutes by
// default) cuts it off; SWE-bench test runs take up to twelve minutes.
// Start, poll and read are each short calls, so no single request can
// outlive it. This belongs in the SDK as an option of Env.Run, since every
// SDK user with long commands meets the same timeout; it lives here until
// that change lands so the benchmark does not wait on it.
//
// If Run returns before the process has exited, it kills the process, so a
// retry of the same command never runs beside the first copy.
func (s envSandbox) Run(ctx context.Context, command string, timeout time.Duration) (Result, error) {
	var proc *env.Process
	return runPolledProcess(ctx, command, timeout, polledProcessOps{
		minPoll: s.minPoll,
		maxPoll: s.maxPoll,
		start: func(ctx context.Context, req *ateenvv1alpha.StartProcessRequest) (string, error) {
			p, err := s.e.StartProcess(ctx, req)
			if err != nil {
				return "", err
			}
			proc = p
			return p.ID(), nil
		},
		poll: func(ctx context.Context, procID string) (*ateenvv1alpha.Process, error) {
			return s.e.FindProcess(ctx, procID)
		},
		kill: func(ctx context.Context, _ string) error {
			_, err := proc.Kill(ctx)
			return err
		},
		output: func(ctx context.Context, _ string) (processOutputStream, error) {
			return proc.Output(ctx, &ateenvv1alpha.StreamProcessOutputRequest{})
		},
	})
}

func (s envSandbox) WriteFile(ctx context.Context, path string, content []byte, mode fs.FileMode) error {
	return s.e.WriteFile(ctx, path, bytes.NewReader(content), mode)
}

func (s envSandbox) Suspend(ctx context.Context) error { return s.e.Suspend(ctx) }

func (s envSandbox) SuspendAsync(ctx context.Context) error { return s.e.Suspend(ctx) }

func (s envSandbox) SuspendWithMode(ctx context.Context, _ bool) error {
	return s.e.Suspend(ctx)
}

func (s envSandbox) Delete(ctx context.Context) error { return s.deletes.do(ctx, s.e.Delete) }
