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
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand/v2"
	"os"
	"path/filepath"
	"slices"

	"github.com/agent-substrate/env/internal/benchmarking/schema"
)

// ScheduleSource identifies how a job's per-step task list is produced.
type ScheduleSource string

const (
	// ScheduleSourceExplicit uses a caller-supplied resolved ScheduledJob directly.
	ScheduleSourceExplicit ScheduleSource = "explicit"
	// ScheduleSourceSampled deterministically samples tasks_per_step instances
	// per step using a seeded PCG PRNG and expands each by group_size.
	ScheduleSourceSampled ScheduleSource = "sampled"
	// ScheduleSourceFixed repeats the first tasks_per_step instances of the
	// dataset on every step (100% working-set overlap across steps).
	ScheduleSourceFixed ScheduleSource = "fixed"
)

// JobScheduleSpec describes how to resolve one job's schedule.
type JobScheduleSpec struct {
	JobID         string
	Source        ScheduleSource
	Seed          uint64
	StartOffsetMS float64
	StepPauseMS   float64
	ParkPolicy    schema.ParkPolicy
	Trajectory    string
	Steps         int
	TasksPerStep  int
	GroupSize     int
	Instances     []Instance
	ExplicitJob   *schema.ScheduledJob
}

// ResolveScheduleOptions configures multi-job deterministic schedule resolution.
type ResolveScheduleOptions struct {
	ScenarioName string
	Seed         int64
	Jobs         []JobScheduleSpec
}

// ResolveSchedule resolves a deterministic v1 schema.Schedule from the given options.
func ResolveSchedule(opts ResolveScheduleOptions) (*schema.Schedule, error) {
	if opts.ScenarioName == "" {
		return nil, errors.New("swebench: scenario_name is required")
	}
	if len(opts.Jobs) == 0 {
		return nil, errors.New("swebench: at least one job is required")
	}
	seed := opts.Seed
	if seed == 0 && len(opts.Jobs) > 0 {
		seed = int64(opts.Jobs[0].Seed)
	}

	out := &schema.Schedule{
		SchemaVersion: schema.SchemaVersion,
		ScenarioName:  opts.ScenarioName,
		Seed:          seed,
		Jobs:          make([]schema.ScheduledJob, 0, len(opts.Jobs)),
	}

	for i, js := range opts.Jobs {
		resolved, err := resolveJobSchedule(i, js)
		if err != nil {
			return nil, err
		}
		out.Jobs = append(out.Jobs, resolved)
	}

	if err := out.Validate(); err != nil {
		return nil, err
	}
	return out, nil
}

// ResolveSingleJobSchedule resolves a single-job schedule for swebench-loader.
func ResolveSingleJobSchedule(jobID string, seed uint64, park schema.ParkPolicy, steps, tasksPerStep, groupSize int, instances []Instance) (*schema.Schedule, error) {
	if tasksPerStep <= 0 {
		tasksPerStep = len(instances)
	}
	if groupSize <= 0 {
		groupSize = 1
	}
	return ResolveSchedule(ResolveScheduleOptions{
		ScenarioName: jobID,
		Seed:         int64(seed),
		Jobs: []JobScheduleSpec{
			{
				JobID:        jobID,
				Source:       ScheduleSourceSampled,
				Seed:         seed,
				ParkPolicy:   park,
				Steps:        steps,
				TasksPerStep: tasksPerStep,
				GroupSize:    groupSize,
				Instances:    instances,
			},
		},
	})
}

func resolveJobSchedule(idx int, spec JobScheduleSpec) (schema.ScheduledJob, error) {
	src := spec.Source
	if src == "" {
		src = ScheduleSourceSampled
	}

	if src == ScheduleSourceExplicit {
		if spec.ExplicitJob == nil {
			return schema.ScheduledJob{}, fmt.Errorf("swebench: jobs[%d] (%s): explicit source requires ExplicitJob", idx, spec.JobID)
		}
		cp := *spec.ExplicitJob
		if spec.JobID != "" {
			cp.Job = spec.JobID
		}
		if spec.ParkPolicy != "" {
			norm, err := schema.NormalizeParkPolicy(spec.ParkPolicy)
			if err != nil {
				return schema.ScheduledJob{}, fmt.Errorf("swebench: job %s: %w", cp.Job, err)
			}
			cp.ParkPolicy = norm
		}
		populatePrefetchImages(cp.Steps)
		return cp, nil
	}

	if spec.JobID == "" {
		return schema.ScheduledJob{}, fmt.Errorf("swebench: jobs[%d]: job name is required", idx)
	}
	var normPark schema.ParkPolicy
	if spec.ParkPolicy != "" {
		var err error
		normPark, err = schema.NormalizeParkPolicy(spec.ParkPolicy)
		if err != nil {
			return schema.ScheduledJob{}, fmt.Errorf("swebench: job %s: %w", spec.JobID, err)
		}
	}
	if spec.Steps < 1 {
		return schema.ScheduledJob{}, fmt.Errorf("swebench: job %s: steps must be >= 1, got %d", spec.JobID, spec.Steps)
	}
	if spec.TasksPerStep < 1 {
		return schema.ScheduledJob{}, fmt.Errorf("swebench: job %s: tasks_per_step must be >= 1, got %d", spec.JobID, spec.TasksPerStep)
	}
	if spec.GroupSize < 1 {
		return schema.ScheduledJob{}, fmt.Errorf("swebench: job %s: group_size must be >= 1, got %d", spec.JobID, spec.GroupSize)
	}
	if len(spec.Instances) == 0 {
		return schema.ScheduledJob{}, fmt.Errorf("swebench: job %s: dataset instances cannot be empty", spec.JobID)
	}

	steps := make([]schema.ScheduledStep, spec.Steps)
	switch src {
	case ScheduleSourceSampled:
		rng := rand.New(rand.NewPCG(spec.Seed, spec.Seed^0x9e3779b97f4a7c15))
		for s := 0; s < spec.Steps; s++ {
			picked := sampleInstancesForStep(rng, spec.Instances, spec.TasksPerStep)
			steps[s] = buildScheduledStep(s+1, picked, spec.GroupSize, spec.Trajectory, spec.StepPauseMS)
		}
	case ScheduleSourceFixed:
		picked := fixedInstancesForStep(spec.Instances, spec.TasksPerStep)
		for s := 0; s < spec.Steps; s++ {
			steps[s] = buildScheduledStep(s+1, picked, spec.GroupSize, spec.Trajectory, spec.StepPauseMS)
		}
	default:
		return schema.ScheduledJob{}, fmt.Errorf("swebench: job %s: unsupported schedule source %q", spec.JobID, src)
	}

	populatePrefetchImages(steps)
	return schema.ScheduledJob{
		Job:           spec.JobID,
		StartOffsetMS: spec.StartOffsetMS,
		ParkPolicy:    normPark,
		Steps:         steps,
	}, nil
}

func sampleInstancesForStep(rng *rand.Rand, pool []Instance, count int) []Instance {
	n := len(pool)
	out := make([]Instance, 0, count)
	for len(out) < count {
		perm := rng.Perm(n)
		need := count - len(out)
		if need > n {
			need = n
		}
		for i := 0; i < need; i++ {
			out = append(out, pool[perm[i]])
		}
	}
	return out
}

func fixedInstancesForStep(pool []Instance, count int) []Instance {
	out := make([]Instance, count)
	for i := 0; i < count; i++ {
		out[i] = pool[i%len(pool)]
	}
	return out
}

func buildScheduledStep(stepNum int, picked []Instance, groupSize int, trajectory string, stepPauseMS float64) schema.ScheduledStep {
	tasks := make([]schema.ScheduledTask, 0, len(picked)*groupSize)
	for _, inst := range picked {
		for g := 0; g < groupSize; g++ {
			tasks = append(tasks, schema.ScheduledTask{
				InstanceID: inst.InstanceID,
				Image:      inst.Image,
				Trajectory: trajectory,
				Rollout:    g,
			})
		}
	}
	return schema.ScheduledStep{
		Step:        stepNum,
		StepPauseMS: stepPauseMS,
		Tasks:       tasks,
	}
}

// populatePrefetchImages computes Step[s].PrefetchImages = need(s+1) \ active(s)
// in deterministic order of first appearance in Step[s+1].
func populatePrefetchImages(steps []schema.ScheduledStep) {
	for s := 0; s+1 < len(steps); s++ {
		active := make(map[string]bool, len(steps[s].Tasks))
		for _, t := range steps[s].Tasks {
			active[t.Image] = true
		}
		seen := make(map[string]bool, len(steps[s+1].Tasks))
		var prefetch []string
		for _, t := range steps[s+1].Tasks {
			if !active[t.Image] && !seen[t.Image] {
				seen[t.Image] = true
				prefetch = append(prefetch, t.Image)
			}
		}
		steps[s].PrefetchImages = prefetch
	}
	if len(steps) > 0 {
		steps[len(steps)-1].PrefetchImages = nil
	}
}

// ResolveScenarioSchedule resolves a deterministic v1 schema.Schedule from a
// multi-job schema.Scenario and loaded dataset map.
func ResolveScenarioSchedule(scen schema.Scenario, defaultDataset string, datasets map[string][]Instance) (*schema.Schedule, error) {
	scenName := scen.Name
	if scenName == "" {
		scenName = "scenario"
	}
	seed := scen.Seed
	if seed == 0 && len(scen.Jobs) > 0 {
		seed = scen.Jobs[0].Seed
	}
	if seed == 0 {
		seed = 42
	}
	jobs := make([]JobScheduleSpec, 0, len(scen.Jobs))
	for idx, j := range scen.Jobs {
		dsKey := j.Dataset
		if dsKey == "" {
			dsKey = defaultDataset
		}
		var instances []Instance
		if datasets != nil {
			instances = datasets[dsKey]
		}
		stepsCount := j.Steps
		if stepsCount <= 0 {
			stepsCount = 1
		}
		tasksPerStep := j.TasksPerStep
		if tasksPerStep <= 0 {
			tasksPerStep = j.BatchSize
		}
		if tasksPerStep <= 0 {
			tasksPerStep = j.Concurrency
		}
		if len(instances) == 0 {
			nInst := j.Instances
			if nInst <= 0 {
				nInst = tasksPerStep
			}
			if nInst <= 0 {
				nInst = 1
			}
			instances = SyntheticInstances(nInst, j.ImagePrefix)
		}
		if j.Images > 0 && j.Images < len(instances) {
			instances = instances[:j.Images]
		}
		if tasksPerStep <= 0 {
			tasksPerStep = len(instances)
		}
		groupSize := j.GroupSize
		if groupSize <= 0 {
			groupSize = j.RolloutsPerTask
		}
		if groupSize <= 0 {
			groupSize = 1
		}
		traj := j.Trajectory
		if traj == "" {
			traj = DefaultTrajectory
		}
		park := j.Park
		if park == "" {
			park = j.ParkPolicy
		}
		src := ScheduleSource(j.ScheduleMode)
		switch src {
		case ScheduleSourceSampled, ScheduleSourceFixed:
		case "":
			src = ScheduleSourceSampled
		default:
			src = ScheduleSourceSampled
		}
		jSeed := uint64(j.Seed)
		if jSeed == 0 {
			jSeed = uint64(seed + int64(idx)*1000)
		}
		jobs = append(jobs, JobScheduleSpec{
			JobID:         j.Name,
			Source:        src,
			Seed:          jSeed,
			StartOffsetMS: j.StartOffsetMS,
			StepPauseMS:   j.StepPauseMS,
			ParkPolicy:    park,
			Trajectory:    traj,
			Steps:         stepsCount,
			TasksPerStep:  tasksPerStep,
			GroupSize:     groupSize,
			Instances:     instances,
		})
	}
	return ResolveSchedule(ResolveScheduleOptions{
		ScenarioName: scenName,
		Seed:         seed,
		Jobs:         jobs,
	})
}

// WriteSchedule validates and writes a Schedule to path as formatted JSON.
func WriteSchedule(path string, sched *schema.Schedule) error {
	if sched == nil {
		return errors.New("swebench: nil schedule")
	}
	if err := sched.Validate(); err != nil {
		return err
	}
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return fmt.Errorf("swebench: mkdir schedule dir: %w", err)
		}
	}
	data, err := json.MarshalIndent(sched, "", "  ")
	if err != nil {
		return fmt.Errorf("swebench: marshal schedule: %w", err)
	}
	data = append(data, '\n')
	return os.WriteFile(path, data, 0o644)
}

// ReadSchedule reads and strictly validates a Schedule from path.
func ReadSchedule(path string) (*schema.Schedule, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("swebench: read schedule: %w", err)
	}
	sched, err := schema.DecodeJSONStrict[schema.Schedule](bytes.NewReader(data))
	if err != nil {
		return nil, fmt.Errorf("swebench: decode schedule %s: %w", path, err)
	}
	return &sched, nil
}

// InstancesByID indexes a slice of Instances by InstanceID.
func InstancesByID(instances []Instance) map[string]Instance {
	m := make(map[string]Instance, len(instances))
	for _, in := range instances {
		m[in.InstanceID] = in
	}
	return m
}

// SyntheticInstances generates n deterministic synthetic SWE-bench instances
// using the given image prefix when no external JSONL dataset is supplied.
func SyntheticInstances(n int, imagePrefix string) []Instance {
	if n <= 0 {
		n = 1
	}
	if imagePrefix == "" {
		imagePrefix = "ghcr.io/epoch-research/swe-bench.eval.x86_64."
	}
	out := make([]Instance, n)
	for i := 0; i < n; i++ {
		id := fmt.Sprintf("synthetic__task-%03d", i)
		out[i] = Instance{
			InstanceID: id,
			Image:      imagePrefix + id + ":latest",
		}
	}
	return out
}

// ScheduleOptions configures deterministic schedule generation for a single job.
type ScheduleOptions struct {
	ScenarioName    string
	JobName         string
	Steps           int
	BatchSize       int
	RolloutsPerTask int
	StepPauseMS     float64
	LookaheadSteps  int
	StartOffsetMS   float64
	ParkPolicy      string
}

// ScheduleGenerator generates deterministic per-step schedules from a seed.
type ScheduleGenerator struct {
	seed uint64
}

// NewScheduleGenerator returns a ScheduleGenerator seeded with seed.
func NewScheduleGenerator(seed int64) *ScheduleGenerator {
	if seed == 0 {
		seed = 42
	}
	return &ScheduleGenerator{seed: uint64(seed)}
}

// Generate builds a deterministic schema.Schedule for the given options and instances.
func (g *ScheduleGenerator) Generate(opts ScheduleOptions, instances []Instance) *schema.Schedule {
	scenName := opts.ScenarioName
	if scenName == "" {
		scenName = "scenario"
	}
	jobName := opts.JobName
	if jobName == "" {
		jobName = "job-0"
	}
	steps := opts.Steps
	if steps <= 0 {
		steps = 1
	}
	batch := opts.BatchSize
	if batch <= 0 {
		batch = len(instances)
		if batch <= 0 {
			batch = 1
		}
	}
	rollouts := opts.RolloutsPerTask
	if rollouts <= 0 {
		rollouts = 1
	}
	if len(instances) == 0 {
		instances = SyntheticInstances(batch, "")
	}
	sched, err := ResolveSchedule(ResolveScheduleOptions{
		ScenarioName: scenName,
		Seed:         int64(g.seed),
		Jobs: []JobScheduleSpec{
			{
				JobID:         jobName,
				Source:        ScheduleSourceSampled,
				Seed:          g.seed,
				StartOffsetMS: opts.StartOffsetMS,
				StepPauseMS:   opts.StepPauseMS,
				ParkPolicy:    schema.ParkPolicy(opts.ParkPolicy),
				Steps:         steps,
				TasksPerStep:  batch,
				GroupSize:     rollouts,
				Instances:     instances,
			},
		},
	})
	if err != nil {
		return &schema.Schedule{
			SchemaVersion: schema.SchemaVersion,
			ScenarioName:  scenName,
			Seed:          int64(g.seed),
		}
	}
	return sched
}

// UniqueScheduleImages returns all unique images referenced by sched in sorted order.
func UniqueScheduleImages(sched *schema.Schedule) []string {
	seen := map[string]bool{}
	var out []string
	for _, job := range sched.Jobs {
		for _, st := range job.Steps {
			for _, t := range st.Tasks {
				if !seen[t.Image] {
					seen[t.Image] = true
					out = append(out, t.Image)
				}
			}
		}
	}
	slices.Sort(out)
	return out
}
