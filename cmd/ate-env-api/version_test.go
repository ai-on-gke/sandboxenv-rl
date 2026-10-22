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
	"runtime/debug"
	"testing"
)

func TestResolveVersion(t *testing.T) {
	info := func(mainVersion string) func() (*debug.BuildInfo, bool) {
		return func() (*debug.BuildInfo, bool) {
			return &debug.BuildInfo{Main: debug.Module{Version: mainVersion}}, true
		}
	}
	// The values the Go toolchain records (go1.27, checked with go version -m).
	pseudo := "v0.0.11-0.20261022233813-c9d0be5bac18"
	cases := []struct {
		name    string
		ldflags string
		read    func() (*debug.BuildInfo, bool)
		want    string
	}{
		{"ldflags win", "v0.2.0", info(pseudo), "v0.2.0"},
		{"release tag", "dev", info("v0.1.0"), "v0.1.0"},
		{"git checkout", "dev", info(pseudo), pseudo},
		{"uncommitted changes", "dev", info(pseudo + "+dirty"), pseudo + "+dirty"},
		{"-buildvcs=false", "dev", info("(devel)"), "dev"},
		{"no build info", "", func() (*debug.BuildInfo, bool) { return nil, false }, "dev"},
	}
	for _, tc := range cases {
		if got := resolveVersion(tc.ldflags, tc.read); got != tc.want {
			t.Errorf("%s: resolveVersion = %q, want %q", tc.name, got, tc.want)
		}
	}
}
