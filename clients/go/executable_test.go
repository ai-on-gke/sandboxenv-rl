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
	"strings"
	"testing"
)

// executable agrees with the Python client's executable_attr on every case
// (the want column is executable_attr's output, "" for None): leading
// NAME=value assignments are skipped so a secret in one is never recorded,
// the line is split with shell quoting, and a line that cannot be split
// records nothing.
func TestExecutableMatchesPythonClient(t *testing.T) {
	for _, tc := range []struct{ line, want string }{
		{"echo hi", "echo"},
		{"API_KEY=s3cr3t python x.py", "python"},
		{"X+=s3cr3t cmd", "cmd"},
		{"PATH+=:/opt/bin A=1 make", "make"},
		{"./a+=b", "a+=b"},
		{"A=1 B=2 /usr/bin/env python3 -c x", "env"},
		{"'my prog' arg", "my prog"},
		{"\"my prog\" arg", "my prog"},
		{"my\\ prog arg", "my prog"},
		{"/opt/bin/tool --flag", "tool"},
		{"'unterminated arg", ""},
		{"say \"unterminated", ""},
		{"trailing\\", ""},
		{"", ""},
		{"   ", ""},
		{"ONLY=assignments", ""},
		{"  ./run.sh  ", "run.sh"},
		{"dir/", ""},
		{"X=\"a b\" cmd y", "cmd"},
		{"pyth'on' x", "python"},
		{"a\"b\\\"c\"d e", "ab\"cd"},
		{"\tls\t-l", "ls"},
		{"python3 -c 'print(1)'", "python3"},
	} {
		if got := executable(tc.line); got != tc.want {
			t.Errorf("executable(%q) = %q, want %q", tc.line, got, tc.want)
		}
	}
}

// No part of a leading assignment reaches the attribute.
func TestExecutableNeverRecordsAssignments(t *testing.T) {
	for _, line := range []string{"API_KEY=s3cr3t python x.py", "TOKEN='s3 cr3t' run", "A=s3cr3t", "A=s3cr3t 'unterminated", "X+=s3cr3t cmd"} {
		got := executable(line)
		if strings.Contains(got, "s3") || strings.Contains(got, "=") {
			t.Errorf("executable(%q) = %q leaks the assignment", line, got)
		}
	}
}
