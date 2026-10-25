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
	"reflect"
	"testing"
)

func TestPortProxyAtespaces(t *testing.T) {
	for in, want := range map[string][]string{
		"ate-env":            {"ate-env"},
		" ate-env , team-a ": {"ate-env", "team-a"},
		"*":                  nil,
		" * ":                nil,
	} {
		if got, err := portProxyAtespaces(in); err != nil || !reflect.DeepEqual(got, want) {
			t.Errorf("portProxyAtespaces(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	// An empty value must not mean "any atespace", and "*" cannot be combined with names.
	for _, in := range []string{"", " ", ",", " , ", "ate-env,*", "*,ate-env"} {
		if got, err := portProxyAtespaces(in); err == nil {
			t.Errorf("portProxyAtespaces(%q) = %q, want an error", in, got)
		}
	}
}

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
