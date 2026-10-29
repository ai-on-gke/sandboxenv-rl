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
	SetRPCSemconvModeForTest(t, semconvNew)
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
		if got["rpc.system.name"].AsString() != "grpc" || got["rpc.method"].AsString() != "s/M" {
			t.Errorf("rpcAttrs(%q) rpc.* = %v", tc.target, got)
		}
		if _, ok := got["rpc.system"]; ok {
			t.Errorf("rpcAttrs(%q) has the old rpc.system in the default mode", tc.target)
		}
		if _, ok := got["rpc.service"]; ok {
			t.Errorf("rpcAttrs(%q) has the old rpc.service in the default mode", tc.target)
		}
		if addr := got["server.address"].AsString(); addr != tc.addr {
			t.Errorf("rpcAttrs(%q) server.address = %q, want %q", tc.target, addr, tc.addr)
		}
		if port := int(got["server.port"].AsInt64()); port != tc.port {
			t.Errorf("rpcAttrs(%q) server.port = %d, want %d", tc.target, port, tc.port)
		}
	}
}

// parseRPCSemconvMode reads OTEL_SEMCONV_STABILITY_OPT_IN like otelgrpc
// v0.72.0's parseSemconvMode and the Python client's _rpc_semconv_mode (the
// same cases as test_rpc_semconv_mode_parses_like_otelgrpc).
func TestParseRPCSemconvModeLikeOtelgrpc(t *testing.T) {
	for _, tc := range []struct{ value, want string }{
		{"", semconvNew},
		{"http", semconvNew},
		{"rpc", semconvNew},
		{"rpc/old", semconvOld},
		{"rpc/dup", semconvDup},
		{" rpc/dup ", semconvDup},
		{"http, rpc/old", semconvOld},
		{"http,rpc/dup", semconvDup},
		{"rpc/old,rpc/dup", semconvOld}, // the first rpc/* item decides
		{"rpc/dup,rpc/old", semconvDup},
		{"RPC/OLD", semconvNew}, // case-sensitive, as in otelgrpc
	} {
		if got := parseRPCSemconvMode(tc.value); got != tc.want {
			t.Errorf("parseRPCSemconvMode(%q) = %q, want %q", tc.value, got, tc.want)
		}
	}
}

// rpcAttrs emits the rpc.* keys of each convention, as the Python client's
// rpc_attrs does (test_rpc_attrs_follow_the_semconv_mode).
func TestRPCAttrsFollowTheSemconvMode(t *testing.T) {
	const svc = "ateenv.v1alpha.EnvironmentService"
	for _, tc := range []struct {
		mode string
		want map[string]string
	}{
		{semconvNew, map[string]string{"rpc.system.name": "grpc", "rpc.method": svc + "/GetEnvironment"}},
		{semconvOld, map[string]string{"rpc.system": "grpc", "rpc.service": svc, "rpc.method": "GetEnvironment"}},
		// Both sets; rpc.method is the full name, the value otelgrpc keeps.
		{semconvDup, map[string]string{"rpc.system": "grpc", "rpc.service": svc, "rpc.system.name": "grpc", "rpc.method": svc + "/GetEnvironment"}},
	} {
		t.Run(tc.mode, func(t *testing.T) {
			SetRPCSemconvModeForTest(t, tc.mode)
			got := map[string]string{}
			for _, kv := range rpcAttrs(svc, "GetEnvironment", "") {
				got[string(kv.Key)] = kv.Value.AsString()
			}
			if len(got) != len(tc.want) {
				t.Errorf("rpcAttrs keys = %v, want %v", got, tc.want)
			}
			for k, v := range tc.want {
				if got[k] != v {
					t.Errorf("rpcAttrs[%s] = %q, want %q (all: %v)", k, got[k], v, got)
				}
			}
		})
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
