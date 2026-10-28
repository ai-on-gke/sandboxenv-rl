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
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/agent-substrate/env/internal/benchmarking/schema"
)

// Event names. Every timed operation of a run is one of these.
const (
	EventCreate  = "create"       // CreateEnvironment call
	EventTTFI    = "ttfi"         // create start to the first command's return
	EventWake    = "wake"         // first command after a park, including the wake it triggers
	EventStep    = "step"         // one trajectory step (name is the step name)
	EventTask    = "task"         // one trajectory replay end to end
	EventSuspend = "suspend"      // SuspendEnvironment call
	EventFork    = "fork"         // ForkEnvironment call
	EventDelete  = "delete"       // DeleteEnvironment call
	EventBarrier = "step_barrier" // steps mode: first task start to last task end in a training step
	EventPark    = "park"         // steps mode: parking the whole fleet between steps
	EventRun     = "run"          // whole run
	EventPrewarm = "prewarm"      // prewarm operation before steady-state window
	EventReap    = "reap"         // post-run orphan reaper check
	EventThink   = "think"        // simulated LLM think pause
	EventPhase   = "phase"        // sub-step lifecycle phase (create, bootstrap, snapshot, resume, eval_harness, delete)
)

// DurMS converts a time.Duration to milliseconds as float64.
func DurMS(d time.Duration) float64 {
	return float64(d) / float64(time.Millisecond)
}

// Event is one JSONL record. Dur is wall time as seen by the loader; it
// includes the network and the API server, which is what a trainer sees.
type Event struct {
	SchemaVersion string    `json:"schema_version,omitempty"`
	Time          time.Time `json:"ts"`
	RunID         string    `json:"run"`
	Job           string    `json:"job,omitempty"`
	Worker        string    `json:"worker,omitempty"`
	Event         string    `json:"event"`
	Name          string    `json:"name,omitempty"`
	Phase         string    `json:"phase,omitempty"`
	Source        string    `json:"source,omitempty"`
	// Env is the environment's name. With a delete park, an environment is
	// recreated under "<name>-s<step>" from step 2 on, thus group a replay's
	// events by Instance and Task, not by Env.
	Env      string `json:"env,omitempty"`
	Instance string `json:"instance,omitempty"`
	Image    string `json:"image,omitempty"`
	// Step is the training step in steps mode (1-based); 0 in eval mode.
	Step int `json:"step,omitempty"`
	// Task numbers the replays of one instance (eval: task index; steps:
	// rollout index).
	Task       int     `json:"task,omitempty"`
	DurMS      float64 `json:"dur_ms"`
	SlotWaitMS float64 `json:"slot_wait_ms,omitempty"`
	OK         bool    `json:"ok"`
	Timeout    *bool   `json:"timeout,omitempty"`
	Exit       *int    `json:"exit,omitempty"`
	Reward     *int    `json:"reward,omitempty"`
	// Detail is the dataset grader's short verdict string for a reward step
	// (for example "f2p 1/1 p2p 3/4" or "invalid run").
	Detail string `json:"detail,omitempty"`
	Err    string `json:"err,omitempty"`
	// Count is the number of tasks or environments an aggregate event covers.
	Count int `json:"count,omitempty"`
	// Tail is the last bytes of a failed or reward step's output.
	Tail string `json:"tail,omitempty"`
	// Attempts is how many tries a ttfi or wake took; above 1 means the
	// environment was not serving when first asked.
	Attempts     int     `json:"attempts,omitempty"`
	Snapshot     string  `json:"snapshot,omitempty"`
	Node         string  `json:"node,omitempty"`
	Bytes        int64   `json:"bytes,omitempty"`
	SizeMB       float64 `json:"size_mb,omitempty"`
	DeletedCount int     `json:"deleted_count,omitempty"`
	OrphanCount  int     `json:"orphan_count,omitempty"`
	CacheHit     *bool   `json:"cache_hit,omitempty"`
	// Labels describe the run, on its run event: the loader's own (mode,
	// width, images, trajectory, the shape flags) and the operator's -label
	// values, which win on a clash.
	Labels  map[string]string  `json:"labels,omitempty"`
	Tags    map[string]string  `json:"tags,omitempty"`
	Metrics map[string]float64 `json:"metrics,omitempty"`
	// Working-set telemetry attached to step_barrier, park, and run events when
	// a scheduled working-set pool is active.
	WorkingSetHits       int     `json:"working_set_hits,omitempty"`
	WorkingSetMisses     int     `json:"working_set_misses,omitempty"`
	WorkingSetHitRate    float64 `json:"working_set_hit_rate,omitempty"`
	StandingPoolSize     int     `json:"standing_pool_size,omitempty"`
	Created              int     `json:"created,omitempty"`
	Reused               int     `json:"reused,omitempty"`
	Retired              int     `json:"retired,omitempty"`
	PrefetchLeadMarginMS float64 `json:"prefetch_lead_margin_ms,omitempty"`
	ReleaseOverlapMS     float64 `json:"release_overlap_ms,omitempty"`
}

const recorderBufferSize = 64 * 1024

// Recorder writes events as buffered JSON lines. It is safe for concurrent use.
type Recorder struct {
	mu     sync.Mutex
	bw     *bufio.Writer
	enc    *json.Encoder
	closer io.Closer
	runID  string
	job    string
	worker string
	n      int
	err    error
}

// NewRecorder writes to w through a 64 KiB buffer; a nil w discards events.
func NewRecorder(w io.Writer) *Recorder {
	if w == nil {
		return &Recorder{enc: json.NewEncoder(io.Discard)}
	}
	bw := bufio.NewWriterSize(w, recorderBufferSize)
	return &Recorder{bw: bw, enc: json.NewEncoder(bw)}
}

// NewFileRecorder creates path (and parent directories if needed) and returns a
// Recorder that stamps runID onto recorded events and closes the file on Close().
func NewFileRecorder(path, runID string) (*Recorder, error) {
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, err
		}
	}
	f, err := os.Create(path)
	if err != nil {
		return nil, err
	}
	r := NewRecorder(f)
	r.closer = f
	r.runID = runID
	return r, nil
}

// ShardEventPath returns the canonical per-worker shard file path under baseDir.
// If baseDir already ends with "events", it returns <baseDir>/<worker>.jsonl;
// otherwise it returns <baseDir>/events/<worker>.jsonl.
func ShardEventPath(baseDir, worker string) string {
	cleanWorker := strings.TrimSpace(worker)
	if cleanWorker == "" {
		cleanWorker = "worker-0"
	}
	cleanWorker = strings.TrimSuffix(cleanWorker, ".jsonl")
	if filepath.Base(filepath.Clean(baseDir)) == "events" {
		return filepath.Join(baseDir, cleanWorker+".jsonl")
	}
	return filepath.Join(baseDir, "events", cleanWorker+".jsonl")
}

// NewShardRecorder creates a per-worker shard file (events/<worker>.jsonl) and
// returns a Recorder that automatically stamps Worker (and RunID if provided) on emitted events.
func NewShardRecorder(baseDir, runID, worker string) (*Recorder, error) {
	p := ShardEventPath(baseDir, worker)
	if dir := filepath.Dir(p); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, err
		}
	}
	f, err := os.Create(p)
	if err != nil {
		return nil, err
	}
	r := NewRecorder(f)
	r.closer = f
	r.runID = runID
	r.worker = strings.TrimSuffix(filepath.Base(p), ".jsonl")
	return r, nil
}

// SetWorker configures a default Worker identifier stamped onto recorded events.
func (r *Recorder) SetWorker(worker string) {
	if r == nil {
		return
	}
	r.mu.Lock()
	r.worker = worker
	r.mu.Unlock()
}

// SetJob configures a default Job identifier stamped onto recorded events.
func (r *Recorder) SetJob(job string) {
	if r == nil {
		return
	}
	r.mu.Lock()
	r.job = job
	r.mu.Unlock()
}

// Record writes one event, stamping the time if unset. Coarse boundary events
// (step_barrier, park, run) flush the underlying buffer automatically.
func (r *Recorder) Record(e Event) {
	if e.Time.IsZero() {
		e.Time = time.Now().UTC()
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if e.RunID == "" && r.runID != "" {
		e.RunID = r.runID
	}
	if e.Job == "" && r.job != "" {
		e.Job = r.job
	}
	if e.Worker == "" && r.worker != "" {
		e.Worker = r.worker
	}
	if err := r.enc.Encode(e); err != nil && r.err == nil {
		r.err = err
	}
	if r.bw != nil && (e.Event == EventBarrier || e.Event == EventPark || e.Event == EventRun || e.Event == EventReap || e.Event == EventPrewarm) {
		if err := r.bw.Flush(); err != nil && r.err == nil {
			r.err = err
		}
	}
	r.n++
}

// Emit is an alias for Record.
func (r *Recorder) Emit(e Event) {
	r.Record(e)
}

// Flush flushes any buffered events to the underlying writer and returns the
// first write or flush error.
func (r *Recorder) Flush() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.flushLocked()
	return r.err
}

// Close flushes buffered events, closes any underlying shard file opened by
// NewShardRecorder, and returns the first write or flush error.
func (r *Recorder) Close() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.flushLocked()
	if r.closer != nil {
		if cerr := r.closer.Close(); cerr != nil && r.err == nil {
			r.err = cerr
		}
		r.closer = nil
	}
	return r.err
}

func (r *Recorder) flushLocked() {
	if r.bw != nil {
		if err := r.bw.Flush(); err != nil && r.err == nil {
			r.err = err
		}
	}
}

// Len is the number of events written so far.
func (r *Recorder) Len() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.n
}

// Err flushes any buffered events and returns the first write error, if any;
// a run whose events were lost must not report success.
func (r *Recorder) Err() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.flushLocked()
	return r.err
}

// WriteRunWindow validates and writes rw as indented JSON to path (run_window.json).
func WriteRunWindow(path string, rw schema.RunWindow) error {
	if rw.SchemaVersion == "" {
		rw.SchemaVersion = schema.SchemaVersion
	}
	if err := rw.Validate(); err != nil {
		return err
	}
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return fmt.Errorf("mkdir for run_window: %w", err)
		}
	}
	b, err := json.MarshalIndent(rw, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal run_window: %w", err)
	}
	b = append(b, '\n')
	if err := os.WriteFile(path, b, 0o644); err != nil {
		return fmt.Errorf("write run_window %q: %w", path, err)
	}
	return nil
}

// ReadRunWindow reads and strictly validates a run_window.json file from path.
func ReadRunWindow(path string) (*schema.RunWindow, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read run_window %q: %w", path, err)
	}
	rw, err := schema.DecodeJSONStrict[schema.RunWindow](bytes.NewReader(raw))
	if err != nil {
		return nil, fmt.Errorf("decode run_window %q: %w", path, err)
	}
	return &rw, nil
}

func intp(v int) *int { return &v }
