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
	"strings"
	"testing"

	"github.com/agent-substrate/env/internal/benchmarking/rlbench"
)

const goodDigest = "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

func manifest(ids ...string) string {
	var b strings.Builder
	for _, id := range ids {
		b.WriteString(`{"instance_id":"` + id + `","repo":"r","image":"reg/img@` + goodDigest + `","patch":"p","eval_script":"e","log_parser":"parse_log_pytest"}` + "\n")
	}
	return b.String()
}

func TestDecodeAndRequireGrading(t *testing.T) {
	full, err := rlbench.ParseDataset(strings.NewReader(manifest("a")))
	if err != nil {
		t.Fatal(err)
	}
	if full[0].Kind != Kind {
		t.Fatalf("Kind = %q, want %q", full[0].Kind, Kind)
	}
	swe, err := Decode(full[0])
	if err != nil {
		t.Fatal(err)
	}
	if swe.Repo != "r" || swe.Patch != "p" || swe.EvalScript != "e" || swe.LogParser != "parse_log_pytest" {
		t.Fatalf("decoded %+v", swe)
	}
	if err := rlbench.RequireGrading(full); err != nil {
		t.Fatalf("complete record: %v", err)
	}
	imageOnly, err := rlbench.ParseDataset(strings.NewReader(`{"instance_id":"x","image":"reg/img@` + goodDigest + `"}` + "\n"))
	if err != nil {
		t.Fatalf("an image-only record must parse: %v", err)
	}
	if err := rlbench.RequireGrading(imageOnly); err == nil || !strings.Contains(err.Error(), "eval_script") {
		t.Fatalf("image-only record must fail grading requirement: %v", err)
	}
	noParser, _ := rlbench.ParseDataset(strings.NewReader(`{"instance_id":"x","image":"reg/img@` + goodDigest + `","eval_script":"e"}` + "\n"))
	if err := rlbench.RequireGrading(noParser); err == nil || !strings.Contains(err.Error(), "log_parser") {
		t.Fatalf("missing log_parser must fail grading requirement: %v", err)
	}
}

func TestBuiltinSWEBenchGoldTrajectoryAndGrades(t *testing.T) {
	def, err := rlbench.LoadTrajectory("")
	if err != nil {
		t.Fatal(err)
	}
	if def.Name != DefaultTrajectory {
		t.Fatalf("default trajectory = %q, want %q", def.Name, DefaultTrajectory)
	}
	if !def.Grades() {
		t.Fatal("the default swebench-gold trajectory grades")
	}
	if !def.HasReward() {
		t.Fatal("swebench-gold has a reward step")
	}
	in, err := pack(Instance{
		InstanceID: "x",
		Image:      "reg/img@" + goodDigest,
		Patch:      "PATCH",
		EvalScript: "EVAL",
		FailToPass: []string{"a::b[1]"},
		LogParser:  "parse_log_pytest",
	})
	if err != nil {
		t.Fatal(err)
	}
	r, err := def.Render(in)
	if err != nil {
		t.Fatalf("render swebench-gold: %v", err)
	}
	for _, s := range r.Steps {
		if s.Write != nil && (s.Write.Content == "" || s.Write.File != "") {
			t.Fatalf("step %s: write.file not resolved into content", s.Name)
		}
	}
	for _, n := range []string{"workload-file-read", "workload-compile", "workload-read-all"} {
		tr, err := rlbench.LoadTrajectory(n)
		if err != nil {
			t.Fatalf("%s: %v", n, err)
		}
		if tr.Grades() {
			t.Fatalf("%s must not need grading material", n)
		}
	}
	plain, err := rlbench.ParseTrajectory([]byte("name: t\nsteps:\n  - {name: w, write: {path: /tmp/p, content: \"{{.Patch}}\"}}\n  - {name: r, run: \"true\"}\n"))
	if err != nil {
		t.Fatal(err)
	}
	if plain.Grades() {
		t.Fatal("a trajectory that uses only the patch does not grade")
	}
}

func TestJSONTemplateFuncRoundTripsSWEBenchInstance(t *testing.T) {
	tr, err := rlbench.ParseTrajectory([]byte("name: t\nsteps:\n  - name: w\n    write: {path: /tmp/i.json, content: \"{{json .}}\"}\n"))
	if err != nil {
		t.Fatal(err)
	}
	in, err := pack(Instance{
		InstanceID: "x",
		Image:      "reg/img@" + goodDigest,
		FailToPass: []string{"a::b[1]"},
		LogParser:  "parse_log_pytest",
	})
	if err != nil {
		t.Fatal(err)
	}
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

// verdict extracts the grader's verdict from the end of a reward step's
// output: "invalid run", "f2p A/B p2p C/D", or "reward 0" when the output
// does not end in the grader's JSON line.
func verdict(tail string) string {
	_, detail, err := (Grader{}).Parse([]byte(tail))
	if err != nil || detail == "" {
		return "reward 0"
	}
	return detail
}

func TestGraderAndSummarizeVerdictFallback(t *testing.T) {
	if v := verdict(`{"valid": false}`); v != "invalid run" {
		t.Fatalf("verdict = %q, want invalid run", v)
	}
	if v := verdict(`{"valid": true, "resolved": false, "f2p": "1/1", "p2p": "3/4"}`); v != "f2p 1/1 p2p 3/4" {
		t.Fatalf("verdict = %q", v)
	}
	if v := verdict("no json"); v != "reward 0" {
		t.Fatalf("verdict = %q", v)
	}

	// Verify that old events.jsonl files with only Tail (and empty Detail) still
	// produce the exact SWE-bench verdict strings via the registered Grader.
	r0 := 0
	var buf bytes.Buffer
	rec := rlbench.NewRecorder(&buf)
	rec.Record(rlbench.Event{RunID: "r", Event: rlbench.EventStep, Name: "grade", Instance: "repo__x-1", DurMS: 9, OK: true, Reward: &r0,
		Tail: `{"valid": true, "resolved": false, "f2p": "1/1", "p2p": "3/4"}`})
	rec.Record(rlbench.Event{RunID: "r", Event: rlbench.EventStep, Name: "grade", Instance: "repo__x-2", DurMS: 9, OK: true, Reward: &r0,
		Tail: "no json"})
	rec.Record(rlbench.Event{RunID: "r", Event: rlbench.EventTask, Step: 1, DurMS: 100, OK: true, Reward: &r0})
	rec.Record(rlbench.Event{RunID: "r", Event: rlbench.EventRun, Name: "eval", DurMS: 100, OK: true, Count: 1})
	if err := rec.Close(); err != nil {
		t.Fatal(err)
	}
	rep, err := rlbench.Summarize(&buf)
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Unresolved) != 2 || rep.Unresolved[0].Instance != "repo__x-1" || rep.Unresolved[0].Reason != "f2p 1/1 p2p 3/4" ||
		rep.Unresolved[1].Reason != "reward 0" {
		t.Fatalf("unresolved: %+v", rep.Unresolved)
	}
}
