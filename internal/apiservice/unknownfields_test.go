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

package apiservice_test

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/agent-substrate/env/guest"
	ateenvv1alpha "github.com/agent-substrate/env/proto/ateenv/v1alpha"
	"go.ate.dev/substrate/pkg/proto/ateapipb"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"
)

// withUnknown sets unknown field num (a varint) on m, as a client built from
// another version or fork of the API sends it.
func withUnknown[M proto.Message](m M, num protowire.Number) M {
	raw := protowire.AppendTag(nil, num, protowire.VarintType)
	raw = protowire.AppendVarint(raw, 1)
	m.ProtoReflect().SetUnknown(raw)
	return m
}

func wantUnknownFields(t *testing.T, err error, want ...string) {
	t.Helper()
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("err = %v, want InvalidArgument", err)
	}
	for _, w := range want {
		if !strings.Contains(status.Convert(err).Message(), w) {
			t.Errorf("message %q does not contain %q", status.Convert(err).Message(), w)
		}
	}
}

func TestCreateEnvironmentRejectsUnknownFields(t *testing.T) {
	te := newFullTestEnv(t)
	ctx := context.Background()

	for _, num := range []protowire.Number{5, 99} {
		req := withUnknown(&ateenvv1alpha.CreateEnvironmentRequest{Id: "env-unknown"}, num)
		_, err := te.envClient.CreateEnvironment(ctx, req)
		wantUnknownFields(t, err, "CreateEnvironment", fmt.Sprintf("CreateEnvironmentRequest carries field numbers %d ", num))
	}
	if got := te.control.Status("env-unknown"); got != ateapipb.ActorState_ACTOR_STATE_UNSPECIFIED {
		t.Errorf("actor state = %v, want no actor: a rejected request must create nothing", got)
	}

	// The same request without the unknown field is served.
	if _, err := te.envClient.CreateEnvironment(ctx, &ateenvv1alpha.CreateEnvironmentRequest{Id: "env-unknown"}); err != nil {
		t.Fatalf("CreateEnvironment without unknown fields: %v", err)
	}
}

func TestCreateEnvironmentRejectsNestedUnknownFields(t *testing.T) {
	te := newFullTestEnv(t)
	req := &ateenvv1alpha.CreateEnvironmentRequest{
		Id:       "env-nested",
		Template: withUnknown(&ateenvv1alpha.Template{Name: "default-template"}, 7),
	}
	_, err := te.envClient.CreateEnvironment(context.Background(), req)
	wantUnknownFields(t, err, "CreateEnvironmentRequest.template carries field numbers 7")
}

func TestStreamsRejectUnknownFields(t *testing.T) {
	te := newFullTestEnv(t)
	ctx := context.Background()
	if _, err := te.envClient.CreateEnvironment(ctx, &ateenvv1alpha.CreateEnvironmentRequest{Id: "guest-unknown"}); err != nil {
		t.Fatalf("CreateEnvironment: %v", err)
	}
	grpcGuestServer, cleanup, err := guest.NewServer(guest.Config{
		Workspace:        t.TempDir(),
		EnableProcess:    true,
		EnableFileSystem: true,
	})
	if err != nil {
		t.Fatalf("guest.NewServer: %v", err)
	}
	t.Cleanup(cleanup)
	te.router.Register("guest-unknown", grpcGuestServer)
	envCtx := metadata.AppendToOutgoingContext(ctx, "x-env-id", "guest-unknown", "x-env-atespace", "default")

	// Client streaming: the first message is known, a later one is not.
	ws, err := te.fsClient.WriteFile(envCtx)
	if err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if err := ws.Send(&ateenvv1alpha.WriteFileRequest{Path: "f.txt", Mode: 0o644, Chunk: []byte("a")}); err != nil {
		t.Fatalf("send first: %v", err)
	}
	// The server can end the stream as soon as it reads the bad message, so
	// a send error here is expected; the status comes from CloseAndRecv.
	_ = ws.Send(withUnknown(&ateenvv1alpha.WriteFileRequest{Chunk: []byte("b")}, 42))
	_, err = ws.CloseAndRecv()
	wantUnknownFields(t, err, "/ateenv.v1alpha.FileSystemService/WriteFile", "WriteFileRequest carries field numbers 42")

	// Server streaming: the single request is checked too.
	rs, err := te.fsClient.ReadFile(envCtx, withUnknown(&ateenvv1alpha.ReadFileRequest{Path: "f.txt"}, 9))
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	_, err = rs.Recv()
	wantUnknownFields(t, err, "ReadFileRequest carries field numbers 9")

	// Known requests on the same streams still work.
	ws, err = te.fsClient.WriteFile(envCtx)
	if err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if err := ws.Send(&ateenvv1alpha.WriteFileRequest{Path: "g.txt", Mode: 0o644, Chunk: []byte("ok")}); err != nil {
		t.Fatalf("send: %v", err)
	}
	if resp, err := ws.CloseAndRecv(); err != nil || resp.GetBytesWritten() != 2 {
		t.Fatalf("WriteFile without unknown fields: %v, %v", resp, err)
	}
}
