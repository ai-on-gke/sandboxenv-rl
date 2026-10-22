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

import "github.com/agent-substrate/env/internal/benchmarking/rlbench"

// DefaultTrajectory is the built-in trajectory used when none is given.
const DefaultTrajectory = rlbench.DefaultTrajectory

// Duration is a time.Duration that decodes from a string such as "1s".
type Duration = rlbench.Duration

// WriteSpec puts a file into the sandbox.
type WriteSpec = rlbench.WriteSpec

// Step is one action of a trajectory: exactly one of Run or Write.
type Step = rlbench.Step

// Trajectory is the scripted rollout replayed in every environment.
type Trajectory = rlbench.Trajectory

// Names lists the built-in trajectories.
func Names() []string {
	return rlbench.Names()
}

// LoadTrajectory returns a built-in trajectory by name or one read from a file.
func LoadTrajectory(source string) (*Trajectory, error) {
	return rlbench.LoadTrajectory(source)
}

// ParseTrajectory decodes and validates a YAML trajectory.
func ParseTrajectory(data []byte) (*Trajectory, error) {
	return rlbench.ParseTrajectory(data)
}
