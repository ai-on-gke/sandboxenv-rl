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

package main

import (
	"strings"
	"testing"
)

func TestValidateGCSFolderName(t *testing.T) {
	for _, tc := range []struct {
		folder  string
		traced  bool
		wantErr string
	}{
		{"2026-10-27_synthetic-w4_math-bb4dcec_suspend-sync", false, ""},
		{"2026-10-28_qwen3.5-35b_r2e-gym-easy_prewarmed_traced", true, ""},
		{" 2026-10-28_qwen3.5-35b_r2e-gym-easy_prewarmed_traced/ ", true, ""},
		{"2026-10-28_qwen3.5-35b_r2e-gym-easy_prewarmed", true, "must end in _traced"},
		{"2026-10-28_qwen3.5-35b_r2e-gym-easy_prewarmed_traced", false, "has no trace"},
		{"BadFolderName", false, "invalid benchmark folder name"},
		{"2026-13-40_a_b_c", false, "invalid date prefix"},
		{"2026-10-28_a_b", false, "invalid benchmark folder name"},
		{"2026-10-28_a_b_c_d", false, "invalid benchmark folder name"},
		{"2026-10-28_a_b_c_traced_traced", true, "invalid benchmark folder name"},
		{"2026-10-28_a_b_c_d_traced", true, "invalid benchmark folder name"},
		{"2026-10-28_a_b_snake_case", false, "invalid benchmark folder name"},
	} {
		err := ValidateGCSFolderName(tc.folder, tc.traced)
		switch {
		case tc.wantErr == "" && err != nil:
			t.Errorf("%q traced=%t: unexpected error %v", tc.folder, tc.traced, err)
		case tc.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tc.wantErr)):
			t.Errorf("%q traced=%t: want error containing %q, got %v", tc.folder, tc.traced, tc.wantErr, err)
		}
	}
}
