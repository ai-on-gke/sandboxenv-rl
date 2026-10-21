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

package env

import (
	"testing"

	"go.opentelemetry.io/otel/attribute"
)

// imageRefAttr agrees with the Python client's image_ref_attr (the want
// column is its output): the tag is dropped, the digest kept.
func TestImageRefAttrMatchesPythonClient(t *testing.T) {
	for _, tc := range []struct{ ref, want string }{
		{"registry.example/swe/repo-a@sha256:abababababababababababababababababababababababababababababababab", "registry.example/swe/repo-a@sha256:abababababababababababababababababababababababababababababababab"},
		{"registry.example/swe/repo-a:v1", "registry.example/swe/repo-a"},
		{"registry.example/swe/repo-a:v1@sha256:abababababababababababababababababababababababababababababababab", "registry.example/swe/repo-a@sha256:abababababababababababababababababababababababababababababababab"},
		{"host:5000/img", "host:5000/img"},
		{"host:5000/img:tag", "host:5000/img"},
		{"img", "img"},
		{"img:latest", "img"},
		{"", ""},
	} {
		if got := imageRefAttr(tc.ref); got != tc.want {
			t.Errorf("imageRefAttr(%q) = %q, want %q", tc.ref, got, tc.want)
		}
	}
}

// rpcAttrs has the Python client's rpc_attrs keys and server.* values.
func TestRPCAttrsMatchPythonClient(t *testing.T) {
	for _, tc := range []struct {
		target, addr string
		port         int
	}{
		{"127.0.0.1:7777", "127.0.0.1", 7777},
		{"[::1]:7777", "::1", 7777},
		{"localhost:7777", "localhost", 7777},
		{"unix:///tmp/sock", "unix:///tmp/sock", 0},
		{"api.example", "api.example", 0},
		{"", "", 0},
		{"dns:///127.0.0.1:7777", "127.0.0.1", 7777},         // Go only: gRPC resolver scheme
		{"passthrough:///localhost:7777", "localhost", 7777}, // Go only
	} {
		got := map[attribute.Key]attribute.Value{}
		for _, kv := range rpcAttrs("s", "M", tc.target) {
			got[kv.Key] = kv.Value
		}
		if got["rpc.system"].AsString() != "grpc" || got["rpc.service"].AsString() != "s" || got["rpc.method"].AsString() != "M" {
			t.Errorf("rpcAttrs(%q) rpc.* = %v", tc.target, got)
		}
		if addr := got["server.address"].AsString(); addr != tc.addr {
			t.Errorf("rpcAttrs(%q) server.address = %q, want %q", tc.target, addr, tc.addr)
		}
		if port := int(got["server.port"].AsInt64()); port != tc.port {
			t.Errorf("rpcAttrs(%q) server.port = %d, want %d", tc.target, port, tc.port)
		}
	}
}

// Empty string values are dropped, other values are kept.
func TestNonEmptyDropsEmptyStrings(t *testing.T) {
	got := nonEmpty([]attribute.KeyValue{
		AttrAtespace.String(""), AttrActorName.String("e1"), AttrExitCode.Int(0), AttrRootfsImage.String(""),
	})
	if len(got) != 2 || got[0].Key != AttrActorName || got[1].Key != AttrExitCode {
		t.Errorf("nonEmpty = %v, want ate.actor.name and ate_env.exit_code", got)
	}
}
