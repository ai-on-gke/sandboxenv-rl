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
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestBuiltinTrajectoriesLoadAndRender(t *testing.T) {
	names := Names()
	if len(names) == 0 {
		t.Fatal("no built-in trajectories")
	}
	in := Instance{InstanceID: "x", Image: "reg/img@" + goodDigest, Patch: "PATCH", EvalScript: "EVAL"}
	for _, n := range names {
		tr, err := LoadTrajectory(n)
		if err != nil {
			t.Fatalf("%s: %v", n, err)
		}
		r, err := tr.Render(in)
		if err != nil {
			t.Fatalf("%s render: %v", n, err)
		}
		if len(r.Steps) != len(tr.Steps) {
			t.Fatalf("%s: rendered %d steps, want %d", n, len(r.Steps), len(tr.Steps))
		}
		for _, s := range r.Steps {
			if s.Write != nil && (s.Write.Content == "" || s.Write.File != "") {
				t.Fatalf("%s step %s: write.file not resolved into content", n, s.Name)
			}
		}
		if len(r.Setup) != len(tr.Setup) || len(r.Reset) != len(tr.Reset) {
			t.Fatalf("%s: setup/reset not rendered", n)
		}
	}
	def, err := LoadTrajectory("")
	if err != nil || def.Name != DefaultTrajectory {
		t.Fatalf("default: %v %v", def, err)
	}
	if !def.HasReward() {
		t.Fatal("default trajectory has no reward step")
	}
}

func TestRenderSubstitutesInstanceFields(t *testing.T) {
	tr, err := ParseTrajectory([]byte(`
name: t
steps:
  - name: w
    write: {path: /tmp/p, content: "{{.Patch}}", mode: "0755"}
  - name: r
    run: "echo {{.InstanceID}}"
    think: 250ms
    timeout: 1m
    reward: true
`))
	if err != nil {
		t.Fatal(err)
	}
	r, err := tr.Render(Instance{InstanceID: "id-1", Patch: "line1\nline2"})
	if err != nil {
		t.Fatal(err)
	}
	if r.Steps[0].Write.Content != "line1\nline2" || r.Steps[1].Run != "echo id-1" {
		t.Fatalf("render: %+v", r.Steps)
	}
	if tr.Steps[1].Run != "echo {{.InstanceID}}" {
		t.Fatal("Render must not modify the original")
	}
	if time.Duration(r.Steps[1].Think) != 250*time.Millisecond || time.Duration(r.Steps[1].Timeout) != time.Minute {
		t.Fatalf("durations: %+v", r.Steps[1])
	}
	if m, _ := r.Steps[0].Write.FileMode(); m != 0o755 {
		t.Fatalf("mode: %v", m)
	}
}

func TestRenderUnknownFieldFails(t *testing.T) {
	tr, err := ParseTrajectory([]byte("name: t\nsteps:\n  - name: r\n    run: \"{{.Nope}}\"\n"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tr.Render(Instance{}); err == nil {
		t.Fatal("unknown field must fail at render")
	}
}

func TestParseTrajectoryRejects(t *testing.T) {
	cases := map[string]string{
		"no name":       "steps:\n  - name: a\n    run: true\n",
		"no steps":      "name: t\n",
		"run and write": "name: t\nsteps:\n  - name: a\n    run: x\n    write: {path: /p}\n",
		"neither":       "name: t\nsteps:\n  - name: a\n",
		"two rewards":   "name: t\nsteps:\n  - {name: a, run: x, reward: true}\n  - {name: b, run: y, reward: true}\n",
		"reward reset":  "name: t\nreset:\n  - {name: a, run: x, reward: true}\nsteps:\n  - {name: b, run: y}\n",
		"reward setup":  "name: t\nsetup:\n  - {name: a, run: x, reward: true}\nsteps:\n  - {name: b, run: y}\n",
		"write reward":  "name: t\nsteps:\n  - name: a\n    write: {path: /p}\n    reward: true\n",
		"bad mode":      "name: t\nsteps:\n  - name: a\n    write: {path: /p, mode: \"zz\"}\n",
		"bad think":     "name: t\nsteps:\n  - name: a\n    run: x\n    think: soon\n",
		"neg think":     "name: t\nsteps:\n  - name: a\n    run: x\n    think: -1s\n",
		"neg timeout":   "name: t\nsteps:\n  - name: a\n    run: x\n    timeout: -1m\n",
		"unknown field": "name: t\nsteps:\n  - name: a\n    run: x\n    bogus: 1\n",
	}
	for name, in := range cases {
		if _, err := ParseTrajectory([]byte(in)); err == nil {
			t.Errorf("%s: expected an error", name)
		} else if strings.Contains(err.Error(), "panic") {
			t.Errorf("%s: %v", name, err)
		}
	}
}

func TestWriteFileResolvesNextToTrajectory(t *testing.T) {
	dir := t.TempDir()
	// The asset contains template syntax on purpose: file contents are not
	// rendered.
	if err := os.WriteFile(filepath.Join(dir, "asset.py"), []byte("print('{{not a template}}')\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	yamlPath := filepath.Join(dir, "t.yaml")
	if err := os.WriteFile(yamlPath, []byte("name: t\nsteps:\n  - name: w\n    write: {path: /tmp/a.py, file: asset.py}\n  - name: r\n    run: \"true\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	tr, err := LoadTrajectory(yamlPath)
	if err != nil {
		t.Fatal(err)
	}
	if got := tr.Steps[0].Write.Content; got != "print('{{not a template}}')\n" {
		t.Fatalf("content %q", got)
	}
	r, err := tr.Render(Instance{InstanceID: "x"})
	if err != nil {
		t.Fatalf("rendering a trajectory with a file asset: %v", err)
	}
	if r.Steps[0].Write.Content != tr.Steps[0].Write.Content {
		t.Fatal("file asset contents must not be rendered")
	}
	for _, bad := range []string{"missing.py", "../asset.py", "sub/asset.py"} {
		if err := os.WriteFile(yamlPath, []byte("name: t\nsteps:\n  - name: w\n    write: {path: /tmp/a, file: "+bad+"}\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := LoadTrajectory(yamlPath); err == nil {
			t.Errorf("write.file %q must fail", bad)
		}
	}
	if _, err := ParseTrajectory([]byte("name: t\nsteps:\n  - name: w\n    write: {path: /p, file: a, content: b}\n")); err == nil {
		t.Error("file and content together must fail")
	}
}

func TestJSONTemplateFunc(t *testing.T) {
	tr, err := ParseTrajectory([]byte("name: t\nsteps:\n  - name: w\n    write: {path: /tmp/i.json, content: \"{{json .}}\"}\n"))
	if err != nil {
		t.Fatal(err)
	}
	in := Instance{InstanceID: "x", Image: "reg/img@" + goodDigest, FailToPass: []string{"a::b[1]"}, LogParser: "parse_log_pytest"}
	r, err := tr.Render(in)
	if err != nil {
		t.Fatal(err)
	}
	var back Instance
	if err := json.Unmarshal([]byte(r.Steps[0].Write.Content), &back); err != nil {
		t.Fatalf("not JSON: %v: %s", err, r.Steps[0].Write.Content)
	}
	if back.InstanceID != "x" || back.LogParser != "parse_log_pytest" || len(back.FailToPass) != 1 {
		t.Fatalf("round trip: %+v", back)
	}
}

func TestGrades(t *testing.T) {
	def, err := LoadTrajectory("")
	if err != nil {
		t.Fatal(err)
	}
	if !def.Grades() {
		t.Fatal("the default trajectory grades")
	}
	for _, n := range []string{"workload-file-read", "workload-compile", "workload-read-all"} {
		tr, err := LoadTrajectory(n)
		if err != nil {
			t.Fatalf("%s: %v", n, err)
		}
		if tr.Grades() {
			t.Fatalf("%s must not need grading material", n)
		}
		if tr.Steps[0].Name != "probe" || tr.Steps[0].Run != "true" {
			t.Fatalf("%s must start with the probe step so ttfi is the cold start alone", n)
		}
		if !tr.HasReward() {
			t.Fatalf("%s needs a reward step so a failed workload is visible", n)
		}
	}
	plain, err := ParseTrajectory([]byte("name: t\nsteps:\n  - {name: w, write: {path: /tmp/p, content: \"{{.Patch}}\"}}\n  - {name: r, run: \"true\"}\n"))
	if err != nil {
		t.Fatal(err)
	}
	if plain.Grades() {
		t.Fatal("a trajectory that uses only the patch does not grade")
	}
}

type customTemplateAdapter struct {
	kind string
	err  error
}

func (a customTemplateAdapter) Kind() string                  { return a.kind }
func (a customTemplateAdapter) Validate(Instance) error       { return nil }
func (a customTemplateAdapter) RequireGrading(Instance) error { return nil }
func (a customTemplateAdapter) TemplateData(in Instance) (any, error) {
	if a.err != nil {
		return nil, a.err
	}
	var m map[string]any
	if err := json.Unmarshal(in.Material, &m); err != nil {
		return nil, err
	}
	m["InstanceID"] = in.InstanceID
	return m, nil
}
func (a customTemplateAdapter) Grades(*Trajectory) bool { return false }
func (a customTemplateAdapter) Grader() Grader          { return nil }

func TestRenderCustomAdapterTemplateData(t *testing.T) {
	RegisterAdapter(customTemplateAdapter{kind: "custom-tmpl"})
	t.Cleanup(func() { unregisterAdapter("custom-tmpl") })

	tr, err := ParseTrajectory([]byte(`
name: custom
steps:
  - name: w
    write: {path: /tmp/custom.txt, content: "{{.InstanceID}}:{{.Prompt}}"}
  - name: r
    run: "echo {{.Prompt}}"
`))
	if err != nil {
		t.Fatal(err)
	}
	in := Instance{
		InstanceID: "task-1",
		Image:      "reg/img@" + goodDigest,
		Kind:       "custom-tmpl",
		Material:   json.RawMessage(`{"Prompt":"solve-me"}`),
	}
	rendered, err := tr.Render(in)
	if err != nil {
		t.Fatal(err)
	}
	if rendered.Steps[0].Write.Content != "task-1:solve-me" || rendered.Steps[1].Run != "echo solve-me" {
		t.Fatalf("rendered custom template data = %+v", rendered.Steps)
	}

	RegisterAdapter(customTemplateAdapter{kind: "custom-tmpl-err", err: os.ErrInvalid})
	t.Cleanup(func() { unregisterAdapter("custom-tmpl-err") })
	if _, err := tr.Render(Instance{InstanceID: "task-2", Kind: "custom-tmpl-err"}); err == nil {
		t.Fatal("TemplateData error must fail Render")
	}
}

type alwaysGradesAdapter struct{}

func (alwaysGradesAdapter) Kind() string            { return "always-grades" }
func (alwaysGradesAdapter) Validate(Instance) error { return nil }
func (alwaysGradesAdapter) RequireGrading(in Instance) error {
	if len(in.Material) == 0 {
		return os.ErrInvalid
	}
	return nil
}
func (alwaysGradesAdapter) TemplateData(in Instance) (any, error) { return in, nil }
func (alwaysGradesAdapter) Grades(*Trajectory) bool {
	// Exercise a recursive AdapterFor lookup inside Grades while a concurrent
	// writer might contend on adaptersMu.
	_ = AdapterFor(DefaultKind)
	return true
}
func (alwaysGradesAdapter) Grader() Grader { return nil }

func TestGradesPerInstanceKindAndLockSafety(t *testing.T) {
	RegisterAdapter(alwaysGradesAdapter{})
	t.Cleanup(func() { unregisterAdapter("always-grades") })

	workload, err := LoadTrajectory("workload-file-read")
	if err != nil {
		t.Fatal(err)
	}
	// An unrelated registered adapter that always returns true from Grades must
	// not cause default SWE-bench trajectories or instances to require grading.
	if workload.Grades() || workload.Grades(DefaultKind) {
		t.Fatal("workload-file-read must not grade for DefaultKind even when always-grades is registered")
	}
	if !workload.Grades("always-grades") {
		t.Fatal("workload-file-read must report Grades=true when queried for always-grades")
	}

	sweImageOnly := []Instance{{InstanceID: "swe-1", Image: "reg/img@" + goodDigest}}
	if err := RequireGrading(sweImageOnly, workload); err != nil {
		t.Fatalf("SWE-bench image-only instance with non-grading trajectory: got %v, want nil", err)
	}
	customMissing := []Instance{{InstanceID: "cust-1", Image: "reg/img@" + goodDigest, Kind: "always-grades"}}
	if err := RequireGrading(customMissing, workload); err == nil {
		t.Fatal("always-grades instance without grading material must fail RequireGrading")
	}
}
