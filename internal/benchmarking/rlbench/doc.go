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

// Package rlbench holds the dataset-agnostic execution, event-recording, and
// reporting engine for the Substrate RL benchmark.
//
//   - Instance, Adapter, Grader, and LoadDataset: the JSONL dataset manifest,
//     where each row carries an instance id, a digest-pinned sandbox image, an
//     optional dataset kind (defaulting to "swebench"), and opaque
//     dataset-specific material decoded by a registered Adapter (such as
//     internal/benchmarking/rlbench/datasets/swebench).
//   - Trajectory: the scripted rollout a task replays (run a command or write
//     a file, with think time, timeout, expected exit, and an optional reward
//     step), with setup steps that run once per environment and reset steps
//     that run before each later step. Built-in image-delivery trajectories
//     live in this package; dataset-specific trajectories and graders live in
//     their respective dataset adapter packages.
//   - Event and Recorder: buffered JSONL writer emitting one line per operation
//     of a run.
//   - Builder, Summarize, and Build: streaming and in-memory aggregation over
//     JSONL events, producing percentiles, retry counts, per-step tables, and
//     unresolved instance lists in exclude-file format.
package rlbench
