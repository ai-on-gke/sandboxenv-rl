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
	"flag"
	"io"
	"log"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	ateapipb "go.ate.dev/substrate/api/ateapi/v1"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"pgregory.net/rapid"
)

// wakeAttempt records one probe of awaitWake and, when the probe found the
// guest unreachable, the state the control plane reported after it.
type wakeAttempt struct {
	start, done time.Time
	unavailable bool // the probe answered Unavailable
	noCapacity  bool // ... with the router's "no worker has room" text
	state       ateapipb.ActorState
}

var wakeStates = []ateapipb.ActorState{
	ateapipb.ActorState_ACTOR_STATE_SUSPENDED,
	ateapipb.ActorState_ACTOR_STATE_RESUMING,
	ateapipb.ActorState_ACTOR_STATE_RUNNING,
	ateapipb.ActorState_ACTOR_STATE_CRASHED,
	ateapipb.ActorState_ACTOR_STATE_DELETING,
}

const maxProbeLatency = 3 * time.Second

// TestAwaitWakeProperties drives awaitWake (ensureAwake's loop) with
// generated router answers, probe latencies, control-plane states and
// budgets, on synctest's fake clock, and checks the fail-fast contract after
// every run (docs/plans/wake-fail-fast.md, WF-1 to WF-6):
//
//   - WF-1: a probe the guest answers ends the wait at once, with no error.
//   - WF-2: an unreachable guest whose actor is CRASHED ends the wait at once
//     with FailedPrecondition, naming the crash reason when there is one;
//     DELETING ends it with NotFound.
//   - WF-3: ResourceExhausted only with a positive no-capacity grace, after a
//     run of no-room answers (actor not RUNNING) that lasted the grace,
//     measured from the first probe's start to the last probe's answer.
//   - WF-4: once such a run has lasted the grace at a probe's answer, that
//     probe is the last one: the wait never runs on past the grace.
//   - WF-5: a timeout (Unavailable) only after the wake budget, or after the
//     running budget counted from the first attempt of the current RUNNING
//     run.
//   - WF-6: the wait never runs past the wake budget by more than one probe.
//
// The fake clock makes each run deterministic and instant, so rapid can try
// hundreds of timelines and shrink a failing one to its shortest form. rapid
// draws a whole timeline first, each run happens in its own synctest bubble,
// and the checks run outside it.
func TestAwaitWakeProperties(t *testing.T) {
	// Runs take microseconds on the fake clock, so try more timelines than
	// rapid's default 100: some orderings (a RUNNING run, a gap, RUNNING
	// again near a budget) need a few hundred. -rapid.checks still wins.
	if f := flag.Lookup("rapid.checks"); f != nil && f.Value.String() == f.DefValue {
		_ = f.Value.Set("2000")
		defer func() { _ = f.Value.Set(f.DefValue) }()
	}
	// awaitWake logs each wait; thousands of runs would flood the output.
	defer log.SetOutput(log.Writer())
	log.SetOutput(io.Discard)
	rapid.Check(t, func(rt *rapid.T) { checkAwaitWake(t, rt) })
}

// wakeStep is what one probe meets: its latency, the router's answer
// (0: the guest answers; 1-4: no room; 5-9: another Unavailable), and the
// state the control plane reports after an unreachable probe (an index into
// wakeStateWeights).
type wakeStep struct {
	latency time.Duration
	answer  int
	state   int
}

// Mostly not-yet-running states, so runs reach the budgets.
var wakeStateWeights = []int{0, 0, 0, 1, 1, 1, 2, 2, 2, 2, 3, 4}

// maxWakeSteps bounds a timeline; a run that outlasts it gets a guest that
// answers.
const maxWakeSteps = 60

func checkAwaitWake(t *testing.T, rt *rapid.T) {
	ms := func(lo, hi int64, label string) time.Duration {
		return time.Duration(rapid.Int64Range(lo, hi).Draw(rt, label)) * time.Millisecond
	}
	s := &Server{
		WakeBudget:        ms(500, 20_000, "wake_budget_ms"),
		WakeRunningBudget: ms(100, 10_000, "running_budget_ms"),
	}
	if rapid.Bool().Draw(rt, "grace_set") {
		s.WakeNoCapacityGrace = ms(50, 10_000, "grace_ms")
	}
	crashReason := rapid.SampledFrom([]string{"", "OOMKilled"}).Draw(rt, "crash_reason")
	steps := rapid.SliceOfN(rapid.Custom(func(t *rapid.T) wakeStep {
		return wakeStep{
			latency: time.Duration(rapid.Int64Range(0, maxProbeLatency.Milliseconds()).Draw(t, "latency_ms")) * time.Millisecond,
			answer:  rapid.IntRange(0, 9).Draw(t, "answer"),
			state:   rapid.IntRange(0, len(wakeStateWeights)-1).Draw(t, "state"),
		}
	}), maxWakeSteps, maxWakeSteps).Draw(rt, "steps")

	var (
		attempts []wakeAttempt
		err      error
		elapsed  time.Duration
		now      time.Time // the fake clock when awaitWake returned
	)
	synctest.Test(t, func(t *testing.T) {
		probe := func(ctx context.Context) error {
			step := wakeStep{}
			if n := len(attempts); n < len(steps) {
				step = steps[n]
			}
			a := wakeAttempt{start: time.Now()}
			time.Sleep(step.latency)
			a.done = time.Now()
			var perr error
			switch {
			case step.answer == 0: // the guest answers (NotFound for the probe id, as in production)
				perr = status.Error(codes.NotFound, "no such process")
			case step.answer <= 4:
				a.unavailable, a.noCapacity = true, true
				perr = status.Error(codes.Unavailable, "503: "+routerNoCapacityText)
			default:
				a.unavailable = true
				perr = status.Error(codes.Unavailable, "503: upstream connect error")
			}
			attempts = append(attempts, a)
			return perr
		}
		readStatus := func(ctx context.Context) (*ateapipb.ActorStatus, error) {
			st := wakeStates[wakeStateWeights[steps[len(attempts)-1].state]]
			attempts[len(attempts)-1].state = st
			out := &ateapipb.ActorStatus{State: st}
			if st == ateapipb.ActorState_ACTOR_STATE_CRASHED && crashReason != "" {
				out.Crash = &ateapipb.ActorCrash{Message: crashReason}
			}
			return out, nil
		}
		start := time.Now()
		err = s.awaitWake(context.Background(), "ns", "env", probe, readStatus)
		now = time.Now()
		elapsed = now.Sub(start)
	})
	code := status.Code(err)
	if len(attempts) == 0 {
		rt.Fatalf("WF-1: returned %v without probing", err)
	}
	last := attempts[len(attempts)-1]

	// WF-1, WF-2: what the last attempt saw decides the result.
	switch {
	case !last.unavailable:
		if err != nil {
			rt.Fatalf("WF-1: the guest answered, but awaitWake returned %v", err)
		}
	case last.state == ateapipb.ActorState_ACTOR_STATE_CRASHED:
		if code != codes.FailedPrecondition {
			rt.Fatalf("WF-2: crashed actor gave %v, want FailedPrecondition", err)
		}
		if crashReason != "" && !strings.Contains(err.Error(), crashReason) {
			rt.Fatalf("WF-2: %q does not name the crash reason %q", err, crashReason)
		}
	case last.state == ateapipb.ActorState_ACTOR_STATE_DELETING:
		if code != codes.NotFound {
			rt.Fatalf("WF-2: deleting actor gave %v, want NotFound", err)
		}
	default:
		if code != codes.ResourceExhausted && code != codes.Unavailable {
			rt.Fatalf("WF-5: an unreachable %s actor gave %v, want ResourceExhausted or Unavailable", last.state, err)
		}
	}
	for i, a := range attempts[:len(attempts)-1] {
		if !a.unavailable || a.state == ateapipb.ActorState_ACTOR_STATE_CRASHED ||
			a.state == ateapipb.ActorState_ACTOR_STATE_DELETING {
			rt.Fatalf("WF-1/WF-2: attempt %d of %d (answered=%t, state %s) should have ended the wait", i+1, len(attempts), !a.unavailable, a.state)
		}
	}

	// WF-3, WF-4: the no-capacity grace.
	var runStart time.Time
	for i, a := range attempts {
		inRun := a.unavailable && a.noCapacity && a.state != ateapipb.ActorState_ACTOR_STATE_RUNNING &&
			a.state != ateapipb.ActorState_ACTOR_STATE_CRASHED && a.state != ateapipb.ActorState_ACTOR_STATE_DELETING
		if !inRun {
			runStart = time.Time{}
			continue
		}
		if runStart.IsZero() {
			runStart = a.start
		}
		if g := s.WakeNoCapacityGrace; g > 0 && a.done.Sub(runStart) >= g {
			if i != len(attempts)-1 || code != codes.ResourceExhausted {
				rt.Fatalf("WF-4: no room for %s (grace %s) at attempt %d, but the wait went on to attempt %d and returned %v",
					a.done.Sub(runStart), g, i+1, len(attempts), err)
			}
		}
	}
	if code == codes.ResourceExhausted {
		g := s.WakeNoCapacityGrace
		if g <= 0 || runStart.IsZero() || last.done.Sub(runStart) < g {
			rt.Fatalf("WF-3: ResourceExhausted with grace %s after a no-room run of %s", g, last.done.Sub(runStart))
		}
		// WF-4: the backoff never sleeps past the grace, so the probe that
		// ends the run starts by then (1 ms is the shortest backoff).
		if late := last.start.Sub(runStart) - g; late > time.Millisecond {
			rt.Fatalf("WF-4: the last probe started %s after the grace %s ran out", late, g)
		}
	}

	// WF-5: a timeout only once a budget is spent.
	if code == codes.Unavailable {
		var runningSince time.Time
		for _, a := range attempts {
			if a.state == ateapipb.ActorState_ACTOR_STATE_RUNNING {
				if runningSince.IsZero() {
					runningSince = a.done // the state read follows the probe's answer
				}
			} else {
				runningSince = time.Time{}
			}
		}
		runningOver := !runningSince.IsZero() && now.Sub(runningSince) >= s.WakeRunningBudget
		if elapsed < s.WakeBudget && !runningOver {
			rt.Fatalf("WF-5: timed out after %s, before the wake budget %s and the running budget %s", elapsed, s.WakeBudget, s.WakeRunningBudget)
		}
	}

	// WF-6: never past the wake budget by more than one probe.
	if elapsed > s.WakeBudget+maxProbeLatency {
		rt.Fatalf("WF-6: waited %s, past the wake budget %s plus one probe (%s)", elapsed, s.WakeBudget, maxProbeLatency)
	}
}
