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

// Stat summarizes one kind of event.
type Stat = rlbench.Stat

// OpRow aliases rlbench.OpRow.
type OpRow = rlbench.OpRow

// Unresolved is an instance whose reward step scored 0.
type Unresolved = rlbench.Unresolved

// StepStat is one training step in steps mode.
type StepStat = rlbench.StepStat

// StepRow aliases rlbench.StepRow.
type StepRow = rlbench.StepRow

// Report is the summary of one run's JSONL.
type Report = rlbench.Report

// ErrorCount is one error string and how often it occurred.
type ErrorCount = rlbench.ErrorCount

// Failure aliases rlbench.Failure.
type Failure = rlbench.Failure

// Summarize reads JSONL events and builds the report.
func Summarize(r io.Reader) (*Report, error) {
	return rlbench.Summarize(r)
}

// Build computes the report from events in memory.
func Build(events []Event) *Report {
	return rlbench.Build(events)
}

func verdict(tail string) string                      { return rlbench.Verdict(tail) }
func percentile(sorted []float64, p float64) float64 { return rlbench.Percentile(sorted, p) }
