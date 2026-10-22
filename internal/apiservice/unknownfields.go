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
	"fmt"
	"strings"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
)

// proto3 keeps fields it does not know as unknown bytes and ignores them. A
// client built from another version or fork of the API can thus send a field
// this server does not implement (another fork's CreateEnvironmentRequest can
// carry an image to run, for example) and get a different environment than it
// asked for, with no error. The interceptors below turn that into
// InvalidArgument for each request message the API decodes, at any depth.

// RejectUnknownFieldsUnaryInterceptor rejects a unary request that carries
// fields this server does not know.
func RejectUnknownFieldsUnaryInterceptor() grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		if err := checkUnknownFields(info.FullMethod, req); err != nil {
			return nil, err
		}
		return handler(ctx, req)
	}
}

// RejectUnknownFieldsStreamInterceptor rejects each streamed request message
// that carries fields this server does not know, including the single request
// of a server-streaming call.
func RejectUnknownFieldsStreamInterceptor() grpc.StreamServerInterceptor {
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
		"%s: %s carries field numbers %s that this server does not know; the client is newer, older or from another fork than the server",
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
