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

// Package trace converts verbose SDK call traces (trace.jsonl) into replayable
// RL benchmark schedules, placement plans, and trajectories.
package trace

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/agent-substrate/env/internal/benchmarking/schema"
	"github.com/agent-substrate/env/internal/benchmarking/swebench"
	"sigs.k8s.io/yaml"
)

const (
	defaultScenarioName = "traced-scenario"
	defaultJobName      = "traced-job"
	defaultTracedImage  = "gcr.io/substrate/traced@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
)

// ConvertOptions configures trace-to-schedule and trajectory conversion.
type ConvertOptions struct {
	ScenarioName      string
	DefaultJob        string
	DefaultImage      string
	DefaultParkPolicy schema.ParkPolicy
	EnvImages         map[string]string
	EnvInstances      map[string]string
}

func (o *ConvertOptions) defaults() {
	if o.ScenarioName == "" {
		o.ScenarioName = defaultScenarioName
	}
	if o.DefaultJob == "" {
		o.DefaultJob = defaultJobName
	}
	if o.DefaultImage == "" {
		o.DefaultImage = defaultTracedImage
	}
}

// ConvertedTrace holds the resolved Schedule, PlacementPlan, Trajectories, and
// Instances recovered from a trace.jsonl stream.
type ConvertedTrace struct {
	Schedule       *schema.Schedule
	Placement      *schema.PlacementPlan
	Trajectories   map[string]*swebench.Trajectory
	Representative *swebench.Trajectory
	Instances      []swebench.Instance
}

type envRollout struct {
	env       string
	firstSeen time.Time
	events    []schema.TraceEvent
	deleted   bool
	suspended bool
}

func eventDuration(ev schema.TraceEvent) time.Duration {
	return time.Duration(math.Round(ev.DurMS * float64(time.Microsecond) * 1000))
}

func eventStart(ev schema.TraceEvent) time.Time {
	return ev.TS.Add(-eventDuration(ev))
}

func eventEnd(ev schema.TraceEvent) time.Time {
	return ev.TS
}

// ConvertTrace reads a JSONL trace stream via strict schema decoding and
// reconstructs a schema.Schedule, schema.PlacementPlan, and swebench.Trajectory
// set preserving intra-trajectory think times, payload sizes, commands,
// working-set reuse, and inter-step trainer pauses.
func ConvertTrace(r io.Reader, opts ConvertOptions) (*ConvertedTrace, error) {
	opts.defaults()
	raw, err := io.ReadAll(r)
	if err != nil {
		return nil, fmt.Errorf("convert trace: read: %w", err)
	}
	if len(bytes.TrimSpace(raw)) == 0 {
		return nil, errors.New("convert trace: trace contains no events")
	}
	if err := schema.ValidateJSONLAgainstJSONSchema("trace_event.schema.json", bytes.NewReader(raw)); err != nil {
		return nil, fmt.Errorf("convert trace: schema validation: %w", err)
	}
	events, err := schema.DecodeJSONLStrict[schema.TraceEvent](bytes.NewReader(raw))
	if err != nil {
		return nil, fmt.Errorf("convert trace: %w", err)
	}
	if len(events) == 0 {
		return nil, errors.New("convert trace: trace contains no events")
	}

	type stepGroup struct {
		stepNum  int
		startTS  time.Time
		endTS    time.Time
		rollouts map[string]*envRollout
	}
	jobSteps := make(map[string]map[int]*stepGroup)
	var jobOrder []string
	seenJob := make(map[string]bool)

	knownEnvImage := make(map[string]string)
	knownEnvInstance := make(map[string]string)
	for k, v := range opts.EnvImages {
		knownEnvImage[k] = v
	}
	for k, v := range opts.EnvInstances {
		knownEnvInstance[k] = v
	}

	for _, ev := range events {
		if ev.Image != "" && strings.Contains(ev.Image, "@sha256:") {
			knownEnvImage[ev.Env] = ev.Image
		}
		if ev.Instance != "" {
			knownEnvInstance[ev.Env] = ev.Instance
		}
		job := ev.Job
		if job == "" {
			job = opts.DefaultJob
		}
		if !seenJob[job] {
			seenJob[job] = true
			jobOrder = append(jobOrder, job)
			jobSteps[job] = make(map[int]*stepGroup)
		}
		stepNum := ev.Step
		if stepNum <= 0 {
			stepNum = 1
		}
		sg, ok := jobSteps[job][stepNum]
		if !ok {
			sg = &stepGroup{
				stepNum:  stepNum,
				rollouts: make(map[string]*envRollout),
			}
			jobSteps[job][stepNum] = sg
		}
		st := eventStart(ev)
		et := eventEnd(ev)
		if sg.startTS.IsZero() || st.Before(sg.startTS) {
			sg.startTS = st
		}
		if sg.endTS.IsZero() || et.After(sg.endTS) {
			sg.endTS = et
		}
		er, ok := sg.rollouts[ev.Env]
		if !ok {
			er = &envRollout{env: ev.Env, firstSeen: st}
			sg.rollouts[ev.Env] = er
		} else if st.Before(er.firstSeen) {
			er.firstSeen = st
		}
		er.events = append(er.events, ev)
		switch ev.Op {
		case "delete":
			er.deleted = true
		case "suspend":
			er.suspended = true
		}
	}

	sched := &schema.Schedule{
		SchemaVersion: schema.SchemaVersion,
		ScenarioName:  opts.ScenarioName,
	}
	placementPlan := &schema.PlacementPlan{
		SchemaVersion: schema.SchemaVersion,
		ScenarioName:  opts.ScenarioName,
	}
	trajectories := make(map[string]*swebench.Trajectory)
	var representative *swebench.Trajectory
	instanceMap := make(map[string]swebench.Instance)
	var instanceOrder []string

	for _, job := range jobOrder {
		stepsMap := jobSteps[job]
		stepNums := make([]int, 0, len(stepsMap))
		for s := range stepsMap {
			stepNums = append(stepNums, s)
		}
		sort.Ints(stepNums)

		lastStepForEnv := make(map[string]int)
		sawSuspend := false
		for _, s := range stepNums {
			for envID, er := range stepsMap[s].rollouts {
				lastStepForEnv[envID] = s
				if er.suspended {
					sawSuspend = true
				}
			}
		}

		parkPolicy := opts.DefaultParkPolicy
		if parkPolicy == "" {
			if sawSuspend || len(stepNums) > 1 {
				parkPolicy = schema.ParkSuspendAsync
			} else {
				parkPolicy = schema.ParkDelete
			}
		}

		aliveEnvs := make(map[string]bool)
		sj := schema.ScheduledJob{
			Job:        job,
			ParkPolicy: parkPolicy,
		}
		jp := schema.JobPlacementPlan{
			Job: job,
		}

		for idx, s := range stepNums {
			sg := stepsMap[s]
			envs := make([]*envRollout, 0, len(sg.rollouts))
			for _, er := range sg.rollouts {
				sort.SliceStable(er.events, func(i, j int) bool {
					return eventStart(er.events[i]).Before(eventStart(er.events[j]))
				})
				envs = append(envs, er)
			}
			sort.Slice(envs, func(i, j int) bool {
				if !envs[i].firstSeen.Equal(envs[j].firstSeen) {
					return envs[i].firstSeen.Before(envs[j].firstSeen)
				}
				return envs[i].env < envs[j].env
			})

			schedStep := schema.ScheduledStep{Step: idx + 1}
			if idx+1 < len(stepNums) {
				nextSG := stepsMap[stepNums[idx+1]]
				if pause := nextSG.startTS.Sub(sg.endTS); pause > 0 {
					schedStep.StepPauseMS = float64(pause.Round(time.Microsecond)) / float64(time.Millisecond)
				}
			}

			for rolloutIdx, er := range envs {
				img := knownEnvImage[er.env]
				if img == "" {
					// Give each distinct traced env its own deterministic digest when
					// not explicitly mapped so WorkingSet image matching preserves
					// per-env reuse across steps.
					img = defaultImageForEnv(er.env, opts.DefaultImage)
					knownEnvImage[er.env] = img
				}
				instID := knownEnvInstance[er.env]
				if instID == "" {
					instID = er.env
					knownEnvInstance[er.env] = instID
				}

				if _, exists := instanceMap[instID]; !exists {
					instanceOrder = append(instanceOrder, instID)
					mat, _ := json.Marshal(swebench.CommandTask{
						Kind:       swebench.CommandKind,
						InstanceID: instID,
						Image:      img,
						Command:    "true",
					})
					instanceMap[instID] = swebench.Instance{
						Kind:       swebench.CommandKind,
						InstanceID: instID,
						Image:      img,
						Material:   mat,
					}
				}

				trajName := fmt.Sprintf("%s-s%d-%s", job, idx+1, er.env)
				traj := buildTrajectory(trajName, er.events)
				trajectories[trajName] = traj
				if representative == nil || len(traj.Steps) > len(representative.Steps) {
					cp := *traj
					cp.Name = opts.ScenarioName
					representative = &cp
				}

				schedStep.Tasks = append(schedStep.Tasks, schema.ScheduledTask{
					InstanceID: instID,
					Image:      img,
					Trajectory: trajName,
					Rollout:    rolloutIdx,
				})

				tp := schema.TaskPlacement{
					Step:       idx + 1,
					InstanceID: instID,
					Rollout:    rolloutIdx,
					EnvID:      er.env,
				}
				if aliveEnvs[er.env] {
					tp.ReuseEnvID = er.env
				}
				if er.deleted || s == lastStepForEnv[er.env] {
					tp.RetireAfter = true
					delete(aliveEnvs, er.env)
				} else {
					aliveEnvs[er.env] = true
				}
				jp.Assignments = append(jp.Assignments, tp)
			}
			sj.Steps = append(sj.Steps, schedStep)
		}

		// Populate PrefetchImages = need(s+1) \ active(s).
		for s := 0; s+1 < len(sj.Steps); s++ {
			active := make(map[string]bool, len(sj.Steps[s].Tasks))
			for _, t := range sj.Steps[s].Tasks {
				active[t.Image] = true
			}
			seen := make(map[string]bool, len(sj.Steps[s+1].Tasks))
			for _, t := range sj.Steps[s+1].Tasks {
				if !active[t.Image] && !seen[t.Image] {
					seen[t.Image] = true
					sj.Steps[s].PrefetchImages = append(sj.Steps[s].PrefetchImages, t.Image)
				}
			}
		}

		sched.Jobs = append(sched.Jobs, sj)
		placementPlan.Jobs = append(placementPlan.Jobs, jp)
	}

	if err := sched.Validate(); err != nil {
		return nil, err
	}
	if err := placementPlan.Validate(); err != nil {
		return nil, err
	}
	if representative == nil {
		return nil, errors.New("convert trace: failed to build representative trajectory")
	}
	if err := representative.Validate(); err != nil {
		return nil, err
	}

	instances := make([]swebench.Instance, 0, len(instanceOrder))
	for _, id := range instanceOrder {
		instances = append(instances, instanceMap[id])
	}

	return &ConvertedTrace{
		Schedule:       sched,
		Placement:      placementPlan,
		Trajectories:   trajectories,
		Representative: representative,
		Instances:      instances,
	}, nil
}

func defaultImageForEnv(envID, fallback string) string {
	repo := fallback
	if at := strings.Index(fallback, "@sha256:"); at > 0 {
		repo = fallback[:at]
	}
	var sum [32]byte
	b := []byte(envID)
	for i := range sum {
		if len(b) > 0 {
			sum[i] = b[i%len(b)] ^ byte(i*31+len(b))
		}
	}
	return fmt.Sprintf("%s@sha256:%x", repo, sum[:])
}

func buildTrajectory(name string, events []schema.TraceEvent) *swebench.Trajectory {
	var steps []swebench.Step
	var prevActionEnd time.Time
	lastRunIdx := -1

	for _, ev := range events {
		st := eventStart(ev)
		et := eventEnd(ev)
		var think time.Duration
		if !prevActionEnd.IsZero() {
			if gap := st.Sub(prevActionEnd); gap > 0 {
				think = gap.Round(time.Microsecond)
			}
		}

		switch ev.Op {
		case "write_file", "write":
			path := ev.Path
			if path == "" {
				path = "/tmp/traced-payload"
			}
			size := int(ev.Bytes)
			if size < 0 {
				size = 0
			}
			steps = append(steps, swebench.Step{
				Name:  fmt.Sprintf("step-%d-write", len(steps)+1),
				Think: swebench.Duration(think),
				Write: &swebench.WriteSpec{
					Path:    path,
					Content: strings.Repeat("x", size),
				},
			})
			prevActionEnd = et
		case "shell", "start_process", "run", "exec":
			cmd := strings.Join(ev.Command, " ")
			if len(ev.Command) == 3 && (ev.Command[0] == "sh" || ev.Command[0] == "/bin/sh") && ev.Command[1] == "-c" {
				cmd = ev.Command[2]
			}
			if cmd == "" {
				cmd = "true"
			}
			lastRunIdx = len(steps)
			steps = append(steps, swebench.Step{
				Name:  fmt.Sprintf("step-%d-run", len(steps)+1),
				Think: swebench.Duration(think),
				Run:   cmd,
			})
			prevActionEnd = et
		}
	}

	if len(steps) == 0 {
		steps = append(steps, swebench.Step{
			Name:   "step-1-noop",
			Run:    "true",
			Reward: true,
		})
	} else if lastRunIdx >= 0 {
		steps[lastRunIdx].Reward = true
	}

	return &swebench.Trajectory{
		Name:  name,
		Steps: steps,
	}
}

// WriteConvertedArtifacts writes the converted schedule.json, placement.json,
// trajectory.yaml, per-rollout trajectories/*.yaml, and instances.jsonl to dir.
func WriteConvertedArtifacts(dir string, conv *ConvertedTrace) error {
	if conv == nil || conv.Schedule == nil || conv.Representative == nil {
		return errors.New("converted trace is incomplete")
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	schedPath := filepath.Join(dir, "schedule.json")
	if err := swebench.WriteSchedule(schedPath, conv.Schedule); err != nil {
		return fmt.Errorf("write schedule.json: %w", err)
	}

	if conv.Placement != nil {
		if err := conv.Placement.Validate(); err != nil {
			return fmt.Errorf("validate placement.json: %w", err)
		}
		placeBytes, err := json.MarshalIndent(conv.Placement, "", "  ")
		if err != nil {
			return fmt.Errorf("marshal placement.json: %w", err)
		}
		placeBytes = append(placeBytes, '\n')
		if err := os.WriteFile(filepath.Join(dir, "placement.json"), placeBytes, 0o644); err != nil {
			return fmt.Errorf("write placement.json: %w", err)
		}
	}

	if err := conv.Representative.Validate(); err != nil {
		return fmt.Errorf("validate representative trajectory: %w", err)
	}
	trajBytes, err := yaml.Marshal(conv.Representative)
	if err != nil {
		return fmt.Errorf("marshal trajectory.yaml: %w", err)
	}
	trajPath := filepath.Join(dir, "trajectory.yaml")
	if err := os.WriteFile(trajPath, trajBytes, 0o644); err != nil {
		return fmt.Errorf("write trajectory.yaml: %w", err)
	}

	if len(conv.Trajectories) > 0 {
		trajDir := filepath.Join(dir, "trajectories")
		if err := os.MkdirAll(trajDir, 0o755); err != nil {
			return err
		}
		names := make([]string, 0, len(conv.Trajectories))
		for name := range conv.Trajectories {
			names = append(names, name)
		}
		sort.Strings(names)
		for _, name := range names {
			tb, err := yaml.Marshal(conv.Trajectories[name])
			if err != nil {
				return fmt.Errorf("marshal trajectory %s: %w", name, err)
			}
			if err := os.WriteFile(filepath.Join(trajDir, name+".yaml"), tb, 0o644); err != nil {
				return fmt.Errorf("write trajectory %s: %w", name, err)
			}
		}
	}

	if len(conv.Instances) > 0 {
		var buf bytes.Buffer
		enc := json.NewEncoder(&buf)
		for _, in := range conv.Instances {
			if err := enc.Encode(in); err != nil {
				return fmt.Errorf("encode instances.jsonl: %w", err)
			}
		}
		if err := os.WriteFile(filepath.Join(dir, "instances.jsonl"), buf.Bytes(), 0o644); err != nil {
			return fmt.Errorf("write instances.jsonl: %w", err)
		}
	}

	return nil
}
