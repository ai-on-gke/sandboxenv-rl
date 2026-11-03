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

// Package measure harvests Prometheus telemetry, node image-cache samples,
// and structured atelet restore timing logs into W9 Measurements artifacts.
package measure

import (
	"bufio"
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/agent-substrate/env/internal/benchmarking/schema"
	"github.com/agent-substrate/env/internal/benchmarking/swebench"
	"sigs.k8s.io/yaml"
)

//go:embed queries.yaml
var defaultQueriesYAML []byte

// ErrMetricMissing is returned when a required Prometheus query returns no series
// or a missing node sample prevents computing a valid delta (Guardrail 3).
var ErrMetricMissing = errors.New("measure: required metric missing")

// PromQuery specifies a named Prometheus instant or range query.
type PromQuery struct {
	Name     string        `json:"name" yaml:"name"`
	Expr     string        `json:"expr" yaml:"expr"`
	Required bool          `json:"required,omitempty" yaml:"required,omitempty"`
	Range    bool          `json:"range,omitempty" yaml:"range,omitempty"`
	Start    time.Time     `json:"start,omitempty" yaml:"start,omitempty"`
	End      time.Time     `json:"end,omitempty" yaml:"end,omitempty"`
	Step     time.Duration `json:"step,omitempty" yaml:"step,omitempty"`
}

type promQueryCatalog struct {
	Queries []PromQuery `json:"queries" yaml:"queries"`
}

// DefaultPromQueries parses and returns the embedded queries.yaml catalog.
func DefaultPromQueries() ([]PromQuery, error) {
	var cat promQueryCatalog
	if err := yaml.UnmarshalStrict(defaultQueriesYAML, &cat); err != nil {
		return nil, fmt.Errorf("measure: parse embedded queries.yaml: %w", err)
	}
	if len(cat.Queries) == 0 {
		return nil, errors.New("measure: embedded queries.yaml contains no queries")
	}
	return cat.Queries, nil
}

// MustDefaultPromQueries returns the embedded queries.yaml catalog or panics if malformed.
func MustDefaultPromQueries() []PromQuery {
	qs, err := DefaultPromQueries()
	if err != nil {
		panic(err)
	}
	return qs
}

// PrometheusScraper queries a Prometheus HTTP API while enforcing Guardrail 3
// (distinguishing measured 0 from missing metrics or failed scrapes).
type PrometheusScraper struct {
	BaseURL string
	Client  *http.Client
}

type promResponse struct {
	Status    string   `json:"status"`
	ErrorType string   `json:"errorType,omitempty"`
	Error     string   `json:"error,omitempty"`
	Data      promData `json:"data"`
}

type promData struct {
	ResultType string       `json:"resultType"`
	Result     []promSample `json:"result"`
}

type promSample struct {
	Metric map[string]string `json:"metric"`
	Value  []any             `json:"value,omitempty"`
	Values [][]any           `json:"values,omitempty"`
}

// Query executes a single instant or range Prometheus query.
func (s *PrometheusScraper) Query(ctx context.Context, q PromQuery) ([]schema.PrometheusSeries, error) {
	if strings.TrimSpace(s.BaseURL) == "" {
		return nil, errors.New("measure: prometheus base url is required")
	}
	if strings.TrimSpace(q.Name) == "" || strings.TrimSpace(q.Expr) == "" {
		return nil, errors.New("measure: query name and expr are required")
	}
	client := s.Client
	if client == nil {
		client = http.DefaultClient
	}

	endpoint := "/api/v1/query"
	params := url.Values{}
	params.Set("query", q.Expr)
	if q.Range {
		endpoint = "/api/v1/query_range"
		params.Set("start", strconv.FormatFloat(float64(q.Start.UnixNano())/1e9, 'f', -1, 64))
		params.Set("end", strconv.FormatFloat(float64(q.End.UnixNano())/1e9, 'f', -1, 64))
		step := q.Step
		if step <= 0 {
			step = 15 * time.Second
		}
		params.Set("step", strconv.FormatFloat(step.Seconds(), 'f', -1, 64))
	} else if !q.End.IsZero() {
		params.Set("time", strconv.FormatFloat(float64(q.End.UnixNano())/1e9, 'f', -1, 64))
	}

	reqURL := strings.TrimRight(s.BaseURL, "/") + endpoint + "?" + params.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, nil)
	if err != nil {
		return nil, fmt.Errorf("measure: build request for %q: %w", q.Name, err)
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("measure: scrape %q failed: %w", q.Name, err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("measure: read response for %q: %w", q.Name, err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("measure: scrape %q returned HTTP %d: %s", q.Name, resp.StatusCode, strings.TrimSpace(string(body)))
	}

	var pr promResponse
	if err := json.Unmarshal(body, &pr); err != nil {
		return nil, fmt.Errorf("measure: decode response for %q: %w", q.Name, err)
	}
	if pr.Status != "success" {
		return nil, fmt.Errorf("measure: prometheus error for %q (%s): %s", q.Name, pr.ErrorType, pr.Error)
	}
	if len(pr.Data.Result) == 0 {
		if q.Required {
			return nil, fmt.Errorf("%w: query %q (%s) returned empty result", ErrMetricMissing, q.Name, q.Expr)
		}
		return nil, nil
	}

	series := make([]schema.PrometheusSeries, 0, len(pr.Data.Result))
	for i, item := range pr.Data.Result {
		labels := make(map[string]string, len(item.Metric))
		for k, v := range item.Metric {
			if k == "__name__" {
				continue
			}
			labels[k] = v
		}
		if len(labels) == 0 {
			labels = nil
		}
		ps := schema.PrometheusSeries{
			Name:   q.Name,
			Labels: labels,
			Status: "ok",
		}
		if len(item.Values) > 0 {
			vals := make([]float64, 0, len(item.Values))
			for _, pair := range item.Values {
				v, err := parsePromTupleValue(pair)
				if err != nil {
					return nil, fmt.Errorf("measure: query %q result[%d]: %w", q.Name, i, err)
				}
				vals = append(vals, v)
			}
			last := vals[len(vals)-1]
			ps.Values = vals
			ps.Value = &last
		} else if len(item.Value) > 0 {
			v, err := parsePromTupleValue(item.Value)
			if err != nil {
				return nil, fmt.Errorf("measure: query %q result[%d]: %w", q.Name, i, err)
			}
			ps.Value = &v
		} else {
			return nil, fmt.Errorf("%w: query %q result[%d] has no value", ErrMetricMissing, q.Name, i)
		}
		if err := ps.Validate(); err != nil {
			return nil, fmt.Errorf("measure: query %q result[%d]: %w", q.Name, i, err)
		}
		series = append(series, ps)
	}
	return series, nil
}

// ScrapeAll executes all configured queries in order and fails immediately on any error.
func (s *PrometheusScraper) ScrapeAll(ctx context.Context, queries []PromQuery) ([]schema.PrometheusSeries, error) {
	var all []schema.PrometheusSeries
	for _, q := range queries {
		res, err := s.Query(ctx, q)
		if err != nil {
			return nil, err
		}
		all = append(all, res...)
	}
	return all, nil
}

func parsePromTupleValue(tuple []any) (float64, error) {
	if len(tuple) != 2 {
		return 0, fmt.Errorf("expected [timestamp, value] tuple, got %d elements", len(tuple))
	}
	strVal, ok := tuple[1].(string)
	if !ok {
		return 0, fmt.Errorf("expected string sample value, got %T", tuple[1])
	}
	val, err := strconv.ParseFloat(strVal, 64)
	if err != nil {
		return 0, fmt.Errorf("parse float %q: %w", strVal, err)
	}
	if math.IsNaN(val) || math.IsInf(val, 0) {
		return 0, fmt.Errorf("%w: non-finite sample value %q", ErrMetricMissing, strVal)
	}
	return val, nil
}

// ParsePromTextExposition parses Prometheus text exposition lines into PrometheusSeries records.
func ParsePromTextExposition(r io.Reader) ([]schema.PrometheusSeries, error) {
	sc := bufio.NewScanner(r)
	var out []schema.PrometheusSeries
	lineNum := 0
	for sc.Scan() {
		lineNum++
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		var namePart, rest string
		var labels map[string]string
		if openIdx := strings.IndexByte(line, '{'); openIdx != -1 {
			closeIdx := strings.IndexByte(line[openIdx:], '}')
			if closeIdx == -1 {
				return nil, fmt.Errorf("line %d: unclosed label brace", lineNum)
			}
			closeIdx += openIdx
			namePart = strings.TrimSpace(line[:openIdx])
			rawLabels := strings.TrimSpace(line[openIdx+1 : closeIdx])
			rest = strings.TrimSpace(line[closeIdx+1:])
			if rawLabels != "" {
				labels = make(map[string]string)
				for _, kv := range strings.Split(rawLabels, ",") {
					kv = strings.TrimSpace(kv)
					if kv == "" {
						continue
					}
					parts := strings.SplitN(kv, "=", 2)
					if len(parts) != 2 {
						return nil, fmt.Errorf("line %d: invalid label %q", lineNum, kv)
					}
					labels[strings.TrimSpace(parts[0])] = strings.Trim(strings.TrimSpace(parts[1]), `"`)
				}
			}
		} else {
			fields := strings.Fields(line)
			if len(fields) < 2 {
				return nil, fmt.Errorf("line %d: expected metric and value", lineNum)
			}
			namePart = fields[0]
			rest = fields[1]
		}
		valFields := strings.Fields(rest)
		if len(valFields) == 0 {
			return nil, fmt.Errorf("line %d: missing sample value", lineNum)
		}
		val, err := strconv.ParseFloat(valFields[0], 64)
		if err != nil || math.IsNaN(val) || math.IsInf(val, 0) {
			return nil, fmt.Errorf("line %d: invalid sample value %q", lineNum, valFields[0])
		}
		ps := schema.PrometheusSeries{Name: namePart, Labels: labels, Status: "ok", Value: &val}
		if err := ps.Validate(); err != nil {
			return nil, fmt.Errorf("line %d: %w", lineNum, err)
		}
		out = append(out, ps)
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("scan text exposition: %w", err)
	}
	return out, nil
}

// ParseNodeCacheSamples parses a NodeCacheArtifact JSON object, JSON array, or JSONL stream of NodeCacheSample records.
func ParseNodeCacheSamples(r io.Reader) ([]schema.NodeCacheSample, error) {
	data, err := io.ReadAll(r)
	if err != nil {
		return nil, fmt.Errorf("read node cache samples: %w", err)
	}
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 {
		return nil, errors.New("parse node cache samples: empty input")
	}
	if trimmed[0] == '[' {
		dec := json.NewDecoder(bytes.NewReader(trimmed))
		dec.DisallowUnknownFields()
		var samples []schema.NodeCacheSample
		if err := dec.Decode(&samples); err != nil {
			return nil, fmt.Errorf("decode node cache samples json: %w", err)
		}
		if dec.More() {
			return nil, errors.New("decode node cache samples json: unexpected trailing data")
		}
		for i, s := range samples {
			if err := s.Validate(); err != nil {
				return nil, fmt.Errorf("node_cache_sample[%d]: %w", i, err)
			}
		}
		return samples, nil
	}
	if trimmed[0] == '{' && !bytes.Contains(trimmed, []byte("\n{")) {
		if art, err := schema.DecodeJSONStrict[schema.NodeCacheArtifact](bytes.NewReader(trimmed)); err == nil {
			return art.Nodes, nil
		}
	}
	if trimmed[0] != '[' && trimmed[0] != '{' {
		return parsePromNodeCacheSamples(trimmed)
	}
	return schema.DecodeJSONLStrict[schema.NodeCacheSample](bytes.NewReader(trimmed))
}

func parsePromNodeCacheSamples(raw []byte) ([]schema.NodeCacheSample, error) {
	parsed, err := swebench.ParseNodeCacheSample(bytes.NewReader(raw))
	if err != nil {
		return nil, fmt.Errorf("prom node cache: %w", err)
	}
	if len(parsed.Nodes) == 0 {
		return nil, errors.New("prom node cache: no nodes found")
	}
	nodes := make([]string, 0, len(parsed.Nodes))
	for n := range parsed.Nodes {
		nodes = append(nodes, n)
	}
	sort.Strings(nodes)
	out := make([]schema.NodeCacheSample, 0, len(nodes))
	for _, node := range nodes {
		counts := parsed.Requests[node]
		errs := int64(math.Round(counts["error"]))
		out = append(out, schema.NodeCacheSample{
			Node:   node,
			Hits:   int64(math.Round(counts["hit"])),
			Misses: int64(math.Round(counts["miss"])),
			Errors: &errs,
		})
	}
	return out, nil
}

// WriteNodeCacheSamples writes validated NodeCacheSample records as a NodeCacheArtifact JSON file to path.
func WriteNodeCacheSamples(path string, samples []schema.NodeCacheSample) error {
	art := schema.NodeCacheArtifact{
		SchemaVersion: schema.SchemaVersion,
		RunID:         "cache-snapshot",
		CollectedAt:   time.Now().UTC(),
		Nodes:         samples,
	}
	if err := art.Validate(); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("mkdir for %s: %w", path, err)
	}
	b, err := json.MarshalIndent(art, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal node cache samples: %w", err)
	}
	return os.WriteFile(path, append(b, '\n'), 0o644)
}

// ComputeCacheDelta computes cluster-wide cache hits, misses, and hit rate across before/after node samples.
func ComputeCacheDelta(before, after []schema.NodeCacheSample) (hits int64, misses int64, hitRate float64, err error) {
	if len(before) == 0 || len(after) == 0 {
		return 0, 0, 0, fmt.Errorf("%w: before (%d) and after (%d) node cache samples are required", ErrMetricMissing, len(before), len(after))
	}
	beforeByNode := make(map[string]schema.NodeCacheSample, len(before))
	for _, s := range before {
		if err := s.Validate(); err != nil {
			return 0, 0, 0, fmt.Errorf("before sample: %w", err)
		}
		beforeByNode[s.Node] = s
	}
	for _, a := range after {
		if err := a.Validate(); err != nil {
			return 0, 0, 0, fmt.Errorf("after sample: %w", err)
		}
		b, ok := beforeByNode[a.Node]
		if !ok {
			return 0, 0, 0, fmt.Errorf("%w: node %q missing from before cache sample", ErrMetricMissing, a.Node)
		}
		dh := a.Hits - b.Hits
		dm := a.Misses - b.Misses
		if dh < 0 || dm < 0 {
			return 0, 0, 0, fmt.Errorf("node %q: negative counter delta (hits=%d, misses=%d) indicates daemon restart", a.Node, dh, dm)
		}
		hits += dh
		misses += dm
	}
	total := hits + misses
	if total > 0 {
		hitRate = float64(hits) / float64(total)
	}
	return hits, misses, hitRate, nil
}

// ParseRestoreTimings strictly decodes newline-delimited RestoreSample records from restore_timings.jsonl.
func ParseRestoreTimings(r io.Reader) ([]schema.RestoreSample, error) {
	return schema.DecodeJSONLStrict[schema.RestoreSample](r)
}

// ValidateRestoreCount verifies that harvested restore log samples match expected client restore counts.
func ValidateRestoreCount(samples []schema.RestoreSample, minExpected int) error {
	if minExpected < 0 {
		return fmt.Errorf("minExpected must be >= 0, got %d", minExpected)
	}
	if len(samples) < minExpected {
		return fmt.Errorf("%w: harvested %d restore timing records, expected at least %d", ErrMetricMissing, len(samples), minExpected)
	}
	return nil
}

// CollectOptions configures measurement harvesting for a benchmark run.
type CollectOptions struct {
	RunID             string
	CollectedAt       time.Time
	Scraper           *PrometheusScraper
	Queries           []PromQuery
	CacheBeforePath   string
	CacheAfterPath    string
	RestoreTimingPath string
	MinRestores       int
}

// CollectMeasurements harvests Prometheus metrics, node cache samples, and restore timings into schema.Measurements.
func CollectMeasurements(ctx context.Context, opts CollectOptions) (*schema.Measurements, error) {
	res, err := RunCollectors(ctx, CollectRequest{
		RunID:             opts.RunID,
		CollectedAt:       opts.CollectedAt,
		Scopes:            []schema.TelemetryScope{schema.ScopeSubstrate},
		EnabledCollectors: []string{"prometheus", "node-cache", "restore-timings"},
		Scraper:           opts.Scraper,
		Queries:           opts.Queries,
		CacheBeforePath:   opts.CacheBeforePath,
		CacheAfterPath:    opts.CacheAfterPath,
		RestoreTimingPath: opts.RestoreTimingPath,
		MinRestores:       opts.MinRestores,
	})
	if err != nil {
		return nil, err
	}
	return res.Measurements, nil
}

// WriteMeasurements validates and writes Measurements as formatted JSON to path.
func WriteMeasurements(path string, m *schema.Measurements) error {
	if m == nil {
		return errors.New("measure: nil measurements")
	}
	if err := m.Validate(); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("mkdir for %s: %w", path, err)
	}
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal measurements: %w", err)
	}
	return os.WriteFile(path, append(b, '\n'), 0o644)
}
