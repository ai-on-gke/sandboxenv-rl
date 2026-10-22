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
	"testing"

	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/types/known/structpb"
)

// The API messages have no repeated or map fields of messages today; these
// cover the list and map branches of the walk with a well-known type.
func TestFindUnknownInListsAndMaps(t *testing.T) {
	unknown := protowire.AppendVarint(protowire.AppendTag(nil, 77, protowire.VarintType), 1)

	inList := structpb.NewNumberValue(1)
	inList.ProtoReflect().SetUnknown(unknown)
	list := &structpb.ListValue{Values: []*structpb.Value{structpb.NewNullValue(), inList}}
	if where, nums := findUnknown(list.ProtoReflect(), "ListValue"); where != "ListValue.values[1]" || nums != "77" {
		t.Errorf("list: where=%q nums=%q, want ListValue.values[1] and 77", where, nums)
	}

	inMap := structpb.NewStringValue("x")
	inMap.ProtoReflect().SetUnknown(unknown)
	s := &structpb.Struct{Fields: map[string]*structpb.Value{"k": inMap}}
	if where, nums := findUnknown(s.ProtoReflect(), "Struct"); where != "Struct.fields[k]" || nums != "77" {
		t.Errorf("map: where=%q nums=%q, want Struct.fields[k] and 77", where, nums)
	}

	clean := &structpb.Struct{Fields: map[string]*structpb.Value{"k": structpb.NewBoolValue(true)}}
	if where, _ := findUnknown(clean.ProtoReflect(), "Struct"); where != "" {
		t.Errorf("clean message reported unknown fields at %q", where)
	}

	if got := unknownNumbers(append(append([]byte{}, unknown...), protowire.AppendVarint(protowire.AppendTag(nil, 4, protowire.VarintType), 2)...)); got != "77, 4" {
		t.Errorf("unknownNumbers = %q, want \"77, 4\"", got)
	}
}
