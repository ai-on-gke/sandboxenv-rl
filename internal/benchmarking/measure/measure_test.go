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

package measure

import (
	"bytes"
	"context"
	"errors"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/agent-substrate/env/internal/benchmarking/schema"
)

func i64(v int64) *int64 { return &v }

func TestPrometheusScraper_DistinguishesZeroFromMissingAndError(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/query", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query().Get("query")
		switch q {
		case "zero_errors":
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{
				"status": "success",
				"data": {
					"resultType": "vector",
					"result": [
						{"metric": {"__name__": "zero_errors", "node": "node-a"}, "value": [1700000000, "0"]}
					]
				}
			}`))
		case "positive_latency":
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{
				"status": "success",
				"data": {
					"resultType": "vector",
					"result": [
						{"metric": {"job": "atelet"}, "value": [1700000000, "42.5"]}
					]
				}
			}`))
		case "empty_series":
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"status": "success", "data": {"resultType": "vector", "result": []}}`))
		case "prom_error":
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"status": "error", "errorType": "bad_data", "error": "parse error"}`))
		case "http_500":
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(`internal error`))
		case "bad_json":
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{not-json`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	})
	mux.HandleFunc("/api/v1/query_range", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{
			"status": "success",
			"data": {
				"resultType": "matrix",
				"result": [
					{
						"metric": {"pool": "worker"},
						"values": [[1700000000, "12.5"], [1700000015, "0"]]
					}
				]
			}
		}`))
	})

	srv := httptest.NewServer(mux)
	defer srv.Close()

	scraper := &PrometheusScraper{BaseURL: srv.URL, Client: srv.Client()}
	ctx := context.Background()

	// 1. Measured 0 is preserved as Value == 0 without error.
	zeroRes, err := scraper.Query(ctx, PromQuery{Name: "router_parking_rejected", Expr: "zero_errors", Required: true})
	if err != nil {
		t.Fatalf("unexpected error on measured 0: %v", err)
	}
	if len(zeroRes) != 1 || zeroRes[0].Value == nil || *zeroRes[0].Value != 0 || zeroRes[0].Labels["node"] != "node-a" {
		t.Fatalf("unexpected zero series: %+v", zeroRes)
	}

	// 2. Positive metric value and range query.
	posRes, err := scraper.Query(ctx, PromQuery{Name: "route_duration_ms", Expr: "positive_latency", Required: true})
	if err != nil {
		t.Fatalf("unexpected error on positive series: %v", err)
	}
	if len(posRes) != 1 || posRes[0].Value == nil || *posRes[0].Value != 42.5 {
		t.Fatalf("unexpected positive series: %+v", posRes)
	}

	rangeRes, err := scraper.Query(ctx, PromQuery{
		Name:     "active_sandboxes",
		Expr:     "sandbox_active",
		Range:    true,
		Required: true,
		Start:    time.Unix(1700000000, 0),
		End:      time.Unix(1700000015, 0),
		Step:     15 * time.Second,
	})
	if err != nil {
		t.Fatalf("unexpected error on range query: %v", err)
	}
	if len(rangeRes) != 1 || len(rangeRes[0].Values) != 2 || rangeRes[0].Values[0] != 12.5 || rangeRes[0].Values[1] != 0 {
		t.Fatalf("unexpected range series: %+v", rangeRes)
	}

	// 3. Guardrail 3: empty result on Required query returns ErrMetricMissing.
	if _, err := scraper.Query(ctx, PromQuery{Name: "missing_req", Expr: "empty_series", Required: true}); !errors.Is(err, ErrMetricMissing) {
		t.Fatalf("expected ErrMetricMissing for empty required metric, got %v", err)
	}

	// 4. Optional query with empty result returns nil without error.
	optRes, err := scraper.Query(ctx, PromQuery{Name: "missing_opt", Expr: "empty_series", Required: false})
	if err != nil || len(optRes) != 0 {
		t.Fatalf("expected nil for empty optional metric, got %v, err=%v", optRes, err)
	}

	// 5. Guardrail 3: HTTP 500, status=="error", and malformed JSON fail explicitly.
	for _, tc := range []string{"prom_error", "http_500", "bad_json"} {
		if _, err := scraper.Query(ctx, PromQuery{Name: tc, Expr: tc, Required: false}); err == nil {
			t.Fatalf("expected scrape failure for %s, got nil", tc)
		}
	}
}

func TestNodeCacheSamplesAndDelta(t *testing.T) {
	dir := t.TempDir()
	beforePath := filepath.Join(dir, "cache_before.json")
	afterPath := filepath.Join(dir, "cache_after.json")

	before := []schema.NodeCacheSample{
		{Node: "node-1", Hits: 10, Misses: 2, Errors: i64(0), Evictions: i64(1), BytesUsed: i64(1024)},
		{Node: "node-2", Hits: 5, Misses: 5, Errors: i64(0), Evictions: i64(0), BytesUsed: i64(2048)},
	}
	after := []schema.NodeCacheSample{
		{Node: "node-1", Hits: 25, Misses: 5, Errors: i64(0), Evictions: i64(1), BytesUsed: i64(4096)},
		{Node: "node-2", Hits: 10, Misses: 7, Errors: i64(0), Evictions: i64(0), BytesUsed: i64(4096)},
	}

	if err := WriteNodeCacheSamples(beforePath, before); err != nil {
		t.Fatalf("WriteNodeCacheSamples before: %v", err)
	}
	if err := WriteNodeCacheSamples(afterPath, after); err != nil {
		t.Fatalf("WriteNodeCacheSamples after: %v", err)
	}

	rawBefore, err := os.ReadFile(beforePath)
	if err != nil {
		t.Fatalf("read before file: %v", err)
	}
	parsedBefore, err := ParseNodeCacheSamples(bytes.NewReader(rawBefore))
	if err != nil {
		t.Fatalf("ParseNodeCacheSamples: %v", err)
	}
	if len(parsedBefore) != 2 {
		t.Fatalf("expected 2 before samples, got %d", len(parsedBefore))
	}

	hits, misses, hitRate, err := ComputeCacheDelta(before, after)
	if err != nil {
		t.Fatalf("ComputeCacheDelta: %v", err)
	}
	// hits = (25-10) + (10-5) = 20; misses = (5-2) + (7-5) = 5; hitRate = 20/25 = 0.8
	if hits != 20 || misses != 5 || math.Abs(hitRate-0.8) > 1e-9 {
		t.Fatalf("unexpected delta: hits=%d misses=%d hitRate=%v", hits, misses, hitRate)
	}

	// Missing node in before sample must fail with ErrMetricMissing.
	_, _, _, err = ComputeCacheDelta(before[:1], after)
	if !errors.Is(err, ErrMetricMissing) {
		t.Fatalf("expected ErrMetricMissing when node is missing, got %v", err)
	}

	// Counter reset (negative delta) must fail.
	resetAfter := []schema.NodeCacheSample{
		{Node: "node-1", Hits: 2, Misses: 5},
		{Node: "node-2", Hits: 10, Misses: 7},
	}
	if _, _, _, err := ComputeCacheDelta(before, resetAfter); err == nil {
		t.Fatal("expected error on negative cache counter delta, got nil")
	}
}

func TestParseRestoreTimingsAndValidateCount(t *testing.T) {
	jsonl := strings.Join([]string{
		`{"node":"node-1","env":"env-1","snapshot":"snap-a","kind":"golden","total_ms":120.5,"manifest_fetch_ms":10,"download_ms":50,"oci_unpack_ms":30,"worker_restore_ms":30.5,"cache_hit":false}`,
		`{"node":"node-1","env":"env-2","snapshot":"snap-a","kind":"latest","total_ms":35.0,"worker_restore_ms":35.0,"cache_hit":true}`,
	}, "\n")

	samples, err := ParseRestoreTimings(strings.NewReader(jsonl))
	if err != nil {
		t.Fatalf("ParseRestoreTimings: %v", err)
	}
	if len(samples) != 2 || samples[1].CacheHit == nil || !*samples[1].CacheHit || samples[0].TotalMS != 120.5 {
		t.Fatalf("unexpected restore samples: %+v", samples)
	}
	if err := ValidateRestoreCount(samples, 2); err != nil {
		t.Fatalf("ValidateRestoreCount(2): %v", err)
	}
	if err := ValidateRestoreCount(samples, 3); !errors.Is(err, ErrMetricMissing) {
		t.Fatalf("expected ErrMetricMissing when restore count < minExpected, got %v", err)
	}

	textExp := "# HELP atenet_router_parking_rejected Rejected count\natenet_router_parking_rejected{node=\"node-1\"} 0\natenet_router_route_duration_ms 14.25\n"
	series, err := ParsePromTextExposition(strings.NewReader(textExp))
	if err != nil || len(series) != 2 || series[0].Value == nil || *series[0].Value != 0 || series[1].Value == nil || *series[1].Value != 14.25 {
		t.Fatalf("ParsePromTextExposition failed: series=%+v err=%v", series, err)
	}
}

func TestCollectAndWriteMeasurementsRoundTrip(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{
			"status": "success",
			"data": {
				"resultType": "vector",
				"result": [
					{"metric": {"node": "node-1"}, "value": [1700000000, "0"]}
				]
			}
		}`))
	}))
	defer srv.Close()

	dir := t.TempDir()
	beforePath := filepath.Join(dir, "cache_before.json")
	afterPath := filepath.Join(dir, "cache_after.json")
	restorePath := filepath.Join(dir, "restore_timings.jsonl")
	outPath := filepath.Join(dir, "measurements.json")

	if err := WriteNodeCacheSamples(beforePath, []schema.NodeCacheSample{{Node: "node-1", Hits: 1, Misses: 1}}); err != nil {
		t.Fatalf("write before: %v", err)
	}
	if err := WriteNodeCacheSamples(afterPath, []schema.NodeCacheSample{{Node: "node-1", Hits: 9, Misses: 3}}); err != nil {
		t.Fatalf("write after: %v", err)
	}
	if err := os.WriteFile(restorePath, []byte(`{"env":"env-1","total_ms":45.2,"cache_hit":true}`+"\n"), 0o644); err != nil {
		t.Fatalf("write restores: %v", err)
	}

	m, err := CollectMeasurements(context.Background(), CollectOptions{
		RunID:       "run-w9-test",
		CollectedAt: time.Date(2026, 10, 22, 2, 0, 0, 0, time.UTC),
		Scraper:     &PrometheusScraper{BaseURL: srv.URL, Client: srv.Client()},
		Queries: []PromQuery{
			{Name: "oom_kills", Expr: "kube_pod_container_status_terminated_reason{reason='OOMKilled'}", Required: true},
		},
		CacheBeforePath:   beforePath,
		CacheAfterPath:    afterPath,
		RestoreTimingPath: restorePath,
		MinRestores:       1,
	})
	if err != nil {
		t.Fatalf("CollectMeasurements: %v", err)
	}
	if err := WriteMeasurements(outPath, m); err != nil {
		t.Fatalf("WriteMeasurements: %v", err)
	}

	raw, err := os.ReadFile(outPath)
	if err != nil {
		t.Fatalf("read measurements.json: %v", err)
	}
	decoded, err := schema.DecodeJSONStrict[schema.Measurements](bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("DecodeJSONStrict[Measurements]: %v", err)
	}
	if decoded.RunID != "run-w9-test" || len(decoded.PromMetrics) != 1 || decoded.PromMetrics[0].Value == nil || *decoded.PromMetrics[0].Value != 0 || decoded.RestoreTimingsRef != "restore_timings.jsonl" || decoded.CacheBeforeRef != "cache_before.json" || decoded.CacheAfterRef != "cache_after.json" {
		t.Fatalf("unexpected round-tripped Measurements: %+v", decoded)
	}
	if err := schema.ValidateAgainstJSONSchema("measurements.schema.json", raw); err != nil {
		t.Fatalf("measurements.json failed JSON schema: %v", err)
	}
}

func TestDefaultPromQueriesIncludesFleetClientAndAPISeries(t *testing.T) {
	qs, err := DefaultPromQueries()
	if err != nil {
		t.Fatalf("DefaultPromQueries: %v", err)
	}
	byName := make(map[string]PromQuery, len(qs))
	for _, q := range qs {
		byName[q.Name] = q
	}
	for _, want := range []string{
		"up",
		"ate_env_fleet_phase_latency_seconds",
		"ate_env_fleet_release_failures_total",
		"ate_env_client_acquire_seconds",
		"ate_env_client_slot_wait_seconds",
		"ate_env_api_requests_total",
		"ate_env_api_request_duration_seconds",
		"ate_env_api_create_to_ready_seconds",
		"ate_env_api_wake_wait_seconds",
		"ate_env_api_environments",
		"ate_env_api_build_info",
	} {
		if _, ok := byName[want]; !ok {
			t.Errorf("embedded queries.yaml missing expected query %q", want)
		}
	}
	if !byName["up"].Required {
		t.Errorf("expected 'up' query to be Required=true")
	}
}
