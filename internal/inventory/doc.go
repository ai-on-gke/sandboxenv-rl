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

// Package inventory defines the fleet inventory snapshot: every atespace,
// actor template, tag, WorkerPool, worker, actor and ate-env-api Deployment
// of one cluster at one instant, as JSON with an embedded JSON Schema (v1/).
//
// A per-cluster reporter writes one Snapshot per interval from the Substrate
// control plane (ListAtespaces, ListActorTemplates, ListTags, ListActors
// across atespaces, ListWorkers) and the Kubernetes API; a fleet-wide reader
// consumes snapshots from many clusters. Both sides decode with
// DecodeSnapshot, which rejects unknown fields and validates the invariants
// the schema cannot express.
//
// State strings use Substrate's own vocabulary: ateapi.ActorState lowercased
// without its prefix, the same values Substrate writes as ate.actor.state on
// its lifecycle records, so a snapshot joins that event stream without a
// mapping. Identity (uid, name, atespace, template) is data in a snapshot;
// it never becomes a metric label. No field carries a credential.
package inventory
