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

package apiservice_test

import (
	"strings"
	"testing"
	"time"

	"github.com/agent-substrate/env/internal/apiservice"
	ateenvv1alpha "github.com/agent-substrate/env/proto/ateenv/v1alpha"
	ateapipb "go.ate.dev/substrate/api/ateapi/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

const noRoom = "actor ate-env/%s unavailable: no worker has room for the actor"

// routerFull makes the fake router answer every gRPC request to an actor
// that is not running the way the real router does while the scheduler
// cannot place it.
func routerFull(te *testEnv) {
	te.router.NotRunningBody = func(id string) string {
		return strings.Replace(noRoom, "%s", id, 1)
	}
}

func startFailure(md metadata.MD) string {
	if v := md.Get(apiservice.StartFailureKey); len(v) > 0 {
		return v[0]
	}
	return ""
}

// A crash fails the call at once with the control plane's crash reason, not
// the probe's transport error, and marks the trailers so a client can retry
// on a fresh environment.
func TestCrashedWhileWakingCarriesTheCrashReason(t *testing.T) {
	te := newFullTestEnv(t)
	envCtx := registerGuest(t, te, "crash-reason")
	te.control.Crash("crash-reason", "assigned worker is draining")

	var trailer metadata.MD
	start := time.Now()
	_, err := te.procClient.StartProcess(envCtx, &ateenvv1alpha.StartProcessRequest{Command: []string{"true"}}, grpc.Trailer(&trailer))
	if status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("code %v (%v), want FailedPrecondition", status.Code(err), err)
	}
	if msg := status.Convert(err).Message(); !strings.Contains(msg, "crashed while waking: assigned worker is draining") {
		t.Errorf("message %q should carry the crash reason", msg)
	}
	if got := startFailure(trailer); got != apiservice.StartFailureCrashed {
		t.Errorf("trailer %s = %q, want %q", apiservice.StartFailureKey, got, apiservice.StartFailureCrashed)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Errorf("took %s; a crash must not wait out the wake budget", elapsed)
	}
}

// A streaming call carries the same trailer.
func TestCrashedWhileWakingMarksStreamingTrailers(t *testing.T) {
	te := newFullTestEnv(t)
	envCtx := registerGuest(t, te, "crash-stream")
	te.control.Crash("crash-stream", "worker restarted while hosting the actor")

	stream, err := te.fsClient.ReadFile(envCtx, &ateenvv1alpha.ReadFileRequest{Path: "x"})
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if _, err = stream.Recv(); status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("Recv: code %v (%v), want FailedPrecondition", status.Code(err), err)
	}
	if got := startFailure(stream.Trailer()); got != apiservice.StartFailureCrashed {
		t.Errorf("trailer = %q, want %q", got, apiservice.StartFailureCrashed)
	}
}

// With a no-capacity grace, a full fleet fails the call with
// ResourceExhausted once the router has said for that long that no worker
// has room, instead of waiting the whole wake budget.
func TestNoCapacityFailsAfterTheGrace(t *testing.T) {
	te := newFullTestEnv(t)
	te.srv.WakeNoCapacityGrace = 500 * time.Millisecond
	routerFull(te)
	envCtx := registerGuest(t, te, "full")
	te.control.SetState("full", ateapipb.ActorState_ACTOR_STATE_SUSPENDED)

	var trailer metadata.MD
	start := time.Now()
	_, err := te.procClient.StartProcess(envCtx, &ateenvv1alpha.StartProcessRequest{Command: []string{"true"}}, grpc.Trailer(&trailer))
	elapsed := time.Since(start)
	if status.Code(err) != codes.ResourceExhausted {
		t.Fatalf("code %v (%v), want ResourceExhausted", status.Code(err), err)
	}
	if msg := status.Convert(err).Message(); !strings.Contains(msg, "no worker has room") {
		t.Errorf("message %q should say why", msg)
	}
	if got := startFailure(trailer); got != apiservice.StartFailureNoCapacity {
		t.Errorf("trailer = %q, want %q", got, apiservice.StartFailureNoCapacity)
	}
	if elapsed < 500*time.Millisecond || elapsed > 3*time.Second {
		t.Errorf("took %s, want about the 500ms grace", elapsed)
	}
}

// Without a grace (the default) a full fleet keeps waiting, as before, and
// times out with the wake budget's Unavailable.
func TestNoCapacityWaitsWithoutAGrace(t *testing.T) {
	te := newFullTestEnv(t)
	te.srv.WakeBudget = 1500 * time.Millisecond
	routerFull(te)
	envCtx := registerGuest(t, te, "full-default")
	te.control.SetState("full-default", ateapipb.ActorState_ACTOR_STATE_SUSPENDED)

	var trailer metadata.MD
	start := time.Now()
	_, err := te.procClient.StartProcess(envCtx, &ateenvv1alpha.StartProcessRequest{Command: []string{"true"}}, grpc.Trailer(&trailer))
	if status.Code(err) != codes.Unavailable {
		t.Fatalf("code %v (%v), want Unavailable after the wake budget", status.Code(err), err)
	}
	if elapsed := time.Since(start); elapsed < 1500*time.Millisecond {
		t.Errorf("returned after %s, before the wake budget", elapsed)
	}
	if got := startFailure(trailer); got != "" {
		t.Errorf("a timeout is not a start failure; trailer = %q", got)
	}
}

// Capacity that comes back within the grace lets the call through.
func TestNoCapacityThatClearsWithinTheGraceWakes(t *testing.T) {
	te := newFullTestEnv(t)
	te.srv.WakeNoCapacityGrace = 3 * time.Second
	routerFull(te)
	envCtx := registerGuest(t, te, "full-then-room")
	te.control.SetState("full-then-room", ateapipb.ActorState_ACTOR_STATE_SUSPENDED)
	go func() {
		time.Sleep(700 * time.Millisecond)
		te.control.SetState("full-then-room", ateapipb.ActorState_ACTOR_STATE_RUNNING)
	}()

	if _, err := te.procClient.StartProcess(envCtx, &ateenvv1alpha.StartProcessRequest{Command: []string{"true"}}); err != nil {
		t.Fatalf("StartProcess after room freed up: %v", err)
	}
}

// Other Unavailable answers, such as a guest that is not yet listening, do
// not start the no-capacity clock.
func TestOtherUnavailableAnswersDoNotCountAsNoCapacity(t *testing.T) {
	te := newFullTestEnv(t)
	te.srv.WakeNoCapacityGrace = 300 * time.Millisecond
	te.srv.WakeBudget = 1500 * time.Millisecond
	te.router.NotRunningBody = func(id string) string { return "actor ate-env/" + id + " unavailable" }
	envCtx := registerGuest(t, te, "plain-unavailable")
	te.control.SetState("plain-unavailable", ateapipb.ActorState_ACTOR_STATE_SUSPENDED)

	_, err := te.procClient.StartProcess(envCtx, &ateenvv1alpha.StartProcessRequest{Command: []string{"true"}})
	if status.Code(err) != codes.Unavailable {
		t.Fatalf("code %v (%v), want the wake budget's Unavailable", status.Code(err), err)
	}
}
