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
	"io"

	"github.com/agent-substrate/env/internal/benchmarking/rlbench"
)

// Instance is one SWE-bench task: the digest-pinned task image and the
// material a trajectory needs to evaluate it.
type Instance = rlbench.Instance

// RequireGrading checks that every instance carries what a grading
// trajectory needs.
func RequireGrading(instances []Instance) error {
	return rlbench.RequireGrading(instances)
}

// LoadDataset reads a JSONL manifest from a local path or an https URL.
func LoadDataset(source string) ([]Instance, error) {
	return rlbench.LoadDataset(source)
}

// ParseDataset decodes a JSONL manifest.
func ParseDataset(r io.Reader) ([]Instance, error) {
	return rlbench.ParseDataset(r)
}

// Select narrows a dataset by exclusion list, explicit IDs, and limit.
func Select(all []Instance, ids, exclude []string, limit int) ([]Instance, error) {
	return rlbench.Select(all, ids, exclude, limit)
}

// ReadIDList reads instance ids from a file, one per line.
func ReadIDList(path string) ([]string, error) {
	return rlbench.ReadIDList(path)
}
