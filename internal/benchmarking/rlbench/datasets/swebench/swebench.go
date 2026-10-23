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

// Package swebench adapts SWE-bench datasets, built-in trajectories
// (swebench-gold and swebench-grade.py), and verdict parsing to the
// dataset-agnostic rlbench engine.
package swebench

import (
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"strings"

	"github.com/agent-substrate/env/internal/benchmarking/rlbench"
)

// Kind identifies SWE-bench rows in dataset manifests.
const Kind = rlbench.DefaultKind

// DefaultTrajectory is the built-in SWE-bench gold trajectory.
const DefaultTrajectory = "swebench-gold"

//go:embed trajectories/*
var trajectoryFS embed.FS

func init() {
	rlbench.RegisterAdapter(Adapter{})
}

// Instance holds the SWE-bench-specific material decoded from an
// rlbench.Instance's Material payload. The manifest is produced by
// benchmarking/swebench/make-dataset.py, one JSON object per line.
type Instance struct {
	InstanceID string `json:"instance_id"`
	Repo       string `json:"repo"`
	Image      string `json:"image"`
	// Patch is the gold patch; applying it makes FailToPass pass.
	Patch string `json:"patch"`
	// TestPatch adds or changes the tests that check the fix.
	TestPatch string `json:"test_patch"`
	// EvalScript is the SWE-bench harness eval script for this instance: it
	// applies TestPatch, runs the repo's test command and restores the test
	// files.
	EvalScript string   `json:"eval_script"`
	FailToPass []string `json:"fail_to_pass"`
	PassToPass []string `json:"pass_to_pass"`
	// LogParser names the SWE-bench harness parser for this repo's test
	// output (parse_log_pytest, parse_log_django, ...); EvalType is
	// pass_and_fail or fail_only. Both come from the dataset row.
	LogParser string `json:"log_parser"`
	EvalType  string `json:"eval_type"`
}

// Decode extracts the SWE-bench Instance fields from an rlbench.Instance.
func Decode(in rlbench.Instance) (Instance, error) {
	var swe Instance
	if len(in.Material) > 0 {
		if err := json.Unmarshal(in.Material, &swe); err != nil {
			return Instance{}, fmt.Errorf("%s: decode swebench material: %w", in.InstanceID, err)
		}
	}
	swe.InstanceID = in.InstanceID
	swe.Image = in.Image
	return swe, nil
}

// pack wraps a SWE-bench Instance into a dataset-agnostic rlbench.Instance.
func pack(in Instance) (rlbench.Instance, error) {
	raw, err := json.Marshal(in)
	if err != nil {
		return rlbench.Instance{}, err
	}
	return rlbench.Instance{
		InstanceID: in.InstanceID,
		Image:      in.Image,
		Kind:       Kind,
		Material:   raw,
	}, nil
}

// errNoVerdict indicates that a reward step's output did not end with a
// swebench-grade.py JSON line.
var errNoVerdict = errors.New("no swebench grader JSON in output")

// Grader parses swebench-grade.py JSON lines ({"valid","resolved","f2p","p2p"})
// into a 0/1 reward and a "f2p A/B p2p C/D" or "invalid run" detail string.
type Grader struct{}

// Parse implements rlbench.Grader.
func (Grader) Parse(stdout []byte) (reward *int, detail string, err error) {
	s := string(stdout)
	i := strings.LastIndex(s, `{"valid"`)
	if i < 0 {
		return nil, "", errNoVerdict
	}
	var g struct {
		Valid    bool   `json:"valid"`
		Resolved *bool  `json:"resolved"`
		F2P      string `json:"f2p"`
		P2P      string `json:"p2p"`
	}
	if err := json.Unmarshal([]byte(strings.TrimSpace(s[i:])), &g); err != nil {
		return nil, "", err
	}
	if !g.Valid {
		r := 0
		return &r, "invalid run", nil
	}
	if g.Resolved != nil {
		r := 0
		if *g.Resolved {
			r = 1
		}
		reward = &r
	}
	return reward, fmt.Sprintf("f2p %s p2p %s", g.F2P, g.P2P), nil
}

// Adapter plugs SWE-bench into rlbench.
type Adapter struct{}

// Kind implements rlbench.Adapter.
func (Adapter) Kind() string { return Kind }

// Validate implements rlbench.Adapter.
func (Adapter) Validate(in rlbench.Instance) error {
	_, err := Decode(in)
	return err
}

// RequireGrading checks that in carries an eval_script and a log_parser, as
// required by a grading SWE-bench trajectory.
func (Adapter) RequireGrading(in rlbench.Instance) error {
	swe, err := Decode(in)
	if err != nil {
		return err
	}
	switch {
	case swe.EvalScript == "":
		return fmt.Errorf("%s: eval_script is required by a grading trajectory", in.InstanceID)
	case swe.LogParser == "":
		return fmt.Errorf("%s: log_parser is required by a grading trajectory", in.InstanceID)
	}
	return nil
}

// TemplateData implements rlbench.Adapter.
func (Adapter) TemplateData(in rlbench.Instance) (any, error) {
	return Decode(in)
}

// Grades reports whether t references SWE-bench grading material (.EvalScript,
// .LogParser, or the full instance JSON).
func (Adapter) Grades(t *rlbench.Trajectory) bool {
	for _, steps := range [][]rlbench.Step{t.Setup, t.Reset, t.Steps} {
		for _, s := range steps {
			texts := []string{s.Run}
			if s.Write != nil {
				texts = append(texts, s.Write.Content)
			}
			for _, text := range texts {
				if strings.Contains(text, ".EvalScript") || strings.Contains(text, ".LogParser") || strings.Contains(text, "json .}}") {
					return true
				}
			}
		}
	}
	return false
}

// Grader implements rlbench.Adapter.
func (Adapter) Grader() rlbench.Grader { return Grader{} }

// Trajectories implements rlbench.Adapter.
func (Adapter) Trajectories() fs.FS {
	sub, err := fs.Sub(trajectoryFS, "trajectories")
	if err != nil {
		return nil
	}
	return sub
}

// DefaultTrajectory implements rlbench.Adapter.
func (Adapter) DefaultTrajectory() string { return DefaultTrajectory }
