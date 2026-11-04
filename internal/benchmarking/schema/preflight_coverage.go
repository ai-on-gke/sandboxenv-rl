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
	"time"
)

// PreflightCoverage is preflight-coverage.json: what a pre-flight did for an
// image placement plan and whether every node of every domain now holds the
// images assigned to it. The pre-flight exits non-zero when complete is
// false; the report says which (image, domain) pairs are short and why.
type PreflightCoverage struct {
	SchemaVersion string    `json:"schema_version"`
	RunID         string    `json:"run_id"`
	StartedAt     time.Time `json:"started_at"`
	FinishedAt    time.Time `json:"finished_at"`
	// Plan is the image-placement.json the pre-flight ran; "" when unknown.
	Plan string `json:"plan,omitempty"`
	// Goldens is the golden state the pre-flight left each image in.
	Goldens []GoldenRecord   `json:"goldens"`
	Domains []DomainCoverage `json:"domains"`
	// Complete is true when every golden the pre-flight waited for is ready
	// and every (image, domain) pair reached its node count.
	Complete bool `json:"complete"`
}

// GoldenRecord is one image's derived template and golden snapshot after the
// pre-flight.
type GoldenRecord struct {
	Image    string `json:"image"`
	Template string `json:"template,omitempty"`
	// State is ready, none (goldens are disabled on the base template),
	// failed, or pending when the pre-flight gave up waiting.
	State string `json:"state"`
	Tag   string `json:"tag,omitempty"`
	Error string `json:"error,omitempty"`
	// WaitMS is how long the pre-flight waited for the golden.
	WaitMS float64 `json:"wait_ms"`
}

// DomainCoverage is one domain's result: the nodes its probes landed on and
// each assigned image's coverage.
type DomainCoverage struct {
	Name string `json:"name"`
	// Nodes is the node count the plan gave the domain.
	Nodes int `json:"nodes"`
	// NodesSeen is every node any probe of the domain landed on, sorted.
	NodesSeen []string        `json:"nodes_seen"`
	Images    []ImageCoverage `json:"images"`
	Complete  bool            `json:"complete"`
}

// ImageCoverage is one image's coverage inside one domain.
type ImageCoverage struct {
	Image string `json:"image"`
	// Nodes is every node a probe of the image landed on in the domain, sorted.
	Nodes []string `json:"nodes"`
	// Wanted is the domain's node count; Complete when len(Nodes) >= Wanted.
	Wanted   int  `json:"wanted"`
	Probes   int  `json:"probes"`
	Failed   int  `json:"failed"`
	Rounds   int  `json:"rounds"`
	Complete bool `json:"complete"`
	// Error is the last probe error when the image could not be covered.
	Error string `json:"error,omitempty"`
}

// Validate checks PreflightCoverage invariants.
func (c PreflightCoverage) Validate() error {
	if c.SchemaVersion != SchemaVersion {
		return fmt.Errorf("preflight_coverage: unsupported schema_version %q (want %q)", c.SchemaVersion, SchemaVersion)
	}
	if c.RunID == "" {
		return errors.New("preflight_coverage: run_id is required")
	}
	if c.StartedAt.IsZero() || c.FinishedAt.IsZero() {
		return errors.New("preflight_coverage: started_at and finished_at are required")
	}
	if c.FinishedAt.Before(c.StartedAt) {
		return errors.New("preflight_coverage: finished_at is before started_at")
	}
	for i, g := range c.Goldens {
		if g.Image == "" || g.State == "" {
			return fmt.Errorf("preflight_coverage goldens[%d]: image and state are required", i)
		}
		if g.WaitMS < 0 {
			return fmt.Errorf("preflight_coverage goldens[%d] (%s): wait_ms must be >= 0", i, g.Image)
		}
	}
	names := make(map[string]bool, len(c.Domains))
	for i, d := range c.Domains {
		if d.Name == "" {
			return fmt.Errorf("preflight_coverage domains[%d]: name is required", i)
		}
		if names[d.Name] {
			return fmt.Errorf("preflight_coverage domains[%d]: duplicate domain %q", i, d.Name)
		}
		names[d.Name] = true
		if d.Nodes < 1 {
			return fmt.Errorf("preflight_coverage domains[%d] (%s): nodes must be >= 1", i, d.Name)
		}
		images := make(map[string]bool, len(d.Images))
		for j, img := range d.Images {
			if img.Image == "" {
				return fmt.Errorf("preflight_coverage domains[%d] (%s) images[%d]: image is required", i, d.Name, j)
			}
			if images[img.Image] {
				return fmt.Errorf("preflight_coverage domains[%d] (%s) images[%d]: duplicate image %q", i, d.Name, j, img.Image)
			}
			images[img.Image] = true
			if img.Wanted < 1 || img.Probes < 0 || img.Failed < 0 || img.Rounds < 0 {
				return fmt.Errorf("preflight_coverage domains[%d] (%s) images[%d] (%s): wanted must be >= 1 and counts >= 0", i, d.Name, j, img.Image)
			}
			if img.Complete != (len(img.Nodes) >= img.Wanted) {
				return fmt.Errorf("preflight_coverage domains[%d] (%s) images[%d] (%s): complete disagrees with nodes (%d) and wanted (%d)", i, d.Name, j, img.Image, len(img.Nodes), img.Wanted)
			}
		}
	}
	return nil
}
