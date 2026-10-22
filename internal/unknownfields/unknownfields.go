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

// Package unknownfields rejects gRPC requests that carry fields the server
// does not know.
//
// proto3 keeps fields it does not know as unknown bytes and ignores them. A
// sender built from another version or fork of the API can thus set a field
// this server does not implement (another fork's CreateEnvironmentRequest can
// carry an image to run, for example) and get a different result than it
// asked for, with no error. The interceptors turn that into InvalidArgument
// for each request message the server decodes, at any depth. Both ate-env-api
// and the guest install them.
//
// A renumbered field is not caught: when sender and server both know field N
// with different meanings, nothing is unknown, and a field of the same type
// is read as the wrong thing. Only agreement on the version catches that.
//
// Cost: the walk visits only the fields that are set. The high-rate stream
// messages (file writes, process input) carry scalar payloads, thus each
// chunk costs a few field lookups.
package unknownfields

import (
	"context"
	"fmt"
	"strings"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
)

// UnaryServerInterceptor rejects a unary request that carries fields this
// server does not know.
func UnaryServerInterceptor() grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		if err := checkUnknownFields(info.FullMethod, req); err != nil {
			return nil, err
		}
		return handler(ctx, req)
	}
}

// StreamServerInterceptor rejects each streamed request message that carries
// fields this server does not know, including the single request of a
// server-streaming call. A rejection in a later message of a client stream
// ends the call, but it does not undo what the handler already did with the
// earlier messages.
func StreamServerInterceptor() grpc.StreamServerInterceptor {
	return func(srv any, ss grpc.ServerStream, info *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
		return handler(srv, &unknownFieldsStream{ServerStream: ss, method: info.FullMethod})
	}
}

type unknownFieldsStream struct {
	grpc.ServerStream
	method string
}

func (s *unknownFieldsStream) RecvMsg(m any) error {
	if err := s.ServerStream.RecvMsg(m); err != nil {
		return err
	}
	return checkUnknownFields(s.method, m)
}

func checkUnknownFields(method string, m any) error {
	msg, ok := m.(proto.Message)
	if !ok {
		return nil
	}
	r := msg.ProtoReflect()
	where, nums := findUnknown(r, string(r.Descriptor().Name()))
	if where == "" {
		return nil
	}
	return status.Errorf(codes.InvalidArgument,
		"%s: %s carries field numbers %s that this server does not know; the sender is built from a newer, older or other version of the API than this server",
		method, where, nums)
}

// findUnknown returns the path of the first message under m that has unknown
// fields, and their field numbers, or "" when there are none.
func findUnknown(m protoreflect.Message, path string) (string, string) {
	if raw := m.GetUnknown(); len(raw) > 0 {
		return path, unknownNumbers(raw)
	}
	var where, nums string
	m.Range(func(fd protoreflect.FieldDescriptor, v protoreflect.Value) bool {
		child := path + "." + string(fd.Name())
		switch {
		case fd.IsList() && fd.Message() != nil:
			l := v.List()
			for i := 0; i < l.Len() && where == ""; i++ {
				where, nums = findUnknown(l.Get(i).Message(), fmt.Sprintf("%s[%d]", child, i))
			}
		case fd.IsMap() && fd.MapValue().Message() != nil:
			v.Map().Range(func(k protoreflect.MapKey, mv protoreflect.Value) bool {
				where, nums = findUnknown(mv.Message(), fmt.Sprintf("%s[%v]", child, k.Interface()))
				return where == ""
			})
		case fd.Message() != nil && !fd.IsMap():
			where, nums = findUnknown(v.Message(), child)
		}
		return where == ""
	})
	return where, nums
}

func unknownNumbers(raw []byte) string {
	var nums []string
	for len(raw) > 0 {
		num, _, n := protowire.ConsumeField(raw)
		if n < 0 {
			break
		}
		nums = append(nums, fmt.Sprint(num))
		raw = raw[n:]
	}
	return strings.Join(nums, ", ")
}
