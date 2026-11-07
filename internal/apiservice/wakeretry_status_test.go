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
	"io"
	"log"
	"testing"
	"testing/synctest"
	"time"

	ateapipb "go.ate.dev/substrate/api/ateapi/v1"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// slowStateRead is a readStatus that answers state after latency, or the
// context's error if ctx ends first, as a gRPC call to the control plane
// does.
func slowStateRead(latency time.Duration, state ateapipb.ActorState) func(context.Context) (*ateapipb.ActorStatus, error) {
	return func(ctx context.Context) (*ateapipb.ActorStatus, error) {
		select {
		case <-ctx.Done():
			return nil, status.FromContextError(ctx.Err()).Err()
		case <-time.After(latency):
			return &ateapipb.ActorStatus{State: state}, nil
		}
	}
}

// A transient control-plane error on the state read (WF-5, and WF-3 for a
// control-plane ResourceExhausted) leaves the state unknown: the wait backs
// off and probes again instead of ending at once with the read's error.
func TestAwaitWakeRetriesTransientStateReadErrors(t *testing.T) {
	prevOut := log.Writer()
	log.SetOutput(io.Discard)
	t.Cleanup(func() { log.SetOutput(prevOut) })

	for _, code := range []codes.Code{codes.Unavailable, codes.DeadlineExceeded, codes.ResourceExhausted} {
		t.Run(code.String(), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				s := &Server{WakeBudget: time.Minute, WakeRunningBudget: 30 * time.Second, WakeNoCapacityGrace: 10 * time.Second}
				probes, reads := 0, 0
				probe := func(context.Context) error {
					probes++
					if probes >= 3 {
						return status.Error(codes.NotFound, "no such process") // the guest answers
					}
					return status.Error(codes.Unavailable, "503: upstream connect error")
				}
				readStatus := func(context.Context) (*ateapipb.ActorStatus, error) {
					reads++
					if reads == 1 {
						return nil, toGRPCError(status.Error(code, "ate-api: transient"))
					}
					return &ateapipb.ActorStatus{State: ateapipb.ActorState_ACTOR_STATE_RESUMING}, nil
				}
				if err := s.awaitWake(context.Background(), "ns", "env", probe, readStatus); err != nil {
					t.Fatalf("awaitWake = %v after %d probes, want the guest's answer (nil)", err, probes)
				}
				if probes != 3 {
					t.Fatalf("probes = %d, want 3", probes)
				}
			})
		})
	}
}

// A state read that is not transient still ends the wait with its error:
// NotFound means the environment is gone.
func TestAwaitWakeEndsOnNotFoundStateRead(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s := &Server{WakeBudget: time.Minute, WakeRunningBudget: 30 * time.Second}
		probes := 0
		probe := func(context.Context) error {
			probes++
			return status.Error(codes.Unavailable, "503: upstream connect error")
		}
		readStatus := func(context.Context) (*ateapipb.ActorStatus, error) {
			return nil, status.Error(codes.NotFound, "actor not found")
		}
		err := s.awaitWake(context.Background(), "ns", "env", probe, readStatus)
		if status.Code(err) != codes.NotFound || probes != 1 {
			t.Fatalf("awaitWake = %v after %d probes, want NotFound after 1", err, probes)
		}
	})
}

// A read that answers NotFound at the instant its deadline passes still ends
// the wait with NotFound: the control plane's answer wins over the deadline.
func TestAwaitWakeNotFoundAtReadDeadlineEndsTheWait(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		const budget = 10 * time.Second
		s := &Server{WakeBudget: budget, WakeRunningBudget: time.Minute}
		probe := func(context.Context) error {
			return status.Error(codes.Unavailable, "503: upstream connect error")
		}
		readStatus := func(context.Context) (*ateapipb.ActorStatus, error) {
			time.Sleep(budget) // answers just as the read's deadline passes
			return nil, status.Error(codes.NotFound, "actor not found")
		}
		if err := s.awaitWake(context.Background(), "ns", "env", probe, readStatus); status.Code(err) != codes.NotFound {
			t.Fatalf("awaitWake = %v, want NotFound", err)
		}
	})
}

// WF-6: a slow state read after the last probe cannot push the wait past
// WakeBudget; the probes here answer at once, so the wait ends at the budget.
func TestAwaitWakeStateReadBoundedByWakeBudget(t *testing.T) {
	prevOut := log.Writer()
	log.SetOutput(io.Discard)
	t.Cleanup(func() { log.SetOutput(prevOut) })

	synctest.Test(t, func(t *testing.T) {
		const budget = 10 * time.Second
		s := &Server{WakeBudget: budget, WakeRunningBudget: time.Minute}
		probe := func(context.Context) error {
			return status.Error(codes.Unavailable, "503: parking budget expired")
		}
		start := time.Now()
		err := s.awaitWake(context.Background(), "ns", "env", probe,
			slowStateRead(2*time.Second, ateapipb.ActorState_ACTOR_STATE_RESUMING))
		elapsed := time.Since(start)
		if status.Code(err) != codes.Unavailable {
			t.Fatalf("awaitWake = %v, want the Unavailable timeout", err)
		}
		if elapsed != budget {
			t.Fatalf("WF-6: the wait took %s, want exactly the wake budget %s (probes take no time)", elapsed, budget)
		}
	})
}

// WF-4: a slow state read cannot delay the probe that ends a no-capacity run
// past WakeNoCapacityGrace + 1 ms. Probe 1 answers no-room at 9.9 s, under the
// 10 s grace; the 2 s read is cut at the grace and the next probe ends the run.
func TestAwaitWakeStateReadBoundedByNoCapacityGrace(t *testing.T) {
	prevOut := log.Writer()
	log.SetOutput(io.Discard)
	t.Cleanup(func() { log.SetOutput(prevOut) })

	synctest.Test(t, func(t *testing.T) {
		const grace = 10 * time.Second
		s := &Server{WakeBudget: 5 * time.Minute, WakeRunningBudget: time.Minute, WakeNoCapacityGrace: grace}
		var starts []time.Time
		probe := func(context.Context) error {
			starts = append(starts, time.Now())
			if len(starts) == 1 {
				time.Sleep(9900 * time.Millisecond)
			}
			return status.Error(codes.Unavailable, "503: "+routerNoCapacityText)
		}
		runStart := time.Now()
		err := s.awaitWake(context.Background(), "ns", "env", probe,
			slowStateRead(2*time.Second, ateapipb.ActorState_ACTOR_STATE_SUSPENDED))
		if status.Code(err) != codes.ResourceExhausted {
			t.Fatalf("awaitWake = %v, want ResourceExhausted", err)
		}
		last := starts[len(starts)-1]
		if late := last.Sub(runStart) - grace; late > time.Millisecond {
			t.Fatalf("WF-4: the probe that ended the run (attempt %d) started %s after the run began, %s past the grace %s",
				len(starts), last.Sub(runStart), late, grace)
		}
	})
}
