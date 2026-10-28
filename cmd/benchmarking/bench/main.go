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

// bench provides composable CLI subcommands (resolve, render, prewarm,
// measure, reap, report) for RL sandbox benchmarking. Each subcommand
// communicates strictly through $ARTIFACT_DIR (--dest) files.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/agent-substrate/env/internal/benchmarking/measure"
	"github.com/agent-substrate/env/internal/benchmarking/schema"
	"github.com/agent-substrate/env/internal/benchmarking/sut"
	"github.com/agent-substrate/env/internal/benchmarking/swebench"
)

func main() {
	os.Exit(execute(os.Args[1:], os.Stdout, os.Stderr))
}

func execute(args []string, stdout, stderr io.Writer) int {
	if len(args) < 1 {
		printUsage(stderr)
		return 2
	}
	var err error
	switch args[0] {
	case "resolve":
		err = resolveCmd(args[1:], stdout, stderr)
	case "render":
		err = renderCmd(args[1:], stdout, stderr)
	case "prewarm":
		err = prewarmCmd(args[1:], stdout, stderr)
	case "spinup":
		err = spinupCmd(args[1:], stdout, stderr)
	case "spindown":
		err = spindownCmd(args[1:], stdout, stderr)
	case "measure":
		err = measureCmd(args[1:], stderr)
	case "reap":
		err = reapCmd(args[1:], stdout, stderr)
	case "report":
		err = reportCmd(args[1:], stdout, stderr)
	case "-h", "--help", "help":
		printUsage(stderr)
		return 0
	default:
		printUsage(stderr)
		return 2
	}
	if err != nil {
		fmt.Fprintln(stderr, "error:", err)
		return 1
	}
	return 0
}

func printUsage(w io.Writer) {
	fmt.Fprintln(w, "usage: bench <resolve|render|prewarm|spinup|spindown|measure|reap|report> [flags]")
}

func resolveCmd(args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("resolve", flag.ContinueOnError)
	fs.SetOutput(stderr)
	scenarioPath := fs.String("scenario", "", "Path to Scenario YAML file. Required.")
	datasetPath := fs.String("dataset", "", "Default dataset JSONL path.")
	runID := fs.String("run-id", "rlb", "Run ID prefix.")
	destDir := fs.String("dest", "", "Artifact directory ($ARTIFACT_DIR).")
	outPath := fs.String("out", "", "Output schedule.json path (defaults to <dest>/schedule.json or schedule.json).")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *scenarioPath == "" {
		return fmt.Errorf("-scenario is required")
	}
	if *outPath == "" {
		if *destDir != "" {
			*outPath = filepath.Join(*destDir, "schedule.json")
		} else {
			*outPath = "schedule.json"
		}
	}
	raw, err := os.ReadFile(*scenarioPath)
	if err != nil {
		return err
	}
	scen, err := schema.DecodeYAMLStrict[schema.Scenario](raw)
	if err != nil {
		return err
	}
	datasets := make(map[string][]swebench.Instance)
	for _, j := range scen.Jobs {
		dsSource := j.Dataset
		if dsSource == "" {
			dsSource = *datasetPath
		}
		if dsSource != "" {
			if _, ok := datasets[dsSource]; !ok {
				ds, derr := swebench.LoadDataset(dsSource)
				if derr != nil {
					return derr
				}
				datasets[dsSource] = ds
			}
		}
	}
	_ = *runID
	sched, err := swebench.ResolveScenarioSchedule(scen, *datasetPath, datasets)
	if err != nil {
		return err
	}
	if err := swebench.WriteSchedule(*outPath, sched); err != nil {
		return err
	}
	fmt.Fprintf(stdout, "wrote %s (%d jobs)\n", *outPath, len(sched.Jobs))
	return nil
}

func renderCmd(args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("render", flag.ContinueOnError)
	fs.SetOutput(stderr)
	scenarioPath := fs.String("scenario", "", "Path to Scenario YAML file. Required.")
	destDir := fs.String("dest", ".", "Destination artifact directory ($ARTIFACT_DIR).")
	cluster := fs.String("cluster", "local-cluster", "Cluster name recorded in system_description.json.")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *scenarioPath == "" {
		return fmt.Errorf("-scenario is required")
	}
	raw, err := os.ReadFile(*scenarioPath)
	if err != nil {
		return err
	}
	scen, err := schema.DecodeYAMLStrict[schema.Scenario](raw)
	if err != nil {
		return err
	}
	prov := sut.NewProvisioner(nil)
	if _, err := prov.Provision(context.Background(), sut.ProvisionOptions{
		Spec:        scen.SUT,
		ArtifactDir: *destDir,
		RenderOnly:  true,
	}); err != nil {
		return err
	}
	sysDesc := sut.BuildSystemDescription(scen.SUT, *cluster, "v1.31", "HEAD", "verified", nil)
	if err := sut.WriteSystemDescription(filepath.Join(*destDir, "system_description.json"), sysDesc); err != nil {
		return err
	}
	sutPath := filepath.Join(*destDir, sut.DefaultManifestFilename)
	fmt.Fprintf(stdout, "rendered SUT manifest %s in %s\n", sutPath, *destDir)
	return nil
}

func prewarmCmd(args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("prewarm", flag.ContinueOnError)
	fs.SetOutput(stderr)
	endpoint := fs.String("endpoint", "localhost:7777", "ate-env API address.")
	atespace := fs.String("atespace", "ate-env", "Atespace.")
	baseTemplate := fs.String("base-template", "default-template", "Base template.")
	token := fs.String("token", "", "Optional bearer token for ate-env-api.")
	dataset := fs.String("dataset", "", "Dataset JSONL path. Required.")
	images := fs.Int("images", 1, "Number of distinct images to prewarm.")
	destDir := fs.String("dest", "", "Artifact directory ($ARTIFACT_DIR); writes events/prewarm.jsonl when set.")
	outPath := fs.String("out", "", "Optional explicit path for prewarm JSONL events.")
	timeout := fs.Duration("timeout", 5*time.Minute, "Prewarm timeout.")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *dataset == "" {
		return fmt.Errorf("-dataset is required")
	}
	all, err := swebench.LoadDataset(*dataset)
	if err != nil {
		return err
	}
	selected, err := swebench.Select(all, nil, nil, *images)
	if err != nil {
		return err
	}
	backend, err := swebench.NewEnvBackend(*endpoint, *atespace, *baseTemplate, 1, *token)
	if err != nil {
		return err
	}
	defer backend.Close()

	eventsFile := *outPath
	if eventsFile == "" && *destDir != "" {
		eventsFile = filepath.Join(*destDir, "events", "prewarm.jsonl")
	}
	var rec *swebench.Recorder
	var f *os.File
	if eventsFile != "" {
		if err := os.MkdirAll(filepath.Dir(eventsFile), 0o755); err != nil {
			return err
		}
		f, err = os.Create(eventsFile)
		if err != nil {
			return err
		}
		defer f.Close()
		rec = swebench.NewRecorder(f)
	}

	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()

	for i, in := range selected {
		start := time.Now()
		id := fmt.Sprintf("prewarm-%d-%d", start.Unix()%10000, i)
		sb, cerr := backend.Create(ctx, id, in.Image)
		dur := time.Since(start)
		if rec != nil {
			ev := swebench.Event{
				Time:     time.Now().UTC(),
				Task:     i,
				Instance: in.InstanceID,
				Image:    in.Image,
				Env:      id,
				Event:    swebench.EventPrewarm,
				DurMS:    float64(dur.Milliseconds()),
				OK:       cerr == nil,
			}
			if cerr != nil {
				ev.Err = cerr.Error()
			}
			rec.Record(ev)
		}
		if cerr != nil {
			return cerr
		}
		_ = sb.Delete(ctx)
		fmt.Fprintf(stdout, "prewarmed image %s\n", in.Image)
	}
	if rec != nil {
		if err := rec.Err(); err != nil {
			return err
		}
	}
	return nil
}

var lifecycleKubeRunner measure.KubeCommandRunner

func spinupCmd(args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("spinup", flag.ContinueOnError)
	fs.SetOutput(stderr)
	atespace := fs.String("atespace", "ate-env", "Kubernetes namespace / Substrate atespace.")
	namespace := fs.String("namespace", "", "Alias for -atespace: Kubernetes namespace containing the WorkerPool.")
	workerPool := fs.String("workerpool", "default-template-workerpool", "Substrate WorkerPool name.")
	replicas := fs.Int("replicas", 2, "Target WorkerPool replica count (> 0).")
	timeout := fs.Duration("timeout", 5*time.Minute, "Maximum time to wait for all WorkerPool pods to become Ready.")
	runID := fs.String("run-id", "rlb", "Run ID.")
	destDir := fs.String("dest", "", "Artifact directory ($ARTIFACT_DIR); merges spin-up metrics into measurements.json and writes events/substrate_lifecycle.jsonl.")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *namespace != "" {
		*atespace = *namespace
	}
	ctx, cancel := context.WithTimeout(context.Background(), *timeout+10*time.Second)
	defer cancel()

	metrics, err := measure.SpinUpWorkerPool(ctx, lifecycleKubeRunner, *atespace, *workerPool, *replicas, *timeout)
	if err != nil {
		return err
	}
	wallMS := metrics["workerpool_spinup_wall_ms"].P50
	readyPods := int(metrics["worker_pod_count"].P50)
	if *destDir != "" {
		if err := recordLifecycleMetrics(*destDir, *runID, "workerpool_spinup", wallMS, readyPods, metrics); err != nil {
			return err
		}
	}
	fmt.Fprintf(stdout, "spun up workerpool/%s in %s (replicas=%d, wall_ms=%.1f)\n", *workerPool, *atespace, readyPods, wallMS)
	return nil
}

func spindownCmd(args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("spindown", flag.ContinueOnError)
	fs.SetOutput(stderr)
	atespace := fs.String("atespace", "ate-env", "Kubernetes namespace / Substrate atespace.")
	namespace := fs.String("namespace", "", "Alias for -atespace: Kubernetes namespace containing the WorkerPool.")
	workerPool := fs.String("workerpool", "default-template-workerpool", "Substrate WorkerPool name.")
	timeout := fs.Duration("timeout", 5*time.Minute, "Maximum time to wait for all WorkerPool pods to terminate.")
	runID := fs.String("run-id", "rlb", "Run ID.")
	destDir := fs.String("dest", "", "Artifact directory ($ARTIFACT_DIR); merges spin-down metrics into measurements.json and writes events/substrate_lifecycle.jsonl.")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *namespace != "" {
		*atespace = *namespace
	}
	ctx, cancel := context.WithTimeout(context.Background(), *timeout+10*time.Second)
	defer cancel()

	metrics, err := measure.SpinDownWorkerPool(ctx, lifecycleKubeRunner, *atespace, *workerPool, *timeout)
	if err != nil {
		return err
	}
	wallMS := metrics["workerpool_spindown_wall_ms"].P50
	drained := int(metrics["workerpool_drained_pods"].P50)
	if *destDir != "" {
		if err := recordLifecycleMetrics(*destDir, *runID, "workerpool_spindown", wallMS, drained, metrics); err != nil {
			return err
		}
	}
	fmt.Fprintf(stdout, "spun down workerpool/%s in %s (drained=%d, wall_ms=%.1f)\n", *workerPool, *atespace, drained, wallMS)
	return nil
}

func recordLifecycleMetrics(destDir, runID, phaseName string, wallMS float64, count int, metrics map[string]schema.MetricSummary) error {
	eventsPath := filepath.Join(destDir, "events", "substrate_lifecycle.jsonl")
	if err := os.MkdirAll(filepath.Dir(eventsPath), 0o755); err != nil {
		return err
	}
	f, err := os.OpenFile(eventsPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	rec := swebench.NewRecorder(f)
	evMetrics := make(map[string]float64, len(metrics))
	for k, ms := range metrics {
		evMetrics[k] = ms.P50
	}
	rec.Record(swebench.Event{
		Time:    time.Now().UTC(),
		RunID:   runID,
		Event:   swebench.EventPhase,
		Name:    phaseName,
		Count:   count,
		DurMS:   wallMS,
		OK:      true,
		Metrics: evMetrics,
	})
	_ = f.Close()

	measPath := filepath.Join(destDir, "measurements.json")
	var meas schema.Measurements
	if raw, rerr := os.ReadFile(measPath); rerr == nil {
		_ = json.Unmarshal(raw, &meas)
	}
	if meas.SchemaVersion == "" {
		meas.SchemaVersion = schema.SchemaVersion
	}
	if meas.RunID == "" {
		meas.RunID = runID
	}
	if meas.CollectedAt.IsZero() {
		meas.CollectedAt = time.Now().UTC()
	}
	if meas.ServerSummary == nil {
		meas.ServerSummary = make(map[string]schema.MetricSummary, len(metrics))
	}
	if meas.CustomMetrics == nil {
		meas.CustomMetrics = make(map[string]schema.MetricSummary, len(metrics))
	}
	for k, ms := range metrics {
		meas.ServerSummary[k] = ms
		meas.CustomMetrics[k] = ms
	}
	if len(meas.PromMetrics) == 0 {
		val := wallMS
		meas.PromMetrics = []schema.PrometheusSeries{
			{
				Name:   phaseName + "_wall_ms",
				Labels: map[string]string{"unit": "ms"},
				Status: "ok",
				Value:  &val,
				Values: []float64{val},
			},
		}
	}
	return measure.WriteMeasurements(measPath, &meas)
}

type querySliceFlag []measure.PromQuery

func (q *querySliceFlag) String() string { return "" }

func (q *querySliceFlag) Set(val string) error {
	parts := strings.SplitN(val, "=", 2)
	if len(parts) != 2 || strings.TrimSpace(parts[0]) == "" || strings.TrimSpace(parts[1]) == "" {
		return fmt.Errorf("expected name=expr, got %q", val)
	}
	*q = append(*q, measure.PromQuery{
		Name:     strings.TrimSpace(parts[0]),
		Expr:     strings.TrimSpace(parts[1]),
		Required: true,
	})
	return nil
}

type stringSliceFlag []string

func (s *stringSliceFlag) String() string { return strings.Join(*s, ",") }

func (s *stringSliceFlag) Set(val string) error {
	*s = append(*s, val)
	return nil
}

func parseCLIPlugins(promFlags, logFlags, metricFlags, cmdFlags []string) ([]schema.TelemetryPluginSpec, error) {
	var out []schema.TelemetryPluginSpec
	for _, raw := range promFlags {
		parts := strings.SplitN(raw, "=", 2)
		if len(parts) != 2 || strings.TrimSpace(parts[0]) == "" || strings.TrimSpace(parts[1]) == "" {
			return nil, fmt.Errorf("-plugin-prom expected name=expr, got %q", raw)
		}
		out = append(out, schema.TelemetryPluginSpec{
			Name: strings.TrimSpace(parts[0]),
			Kind: schema.PluginKindPromQuery,
			Expr: strings.TrimSpace(parts[1]),
		})
	}
	for _, raw := range logFlags {
		parts := strings.SplitN(raw, "=", 2)
		if len(parts) != 2 || strings.TrimSpace(parts[0]) == "" || strings.TrimSpace(parts[1]) == "" {
			return nil, fmt.Errorf("-plugin-log expected name=namespace/labelSelector[:grepRegex], got %q", raw)
		}
		name := strings.TrimSpace(parts[0])
		specStr := strings.TrimSpace(parts[1])
		ns := "default"
		selectorAndGrep := specStr
		if slash := strings.IndexByte(specStr, '/'); slash > 0 {
			ns = specStr[:slash]
			selectorAndGrep = specStr[slash+1:]
		}
		selector := selectorAndGrep
		grep := ""
		if colon := strings.IndexByte(selectorAndGrep, ':'); colon > 0 {
			selector = selectorAndGrep[:colon]
			grep = selectorAndGrep[colon+1:]
		}
		out = append(out, schema.TelemetryPluginSpec{
			Name:          name,
			Kind:          schema.PluginKindK8sLogs,
			Namespace:     ns,
			LabelSelector: selector,
			Grep:          grep,
			Unit:          "lines",
		})
	}
	for _, raw := range metricFlags {
		parts := strings.SplitN(raw, "=", 2)
		if len(parts) != 2 || strings.TrimSpace(parts[0]) == "" || strings.TrimSpace(parts[1]) == "" {
			return nil, fmt.Errorf("-plugin-metric expected name=glob:regex, got %q", raw)
		}
		name := strings.TrimSpace(parts[0])
		globAndRegex := strings.TrimSpace(parts[1])
		colon := strings.IndexByte(globAndRegex, ':')
		if colon <= 0 || colon == len(globAndRegex)-1 {
			return nil, fmt.Errorf("-plugin-metric expected name=glob:regex, got %q", raw)
		}
		out = append(out, schema.TelemetryPluginSpec{
			Name:  name,
			Kind:  schema.PluginKindRegexMetric,
			Glob:  globAndRegex[:colon],
			Regex: globAndRegex[colon+1:],
		})
	}
	for _, raw := range cmdFlags {
		parts := strings.SplitN(raw, "=", 2)
		if len(parts) != 2 || strings.TrimSpace(parts[0]) == "" || strings.TrimSpace(parts[1]) == "" {
			return nil, fmt.Errorf("-plugin-cmd expected name=command, got %q", raw)
		}
		out = append(out, schema.TelemetryPluginSpec{
			Name:    strings.TrimSpace(parts[0]),
			Kind:    schema.PluginKindCommand,
			Command: []string{"sh", "-c", strings.TrimSpace(parts[1])},
		})
	}
	return out, nil
}

func loadTelemetrySpecFromScenario(scenarioPath string) (*schema.TelemetrySpec, error) {
	if strings.TrimSpace(scenarioPath) == "" {
		return nil, nil
	}
	raw, err := os.ReadFile(scenarioPath)
	if err != nil {
		return nil, err
	}
	scen, err := schema.DecodeYAMLStrict[schema.Scenario](raw)
	if err != nil {
		return nil, err
	}
	return scen.Telemetry, nil
}

func measureCmd(args []string, stderr io.Writer) error {
	fs := flag.NewFlagSet("measure", flag.ContinueOnError)
	fs.SetOutput(stderr)
	runID := fs.String("run-id", "rlb", "Run ID.")
	promURL := fs.String("prom-url", "", "Prometheus HTTP base URL.")
	cacheBefore := fs.String("cache-before", "", "Path to cache_before.json or cache_before.prom.")
	cacheAfter := fs.String("cache-after", "", "Path to cache_after.json or cache_after.prom.")
	sampleNodeCache := fs.String("sample-node-cache", "", "Scrape K8s node image-cache counters into this .prom file and exit.")
	restoreLog := fs.String("restore-log", "", "Path to restore_timings.jsonl.")
	restoreTimings := fs.String("restore-timings", "", "Alias for -restore-log: path to restore_timings.jsonl.")
	minRestores := fs.Int("min-restores", 0, "Minimum expected restore timing records.")
	destDir := fs.String("dest", "", "Artifact directory ($ARTIFACT_DIR).")
	outPath := fs.String("out", "", "Output measurements.json path (defaults to <dest>/measurements.json or measurements.json).")
	k8sCollect := fs.Bool("k8s", false, "Collect Kubernetes node/pod/metrics.k8s.io and Substrate telemetry.")
	ateEndpoint := fs.String("ate-endpoint", "", "Optional Substrate ate-api gRPC endpoint for ListActors/GetActorTemplate.")
	atespace := fs.String("atespace", "ate-env", "Substrate atespace for K8s/actor telemetry.")
	baseTemplate := fs.String("base-template", "default-template", "Base actor template name.")
	clusterName := fs.String("cluster", "", "Optional cluster name override for system_description.json.")
	sysOut := fs.String("sys-out", "", "Optional output path for system_description.json (defaults to <dest>/system_description.json when -k8s and -dest are set).")
	scopesFlag := fs.String("scopes", "", "Comma-separated telemetry scopes to collect (rl,user-sandbox,substrate,k8s or all; default all).")
	collectorsFlag := fs.String("collectors", "", "Optional comma-separated collector names to run.")
	scenarioPath := fs.String("scenario", "", "Optional Scenario YAML path to load telemetry.scopes and telemetry.plugins.")
	var queries querySliceFlag
	var pluginProms, pluginLogs, pluginMetrics, pluginCmds stringSliceFlag
	fs.Var(&queries, "query", "Prometheus query in name=expr format (repeatable, required).")
	fs.Var(&pluginProms, "plugin-prom", "Ad-hoc Prometheus feature plugin in name=expr format (repeatable).")
	fs.Var(&pluginLogs, "plugin-log", "Ad-hoc K8s pod log harvester in name=ns/labelSelector[:grepRegex] format (repeatable).")
	fs.Var(&pluginMetrics, "plugin-metric", "Ad-hoc log/JSONL regex metric extractor in name=glob:regex format (repeatable).")
	fs.Var(&pluginCmds, "plugin-cmd", "Ad-hoc diagnostic command plugin in name=cmd format (repeatable).")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *restoreLog == "" && *restoreTimings != "" {
		*restoreLog = *restoreTimings
	}
	if *sampleNodeCache != "" {
		promText, err := measure.CollectK8sNodeCachePromText(context.Background(), nil, nil)
		if err != nil {
			return err
		}
		if err := os.MkdirAll(filepath.Dir(*sampleNodeCache), 0o755); err != nil {
			return err
		}
		return os.WriteFile(*sampleNodeCache, []byte(promText), 0o644)
	}
	if *outPath == "" {
		if *destDir != "" {
			*outPath = filepath.Join(*destDir, "measurements.json")
		} else {
			*outPath = "measurements.json"
		}
	}
	if *sysOut == "" && *k8sCollect && *destDir != "" {
		*sysOut = filepath.Join(*destDir, "system_description.json")
	}
	if *destDir != "" {
		if *cacheBefore == "" {
			for _, name := range []string{"cache_before.json", "cache_before.prom"} {
				p := filepath.Join(*destDir, name)
				if _, err := os.Stat(p); err == nil {
					*cacheBefore = p
					break
				}
			}
		}
		if *cacheAfter == "" {
			for _, name := range []string{"cache_after.json", "cache_after.prom"} {
				p := filepath.Join(*destDir, name)
				if _, err := os.Stat(p); err == nil {
					*cacheAfter = p
					break
				}
			}
		}
		if *restoreLog == "" {
			p := filepath.Join(*destDir, "restore_timings.jsonl")
			if _, err := os.Stat(p); err == nil {
				*restoreLog = p
			}
		}
	}
	telSpec, err := loadTelemetrySpecFromScenario(*scenarioPath)
	if err != nil {
		return err
	}
	var scopeInputs []string
	if *scopesFlag != "" {
		scopeInputs = append(scopeInputs, *scopesFlag)
	} else if telSpec != nil {
		for _, s := range telSpec.Scopes {
			scopeInputs = append(scopeInputs, string(s))
		}
	}
	activeScopes, err := schema.ParseTelemetryScopes(scopeInputs...)
	if err != nil {
		return err
	}
	var enabledCollectors []string
	if *collectorsFlag != "" {
		enabledCollectors = append(enabledCollectors, *collectorsFlag)
	} else if telSpec != nil && len(telSpec.Collectors) > 0 {
		enabledCollectors = append(enabledCollectors, telSpec.Collectors...)
	}
	cliPlugins, err := parseCLIPlugins(pluginProms, pluginLogs, pluginMetrics, pluginCmds)
	if err != nil {
		return err
	}
	var plugins []schema.TelemetryPluginSpec
	if telSpec != nil {
		plugins = append(plugins, telSpec.Plugins...)
	}
	plugins = append(plugins, cliPlugins...)

	var scraper *measure.PrometheusScraper
	if *promURL != "" {
		scraper = &measure.PrometheusScraper{BaseURL: *promURL}
		if len(queries) == 0 && len(pluginProms) == 0 {
			queries = measure.MustDefaultPromQueries()
		}
	}
	res, err := measure.RunCollectors(context.Background(), measure.CollectRequest{
		RunID:             *runID,
		DestDir:           *destDir,
		Scopes:            activeScopes,
		EnabledCollectors: enabledCollectors,
		Scraper:           scraper,
		Queries:           queries,
		CacheBeforePath:   *cacheBefore,
		CacheAfterPath:    *cacheAfter,
		RestoreTimingPath: *restoreLog,
		MinRestores:       *minRestores,
		EnableK8s:         *k8sCollect,
		K8s: measure.K8sCollectOptions{
			Cluster:      *clusterName,
			Atespace:     *atespace,
			BaseTemplate: *baseTemplate,
			ATEEndpoint:  *ateEndpoint,
		},
		Plugins: plugins,
	})
	if err != nil {
		return err
	}
	if *sysOut != "" && res.SystemDescription != nil {
		if err := sut.WriteSystemDescription(*sysOut, *res.SystemDescription); err != nil {
			return err
		}
	}
	return measure.WriteMeasurements(*outPath, res.Measurements)
}

func reapCmd(args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("reap", flag.ContinueOnError)
	fs.SetOutput(stderr)
	ateEndpoint := fs.String("ate-endpoint", "", "Substrate ate-api endpoint for server-queried reap.")
	atespace := fs.String("atespace", "ate-env", "Atespace to reap.")
	prefix := fs.String("prefix", "", "Run ID prefix to reap.")
	timeout := fs.Duration("timeout", 2*time.Minute, "Reap timeout.")
	pollInterval := fs.Duration("poll-interval", 500*time.Millisecond, "Poll interval between reap checks.")
	destDir := fs.String("dest", "", "Artifact directory ($ARTIFACT_DIR); writes events/reap.jsonl when set.")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *ateEndpoint == "" {
		fmt.Fprintf(stdout, "reap: no -ate-endpoint specified, 0 remaining\n")
		return nil
	}
	reaper, closeFn, err := sut.DialATEClusterReaper(*ateEndpoint)
	if err != nil {
		return err
	}
	defer func() { _ = closeFn() }()
	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()
	res, reapErr := sut.Reap(ctx, reaper, *atespace, *prefix, *pollInterval)
	if *destDir != "" && res != nil {
		eventsDir := filepath.Join(*destDir, "events")
		if mkErr := os.MkdirAll(eventsDir, 0o755); mkErr == nil {
			if f, ferr := os.Create(filepath.Join(eventsDir, "reap.jsonl")); ferr == nil {
				rec := swebench.NewRecorder(f)
				now := time.Now().UTC()
				var delIDs []string
				for id := range res.DeleteDurations {
					delIDs = append(delIDs, id)
				}
				sort.Strings(delIDs)
				for _, id := range delIDs {
					d := res.DeleteDurations[id]
					rec.Record(swebench.Event{
						Time:  now,
						RunID: *prefix,
						Env:   id,
						Event: swebench.EventDelete,
						DurMS: float64(d.Microseconds()) / 1000.0,
						OK:    true,
					})
				}
				ev := swebench.Event{
					Time:  now,
					RunID: *prefix,
					Task:  0,
					Env:   *prefix,
					Event: swebench.EventReap,
					DurMS: float64(res.Duration.Microseconds()) / 1000.0,
					OK:    reapErr == nil && res.Remaining == 0,
					Metrics: map[string]float64{
						"orphan_count":  float64(res.Remaining),
						"deleted_count": float64(res.Deleted),
					},
				}
				if reapErr != nil {
					ev.Err = reapErr.Error()
				}
				rec.Record(ev)
				_ = rec.Flush()
				_ = f.Close()
			}
		}
	}
	if reapErr != nil {
		return reapErr
	}
	fmt.Fprintf(stdout, "reaped %d environments (remaining=%d, duration=%s)\n", res.Deleted, res.Remaining, res.Duration)
	return nil
}

type benchArtifactReport struct {
	*swebench.Report
	Measurements      *schema.Measurements      `json:"measurements,omitempty"`
	SystemDescription *schema.SystemDescription `json:"system_description,omitempty"`
}

func reportCmd(args []string, stdout, stderr io.Writer) error {
	var path string
	flagArgs := args
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		path = args[0]
		flagArgs = args[1:]
	}
	fs := flag.NewFlagSet("report", flag.ContinueOnError)
	fs.SetOutput(stderr)
	destDir := fs.String("dest", "", "Artifact directory containing events/*.jsonl, measurements.json, system_description.json.")
	reportOnly := fs.Bool("report-only", false, "Generate report.json from existing artifacts in -dest.")
	jsonOut := fs.String("json", "", "Write the summary as JSON to this path.")
	unresolvedOut := fs.String("unresolved", "", "Write unresolved instances to this path in -exclude-file format.")
	unresolvedNote := fs.String("unresolved-note", "Instances graded unresolved", "First comment line of the -unresolved file.")
	if err := fs.Parse(flagArgs); err != nil {
		return err
	}
	_ = reportOnly
	if path == "" && fs.NArg() > 0 {
		path = fs.Arg(0)
	}
	if *destDir != "" {
		return reportFromArtifactDir(*destDir, *jsonOut, stdout)
	}
	if path == "" {
		return fmt.Errorf("report needs a JSONL file or -dest DIR")
	}
	rep, err := swebench.LoadOrSummarizeReport(path)
	if err != nil {
		return err
	}
	rep.WriteText(stdout)
	return swebench.WriteReportFiles(rep, *jsonOut, "", *unresolvedOut, *unresolvedNote, "", "", "")
}

func reportFromArtifactDir(destDir, jsonOut string, stdout io.Writer) error {
	matches, err := filepath.Glob(filepath.Join(destDir, "events", "*.jsonl"))
	if err != nil {
		return err
	}
	sort.Strings(matches)
	var combined bytes.Buffer
	for _, m := range matches {
		b, err := os.ReadFile(m)
		if err != nil {
			return err
		}
		combined.Write(b)
		if len(b) > 0 && b[len(b)-1] != '\n' {
			combined.WriteByte('\n')
		}
	}
	rep, err := swebench.Summarize(&combined)
	if err != nil {
		return err
	}
	rep.WriteText(stdout)

	outRep := benchArtifactReport{Report: rep}
	if rawMeas, err := os.ReadFile(filepath.Join(destDir, "measurements.json")); err == nil {
		meas, derr := schema.DecodeJSONStrict[schema.Measurements](bytes.NewReader(rawMeas))
		if derr != nil {
			return fmt.Errorf("invalid measurements.json: %w", derr)
		}
		outRep.Measurements = &meas
	}
	if rawSys, err := os.ReadFile(filepath.Join(destDir, "system_description.json")); err == nil {
		sysDesc, derr := schema.DecodeJSONStrict[schema.SystemDescription](bytes.NewReader(rawSys))
		if derr != nil {
			return fmt.Errorf("invalid system_description.json: %w", derr)
		}
		outRep.SystemDescription = &sysDesc
	}

	targetJSON := jsonOut
	if targetJSON == "" {
		targetJSON = filepath.Join(destDir, "report.json")
	}
	b, err := json.MarshalIndent(outRep, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(targetJSON, append(b, '\n'), 0o644)
}
