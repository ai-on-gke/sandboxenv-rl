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
	"io"

	"github.com/agent-substrate/env/internal/benchmarking/rlbench"
)

// Event names. Every timed operation of a run is one of these.
const (
	EventCreate  = rlbench.EventCreate
	EventTTFI    = rlbench.EventTTFI
	EventWake    = rlbench.EventWake
	EventStep    = rlbench.EventStep
	EventTask    = rlbench.EventTask
	EventSuspend = rlbench.EventSuspend
	EventDelete  = rlbench.EventDelete
	EventBarrier = rlbench.EventBarrier
	EventPark    = rlbench.EventPark
	EventRun     = rlbench.EventRun
)

// Event is one JSONL record.
type Event = rlbench.Event

// Recorder writes events as JSON lines.
type Recorder = rlbench.Recorder

// NewRecorder writes to w; a nil w discards events.
func NewRecorder(w io.Writer) *Recorder {
	return rlbench.NewRecorder(w)
}

func intp(v int) *int { return &v }
