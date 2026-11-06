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

// Package synctestrapid bridges pgregory.net/rapid property-based testing and
// stateful rapid.StateMachine execution with Go's testing/synctest bubbles.
//
// Two runtime constraints govern how rapid and testing/synctest compose:
//
//  1. rapid.Check calls t.Deadline() and measures shrink budgets against wall
//     time, while synctest.Test forbids calling T.Deadline, T.Run, or
//     T.Parallel on the bubbled *testing.T. Therefore rapid.Check must wrap
//     synctest.Test from the outside, creating one fresh isolated bubble (and
//     fake clock starting at 2000-01-01 00:00:00 UTC) per generated or shrunk
//     test case.
//  2. rapid uses sentinel panics (stopTest from rt.Fatalf/rt.FailNow and
//     invalidData from rt.Skip or bitstream overrun) inside checkOnce to detect
//     falsified properties, skip invalid draws, and shrink failing inputs.
//     synctest.Test runs its callback on a child goroutine via testing.tRunner,
//     which would abort the outer *testing.T on an uncaught panic before rapid
//     can shrink. In addition, rapid's shrinker accepts a smaller bitstream
//     only when its panic traceback matches the original failure's traceback
//     (shrinker.accept), so stopTest, invalidData, and unexpected panics must
//     re-panic from distinct frames outside the bubble after bt.Cleanup hooks
//     drain parked goroutines. Within one class, all rt.Fatalf calls share
//     repanicStopTest's traceback, so rapid may shrink to a smaller input that
//     fails a different assertion site.
package synctestrapid

import (
	"reflect"
	"runtime/debug"
	"testing"
	"testing/synctest"

	"pgregory.net/rapid"
)

// Check runs a rapid property where every generated test case (and every
// shrinking attempt) executes inside a fresh testing/synctest bubble.
//
// Assert through rt; use bt for Cleanup only, because failing bt aborts the
// outer test via synctest.Test without shrinking. Register cleanup hooks on bt
// to close open gate channels or cancel contexts so goroutines parked inside
// the bubble exit cleanly when an assertion fails mid-sequence.
func Check(t *testing.T, prop func(bt *testing.T, rt *rapid.T)) {
	t.Helper()
	check(t, t, prop)
}

// Repeat runs a stateful rapid.StateMachine where each generated action
// sequence (and each shrink attempt) executes inside a fresh testing/synctest
// bubble. Actions spawn goroutines, release channel gates, or advance the
// bubble's fake clock with time.Sleep, calling synctest.Wait() to reach exact
// runtime quiescence before Check verifies invariants. Assert through rt; use
// bt for Cleanup only.
func Repeat(t *testing.T, newMachine func(bt *testing.T, rt *rapid.T) rapid.StateMachine) {
	t.Helper()
	repeat(t, t, newMachine)
}

func check(t *testing.T, tb rapid.TB, prop func(bt *testing.T, rt *rapid.T)) {
	t.Helper()
	tb.Helper()
	rapid.Check(tb, func(rt *rapid.T) {
		runBubble(t, rt, func(bt *testing.T) {
			prop(bt, rt)
		})
	})
}

func repeat(t *testing.T, tb rapid.TB, newMachine func(bt *testing.T, rt *rapid.T) rapid.StateMachine) {
	t.Helper()
	tb.Helper()
	check(t, tb, func(bt *testing.T, rt *rapid.T) {
		sm := newMachine(bt, rt)
		rt.Repeat(rapid.StateMachineActions(sm))
	})
}

type panicKind int

const (
	panicNone panicKind = iota
	panicInvalidData
	panicStopTest
	panicUnexpected
)

func classifyPanic(r any) panicKind {
	if typ := reflect.TypeOf(r); typ != nil && typ.PkgPath() == "pgregory.net/rapid" {
		switch typ.Name() {
		case "invalidData":
			return panicInvalidData
		case "stopTest":
			return panicStopTest
		}
	}
	return panicUnexpected
}

func runBubble(t *testing.T, rt *rapid.T, fn func(bt *testing.T)) {
	t.Helper()
	var (
		kind   = panicNone
		caught any
	)
	synctest.Test(t, func(bt *testing.T) {
		defer func() {
			if r := recover(); r != nil {
				kind = classifyPanic(r)
				if kind == panicUnexpected {
					rt.Logf("panic inside synctest bubble: %v\n%s", r, debug.Stack())
				}
				caught = r
			}
		}()
		fn(bt)
	})
	switch kind {
	case panicNone:
		return
	case panicInvalidData:
		repanicInvalidData(caught)
	case panicStopTest:
		repanicStopTest(caught)
	default:
		repanicUnexpected(caught)
	}
}

//go:noinline
func repanicInvalidData(v any) {
	panic(v)
}

//go:noinline
func repanicStopTest(v any) {
	panic(v)
}

//go:noinline
func repanicUnexpected(v any) {
	panic(v)
}
