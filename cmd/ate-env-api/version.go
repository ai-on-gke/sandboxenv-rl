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
// when set; otherwise the main module version that the Go toolchain records.
// Since Go 1.24, `go build` and `ko build` in a git checkout record a
// pseudo-version (vX.Y.Z-0.<UTC time>-<12 hex digits of the commit>, with
// "+dirty" for uncommitted changes), and `go install ...@vX` records vX. A
// build without version information (-buildvcs=false, or no git checkout)
// records "(devel)", which reports "dev".
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
	return "dev"
}
