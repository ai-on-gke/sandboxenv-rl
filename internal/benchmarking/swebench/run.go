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
	"context"
	"log/slog"
	"time"

	"github.com/agent-substrate/env/internal/benchmarking/rlbench"
)

// Mode selects the workload shape.
type Mode = rlbench.Mode

const (
	// ModeEval runs every (image, task) once: create, replay, delete.
	ModeEval = rlbench.ModeEval
	// ModeSteps keeps a fleet across training steps and parks it in between.
	ModeSteps = rlbench.ModeSteps
)

// Park is what happens to the fleet between training steps.
type Park = rlbench.Park

const (
	ParkNone    = rlbench.ParkNone
	ParkSuspend = rlbench.ParkSuspend
	ParkDelete  = rlbench.ParkDelete
)

// DefaultWakeBackoff is the initial pause between first-command retries.
const DefaultWakeBackoff = rlbench.DefaultWakeBackoff

const (
	maxRunIDLen = rlbench.MaxRunIDLen
	maxNameLen  = rlbench.MaxNameLen
)

// Config is one run.
type Config struct {
	RunID      string
	Backend    Backend
	Instances  []Instance
	Trajectory *Trajectory
	Recorder   *Recorder
	Logger     *slog.Logger

	Mode          Mode
	Width         int
	TasksPerImage int
	Rollouts      int
	Steps         int
	Park          Park

	CreateTimeout time.Duration
	StepTimeout   time.Duration
	WakeBudget    time.Duration
	WakeBackoff   time.Duration
	Keep          bool
}

func (c *Config) validate() error    { rc := rlbench.Config(*c); return rc.Validate() }
func (c *Config) longestName() string { rc := rlbench.Config(*c); return rc.LongestName() }

// Summary is what a run produced, for the exit status and the log.
type Summary = rlbench.Summary

// Run executes the configured workload and returns its summary.
func Run(ctx context.Context, cfg Config) (*Summary, error) {
	return rlbench.Run(ctx, rlbench.Config(cfg))
}

// EnvID derives a Substrate-valid name from the run, the instance and the replay index.
func EnvID(runID, instanceID string, k int) string {
	return rlbench.EnvID(runID, instanceID, k)
}

func isTransient(err error) bool { return rlbench.IsTransient(err) }
