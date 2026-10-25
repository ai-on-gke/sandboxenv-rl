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
		// The knob without a default; the others take theirs.
		tr.Vars = map[string]string{"READ_FILE": "/testbed/setup.py"}
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
	for _, n := range []string{"workload-file-read-one", "workload-file-read", "workload-compile", "workload-read-all"} {
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

func TestVarTemplateFunc(t *testing.T) {
	tr, err := ParseTrajectory([]byte(`
name: vars
steps:
  - {name: a, run: "cat {{var \"READ_FILE\" \"/default.py\"}} && echo {{var \"COUNT\" \"200\"}}", reward: true}
`))
	if err != nil {
		t.Fatal(err)
	}
	in := Instance{InstanceID: "x", Image: "reg/img@" + goodDigest}
	got, err := tr.Render(in)
	if err != nil {
		t.Fatal(err)
	}
	if got.Steps[0].Run != "cat '/default.py' && echo '200'" {
		t.Fatalf("defaults: %q", got.Steps[0].Run)
	}
	used, err := tr.VarsUsed(in)
	if err != nil || !used["READ_FILE"] || !used["COUNT"] || len(used) != 2 {
		t.Fatalf("VarsUsed = %v, %v", used, err)
	}
	// A value is one shell word whatever it holds: a quote cannot end it.
	tr.Vars = map[string]string{"READ_FILE": "/testbed/it's.py; rm -rf /"}
	got, err = tr.Render(in)
	if err != nil {
		t.Fatal(err)
	}
	if got.Steps[0].Run != `cat '/testbed/it'\''s.py; rm -rf /' && echo '200'` {
		t.Fatalf("set value: %q", got.Steps[0].Run)
	}
	twoDefaults, err := ParseTrajectory([]byte(`
name: twodefaults
steps:
  - {name: a, run: "echo {{var \"X\" \"1\" \"2\"}}", reward: true}
`))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := twoDefaults.Render(in); err == nil || !strings.Contains(err.Error(), "one default at most") {
		t.Fatalf("two defaults must fail to render: %v", err)
	}
	noDefault, err := ParseTrajectory([]byte(`
name: nodefault
steps:
  - {name: a, run: "cat {{var \"MUST\"}}", reward: true}
`))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := noDefault.Render(in); err == nil || !strings.Contains(err.Error(), "MUST") {
		t.Fatalf("a variable without a value or a default must fail to render: %v", err)
	}
	noDefault.Vars = map[string]string{"MUST": "/x"}
	got, err = noDefault.Render(in)
	if err != nil || got.Steps[0].Run != "cat '/x'" {
		t.Fatalf("set without default: %v, %v", got, err)
	}
}
