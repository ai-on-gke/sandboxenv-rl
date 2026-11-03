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
	"math"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/agent-substrate/env/internal/benchmarking/schema"
)

const (
	imgA = "registry.example/swebench/a@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	imgB = "registry.example/swebench/b@sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	imgC = "registry.example/swebench/c@sha256:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"
	imgD = "registry.example/swebench/d@sha256:dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd"
)

func twoDomains(budget int64) []schema.PlacementDomain {
	return []schema.PlacementDomain{
		{Name: "d0", Selector: map[string]string{"ate-env/domain": "d0"}, Nodes: 4, NodeCacheBytes: budget},
		{Name: "d1", Selector: map[string]string{"ate-env/domain": "d1"}, Nodes: 4, NodeCacheBytes: budget},
	}
}

func domainsOf(p *schema.ImagePlacement, image string) []string {
	if e := p.Entry(image); e != nil {
		return e.Domains
	}
	return nil
}

func TestPlanImagePlacementPacksLargestFirst(t *testing.T) {
	plan, err := PlanImagePlacement(PlanOptions{
		Instances: []Instance{
			{InstanceID: "c", Image: imgC, ImageBytesCompressed: 1},
			{InstanceID: "a", Image: imgA, ImageBytesCompressed: 3},
			{InstanceID: "b", Image: imgB, ImageBytesCompressed: 2},
		},
		Domains:      twoDomains(4),
		UnpackFactor: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	// a (3) takes d0, b (2) takes d1 (the emptier), c (1) takes d1 (2 free vs 1).
	want := map[string][]string{imgA: {"d0"}, imgB: {"d1"}, imgC: {"d1"}}
	for img, doms := range want {
		if got := domainsOf(plan, img); !reflect.DeepEqual(got, doms) {
			t.Errorf("%s placed in %v, want %v", img, got, doms)
		}
	}
	if d := plan.Domain("d0"); d.AssignedBytes != 3 || d.ImageCount != 1 {
		t.Errorf("d0 = %+v, want 3 bytes, 1 image", *d)
	}
	if d := plan.Domain("d1"); d.AssignedBytes != 3 || d.ImageCount != 2 {
		t.Errorf("d1 = %+v, want 3 bytes, 2 images", *d)
	}
	if len(plan.Unplaced) != 0 {
		t.Errorf("unplaced = %v, want none", plan.Unplaced)
	}
	// Images come out sorted by reference, whatever the packing order.
	for i := 1; i < len(plan.Images); i++ {
		if plan.Images[i-1].Image > plan.Images[i].Image {
			t.Fatalf("images not sorted: %v", plan.Images)
		}
	}
	again, err := PlanImagePlacement(PlanOptions{
		Instances:    []Instance{{Image: imgB, ImageBytesCompressed: 2}, {Image: imgC, ImageBytesCompressed: 1}, {Image: imgA, ImageBytesCompressed: 3}},
		Domains:      twoDomains(4),
		UnpackFactor: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(again, plan) {
		t.Error("the plan depends on the manifest's order")
	}
}

func TestPlanImagePlacementReplicationAndUnpackFactor(t *testing.T) {
	plan, err := PlanImagePlacement(PlanOptions{
		Instances:   []Instance{{Image: imgA, ImageBytesCompressed: 10}, {Image: imgB, ImageBytesCompressed: 10}},
		Domains:     twoDomains(100),
		Replication: 2,
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, img := range []string{imgA, imgB} {
		if got := domainsOf(plan, img); !reflect.DeepEqual(got, []string{"d0", "d1"}) {
			t.Errorf("%s placed in %v, want both domains", img, got)
		}
	}
	if e := plan.Entry(imgA); e.BytesUnpacked != 25 || plan.UnpackFactor != DefaultUnpackFactor {
		t.Errorf("unpacked = %d with factor %v, want 25 with %v", e.BytesUnpacked, plan.UnpackFactor, DefaultUnpackFactor)
	}
	// Replication beyond the domain count is capped, not an error.
	plan, err = PlanImagePlacement(PlanOptions{Instances: []Instance{{Image: imgA, ImageBytesCompressed: 1}}, Domains: twoDomains(0), Replication: 5})
	if err != nil {
		t.Fatal(err)
	}
	if got := domainsOf(plan, imgA); len(got) != 2 {
		t.Errorf("replication 5 over 2 domains placed in %v", got)
	}
}

func TestPlanImagePlacementUnplacedAndAssumedSizes(t *testing.T) {
	plan, err := PlanImagePlacement(PlanOptions{
		Instances: []Instance{
			{Image: imgA, ImageBytesCompressed: 50}, // fits nowhere at factor 1 with budget 40
			{Image: imgB},                           // no size: the default stands in
			{Image: imgC, ImageBytesCompressed: 30},
		},
		Domains:                twoDomains(40),
		UnpackFactor:           1,
		DefaultBytesCompressed: 5,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Unplaced) != 1 || plan.Unplaced[0].Image != imgA || !strings.Contains(plan.Unplaced[0].Reason, "needs 50") {
		t.Errorf("unplaced = %+v, want a (needs 50)", plan.Unplaced)
	}
	if e := plan.Entry(imgB); e == nil || !e.SizeAssumed || e.BytesCompressed != 5 || len(e.Domains) != 1 {
		t.Errorf("b = %+v, want placed with the assumed size 5", e)
	}
	if e := plan.Entry(imgC); e == nil || e.SizeAssumed {
		t.Errorf("c = %+v, want placed with its own size", e)
	}

	// Without a default, an image without a size is unplaced and says so.
	plan, err = PlanImagePlacement(PlanOptions{Instances: []Instance{{Image: imgB}}, Domains: twoDomains(0), UnpackFactor: 1})
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Unplaced) != 1 || !strings.Contains(plan.Unplaced[0].Reason, "no image_bytes_compressed") {
		t.Errorf("unplaced = %+v, want b for lack of a size", plan.Unplaced)
	}

	// Replication 2 on a budget that holds one image per domain: each image
	// gets one domain (coverage before replicas), none is unplaced.
	plan, err = PlanImagePlacement(PlanOptions{
		Instances:    []Instance{{Image: imgA, ImageBytesCompressed: 30}, {Image: imgB, ImageBytesCompressed: 30}},
		Domains:      twoDomains(40),
		UnpackFactor: 1,
		Replication:  2,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Unplaced) != 0 || len(domainsOf(plan, imgA)) != 1 || len(domainsOf(plan, imgB)) != 1 {
		t.Errorf("plan = %+v, want a and b in one domain each", plan.Images)
	}
	if under := UnderReplicated(plan, 0, 0); len(under) != 2 {
		t.Errorf("under-replicated = %v, want both", under)
	}
}

func TestPlanImagePlacementDemandFromSchedule(t *testing.T) {
	sched := &schema.Schedule{
		SchemaVersion: schema.SchemaVersion,
		ScenarioName:  "s",
		Jobs: []schema.ScheduledJob{{
			Job: "j",
			Steps: []schema.ScheduledStep{
				{Step: 1, Tasks: []schema.ScheduledTask{{InstanceID: "a", Image: imgA}, {InstanceID: "a", Image: imgA, Rollout: 1}, {InstanceID: "b", Image: imgB}}},
				{Step: 2, Tasks: []schema.ScheduledTask{{InstanceID: "a", Image: imgA}, {InstanceID: "a", Image: imgA, Rollout: 1}, {InstanceID: "a", Image: imgA, Rollout: 2}}},
			},
		}},
	}
	plan, err := PlanImagePlacement(PlanOptions{
		// d is in the manifest but not in the schedule: not placed.
		Instances:      []Instance{{Image: imgA, ImageBytesCompressed: 1}, {Image: imgB, ImageBytesCompressed: 1}, {Image: imgD, ImageBytesCompressed: 1}},
		Schedule:       sched,
		Domains:        twoDomains(0),
		UnpackFactor:   1,
		HotDemand:      3,
		HotReplication: 2,
	})
	if err != nil {
		t.Fatal(err)
	}
	if e := plan.Entry(imgA); e == nil || e.Demand != 3 || len(e.Domains) != 2 {
		t.Errorf("a = %+v, want demand 3 and two domains (hot)", e)
	}
	if e := plan.Entry(imgB); e == nil || e.Demand != 1 || len(e.Domains) != 1 {
		t.Errorf("b = %+v, want demand 1 and one domain", e)
	}
	if plan.Entry(imgD) != nil {
		t.Error("d is not in the schedule but was placed")
	}
}

func TestReadPlacementDomainsAndWriteReadPlan(t *testing.T) {
	dir := t.TempDir()
	write := func(name, body string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		return p
	}
	good := write("domains.json", `{"domains":[{"name":"d0","selector":{"ate-env/domain":"d0"},"nodes":4,"node_cache_bytes":100}]}`)
	domains, err := ReadPlacementDomains(good)
	if err != nil || len(domains) != 1 || domains[0].Nodes != 4 {
		t.Fatalf("ReadPlacementDomains = %v, %v", domains, err)
	}
	for name, body := range map[string]string{
		"filled.json": `{"domains":[{"name":"d0","selector":{"a":"b"},"nodes":1,"node_cache_bytes":0,"assigned_bytes":5}]}`,
		"dup.json":    `{"domains":[{"name":"d0","selector":{"a":"b"},"nodes":1},{"name":"d0","selector":{"a":"c"},"nodes":1}]}`,
		"nosel.json":  `{"domains":[{"name":"d0","nodes":1}]}`,
		"empty.json":  `{"domains":[]}`,
		"extra.json":  `{"domains":[{"name":"d0","selector":{"a":"b"},"nodes":1}],"other":1}`,
	} {
		if _, err := ReadPlacementDomains(write(name, body)); err == nil {
			t.Errorf("%s accepted, want an error", name)
		}
	}

	plan, err := PlanImagePlacement(PlanOptions{Instances: []Instance{{Image: imgA, ImageBytesCompressed: 7}}, Domains: domains})
	if err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(dir, "sub", "image-placement.json")
	if err := WriteImagePlacement(out, plan); err != nil {
		t.Fatal(err)
	}
	back, err := ReadImagePlacement(out)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(back, plan) {
		t.Errorf("round trip changed the plan:\n%+v\n%+v", back, plan)
	}
}

func TestParseByteSize(t *testing.T) {
	cases := map[string]int64{
		"4096": 4096, "4096B": 4096, "1K": 1024, "1KiB": 1024, "1KB": 1000, "3G": 3 << 30, "3GiB": 3 << 30,
		"3GB": 3_000_000_000, "1.5TB": 1_500_000_000_000, "0.5MiB": 512 * 1024, " 2 g ": 2 << 30,
	}
	for in, want := range cases {
		got, err := ParseByteSize(in)
		if err != nil || got != want {
			t.Errorf("ParseByteSize(%q) = %d, %v; want %d", in, got, err, want)
		}
	}
	for _, bad := range []string{"", "GiB", "1X", "-1G", "1.2.3G"} {
		if _, err := ParseByteSize(bad); err == nil {
			t.Errorf("ParseByteSize(%q) accepted", bad)
		}
	}
}

func TestPlanImagePlacementRejectsNonFiniteFactorsAndOverflow(t *testing.T) {
	for _, factor := range []float64{math.NaN(), math.Inf(1), -1} {
		if _, err := PlanImagePlacement(PlanOptions{Instances: []Instance{{Image: imgA, ImageBytesCompressed: 1}}, Domains: twoDomains(0), UnpackFactor: factor}); err == nil {
			t.Errorf("factor %v accepted", factor)
		}
	}
	if _, err := PlanImagePlacement(PlanOptions{Instances: []Instance{{Image: imgA, ImageBytesCompressed: math.MaxInt64}}, Domains: twoDomains(0)}); err == nil || !strings.Contains(err.Error(), "overflows") {
		t.Errorf("an overflowing unpacked size was accepted: %v", err)
	}
	for _, bad := range []string{"1e20GiB", "nan", "inf", "9223372036854775807KiB"} {
		if _, err := ParseByteSize(bad); err == nil {
			t.Errorf("ParseByteSize(%q) accepted", bad)
		}
	}
}
