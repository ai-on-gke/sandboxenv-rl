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
	"strings"
)

// CommandKind is the dataset adapter kind for generic shell-command RL/coding tasks.
const CommandKind = "command"

// CommandTrajectoryName is the built-in default trajectory name for CommandKind tasks.
const CommandTrajectoryName = "command-default"

const defaultCommandTrajectoryYAML = `name: command-default
setup:
  - name: setup
    run: "{{if .SetupCommand}}{{.SetupCommand}}{{else}}true{{end}}"
reset:
  - name: reset
    run: "{{if .ResetCommand}}{{.ResetCommand}}{{else}}true{{end}}"
steps:
  - name: grade
    run: "{{.Command}}"
    reward: true
`

// CommandTask is the per-instance payload for a "command" dataset row.
type CommandTask struct {
	Kind             string `json:"kind,omitempty"`
	InstanceID       string `json:"instance_id"`
	Image            string `json:"image"`
	Command          string `json:"command"`
	SetupCommand     string `json:"setup_command,omitempty"`
	ResetCommand     string `json:"reset_command,omitempty"`
	ExpectedStdout   string `json:"expected_stdout,omitempty"`
	ExpectedExitCode int    `json:"expected_exit_code"`
}

// DecodeCommandTask extracts a CommandTask from an Instance.
func DecodeCommandTask(in Instance) (CommandTask, error) {
	task := CommandTask{
		Kind:       in.Kind,
		InstanceID: in.InstanceID,
		Image:      in.Image,
	}
	if len(in.Material) > 0 {
		if err := json.Unmarshal(in.Material, &task); err != nil {
			return CommandTask{}, fmt.Errorf("%s: decode command material: %w", in.InstanceID, err)
		}
	}
	if task.InstanceID == "" {
		task.InstanceID = in.InstanceID
	}
	if task.Image == "" {
		task.Image = in.Image
	}
	if task.Kind == "" {
		task.Kind = CommandKind
	}
	return task, nil
}

// CommandGrader parses either a JSON verdict line
// ({"valid":true,"reward":1,"detail":"..."}) or plain stdout matching
// ExpectedStdout / exit-code 0 into a 0/1 reward and human-readable detail.
type CommandGrader struct {
	ExpectedStdout   string
	ExpectedExitCode int
}

func parseCommandJSONVerdict(s string) (reward *int, detail string, ok bool) {
	for _, marker := range []string{`{"valid"`, `{"reward"`} {
		i := strings.LastIndex(s, marker)
		if i < 0 {
			continue
		}
		line := s[i:]
		if nl := strings.IndexByte(line, '\n'); nl >= 0 {
			line = line[:nl]
		}
		var g struct {
			Valid    *bool  `json:"valid"`
			Reward   *int   `json:"reward"`
			Resolved *bool  `json:"resolved"`
			Detail   string `json:"detail"`
		}
		if err := json.Unmarshal([]byte(strings.TrimSpace(line)), &g); err != nil {
			continue
		}
		if g.Valid == nil && g.Reward == nil && g.Resolved == nil {
			continue
		}
		if g.Valid != nil && !*g.Valid {
			r := 0
			d := g.Detail
			if d == "" {
				d = "invalid run"
			}
			return &r, d, true
		}
		r := 0
		switch {
		case g.Reward != nil:
			if *g.Reward > 0 {
				r = 1
			}
		case g.Resolved != nil:
			if *g.Resolved {
				r = 1
			}
		case g.Valid != nil && *g.Valid:
			r = 1
		}
		d := g.Detail
		if d == "" {
			d = fmt.Sprintf("reward %d", r)
		}
		return &r, d, true
	}
	return nil, "", false
}

// Parse implements Grader.
func (g CommandGrader) Parse(stdout []byte) (reward *int, detail string, err error) {
	s := string(stdout)
	if r, d, ok := parseCommandJSONVerdict(s); ok {
		return r, d, nil
	}
	actual := strings.TrimSpace(s)
	if g.ExpectedStdout != "" {
		want := strings.TrimSpace(g.ExpectedStdout)
		if actual == want {
			r := 1
			return &r, "stdout matched", nil
		}
		r := 0
		return &r, fmt.Sprintf("stdout mismatch: got %q, want %q", actual, want), nil
	}
	r := 1
	d := actual
	if d == "" {
		d = "exit 0"
	}
	return &r, d, nil
}

// GradeResult evaluates a command step's Result against both JSON verdicts and
// the instance's ExpectedExitCode and ExpectedStdout.
func (g CommandGrader) GradeResult(res Result) (reward *int, detail string, err error) {
	combined := res.Stdout + res.Stderr
	if r, d, ok := parseCommandJSONVerdict(combined); ok {
		return r, d, nil
	}
	if res.ExitCode != g.ExpectedExitCode {
		r := 0
		return &r, fmt.Sprintf("exit %d, want %d", res.ExitCode, g.ExpectedExitCode), nil
	}
	if g.ExpectedStdout != "" {
		actual := strings.TrimSpace(res.Stdout)
		want := strings.TrimSpace(g.ExpectedStdout)
		if actual != want {
			r := 0
			return &r, fmt.Sprintf("stdout mismatch: got %q, want %q", actual, want), nil
		}
		r := 1
		return &r, "stdout matched", nil
	}
	r := 1
	d := strings.TrimSpace(res.Stdout)
	if d == "" {
		d = fmt.Sprintf("exit %d", res.ExitCode)
	}
	return &r, d, nil
}

// CommandAdapter is the built-in Adapter for generic "command" JSONL manifests.
type CommandAdapter struct{}

// Kind implements Adapter.
func (CommandAdapter) Kind() string { return CommandKind }

// Validate implements Adapter.
func (a CommandAdapter) Validate(in Instance) error {
	return a.ValidateInstance(in)
}

// ValidateInstance checks that in carries a non-empty command.
func (CommandAdapter) ValidateInstance(in Instance) error {
	task, err := DecodeCommandTask(in)
	if err != nil {
		return err
	}
	if strings.TrimSpace(task.Command) == "" {
		return fmt.Errorf("%s: command is required", in.InstanceID)
	}
	return nil
}

// RequireGrading checks that in is a valid command instance for grading trajectories.
func (a CommandAdapter) RequireGrading(in Instance) error {
	return a.ValidateInstance(in)
}

// TemplateData implements Adapter, exposing InstanceID, Image, Command,
// SetupCommand, ResetCommand, ExpectedStdout, and ExpectedExitCode.
func (CommandAdapter) TemplateData(in Instance) (any, error) {
	task, err := DecodeCommandTask(in)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(task.Command) == "" {
		return nil, fmt.Errorf("%s: command is required", in.InstanceID)
	}
	return task, nil
}

// Grades reports whether t references .ExpectedStdout or has a "grade" step.
func (CommandAdapter) Grades(t *Trajectory) bool {
	for _, steps := range [][]Step{t.Setup, t.Reset, t.Steps} {
		for _, s := range steps {
			if s.Name == "grade" || strings.Contains(s.Run, ".ExpectedStdout") {
				return true
			}
			if s.Write != nil && strings.Contains(s.Write.Content, ".ExpectedStdout") {
				return true
			}
		}
	}
	return false
}

// Grader implements Adapter.
func (CommandAdapter) Grader() Grader { return CommandGrader{} }

// GradeInstance implements InstanceGrader.
func (CommandAdapter) GradeInstance(in Instance, res Result) (*int, string, error) {
	task, err := DecodeCommandTask(in)
	if err != nil {
		return nil, "", err
	}
	return CommandGrader{
		ExpectedStdout:   task.ExpectedStdout,
		ExpectedExitCode: task.ExpectedExitCode,
	}.GradeResult(res)
}

// DefaultTrajectory returns the built-in default command trajectory YAML.
func (CommandAdapter) DefaultTrajectory() string {
	return defaultCommandTrajectoryYAML
}
