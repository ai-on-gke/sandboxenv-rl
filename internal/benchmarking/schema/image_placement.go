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

package schema

import (
	"errors"
	"fmt"
)

// ImagePlacement is the image-placement.json artifact that
// `swebench-loader plan-placement` writes and the pre-flight and the run read:
// which placement domains hold each task image. A domain is a WorkerPool, or
// a set of pools sharing a label, that a per-environment worker selector
// addresses; every node of a domain holds every image assigned to it, so the
// unpacked size of a domain's images must fit each node's image-cache budget.
type ImagePlacement struct {
	SchemaVersion string `json:"schema_version"`
	// UnpackFactor is the ratio of an image's unpacked size on a node to its
	// compressed size, applied to bytes_compressed to budget the caches.
	UnpackFactor float64 `json:"unpack_factor"`
	// Replication is how many domains an image goes to unless it is hot.
	Replication int `json:"replication"`
	// Domains, in the order the planner was given them.
	Domains []PlacementDomain `json:"domains"`
	// Images, sorted by image reference, each with the domains that hold it.
	Images []ImagePlacementEntry `json:"images"`
	// Unplaced lists the images no domain could take, with the reason; a run
	// over them pays cold first boots.
	Unplaced []UnplacedImage `json:"unplaced,omitempty"`
}

// PlacementDomain is one placement domain: the labels its worker pools carry,
// its node count and each node's usable image-cache budget.
type PlacementDomain struct {
	Name string `json:"name"`
	// Selector is the per-environment worker selector that keeps an
	// environment inside the domain; Substrate ANDs it with the template's.
	Selector map[string]string `json:"selector"`
	Nodes    int               `json:"nodes"`
	// NodeCacheBytes is the unpacked bytes one node of the domain may hold for
	// this plan: the image-cache volume times atelet's low watermark, minus
	// headroom. 0 means no budget check.
	NodeCacheBytes int64 `json:"node_cache_bytes"`
	// AssignedBytes is the unpacked size of the images assigned to the
	// domain, which each of its nodes holds; filled by the planner.
	AssignedBytes int64 `json:"assigned_bytes"`
	// ImageCount is how many images the planner assigned to the domain.
	ImageCount int `json:"image_count"`
}

// ImagePlacementEntry is one image and the domains that hold it.
type ImagePlacementEntry struct {
	Image   string   `json:"image"`
	Domains []string `json:"domains"`
	// BytesCompressed is the manifest's image_bytes_compressed, or the
	// planner's default when the manifest had none (size_assumed).
	BytesCompressed int64 `json:"bytes_compressed"`
	BytesUnpacked   int64 `json:"bytes_unpacked"`
	SizeAssumed     bool  `json:"size_assumed,omitempty"`
	// Demand is the most environments of the image one step asks for, from
	// the schedule; 0 when the planner had no schedule.
	Demand int `json:"demand,omitempty"`
}

// UnplacedImage is an image the planner could not place, and why.
type UnplacedImage struct {
	Image  string `json:"image"`
	Reason string `json:"reason"`
}

// Validate checks ImagePlacement invariants.
func (p ImagePlacement) Validate() error {
	if p.SchemaVersion != SchemaVersion {
		return fmt.Errorf("image_placement: unsupported schema_version %q (want %q)", p.SchemaVersion, SchemaVersion)
	}
	if p.UnpackFactor <= 0 {
		return errors.New("image_placement: unpack_factor must be > 0")
	}
	if p.Replication < 1 {
		return errors.New("image_placement: replication must be >= 1")
	}
	if len(p.Domains) == 0 {
		return errors.New("image_placement: at least one domain is required")
	}
	domains := make(map[string]bool, len(p.Domains))
	for i, d := range p.Domains {
		if err := d.Validate(); err != nil {
			return fmt.Errorf("image_placement domains[%d]: %w", i, err)
		}
		if domains[d.Name] {
			return fmt.Errorf("image_placement domains[%d]: duplicate domain %q", i, d.Name)
		}
		domains[d.Name] = true
	}
	images := make(map[string]bool, len(p.Images))
	for i, e := range p.Images {
		if e.Image == "" {
			return fmt.Errorf("image_placement images[%d]: image is required", i)
		}
		if images[e.Image] {
			return fmt.Errorf("image_placement images[%d]: duplicate image %q", i, e.Image)
		}
		images[e.Image] = true
		if len(e.Domains) == 0 {
			return fmt.Errorf("image_placement images[%d] (%s): at least one domain is required", i, e.Image)
		}
		seen := make(map[string]bool, len(e.Domains))
		for _, name := range e.Domains {
			if !domains[name] {
				return fmt.Errorf("image_placement images[%d] (%s): unknown domain %q", i, e.Image, name)
			}
			if seen[name] {
				return fmt.Errorf("image_placement images[%d] (%s): domain %q listed twice", i, e.Image, name)
			}
			seen[name] = true
		}
		if e.BytesCompressed < 0 || e.BytesUnpacked < 0 || e.Demand < 0 {
			return fmt.Errorf("image_placement images[%d] (%s): sizes and demand must be >= 0", i, e.Image)
		}
	}
	for i, u := range p.Unplaced {
		if u.Image == "" || u.Reason == "" {
			return fmt.Errorf("image_placement unplaced[%d]: image and reason are required", i)
		}
		if images[u.Image] {
			return fmt.Errorf("image_placement unplaced[%d]: %q is also placed", i, u.Image)
		}
	}
	return nil
}

// Validate checks PlacementDomain invariants.
func (d PlacementDomain) Validate() error {
	if d.Name == "" {
		return errors.New("name is required")
	}
	if len(d.Selector) == 0 {
		return fmt.Errorf("domain %q: selector is required", d.Name)
	}
	for k, v := range d.Selector {
		if k == "" || v == "" {
			return fmt.Errorf("domain %q: selector keys and values must be non-empty", d.Name)
		}
	}
	if d.Nodes < 1 {
		return fmt.Errorf("domain %q: nodes must be >= 1", d.Name)
	}
	if d.NodeCacheBytes < 0 || d.AssignedBytes < 0 || d.ImageCount < 0 {
		return fmt.Errorf("domain %q: node_cache_bytes, assigned_bytes and image_count must be >= 0", d.Name)
	}
	return nil
}

// Domain returns the named domain, or nil.
func (p *ImagePlacement) Domain(name string) *PlacementDomain {
	for i := range p.Domains {
		if p.Domains[i].Name == name {
			return &p.Domains[i]
		}
	}
	return nil
}

// Entry returns the placement of image, or nil when the plan does not place it.
func (p *ImagePlacement) Entry(image string) *ImagePlacementEntry {
	for i := range p.Images {
		if p.Images[i].Image == image {
			return &p.Images[i]
		}
	}
	return nil
}
