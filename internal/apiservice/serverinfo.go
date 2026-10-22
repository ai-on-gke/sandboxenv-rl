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

package apiservice

import (
	"context"
	"runtime/debug"

	ateenvv1alpha "github.com/agent-substrate/env/proto/ateenv/v1alpha"
)

// ServerName is the server implementation GetServerInfo reports. Another
// ate-env-api build that serves the same API under another name lets a client
// tell the two apart.
const ServerName = "sandboxenv-rl"

// Optional features GetServerInfo reports. A client checks for a name before
// it depends on the feature and ignores names it does not know.
const (
	// FeatureRootfsImage: CreateEnvironment accepts rootfs_image.
	FeatureRootfsImage = "rootfs_image"
	// FeaturePTY: StartProcess accepts tty, and ResizeProcess is served.
	FeaturePTY = "pty"
	// FeaturePortProxy: the listener forwards HTTP to ports in environments.
	FeaturePortProxy = "port_proxy"
	// FeatureBearerAuth: callers must send the API's bearer token.
	FeatureBearerAuth = "bearer_auth"
)

const substrateModule = "go.ate.dev/substrate"

// ServerInfo is what the binary tells the Server about its build and
// configuration, for GetServerInfo.
type ServerInfo struct {
	// Version is the build version of the binary; empty reports "dev".
	Version string
	// PortProxy is true when the listener serves the port proxy.
	PortProxy bool
	// BearerAuth is true when callers must send a bearer token.
	BearerAuth bool
}

// GetServerInfo reports the server implementation, its version, the Substrate
// module it was built against and its optional features.
func (s *Server) GetServerInfo(ctx context.Context, req *ateenvv1alpha.GetServerInfoRequest) (*ateenvv1alpha.GetServerInfoResponse, error) {
	version := s.Info.Version
	if version == "" {
		version = "dev"
	}
	return &ateenvv1alpha.GetServerInfoResponse{
		Server:           ServerName,
		Version:          version,
		SubstrateVersion: substrateVersion(debug.ReadBuildInfo),
		Features:         s.features(),
	}, nil
}

// features lists the optional features this server serves, in a fixed order.
func (s *Server) features() []string {
	f := []string{FeatureRootfsImage, FeaturePTY}
	if s.Info.PortProxy {
		f = append(f, FeaturePortProxy)
	}
	if s.Info.BearerAuth {
		f = append(f, FeatureBearerAuth)
	}
	return f
}

// substrateVersion returns the version of the Substrate module in the binary's
// build information (its replacement's, when replaced), or "unknown".
func substrateVersion(read func() (*debug.BuildInfo, bool)) string {
	bi, ok := read()
	if !ok {
		return "unknown"
	}
	for _, dep := range bi.Deps {
		if dep.Path != substrateModule {
			continue
		}
		if dep.Replace != nil && dep.Replace.Version != "" {
			return dep.Replace.Version
		}
		if dep.Version != "" {
			return dep.Version
		}
	}
	return "unknown"
}
