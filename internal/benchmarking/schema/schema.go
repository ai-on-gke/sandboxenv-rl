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

// Package schema defines the v1 JSON/YAML data contracts, strict codecs, and
// validators for the RL environment benchmark suite.
package schema

import (
	"bufio"
	"bytes"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"path"
	"strings"
	"sync"
	"time"

	"github.com/google/jsonschema-go/jsonschema"
	"sigs.k8s.io/yaml"
)

// SchemaVersion is the canonical schema version identifier for v1 artifacts.
const SchemaVersion = "v1"

// VersionV1 is an alias for SchemaVersion.
const VersionV1 = SchemaVersion

// CheckVersion verifies that v equals SchemaVersion ("v1").
func CheckVersion(v string) error {
	return checkSchemaVersion(v)
}

const baseSchemaURI = "https://ai-on-gke.github.io/sandboxenv-rl/schema/v1/"

// SchemaFS embeds the canonical JSON Schema v1 files under v1/.
//
//go:embed v1/*.schema.json
var SchemaFS embed.FS

// Validator is implemented by schema structs that enforce domain invariants.
type Validator interface {
	Validate() error
}

// ParkPolicy enumerates how a step-mode working set parks environments between steps.
type ParkPolicy string

const (
	// ParkSuspendAsync suspends with asynchronous durability (on-node snapshot with background upload).
	ParkSuspendAsync ParkPolicy = "suspend-async"
	// ParkSuspendSync suspends with synchronous durability (waits for durable object storage).
	ParkSuspendSync ParkPolicy = "suspend-sync"
	// ParkFork parks via snapshot/fork semantics.
	ParkFork ParkPolicy = "fork"
	// ParkDelete deletes environments at the step boundary instead of parking.
	ParkDelete ParkPolicy = "delete"
	// ParkNone leaves environments unparked across step boundaries.
	ParkNone ParkPolicy = "none"
	// ParkLegacySuspend is the legacy swebench-loader "suspend" policy, which maps
	// to ParkSuspendSync per DESIGN §5.4 (ate-env-api hardcodes DURABILITY_MODE_SYNCHRONOUS).
	ParkLegacySuspend ParkPolicy = "suspend"
	// ParkSuspend is an alias for ParkLegacySuspend.
	ParkSuspend ParkPolicy = "suspend"
)

// NormalizeParkPolicy validates p against the canonical v1 park policies and
// normalizes legacy "suspend" to ParkSuspendSync per DESIGN §5.4.
func NormalizeParkPolicy(p ParkPolicy) (ParkPolicy, error) {
	switch p {
	case ParkSuspendAsync, ParkSuspendSync, ParkFork, ParkDelete, ParkNone:
		return p, nil
	case ParkPolicyHold:
		return ParkNone, nil
	case ParkPolicySnapshot, "", ParkLegacySuspend:
		return ParkSuspendSync, nil
	case ParkPolicyRelease:
		return ParkDelete, nil
	default:
		return "", fmt.Errorf("invalid park policy %q (want %q, %q, %q, %q, %q, or legacy %q)",
			p, ParkSuspendAsync, ParkSuspendSync, ParkFork, ParkDelete, ParkNone, ParkLegacySuspend)
	}
}

var (
	resolvedCacheMu sync.Mutex
	resolvedCache   = map[string]*jsonschema.Resolved{}
)

// LoadJSONSchema resolves one of the embedded v1/*.schema.json files by name
// (for example, "schedule.schema.json" or "v1/schedule.schema.json").
func LoadJSONSchema(name string) (*jsonschema.Resolved, error) {
	name = strings.TrimPrefix(name, "v1/")
	resolvedCacheMu.Lock()
	if r, ok := resolvedCache[name]; ok {
		resolvedCacheMu.Unlock()
		return r, nil
	}
	resolvedCacheMu.Unlock()

	raw, err := SchemaFS.ReadFile(path.Join("v1", name))
	if err != nil {
		return nil, fmt.Errorf("read embedded schema %q: %w", name, err)
	}
	var s jsonschema.Schema
	if err := json.Unmarshal(raw, &s); err != nil {
		return nil, fmt.Errorf("unmarshal schema %q: %w", name, err)
	}
	resolved, err := s.Resolve(&jsonschema.ResolveOptions{
		BaseURI: baseSchemaURI + name,
		Loader:  loadEmbeddedSchema,
	})
	if err != nil {
		return nil, fmt.Errorf("resolve schema %q: %w", name, err)
	}

	resolvedCacheMu.Lock()
	resolvedCache[name] = resolved
	resolvedCacheMu.Unlock()
	return resolved, nil
}

func loadEmbeddedSchema(uri *url.URL) (*jsonschema.Schema, error) {
	base := path.Base(uri.Path)
	raw, err := SchemaFS.ReadFile(path.Join("v1", base))
	if err != nil {
		return nil, fmt.Errorf("load referenced schema %q: %w", uri.String(), err)
	}
	var s jsonschema.Schema
	if err := json.Unmarshal(raw, &s); err != nil {
		return nil, fmt.Errorf("unmarshal referenced schema %q: %w", uri.String(), err)
	}
	return &s, nil
}

// ValidateAgainstJSONSchema validates a raw JSON document against the named
// embedded JSON Schema.
func ValidateAgainstJSONSchema(schemaName string, data []byte) error {
	resolved, err := LoadJSONSchema(schemaName)
	if err != nil {
		return err
	}
	var instance any
	dec := json.NewDecoder(bytes.NewReader(data))
	if err := dec.Decode(&instance); err != nil {
		return fmt.Errorf("decode json instance for %s: %w", schemaName, err)
	}
	if err := ensureEOF(dec); err != nil {
		return fmt.Errorf("decode json instance for %s: %w", schemaName, err)
	}
	if err := resolved.Validate(instance); err != nil {
		return fmt.Errorf("jsonschema %s validation failed: %w", schemaName, err)
	}
	return nil
}

// ValidateJSONLAgainstJSONSchema validates each non-empty line in r against the
// named embedded JSON Schema, reporting 1-based line numbers on error.
func ValidateJSONLAgainstJSONSchema(schemaName string, r io.Reader) error {
	resolved, err := LoadJSONSchema(schemaName)
	if err != nil {
		return err
	}
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	lineNum := 0
	for sc.Scan() {
		lineNum++
		line := bytes.TrimSpace(sc.Bytes())
		if len(line) == 0 {
			continue
		}
		var instance any
		dec := json.NewDecoder(bytes.NewReader(line))
		if err := dec.Decode(&instance); err != nil {
			return fmt.Errorf("line %d: decode json: %w", lineNum, err)
		}
		if err := ensureEOF(dec); err != nil {
			return fmt.Errorf("line %d: %w", lineNum, err)
		}
		if err := resolved.Validate(instance); err != nil {
			return fmt.Errorf("line %d: jsonschema %s validation failed: %w", lineNum, schemaName, err)
		}
	}
	if err := sc.Err(); err != nil {
		return fmt.Errorf("scan jsonl: %w", err)
	}
	return nil
}

// DecodeJSONStrict decodes a single JSON value from r with DisallowUnknownFields
// and rejects trailing non-whitespace tokens. If *T or T implements Validator,
// Validate() is called before returning.
func DecodeJSONStrict[T any](r io.Reader) (T, error) {
	var zero T
	dec := json.NewDecoder(r)
	dec.DisallowUnknownFields()
	var out T
	if err := dec.Decode(&out); err != nil {
		return zero, fmt.Errorf("strict json decode: %w", err)
	}
	if err := ensureEOF(dec); err != nil {
		return zero, fmt.Errorf("strict json decode: %w", err)
	}
	if err := runValidate(&out); err != nil {
		return zero, err
	}
	return out, nil
}

// ForEachJSONL streams JSONL records from r using strict decoding, invoking fn
// for each decoded item. Empty/whitespace-only lines are skipped. Any decode or
// validation error includes the 1-based line number.
func ForEachJSONL[T any](r io.Reader, fn func(lineNum int, item T) error) error {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	lineNum := 0
	for sc.Scan() {
		lineNum++
		line := bytes.TrimSpace(sc.Bytes())
		if len(line) == 0 {
			continue
		}
		item, err := DecodeJSONStrict[T](bytes.NewReader(line))
		if err != nil {
			return fmt.Errorf("line %d: %w", lineNum, err)
		}
		if fn != nil {
			if err := fn(lineNum, item); err != nil {
				return fmt.Errorf("line %d: %w", lineNum, err)
			}
		}
	}
	if err := sc.Err(); err != nil {
		return fmt.Errorf("scan jsonl: %w", err)
	}
	return nil
}

// DecodeJSONLStrict decodes all JSONL records from r into a slice.
func DecodeJSONLStrict[T any](r io.Reader) ([]T, error) {
	var out []T
	err := ForEachJSONL[T](r, func(_ int, item T) error {
		out = append(out, item)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// DecodeYAMLStrict decodes YAML bytes strictly (disallowing unknown fields) and
// runs Validate() if implemented.
func DecodeYAMLStrict[T any](data []byte) (T, error) {
	var zero T
	jsonBytes, err := yaml.YAMLToJSONStrict(data)
	if err != nil {
		return zero, fmt.Errorf("strict yaml decode: %w", err)
	}
	return DecodeJSONStrict[T](bytes.NewReader(jsonBytes))
}

func ensureEOF(dec *json.Decoder) error {
	var extra any
	if err := dec.Decode(&extra); err != io.EOF {
		if err == nil {
			return errors.New("unexpected trailing JSON token")
		}
		return fmt.Errorf("trailing data: %w", err)
	}
	return nil
}

func runValidate[T any](ptr *T) error {
	if v, ok := any(ptr).(Validator); ok {
		return v.Validate()
	}
	if v, ok := any(*ptr).(Validator); ok {
		return v.Validate()
	}
	return nil
}

func checkSchemaVersion(v string) error {
	if v != SchemaVersion {
		return fmt.Errorf("schema_version must be %q, got %q", SchemaVersion, v)
	}
	return nil
}

func requireRFC3339(field, val string) error {
	if strings.TrimSpace(val) == "" {
		return fmt.Errorf("%s is required", field)
	}
	if _, err := time.Parse(time.RFC3339Nano, val); err != nil {
		return fmt.Errorf("%s must be RFC3339 timestamp (%q): %w", field, val, err)
	}
	return nil
}

func optionalRFC3339(field, val string) error {
	if strings.TrimSpace(val) == "" {
		return nil
	}
	return requireRFC3339(field, val)
}
