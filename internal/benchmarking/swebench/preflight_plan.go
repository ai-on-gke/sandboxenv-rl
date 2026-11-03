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
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"

	"github.com/agent-substrate/env/internal/benchmarking/schema"
)

// DefaultUnpackFactor is the ratio of an image's unpacked size on a node to
// its compressed size when the planner is given none: 2 to 2.5 observed on
// SWE-bench images (benchmarking/swebench/SKILL.md).
const DefaultUnpackFactor = 2.5

// PlacementDomainsFile is domains.json, the placement domains an operator
// describes once per cluster shape: for each, the labels its worker pools
// carry, its node count and the unpacked bytes one node may hold for a plan.
type PlacementDomainsFile struct {
	Domains []schema.PlacementDomain `json:"domains"`
}

// ReadPlacementDomains reads domains.json. The planner fills assigned_bytes
// and image_count, so a file that sets them is refused.
func ReadPlacementDomains(path string) ([]schema.PlacementDomain, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	file, err := schema.DecodeJSONStrict[PlacementDomainsFile](f)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if len(file.Domains) == 0 {
		return nil, fmt.Errorf("%s: at least one domain is required", path)
	}
	names := make(map[string]bool, len(file.Domains))
	for i, d := range file.Domains {
		if err := d.Validate(); err != nil {
			return nil, fmt.Errorf("%s: domains[%d]: %w", path, i, err)
		}
		if d.AssignedBytes != 0 || d.ImageCount != 0 {
			return nil, fmt.Errorf("%s: domains[%d] (%s): assigned_bytes and image_count are the planner's to fill", path, i, d.Name)
		}
		if names[d.Name] {
			return nil, fmt.Errorf("%s: domains[%d]: duplicate domain %q", path, i, d.Name)
		}
		names[d.Name] = true
	}
	return file.Domains, nil
}

// PlanOptions are the inputs of PlanImagePlacement.
type PlanOptions struct {
	// Instances is the manifest; it gives each image's compressed size.
	Instances []Instance
	// Schedule, when set, names the images to place (its unique images) and
	// gives each image's demand, the most environments one step asks for.
	// Without it every image of Instances is placed with demand 0.
	Schedule *schema.Schedule
	// Domains are the placement domains, with assigned_bytes and image_count
	// zero.
	Domains []schema.PlacementDomain
	// Replication is how many domains each image goes to; 0 means 1.
	Replication int
	// HotDemand, when > 0, makes an image whose demand reaches it go to
	// HotReplication domains instead.
	HotDemand      int
	HotReplication int
	// UnpackFactor converts compressed bytes to the unpacked bytes a node
	// holds; 0 means DefaultUnpackFactor.
	UnpackFactor float64
	// DefaultBytesCompressed stands in for an image the manifest gives no
	// size for, marked size_assumed; 0 leaves such an image unplaced.
	DefaultBytesCompressed int64
}

// PlanImagePlacement assigns each image to domains: largest image first, each
// to the domains with the most free budget per node that can still take it,
// so the images of one domain fit each of its nodes' image cache. An image
// that fits nowhere is listed as unplaced; one that fits fewer domains than
// its replication is placed where it fits. Deterministic for equal inputs.
func PlanImagePlacement(opts PlanOptions) (*schema.ImagePlacement, error) {
	if len(opts.Domains) == 0 {
		return nil, errors.New("plan: at least one domain is required")
	}
	factor := opts.UnpackFactor
	if factor == 0 {
		factor = DefaultUnpackFactor
	}
	if factor <= 0 {
		return nil, fmt.Errorf("plan: unpack factor must be > 0, got %v", factor)
	}
	replication := opts.Replication
	if replication == 0 {
		replication = 1
	}
	if replication < 1 {
		return nil, fmt.Errorf("plan: replication must be >= 1, got %d", replication)
	}
	if opts.HotDemand > 0 && opts.HotReplication < 1 {
		return nil, fmt.Errorf("plan: hot replication must be >= 1 when a hot demand is set, got %d", opts.HotReplication)
	}
	domains := make([]schema.PlacementDomain, len(opts.Domains))
	names := make(map[string]bool, len(opts.Domains))
	for i, d := range opts.Domains {
		if err := d.Validate(); err != nil {
			return nil, fmt.Errorf("plan: domains[%d]: %w", i, err)
		}
		if names[d.Name] {
			return nil, fmt.Errorf("plan: duplicate domain %q", d.Name)
		}
		names[d.Name] = true
		domains[i] = d
		domains[i].AssignedBytes = 0
		domains[i].ImageCount = 0
	}

	sizes := make(map[string]int64, len(opts.Instances))
	for _, inst := range opts.Instances {
		if inst.ImageBytesCompressed > 0 && sizes[inst.Image] == 0 {
			sizes[inst.Image] = inst.ImageBytesCompressed
		} else if _, ok := sizes[inst.Image]; !ok {
			sizes[inst.Image] = 0
		}
	}
	var images []string
	demand := map[string]int{}
	if opts.Schedule != nil {
		images = UniqueScheduleImages(opts.Schedule)
		demand = scheduleImageDemand(opts.Schedule)
	} else {
		for img := range sizes {
			images = append(images, img)
		}
		sort.Strings(images)
	}

	type candidate struct {
		image    string
		entry    schema.ImagePlacementEntry
		unpacked int64
		want     int
	}
	var work []candidate
	var unplaced []schema.UnplacedImage
	for _, img := range images {
		bc, assumed := sizes[img], false
		if bc == 0 {
			if opts.DefaultBytesCompressed <= 0 {
				unplaced = append(unplaced, schema.UnplacedImage{Image: img, Reason: "no image_bytes_compressed in the manifest and no default size"})
				continue
			}
			bc, assumed = opts.DefaultBytesCompressed, true
		}
		unpacked := int64(math.Ceil(float64(bc) * factor))
		want := replication
		if opts.HotDemand > 0 && demand[img] >= opts.HotDemand {
			want = opts.HotReplication
		}
		if want > len(domains) {
			want = len(domains)
		}
		work = append(work, candidate{
			image:    img,
			unpacked: unpacked,
			want:     want,
			entry: schema.ImagePlacementEntry{
				Image:           img,
				BytesCompressed: bc,
				BytesUnpacked:   unpacked,
				SizeAssumed:     assumed,
				Demand:          demand[img],
			},
		})
	}
	// Largest first, so the big images get the emptiest domains and the
	// small ones fill the gaps; by name among equals, for determinism.
	sort.Slice(work, func(i, j int) bool {
		if work[i].unpacked != work[j].unpacked {
			return work[i].unpacked > work[j].unpacked
		}
		return work[i].image < work[j].image
	})

	free := func(d *schema.PlacementDomain) int64 {
		if d.NodeCacheBytes == 0 {
			return math.MaxInt64
		}
		return d.NodeCacheBytes - d.AssignedBytes
	}
	// placeInto gives c up to count more domains, the emptiest first, never
	// one it already has; it returns the names it took.
	placeInto := func(c *candidate, count int) []string {
		order := make([]*schema.PlacementDomain, 0, len(domains))
		for i := range domains {
			order = append(order, &domains[i])
		}
		sort.SliceStable(order, func(i, j int) bool {
			fi, fj := free(order[i]), free(order[j])
			if fi != fj {
				return fi > fj
			}
			if order[i].ImageCount != order[j].ImageCount {
				return order[i].ImageCount < order[j].ImageCount
			}
			return order[i].Name < order[j].Name
		})
		var chosen []string
		for _, d := range order {
			if len(chosen) == count {
				break
			}
			if slices.Contains(c.entry.Domains, d.Name) || free(d) < c.unpacked {
				continue
			}
			d.AssignedBytes += c.unpacked
			d.ImageCount++
			chosen = append(chosen, d.Name)
		}
		c.entry.Domains = append(c.entry.Domains, chosen...)
		return chosen
	}
	// Two passes: every image gets one domain before any image gets a
	// second, so a tight budget costs replication, not coverage.
	var entries []schema.ImagePlacementEntry
	placed := make([]*candidate, 0, len(work))
	for i := range work {
		c := &work[i]
		if len(placeInto(c, 1)) == 0 {
			var most int64
			for j := range domains {
				if f := free(&domains[j]); f > most && f != math.MaxInt64 {
					most = f
				}
			}
			unplaced = append(unplaced, schema.UnplacedImage{
				Image:  c.image,
				Reason: fmt.Sprintf("needs %d unpacked bytes on a node, the emptiest domain has %d free", c.unpacked, most),
			})
			continue
		}
		placed = append(placed, c)
	}
	// Replicas: the hot images first, then the largest.
	sort.SliceStable(placed, func(i, j int) bool { return placed[i].want > placed[j].want })
	for _, c := range placed {
		if c.want > 1 {
			placeInto(c, c.want-1)
		}
		sort.Strings(c.entry.Domains)
		entries = append(entries, c.entry)
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Image < entries[j].Image })
	sort.Slice(unplaced, func(i, j int) bool { return unplaced[i].Image < unplaced[j].Image })

	plan := &schema.ImagePlacement{
		SchemaVersion: schema.SchemaVersion,
		UnpackFactor:  factor,
		Replication:   replication,
		Domains:       domains,
		Images:        entries,
		Unplaced:      unplaced,
	}
	if plan.Images == nil {
		plan.Images = []schema.ImagePlacementEntry{}
	}
	if err := plan.Validate(); err != nil {
		return nil, fmt.Errorf("plan: %w", err)
	}
	return plan, nil
}

// scheduleImageDemand is, per image, the most tasks any one step of any job
// asks for.
func scheduleImageDemand(s *schema.Schedule) map[string]int {
	demand := map[string]int{}
	for _, job := range s.Jobs {
		for _, step := range job.Steps {
			perStep := map[string]int{}
			for _, t := range step.Tasks {
				perStep[t.Image]++
			}
			for img, n := range perStep {
				if n > demand[img] {
					demand[img] = n
				}
			}
		}
	}
	return demand
}

// UnderReplicated returns the images the plan placed in fewer domains than
// their replication asked for.
func UnderReplicated(p *schema.ImagePlacement, hotDemand, hotReplication int) []string {
	var out []string
	for _, e := range p.Images {
		want := p.Replication
		if hotDemand > 0 && e.Demand >= hotDemand {
			want = hotReplication
		}
		if want > len(p.Domains) {
			want = len(p.Domains)
		}
		if len(e.Domains) < want {
			out = append(out, e.Image)
		}
	}
	return out
}

// WriteImagePlacement validates p and writes it as indented JSON.
func WriteImagePlacement(path string, p *schema.ImagePlacement) error {
	if err := p.Validate(); err != nil {
		return err
	}
	return writeJSONFile(path, p)
}

// writeJSONFile writes v as indented JSON with a trailing newline, creating
// the directory.
func writeJSONFile(path string, v any) error {
	if dir := filepath.Dir(path); dir != "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	}
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(data, '\n'), 0o644)
}

// ReadImagePlacement reads and validates an image-placement.json.
func ReadImagePlacement(path string) (*schema.ImagePlacement, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	p, err := schema.DecodeJSONStrict[schema.ImagePlacement](f)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if err := p.Validate(); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return &p, nil
}

// ParseByteSize parses "500GiB", "3G", "1.5TB" or "4096". K, M, G, T and the
// IEC KiB, MiB, GiB, TiB are powers of 1024; KB, MB, GB, TB are powers of
// 1000.
func ParseByteSize(s string) (int64, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, errors.New("empty size")
	}
	i := len(s)
	for i > 0 && (s[i-1] < '0' || s[i-1] > '9') && s[i-1] != '.' {
		i--
	}
	num, unit := s[:i], strings.ToLower(strings.TrimSpace(s[i:]))
	value, err := strconv.ParseFloat(num, 64)
	if err != nil || value < 0 {
		return 0, fmt.Errorf("size %q: want a number with an optional unit (K, M, G, T, KiB, MiB, GiB, TiB, KB, MB, GB, TB)", s)
	}
	var mult float64
	switch unit {
	case "", "b":
		mult = 1
	case "k", "kib":
		mult = 1 << 10
	case "m", "mib":
		mult = 1 << 20
	case "g", "gib":
		mult = 1 << 30
	case "t", "tib":
		mult = 1 << 40
	case "kb":
		mult = 1e3
	case "mb":
		mult = 1e6
	case "gb":
		mult = 1e9
	case "tb":
		mult = 1e12
	default:
		return 0, fmt.Errorf("size %q: unknown unit %q", s, unit)
	}
	return int64(math.Round(value * mult)), nil
}

// FormatByteSize prints n with the largest IEC unit that keeps one decimal,
// for the planner's summary.
func FormatByteSize(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit && exp < 3; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGT"[exp])
}
