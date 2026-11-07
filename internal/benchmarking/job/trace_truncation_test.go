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

package job

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/agent-substrate/env/internal/benchmarking/schema"
)

const (
	tornLineA = `{"schema_version":"v1","ts":"2026-04-19T12:00:00Z","run":"r","job":"j","step":1,"op":"create","env":"e1","dur_ms":100.0,"ok":true}`
	tornLineB = `{"schema_version":"v1","ts":"2026-04-19T12:00:01Z","run":"r","job":"j","step":1,"op":"exec","command":["true"],"env":"e1","dur_ms":50.0,"ok":true,"exit":0}`
	// A record cut mid-write: the writer got as far as the nested command
	// array. Note it does not end in `}` *or* in a complete token.
	tornTail = `{"schema_version":"v1","ts":"2026-04-19T12:00:02Z","run":"r","job":"j","step":1,"op":"exec","command":["bash","-lc","py`
)

func writeTrace(t *testing.T, lines ...string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "trace.jsonl")
	if err := os.WriteFile(p, []byte(strings.Join(lines, "\n")), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestParseTraceJSONLFileDropsTornTailOnly(t *testing.T) {
	p := writeTrace(t, tornLineA, tornLineB, tornTail)
	evs, st, err := ParseTraceJSONLFileWithStats(p, "j")
	if err != nil {
		t.Fatalf("torn tail must not fail the shard: %v", err)
	}
	if st.Records != 2 || st.TruncatedTail != 1 || st.TruncatedLine != 3 {
		t.Fatalf("stats = %+v, want 2 records, tail dropped at line 3", st)
	}
	if len(evs) == 0 {
		t.Fatal("expected events from the two intact records")
	}
	// The plain wrapper keeps working and keeps the same events.
	evs2, err := ParseTraceJSONLFile(p, "j")
	if err != nil || len(evs2) != len(evs) {
		t.Fatalf("ParseTraceJSONLFile: err=%v len=%d want %d", err, len(evs2), len(evs))
	}
}

func TestParseTraceJSONLFileTornTailWithTrailingNewlineAndBlankLines(t *testing.T) {
	p := writeTrace(t, tornLineA, tornTail, "", "   ", "")
	_, st, err := ParseTraceJSONLFileWithStats(p, "j")
	if err != nil {
		t.Fatalf("blank lines after a torn tail are still a tail: %v", err)
	}
	if st.Records != 1 || st.TruncatedTail != 1 || st.TruncatedLine != 2 {
		t.Fatalf("stats = %+v", st)
	}
}

func TestParseTraceJSONLFileRejectsMidFileCorruption(t *testing.T) {
	p := writeTrace(t, tornLineA, tornTail, tornLineB)
	_, _, err := ParseTraceJSONLFileWithStats(p, "j")
	if err == nil {
		t.Fatal("a malformed line followed by more records is corruption and must fail")
	}
	if !strings.Contains(err.Error(), "line 2") {
		t.Fatalf("error must name the line: %v", err)
	}
}

func TestParseTraceJSONLFileCleanFileHasNoTruncation(t *testing.T) {
	p := writeTrace(t, tornLineA, tornLineB, "")
	_, st, err := ParseTraceJSONLFileWithStats(p, "j")
	if err != nil || st.TruncatedTail != 0 || st.Records != 2 {
		t.Fatalf("err=%v stats=%+v", err, st)
	}
}

func TestIngestArtifactDirCountsTruncatedShardsAcrossLayouts(t *testing.T) {
	dir := t.TempDir()
	// traces/<job>/<pod>/ (trace_collection layout) plus a flat shard.
	podDir := filepath.Join(dir, "traces", "j", "worker-0")
	if err := os.MkdirAll(podDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(podDir, "trace.jsonl"), []byte(tornLineA+"\n"+tornTail), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "traces", "flat.jsonl"), []byte(tornLineB+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	res, err := IngestArtifactDir(IngestContext{
		DestDir: dir,
		Scopes:  []schema.TelemetryScope{schema.ScopeUserSandbox},
	})
	if err != nil {
		t.Fatalf("IngestArtifactDir: %v", err)
	}
	if res.TraceTruncatedLines != 1 {
		t.Fatalf("TraceTruncatedLines = %d, want 1", res.TraceTruncatedLines)
	}
	if len(res.TraceTruncatedFiles) != 1 || res.TraceTruncatedFiles[0] != filepath.Join("traces", "j", "worker-0", "trace.jsonl")+":2" {
		t.Fatalf("TraceTruncatedFiles = %v", res.TraceTruncatedFiles)
	}
	if len(res.TraceEvents) == 0 {
		t.Fatal("expected events from both the nested and the flat shard")
	}
}
