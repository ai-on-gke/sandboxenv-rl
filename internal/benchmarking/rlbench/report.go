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

package rlbench

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"sort"
	"strings"
	"text/tabwriter"
)

// Stat summarizes one kind of event.
type Stat struct {
	Event  string  `json:"event"`
	Name   string  `json:"name,omitempty"`
	Count  int     `json:"count"`
	Failed int     `json:"failed"`
	P50MS  float64 `json:"p50_ms"`
	P95MS  float64 `json:"p95_ms"`
	MaxMS  float64 `json:"max_ms"`
	// Retried counts ttfi and wake events that took more than one attempt:
	// the environment was not serving when first asked.
	Retried int `json:"retried,omitempty"`
}

// Unresolved is an instance whose reward step scored 0, with the grader's
// verdict when available.
type Unresolved struct {
	Instance string `json:"instance"`
	Reason   string `json:"reason"`
}

// StepStat is one training step in steps mode.
type StepStat struct {
	Step     int `json:"step"`
	Tasks    int `json:"tasks"`
	Failed   int `json:"failed"`
	Rewarded int `json:"rewarded"`
	// Live is how many environments were still in the fleet at this step;
	// steps mode drops an environment after its first failure, so a
	// shrinking Live makes later steps' counts incomparable.
	Live      int     `json:"live"`
	BarrierMS float64 `json:"barrier_ms"`
	ParkMS    float64 `json:"park_ms"`
	WakeP50MS float64 `json:"wake_p50_ms"`
	WakeP95MS float64 `json:"wake_p95_ms"`
}

// Report is the summary of one run's JSONL, stable enough to diff between
// runs.
type Report struct {
	RunID      string     `json:"run"`
	Mode       string     `json:"mode,omitempty"`
	RunMS      float64    `json:"run_ms,omitempty"`
	Tasks      int        `json:"tasks"`
	Failed     int        `json:"failed"`
	Rewarded   int        `json:"rewarded"`
	RewardRate float64    `json:"reward_rate"`
	Stats      []Stat     `json:"stats"`
	Steps      []StepStat `json:"steps,omitempty"`
	// Errors counts distinct error strings, most frequent first.
	Errors []ErrorCount `json:"errors,omitempty"`
	// Unresolved lists the instances whose reward was 0, once each.
	Unresolved []Unresolved `json:"unresolved,omitempty"`
}

// ErrorCount is one error string and how often it occurred.
type ErrorCount struct {
	Err   string `json:"err"`
	Count int    `json:"count"`
}

type statKey struct{ event, name string }

// Builder folds a stream of events into a Report without materializing the full
// event slice in memory.
type Builder struct {
	rep        Report
	durs       map[statKey][]float64
	failed     map[statKey]int
	retried    map[statKey]int
	unresolved map[string]string
	order      []statKey
	steps      map[int]*StepStat
	wakes      map[int][]float64
	errs       map[string]int
}

// NewBuilder returns a streaming report Builder.
func NewBuilder() *Builder {
	return &Builder{
		durs:       map[statKey][]float64{},
		failed:     map[statKey]int{},
		retried:    map[statKey]int{},
		unresolved: map[string]string{},
		steps:      map[int]*StepStat{},
		wakes:      map[int][]float64{},
		errs:       map[string]int{},
	}
}

// Add folds one event into the Builder.
func (b *Builder) Add(e Event) {
	if b.rep.RunID == "" {
		b.rep.RunID = e.RunID
	}
	k := statKey{e.Event, ""}
	if e.Event == EventStep || e.Event == EventPark {
		k.name = e.Name
	}
	if _, ok := b.durs[k]; !ok {
		b.order = append(b.order, k)
		b.durs[k] = nil
	}
	if e.Attempts > 1 {
		b.retried[k]++
	}
	if e.Event == EventStep && e.Reward != nil && *e.Reward == 0 {
		if _, seen := b.unresolved[e.Instance]; !seen {
			b.unresolved[e.Instance] = resolveUnresolvedReason(e)
		}
	}
	if e.OK {
		b.durs[k] = append(b.durs[k], e.DurMS)
	} else {
		b.failed[k]++
		if e.Err != "" {
			b.errs[e.Err]++
		}
	}
	switch e.Event {
	case EventRun:
		b.rep.Mode = e.Name
		b.rep.RunMS = e.DurMS
	case EventTask:
		b.rep.Tasks++
		if !e.OK {
			b.rep.Failed++
		}
		if e.Reward != nil && *e.Reward == 1 {
			b.rep.Rewarded++
		}
		if e.Step > 0 {
			s := stepStat(b.steps, e.Step)
			s.Tasks++
			if !e.OK {
				s.Failed++
			}
			if e.Reward != nil && *e.Reward == 1 {
				s.Rewarded++
			}
		}
	case EventBarrier:
		stepStat(b.steps, e.Step).BarrierMS = e.DurMS
		stepStat(b.steps, e.Step).Live = e.Count
	case EventPark:
		stepStat(b.steps, e.Step).ParkMS = e.DurMS
	case EventWake:
		if e.OK {
			b.wakes[e.Step] = append(b.wakes[e.Step], e.DurMS)
		}
	}
}

// Finish computes exact percentiles over the accumulated durations and returns
// the finalized Report.
func (b *Builder) Finish() *Report {
	rep := b.rep
	if rep.Tasks > 0 {
		rep.RewardRate = float64(rep.Rewarded) / float64(rep.Tasks)
	}
	for _, k := range b.order {
		d := b.durs[k]
		sort.Float64s(d)
		rep.Stats = append(rep.Stats, Stat{
			Event:   k.event,
			Name:    k.name,
			Count:   len(d) + b.failed[k],
			Failed:  b.failed[k],
			P50MS:   percentile(d, 0.50),
			P95MS:   percentile(d, 0.95),
			MaxMS:   percentile(d, 1),
			Retried: b.retried[k],
		})
	}
	for step, s := range b.steps {
		w := b.wakes[step]
		sort.Float64s(w)
		cp := *s
		cp.WakeP50MS = percentile(w, 0.5)
		cp.WakeP95MS = percentile(w, 0.95)
		rep.Steps = append(rep.Steps, cp)
	}
	sort.Slice(rep.Steps, func(i, j int) bool { return rep.Steps[i].Step < rep.Steps[j].Step })
	for msg, n := range b.errs {
		rep.Errors = append(rep.Errors, ErrorCount{Err: msg, Count: n})
	}
	for inst, why := range b.unresolved {
		rep.Unresolved = append(rep.Unresolved, Unresolved{Instance: inst, Reason: why})
	}
	sort.Slice(rep.Unresolved, func(i, j int) bool { return rep.Unresolved[i].Instance < rep.Unresolved[j].Instance })
	sort.Slice(rep.Errors, func(i, j int) bool {
		if rep.Errors[i].Count != rep.Errors[j].Count {
			return rep.Errors[i].Count > rep.Errors[j].Count
		}
		return rep.Errors[i].Err < rep.Errors[j].Err
	})
	return &rep
}

// resolveUnresolvedReason returns e.Detail when present, falls back to asking
// registered dataset graders to parse e.Tail (for pre-existing JSONL files),
// and otherwise returns "reward 0".
func resolveUnresolvedReason(e Event) string {
	if e.Detail != "" {
		return e.Detail
	}
	if e.Tail != "" {
		if a := AdapterFor(DefaultKind); a != nil && a.Grader() != nil {
			if _, detail, err := a.Grader().Parse([]byte(e.Tail)); err == nil && detail != "" {
				return detail
			}
		}
		adaptersMu.RLock()
		defer adaptersMu.RUnlock()
		for _, a := range adapters {
			if g := a.Grader(); g != nil {
				if _, detail, err := g.Parse([]byte(e.Tail)); err == nil && detail != "" {
					return detail
				}
			}
		}
	}
	return "reward 0"
}

// Summarize streams JSONL events from r into a Builder and returns the Report.
func Summarize(r io.Reader) (*Report, error) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 1<<20), 16<<20)
	b := NewBuilder()
	for sc.Scan() {
		line := bytes.TrimSpace(sc.Bytes())
		if len(line) == 0 {
			continue
		}
		var e Event
		if err := json.Unmarshal(line, &e); err != nil {
			return nil, fmt.Errorf("parse event: %w", err)
		}
		b.Add(e)
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	return b.Finish(), nil
}

// Build computes the report from a slice of events in memory.
func Build(events []Event) *Report {
	b := NewBuilder()
	for _, e := range events {
		b.Add(e)
	}
	return b.Finish()
}

// WriteExclusions writes the unresolved instances in the exclude-file format:
// one id per line followed by the reason as a comment.
func (rep *Report) WriteExclusions(w io.Writer, header string) {
	fmt.Fprintf(w, "# %s\n# Generated from run %s: %d of %d tasks unresolved.\n", header, rep.RunID, len(rep.Unresolved), rep.Tasks)
	for _, u := range rep.Unresolved {
		fmt.Fprintf(w, "%s  %s\n", u.Instance, u.Reason)
	}
}

func stepStat(m map[int]*StepStat, step int) *StepStat {
	s, ok := m[step]
	if !ok {
		s = &StepStat{Step: step}
		m[step] = s
	}
	return s
}

// Percentile computes the nearest-rank percentile of a sorted slice.
func Percentile(sorted []float64, p float64) float64 {
	return percentile(sorted, p)
}

// percentile of a sorted slice by the nearest-rank method, ceil(p*n)-1; 0
// for an empty slice.
func percentile(sorted []float64, p float64) float64 {
	n := len(sorted)
	if n == 0 {
		return 0
	}
	if p >= 1 {
		return sorted[n-1]
	}
	i := int(math.Ceil(p*float64(n))) - 1
	if i < 0 {
		i = 0
	}
	if i >= n {
		i = n - 1
	}
	return sorted[i]
}

// WriteText prints the report as tables.
func (rep *Report) WriteText(w io.Writer) {
	fmt.Fprintf(w, "run %s mode=%s tasks=%d failed=%d rewarded=%d reward_rate=%.3f run=%.1fs\n\n",
		rep.RunID, rep.Mode, rep.Tasks, rep.Failed, rep.Rewarded, rep.RewardRate, rep.RunMS/1000)
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "event\tname\tcount\tfailed\tretried\tp50_ms\tp95_ms\tmax_ms")
	for _, s := range rep.Stats {
		fmt.Fprintf(tw, "%s\t%s\t%d\t%d\t%d\t%.0f\t%.0f\t%.0f\n", s.Event, s.Name, s.Count, s.Failed, s.Retried, s.P50MS, s.P95MS, s.MaxMS)
	}
	tw.Flush()
	if len(rep.Steps) > 0 {
		fmt.Fprintln(w)
		tw = tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
		fmt.Fprintln(tw, "step\tlive\ttasks\tfailed\trewarded\tbarrier_s\tpark_s\twake_p50_ms\twake_p95_ms")
		for _, s := range rep.Steps {
			fmt.Fprintf(tw, "%d\t%d\t%d\t%d\t%d\t%.1f\t%.1f\t%.0f\t%.0f\n", s.Step, s.Live, s.Tasks, s.Failed, s.Rewarded,
				s.BarrierMS/1000, s.ParkMS/1000, s.WakeP50MS, s.WakeP95MS)
		}
		tw.Flush()
	}
	if len(rep.Unresolved) > 0 {
		fmt.Fprintf(w, "\nunresolved: %d instances (report -unresolved FILE writes them in exclude-file format)\n", len(rep.Unresolved))
	}
	if len(rep.Errors) > 0 {
		fmt.Fprintln(w)
		fmt.Fprintln(w, "errors:")
		for i, e := range rep.Errors {
			if i == 10 {
				fmt.Fprintf(w, "  ... %d more\n", len(rep.Errors)-10)
				break
			}
			msg := strings.ReplaceAll(e.Err, "\n", " ")
			if len(msg) > 160 {
				msg = msg[:160] + "..."
			}
			fmt.Fprintf(w, "  %5d  %s\n", e.Count, msg)
		}
	}
}
