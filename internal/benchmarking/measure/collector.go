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
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/agent-substrate/env/internal/benchmarking/schema"
)

// CollectRequest configures a scoped and pluggable measurement collection pass.
type CollectRequest struct {
	RunID             string
	CollectedAt       time.Time
	DestDir           string
	Scopes            []schema.TelemetryScope
	EnabledCollectors []string
	Scraper           *PrometheusScraper
	Queries           []PromQuery
	CacheBeforePath   string
	CacheAfterPath    string
	RestoreTimingPath string
	MinRestores       int
	EnableK8s         bool
	K8s               K8sCollectOptions
	Plugins           []schema.TelemetryPluginSpec
}

// CollectResult holds the aggregated Measurements, SystemDescription, and written plugin files.
type CollectResult struct {
	Measurements      *schema.Measurements
	SystemDescription *schema.SystemDescription
	PluginFiles       map[string]string
}

// EnsureServerSummary initializes and returns out.Measurements.ServerSummary.
func (r *CollectResult) EnsureServerSummary() map[string]schema.MetricSummary {
	if r.Measurements.ServerSummary == nil {
		r.Measurements.ServerSummary = make(map[string]schema.MetricSummary)
	}
	return r.Measurements.ServerSummary
}

// EnsureCustomMetrics initializes and returns out.Measurements.CustomMetrics.
func (r *CollectResult) EnsureCustomMetrics() map[string]schema.MetricSummary {
	if r.Measurements.CustomMetrics == nil {
		r.Measurements.CustomMetrics = make(map[string]schema.MetricSummary)
	}
	return r.Measurements.CustomMetrics
}

// Collector harvests one aspect of server, Substrate, Kubernetes, or feature-plugin telemetry.
type Collector interface {
	Name() string
	Scope() schema.TelemetryScope
	Collect(ctx context.Context, req CollectRequest, out *CollectResult) error
}

var (
	collectorsMu      sync.RWMutex
	collectorRegistry = make(map[string]Collector)
	collectorOrder    []string
)

// RegisterCollector registers a Collector by its Name(). Re-registering an existing
// name replaces the implementation in-place.
func RegisterCollector(c Collector) {
	if c == nil {
		return
	}
	name := strings.TrimSpace(c.Name())
	if name == "" {
		return
	}
	collectorsMu.Lock()
	defer collectorsMu.Unlock()
	if _, exists := collectorRegistry[name]; !exists {
		collectorOrder = append(collectorOrder, name)
	}
	collectorRegistry[name] = c
}

// RegisteredCollectors returns all registered collectors in registration order.
func RegisteredCollectors() []Collector {
	collectorsMu.RLock()
	defer collectorsMu.RUnlock()
	out := make([]Collector, 0, len(collectorOrder))
	for _, name := range collectorOrder {
		if c, ok := collectorRegistry[name]; ok {
			out = append(out, c)
		}
	}
	return out
}

// CollectorsForScopes returns the registered collectors whose Scope() is enabled
// in scopes (and whose Name() is in enabledNames when enabledNames is non-empty).
func CollectorsForScopes(scopes []schema.TelemetryScope, enabledNames []string) []Collector {
	allowName := make(map[string]bool, len(enabledNames))
	for _, n := range enabledNames {
		for _, part := range strings.Split(n, ",") {
			if p := strings.TrimSpace(part); p != "" {
				allowName[p] = true
			}
		}
	}
	all := RegisteredCollectors()
	var selected []Collector
	for _, c := range all {
		if len(allowName) > 0 && !allowName[c.Name()] {
			continue
		}
		if schema.HasTelemetryScope(scopes, c.Scope()) {
			selected = append(selected, c)
		}
	}
	return selected
}

func init() {
	RegisterCollector(prometheusCollector{})
	RegisterCollector(nodeCacheCollector{})
	RegisterCollector(restoreTimingsCollector{})
	RegisterCollector(k8sHealthCollector{})
	RegisterCollector(substrateControlCollector{})
}

type prometheusCollector struct{}

func (prometheusCollector) Name() string                 { return "prometheus" }
func (prometheusCollector) Scope() schema.TelemetryScope { return schema.ScopeSubstrate }

func (prometheusCollector) Collect(ctx context.Context, req CollectRequest, out *CollectResult) error {
	if len(req.Queries) == 0 {
		return nil
	}
	if req.Scraper == nil {
		return errors.New("measure: scraper is required when queries are configured")
	}
	series, err := req.Scraper.ScrapeAll(ctx, req.Queries)
	if err != nil {
		return err
	}
	out.Measurements.PromMetrics = append(out.Measurements.PromMetrics, series...)
	return nil
}

type nodeCacheCollector struct{}

func (nodeCacheCollector) Name() string                 { return "node-cache" }
func (nodeCacheCollector) Scope() schema.TelemetryScope { return schema.ScopeSubstrate }

func (nodeCacheCollector) Collect(_ context.Context, req CollectRequest, out *CollectResult) error {
	var beforeSamples, afterSamples []schema.NodeCacheSample
	if req.CacheBeforePath != "" {
		f, err := os.Open(req.CacheBeforePath)
		if err != nil {
			return fmt.Errorf("open cache_before %s: %w", req.CacheBeforePath, err)
		}
		defer f.Close()
		samples, err := ParseNodeCacheSamples(f)
		if err != nil {
			return fmt.Errorf("parse cache_before: %w", err)
		}
		beforeSamples = samples
		out.Measurements.CacheBeforeRef = filepath.Base(req.CacheBeforePath)
	}
	if req.CacheAfterPath != "" {
		f, err := os.Open(req.CacheAfterPath)
		if err != nil {
			return fmt.Errorf("open cache_after %s: %w", req.CacheAfterPath, err)
		}
		defer f.Close()
		samples, err := ParseNodeCacheSamples(f)
		if err != nil {
			return fmt.Errorf("parse cache_after: %w", err)
		}
		afterSamples = samples
		out.Measurements.CacheAfterRef = filepath.Base(req.CacheAfterPath)
	}
	if len(beforeSamples) > 0 && len(afterSamples) > 0 {
		hits, misses, hitRate, err := ComputeCacheDelta(beforeSamples, afterSamples)
		if err != nil {
			return err
		}
		srv := out.EnsureServerSummary()
		srv["cache_hits"] = schema.MetricSummary{Count: len(afterSamples), Sum: float64(hits), Min: float64(hits), P50: float64(hits), P90: float64(hits), P95: float64(hits), P99: float64(hits), Max: float64(hits), Unit: "count"}
		srv["cache_misses"] = schema.MetricSummary{Count: len(afterSamples), Sum: float64(misses), Min: float64(misses), P50: float64(misses), P90: float64(misses), P95: float64(misses), P99: float64(misses), Max: float64(misses), Unit: "count"}
		srv["cache_hit_rate"] = schema.MetricSummary{Count: len(afterSamples), Sum: hitRate, Min: hitRate, P50: hitRate, P90: hitRate, P95: hitRate, P99: hitRate, Max: hitRate, Unit: "ratio"}
	}
	return nil
}

type restoreTimingsCollector struct{}

func (restoreTimingsCollector) Name() string                 { return "restore-timings" }
func (restoreTimingsCollector) Scope() schema.TelemetryScope { return schema.ScopeSubstrate }

func (restoreTimingsCollector) Collect(_ context.Context, req CollectRequest, out *CollectResult) error {
	if req.RestoreTimingPath == "" {
		if req.MinRestores > 0 {
			return fmt.Errorf("%w: min_restores=%d requested but restore_timing_path is empty", ErrMetricMissing, req.MinRestores)
		}
		return nil
	}
	f, err := os.Open(req.RestoreTimingPath)
	if err != nil {
		return fmt.Errorf("open restore_timings %s: %w", req.RestoreTimingPath, err)
	}
	defer f.Close()
	restores, err := ParseRestoreTimings(f)
	if err != nil {
		return fmt.Errorf("parse restore_timings: %w", err)
	}
	if err := ValidateRestoreCount(restores, req.MinRestores); err != nil {
		return err
	}
	out.Measurements.RestoreTimingsRef = filepath.Base(req.RestoreTimingPath)
	if len(restores) > 0 {
		srv := out.EnsureServerSummary()
		var mf, dl, ou, wr, pe, tot []float64
		for _, r := range restores {
			mf = append(mf, r.ManifestFetchMS)
			dl = append(dl, r.DownloadMS)
			ou = append(ou, r.OCIUnpackMS)
			wr = append(wr, r.WorkerRestoreMS)
			pe = append(pe, r.PersistMS)
			tot = append(tot, r.TotalMS)
		}
		srv["restore_manifest_fetch_ms"] = summarizeSlice(mf, "ms")
		srv["restore_download_ms"] = summarizeSlice(dl, "ms")
		srv["restore_oci_unpack_ms"] = summarizeSlice(ou, "ms")
		srv["restore_worker_restore_ms"] = summarizeSlice(wr, "ms")
		srv["restore_persist_ms"] = summarizeSlice(pe, "ms")
		srv["restore_total_ms"] = summarizeSlice(tot, "ms")
	}
	return nil
}

type k8sHealthCollector struct{}

func (k8sHealthCollector) Name() string                 { return "k8s-health" }
func (k8sHealthCollector) Scope() schema.TelemetryScope { return schema.ScopeK8s }

func (k8sHealthCollector) Collect(ctx context.Context, req CollectRequest, out *CollectResult) error {
	if !req.EnableK8s {
		return nil
	}
	// When both k8s and substrate scopes are active, CollectK8sClusterMeasurements
	// collects both in a single pass so actor counts and node densities are joined.
	var (
		sysDesc    *schema.SystemDescription
		k8sSummary map[string]schema.MetricSummary
		err        error
	)
	if schema.HasTelemetryScope(req.Scopes, schema.ScopeSubstrate) {
		sysDesc, k8sSummary, err = CollectK8sClusterMeasurements(ctx, req.K8s)
	} else {
		sysDesc, k8sSummary, err = CollectK8sHealthMeasurements(ctx, req.K8s)
	}
	if err != nil {
		return err
	}
	if sysDesc != nil {
		out.SystemDescription = sysDesc
	}
	srv := out.EnsureServerSummary()
	for k, v := range k8sSummary {
		srv[k] = v
	}
	if len(out.Measurements.PromMetrics) == 0 && len(k8sSummary) > 0 {
		var keys []string
		for k := range k8sSummary {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			ms := k8sSummary[k]
			val := ms.P50
			out.Measurements.PromMetrics = append(out.Measurements.PromMetrics, schema.PrometheusSeries{
				Name:   k,
				Labels: map[string]string{"unit": ms.Unit},
				Status: "ok",
				Value:  &val,
				Values: []float64{val},
			})
		}
	}
	return nil
}

type substrateControlCollector struct{}

func (substrateControlCollector) Name() string                 { return "substrate-control" }
func (substrateControlCollector) Scope() schema.TelemetryScope { return schema.ScopeSubstrate }

func (substrateControlCollector) Collect(ctx context.Context, req CollectRequest, out *CollectResult) error {
	if !req.EnableK8s && req.K8s.Control == nil && req.K8s.ATEEndpoint == "" {
		return nil
	}
	// If k8s-health already ran with ScopeSubstrate enabled, actor counts are already populated.
	if out.Measurements.ServerSummary != nil {
		if _, ok := out.Measurements.ServerSummary["active_sandboxes"]; ok && schema.HasTelemetryScope(req.Scopes, schema.ScopeK8s) {
			return nil
		}
	}
	srv := out.EnsureServerSummary()
	return CollectSubstrateControlMeasurements(ctx, req.K8s, out.SystemDescription, srv)
}

// RunCollectors executes all collectors matching req.Scopes (and req.EnabledCollectors)
// followed by any declarative feature-debug plugins in req.Plugins.
func RunCollectors(ctx context.Context, req CollectRequest) (*CollectResult, error) {
	if strings.TrimSpace(req.RunID) == "" {
		return nil, errors.New("measure: run_id is required")
	}
	collectedAt := req.CollectedAt
	if collectedAt.IsZero() {
		collectedAt = time.Now().UTC()
	}
	res := &CollectResult{
		Measurements: &schema.Measurements{
			SchemaVersion: schema.SchemaVersion,
			RunID:         req.RunID,
			CollectedAt:   collectedAt,
		},
		PluginFiles: make(map[string]string),
	}

	collectors := CollectorsForScopes(req.Scopes, req.EnabledCollectors)
	for _, c := range collectors {
		if err := c.Collect(ctx, req, res); err != nil {
			return nil, err
		}
	}
	if err := RunPluginCollectors(ctx, req, res); err != nil {
		return nil, err
	}
	if err := res.Measurements.Validate(); err != nil {
		return nil, err
	}
	return res, nil
}

// RunPluginCollectors executes declarative feature-debug plugins (prom_query, k8s_logs,
// regex_metric, command, custom) whose Scope is active in req.Scopes.
func RunPluginCollectors(ctx context.Context, req CollectRequest, out *CollectResult) error {
	for _, p := range req.Plugins {
		if err := p.Validate(); err != nil {
			return err
		}
		if p.Scope != "" && !schema.HasTelemetryScope(req.Scopes, p.Scope) {
			continue
		}
		switch p.Kind {
		case schema.PluginKindPromQuery:
			if req.Scraper == nil {
				return fmt.Errorf("telemetry_plugin[%q]: prometheus scraper is required for prom_query", p.Name)
			}
			series, err := req.Scraper.Query(ctx, PromQuery{
				Name:     p.Name,
				Expr:     p.Expr,
				Required: false,
			})
			if err != nil {
				return fmt.Errorf("telemetry_plugin[%q]: %w", p.Name, err)
			}
			out.Measurements.PromMetrics = append(out.Measurements.PromMetrics, series...)
			var vals []float64
			for _, s := range series {
				if s.Value != nil {
					vals = append(vals, *s.Value)
				}
				vals = append(vals, s.Values...)
			}
			out.EnsureCustomMetrics()[p.Name] = summarizeSlice(vals, p.Unit)

		case schema.PluginKindK8sLogs:
			kube := req.K8s.Kube
			if kube == nil {
				kube = defaultKubeRunner{}
			}
			ns := strings.TrimSpace(p.Namespace)
			if ns == "" {
				ns = "default"
			}
			args := []string{"logs", "-n", ns, "-l", p.LabelSelector, "--tail=-1"}
			if strings.TrimSpace(p.Container) != "" {
				args = append(args, "-c", strings.TrimSpace(p.Container))
			}
			rawLogs, err := kube.Run(ctx, args...)
			if err != nil {
				return fmt.Errorf("telemetry_plugin[%q] kubectl logs: %w", p.Name, err)
			}
			filtered, lineCount, err := filterLogLines(rawLogs, p.Grep)
			if err != nil {
				return fmt.Errorf("telemetry_plugin[%q] grep: %w", p.Name, err)
			}
			if req.DestDir != "" {
				pluginDir := filepath.Join(req.DestDir, "plugins")
				if err := os.MkdirAll(pluginDir, 0o755); err != nil {
					return err
				}
				logPath := filepath.Join(pluginDir, p.Name+".log")
				if err := os.WriteFile(logPath, filtered, 0o644); err != nil {
					return err
				}
				out.PluginFiles[p.Name] = logPath
			}
			unit := p.Unit
			if unit == "" {
				unit = "lines"
			}
			out.EnsureCustomMetrics()[p.Name] = singleScalarSummary(float64(lineCount), unit)

		case schema.PluginKindRegexMetric:
			summary, err := ExtractRegexMetricSummary(req.DestDir, p.Glob, p.Regex, p.Unit)
			if err != nil {
				return fmt.Errorf("telemetry_plugin[%q]: %w", p.Name, err)
			}
			out.EnsureCustomMetrics()[p.Name] = summary

		case schema.PluginKindCommand:
			cmd := exec.CommandContext(ctx, p.Command[0], p.Command[1:]...)
			if req.DestDir != "" {
				cmd.Dir = req.DestDir
			}
			rawOut, err := cmd.CombinedOutput()
			if err != nil {
				return fmt.Errorf("telemetry_plugin[%q] command failed: %w (%s)", p.Name, err, strings.TrimSpace(string(rawOut)))
			}
			if req.DestDir != "" {
				pluginDir := filepath.Join(req.DestDir, "plugins")
				if err := os.MkdirAll(pluginDir, 0o755); err != nil {
					return err
				}
				outFile := filepath.Join(pluginDir, p.Name+".log")
				if err := os.WriteFile(outFile, rawOut, 0o644); err != nil {
					return err
				}
				out.PluginFiles[p.Name] = outFile
			}
			if v, perr := strconv.ParseFloat(strings.TrimSpace(string(rawOut)), 64); perr == nil {
				out.EnsureCustomMetrics()[p.Name] = singleScalarSummary(v, p.Unit)
			}

		case schema.PluginKindCustom:
			collectorsMu.RLock()
			customCol, ok := collectorRegistry[p.Name]
			collectorsMu.RUnlock()
			if !ok {
				return fmt.Errorf("telemetry_plugin[%q]: custom collector not registered", p.Name)
			}
			if err := customCol.Collect(ctx, req, out); err != nil {
				return fmt.Errorf("telemetry_plugin[%q]: %w", p.Name, err)
			}
		}
	}
	return nil
}

func filterLogLines(raw []byte, grepPattern string) ([]byte, int, error) {
	var re *regexp.Regexp
	if strings.TrimSpace(grepPattern) != "" {
		var err error
		re, err = regexp.Compile(grepPattern)
		if err != nil {
			return nil, 0, err
		}
	}
	var buf bytes.Buffer
	var count int
	sc := bufio.NewScanner(bytes.NewReader(raw))
	sc.Buffer(make([]byte, 0, 64*1024), 4<<20)
	for sc.Scan() {
		line := sc.Text()
		if re != nil && !re.MatchString(line) {
			continue
		}
		count++
		buf.WriteString(line)
		buf.WriteByte('\n')
	}
	return buf.Bytes(), count, sc.Err()
}

// ExtractRegexMetricSummary scans files matching globPattern (relative to baseDir when not absolute)
// and extracts numeric captures from regexExpr (using the "value" named group or group 1).
func ExtractRegexMetricSummary(baseDir, globPattern, regexExpr, unit string) (schema.MetricSummary, error) {
	re, err := regexp.Compile(regexExpr)
	if err != nil {
		return schema.MetricSummary{}, fmt.Errorf("compile regex %q: %w", regexExpr, err)
	}
	pattern := globPattern
	if baseDir != "" && !filepath.IsAbs(pattern) {
		pattern = filepath.Join(baseDir, pattern)
	}
	matches, err := filepath.Glob(pattern)
	if err != nil {
		return schema.MetricSummary{}, fmt.Errorf("glob %q: %w", pattern, err)
	}
	sort.Strings(matches)
	valIdx := re.SubexpIndex("value")
	if valIdx < 0 && re.NumSubexp() >= 1 {
		valIdx = 1
	}
	var vals []float64
	for _, m := range matches {
		f, err := os.Open(m)
		if err != nil {
			continue
		}
		sc := bufio.NewScanner(f)
		sc.Buffer(make([]byte, 0, 64*1024), 4<<20)
		for sc.Scan() {
			sub := re.FindStringSubmatch(sc.Text())
			if len(sub) == 0 {
				continue
			}
			rawVal := sub[0]
			if valIdx > 0 && valIdx < len(sub) {
				rawVal = sub[valIdx]
			}
			if v, perr := strconv.ParseFloat(strings.TrimSpace(rawVal), 64); perr == nil {
				vals = append(vals, v)
			}
		}
		_ = f.Close()
	}
	return summarizeSlice(vals, unit), nil
}
