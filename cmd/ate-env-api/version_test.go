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
	info := func(mainVersion string, settings ...debug.BuildSetting) func() (*debug.BuildInfo, bool) {
		return func() (*debug.BuildInfo, bool) {
			return &debug.BuildInfo{Main: debug.Module{Version: mainVersion}, Settings: settings}, true
		}
	}
	rev := debug.BuildSetting{Key: "vcs.revision", Value: "0123456789abcdef0123"}
	cases := []struct {
		name    string
		ldflags string
		read    func() (*debug.BuildInfo, bool)
		want    string
	}{
		{"ldflags win", "v0.2.0", info("v0.1.0", rev), "v0.2.0"},
		{"module version", "dev", info("v0.1.0", rev), "v0.1.0"},
		{"vcs revision", "dev", info("(devel)", rev), "dev+0123456789ab"},
		{"dirty tree", "dev", info("(devel)", rev, debug.BuildSetting{Key: "vcs.modified", Value: "true"}), "dev+0123456789ab-dirty"},
		{"no vcs stamp", "dev", info("(devel)"), "dev"},
		{"no build info", "", func() (*debug.BuildInfo, bool) { return nil, false }, "dev"},
	}
	for _, tc := range cases {
		if got := resolveVersion(tc.ldflags, tc.read); got != tc.want {
			t.Errorf("%s: resolveVersion = %q, want %q", tc.name, got, tc.want)
		}
	}
}
