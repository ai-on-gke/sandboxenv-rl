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
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"regexp"
	"strings"
	"sync"
	"time"
)

// DefaultKind is assumed when a dataset manifest row omits "kind", so existing
// SWE-bench manifests load without modification.
const DefaultKind = "swebench"

// Grader parses the stdout/stderr of a trajectory's reward step into a 0/1
// reward and a human-readable detail/verdict string (for example
// "f2p 1/1 p2p 3/4" or "invalid run"). When Parse returns a non-nil error or
// a nil reward, the runner falls back to grading by process exit code.
type Grader interface {
	Parse(stdout []byte) (reward *int, detail string, err error)
}

// Adapter plugs a dataset kind (such as "swebench") into the dataset-agnostic
// rlbench core.
type Adapter interface {
	// Kind returns the dataset identifier matched against Instance.Kind.
	Kind() string
	// Validate checks any dataset-specific invariants on in.Material.
	Validate(in Instance) error
	// RequireGrading checks that in carries the material needed by a grading
	// trajectory.
	RequireGrading(in Instance) error
	// TemplateData decodes in into the value passed to text/template when
	// rendering a trajectory.
	TemplateData(in Instance) (any, error)
	// Grades reports whether t references grading material specific to this
	// dataset kind.
	Grades(t *Trajectory) bool
	// Grader returns the output parser for reward steps of this dataset kind,
	// or nil when exit code alone decides the reward.
	Grader() Grader
	// Trajectories returns an optional embedded filesystem of built-in
	// trajectories (<name>.yaml and companion assets) contributed by this
	// adapter.
	Trajectories() fs.FS
	// DefaultTrajectory names the default built-in trajectory for this dataset
	// kind, or "".
	DefaultTrajectory() string
}

var (
	adaptersMu sync.RWMutex
	adapters   = map[string]Adapter{}
)

// RegisterAdapter registers a dataset adapter by its Kind().
func RegisterAdapter(a Adapter) {
	adaptersMu.Lock()
	defer adaptersMu.Unlock()
	adapters[a.Kind()] = a
}

func unregisterAdapter(kind string) {
	adaptersMu.Lock()
	defer adaptersMu.Unlock()
	delete(adapters, kind)
}

// AdapterFor returns the registered adapter for kind (defaulting an empty kind
// to DefaultKind), or nil if none is registered.
func AdapterFor(kind string) Adapter {
	if kind == "" {
		kind = DefaultKind
	}
	adaptersMu.RLock()
	defer adaptersMu.RUnlock()
	return adapters[kind]
}

// Instance is one benchmark task in a dataset manifest: the unique instance
// id, the digest-pinned sandbox image, the dataset kind (defaulting to
// DefaultKind), and the opaque dataset-specific JSON payload consumed by
// trajectory templates and dataset adapters.
type Instance struct {
	InstanceID string `json:"instance_id"`
	// Image is the task image, pinned by digest (repo@sha256:...). Substrate
	// requires pinned images because changing the image invalidates snapshots.
	Image    string          `json:"image"`
	Kind     string          `json:"kind,omitempty"`
	Material json.RawMessage `json:"material,omitempty"`
}

// UnmarshalJSON decodes a manifest row. Rows may either nest dataset-specific
// fields under "material" or place them at the top level alongside
// "instance_id" and "image"; when "kind" is omitted it defaults to DefaultKind.
func (in *Instance) UnmarshalJSON(b []byte) error {
	var wire struct {
		InstanceID string          `json:"instance_id"`
		Image      string          `json:"image"`
		Kind       string          `json:"kind"`
		Material   json.RawMessage `json:"material"`
	}
	if err := json.Unmarshal(b, &wire); err != nil {
		return err
	}
	if wire.Kind == "" {
		wire.Kind = DefaultKind
	}
	in.InstanceID = wire.InstanceID
	in.Image = wire.Image
	in.Kind = wire.Kind
	if len(wire.Material) > 0 && string(wire.Material) != "null" {
		in.Material = append(json.RawMessage(nil), wire.Material...)
	} else {
		in.Material = append(json.RawMessage(nil), b...)
	}
	return nil
}

// MarshalJSON encodes the instance back to a flat JSON object merging Material
// with InstanceID, Image, and Kind so "{{json .}}" in templates produces the
// full instance record.
func (in Instance) MarshalJSON() ([]byte, error) {
	m := map[string]any{}
	if len(in.Material) > 0 {
		if err := json.Unmarshal(in.Material, &m); err != nil {
			return nil, err
		}
	}
	m["instance_id"] = in.InstanceID
	m["image"] = in.Image
	if in.Kind != "" && in.Kind != DefaultKind {
		m["kind"] = in.Kind
	}
	return json.Marshal(m)
}

var digestPinned = regexp.MustCompile(`@sha256:[0-9a-f]{64}$`)

// Validate reports the first thing that would make the instance unusable.
func (in Instance) Validate() error {
	switch {
	case in.InstanceID == "":
		return errors.New("instance_id is required")
	case in.Image == "":
		return fmt.Errorf("%s: image is required", in.InstanceID)
	case !digestPinned.MatchString(in.Image):
		return fmt.Errorf("%s: image %q is not digest-pinned", in.InstanceID, in.Image)
	}
	if a := AdapterFor(in.Kind); a != nil {
		return a.Validate(in)
	}
	return nil
}

// RequireGrading checks that every instance carries what a grading trajectory
// needs via its dataset adapter, so a missing field is a configuration error
// before the run rather than a reward of zero after it.
func RequireGrading(instances []Instance) error {
	for _, in := range instances {
		a := AdapterFor(in.Kind)
		if a == nil {
			return fmt.Errorf("%s: no dataset adapter registered for kind %q", in.InstanceID, in.Kind)
		}
		if err := a.RequireGrading(in); err != nil {
			return err
		}
	}
	return nil
}

// datasetFetchTimeout bounds a manifest download; a stalled server must not
// hang the loader before it starts.
const datasetFetchTimeout = 2 * time.Minute

// LoadDataset reads a JSONL manifest from a local path or an https URL. The
// manifest carries scripts that run inside the sandboxes, so a remote source
// must be https.
func LoadDataset(source string) ([]Instance, error) {
	var r io.ReadCloser
	if strings.HasPrefix(source, "http://") {
		return nil, fmt.Errorf("dataset %s: remote manifests must use https", source)
	}
	if strings.HasPrefix(source, "https://") {
		client := &http.Client{
			Timeout: datasetFetchTimeout,
			// The https rule holds for each hop: a redirect to http would
			// fetch the scripts in clear text.
			CheckRedirect: func(req *http.Request, via []*http.Request) error {
				if req.URL.Scheme != "https" {
					return fmt.Errorf("redirect to %s: remote manifests must use https", req.URL.Redacted())
				}
				if len(via) >= 10 {
					return errors.New("stopped after 10 redirects")
				}
				return nil
			},
		}
		resp, err := client.Get(source)
		if err != nil {
			return nil, fmt.Errorf("fetch dataset: %w", err)
		}
		if resp.StatusCode != http.StatusOK {
			resp.Body.Close()
			return nil, fmt.Errorf("fetch dataset: HTTP %d", resp.StatusCode)
		}
		r = resp.Body
	} else {
		f, err := os.Open(source)
		if err != nil {
			return nil, fmt.Errorf("open dataset: %w", err)
		}
		r = f
	}
	defer r.Close()
	return ParseDataset(r)
}

// ParseDataset decodes a JSONL manifest. Blank lines are skipped; every
// record must validate and instance ids must be unique.
func ParseDataset(r io.Reader) ([]Instance, error) {
	sc := bufio.NewScanner(r)
	// Eval scripts and patches run to hundreds of KiB for some repos.
	sc.Buffer(make([]byte, 0, 1<<20), 64<<20)
	var out []Instance
	seen := map[string]bool{}
	line := 0
	for sc.Scan() {
		line++
		b := bytes.TrimSpace(sc.Bytes())
		if len(b) == 0 {
			continue
		}
		var in Instance
		if err := json.Unmarshal(b, &in); err != nil {
			return nil, fmt.Errorf("dataset line %d: %w", line, err)
		}
		if err := in.Validate(); err != nil {
			return nil, fmt.Errorf("dataset line %d: %w", line, err)
		}
		if seen[in.InstanceID] {
			return nil, fmt.Errorf("dataset line %d: duplicate instance %s", line, in.InstanceID)
		}
		seen[in.InstanceID] = true
		out = append(out, in)
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("read dataset: %w", err)
	}
	if len(out) == 0 {
		return nil, errors.New("dataset is empty")
	}
	return out, nil
}

// Select narrows a dataset: exclude drops the named instances first (a
// known-bad list; unknown names are ignored), ids then keeps only the named
// instances (in manifest order) and errors on an unknown or excluded id, and
// limit > 0 keeps the first limit of what is left.
func Select(all []Instance, ids, exclude []string, limit int) ([]Instance, error) {
	out := all
	drop := map[string]bool{}
	if len(exclude) > 0 {
		for _, id := range exclude {
			drop[id] = true
		}
		out = nil
		for _, in := range all {
			if !drop[in.InstanceID] {
				out = append(out, in)
			}
		}
		all = out
	}
	if len(ids) > 0 {
		for _, id := range ids {
			if drop[id] {
				return nil, fmt.Errorf("instance %s is on the exclude list", id)
			}
		}
		want := map[string]bool{}
		for _, id := range ids {
			want[id] = true
		}
		out = nil
		for _, in := range all {
			if want[in.InstanceID] {
				out = append(out, in)
				delete(want, in.InstanceID)
			}
		}
		if len(want) > 0 {
			missing := make([]string, 0, len(want))
			for id := range want {
				missing = append(missing, id)
			}
			return nil, fmt.Errorf("instances not in dataset: %s", strings.Join(missing, ", "))
		}
	}
	if limit > 0 && limit < len(out) {
		out = out[:limit]
	}
	return out, nil
}

// ReadIDList reads instance ids from a file, one per line. Blank lines and
// lines starting with '#' are skipped, and anything after the first
// whitespace on a line is a comment, so a list can say why each id is there.
func ReadIDList(path string) ([]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var ids []string
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		ids = append(ids, strings.Fields(line)[0])
	}
	return ids, sc.Err()
}
