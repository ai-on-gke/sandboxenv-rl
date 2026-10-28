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
	"context"
	"net"
	"os"
	"slices"
	"testing"

	"github.com/agent-substrate/env/internal/apiservice"
	ateenvv1alpha "github.com/agent-substrate/env/proto/ateenv/v1alpha"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protowire"
)

func TestIsLoopbackAddr(t *testing.T) {
	cases := []struct {
		addr string
		want bool
	}{
		{"127.0.0.1:7777", true},
		{"localhost:7777", true},
		{"[::1]:7777", true},
		{"0.0.0.0:7777", false},
		{":7777", false},
		{"10.0.0.5:7777", false},
	}
	for _, tc := range cases {
		if got := isLoopbackAddr(tc.addr); got != tc.want {
			t.Errorf("isLoopbackAddr(%q) = %v, want %v", tc.addr, got, tc.want)
		}
	}
}

type stubEnvironments struct {
	ateenvv1alpha.UnimplementedEnvironmentServiceServer
}

func (stubEnvironments) GetEnvironment(context.Context, *ateenvv1alpha.GetEnvironmentRequest) (*ateenvv1alpha.GetEnvironmentResponse, error) {
	return &ateenvv1alpha.GetEnvironmentResponse{}, nil
}

// The API's interceptor chain authenticates before it checks for unknown
// fields, so an unauthenticated caller learns nothing from that check.
func TestGRPCServerOptionsAuthenticateBeforeUnknownFieldCheck(t *testing.T) {
	auth, err := apiservice.NewBearerAuth("tok")
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name    string
		auth    *apiservice.BearerAuth
		token   string
		unknown bool
		want    codes.Code
	}{
		{"no token, unknown field", auth, "", true, codes.Unauthenticated},
		{"token, unknown field", auth, "tok", true, codes.InvalidArgument},
		{"token, clean request", auth, "tok", false, codes.OK},
		{"auth off, unknown field", nil, "", true, codes.InvalidArgument},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := grpc.NewServer(grpcServerOptions(tc.auth, nil, false)...)
			ateenvv1alpha.RegisterEnvironmentServiceServer(srv, stubEnvironments{})
			lis, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			go srv.Serve(lis)
			defer srv.Stop()
			conn, err := grpc.NewClient(lis.Addr().String(), grpc.WithTransportCredentials(insecure.NewCredentials()))
			if err != nil {
				t.Fatal(err)
			}
			defer conn.Close()

			req := &ateenvv1alpha.GetEnvironmentRequest{Id: "e1"}
			if tc.unknown {
				req.ProtoReflect().SetUnknown(protowire.AppendVarint(protowire.AppendTag(nil, 99, protowire.VarintType), 1))
			}
			ctx := context.Background()
			if tc.token != "" {
				ctx = metadata.AppendToOutgoingContext(ctx, "authorization", "Bearer "+tc.token)
			}
			_, err = ateenvv1alpha.NewEnvironmentServiceClient(conn).GetEnvironment(ctx, req)
			if got := status.Code(err); got != tc.want {
				t.Errorf("code = %v (%v), want %v", got, err, tc.want)
			}
		})
	}
}

func TestServiceHostNames(t *testing.T) {
	files := func(m map[string]string) func(string) ([]byte, error) {
		return func(p string) ([]byte, error) {
			if v, ok := m[p]; ok {
				return []byte(v), nil
			}
			return nil, os.ErrNotExist
		}
	}
	env := func(m map[string]string) func(string) string { return func(k string) string { return m[k] } }
	kubelet := "search team-env.svc.corp.k8s svc.corp.k8s corp.k8s\nnameserver 10.0.0.10\n"

	for _, tc := range []struct {
		name    string
		service string
		files   map[string]string
		env     map[string]string
		want    []string
	}{
		{"resolv.conf gives namespace and domain", "ate-env-api", map[string]string{resolvConfPath: kubelet}, map[string]string{"POD_NAMESPACE": "other"},
			[]string{"ate-env-api", "ate-env-api.team-env", "ate-env-api.team-env.svc", "ate-env-api.team-env.svc.corp.k8s"}},
		{"POD_NAMESPACE without a cluster search list", "ate-env-api", map[string]string{resolvConfPath: "nameserver 1.1.1.1\n"}, map[string]string{"POD_NAMESPACE": "team-env"},
			[]string{"ate-env-api", "ate-env-api.team-env", "ate-env-api.team-env.svc"}},
		{"ServiceAccount namespace file", "ate-env-api", map[string]string{podNamespacePath: "team-env\n"}, nil,
			[]string{"ate-env-api", "ate-env-api.team-env", "ate-env-api.team-env.svc"}},
		{"outside a cluster", "ate-env-api", nil, nil, []string{"ate-env-api"}},
		{"disabled", "", map[string]string{resolvConfPath: kubelet}, nil, nil},
	} {
		got := serviceHostNames(tc.service, files(tc.files), env(tc.env))
		if !slices.Equal(got, tc.want) {
			t.Errorf("%s: %q, want %q", tc.name, got, tc.want)
		}
	}
}
