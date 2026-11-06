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

package synctestrapid

import (
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"pgregory.net/rapid"
)

// Skipping invalid draws inside a synctest bubble must discard only that draw
// rather than failing the outer *testing.T, and fake-clock sleeps inside each
// bubble must start fresh without leaking goroutines.
func TestCheckSupportsSkipsAndFakeClock(t *testing.T) {
	Check(t, func(bt *testing.T, rt *rapid.T) {
		n := rapid.IntRange(0, 5).Draw(rt, "n")
		if n == 0 {
			rt.Skip("exercise rapid invalidData skip across synctest boundary")
		}

		gate := make(chan struct{})
		done := make(chan time.Duration, 1)
		closed := false
		bt.Cleanup(func() {
			if !closed {
				close(gate)
			}
		})

		start := time.Now()
		go func() {
			<-gate
			time.Sleep(time.Duration(n) * time.Minute)
			done <- time.Since(start)
		}()

		synctest.Wait()
		select {
		case <-done:
			rt.Fatalf("worker finished before gate closed")
		default:
		}

		closed = true
		close(gate)
		synctest.Wait()

		// Advance fake time so the worker's Sleep completes instantaneously.
		time.Sleep(time.Duration(n) * time.Minute)
		synctest.Wait()

		got := <-done
		want := time.Duration(n) * time.Minute
		if got != want {
			rt.Fatalf("elapsed fake time = %v, want %v", got, want)
		}
	})
}

type fakeTB struct {
	name   string
	failed bool
	logs   []string
	errors []string
}

func (f *fakeTB) Helper() {}
func (f *fakeTB) Name() string {
	if f.name != "" {
		return f.name
	}
	return "fakeTB"
}
func (f *fakeTB) Logf(format string, args ...any) {
	f.logs = append(f.logs, fmt.Sprintf(format, args...))
}
func (f *fakeTB) Log(args ...any) {
	f.logs = append(f.logs, fmt.Sprint(args...))
}
func (f *fakeTB) Errorf(format string, args ...any) {
	f.failed = true
	f.errors = append(f.errors, fmt.Sprintf(format, args...))
}
func (f *fakeTB) Error(args ...any) {
	f.failed = true
	f.errors = append(f.errors, fmt.Sprint(args...))
}
func (f *fakeTB) Fatalf(format string, args ...any) {
	f.Errorf(format, args...)
	f.FailNow()
}
func (f *fakeTB) Fatal(args ...any) {
	f.Error(args...)
	f.FailNow()
}
func (f *fakeTB) FailNow()             { f.failed = true; panic(f) }
func (f *fakeTB) Fail()                { f.failed = true }
func (f *fakeTB) Failed() bool         { return f.failed }
func (f *fakeTB) Skipf(string, ...any) {}
func (f *fakeTB) Skip(...any)          {}
func (f *fakeTB) SkipNow()             {}
func (f *fakeTB) Skipped() bool        { return false }

func runExpectingFailure(tb *fakeTB, fn func()) {
	defer func() {
		if r := recover(); r != nil && r != tb {
			panic(r)
		}
	}()
	fn()
}

// A falsified property inside Check must shrink to the minimal counterexample,
// log the shrunk draw values, write a non-empty fail file, and replay that fail
// file cleanly without degenerating into "invalid data: overrun".
func TestCheckShrinksCounterexampleAndReplaysFailFile(t *testing.T) {
	t.Chdir(t.TempDir())

	prop := func(_ *testing.T, rt *rapid.T) {
		xs := rapid.SliceOfN(rapid.IntRange(0, 9), 0, 8).Draw(rt, "xs")
		if slices.Contains(xs, 7) {
			rt.Fatalf("found 7 in %v", xs)
		}
	}

	first := &fakeTB{name: "TestShrinkSliceWith7"}
	runExpectingFailure(first, func() {
		check(t, first, prop)
	})
	if !first.Failed() {
		t.Fatal("expected check to fail when slice contains 7")
	}
	errOut := strings.Join(first.errors, "\n")
	if strings.Contains(errOut, "overrun") {
		t.Fatalf("shrink degenerated into bitstream overrun:\n%s", errOut)
	}
	if !strings.Contains(errOut, "[rapid] failed after") || !strings.Contains(errOut, "found 7 in [7]") {
		t.Fatalf("unexpected error output:\n%s", errOut)
	}
	logOut := strings.Join(first.logs, "\n")
	if !strings.Contains(logOut, "[rapid] draw xs: []int{7}") {
		t.Fatalf("expected shrunk draw []int{7} in logs, got:\n%s", logOut)
	}

	matches, err := filepath.Glob("testdata/rapid/TestShrinkSliceWith7/*.fail")
	if err != nil || len(matches) != 1 {
		t.Fatalf("fail files = %v (err=%v), want 1 fail file", matches, err)
	}

	replay := &fakeTB{name: "TestShrinkSliceWith7"}
	runExpectingFailure(replay, func() {
		check(t, replay, prop)
	})
	replayErr := strings.Join(replay.errors, "\n")
	if !strings.Contains(replayErr, "found 7 in [7]") || strings.Contains(replayErr, "overrun") {
		t.Fatalf("fail-file replay produced unexpected error:\n%s", replayErr)
	}
}

// Verify that when a gated multi-goroutine state machine falsifies an invariant
// mid-flight (with parked goroutines still waiting on channels in the bubble),
// bt.Cleanup drains the bubble without deadlock AND rapid shrinks the trace to
// the minimal failing interleaving without hitting "invalid data: overrun".
type buggyCounterMachine struct {
	mu      sync.Mutex
	live    int
	gates   []chan struct{}
	aborted bool
	steps   []string
	onFail  func([]string)
}

func (m *buggyCounterMachine) record(step string) {
	m.steps = append(m.steps, step)
}

func (m *buggyCounterMachine) Noop(_ *rapid.T) {
	m.record("Noop")
}

func (m *buggyCounterMachine) StartOverlappingCreate(_ *rapid.T) {
	m.record("StartOverlappingCreate")
	gate := make(chan struct{})
	m.gates = append(m.gates, gate)

	// Intentional bug: checks `live` before parking on gate, ignoring other
	// in-flight creates, so two overlapping creates both pass.
	m.mu.Lock()
	need := 1 - m.live
	m.mu.Unlock()
	if need <= 0 {
		close(gate)
		m.gates = m.gates[:len(m.gates)-1]
		return
	}

	go func() {
		<-gate
		m.mu.Lock()
		defer m.mu.Unlock()
		if m.aborted {
			return
		}
		m.live += need
	}()
	synctest.Wait()
}

func (m *buggyCounterMachine) ReleaseGates(rt *rapid.T) {
	if len(m.gates) == 0 {
		rt.Skip("no gates open")
	}
	m.record("ReleaseGates")
	for _, g := range m.gates {
		close(g)
	}
	m.gates = nil
	synctest.Wait()
}

func (m *buggyCounterMachine) Check(rt *rapid.T) {
	m.mu.Lock()
	live := m.live
	m.mu.Unlock()
	if live > 1 {
		if m.onFail != nil {
			m.onFail(append([]string(nil), m.steps...))
		}
		rt.Fatalf("over-provisioned: live=%d exceeds max=1", live)
	}
}

func TestRepeatShrinksBuggyInterleavingCleanly(t *testing.T) {
	t.Chdir(t.TempDir())

	var lastTrace []string
	probeTB := &fakeTB{name: "TestBuggyCounterMachine"}
	runExpectingFailure(probeTB, func() {
		repeat(t, probeTB, func(bt *testing.T, _ *rapid.T) rapid.StateMachine {
			sm := &buggyCounterMachine{
				onFail: func(steps []string) {
					lastTrace = steps
				},
			}
			bt.Cleanup(func() {
				sm.mu.Lock()
				sm.aborted = true
				sm.mu.Unlock()
				for _, g := range sm.gates {
					close(g)
				}
				sm.gates = nil
			})
			return sm
		})
	})

	if !probeTB.Failed() {
		t.Fatal("expected buggyCounterMachine to fail repeat")
	}
	errOut := strings.Join(probeTB.errors, "\n")
	if strings.Contains(errOut, "overrun") {
		t.Fatalf("state-machine shrink degenerated into bitstream overrun:\n%s", errOut)
	}
	if !strings.Contains(errOut, "[rapid] failed after") || !strings.Contains(errOut, "over-provisioned: live=2 exceeds max=1") {
		t.Fatalf("unexpected error output:\n%s", errOut)
	}

	var nonNoop []string
	for _, step := range lastTrace {
		if step != "Noop" {
			nonNoop = append(nonNoop, step)
		}
	}
	want := []string{"StartOverlappingCreate", "StartOverlappingCreate", "ReleaseGates"}
	if !slices.Equal(nonNoop, want) {
		t.Fatalf("shrunk non-noop trace = %v (full trace = %v), want %v", nonNoop, lastTrace, want)
	}
}

// TestRepeatSmoke verifies the exported Repeat entrypoint on a valid state machine.
type validPingMachine struct {
	count int
}

func (m *validPingMachine) Ping(_ *rapid.T) {
	done := make(chan struct{})
	go func() {
		time.Sleep(10 * time.Millisecond)
		close(done)
	}()
	time.Sleep(10 * time.Millisecond)
	synctest.Wait()
	<-done
	m.count++
}

func (m *validPingMachine) Check(rt *rapid.T) {
	if m.count < 0 {
		rt.Fatalf("negative count %d", m.count)
	}
}

func TestRepeatSmoke(t *testing.T) {
	Repeat(t, func(_ *testing.T, _ *rapid.T) rapid.StateMachine {
		return &validPingMachine{}
	})
}

func TestCheckShrinksRuntimePanic(t *testing.T) {
	t.Chdir(t.TempDir())

	probeTB := &fakeTB{name: "TestRuntimePanicShrink"}
	runExpectingFailure(probeTB, func() {
		check(t, probeTB, func(_ *testing.T, rt *rapid.T) {
			n := rapid.IntRange(0, 100).Draw(rt, "n")
			if n >= 50 {
				var empty []int
				_ = empty[0]
			}
		})
	})

	if !probeTB.Failed() {
		t.Fatal("expected runtime panic to fail check")
	}
	errOut := strings.Join(probeTB.errors, "\n")
	if strings.Contains(errOut, "overrun") {
		t.Fatalf("runtime panic shrink degenerated into bitstream overrun:\n%s", errOut)
	}
	if !strings.Contains(errOut, "[rapid] panic after") || !strings.Contains(errOut, "index out of range") {
		t.Fatalf("unexpected error output:\n%s", errOut)
	}
	logOut := strings.Join(probeTB.logs, "\n")
	if !strings.Contains(logOut, "[rapid] draw n: 50") {
		t.Fatalf("expected n to shrink to 50, got logs:\n%s", logOut)
	}
	if !strings.Contains(logOut, "panic inside synctest bubble:") {
		t.Fatalf("expected in-bubble stack log, got:\n%s", logOut)
	}
}
