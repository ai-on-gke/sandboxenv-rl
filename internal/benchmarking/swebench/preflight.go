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
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/agent-substrate/env/internal/benchmarking/schema"
)

// Pre-flight event names, next to the loader's own. A prewarm event is
// recorded per image when its template is ensured, so Validity Rule 1 and
// the report's prewarm count see a real pre-flight.
const (
	EventPreflightGolden = "preflight_golden" // one image's golden wait
	EventPreflightProbe  = "preflight_probe"  // one probe environment: created, ready, node read, released
)

// PreflightOptions configure a PreflightWarmer.
type PreflightOptions struct {
	// Plan is the image placement to fill; PlanPath names it in the report.
	Plan     *schema.ImagePlacement
	PlanPath string
	Backend  ImageWarmer
	// Recorder receives the events; nil records nothing.
	Recorder *Recorder
	Logger   *slog.Logger
	// RunID prefixes the probe environment names (at most 20 characters).
	RunID string
	// ProbesPerNode is how many probes a round starts per node of a domain;
	// a round holds its probes alive until all are placed, so the scheduler
	// spreads them. Default 2.
	ProbesPerNode int
	// MaxRounds caps the rounds per (image, domain); default 3.
	MaxRounds int
	// GoldenTimeout bounds the wait for one image's golden; default 30 min.
	// SkipGoldenWait ensures the templates and moves on, for a layer-only
	// wave while the bakes run.
	GoldenTimeout  time.Duration
	GoldenPoll     time.Duration
	SkipGoldenWait bool
	// ProbeTimeout bounds one probe's create and first command; default 10 min.
	ProbeTimeout time.Duration
	// Pin suspends the probes instead of deleting them: their local
	// snapshots pin them to their nodes and keep the layers referenced.
	Pin bool
	// Images and Domains, when set, restrict the pass to those.
	Images  []string
	Domains []string
	// EnsureWidth is how many templates are ensured and polled at once;
	// default 8.
	EnsureWidth int

	now func() time.Time
}

// PreflightWarmer fills the node caches an image placement plan describes:
// it ensures each image's derived template and waits for its golden, then
// per (image, domain) creates probe environments with the domain's worker
// selector until every node of the domain has run the image once, reading
// the node each probe landed on, and releases them.
type PreflightWarmer struct {
	opts PreflightOptions
}

// NewPreflightWarmer validates opts and applies the defaults.
func NewPreflightWarmer(opts PreflightOptions) (*PreflightWarmer, error) {
	if opts.Plan == nil {
		return nil, errors.New("preflight: a plan is required")
	}
	if err := opts.Plan.Validate(); err != nil {
		return nil, fmt.Errorf("preflight: %w", err)
	}
	if opts.Backend == nil {
		return nil, errors.New("preflight: a backend is required")
	}
	if opts.RunID == "" {
		opts.RunID = "pf"
	}
	if len(opts.RunID) > maxRunIDLen || !runIDPattern.MatchString(opts.RunID) {
		return nil, fmt.Errorf("preflight: run id %q must match %s and be at most %d characters", opts.RunID, runIDPattern, maxRunIDLen)
	}
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	if opts.ProbesPerNode <= 0 {
		opts.ProbesPerNode = 2
	}
	if opts.MaxRounds <= 0 {
		opts.MaxRounds = 3
	}
	if opts.GoldenTimeout <= 0 {
		opts.GoldenTimeout = 30 * time.Minute
	}
	if opts.GoldenPoll <= 0 {
		opts.GoldenPoll = 10 * time.Second
	}
	if opts.ProbeTimeout <= 0 {
		opts.ProbeTimeout = 10 * time.Minute
	}
	if opts.EnsureWidth <= 0 {
		opts.EnsureWidth = 8
	}
	if opts.now == nil {
		opts.now = time.Now
	}
	for _, name := range opts.Domains {
		if opts.Plan.Domain(name) == nil {
			return nil, fmt.Errorf("preflight: the plan has no domain %q", name)
		}
	}
	for _, img := range opts.Images {
		if opts.Plan.Entry(img) == nil {
			return nil, fmt.Errorf("preflight: the plan does not place %q", img)
		}
	}
	return &PreflightWarmer{opts: opts}, nil
}

// Run executes the pre-flight and returns the coverage report. The error is
// non-nil when the pass is incomplete (a golden not ready, a pair short of
// its nodes) or the context ended; the report is returned either way, so
// the caller can write it.
func (w *PreflightWarmer) Run(ctx context.Context) (*schema.PreflightCoverage, error) {
	o := w.opts
	started := o.now()
	images := w.images()
	domains := w.domains()

	report := &schema.PreflightCoverage{
		SchemaVersion: schema.SchemaVersion,
		RunID:         o.RunID,
		StartedAt:     started,
		Plan:          o.PlanPath,
		Pinned:        o.Pin,
	}

	goldens, goldensOK := w.ensureGoldens(ctx, images)
	report.Goldens = goldens

	coverage, coverageOK := w.cover(ctx, images, domains)
	report.Domains = coverage
	report.FinishedAt = o.now()
	report.Complete = goldensOK && coverageOK && ctx.Err() == nil

	if o.Recorder != nil {
		_ = o.Recorder.Flush()
	}
	if ctx.Err() != nil {
		return report, ctx.Err()
	}
	if !report.Complete {
		return report, fmt.Errorf("pre-flight incomplete: %s", w.shortfall(report))
	}
	return report, nil
}

func (w *PreflightWarmer) images() []string {
	if len(w.opts.Images) > 0 {
		out := append([]string(nil), w.opts.Images...)
		sort.Strings(out)
		return out
	}
	out := make([]string, 0, len(w.opts.Plan.Images))
	for _, e := range w.opts.Plan.Images {
		out = append(out, e.Image)
	}
	return out
}

func (w *PreflightWarmer) domains() []schema.PlacementDomain {
	if len(w.opts.Domains) == 0 {
		return w.opts.Plan.Domains
	}
	want := make(map[string]bool, len(w.opts.Domains))
	for _, n := range w.opts.Domains {
		want[n] = true
	}
	var out []schema.PlacementDomain
	for _, d := range w.opts.Plan.Domains {
		if want[d.Name] {
			out = append(out, d)
		}
	}
	return out
}

// ensureGoldens derives every image's template and, unless skipped, waits
// for its golden. It reports whether every golden it waited for is ready.
func (w *PreflightWarmer) ensureGoldens(ctx context.Context, images []string) ([]schema.GoldenRecord, bool) {
	o := w.opts
	records := make([]schema.GoldenRecord, len(images))
	var mu sync.Mutex
	allOK := true
	forEach(ctx, len(images), o.EnsureWidth, func(i int) {
		img := images[i]
		rec := schema.GoldenRecord{Image: img}
		t0 := o.now()
		st, err := o.Backend.EnsureImageTemplate(ctx, img)
		w.record(Event{Event: EventPrewarm, Task: i, Image: img, Env: st.Template, DurMS: DurMS(o.now().Sub(t0)), OK: err == nil, Err: errString(err)})
		if err != nil {
			rec.State = "failed"
			rec.Error = err.Error()
			o.Logger.Warn("preflight: ensure template failed", slog.String("image", img), slog.Any("err", err))
			mu.Lock()
			records[i], allOK = rec, false
			mu.Unlock()
			return
		}
		rec.Template = st.Template
		if o.SkipGoldenWait {
			rec.State, rec.Tag, rec.Error = "skipped", st.Tag, st.Error
			mu.Lock()
			records[i] = rec
			mu.Unlock()
			return
		}
		waitStart := o.now()
		st, err = w.waitGolden(ctx, img, st)
		rec.WaitMS = DurMS(o.now().Sub(waitStart))
		rec.State, rec.Tag, rec.Error = string(st.Golden), st.Tag, st.Error
		ok := err == nil && st.Golden == GoldenReady
		if err != nil {
			rec.Error = err.Error()
		}
		w.record(Event{Event: EventPreflightGolden, Task: i, Image: img, Env: st.Template, DurMS: rec.WaitMS, OK: ok, Err: rec.Error})
		if !ok {
			o.Logger.Warn("preflight: golden not ready", slog.String("image", img), slog.String("state", rec.State), slog.String("err", rec.Error))
		}
		mu.Lock()
		records[i] = rec
		if !ok {
			allOK = false
		}
		mu.Unlock()
	})
	return records, allOK
}

// waitGolden polls the image's status until its golden is ready or failed,
// or the timeout passes.
func (w *PreflightWarmer) waitGolden(ctx context.Context, img string, st ImageState) (ImageState, error) {
	o := w.opts
	deadline := o.now().Add(o.GoldenTimeout)
	for {
		switch st.Golden {
		case GoldenReady:
			return st, nil
		case GoldenFailed:
			return st, fmt.Errorf("golden bake failed: %s", st.Error)
		}
		if o.now().After(deadline) {
			return st, fmt.Errorf("golden still %s after %s", st.Golden, o.GoldenTimeout)
		}
		select {
		case <-ctx.Done():
			return st, ctx.Err()
		case <-time.After(o.GoldenPoll):
		}
		next, err := o.Backend.ImageStatus(ctx, img)
		if err != nil {
			return st, err
		}
		st = next
	}
}

// cover runs the coverage pass, one domain at a time in parallel, the
// images of a domain one after another so a domain never has more than
// nodes x ProbesPerNode probes in flight.
func (w *PreflightWarmer) cover(ctx context.Context, images []string, domains []schema.PlacementDomain) ([]schema.DomainCoverage, bool) {
	wanted := make(map[string]bool, len(images))
	for _, img := range images {
		wanted[img] = true
	}
	results := make([]schema.DomainCoverage, len(domains))
	forEach(ctx, len(domains), len(domains), func(di int) {
		d := domains[di]
		dc := schema.DomainCoverage{Name: d.Name, Nodes: d.Nodes, NodesSeen: []string{}, Images: []schema.ImageCoverage{}, Complete: true}
		seen := map[string]bool{}
		for ii, e := range w.opts.Plan.Images {
			if !wanted[e.Image] || !contains(e.Domains, d.Name) {
				continue
			}
			if ctx.Err() != nil {
				dc.Complete = false
				break
			}
			ic := w.coverImage(ctx, di, ii, d, e.Image)
			for _, n := range ic.Nodes {
				seen[n] = true
			}
			if !ic.Complete {
				dc.Complete = false
			}
			dc.Images = append(dc.Images, ic)
		}
		dc.NodesSeen = sortedKeys(seen)
		if len(dc.NodesSeen) > d.Nodes {
			w.opts.Logger.Warn("preflight: domain has more nodes than the plan says", slog.String("domain", d.Name), slog.Int("plan", d.Nodes), slog.Int("seen", len(dc.NodesSeen)))
		}
		results[di] = dc
	})
	allOK := ctx.Err() == nil
	for _, dc := range results {
		if !dc.Complete {
			allOK = false
		}
	}
	return results, allOK
}

// coverImage runs rounds of probes for one image in one domain until every
// node of the domain has run it or the rounds run out.
func (w *PreflightWarmer) coverImage(ctx context.Context, di, ii int, d schema.PlacementDomain, img string) schema.ImageCoverage {
	o := w.opts
	ic := schema.ImageCoverage{Image: img, Nodes: []string{}, Wanted: d.Nodes}
	nodes := map[string]bool{}
	pinned := map[string]string{}
	width := d.Nodes * o.ProbesPerNode
	for round := 1; round <= o.MaxRounds && len(nodes) < d.Nodes && ctx.Err() == nil; round++ {
		ic.Rounds = round
		type outcome struct {
			id   string
			sb   Sandbox
			node string
			err  error
		}
		outcomes := make([]outcome, width)
		// Create and wait for every probe of the round before releasing any,
		// so they occupy distinct slots while the scheduler places the rest.
		forEach(ctx, width, width, func(k int) {
			id := probeID(o.RunID, di, ii, round, k)
			t0 := o.now()
			pctx, cancel := context.WithTimeout(ctx, o.ProbeTimeout)
			defer cancel()
			var node string
			sb, err := o.Backend.CreatePlaced(pctx, id, img, d.Selector)
			if err == nil {
				// The first command returns once the restore finished: the
				// layers and the golden snapshot are on the node by then.
				if _, rerr := sb.Run(pctx, "true", o.ProbeTimeout); rerr != nil {
					err = fmt.Errorf("first command: %w", rerr)
				} else if node, err = o.Backend.Node(pctx, id); err == nil && node == "" {
					err = errors.New("the environment reports no node")
				}
			}
			w.record(Event{Event: EventPreflightProbe, Image: img, Env: id, Node: node, Attempts: round, DurMS: DurMS(o.now().Sub(t0)), OK: err == nil, Err: errString(err), Labels: map[string]string{"domain": d.Name}})
			outcomes[k] = outcome{id: id, sb: sb, node: node, err: err}
		})
		for _, oc := range outcomes {
			ic.Probes++
			if oc.err != nil {
				ic.Failed++
				ic.Error = oc.err.Error()
			} else {
				nodes[oc.node] = true
			}
			if oc.sb == nil {
				if oc.id != "" {
					// A create that failed client-side may exist server-side.
					if del, ok := o.Backend.(Backend); ok {
						_ = del.Delete(context.WithoutCancel(ctx), oc.id)
					}
				}
				continue
			}
			rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), cleanupTimeout)
			if o.Pin && oc.err == nil {
				if err := oc.sb.Suspend(rctx); err != nil {
					o.Logger.Warn("preflight: suspend failed, deleting", slog.String("env", oc.id), slog.Any("err", err))
					_ = oc.sb.Delete(rctx)
				} else {
					pinned[oc.node] = oc.id
				}
			} else if err := oc.sb.Delete(rctx); err != nil {
				o.Logger.Warn("preflight: delete failed", slog.String("env", oc.id), slog.Any("err", err))
			}
			cancel()
		}
		if ic.Failed == ic.Probes {
			// Nothing of this round came up: the image, the domain or the
			// cluster is the problem, and more rounds would not change it.
			o.Logger.Warn("preflight: every probe of the round failed", slog.String("image", img), slog.String("domain", d.Name), slog.String("err", ic.Error))
			break
		}
	}
	ic.Nodes = sortedKeys(nodes)
	ic.Complete = len(ic.Nodes) >= d.Nodes
	if len(pinned) > 0 {
		ic.Pinned = pinned
	}
	if o.Recorder != nil {
		_ = o.Recorder.Flush()
	}
	return ic
}

// probeID names a probe environment: short, Substrate-valid, unique per
// run, domain, image, round and slot.
func probeID(runID string, domain, image, round, slot int) string {
	return fmt.Sprintf("%s-%d-%d-%d-%d", runID, domain, image, round, slot)
}

func (w *PreflightWarmer) record(e Event) {
	if w.opts.Recorder != nil {
		w.opts.Recorder.Record(e)
	}
}

// shortfall summarises what kept the pass from completing.
func (w *PreflightWarmer) shortfall(r *schema.PreflightCoverage) string {
	return preflightShortfall(r)
}

// preflightShortfall names what a pre-flight report is missing: goldens not
// ready, (image, domain) pairs short of their nodes, or an interruption.
func preflightShortfall(r *schema.PreflightCoverage) string {
	var parts []string
	notReady := 0
	for _, g := range r.Goldens {
		if g.State != "ready" && g.State != "skipped" {
			notReady++
		}
	}
	if notReady > 0 {
		parts = append(parts, fmt.Sprintf("%d goldens not ready", notReady))
	}
	short := 0
	for _, d := range r.Domains {
		for _, ic := range d.Images {
			if !ic.Complete {
				short++
			}
		}
	}
	if short > 0 {
		parts = append(parts, fmt.Sprintf("%d (image, domain) pairs short of their nodes", short))
	}
	if len(parts) == 0 {
		parts = append(parts, "interrupted")
	}
	return strings.Join(parts, "; ")
}

func errString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// WritePreflightCoverage validates c and writes it as indented JSON.
func WritePreflightCoverage(path string, c *schema.PreflightCoverage) error {
	if err := c.Validate(); err != nil {
		return err
	}
	return writeJSONFile(path, c)
}

// ReadPreflightCoverage reads and validates a preflight-coverage.json.
func ReadPreflightCoverage(path string) (*schema.PreflightCoverage, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	c, err := schema.DecodeJSONStrict[schema.PreflightCoverage](f)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if err := c.Validate(); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return &c, nil
}
