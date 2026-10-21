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
	"reflect"
	"testing"
)

func TestLeadingFiles(t *testing.T) {
	for _, tc := range []struct {
		args, files, rest []string
	}{
		{nil, nil, nil},
		{[]string{"a.jsonl"}, []string{"a.jsonl"}, nil},
		{[]string{"a.jsonl", "b.jsonl", "-json", "o.json"}, []string{"a.jsonl", "b.jsonl"}, []string{"-json", "o.json"}},
		{[]string{"-json", "o.json", "a.jsonl"}, nil, []string{"-json", "o.json", "a.jsonl"}},
	} {
		files, rest := leadingFiles(tc.args)
		if len(files) == 0 {
			files = nil
		}
		if len(rest) == 0 {
			rest = nil
		}
		if !reflect.DeepEqual(files, tc.files) || !reflect.DeepEqual(rest, tc.rest) {
			t.Errorf("leadingFiles(%q) = %q, %q; want %q, %q", tc.args, files, rest, tc.files, tc.rest)
		}
	}
}
