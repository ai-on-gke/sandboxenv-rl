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
	"errors"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestPercentile(t *testing.T) {
	s := []float64{1, 2, 3, 4, 5, 6, 7, 8, 9, 10}
	if p := percentile(s, 0.5); p != 5 {
		t.Errorf("p50 = %v", p)
	}
	if p := percentile(s, 0.95); p != 10 {
		t.Errorf("p95 = %v", p)
	}
	if p := percentile(s, 1); p != 10 {
		t.Errorf("max = %v", p)
	}
	if p := percentile(nil, 0.5); p != 0 {
		t.Errorf("empty = %v", p)
	}
	thirteen := make([]float64, 13)
	for i := range thirteen {
		thirteen[i] = float64(i + 1)
	}
	if p := percentile(thirteen, 0.95); p != 13 {
		t.Errorf("nearest rank p95 of 13 samples = %v, want the 13th", p)
	}
}

func TestSummarizeRoundTrip(t *testing.T) {
	var buf bytes.Buffer
	rec := NewRecorder(&buf)
	rec.Record(Event{RunID: "r", Event: EventCreate, Env: "e1", DurMS: 100, OK: true})
	rec.Record(Event{RunID: "r", Event: EventCreate, Env: "e2", DurMS: 300, OK: false, Err: "boom"})
	rec.Record(Event{RunID: "r", Event: EventStep, Name: "run_tests", DurMS: 50, OK: true, Exit: intp(0)})
	rec.Record(Event{RunID: "r", Event: EventStep, Name: "explore", DurMS: 5, OK: true, Exit: intp(0)})
	rec.Record(Event{RunID: "r", Event: EventTask, Step: 1, DurMS: 500, OK: true, Reward: intp(1)})
	rec.Record(Event{RunID: "r", Event: EventTask, Step: 1, DurMS: 600, OK: false})
	rec.Record(Event{RunID: "r", Event: EventWake, Step: 2, DurMS: 40, OK: true, Attempts: 3})
	rec.Record(Event{RunID: "r", Event: EventStep, Name: "grade", Instance: "repo__x-1", DurMS: 9, OK: true, Reward: intp(0),
		Detail: "f2p 1/1 p2p 3/4"})
	rec.Record(Event{RunID: "r", Event: EventStep, Name: "grade", Instance: "repo__x-2", DurMS: 9, OK: true, Reward: intp(0), Tail: "no json"})
	rec.Record(Event{RunID: "r", Event: EventStep, Name: "grade", Instance: "repo__x-3", DurMS: 9, OK: true, Reward: intp(0),
		Tail: `{"valid": true, "resolved": false, "f2p": "0/1", "p2p": "2/2"}`})
	rec.Record(Event{RunID: "r", Event: EventStep, Name: "grade", Instance: "repo__x-1", DurMS: 9, OK: true, Reward: intp(0),
		Tail: `{"valid": false, "resolved": false}`})
	rec.Record(Event{RunID: "r", Event: EventTask, Step: 2, DurMS: 700, OK: true, Reward: intp(0)})
	rec.Record(Event{RunID: "r", Event: EventBarrier, Step: 1, DurMS: 650, OK: true, Count: 2})
	rec.Record(Event{RunID: "r", Event: EventPark, Name: "suspend", Step: 1, DurMS: 90, OK: true, Count: 1})
	rec.Record(Event{RunID: "r", Event: EventRun, Name: "steps", DurMS: 2000, OK: false, Count: 3})
	if err := rec.Close(); err != nil {
		t.Fatal(err)
	}
	if rec.Len() != 15 {
		t.Fatalf("recorded %d", rec.Len())
	}

	rep, err := Summarize(strings.NewReader(buf.String()))
	if err != nil {
		t.Fatal(err)
	}
	if rep.RunID != "r" || rep.Mode != "steps" || rep.RunMS != 2000 {
		t.Fatalf("header: %+v", rep)
	}
	if rep.Tasks != 3 || rep.Failed != 1 || rep.Rewarded != 1 {
		t.Fatalf("tasks: %+v", rep)
	}
	find := func(event, name string) Stat {
		for _, s := range rep.Stats {
			if s.Event == event && s.Name == name {
				return s
			}
		}
		t.Fatalf("no stat %s/%s in %+v", event, name, rep.Stats)
		return Stat{}
	}
	if c := find(EventCreate, ""); c.Count != 2 || c.Failed != 1 || c.P50MS != 100 || c.MaxMS != 100 {
		t.Fatalf("create: %+v", c)
	}
	if s := find(EventStep, "run_tests"); s.Count != 1 || s.P50MS != 50 {
		t.Fatalf("step: %+v", s)
	}
	if len(rep.Steps) != 2 || rep.Steps[0].BarrierMS != 650 || rep.Steps[0].ParkMS != 90 || rep.Steps[0].Live != 2 || rep.Steps[1].WakeP50MS != 40 {
		t.Fatalf("steps: %+v", rep.Steps)
	}
	if len(rep.Errors) != 1 || rep.Errors[0].Err != "boom" || rep.Errors[0].Count != 1 {
		t.Fatalf("errors: %+v", rep.Errors)
	}
	if w := find(EventWake, ""); w.Retried != 1 {
		t.Fatalf("wake retried: %+v", w)
	}
	if len(rep.Unresolved) != 3 || rep.Unresolved[0].Instance != "repo__x-1" || rep.Unresolved[0].Reason != "f2p 1/1 p2p 3/4" ||
		rep.Unresolved[1].Reason != "reward 0" || rep.Unresolved[2].Reason != "f2p 0/1 p2p 2/2" {
		t.Fatalf("unresolved: %+v", rep.Unresolved)
	}
	var excl bytes.Buffer
	rep.WriteExclusions(&excl, "test list")
	if !strings.Contains(excl.String(), "# test list\n") || !strings.Contains(excl.String(), "repo__x-1  f2p 1/1 p2p 3/4\n") {
		t.Fatalf("exclusions:\n%s", excl.String())
	}
	if v := verdict(`{"valid": false}`); v != "invalid run" {
		t.Fatalf("verdict: %q", v)
	}
	var text bytes.Buffer
	rep.WriteText(&text)
	for _, want := range []string{"run r", "create", "run_tests", "barrier_s", "boom"} {
		if !strings.Contains(text.String(), want) {
			t.Errorf("text report lacks %q:\n%s", want, text.String())
		}
	}
}

type failingWriter struct{}

func (failingWriter) Write([]byte) (int, error) { return 0, errors.New("disk full") }

func TestRecorderKeepsFirstWriteError(t *testing.T) {
	rec := NewRecorder(failingWriter{})
	rec.Record(Event{RunID: "r", Event: EventCreate})
	rec.Record(Event{RunID: "r", Event: EventDelete})
	if err := rec.Err(); err == nil || err.Error() != "disk full" {
		t.Fatalf("Err = %v", err)
	}
	if NewRecorder(nil).Err() != nil {
		t.Fatal("a discarding recorder has no error")
	}
}

type spyWriter struct {
	buf    bytes.Buffer
	writes int
}

func (s *spyWriter) Write(p []byte) (int, error) {
	s.writes++
	return s.buf.Write(p)
}

func TestRecorderBufferedFlushAndClose(t *testing.T) {
	spy := &spyWriter{}
	rec := NewRecorder(spy)
	// Normal fine-grained events stay in the 64 KiB buffer until a boundary event or Flush/Close.
	rec.Record(Event{RunID: "r", Event: EventCreate, Env: "e1", DurMS: 10, OK: true})
	rec.Record(Event{RunID: "r", Event: EventStep, Name: "explore", Env: "e1", DurMS: 20, OK: true})
	if spy.writes != 0 {
		t.Fatalf("writes before boundary/flush = %d, want 0", spy.writes)
	}
	// Coarse boundary events (step_barrier, park, run) auto-flush immediately.
	rec.Record(Event{RunID: "r", Event: EventBarrier, Step: 1, DurMS: 30, OK: true, Count: 1})
	if spy.writes != 1 {
		t.Fatalf("writes after EventBarrier = %d, want 1", spy.writes)
	}
	rec.Record(Event{RunID: "r", Event: EventPark, Name: "suspend", Step: 1, DurMS: 15, OK: true, Count: 1})
	if spy.writes != 2 {
		t.Fatalf("writes after EventPark = %d, want 2", spy.writes)
	}
	rec.Record(Event{RunID: "r", Event: EventDelete, Env: "e1", DurMS: 5, OK: true})
	if spy.writes != 2 {
		t.Fatalf("writes after EventDelete = %d, want 2 (still buffered)", spy.writes)
	}
	if err := rec.Close(); err != nil {
		t.Fatalf("Close() = %v", err)
	}
	if spy.writes != 3 {
		t.Fatalf("writes after Close() = %d, want 3", spy.writes)
	}
	evs, err := ReadEvents(&spy.buf)
	if err != nil || len(evs) != 5 {
		t.Fatalf("ReadEvents = %d, %v; want 5 events", len(evs), err)
	}
}

func TestBuildAndSummarizeParity(t *testing.T) {
	var buf bytes.Buffer
	rec := NewRecorder(&buf)
	input := []Event{
		{RunID: "parity", Event: EventCreate, Env: "e1", Instance: "repo__a-1", DurMS: 110, OK: true},
		{RunID: "parity", Event: EventCreate, Env: "e2", Instance: "repo__a-2", DurMS: 210, OK: false, Err: "create boom"},
		{RunID: "parity", Event: EventTTFI, Env: "e1", Instance: "repo__a-1", Step: 1, DurMS: 950, OK: true, Attempts: 2},
		{RunID: "parity", Event: EventStep, Name: "grade", Env: "e1", Instance: "repo__a-1", Step: 1, DurMS: 45, OK: true, Reward: intp(0), Detail: "f2p 0/1 p2p 3/3"},
		{RunID: "parity", Event: EventStep, Name: "grade", Env: "e2", Instance: "repo__a-2", Step: 1, DurMS: 55, OK: true, Reward: intp(0), Tail: `{"valid": true, "resolved": false, "f2p": "0/2", "p2p": "1/1"}`},
		{RunID: "parity", Event: EventTask, Env: "e1", Instance: "repo__a-1", Step: 1, DurMS: 1000, OK: true, Reward: intp(0)},
		{RunID: "parity", Event: EventTask, Env: "e2", Instance: "repo__a-2", Step: 1, DurMS: 1200, OK: false},
		{RunID: "parity", Event: EventBarrier, Step: 1, DurMS: 1250, OK: true, Count: 2},
		{RunID: "parity", Event: EventPark, Name: "suspend", Step: 1, DurMS: 80, OK: true, Count: 1},
		{RunID: "parity", Event: EventWake, Env: "e1", Instance: "repo__a-1", Step: 2, DurMS: 35, OK: true, Attempts: 1},
		{RunID: "parity", Event: EventTask, Env: "e1", Instance: "repo__a-1", Step: 2, DurMS: 800, OK: true, Reward: intp(1)},
		{RunID: "parity", Event: EventRun, Name: "steps", DurMS: 2500, OK: false, Count: 3},
	}
	for _, e := range input {
		rec.Record(e)
	}
	if err := rec.Close(); err != nil {
		t.Fatal(err)
	}
	evs, err := ReadEvents(strings.NewReader(buf.String()))
	if err != nil {
		t.Fatal(err)
	}
	fromBuild := Build(evs)
	fromStream, err := Summarize(strings.NewReader(buf.String()))
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(fromBuild, fromStream) {
		t.Fatalf("Build vs Summarize mismatch:\nBuild:     %+v\nSummarize: %+v", fromBuild, fromStream)
	}
	if fromStream.RunID != "parity" || fromStream.Mode != "steps" || fromStream.RunMS != 2500 {
		t.Fatalf("unexpected header: %+v", fromStream)
	}
	if fromStream.Tasks != 3 || fromStream.Failed != 1 || fromStream.Rewarded != 1 || fromStream.RewardRate != 1.0/3.0 {
		t.Fatalf("unexpected task totals: Tasks=%d Failed=%d Rewarded=%d RewardRate=%v",
			fromStream.Tasks, fromStream.Failed, fromStream.Rewarded, fromStream.RewardRate)
	}
	if len(fromStream.Errors) != 1 || fromStream.Errors[0] != (ErrorCount{Err: "create boom", Count: 1}) {
		t.Fatalf("unexpected errors: %+v", fromStream.Errors)
	}
	wantUnresolved := []Unresolved{
		{Instance: "repo__a-1", Reason: "f2p 0/1 p2p 3/3"},
		{Instance: "repo__a-2", Reason: "f2p 0/2 p2p 1/1"},
	}
	if !reflect.DeepEqual(fromStream.Unresolved, wantUnresolved) {
		t.Fatalf("unexpected unresolved: got %+v, want %+v", fromStream.Unresolved, wantUnresolved)
	}
	wantSteps := []StepStat{
		{Step: 1, Tasks: 2, Failed: 1, Rewarded: 0, Live: 2, BarrierMS: 1250, ParkMS: 80},
		{Step: 2, Tasks: 1, Failed: 0, Rewarded: 1, WakeP50MS: 35, WakeP95MS: 35},
	}
	if !reflect.DeepEqual(fromStream.Steps, wantSteps) {
		t.Fatalf("unexpected steps: got %+v, want %+v", fromStream.Steps, wantSteps)
	}
}

func TestWriteComparison(t *testing.T) {
	a := Build([]Event{
		{RunID: "a", Event: EventCreate, DurMS: 100, OK: true},
		{RunID: "a", Event: EventTTFI, DurMS: 900, OK: true, Attempts: 2},
		{RunID: "a", Event: EventTask, Step: 1, DurMS: 500, OK: true, Reward: intp(1)},
		{RunID: "a", Event: EventWake, Step: 1, DurMS: 40, OK: true},
		{RunID: "a", Event: EventBarrier, Step: 1, DurMS: 700, OK: true, Count: 3},
		{RunID: "a", Event: EventPark, Name: "suspend", Step: 1, DurMS: 100, OK: true, Count: 1},
		{RunID: "a", Event: EventRun, Name: "steps", DurMS: 2000, OK: true, Count: 1},
	})
	b := Build([]Event{
		{RunID: "b", Event: EventCreate, DurMS: 50, OK: true},
		{RunID: "b", Event: EventCreate, DurMS: 70, OK: false, Err: "boom"},
		{RunID: "b", Event: EventDelete, DurMS: 70, OK: false, Err: "boom"},
		{RunID: "b", Event: EventTask, DurMS: 400, OK: true, Reward: intp(1)}, // eval mode: step 0
		{RunID: "b", Event: EventRun, Name: "eval", DurMS: 1000, OK: true, Count: 1},
	})
	var out bytes.Buffer
	WriteComparison(&out, []*Report{a, b}, []string{"runs/a.jsonl", "runs/b.jsonl"})
	text := out.String()
	for _, want := range []string{"100/100 n=1", "50/50 n=2 f=1", "900/900 n=1 r=1", "-/- n=1 f=1", "live=3 tasks=1 0.7/0.1 40/40"} {
		if !strings.Contains(text, want) {
			t.Errorf("comparison lacks %q:\n%s", want, text)
		}
	}
	if strings.Count(text, "cells:") != 1 || !strings.HasPrefix(text, "cells:") {
		t.Errorf("the legend must appear once, above the table:\n%s", text)
	}
	// A run without the event, or without the step, shows a dash in its column.
	for _, label := range []string{"ttfi", "step 1"} {
		found := false
		for _, line := range strings.Split(text, "\n") {
			if strings.HasPrefix(line, label+" ") {
				found = true
				if !strings.HasSuffix(strings.TrimSpace(line), "-") {
					t.Errorf("%s row should end with a dash for run b: %q", label, line)
				}
			}
		}
		if !found {
			t.Errorf("no %s row:\n%s", label, text)
		}
	}
	header := strings.Split(text, "\n")[1]
	if fields := strings.Fields(header); len(fields) != 3 || fields[0] != "run" || fields[1] != "a" || fields[2] != "b" {
		t.Errorf("header row: %q", header)
	}

	// Two files with the same run id, or none, are told apart by file name,
	// and by the full path when the file names repeat too.
	same := Build([]Event{{RunID: "a", Event: EventCreate, DurMS: 1, OK: true}, {RunID: "a", Event: EventRun, Name: "eval", DurMS: 1, OK: true}})
	empty := Build(nil)
	got := columnLabels([]*Report{a, same, empty}, []string{"x/first.jsonl", "x/second.jsonl", ""})
	if want := []string{"first", "second", "run 3"}; strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("labels = %q, want %q", got, want)
	}
	got = columnLabels([]*Report{a, same}, []string{"east/run.jsonl", "west/run.jsonl"})
	if want := []string{"east/run.jsonl", "west/run.jsonl"}; strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("labels = %q, want %q", got, want)
	}

	// A step with no wake, the first one or -park none, shows a dash for wake.
	nowake := Build([]Event{
		{RunID: "n", Event: EventTask, Step: 1, DurMS: 500, OK: true, Reward: intp(1)},
		{RunID: "n", Event: EventBarrier, Step: 1, DurMS: 700, OK: true, Count: 1},
		{RunID: "n", Event: EventRun, Name: "steps", DurMS: 1000, OK: true, Count: 1},
	})
	out.Reset()
	WriteComparison(&out, []*Report{nowake}, []string{"n.jsonl"})
	if !strings.Contains(out.String(), "live=1 tasks=1 0.7/0.0 -/-") {
		t.Errorf("step without wakes:\n%s", out.String())
	}
}

func TestNodeCacheDelta(t *testing.T) {
	const scope = `otel_scope_name="go.ate.dev/substrate/internal/imagecache",otel_scope_version=""`
	req := func(node, outcome string, v int) string {
		return node + "\tate_imagecache_requests_total{ate_imagecache_outcome=\"" + outcome + "\"," + scope + "} " + strconv.Itoa(v) + "\n"
	}
	start := func(node string, unix int) string {
		return node + "\tprocess_start_time_seconds " + strconv.Itoa(unix) + "\n"
	}
	before := "n1\t# sampled 1000\n" + start("n1", 500) +
		"n1\t# HELP ate_imagecache_requests_total Number of image lookups\n" +
		req("n1", "hit", 10) + req("n1", "miss", 4) +
		"n1\tate_imagecache_requests_created{ate_imagecache_outcome=\"miss\"} 1.7e+09\n" + // not the counter
		"n2\t# sampled 1000\n" + start("n2", 500) + req("n2", "miss", 7) +
		"n3\t# sampled 1000\n" + start("n3", 500) + req("n3", "hit", 1)
	after := "n1\t# sampled 2000\n" + start("n1", 500) +
		req("n1", "hit", 12) + req("n1", "miss", 9) +
		"n1\tate_imagecache_requests_total{ate_imagecache_outcome=\"error\",error_type=\"429\"," + scope + "} 1\n" +
		"n2\t# sampled 2000\n" + start("n2", 1500) + req("n2", "miss", 2) + // atelet restarted: count from zero
		"n4\t# sampled 2000\n" + "n4\tprocess_start_time_seconds{otel_scope_name=\"x\"} 1.5e+03\n" + req("n4", "hit", 1) + req("n4", "miss", 3) + // node added after the before sample; labeled form
		"n6\t# sampled 2000\n" + start("n6", 400) + req("n6", "miss", 100) // older than the before sample: its before scrape failed
	b, err := ParseNodeCacheSample(strings.NewReader(before))
	if err != nil {
		t.Fatal(err)
	}
	a, err := ParseNodeCacheSample(strings.NewReader(after))
	if err != nil {
		t.Fatal(err)
	}
	if !b.Time.Equal(time.Unix(1000, 0)) || !b.Nodes["n1"].Equal(time.Unix(500, 0)) || b.Requests["n1"]["miss"] != 4 {
		t.Fatalf("before sample: %+v", b)
	}
	d := b.Delta(a)
	want := CacheDelta{Nodes: 1, Restarted: 1, Added: 1, Removed: 1, Skipped: 1, Hits: 3, Misses: 10, Errors: 1, NodesWithMisses: 3}
	if d != want {
		t.Fatalf("delta = %+v, want %+v", d, want)
	}
	for _, bad := range []string{
		"n1 ate_imagecache_requests_total{ate_imagecache_outcome=\"hit\"} 1\n",
		"n1\tate_imagecache_requests_total{otel_scope_name=\"x\"} 1\n",
		"n1\tate_imagecache_requests_total{ate_imagecache_outcome=\"hit\"} many\n",
		"n1\t# sampled soon\n",
	} {
		if _, err := ParseNodeCacheSample(strings.NewReader(bad)); err == nil {
			t.Errorf("line %q must be rejected", bad)
		}
	}

	rep := Build([]Event{
		{RunID: "r", Event: EventTTFI, DurMS: 100, OK: true},
		{RunID: "r", Event: EventTTFI, DurMS: 100, OK: true},
		{RunID: "r", Event: EventRun, Name: "eval", DurMS: 1000, OK: true, Count: 2},
	})
	rep.Cache = &d
	var text bytes.Buffer
	rep.WriteText(&text)
	want2 := "image cache during the run: 1 nodes differenced, 1 restarted, counted from zero, 1 added, counted from zero, 1 removed, left out, 1 with no before sample, left out; misses (cold pulls) 10 on 3 nodes, hits 3, errors 1; first commands 2, misses per first command 5.00"
	if !strings.Contains(text.String(), want2) {
		t.Fatalf("text report lacks %q:\n%s", want2, text.String())
	}
}

func TestJoinRestores(t *testing.T) {
	rec := func(ts, actor, errType string, phases string) string {
		e := ""
		if errType != "" {
			e = `,"error.type":"` + errType + `"`
		}
		return `{"time":"2026-10-21T` + ts + `Z","level":"INFO","msg":"Restore timing breakdown","ate.atespace":"ate-env","ate.actor.name":"` + actor +
			`","ate.actor.uid":"u","ate.template.name":"t","ate.snapshot.fidelity":"memory"` + e + phases + "}\n"
	}
	const total = `,"ate.actor.restore.duration.total":`
	log := `{"time":"2026-10-21T10:00:00Z","level":"INFO","msg":"Something else","ate.actor.name":"r-a-0"}` + "\n" +
		// The worker's own record: same message, other key prefix, skipped.
		`{"time":"2026-10-21T10:00:03Z","msg":"Restore timing breakdown","ate.actor.name":"r-a-0","ateworker.actor.restore.duration.total":2}` + "\n" +
		rec("09:00:00", "r-a-0", "", total+"1") + // an earlier run with the same names
		rec("10:00:01", "r-a-0", "", `,"ate.actor.restore.duration.manifest_fetch":0.1,"ate.actor.restore.duration.download":2,"ate.actor.restore.duration.oci_unpack":5,"ate.actor.restore.duration.worker_restore":1`+total+"8.1") +
		rec("10:03:00", "r-a-0", "", total+"1") + // inside the run, inside no window
		rec("10:05:01", "r-a-0", "", `,"ate.actor.restore.duration.download":0.5,"ate.actor.restore.duration.worker_restore":0.5`+total+"1") +
		rec("10:00:00.5", "r-b-0", "DeadlineExceeded", `,"ate.actor.restore.duration.download":30`+total+"30.5") +
		rec("10:00:40", "r-b-0", "", `,"ate.actor.restore.duration.download":3`+total+"4") +
		rec("10:00:18", "r-c-0", "", total+"2") +
		rec("10:10:01", "r-c-0", "", total+"1") +
		rec("10:20:01", "r-c-0", "", total+"1.5") + // its wake event failed
		rec("10:30:01", "r-c-0", "", `,"ate.actor.restore.duration.ateom_restore":0.4`+total+"1") + // an older atelet's phase name
		rec("10:00:02", "other-x-0", "", total+"1")
	timings, err := ParseRestoreTimings(strings.NewReader(log))
	if err != nil {
		t.Fatal(err)
	}
	if len(timings["r-a-0"]) != 4 || timings["r-a-0"][1].Phases["total"] != 8.1 || timings["r-b-0"][0].Err != "DeadlineExceeded" {
		t.Fatalf("timings: %+v", timings)
	}
	for _, bad := range []string{
		"not json\n",
		`{"time":"2026-10-21T10:00:00Z","msg":"Restore timing breakdown"` + total + "1}\n",
		`{"msg":"Restore timing breakdown","ate.actor.name":"x"` + total + "1}\n",
		`{"time":"garbage","msg":"Restore timing breakdown","ate.actor.name":"x"` + total + "1}\n",
	} {
		if _, err := ParseRestoreTimings(strings.NewReader(bad)); err == nil {
			t.Errorf("%q must be rejected", bad)
		}
	}

	at := func(s string) time.Time {
		v, _ := time.Parse(time.RFC3339, "2026-10-21T"+s+"Z")
		return v
	}
	events := []Event{
		{Time: at("10:00:10"), RunID: "r", Event: EventTTFI, Env: "r-a-0", DurMS: 10100, OK: true},
		{Time: at("10:05:02"), RunID: "r", Event: EventWake, Env: "r-a-0", Step: 2, DurMS: 1500, OK: true},
		{Time: at("10:00:45"), RunID: "r", Event: EventTTFI, Env: "r-b-0", DurMS: 46000, OK: true, Attempts: 2},
		{Time: at("10:00:20"), RunID: "r", Event: EventTTFI, Env: "r-c-0", DurMS: 5000, OK: true},
		{Time: at("10:10:02"), RunID: "r", Event: EventWake, Env: "r-c-0", Step: 2, DurMS: 2000, OK: true},
		{Time: at("10:20:03"), RunID: "r", Event: EventWake, Env: "r-c-0", Step: 3, DurMS: 3000, OK: false, Err: "timeout"},
		{Time: at("10:30:02"), RunID: "r", Event: EventWake, Env: "r-c-0", Step: 4, DurMS: 2000, OK: true},
	}
	b := JoinRestores(events, timings)
	if b.OutsideRun != 1 || b.Unmatched != 1 || b.Unpaired != 1 {
		t.Fatalf("counts: %+v", b)
	}
	if b.First.Restores != 3 || b.First.Failed != 1 || b.First.FailedEvents != 0 {
		t.Fatalf("first: %+v", b.First)
	}
	if b.Wakes.Restores != 4 || b.Wakes.Failed != 0 || b.Wakes.FailedEvents != 1 {
		t.Fatalf("wakes: %+v", b.Wakes)
	}
	find := func(g RestoreGroup, phase string) PhaseStat {
		for _, p := range g.Phases {
			if p.Phase == phase {
				return p
			}
		}
		t.Fatalf("no %s in %+v", phase, g.Phases)
		return PhaseStat{}
	}
	if p := find(b.First, "download"); p.Count != 2 || p.MaxMS != 3000 {
		t.Fatalf("first download: %+v", p)
	}
	if p := find(b.First, "total"); p.Count != 3 || p.MaxMS != 8100 {
		t.Fatalf("first total: %+v", p)
	}
	if b.First.Phases[0].Phase != "manifest_fetch" || b.First.Phases[len(b.First.Phases)-1].Phase != "total" {
		t.Fatalf("first phase order: %+v", b.First.Phases)
	}
	// outside = event span - total: a 10100-8100, b 46000-4000, c 5000-2000.
	if o := b.First.Outside; o == nil || o.Count != 3 || o.P50MS != 3000 || o.MaxMS != 42000 {
		t.Fatalf("first outside: %+v", o)
	}
	// Wakes: a 1500-1000, c 2000-1000, c 2000-1000; the failed wake's restore is left out.
	if o := b.Wakes.Outside; o == nil || o.Count != 3 || o.P50MS != 1000 || o.MaxMS != 1000 {
		t.Fatalf("wake outside: %+v", o)
	}
	n := len(b.Wakes.Phases)
	if n < 2 || b.Wakes.Phases[n-1].Phase != "ateom_restore" || b.Wakes.Phases[n-2].Phase != "total" {
		t.Fatalf("unknown phase must follow the known ones: %+v", b.Wakes.Phases)
	}

	rep := Build(events)
	rep.Restores = b
	var text bytes.Buffer
	rep.WriteText(&text)
	flat := strings.Join(strings.Fields(text.String()), " ")
	for _, want := range []string{"1 records before or after this run", "1 records for actors not in this run", "1 records inside no ttfi or wake window", "first failed 1", "wake failed_events 1", "wake total 4"} {
		if !strings.Contains(flat, want) {
			t.Errorf("text report lacks %q:\n%s", want, text.String())
		}
	}
}
