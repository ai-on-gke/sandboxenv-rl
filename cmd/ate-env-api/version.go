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

import "runtime/debug"

// version is set at build time with -ldflags "-X main.version=...", as for the
// ate-env CLI. Without it, buildVersion falls back to the Go build information.
var version = "dev"

// buildVersion returns the version GetServerInfo reports: the -ldflags value
// when set; otherwise the module version (a `go install ...@vX` build);
// otherwise "dev+<commit>" from the VCS stamp that `go build` and `ko build`
// record in a git checkout, with "-dirty" for uncommitted changes; otherwise
// "dev".
func buildVersion() string {
	return resolveVersion(version, debug.ReadBuildInfo)
}

func resolveVersion(ldflags string, read func() (*debug.BuildInfo, bool)) string {
	if ldflags != "" && ldflags != "dev" {
		return ldflags
	}
	bi, ok := read()
	if !ok {
		return "dev"
	}
	if v := bi.Main.Version; v != "" && v != "(devel)" {
		return v
	}
	var revision string
	var dirty bool
	for _, s := range bi.Settings {
		switch s.Key {
		case "vcs.revision":
			revision = s.Value
		case "vcs.modified":
			dirty = s.Value == "true"
		}
	}
	if revision == "" {
		return "dev"
	}
	if len(revision) > 12 {
		revision = revision[:12]
	}
	v := "dev+" + revision
	if dirty {
		v += "-dirty"
	}
	return v
}
