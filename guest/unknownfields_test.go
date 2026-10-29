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

package guest_test

import (
	"context"
	"net"
	"strings"
	"testing"

	"github.com/agent-substrate/env/guest"
	ateenvv1alpha "github.com/agent-substrate/env/proto/ateenv/v1alpha"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"
)

func withUnknown[M proto.Message](m M, num protowire.Number) M {
	m.ProtoReflect().SetUnknown(protowire.AppendVarint(protowire.AppendTag(nil, num, protowire.VarintType), 1))
	return m
}

// The guest rejects fields it does not know, as ate-env-api does: an API newer
// than the guest baked into a golden snapshot can send one.
func TestGuestRejectsUnknownFields(t *testing.T) {
	srv, cleanup, err := guest.NewServer(guest.Config{Workspace: t.TempDir(), LogDir: t.TempDir(), EnableProcess: true, EnableFileSystem: true})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(cleanup)
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go srv.Serve(lis)
	t.Cleanup(srv.Stop)
	conn, err := grpc.NewClient(lis.Addr().String(), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	ctx := context.Background()

	wantRejected := func(err error, want string) {
		t.Helper()
		if status.Code(err) != codes.InvalidArgument || !strings.Contains(status.Convert(err).Message(), want) {
			t.Errorf("err = %v, want InvalidArgument containing %q", err, want)
		}
	}

	proc := ateenvv1alpha.NewProcessServiceClient(conn)
	_, err = proc.StartProcess(ctx, withUnknown(&ateenvv1alpha.StartProcessRequest{Command: []string{"true"}}, 50))
	wantRejected(err, "StartProcessRequest carries field numbers 50")

	ws, err := ateenvv1alpha.NewFileSystemServiceClient(conn).WriteFile(ctx)
	if err != nil {
		t.Fatal(err)
	}
	_ = ws.Send(withUnknown(&ateenvv1alpha.WriteFileRequest{Path: "f.txt", Chunk: []byte("a")}, 51))
	_, err = ws.CloseAndRecv()
	wantRejected(err, "WriteFileRequest carries field numbers 51")

	// Known requests are served.
	if _, err := proc.StartProcess(ctx, &ateenvv1alpha.StartProcessRequest{Command: []string{"true"}}); err != nil {
		t.Errorf("StartProcess without unknown fields: %v", err)
	}
}
