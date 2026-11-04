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
	"encoding/json"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// RenderedStep is one concrete action of a rendered Trajectory, ready to replay
// in non-Go drivers (such as benchmarking/swebench/run_fleet_bench.py) without
// re-implementing Go text/template rendering.
type RenderedStep struct {
	Name       string     `json:"name"`
	Run        string     `json:"run,omitempty"`
	Write      *WriteSpec `json:"write,omitempty"`
	ThinkMS    float64    `json:"think_ms,omitempty"`
	TimeoutS   float64    `json:"timeout_s,omitempty"`
	ExpectExit *int       `json:"expect_exit,omitempty"`
	Reward     bool       `json:"reward,omitempty"`
}

// RenderedRollout holds the concrete rendered trajectory assigned to one
// rollout index when a run mixes multiple trajectories.
type RenderedRollout struct {
	TrajectoryName string         `json:"trajectory_name"`
	HasReward      bool           `json:"has_reward"`
	Setup          []RenderedStep `json:"setup,omitempty"`
	Reset          []RenderedStep `json:"reset,omitempty"`
	Steps          []RenderedStep `json:"steps"`
}

// RenderedInstance holds a selected dataset Instance together with its
// pre-rendered trajectory steps and dataset-specific grading expectations.
type RenderedInstance struct {
	InstanceID       string            `json:"instance_id"`
	Repo             string            `json:"repo,omitempty"`
	Image            string            `json:"image"`
	Kind             string            `json:"kind"`
	TrajectoryName   string            `json:"trajectory_name"`
	HasReward        bool              `json:"has_reward"`
	Setup            []RenderedStep    `json:"setup,omitempty"`
	Reset            []RenderedStep    `json:"reset,omitempty"`
	Steps            []RenderedStep    `json:"steps"`
	Rollouts         []RenderedRollout `json:"rollouts,omitempty"`
	ExpectedStdout   string            `json:"expected_stdout,omitempty"`
	ExpectedExitCode int               `json:"expected_exit_code"`
}

// RenderedPlan is a portable execution plan produced by `swebench-loader render-plan`
// so Python SandboxFleet runs execute the exact same selected instances,
// rendered steps, grading rules, and loader labels as Go runs.
type RenderedPlan struct {
	Dataset          string             `json:"dataset"`
	DatasetLabel     string             `json:"dataset_label"`
	ExcludeLabel     string             `json:"exclude_label,omitempty"`
	InstancesLabel   string             `json:"instances_label,omitempty"`
	TrajectoryNames  string             `json:"trajectory_names"`
	Seed             int64              `json:"seed,omitempty"`
	HasTrajectoryMix bool               `json:"has_trajectory_mix,omitempty"`
	SetVars          map[string]string  `json:"set_vars,omitempty"`
	Instances        []RenderedInstance `json:"instances"`
}

func convertRenderedSteps(steps []Step) []RenderedStep {
	if len(steps) == 0 {
		return nil
	}
	out := make([]RenderedStep, len(steps))
	for i, s := range steps {
		rs := RenderedStep{
			Name:       s.Name,
			Run:        s.Run,
			ExpectExit: s.ExpectExit,
			Reward:     s.Reward,
		}
		if s.Write != nil {
			w := *s.Write
			rs.Write = &w
		}
		if s.Think > 0 {
			rs.ThinkMS = ms(time.Duration(s.Think))
		}
		if s.Timeout > 0 {
			rs.TimeoutS = time.Duration(s.Timeout).Seconds()
		}
		out[i] = rs
	}
	return out
}

// BuildRenderedPlan renders trajs across selected instances (applying seed and
// fanout when multiple trajectories are mixed) and packages the result as a
// RenderedPlan.
func BuildRenderedPlan(
	dataset string,
	datasetLabel string,
	excludeLabel string,
	instancesLabel string,
	selected []Instance,
	trajs []*Trajectory,
	seed int64,
	fanout int,
	setVars map[string]string,
) (*RenderedPlan, error) {
	if len(selected) == 0 {
		return nil, fmt.Errorf("render plan: no instances selected")
	}
	if len(trajs) == 0 {
		return nil, fmt.Errorf("render plan: at least one trajectory is required")
	}
	if fanout < 1 {
		fanout = 1
	}

	names := make([]string, len(trajs))
	for i, t := range trajs {
		names[i] = t.Name
	}

	var pick *rand.Rand
	if len(trajs) > 1 {
		pick = rand.New(rand.NewSource(seed))
	}

	outInstances := make([]RenderedInstance, 0, len(selected))
	for _, in := range selected {
		kind := in.Kind
		if kind == "" {
			kind = DefaultKind
		}
		ri := RenderedInstance{
			InstanceID: in.InstanceID,
			Repo:       in.Repo,
			Image:      in.Image,
			Kind:       kind,
		}
		if kind == CommandKind {
			ct, err := DecodeCommandTask(in)
			if err != nil {
				return nil, err
			}
			ri.ExpectedStdout = ct.ExpectedStdout
			ri.ExpectedExitCode = ct.ExpectedExitCode
		}

		if pick == nil {
			rendered, err := trajs[0].Render(in)
			if err != nil {
				return nil, err
			}
			ri.TrajectoryName = rendered.Name
			ri.HasReward = rendered.HasReward()
			ri.Setup = convertRenderedSteps(rendered.Setup)
			ri.Reset = convertRenderedSteps(rendered.Reset)
			ri.Steps = convertRenderedSteps(rendered.Steps)
		} else {
			ri.Rollouts = make([]RenderedRollout, fanout)
			for k := 0; k < fanout; k++ {
				src := trajs[pick.Intn(len(trajs))]
				rendered, err := src.Render(in)
				if err != nil {
					return nil, err
				}
				rr := RenderedRollout{
					TrajectoryName: rendered.Name,
					HasReward:      rendered.HasReward(),
					Setup:          convertRenderedSteps(rendered.Setup),
					Reset:          convertRenderedSteps(rendered.Reset),
					Steps:          convertRenderedSteps(rendered.Steps),
				}
				ri.Rollouts[k] = rr
				if k == 0 {
					ri.TrajectoryName = rr.TrajectoryName
					ri.HasReward = rr.HasReward
					ri.Setup = rr.Setup
					ri.Reset = rr.Reset
					ri.Steps = rr.Steps
				}
			}
		}
		outInstances = append(outInstances, ri)
	}

	plan := &RenderedPlan{
		Dataset:          dataset,
		DatasetLabel:     datasetLabel,
		ExcludeLabel:     excludeLabel,
		InstancesLabel:   instancesLabel,
		TrajectoryNames:  strings.Join(names, ","),
		HasTrajectoryMix: len(trajs) > 1,
		SetVars:          setVars,
		Instances:        outInstances,
	}
	if len(trajs) > 1 {
		plan.Seed = seed
	}
	return plan, nil
}

// WriteRenderedPlan serializes plan as indented JSON to path.
func WriteRenderedPlan(path string, plan *RenderedPlan) error {
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return fmt.Errorf("mkdir for rendered plan: %w", err)
		}
	}
	b, err := json.MarshalIndent(plan, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal rendered plan: %w", err)
	}
	b = append(b, '\n')
	if err := os.WriteFile(path, b, 0o644); err != nil {
		return fmt.Errorf("write rendered plan %q: %w", path, err)
	}
	return nil
}
