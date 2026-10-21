//go:build linux

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

package main

import (
	"os"
	"os/signal"
	"syscall"
	"testing"
	"time"
)

func TestSuperviseReturnsExitCode(t *testing.T) {
	if got := supervise("/bin/sh", []string{"sh", "-c", "exit 3"}); got != 3 {
		t.Fatalf("supervise = %d, want 3", got)
	}
}

func TestSuperviseReturnsSignalExitCode(t *testing.T) {
	if got := supervise("/bin/sh", []string{"sh", "-c", "kill -KILL $$"}); got != 128+int(syscall.SIGKILL) {
		t.Fatalf("supervise = %d, want %d", got, 128+int(syscall.SIGKILL))
	}
}

func TestSuperviseReapsOrphans(t *testing.T) {
	// Spawn 20 orphaned grandchildren whose intermediate subshells exit
	// immediately while the supervised child continues running.
	script := `for i in $(seq 1 20); do (sh -c 'exit 0' &); done; sleep 0.3`
	if got := supervise("/bin/sh", []string{"sh", "-c", script}); got != 0 {
		t.Fatalf("supervise = %d, want 0", got)
	}
	var status syscall.WaitStatus
	if pid, err := syscall.Wait4(-1, &status, syscall.WNOHANG, nil); err != syscall.ECHILD {
		t.Fatalf("Wait4 = (%d, %v), want ECHILD: an orphaned child was left unreaped", pid, err)
	}
}

func TestSuperviseForwardsSignals(t *testing.T) {
	// The test sends SIGTERM to its own process until supervise forwards one.
	// ignore keeps SIGTERM caught for the whole test, so a signal that
	// supervise does not take cannot end the test binary.
	ignore := make(chan os.Signal, 1)
	signal.Notify(ignore, syscall.SIGTERM)

	done := make(chan int, 1)
	go func() {
		done <- supervise("/bin/sh", []string{"sh", "-c", "exec sleep 30"})
	}()

	tick := time.NewTicker(10 * time.Millisecond)
	timeout := time.After(10 * time.Second)
	got, finished := 0, false
loop:
	for {
		select {
		case got = <-done:
			finished = true
			break loop
		case <-tick.C:
			_ = syscall.Kill(syscall.Getpid(), syscall.SIGTERM)
		case <-timeout:
			break loop
		}
	}
	// Stop sending, then drain before signal.Stop restores the default
	// action: a SIGTERM that arrives after it would end the test binary.
	tick.Stop()
	for drained := false; !drained; {
		select {
		case <-ignore:
		case <-time.After(100 * time.Millisecond):
			drained = true
		}
	}
	signal.Stop(ignore)

	if !finished {
		t.Fatal("SIGTERM was not forwarded to the supervised child")
	}
	if got != 128+int(syscall.SIGTERM) {
		t.Fatalf("supervise = %d, want %d", got, 128+int(syscall.SIGTERM))
	}
}
