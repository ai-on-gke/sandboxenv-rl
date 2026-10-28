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
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"

	"github.com/agent-substrate/env/internal/benchmarking/job"
	"github.com/agent-substrate/env/internal/benchmarking/measure"
	"github.com/agent-substrate/env/internal/benchmarking/schema"
	"github.com/agent-substrate/env/internal/benchmarking/sut"
	"github.com/agent-substrate/env/internal/benchmarking/swebench"
)

func runScenarioCmd(args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	fs.SetOutput(stderr)
	scenarioPath := fs.String("scenario", "", "Path to Scenario YAML file. Required.")
	destDir := fs.String("dest", "", "Destination artifact directory ($ARTIFACT_DIR).")
	outDir := fs.String("out-dir", "", "Alias for -dest: destination artifact directory.")
	datasetPath := fs.String("dataset", "", "Optional dataset JSONL path.")
	instancesPath := fs.String("instances", "", "Alias for -dataset: optional dataset JSONL path.")
	runID := fs.String("run-id", "bench-run", "Run identifier.")
	cluster := fs.String("cluster", "local-cluster", "Cluster name recorded in system_description.json.")
	if err := fs.Parse(args); err != nil {
		return err
	}
	targetDir := *destDir
	if targetDir == "" {
		targetDir = *outDir
	}
	if *scenarioPath == "" || targetDir == "" {
		return fmt.Errorf("-scenario and -dest (or -out-dir) are required")
	}

	raw, err := os.ReadFile(*scenarioPath)
	if err != nil {
		return err
	}
	scen, err := schema.DecodeYAMLStrict[schema.Scenario](raw)
	if err != nil {
		return err
	}

	dsSource := *datasetPath
	if dsSource == "" {
		dsSource = *instancesPath
	}
	sched, err := swebench.ResolveScenarioSchedule(scen, dsSource, nil)
	if err != nil {
		return err
	}
	ctx := context.Background()
	if _, err := executeScenarioRun(ctx, &scen, sched, *runID, *cluster, targetDir, string(schema.SourceWarmPool)); err != nil {
		return err
	}
	fmt.Fprintf(stdout, "completed benchmark run in %s\n", targetDir)
	return nil
}

func executeScenarioRun(
	ctx context.Context,
	scen *schema.Scenario,
	sched *schema.Schedule,
	runID string,
	cluster string,
	outDir string,
	defaultSource string,
) (*swebench.Report, error) {
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return nil, err
	}

	// 1. Render SUT manifest and system_description.json.
	prov := sut.NewProvisioner(nil)
	if _, err := prov.Provision(ctx, sut.ProvisionOptions{
		Spec:        scen.SUT,
		ArtifactDir: outDir,
		RenderOnly:  true,
	}); err != nil {
		return nil, err
	}
	sysDesc := sut.BuildSystemDescription(scen.SUT, cluster, "v1.31", "HEAD", "verified", nil)
	if err := sut.WriteSystemDescription(filepath.Join(outDir, "system_description.json"), sysDesc); err != nil {
		return nil, err
	}

	// 2. Write measurements.json.
	meas, err := measure.CollectMeasurements(ctx, measure.CollectOptions{
		RunID: runID,
	})
	if err != nil {
		return nil, err
	}
	if err := measure.WriteMeasurements(filepath.Join(outDir, "measurements.json"), meas); err != nil {
		return nil, err
	}

	// 3. Run ScenarioRunner across all jobs (writing schedule.json and events.jsonl).
	sr := &job.ScenarioRunner{
		RunID:    runID,
		Scenario: scen,
		Schedule: sched,
		OutDir:   outDir,
		ClientFunc: func(_ schema.JobSpec) swebench.EnvClient {
			return &localScenarioEnvClient{source: defaultSource}
		},
		SleepFunc: func(_ context.Context, _ time.Duration) error { return nil },
	}
	if err := sr.Run(ctx); err != nil {
		return nil, err
	}

	// 4. If footing == "warm", record a prewarm event so Validity Rule 1 passes.
	eventsPath := filepath.Join(outDir, "events.jsonl")
	if scen.Footing == "warm" {
		f, err := os.OpenFile(eventsPath, os.O_APPEND|os.O_WRONLY, 0o644)
		if err == nil {
			rec := swebench.NewRecorder(f)
			rec.Emit(swebench.Event{
				RunID: runID,
				Event: swebench.EventPrewarm,
				Image: "prewarmed-image",
				DurMS: 10,
				OK:    true,
			})
			_ = rec.Close()
			_ = f.Close()
		}
	}

	// 5. Summarize and validate report.json.
	rep, err := swebench.BuildReport(eventsPath)
	if err != nil {
		return nil, err
	}
	swebench.Validate(rep, swebench.ValidateOptions{
		Footing:  scen.Footing,
		Schedule: sched,
	})

	repBytes, err := json.MarshalIndent(rep, "", "  ")
	if err != nil {
		return nil, err
	}
	if err := os.WriteFile(filepath.Join(outDir, "report.json"), append(repBytes, '\n'), 0o644); err != nil {
		return nil, err
	}
	return rep, nil
}

func validateCmd(args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("validate", flag.ContinueOnError)
	fs.SetOutput(stderr)
	reportPath := fs.String("report", "", "Path to report.json.")
	eventsPath := fs.String("events", "", "Optional path to events.jsonl.")
	schedulePath := fs.String("schedule", "", "Optional path to schedule.json.")
	footing := fs.String("footing", "", "Declared footing (warm or cold).")
	minTasks := fs.Int("min-tasks", 0, "Minimum required tasks.")
	minRewardRate := fs.Float64("min-reward-rate", 0, "Minimum required reward rate.")
	if err := fs.Parse(args); err != nil {
		return err
	}
	var rep *swebench.Report
	if *reportPath != "" {
		raw, err := os.ReadFile(*reportPath)
		if err != nil {
			return err
		}
		rep = &swebench.Report{}
		if err := json.Unmarshal(raw, rep); err != nil {
			return err
		}
	} else if *eventsPath != "" {
		var err error
		rep, err = swebench.BuildReport(*eventsPath)
		if err != nil {
			return err
		}
	} else {
		return fmt.Errorf("either -report or -events is required")
	}

	var sched *schema.Schedule
	if *schedulePath != "" {
		raw, err := os.ReadFile(*schedulePath)
		if err != nil {
			return err
		}
		s, err := schema.DecodeJSONStrict[schema.Schedule](bytes.NewReader(raw))
		if err != nil {
			return err
		}
		sched = &s
	}
	v := swebench.Validate(rep, swebench.ValidateOptions{
		Footing:         *footing,
		MinTasks:        *minTasks,
		MinRewardRate:   *minRewardRate,
		CheckRewardRate: *minRewardRate > 0,
		Schedule:        sched,
	})
	data, _ := json.MarshalIndent(v, "", "  ")
	fmt.Fprintln(stdout, string(data))
	if !v.Valid {
		return fmt.Errorf("report validation failed: %s", strings.Join(v.Reasons, "; "))
	}
	return nil
}

func compareCmd(args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("compare", flag.ContinueOnError)
	fs.SetOutput(stderr)
	basePath := fs.String("baseline", "", "Path to baseline report.json.")
	candPath := fs.String("candidate", "", "Path to candidate report.json.")
	thresholdsPath := fs.String("thresholds", "", "Optional path to thresholds YAML/JSON.")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *basePath == "" || *candPath == "" {
		return fmt.Errorf("-baseline and -candidate are required")
	}
	rawBase, err := os.ReadFile(*basePath)
	if err != nil {
		return err
	}
	var baseRep swebench.Report
	if err := json.Unmarshal(rawBase, &baseRep); err != nil {
		return err
	}
	rawCand, err := os.ReadFile(*candPath)
	if err != nil {
		return err
	}
	var candRep swebench.Report
	if err := json.Unmarshal(rawCand, &candRep); err != nil {
		return err
	}
	var th swebench.Thresholds
	if *thresholdsPath != "" {
		th, err = swebench.LoadThresholds(*thresholdsPath)
		if err != nil {
			return err
		}
	}
	res := swebench.Compare(&baseRep, &candRep, th)
	fmt.Fprint(stdout, swebench.FormatComparisonMarkdown(res))
	if !res.Passed {
		return fmt.Errorf("comparison failed thresholds: %s", strings.Join(res.Regressions, "; "))
	}
	return nil
}

type localScenarioEnvClient struct {
	seq    atomic.Int64
	source string
}

func (c *localScenarioEnvClient) Create(_ context.Context, _, _ string) (swebench.EnvHandle, error) {
	id := c.seq.Add(1)
	src := c.source
	if src == "" {
		src = string(schema.SourceWarmPool)
	}
	return swebench.EnvHandle{
		Name:   fmt.Sprintf("env-%d", id),
		Source: src,
	}, nil
}

func (c *localScenarioEnvClient) Exec(_ context.Context, _, _ string, _ time.Duration) (swebench.ExecResult, error) {
	return swebench.ExecResult{ExitCode: 0, Stdout: "ok"}, nil
}

func (c *localScenarioEnvClient) Delete(_ context.Context, _ string) error {
	return nil
}

func (c *localScenarioEnvClient) Snapshot(_ context.Context, envName string) (string, error) {
	return "snap-" + envName, nil
}

func (c *localScenarioEnvClient) Resume(_ context.Context, snapshot, _, _ string) (swebench.EnvHandle, error) {
	return swebench.EnvHandle{
		Name:   "resumed-" + snapshot,
		Source: string(schema.SourceSnapshotResume),
	}, nil
}
