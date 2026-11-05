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

package inventory

import (
	"bytes"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"path"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/google/jsonschema-go/jsonschema"
	ateapipb "go.ate.dev/substrate/api/ateapi/v1"
)

// SchemaVersion is the schema_version every v1 snapshot carries.
const SchemaVersion = "v1"

// SnapshotSchema is the embedded JSON Schema file for Snapshot.
const SnapshotSchema = "snapshot.schema.json"

const baseSchemaURI = "https://ai-on-gke.github.io/sandboxenv-rl/schema/inventory/v1/"

// SchemaFS embeds the JSON Schema v1 files under v1/.
//
//go:embed v1/*.schema.json
var SchemaFS embed.FS

// Actor states: ateapi.ActorState lowercased without ACTOR_STATE_, the values
// Substrate writes as ate.actor.state. Deleted appears on Substrate's
// lifecycle records only; a snapshot never lists a deleted actor, but a reader
// that merges events and snapshots shares the vocabulary. Unspecified is what
// ActorStateValue returns for an enum value this package does not know, so a
// newer control plane does not break a reader.
const (
	ActorStateUnspecified = "unspecified"
	ActorStateResuming    = "resuming"
	ActorStateRunning     = "running"
	ActorStateSuspending  = "suspending"
	ActorStateSuspended   = "suspended"
	ActorStateCrashed     = "crashed"
	ActorStateDeleting    = "deleting"
	ActorStateReverting   = "reverting"
	ActorStateDeleted     = "deleted"
)

// Worker states: ateapi.WorkerState lowercased without WORKER_STATE_.
const (
	WorkerStateUnspecified = "unspecified"
	WorkerStateActive      = "active"
	WorkerStateDraining    = "draining"
	WorkerStateUnavailable = "unavailable"
)

// Golden snapshot states of a template, derived by GoldenState from
// ActorTemplateStatus.golden_snapshot_status.
const (
	GoldenNone    = "none"
	GoldenPending = "pending"
	GoldenReady   = "ready"
	GoldenFailed  = "failed"
)

var (
	actorStates = map[string]bool{
		ActorStateUnspecified: true, ActorStateResuming: true, ActorStateRunning: true,
		ActorStateSuspending: true, ActorStateSuspended: true, ActorStateCrashed: true,
		ActorStateDeleting: true, ActorStateReverting: true,
	}
	workerStates = map[string]bool{
		WorkerStateUnspecified: true, WorkerStateActive: true, WorkerStateDraining: true,
		WorkerStateUnavailable: true,
	}
	goldenStates = map[string]bool{
		GoldenNone: true, GoldenPending: true, GoldenReady: true, GoldenFailed: true,
	}
)

// credentialWords are the words an actor label key must not contain, in any
// letter case. A snapshot is published to a shared bucket, and a label key
// that names a credential is the usual sign that its value is one.
// TestSchemaHasNoCredentialFields holds the schema's own property names to the
// same list.
var credentialWords = []string{"token", "password", "authorization", "secret", "credential"}

// labelKeyPattern matches a label key that names a credential. It is spelled
// with character classes instead of (?i) so the same text is a valid pattern
// in Go's RE2 and in the ECMA-262 dialect JSON Schema uses; the propertyNames
// rule on Actor.labels in snapshot.schema.json carries the identical pattern,
// and TestLabelKeyPatternMatchesSchema keeps the two equal.
var labelKeyPattern = regexp.MustCompile(caseInsensitivePattern(credentialWords))

func caseInsensitivePattern(words []string) string {
	var b strings.Builder
	for i, w := range words {
		if i > 0 {
			b.WriteByte('|')
		}
		for _, r := range w {
			b.WriteByte('[')
			b.WriteRune(unicode.ToUpper(r))
			b.WriteRune(r)
			b.WriteByte(']')
		}
	}
	return b.String()
}

// Snapshot is one cluster's inventory at GeneratedAt.
type Snapshot struct {
	SchemaVersion string    `json:"schema_version"`
	GeneratedAt   time.Time `json:"generated_at"`
	// Project, Cluster and Location identify the cluster the reporter runs in;
	// a reader lists snapshots across them.
	Project  string   `json:"project"`
	Cluster  string   `json:"cluster"`
	Location string   `json:"location,omitempty"`
	Reporter Reporter `json:"reporter"`

	Atespaces   []Atespace   `json:"atespaces"`
	Templates   []Template   `json:"templates"`
	Tags        []Tag        `json:"tags,omitempty"`
	WorkerPools []WorkerPool `json:"workerpools"`
	Workers     []Worker     `json:"workers"`
	Actors      []Actor      `json:"actors"`
	Deployments []Deployment `json:"deployments"`
	// Counts is derived from Actors; Validate checks that it matches.
	Counts Counts `json:"counts"`
}

// Reporter identifies the writer and its interval, so a reader can tell a
// stale snapshot from a slow one.
type Reporter struct {
	Version         string `json:"version"`
	Commit          string `json:"commit,omitempty"`
	IntervalSeconds int    `json:"interval_s"`
}

// Atespace is a Substrate tenant boundary.
type Atespace struct {
	Name       string    `json:"name"`
	CreateTime time.Time `json:"create_time"`
}

// Template is an ActorTemplate. A derived per-image template names its base
// and its rootfs image; a base template has neither.
type Template struct {
	Atespace    string    `json:"atespace"`
	Name        string    `json:"name"`
	Base        string    `json:"base,omitempty"`
	RootfsImage string    `json:"rootfs_image,omitempty"`
	GuestImage  string    `json:"guest_image,omitempty"`
	WorkerPool  string    `json:"workerpool,omitempty"`
	GoldenState string    `json:"golden_state,omitempty"`
	CreateTime  time.Time `json:"create_time"`
}

// Tag is a named snapshot, for example a template's golden snapshot.
type Tag struct {
	Atespace    string    `json:"atespace"`
	Name        string    `json:"name"`
	SourceActor string    `json:"source_actor,omitempty"`
	CreateTime  time.Time `json:"create_time"`
}

// WorkerPool is the Kubernetes WorkerPool object that owns worker pods.
type WorkerPool struct {
	Namespace     string `json:"namespace"`
	Name          string `json:"name"`
	Replicas      int32  `json:"replicas"`
	ReadyReplicas int32  `json:"ready_replicas"`
	SandboxClass  string `json:"sandbox_class,omitempty"`
	SandboxConfig string `json:"sandbox_config,omitempty"`
	WorkerImage   string `json:"worker_image,omitempty"`
}

// Resources are a worker's capacity or allocation, in the units
// ateapi.WorkerResources uses.
type Resources struct {
	CPUMillicores int64 `json:"cpu_m"`
	MemoryBytes   int64 `json:"memory_bytes"`
}

// Worker is one worker pod as the control plane registers it. Name is the
// control plane's opaque worker name; never parse it.
type Worker struct {
	Name      string    `json:"name"`
	Namespace string    `json:"namespace"`
	Pool      string    `json:"pool"`
	Pod       string    `json:"pod"`
	Node      string    `json:"node,omitempty"`
	State     string    `json:"state"`
	Capacity  Resources `json:"capacity"`
	Allocated Resources `json:"allocated"`
}

// Actor is one Substrate actor, the entity behind an ate-env environment.
// Labels are the actor's labels as data; Validate refuses a key that names a
// credential (see credentialWords).
type Actor struct {
	UID          string            `json:"uid"`
	Atespace     string            `json:"atespace"`
	Name         string            `json:"name"`
	Template     string            `json:"template,omitempty"`
	SourceTag    string            `json:"source_tag,omitempty"`
	State        string            `json:"state"`
	AssignedNode string            `json:"assigned_node,omitempty"`
	Worker       string            `json:"worker,omitempty"`
	CreateTime   time.Time         `json:"create_time"`
	UpdateTime   time.Time         `json:"update_time"`
	Crash        *Crash            `json:"crash,omitempty"`
	Labels       map[string]string `json:"labels,omitempty"`
}

// Crash is the control plane's crash record, ActorStatus.crash. Substrate sets
// it when an actor enters crashed, keeps it while a crashed actor is deleting
// or reverting, and clears it only when a revert returns the actor to
// suspended; so Crash can accompany any state, and a crashed actor can lack
// one (fakecontrol.SetState writes crashed without a record).
type Crash struct {
	Message   string    `json:"message"`
	CrashTime time.Time `json:"crash_time"`
}

// Deployment is an ate-env-api Deployment found in the cluster.
type Deployment struct {
	Namespace   string `json:"namespace"`
	Name        string `json:"name"`
	Image       string `json:"image"`
	Version     string `json:"version,omitempty"`
	Ready       int32  `json:"ready"`
	Tracing     bool   `json:"tracing"`
	MetricsPort int32  `json:"metrics_port,omitempty"`
}

// Counts summarizes Actors so a reader can render a fleet page without
// decoding every actor.
type Counts struct {
	ActorsByState    map[string]int `json:"actors_by_state"`
	ActorsByAtespace map[string]int `json:"actors_by_atespace"`
}

// ActorStateValue returns the snapshot vocabulary for s.
func ActorStateValue(s ateapipb.ActorState) string {
	switch s {
	case ateapipb.ActorState_ACTOR_STATE_RESUMING:
		return ActorStateResuming
	case ateapipb.ActorState_ACTOR_STATE_RUNNING:
		return ActorStateRunning
	case ateapipb.ActorState_ACTOR_STATE_SUSPENDING:
		return ActorStateSuspending
	case ateapipb.ActorState_ACTOR_STATE_SUSPENDED:
		return ActorStateSuspended
	case ateapipb.ActorState_ACTOR_STATE_CRASHED:
		return ActorStateCrashed
	case ateapipb.ActorState_ACTOR_STATE_DELETING:
		return ActorStateDeleting
	case ateapipb.ActorState_ACTOR_STATE_REVERTING:
		return ActorStateReverting
	default:
		return ActorStateUnspecified
	}
}

// WorkerStateValue returns the snapshot vocabulary for s.
func WorkerStateValue(s ateapipb.WorkerState) string {
	switch s {
	case ateapipb.WorkerState_WORKER_STATE_ACTIVE:
		return WorkerStateActive
	case ateapipb.WorkerState_WORKER_STATE_DRAINING:
		return WorkerStateDraining
	case ateapipb.WorkerState_WORKER_STATE_UNAVAILABLE:
		return WorkerStateUnavailable
	default:
		return WorkerStateUnspecified
	}
}

// GoldenState derives a template's golden snapshot state. configured says
// whether the template asks for a golden snapshot at all; status is its
// golden_snapshot_status, nil when the control plane reports none.
func GoldenState(status *ateapipb.GoldenSnapshotStatus, configured bool) string {
	switch {
	case !configured:
		return GoldenNone
	case status.GetErrorMessage() != "":
		return GoldenFailed
	case status.GetGoldenTag().GetName() != "":
		return GoldenReady
	default:
		return GoldenPending
	}
}

// ComputeCounts derives Counts from actors.
func ComputeCounts(actors []Actor) Counts {
	c := Counts{ActorsByState: map[string]int{}, ActorsByAtespace: map[string]int{}}
	for _, a := range actors {
		c.ActorsByState[a.State]++
		c.ActorsByAtespace[a.Atespace]++
	}
	return c
}

// Sort orders every list by its identity, so two snapshots of the same state
// encode to the same bytes whatever order the control plane returned.
func (s *Snapshot) Sort() {
	sort.Slice(s.Atespaces, func(i, j int) bool { return s.Atespaces[i].Name < s.Atespaces[j].Name })
	sort.Slice(s.Templates, func(i, j int) bool {
		return key2(s.Templates[i].Atespace, s.Templates[i].Name) < key2(s.Templates[j].Atespace, s.Templates[j].Name)
	})
	sort.Slice(s.Tags, func(i, j int) bool {
		return key2(s.Tags[i].Atespace, s.Tags[i].Name) < key2(s.Tags[j].Atespace, s.Tags[j].Name)
	})
	sort.Slice(s.WorkerPools, func(i, j int) bool {
		return key2(s.WorkerPools[i].Namespace, s.WorkerPools[i].Name) < key2(s.WorkerPools[j].Namespace, s.WorkerPools[j].Name)
	})
	sort.Slice(s.Workers, func(i, j int) bool { return s.Workers[i].Name < s.Workers[j].Name })
	sort.Slice(s.Actors, func(i, j int) bool { return s.Actors[i].UID < s.Actors[j].UID })
	sort.Slice(s.Deployments, func(i, j int) bool {
		return key2(s.Deployments[i].Namespace, s.Deployments[i].Name) < key2(s.Deployments[j].Namespace, s.Deployments[j].Name)
	})
}

func key2(a, b string) string { return a + "\x00" + b }

// Validate checks what the JSON Schema requires and what it cannot express:
// the version, non-null lists and counts maps, the identity and timestamps of
// the snapshot and of each object, the state vocabularies, the numeric bounds,
// unique keys in every list, label keys that name no credential, and Counts
// matching Actors. It does not tie Crash to a state; see Crash. A snapshot
// that passes Validate encodes to a document that validates against
// snapshot.schema.json.
func (s *Snapshot) Validate() error {
	if s.SchemaVersion != SchemaVersion {
		return fmt.Errorf("inventory: unsupported schema_version %q (want %q)", s.SchemaVersion, SchemaVersion)
	}
	if s.GeneratedAt.IsZero() {
		return errors.New("inventory: generated_at is required")
	}
	if s.Project == "" || s.Cluster == "" {
		return errors.New("inventory: project and cluster are required")
	}
	if s.Reporter.Version == "" {
		return errors.New("inventory: reporter.version is required")
	}
	if s.Reporter.IntervalSeconds <= 0 {
		return fmt.Errorf("inventory: reporter.interval_s must be positive, got %d", s.Reporter.IntervalSeconds)
	}
	// The schema requires these lists; a nil slice would encode as null. Tags
	// is optional and omitted when nil.
	for _, l := range []struct {
		name string
		nil  bool
	}{
		{"atespaces", s.Atespaces == nil}, {"templates", s.Templates == nil},
		{"workerpools", s.WorkerPools == nil}, {"workers", s.Workers == nil},
		{"actors", s.Actors == nil}, {"deployments", s.Deployments == nil},
	} {
		if l.nil {
			return fmt.Errorf("inventory: %s must be a list, not null (use an empty slice)", l.name)
		}
	}
	seen := map[string]map[string]int{}
	unique := func(list, key string, i int) error {
		m := seen[list]
		if m == nil {
			m = map[string]int{}
			seen[list] = m
		}
		if j, dup := m[key]; dup {
			return fmt.Errorf("inventory: %s[%d] repeats %s[%d]", list, i, list, j)
		}
		m[key] = i
		return nil
	}
	for i, a := range s.Atespaces {
		if a.Name == "" {
			return fmt.Errorf("inventory: atespaces[%d]: name is required", i)
		}
		if a.CreateTime.IsZero() {
			return fmt.Errorf("inventory: atespaces[%d]: create_time is required", i)
		}
		if err := unique("atespaces", a.Name, i); err != nil {
			return err
		}
	}
	for i, t := range s.Templates {
		if t.Atespace == "" || t.Name == "" {
			return fmt.Errorf("inventory: templates[%d]: atespace and name are required", i)
		}
		if t.CreateTime.IsZero() {
			return fmt.Errorf("inventory: templates[%d]: create_time is required", i)
		}
		if t.GoldenState != "" && !goldenStates[t.GoldenState] {
			return fmt.Errorf("inventory: templates[%d]: unknown golden_state %q", i, t.GoldenState)
		}
		if err := unique("templates", key2(t.Atespace, t.Name), i); err != nil {
			return err
		}
	}
	for i, t := range s.Tags {
		if t.Atespace == "" || t.Name == "" {
			return fmt.Errorf("inventory: tags[%d]: atespace and name are required", i)
		}
		if t.CreateTime.IsZero() {
			return fmt.Errorf("inventory: tags[%d]: create_time is required", i)
		}
		if err := unique("tags", key2(t.Atespace, t.Name), i); err != nil {
			return err
		}
	}
	for i, p := range s.WorkerPools {
		if p.Namespace == "" || p.Name == "" {
			return fmt.Errorf("inventory: workerpools[%d]: namespace and name are required", i)
		}
		if p.Replicas < 0 || p.ReadyReplicas < 0 {
			return fmt.Errorf("inventory: workerpools[%d]: replica counts must not be negative", i)
		}
		if err := unique("workerpools", key2(p.Namespace, p.Name), i); err != nil {
			return err
		}
	}
	for i, w := range s.Workers {
		if w.Name == "" || w.Namespace == "" || w.Pool == "" || w.Pod == "" {
			return fmt.Errorf("inventory: workers[%d]: name, namespace, pool and pod are required", i)
		}
		if !workerStates[w.State] {
			return fmt.Errorf("inventory: workers[%d]: unknown state %q", i, w.State)
		}
		if w.Capacity.CPUMillicores < 0 || w.Capacity.MemoryBytes < 0 || w.Allocated.CPUMillicores < 0 || w.Allocated.MemoryBytes < 0 {
			return fmt.Errorf("inventory: workers[%d]: resources must not be negative", i)
		}
		if err := unique("workers", w.Name, i); err != nil {
			return err
		}
	}
	for i, a := range s.Actors {
		if a.UID == "" || a.Atespace == "" || a.Name == "" {
			return fmt.Errorf("inventory: actors[%d]: uid, atespace and name are required", i)
		}
		if err := unique("actors", a.UID, i); err != nil {
			return err
		}
		if !actorStates[a.State] {
			return fmt.Errorf("inventory: actors[%d]: unknown state %q", i, a.State)
		}
		if a.CreateTime.IsZero() || a.UpdateTime.IsZero() {
			return fmt.Errorf("inventory: actors[%d]: create_time and update_time are required", i)
		}
		if a.Crash != nil && a.Crash.CrashTime.IsZero() {
			return fmt.Errorf("inventory: actors[%d]: crash.crash_time is required", i)
		}
		for k := range a.Labels {
			if labelKeyPattern.MatchString(k) {
				return fmt.Errorf("inventory: actors[%d]: label key %q names a credential (%s); a snapshot carries none", i, k, strings.Join(credentialWords, ", "))
			}
		}
	}
	for i, d := range s.Deployments {
		if d.Namespace == "" || d.Name == "" || d.Image == "" {
			return fmt.Errorf("inventory: deployments[%d]: namespace, name and image are required", i)
		}
		if d.Ready < 0 {
			return fmt.Errorf("inventory: deployments[%d]: ready must not be negative", i)
		}
		if d.MetricsPort < 0 || d.MetricsPort > 65535 {
			return fmt.Errorf("inventory: deployments[%d]: metrics_port %d is not a port number", i, d.MetricsPort)
		}
		if err := unique("deployments", key2(d.Namespace, d.Name), i); err != nil {
			return err
		}
	}
	// The schema requires both maps as objects; a nil map would encode as null
	// and still compare equal to the empty counts of an empty actor list.
	if s.Counts.ActorsByState == nil || s.Counts.ActorsByAtespace == nil {
		return errors.New("inventory: counts.actors_by_state and counts.actors_by_atespace must be objects, not null (use ComputeCounts)")
	}
	if want := ComputeCounts(s.Actors); !countsEqual(want, s.Counts) {
		return errors.New("inventory: counts do not match actors")
	}
	return nil
}

func countsEqual(a, b Counts) bool {
	return mapsEqual(a.ActorsByState, b.ActorsByState) && mapsEqual(a.ActorsByAtespace, b.ActorsByAtespace)
}

func mapsEqual(a, b map[string]int) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}

// Encode writes s as indented JSON with a trailing newline, the form the
// reporter stores and the golden test pins.
func (s *Snapshot) Encode(w io.Writer) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(s)
}

// DecodeSnapshot reads one snapshot from r. It rejects unknown fields, a
// second JSON value after the first, any document snapshot.schema.json
// refuses, and any snapshot Validate refuses. The schema pass is not
// redundant: the Go types accept null for an optional list, map or object and
// a zero value for an optional enum or port, forms omitempty never writes and
// the schema does not allow, so without it a reader would accept documents a
// writer cannot produce.
func DecodeSnapshot(r io.Reader) (*Snapshot, error) {
	raw, err := io.ReadAll(r)
	if err != nil {
		return nil, fmt.Errorf("inventory: decode snapshot: %w", err)
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	var s Snapshot
	if err := dec.Decode(&s); err != nil {
		return nil, fmt.Errorf("inventory: decode snapshot: %w", err)
	}
	if err := ensureEOF(dec); err != nil {
		return nil, fmt.Errorf("inventory: decode snapshot: %w", err)
	}
	if err := ValidateAgainstJSONSchema(SnapshotSchema, raw); err != nil {
		return nil, err
	}
	if err := s.Validate(); err != nil {
		return nil, err
	}
	return &s, nil
}

func ensureEOF(dec *json.Decoder) error {
	var extra json.RawMessage
	err := dec.Decode(&extra)
	switch {
	case errors.Is(err, io.EOF):
		return nil
	case err != nil:
		return err
	default:
		return errors.New("trailing data after the JSON value")
	}
}

var (
	resolvedMu sync.Mutex
	resolved   = map[string]*jsonschema.Resolved{}
)

// LoadJSONSchema resolves one of the embedded v1/*.schema.json files by name
// ("snapshot.schema.json" or "v1/snapshot.schema.json").
func LoadJSONSchema(name string) (*jsonschema.Resolved, error) {
	name = strings.TrimPrefix(name, "v1/")
	resolvedMu.Lock()
	defer resolvedMu.Unlock()
	if r, ok := resolved[name]; ok {
		return r, nil
	}
	raw, err := SchemaFS.ReadFile(path.Join("v1", name))
	if err != nil {
		return nil, fmt.Errorf("inventory: read embedded schema %q: %w", name, err)
	}
	var s jsonschema.Schema
	if err := json.Unmarshal(raw, &s); err != nil {
		return nil, fmt.Errorf("inventory: unmarshal schema %q: %w", name, err)
	}
	r, err := s.Resolve(&jsonschema.ResolveOptions{BaseURI: baseSchemaURI + name, Loader: loadEmbeddedSchema})
	if err != nil {
		return nil, fmt.Errorf("inventory: resolve schema %q: %w", name, err)
	}
	resolved[name] = r
	return r, nil
}

func loadEmbeddedSchema(uri *url.URL) (*jsonschema.Schema, error) {
	raw, err := SchemaFS.ReadFile(path.Join("v1", path.Base(uri.Path)))
	if err != nil {
		return nil, fmt.Errorf("inventory: load referenced schema %q: %w", uri.String(), err)
	}
	var s jsonschema.Schema
	if err := json.Unmarshal(raw, &s); err != nil {
		return nil, fmt.Errorf("inventory: unmarshal referenced schema %q: %w", uri.String(), err)
	}
	return &s, nil
}

// ValidateAgainstJSONSchema validates raw JSON against the named embedded
// schema. Readers in other languages run the same check from the schema file.
func ValidateAgainstJSONSchema(schemaName string, data []byte) error {
	r, err := LoadJSONSchema(schemaName)
	if err != nil {
		return err
	}
	var instance any
	dec := json.NewDecoder(bytes.NewReader(data))
	if err := dec.Decode(&instance); err != nil {
		return fmt.Errorf("inventory: decode instance for %s: %w", schemaName, err)
	}
	if err := ensureEOF(dec); err != nil {
		return fmt.Errorf("inventory: decode instance for %s: %w", schemaName, err)
	}
	if err := r.Validate(instance); err != nil {
		return fmt.Errorf("inventory: %s validation failed: %w", schemaName, err)
	}
	return nil
}
