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
		Tail: `{"valid": true, "resolved": false, "f2p": "1/1", "p2p": "3/4"}`})
	rec.Record(Event{RunID: "r", Event: EventStep, Name: "grade", Instance: "repo__x-2", DurMS: 9, OK: true, Reward: intp(0), Tail: "no json"})
	rec.Record(Event{RunID: "r", Event: EventStep, Name: "grade", Instance: "repo__x-1", DurMS: 9, OK: true, Reward: intp(0),
		Tail: `{"valid": false, "resolved": false}`})
	rec.Record(Event{RunID: "r", Event: EventTask, Step: 2, DurMS: 700, OK: true, Reward: intp(0)})
	rec.Record(Event{RunID: "r", Event: EventBarrier, Step: 1, DurMS: 650, OK: true, Count: 2})
	rec.Record(Event{RunID: "r", Event: EventPark, Name: "suspend", Step: 1, DurMS: 90, OK: true, Count: 1})
	rec.Record(Event{RunID: "r", Event: EventRun, Name: "steps", DurMS: 2000, OK: false, Count: 3})
	if rec.Len() != 14 {
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
	if len(rep.Unresolved) != 2 || rep.Unresolved[0].Instance != "repo__x-1" || rep.Unresolved[0].Reason != "f2p 1/1 p2p 3/4" ||
		rep.Unresolved[1].Reason != "reward 0" {
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
