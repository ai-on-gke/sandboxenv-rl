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

package apiservice

import (
	"context"
	"log"
	"strings"
	"time"

	"github.com/agent-substrate/env/internal/routertls"
	"github.com/agent-substrate/env/internal/telemetry"
	ateenvv1alpha "github.com/agent-substrate/env/proto/ateenv/v1alpha"
	ateapipb "go.ate.dev/substrate/api/ateapi/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

// DefaultWakeBudget bounds how long a guest call waits for an environment
// that the control plane reports as suspended or in transition (resuming,
// pausing, suspending) to wake before the error reaches the caller. The first
// request that reaches the router is what resumes a suspended actor, and the
// router holds that request only for its own parking budget, a few seconds. A
// wake that has to pull the environment's image onto the node or read its
// snapshot across a region takes longer than that: 18 s for a cold image pull
// and 7 s for a 1 GiB snapshot read across regions, measured on GKE. The
// control plane reports RUNNING only after the template's wakeup probe
// passes, so the budget must also cover that probe's timeout: 120 s for a
// template with a sidecar readiness URL (ate-env manifest template
// --wakeup-timeout). A cold restore of a 9 GB image on a node that is
// unpacking other images takes more than two minutes while still making
// progress, so the budget is five minutes: that restore, or the probe
// timeout plus the pull. A crashed or deleted environment still fails at
// once, not after the budget.
const DefaultWakeBudget = 5 * time.Minute

// DefaultWakeRunningBudget bounds the wait when the control plane already
// reports the environment RUNNING but the router still answers Unavailable.
// That has two causes: the guest is not yet listening after a restore, which
// resolves in seconds, or the path to the worker is down, which does not.
// A short budget covers the first without turning the second into a
// two-minute hang per call.
const DefaultWakeRunningBudget = 20 * time.Second

const (
	wakeRetryInitialBackoff = 200 * time.Millisecond
	wakeRetryMaxBackoff     = 2 * time.Second
)

// StartFailureKey is the trailing metadata key on a guest call that failed
// because the environment cannot start, so clients can tell that failure
// from any other without parsing the message: retry the work on a fresh
// environment rather than score it. Its value is StartFailureCrashed or
// StartFailureNoCapacity.
const StartFailureKey = "ate-env-start-failure"

const (
	// StartFailureCrashed: the control plane moved the environment to
	// CRASHED while it was waking (FailedPrecondition; the message carries
	// the control plane's crash reason).
	StartFailureCrashed = "crashed"
	// StartFailureNoCapacity: no worker had room to run the environment for
	// the whole WakeNoCapacityGrace (ResourceExhausted).
	StartFailureNoCapacity = "no-capacity"
)

// routerNoCapacityText is how the atenet router describes a resume that the
// scheduler could not place: it keeps the ResourceExhausted description,
// "no worker has room for the actor", in the 503 body that reaches a gRPC
// caller as an Unavailable message.
const routerNoCapacityText = "no worker has room for the actor"

// routerNoCapacity reports whether err is the router saying that no worker
// has room for the actor.
func routerNoCapacity(err error) bool {
	return err != nil && strings.Contains(err.Error(), routerNoCapacityText)
}

// setStartFailure marks the call's trailers with why the environment cannot
// start. Outside a gRPC handler (no server transport in ctx) it does nothing.
func setStartFailure(ctx context.Context, reason string) {
	_ = grpc.SetTrailer(ctx, metadata.Pairs(StartFailureKey, reason))
}

// ensureAwake sends a trivial request to the environment's guest over conn
// and repeats it while the router answers Unavailable, which is what an
// expired parking budget and a guest that is not yet listening both look
// like. It returns when the guest answers anything else, when the actor has
// crashed, when ctx ends, or when a budget is spent: WakeBudget bounds the
// whole wait, and WakeRunningBudget bounds the part of it that starts with
// the first attempt that sees the actor RUNNING, so a long suspended phase
// does not eat the time the guest needs to start listening after the state
// flips. Because the probe is the first request through the router, it is
// also what wakes the actor; the calls that follow on conn find it up. The
// state is read once per attempt, and the backoff caps attempts at roughly
// one per two seconds, so a full four-minute wait costs about 120
// control-plane reads.
//
// Two failures end the wait early, because waiting cannot fix them, and mark
// the call's trailers with StartFailureKey: an actor the control plane moved
// to CRASHED (FailedPrecondition, with the control plane's crash reason), and,
// when WakeNoCapacityGrace is set, a router that has said for that long that
// no worker has room for the actor (ResourceExhausted). Without the grace a
// fleet that is full waits the whole WakeBudget, as capacity can come back.
//
// With Metrics set, a wait (a first probe that found the guest unreachable)
// is observed in ate_env_api_wake_wait_seconds with its outcome, and the
// guest's first answer after CreateEnvironment ends create_to_ready.
func (s *Server) ensureAwake(ctx context.Context, conn *grpc.ClientConn, atespace, id string) error {
	client := ateenvv1alpha.NewProcessServiceClient(conn)
	probe := func(ctx context.Context) error {
		_, err := client.GetProcess(ctx, &ateenvv1alpha.GetProcessRequest{ProcessId: "ate-env-wake-probe"})
		return err
	}
	readStatus := func(ctx context.Context) (*ateapipb.ActorStatus, error) {
		return s.actorStatus(ctx, atespace, id)
	}
	return s.awaitWake(ctx, atespace, id, probe, readStatus)
}

// awaitWake is ensureAwake with its two reads passed in: probe sends the
// trivial guest request through the router, and readStatus reads the actor's
// status from the control plane. The property test in
// wakeretry_property_test.go drives it with scripted answers on a fake clock.
func (s *Server) awaitWake(ctx context.Context, atespace, id string,
	probe func(context.Context) error,
	readStatus func(context.Context) (*ateapipb.ActorStatus, error),
) (retErr error) {
	start := time.Now()
	waited, outcome := false, telemetry.WakeOutcomeError
	defer func() {
		if s.Metrics == nil {
			return
		}
		if retErr == nil {
			s.finishCreate(atespace, id, telemetry.ReadyStatusOK)
			outcome = telemetry.WakeOutcomeWoke
		}
		if waited {
			s.Metrics.ObserveWakeWait(outcome, time.Since(start))
		}
	}()
	budget := s.WakeBudget
	if budget <= 0 {
		budget = DefaultWakeBudget
	}
	runningBudget := s.WakeRunningBudget
	if runningBudget <= 0 {
		runningBudget = DefaultWakeRunningBudget
	}
	var runningSince time.Time // first attempt that saw the actor RUNNING
	// When the first probe of the current run of no-room answers started:
	// the router parks a probe for its parking budget before it answers
	// no-room, so that wait counts toward the grace.
	var noCapacitySince time.Time
	backoff := wakeRetryInitialBackoff
	for attempt := 1; ; attempt++ {
		probeStart := time.Now()
		err := probe(ctx)
		probeDone := time.Now()
		if status.Code(err) != codes.Unavailable {
			// Anything else, NotFound for the probe id included, came from
			// the guest itself.
			return nil
		}
		if routertls.RejectedCertificate(err) {
			// The router turned the API's certificate away: no wait fixes
			// that, and every environment fails the same way.
			return status.Errorf(codes.FailedPrecondition, "the atenet router rejected ate-env-api's client certificate%s: %v; %s",
				identityNote(s.RouterIdentity), err, routertls.RejectionAdvice)
		}
		waited = true
		st, serr := readStatus(ctx)
		if serr != nil {
			return serr
		}
		state := st.GetState()
		if state == ateapipb.ActorState_ACTOR_STATE_CRASHED {
			s.finishCreate(atespace, id, telemetry.ReadyStatusCrashed)
			setStartFailure(ctx, StartFailureCrashed)
			if reason := st.GetCrash().GetMessage(); reason != "" {
				return status.Errorf(codes.FailedPrecondition, "environment %s/%s crashed while waking: %s", atespace, id, reason)
			}
			return status.Errorf(codes.FailedPrecondition, "environment %s/%s crashed while waking: %v", atespace, id, err)
		}
		if state == ateapipb.ActorState_ACTOR_STATE_DELETING {
			// It will not wake; the port proxy answers 404 for it too.
			return status.Errorf(codes.NotFound, "environment %s/%s is being deleted", atespace, id)
		}
		allowed, since, which := budget, start, "wake budget"
		if state == ateapipb.ActorState_ACTOR_STATE_RUNNING {
			if runningSince.IsZero() {
				runningSince = time.Now()
			}
			allowed, since, which = runningBudget, runningSince, "running budget"
		} else {
			runningSince = time.Time{}
		}
		if routerNoCapacity(err) && state != ateapipb.ActorState_ACTOR_STATE_RUNNING {
			if noCapacitySince.IsZero() {
				noCapacitySince = probeStart
			}
		} else {
			noCapacitySince = time.Time{}
		}
		// Measured to the probe's answer, so the state read above does not
		// count toward the grace.
		if grace := s.WakeNoCapacityGrace; grace > 0 && !noCapacitySince.IsZero() && probeDone.Sub(noCapacitySince) >= grace {
			setStartFailure(ctx, StartFailureNoCapacity)
			return status.Errorf(codes.ResourceExhausted, "environment %s/%s cannot start: no worker has had room for it for the no-capacity grace of %s (%d attempts): %v",
				atespace, id, grace, attempt, err)
		}
		remaining := allowed - time.Since(since)
		if overall := budget - time.Since(start); overall < remaining {
			remaining, allowed, which = overall, budget, "wake budget"
		}
		if remaining <= 0 {
			outcome = telemetry.WakeOutcomeTimeout
			return status.Errorf(codes.Unavailable, "environment %s/%s did not answer within the %s of %s while %s (%d attempts): %v",
				atespace, id, which, allowed, shortState(state), attempt, err)
		}
		if attempt == 1 {
			log.Printf("waiting for environment %s/%s to wake (state %s, %s %s)", atespace, id, shortState(state), which, allowed)
		}
		select {
		case <-ctx.Done():
			return status.FromContextError(ctx.Err()).Err()
		case <-time.After(min(backoff, remaining, s.noCapacityLeft(noCapacitySince))):
		}
		backoff = min(backoff*2, wakeRetryMaxBackoff)
	}
}

func identityNote(id string) string {
	if id == "" {
		return ""
	}
	return " (" + id + ")"
}

// ActorState is the state read ensureAwake uses, for the port proxy
// (portproxy.Options.ActorState): a deleted environment is NotFound.
func (s *Server) ActorState(ctx context.Context, atespace, id string) (ateapipb.ActorState, error) {
	return s.actorState(ctx, atespace, id)
}

// actorState reads the actor's state from the control plane. A deleted
// environment surfaces here as NotFound (through toGRPCError), which is the
// right answer to a caller still holding its handle.
func (s *Server) actorState(ctx context.Context, atespace, id string) (ateapipb.ActorState, error) {
	st, err := s.actorStatus(ctx, atespace, id)
	return st.GetState(), err
}

// actorStatus is actorState with the rest of the status, such as the crash
// reason.
func (s *Server) actorStatus(ctx context.Context, atespace, id string) (*ateapipb.ActorStatus, error) {
	actor, err := s.client.Get(ctx, atespace, id)
	if err != nil {
		return nil, toGRPCError(err)
	}
	return actor.GetStatus(), nil
}

// noCapacityLeft is how long the current run of no-room answers may go on
// before ensureAwake fails it, so the backoff does not sleep past the grace.
// It is unbounded when there is no grace or no such run.
func (s *Server) noCapacityLeft(since time.Time) time.Duration {
	if s.WakeNoCapacityGrace <= 0 || since.IsZero() {
		return wakeRetryMaxBackoff
	}
	return max(s.WakeNoCapacityGrace-time.Since(since), time.Millisecond)
}

func shortState(st ateapipb.ActorState) string {
	return strings.TrimPrefix(st.String(), "ACTOR_STATE_")
}
