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
	"context"
	"encoding/json"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/agent-substrate/env/internal/benchmarking/schema"
)

const (
	pfImgA = "registry.example/swebench/a@sha256:" + "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	pfImgB = "registry.example/swebench/b@sha256:" + "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
)

// twoDomainPlan places a in d0 and b in both domains, two nodes each.
func twoDomainPlan() *schema.ImagePlacement {
	return &schema.ImagePlacement{
		SchemaVersion: schema.SchemaVersion,
		UnpackFactor:  1,
		Replication:   1,
		Domains: []schema.PlacementDomain{
			{Name: "d0", Selector: map[string]string{"ate-env/domain": "d0"}, Nodes: 2, NodeCacheBytes: 100, AssignedBytes: 2, ImageCount: 2},
			{Name: "d1", Selector: map[string]string{"ate-env/domain": "d1"}, Nodes: 2, NodeCacheBytes: 100, AssignedBytes: 1, ImageCount: 1},
		},
		Images: []schema.ImagePlacementEntry{
			{Image: pfImgA, Domains: []string{"d0"}, BytesCompressed: 1, BytesUnpacked: 1},
			{Image: pfImgB, Domains: []string{"d0", "d1"}, BytesCompressed: 1, BytesUnpacked: 1},
		},
	}
}

func newPreflightFake() *fakeAPI {
	return &fakeAPI{
		domainNodes:      map[string][]string{"d0": {"node-d0-1", "node-d0-2"}, "d1": {"node-d1-1", "node-d1-2"}},
		goldenReadyAfter: 2,
	}
}

func runPreflight(t *testing.T, api *fakeAPI, mutate func(*PreflightOptions)) (*schema.PreflightCoverage, error, []Event) {
	t.Helper()
	b := newFakeBackend(t, api)
	var buf bytes.Buffer
	rec := NewRecorder(&buf)
	opts := PreflightOptions{
		Plan:          twoDomainPlan(),
		PlanPath:      "image-placement.json",
		Backend:       b,
		Recorder:      rec,
		RunID:         "pf-test",
		ProbesPerNode: 1,
		MaxRounds:     3,
		GoldenTimeout: time.Second,
		GoldenPoll:    time.Millisecond,
		ProbeTimeout:  5 * time.Second,
	}
	if mutate != nil {
		mutate(&opts)
	}
	w, err := NewPreflightWarmer(opts)
	if err != nil {
		t.Fatal(err)
	}
	report, runErr := w.Run(context.Background())
	if err := rec.Close(); err != nil {
		t.Fatal(err)
	}
	var events []Event
	for _, line := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
		if line == "" {
			continue
		}
		var e Event
		if err := json.Unmarshal([]byte(line), &e); err != nil {
			t.Fatalf("event %q: %v", line, err)
		}
		events = append(events, e)
	}
	if err := report.Validate(); err != nil {
		t.Fatalf("report does not validate: %v", err)
	}
	return report, runErr, events
}

func countEvents(events []Event, kind string) int {
	n := 0
	for _, e := range events {
		if e.Event == kind {
			n++
		}
	}
	return n
}

func TestPreflightCoversEveryNodeOfEveryDomain(t *testing.T) {
	api := newPreflightFake()
	report, err, events := runPreflight(t, api, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !report.Complete || report.Pinned {
		t.Errorf("report complete=%v pinned=%v, want true false", report.Complete, report.Pinned)
	}
	// Goldens: ensured once per image (in parallel, so in any order), polled
	// until ready, tag recorded.
	ensured := append([]string(nil), api.ensured...)
	sort.Strings(ensured)
	if !reflect.DeepEqual(ensured, []string{pfImgA, pfImgB}) {
		t.Errorf("ensured = %v", api.ensured)
	}
	for _, g := range report.Goldens {
		if g.State != "ready" || !strings.HasPrefix(g.Tag, "golden-") || g.Template == "" {
			t.Errorf("golden %+v, want ready with a tag and template", g)
		}
	}
	// Coverage: a on both nodes of d0; b on both nodes of d0 and d1.
	want := map[string]map[string][]string{
		"d0": {pfImgA: {"node-d0-1", "node-d0-2"}, pfImgB: {"node-d0-1", "node-d0-2"}},
		"d1": {pfImgB: {"node-d1-1", "node-d1-2"}},
	}
	for _, d := range report.Domains {
		if !d.Complete {
			t.Errorf("domain %s incomplete: %+v", d.Name, d)
		}
		for _, ic := range d.Images {
			if got := want[d.Name][ic.Image]; !reflect.DeepEqual(ic.Nodes, got) || !ic.Complete || ic.Rounds != 1 || ic.Probes != 2 || ic.Failed != 0 {
				t.Errorf("%s/%s = %+v, want nodes %v in one round of two probes", d.Name, ic.Image, ic, got)
			}
		}
	}
	// Every probe carried its domain's selector and was deleted, not suspended.
	for id, sel := range api.selectors {
		domain := sel["ate-env/domain"]
		if domain == "" {
			t.Errorf("probe %s has no domain selector: %v", id, sel)
		}
	}
	if len(api.deleted) != len(api.created) || len(api.suspended) != 0 {
		t.Errorf("created %d, deleted %d, suspended %d; want all deleted", len(api.created), len(api.deleted), len(api.suspended))
	}
	// Events: a prewarm per image (Validity Rule 1 sees a real pre-flight), a
	// golden wait per image, a probe per environment with its node.
	if n := countEvents(events, EventPrewarm); n != 2 {
		t.Errorf("prewarm events = %d, want 2", n)
	}
	if n := countEvents(events, EventPreflightGolden); n != 2 {
		t.Errorf("golden events = %d, want 2", n)
	}
	probes := 0
	for _, e := range events {
		if e.Event != EventPreflightProbe {
			continue
		}
		probes++
		if e.Node == "" || e.Labels["domain"] == "" || !e.OK || e.Env == "" || e.Image == "" {
			t.Errorf("probe event lacks node, domain, env or image: %+v", e)
		}
	}
	if probes != len(api.created) {
		t.Errorf("probe events = %d, created = %d", probes, len(api.created))
	}
}

func TestPreflightPinLeavesProbesSuspended(t *testing.T) {
	api := newPreflightFake()
	report, err, _ := runPreflight(t, api, func(o *PreflightOptions) { o.Pin = true })
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !report.Pinned || len(api.suspended) != len(api.created) || len(api.deleted) != 0 {
		t.Errorf("pinned=%v created %d suspended %d deleted %d; want all suspended", report.Pinned, len(api.created), len(api.suspended), len(api.deleted))
	}
	for _, d := range report.Domains {
		for _, ic := range d.Images {
			if len(ic.Pinned) != d.Nodes {
				t.Errorf("%s/%s pinned = %v, want one per node", d.Name, ic.Image, ic.Pinned)
			}
			for node, id := range ic.Pinned {
				if api.nodes[id] != node {
					t.Errorf("pinned %s recorded on %s, the fake put it on %s", id, node, api.nodes[id])
				}
			}
		}
	}
}

func TestPreflightReportsShortCoverageAfterItsRounds(t *testing.T) {
	api := newPreflightFake()
	api.sameNode = true
	report, err, _ := runPreflight(t, api, nil)
	if err == nil || !strings.Contains(err.Error(), "short of their nodes") {
		t.Fatalf("Run error = %v, want a shortfall", err)
	}
	if report.Complete {
		t.Error("report complete with one node per domain")
	}
	for _, d := range report.Domains {
		if d.Complete || len(d.NodesSeen) != 1 {
			t.Errorf("domain %s: complete=%v nodes seen %v", d.Name, d.Complete, d.NodesSeen)
		}
		for _, ic := range d.Images {
			if ic.Complete || ic.Rounds != 3 || len(ic.Nodes) != 1 || ic.Probes != 6 {
				t.Errorf("%s/%s = %+v, want 3 rounds of 2 probes on one node", d.Name, ic.Image, ic)
			}
		}
	}
	if len(api.deleted) != len(api.created) {
		t.Errorf("created %d, deleted %d", len(api.created), len(api.deleted))
	}
}

func TestPreflightGoldenFailureAndSkip(t *testing.T) {
	api := newPreflightFake()
	api.goldenFail = map[string]string{pfImgB: "GoldenActorCrashed: boom"}
	report, err, events := runPreflight(t, api, nil)
	if err == nil || !strings.Contains(err.Error(), "1 goldens not ready") {
		t.Fatalf("Run error = %v, want one golden not ready", err)
	}
	var b schema.GoldenRecord
	for _, g := range report.Goldens {
		if g.Image == pfImgB {
			b = g
		}
	}
	if b.State != "failed" || !strings.Contains(b.Error, "boom") {
		t.Errorf("b golden = %+v, want failed with the reason", b)
	}
	// The coverage pass still ran: a failed golden means boots, and boots
	// fill the layers too.
	for _, d := range report.Domains {
		if !d.Complete {
			t.Errorf("domain %s incomplete with a failed golden: %+v", d.Name, d)
		}
	}
	failedGoldenEvents := 0
	for _, e := range events {
		if e.Event == EventPreflightGolden && !e.OK {
			failedGoldenEvents++
		}
	}
	if failedGoldenEvents != 1 {
		t.Errorf("failed golden events = %d, want 1", failedGoldenEvents)
	}

	// Skipping the wait records the state and completes on coverage alone.
	api = newPreflightFake()
	api.goldenReadyAfter = 1 << 20
	report, err, _ = runPreflight(t, api, func(o *PreflightOptions) { o.SkipGoldenWait = true })
	if err != nil || !report.Complete {
		t.Fatalf("skip golden wait: err=%v complete=%v", err, report.Complete)
	}
	for _, g := range report.Goldens {
		if g.State != "skipped" || g.WaitMS != 0 {
			t.Errorf("golden = %+v, want skipped", g)
		}
	}
	if n := api.statusCalls[pfImgA]; n != 1 {
		t.Errorf("status calls with the wait skipped = %d, want the one from ensure", n)
	}
}

func TestPreflightGoldenTimeout(t *testing.T) {
	api := newPreflightFake()
	api.goldenReadyAfter = 1 << 20
	report, err, _ := runPreflight(t, api, func(o *PreflightOptions) { o.GoldenTimeout = 5 * time.Millisecond })
	if err == nil || !strings.Contains(err.Error(), "2 goldens not ready") {
		t.Fatalf("Run error = %v, want both goldens pending", err)
	}
	for _, g := range report.Goldens {
		if g.State != "pending" || !strings.Contains(g.Error, "still pending") {
			t.Errorf("golden = %+v, want pending with a timeout error", g)
		}
	}
}

func TestPreflightSubsetsAndValidation(t *testing.T) {
	api := newPreflightFake()
	report, err, _ := runPreflight(t, api, func(o *PreflightOptions) { o.Images = []string{pfImgA}; o.Domains = []string{"d0"} })
	if err != nil || !report.Complete {
		t.Fatalf("subset run: err=%v complete=%v", err, report.Complete)
	}
	if len(report.Goldens) != 1 || len(report.Domains) != 1 || len(report.Domains[0].Images) != 1 {
		t.Errorf("subset report = %+v", report)
	}
	for _, bad := range []func(*PreflightOptions){
		func(o *PreflightOptions) { o.Images = []string{"registry.example/x@sha256:" + strings.Repeat("c", 64)} },
		func(o *PreflightOptions) { o.Domains = []string{"d9"} },
		func(o *PreflightOptions) { o.RunID = "Not Valid" },
		func(o *PreflightOptions) { o.Plan = nil },
		func(o *PreflightOptions) { o.Backend = nil },
	} {
		opts := PreflightOptions{Plan: twoDomainPlan(), Backend: newFakeBackend(t, newPreflightFake())}
		bad(&opts)
		if _, err := NewPreflightWarmer(opts); err == nil {
			t.Error("NewPreflightWarmer accepted an invalid option")
		}
	}
}
