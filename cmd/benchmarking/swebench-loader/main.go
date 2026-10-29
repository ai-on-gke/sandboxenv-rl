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

// swebench-loader drives SWE-bench task images through the ate-env API as a
// simulated RL job and writes one JSONL event per operation.
//
//	swebench-loader run    -dataset manifest.jsonl -mode eval  -images 500 -tasks-per-image 6 -width 500 -out run.jsonl
//	swebench-loader run    -dataset manifest.jsonl -mode steps -images 500 -rollouts 2 -steps 8 -park suspend -out run.jsonl
//	swebench-loader report run.jsonl [-json summary.json]
//	swebench-loader report a.jsonl b.jsonl   (one table, a column per run)
//	swebench-loader report -arm pull=p1.jsonl,p2.jsonl -arm stream=s1.jsonl,s2.jsonl   (a column per arm, reps merged)
//	swebench-loader report run.jsonl -cache-before before.txt -cache-after after.txt -restore-log restores.jsonl
package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"net/url"
	"os"
	"os/signal"
	"path"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/agent-substrate/env/internal/benchmarking/schema"
	"github.com/agent-substrate/env/internal/benchmarking/swebench"
)

func main() {
	os.Exit(runMain(os.Args[1:], os.Stdout, os.Stderr))
}

func runMain(args []string, stdout, stderr io.Writer) int {
	if len(args) < 1 {
		usage(stderr)
		return 2
	}
	var err error
	switch args[0] {
	case "run":
		err = runCmd(args[1:])
	case "resolve":
		err = resolveCmd(args[1:])
	case "report", "compare":
		err = reportCmdWithIO(args[1:], stdout, stderr)
	case "trajectory-assets":
		err = assetsCmd(args[1:], stdout)
	case "-h", "--help", "help":
		usage(stderr)
		return 0
	default:
		usage(stderr)
		return 2
	}
	if err != nil {
		fmt.Fprintln(stderr, "error:", err)
		return 1
	}
	return 0
}

// authTokenEnv names the environment variable that holds the bearer token,
// the same one the ate-env CLI reads.
const authTokenEnv = "SUBSTRATE_ENV_AUTH_TOKEN"

// testBackendOverride allows hermetic end-to-end CLI tests in main_test.go to
// exercise runCmd without dialing a live gRPC server.
var testBackendOverride swebench.Backend

// resolveAuthToken returns the token from file when file is set, otherwise
// from authTokenEnv, otherwise "" (no token sent). A file that holds only
// whitespace is an error, because it is a misconfiguration and not a request
// to run without the token.
func resolveAuthToken(file string, getenv func(string) string) (string, error) {
	if file == "" {
		return strings.TrimSpace(getenv(authTokenEnv)), nil
	}
	b, err := os.ReadFile(file)
	if err != nil {
		return "", fmt.Errorf("reading -auth-token-file: %w", err)
	}
	token := strings.TrimSpace(string(b))
	if token == "" {
		return "", fmt.Errorf("-auth-token-file %s is empty", file)
	}
	return token, nil
}

// labelFlags collects repeated KEY=VALUE flags (-label and -set on run).
type labelFlags map[string]string

func (l labelFlags) String() string {
	keys := make([]string, 0, len(l))
	for k := range l {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, k+"="+l[k])
	}
	return strings.Join(parts, ",")
}

func (l labelFlags) Set(v string) error {
	k, val, ok := strings.Cut(v, "=")
	if !ok || k == "" {
		return fmt.Errorf("want KEY=VALUE, got %q", v)
	}
	if strings.ContainsAny(v, "\t\n") {
		return fmt.Errorf("%q: no tabs or newlines, they break the report's table", v)
	}
	if _, dup := l[k]; dup {
		return fmt.Errorf("%s is given twice", k)
	}
	l[k] = val
	return nil
}

// arm is one -arm value: a column of the comparison and the files pooled
// into it.
type arm struct {
	name  string
	files []string
}

// armFlags collects repeated -arm NAME=FILE[,FILE...] flags in command-line
// order, so the baseline given first is the first column.
type armFlags []arm

func (a *armFlags) String() string {
	parts := make([]string, 0, len(*a))
	for _, x := range *a {
		parts = append(parts, x.name+"="+strings.Join(x.files, ","))
	}
	return strings.Join(parts, " ")
}

func (a *armFlags) Set(v string) error {
	name, list, ok := strings.Cut(v, "=")
	if !ok || name == "" || strings.TrimSpace(list) == "" {
		return fmt.Errorf("want NAME=FILE[,FILE...], got %q", v)
	}
	for _, x := range *a {
		if x.name == name {
			return fmt.Errorf("arm %s is given twice", name)
		}
	}
	var files []string
	for _, f := range strings.Split(list, ",") {
		f = strings.TrimSpace(f)
		if f == "" {
			return fmt.Errorf("arm %s: empty file name in %q", name, list)
		}
		if slices.Contains(files, f) {
			return fmt.Errorf("arm %s: %s is given twice", name, f)
		}
		files = append(files, f)
	}
	*a = append(*a, arm{name, files})
	return nil
}

// datasetLabel is the dataset's file name. For an https URL that is the last
// path element without the query, so a signed URL's signature stays out of
// the events file, which operators share.
func datasetLabel(dataset string) string {
	if u, err := url.Parse(dataset); err == nil && u.Scheme != "" && u.Host != "" {
		return path.Base(u.Path)
	}
	return filepath.Base(dataset)
}

// ownLabels are the loader's labels that only the command line knows (the
// rest come from Config.labels); every key is in swebench.LoaderLabelKeys or
// is a var's effective value under set.KEY. The exclude file and the
// instance list are named by their file name or count plus a digest of their
// ids, so two lists of one length differ.
func ownLabels(dataset string, connections int, baseTemplate, excludeFile string, exclude, instances []string, vars map[string]string) map[string]string {
	l := map[string]string{
		"dataset":       datasetLabel(dataset),
		"connections":   strconv.Itoa(connections),
		"base_template": baseTemplate,
	}
	for k, v := range vars {
		l["set."+k] = v
	}
	if excludeFile != "" {
		l["exclude_file"] = filepath.Base(excludeFile) + "@" + idsDigest(exclude)
	}
	if len(instances) > 0 {
		l["instances"] = strconv.Itoa(len(instances)) + "@" + idsDigest(instances)
	}
	return l
}

// idsDigest is the first eight hex digits of the SHA-256 of the sorted ids.
func idsDigest(ids []string) string {
	sorted := slices.Clone(ids)
	slices.Sort(sorted)
	sum := sha256.Sum256([]byte(strings.Join(sorted, "\n")))
	return hex.EncodeToString(sum[:4])
}

func usage(w io.Writer) {
	fmt.Fprintln(w, "usage: swebench-loader run [flags] | swebench-loader resolve [flags] | swebench-loader report FILE... [-json OUT] | swebench-loader trajectory-assets FILE")
}

// assetsCmd prints the files a trajectory's write steps name, one per line,
// for run-in-cluster.sh to bake next to it.
func assetsCmd(args []string, w io.Writer) error {
	if len(args) != 1 {
		return errors.New("usage: swebench-loader trajectory-assets FILE")
	}
	names, err := swebench.TrajectoryAssets(args[0])
	if err != nil {
		return err
	}
	for _, n := range names {
		fmt.Fprintln(w, n)
	}
	return nil
}

func resolveCmd(args []string) error {
	fs := flag.NewFlagSet("resolve", flag.ContinueOnError)
	var (
		dataset      = fs.String("dataset", "", "JSONL manifest of instances; a path or an https URL. Required.")
		images       = fs.Int("images", 0, "Use the first N instances of the dataset; 0 means all.")
		instances    = fs.String("instances", "", "Comma-separated instance ids to use instead of the first N.")
		excludeFile  = fs.String("exclude-file", "", "File of instance ids to leave out, one per line.")
		steps        = fs.Int("steps", 8, "Number of training steps.")
		tasksPerStep = fs.Int("tasks-per-step", 0, "Tasks sampled per step; 0 means all selected instances.")
		groupSize    = fs.Int("group-size", 2, "Group size (rollouts) per sampled instance.")
		park         = fs.String("park", string(schema.ParkSuspendAsync), "Park policy: suspend-async, suspend-sync, suspend, fork, delete, or none.")
		seed         = fs.Uint64("seed", 42, "Deterministic RNG seed for step sampling.")
		runID        = fs.String("run-id", "swe-sched", "Job / schedule ID.")
		out          = fs.String("out", "schedule.json", "Output path for the resolved schedule JSON.")
	)
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
	var ids []string
	if *instances != "" {
		ids = strings.Split(*instances, ",")
	}
	var exclude []string
	if *excludeFile != "" {
		if exclude, err = swebench.ReadIDList(*excludeFile); err != nil {
			return err
		}
	}
	selected, err := swebench.Select(all, ids, exclude, *images)
	if err != nil {
		return err
	}
	sched, err := swebench.ResolveSingleJobSchedule(*runID, *seed, schema.ParkPolicy(*park), *steps, *tasksPerStep, *groupSize, selected)
	if err != nil {
		return err
	}
	return swebench.WriteSchedule(*out, sched)
}

func runCmd(args []string) (retErr error) {
	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	var (
		endpoint      = fs.String("endpoint", "localhost:7777", "ate-env API address (host:port).")
		connections   = fs.Int("connections", 4, "gRPC connections to the API, handed out round-robin per environment; a Service balances per connection, so use at least the number of API replicas.")
		atespace      = fs.String("atespace", "ate-env", "Atespace the environments are created in.")
		baseTemplate  = fs.String("base-template", "default-template", "Base ActorTemplate the per-image templates derive from (ate-env manifest template).")
		dataset       = fs.String("dataset", "", "JSONL manifest of SWE-bench instances; a path or an https URL. Required.")
		images        = fs.Int("images", 0, "Use the first N instances of the dataset; 0 means all.")
		instances     = fs.String("instances", "", "Comma-separated instance ids to use instead of the first N.")
		excludeFile   = fs.String("exclude-file", "", "File of instance ids to leave out, one per line; text after the id is a comment.")
		trajectory    = fs.String("trajectory", swebench.DefaultTrajectory, fmt.Sprintf("Trajectory file or built-in name (%s); a comma-separated list mixes them, one per environment, drawn with -seed.", strings.Join(swebench.Names(), ", ")))
		seed          = fs.Int64("seed", 1, "Seed for the trajectory mix, so reps assign the same trajectory to the same environment.")
		mode          = fs.String("mode", string(swebench.ModeEval), "eval (images x tasks-per-image, each create/replay/delete) or steps (images x rollouts kept across steps).")
		width         = fs.Int("width", 50, "Most tasks in flight at once.")
		tasksPerImage = fs.Int("tasks-per-image", 1, "eval: replays per image.")
		rollouts      = fs.Int("rollouts", 2, "steps: environments per image.")
		steps         = fs.Int("steps", 8, "steps: training steps.")
		park          = fs.String("park", string(swebench.ParkSuspend), "steps: what happens between steps: suspend, suspend-sync, suspend-async, fork, delete or none.")
		imagesPerStep = fs.Int("images-per-step", 0, "steps: each step takes the next N images (wrapping around the dataset) with fresh environments, retired after the step: the create-and-retire-pools-per-step shape; needs -park delete. 0 keeps one fleet across the steps.")
		schedulePath  = fs.String("schedule", "", "steps: path to a pre-resolved schedule.json.")
		resolveOut    = fs.String("resolve-out", "", "steps: write the resolved schedule.json to this path before running.")
		tasksPerStep  = fs.Int("tasks-per-step", 0, "steps: tasks sampled per step when resolving a schedule (0 means all selected instances).")
		groupSize     = fs.Int("group-size", 0, "steps: group size per sampled task when resolving a schedule (0 uses -rollouts).")
		stepPause     = fs.Duration("step-pause", 0, "steps: simulated trainer pause between steps.")
		prefetch      = fs.Bool("prefetch", false, "steps: asynchronously prewarm next step's prefetch_images off the critical path.")
		createTimeout = fs.Duration("create-timeout", 5*time.Minute, "Deadline for one create call.")
		stepTimeout   = fs.Duration("step-timeout", 10*time.Minute, "Default deadline for a run step without its own timeout.")
		wakeBudget    = fs.Duration("wake-budget", 3*time.Minute, "How long a task's first command is retried while the environment is still waking or its image is still being baked.")
		wakeBackoff   = fs.Duration("wake-backoff", swebench.DefaultWakeBackoff, "Initial pause between wake retries; doubles up to five times this.")
		runID         = fs.String("run-id", "", "Run id, prefix of every environment name: lowercase letters, digits and '-', at most 20 characters; default swe-<unix time>.")
		out           = fs.String("out", "", "JSONL events file; default <run-id>.jsonl in the working directory.")
		eventsDir     = fs.String("events-dir", "", "Optional base directory for per-worker event shards (events/<worker>.jsonl).")
		worker        = fs.String("worker", "", "Optional worker shard name stamped on emitted events.")
		jobName       = fs.String("job", "", "Optional job name stamped on emitted events.")
		runWindow     = fs.String("run-window", "", "Optional path to write run_window.json.")
		statsOut      = fs.String("stats-out", "", "Optional path to write flat stats.jsonl rows after the run.")
		testName      = fs.String("test-name", "", "Optional test_name for run_window.json and stats.jsonl.")
		tag           = fs.String("tag", "", "Optional tag for run_window.json and stats.jsonl.")
		validate      = fs.Bool("validate", false, "Evaluate Validity Rules 1-7 after the run and fail if invalid.")
		keep          = fs.Bool("keep", false, "Leave the environments in place when the run ends.")
		dryRun        = fs.Bool("dry-run", false, "Load and render everything, print the plan and exit without touching the API.")
		authTokenFile = fs.String("auth-token-file", "", "File that holds the bearer token for ate-env-api; surrounding whitespace is ignored. Without it, SUBSTRATE_ENV_AUTH_TOKEN is used when set.")
		labels        = labelFlags{}
		vars          = labelFlags{}
	)
	fs.Var(vars, "set", "KEY=VALUE for the trajectories' var function, repeatable, e.g. READ_FILE=/testbed/setup.py or COMPILE_DIR=/testbed; a workload's defaults are in its file. A key no trajectory reads is an error; each value is recorded as a set.KEY label.")
	fs.Var(labels, "label", "KEY=VALUE recorded on the run event, repeatable: the Substrate commit, the cluster, the arm. The loader records its own flags itself (mode, width, images, trajectory, dataset, the shape flags, connections, base-template, exclude-file, instances and the timeouts); a -label with one of those keys is refused, so report -arm can trust them.")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *dataset == "" {
		return fmt.Errorf("-dataset is required")
	}
	authToken, err := resolveAuthToken(*authTokenFile, os.Getenv)
	if err != nil {
		return err
	}
	if *runID == "" {
		*runID = fmt.Sprintf("swe-%d", time.Now().Unix())
	}
	if *eventsDir != "" {
		wName := *worker
		if wName == "" {
			wName = "worker-0"
		}
		*out = swebench.ShardEventPath(*eventsDir, wName)
	} else if *out == "" {
		*out = *runID + ".jsonl"
	}
	log := slog.New(slog.NewJSONHandler(os.Stderr, nil))

	all, err := swebench.LoadDataset(*dataset)
	if err != nil {
		return err
	}
	var ids []string
	if *instances != "" {
		ids = strings.Split(*instances, ",")
	}
	var exclude []string
	if *excludeFile != "" {
		if exclude, err = swebench.ReadIDList(*excludeFile); err != nil {
			return err
		}
	}
	selected, err := swebench.Select(all, ids, exclude, *images)
	if err != nil {
		return err
	}
	if *images > 0 && len(selected) < *images && len(ids) == 0 {
		fmt.Fprintf(os.Stderr, "note: %d instances requested, %d available after the exclude list\n", *images, len(selected))
	}
	trajSpec := *trajectory
	if trajSpec == swebench.DefaultTrajectory && len(selected) > 0 && selected[0].Kind == swebench.CommandKind {
		trajSpec = swebench.CommandTrajectoryName
	}
	var trajs []*swebench.Trajectory
	for _, source := range strings.Split(trajSpec, ",") {
		if source = strings.TrimSpace(source); source == "" {
			continue
		}
		t, err := swebench.LoadTrajectory(source)
		if err != nil {
			return err
		}
		t.Vars = vars
		if err := swebench.RequireGrading(selected, t); err != nil {
			return err
		}
		for _, in := range selected {
			if _, err := t.Render(in); err != nil {
				return err
			}
		}
		trajs = append(trajs, t)
	}
	if len(trajs) == 0 {
		return fmt.Errorf("-trajectory names no trajectory")
	}
	// The value each var resolved to, defaults included, goes on the run
	// event as a set.KEY label, so a run on the defaults and a run that
	// spelled the same values out pool together. A -set key no trajectory
	// reads is a typo, or a knob of another workload; with a default in
	// place it would pass unnoticed.
	effective := map[string]string{}
	if len(selected) > 0 {
		for _, t := range trajs {
			u, err := t.VarsUsed(selected[0])
			if err != nil {
				return err
			}
			maps.Copy(effective, u)
		}
	}
	for _, k := range slices.Sorted(maps.Keys(vars)) {
		if _, ok := effective[k]; !ok {
			return fmt.Errorf("-set %s: no trajectory reads it (they read: %s)", k, strings.Join(slices.Sorted(maps.Keys(effective)), ", "))
		}
	}
	traj := trajs[0]

	var sched *schema.Schedule
	if *schedulePath != "" {
		sched, err = swebench.ReadSchedule(*schedulePath)
		if err != nil {
			return err
		}
	} else if *resolveOut != "" {
		gs := *groupSize
		if gs <= 0 {
			gs = *rollouts
		}
		sched, err = swebench.ResolveSingleJobSchedule(*runID, uint64(*seed), schema.ParkPolicy(*park), *steps, *tasksPerStep, gs, selected)
		if err != nil {
			return err
		}
	}
	if *resolveOut != "" && sched != nil {
		if err := swebench.WriteSchedule(*resolveOut, sched); err != nil {
			return err
		}
	}

	fanout := *tasksPerImage
	if swebench.Mode(*mode) == swebench.ModeSteps {
		fanout = *rollouts
	}
	environments := len(selected) * fanout
	if *imagesPerStep > 0 {
		environments = *imagesPerStep * fanout // per step
	}
	log.Info("plan", slog.String("run", *runID), slog.String("mode", *mode), slog.Int("images", len(selected)),
		slog.Int("images_per_step", *imagesPerStep),
		slog.Int("environments", environments), slog.Int("width", *width), slog.Int("steps", *steps),
		slog.String("park", *park), slog.String("trajectory", *trajectory), slog.String("out", *out))
	if *dryRun {
		return nil
	}

	var backend swebench.Backend
	if testBackendOverride != nil {
		backend = testBackendOverride
	} else {
		eb, err := swebench.NewEnvBackend(*endpoint, *atespace, *baseTemplate, *connections, authToken)
		if err != nil {
			return err
		}
		defer eb.Close()
		backend = eb
	}

	var rec *swebench.Recorder
	var f *os.File
	if *eventsDir != "" {
		rec, err = swebench.NewShardRecorder(*eventsDir, *runID, *worker)
		if err != nil {
			return err
		}
	} else {
		f, err = os.Create(*out)
		if err != nil {
			return err
		}
		rec = swebench.NewRecorder(f)
		if *worker != "" {
			rec.SetWorker(*worker)
		}
	}
	if *jobName != "" {
		rec.SetJob(*jobName)
	}
	// Events lost to a write error, or a file that does not flush, must fail
	// the run rather than produce a report with holes.
	defer func() {
		if rerr := rec.Close(); rerr != nil && retErr == nil {
			retErr = fmt.Errorf("writing events to %s: %w", *out, rerr)
		}
		if f != nil {
			if cerr := f.Close(); cerr != nil && retErr == nil {
				retErr = fmt.Errorf("closing %s: %w", *out, cerr)
			}
		}
	}()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	startTS := time.Now().UTC()
	sum, err := swebench.Run(ctx, swebench.Config{
		RunID:         *runID,
		Dataset:       *dataset,
		Backend:       backend,
		Instances:     selected,
		Trajectory:    traj,
		Trajectories:  trajs,
		Seed:          *seed,
		Recorder:      rec,
		Logger:        log,
		Mode:          swebench.Mode(*mode),
		Width:         *width,
		TasksPerImage: *tasksPerImage,
		Rollouts:      *rollouts,
		Steps:         *steps,
		Park:          swebench.Park(*park),
		ImagesPerStep: *imagesPerStep,
		Schedule:      sched,
		TasksPerStep:  *tasksPerStep,
		GroupSize:     *groupSize,
		StepPause:     *stepPause,
		Prefetch:      *prefetch,
		CreateTimeout: *createTimeout,
		StepTimeout:   *stepTimeout,
		WakeBudget:    *wakeBudget,
		WakeBackoff:   *wakeBackoff,
		Keep:          *keep,
		Labels:        labels,
		Invocation:    ownLabels(*dataset, *connections, *baseTemplate, *excludeFile, exclude, ids, effective),
	})
	endTS := time.Now().UTC()
	_ = rec.Flush()

	if *runWindow != "" {
		tName := *testName
		if tName == "" {
			tName = "swebench-" + *mode
		}
		rw := schema.RunWindow{
			SchemaVersion: schema.SchemaVersion,
			RunID:         *runID,
			TestName:      tName,
			Tag:           *tag,
			StartTS:       startTS.Format(time.RFC3339Nano),
			EndTS:         endTS.Format(time.RFC3339Nano),
			SUTEndpoint:   *endpoint,
		}
		if wErr := swebench.WriteRunWindow(*runWindow, rw); wErr != nil {
			return wErr
		}
	}

	if *statsOut != "" || *validate {
		rep, sErr := swebench.SummarizeFile(*out)
		if sErr != nil {
			return sErr
		}
		if *statsOut != "" {
			if wErr := swebench.WriteStatsJSONL(*statsOut, rep, *runID, *testName, *tag); wErr != nil {
				return wErr
			}
		}
		if *validate {
			v := swebench.Validate(rep, swebench.ValidateOptions{})
			if !v.Valid {
				return fmt.Errorf("report validation failed: %s", strings.Join(v.Reasons, "; "))
			}
		}
	}

	if sum != nil {
		log.Info("run finished", slog.Int("tasks", sum.Tasks), slog.Int("failed", sum.Failed),
			slog.Int("rewarded", sum.Rewarded), slog.Duration("duration", sum.Duration))
	}
	if err != nil {
		return err
	}
	if sum.Failed > 0 {
		return fmt.Errorf("%d of %d tasks failed", sum.Failed, sum.Tasks)
	}
	return nil
}

func reportCmd(args []string, ws ...io.Writer) error {
	var stdout io.Writer = os.Stdout
	if len(ws) > 0 && ws[0] != nil {
		stdout = ws[0]
	}
	return reportCmdWithIO(args, stdout, os.Stderr)
}

func reportCmdWithIO(args []string, stdout, stderr io.Writer) error {
	files, rest := leadingFiles(args)
	fs := flag.NewFlagSet("report", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var arms armFlags
	fs.Var(&arms, "arm", "NAME=FILE[,FILE...], repeatable: one column per arm, in flag order, its percentiles computed over the merged raw events of its files, the reps of one cluster or build, which must be complete runs of one workload (same loader labels, distinct run ids, a run in one arm only). A path may not contain a comma. No positional files with -arm.")
	var (
		top            = fs.Int("top", 0, "List the N slowest successful first commands and wakes by environment. One file only.")
		in             = fs.String("in", "", "Input events.jsonl file (alternative to positional FILE).")
		eventsGlob     = fs.String("events-glob", "", "Glob pattern for multi-worker event shards (e.g. 'events/*.jsonl').")
		jsonOut        = fs.String("json", "", "Also write the summary as JSON to this path (an array when several files are given).")
		out            = fs.String("out", "", "Alias for -json: write the summary as JSON to this path.")
		mdOut          = fs.String("md", "", "Optional path to write Markdown report (including ## Validity).")
		unresolved     = fs.String("unresolved", "", "Write the unresolved instances to this path in -exclude-file format; one file only.")
		unresNote      = fs.String("unresolved-note", "Instances graded unresolved", "First comment line of the -unresolved file.")
		cacheBefore    = fs.String("cache-before", "", "Node cache sample taken before the run (benchmarking/swebench/sample-node-cache.sh); with -cache-after, the report adds the run's cache misses. One file only.")
		cacheAfter     = fs.String("cache-after", "", "Node cache sample taken after the run.")
		restoreLog     = fs.String("restore-log", "", "atelet restore records for the run (benchmarking/swebench/harvest-restore-timings.sh); the report adds the restore phases per environment. One file only.")
		restoreTimings = fs.String("restore-timings", "", "Alias for -restore-log: restore records for the run.")
		runWindow      = fs.String("run-window", "", "Optional path to run_window.json to validate and attach to the report.")
		statsOut       = fs.String("stats-out", "", "Optional path to write flat stats.jsonl rows.")
		testName       = fs.String("test-name", "", "Optional test_name for stats.jsonl (default: swebench-<mode>).")
		tag            = fs.String("tag", "", "Optional tag for stats.jsonl.")
		validate       = fs.Bool("validate", false, "Evaluate Validity Rules 1-7 and exit non-zero on failure.")
		footing        = fs.String("footing", "", "Expected footing for Validity Rule 1 (warm | cold).")
		minReward      = fs.Float64("min-reward-rate", 0, "Minimum reward rate for Validity Rule 2.")
		minTasks       = fs.Int("min-tasks", 0, "Minimum task count for Validity Rule 3.")
		maxFailCr      = fs.Float64("max-failed-create-ratio", -1, "Maximum failed create ratio for Validity Rule 4.")
		schedPath      = fs.String("schedule", "", "Optional schedule.json path for Validity Rule 5.")
		traceMode      = fs.Bool("trace-mode", false, "Mark scenario as having a trace-mode job (Validity Rule 6).")
		orphanCount    = fs.Int("orphan-count", 0, "Remaining orphan count after reap (Validity Rule 7).")
		compareFlag    = fs.String("compare", "", "Comma-separated events.jsonl or report.json files to compare against.")
		thresholds     = fs.String("thresholds", "", "Optional thresholds.yaml path for regression gating.")
	)
	if err := fs.Parse(rest); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		// flag stops at the first non-flag, so a file after a flag would be
		// dropped without a word.
		return fmt.Errorf("unexpected arguments after the flags: %s; put every events file before the flags", strings.Join(fs.Args(), " "))
	}
	if *top < 0 {
		return fmt.Errorf("-top wants 0 or more, got %d", *top)
	}
	if *out != "" && *jsonOut == "" {
		*jsonOut = *out
	}
	if *restoreTimings != "" && *restoreLog == "" {
		*restoreLog = *restoreTimings
	}
	if len(arms) > 0 {
		if len(files) > 0 || *in != "" {
			return fmt.Errorf("with -arm, give every events file inside an -arm value")
		}
		if *unresolved != "" || *cacheBefore != "" || *cacheAfter != "" || *restoreLog != "" || *top > 0 {
			return fmt.Errorf("-unresolved, -cache-before, -cache-after, -restore-log and -top take one events file, not -arm")
		}
		return reportArms(arms, *jsonOut, stdout)
	}
	if *in != "" {
		files = append(files, *in)
	}

	var compareFiles []string
	if *compareFlag != "" {
		for _, p := range strings.Split(*compareFlag, ",") {
			if p = strings.TrimSpace(p); p != "" {
				compareFiles = append(compareFiles, p)
			}
		}
	}

	if len(files) == 0 && *eventsGlob == "" && len(compareFiles) == 0 {
		return fmt.Errorf("report needs at least one JSONL file, or -arm")
	}

	var rw *schema.RunWindow
	if *runWindow != "" {
		loadedRW, err := swebench.ReadRunWindow(*runWindow)
		if err != nil {
			return err
		}
		rw = loadedRW
	}

	var reps []*swebench.Report
	var labels []string
	var events []swebench.Event // of the single file, for the restore join

	if *eventsGlob != "" {
		rep, err := swebench.SummarizeGlob(*eventsGlob)
		if err != nil {
			return err
		}
		reps = append(reps, rep)
		labels = append(labels, *eventsGlob)
	}

	for _, path := range files {
		if (*restoreLog != "" || *top > 0) && len(files) == 1 && *eventsGlob == "" && len(compareFiles) == 0 {
			evs, err := readEventsFile(path)
			if err != nil {
				return fmt.Errorf("%s: %w", path, err)
			}
			events = evs
			reps = append(reps, swebench.Build(evs))
			labels = append(labels, path)
			continue
		}
		rep, err := loadOrSummarizeFile(path)
		if err != nil {
			return fmt.Errorf("%s: %w", path, err)
		}
		reps = append(reps, rep)
		labels = append(labels, path)
	}

	// If -compare was provided along with a single primary report (-in / -events-glob / single file)
	// and len(compareFiles) == 1, treat compareFiles[0] as baseline and the primary report as candidate.
	if len(compareFiles) > 0 {
		var compReps []*swebench.Report
		for _, p := range compareFiles {
			r, err := loadOrSummarizeFile(p)
			if err != nil {
				return fmt.Errorf("%s: %w", p, err)
			}
			compReps = append(compReps, r)
		}
		if len(reps) == 1 && len(compReps) == 1 {
			reps = []*swebench.Report{compReps[0], reps[0]}
			labels = []string{compareFiles[0], labels[0]}
		} else {
			reps = append(reps, compReps...)
			labels = append(labels, compareFiles...)
		}
	}

	if len(reps) > 1 {
		// Several runs: one table, a column per run, for the same workload on
		// different clusters or builds.
		if *unresolved != "" || (*cacheBefore != "" && !*validate) || (*cacheAfter != "" && !*validate) || *restoreLog != "" || *top > 0 {
			return fmt.Errorf("-unresolved, -cache-before, -cache-after, -restore-log and -top take one events file")
		}
		swebench.WriteComparison(stdout, reps, labels)
		if *thresholds != "" {
			th, err := swebench.LoadThresholds(*thresholds)
			if err != nil {
				return err
			}
			res := swebench.Compare(reps[0], reps[len(reps)-1], th)
			fmt.Fprintln(stdout)
			fmt.Fprint(stdout, swebench.FormatComparisonMarkdown(res))
			if *mdOut != "" {
				if err := os.WriteFile(*mdOut, []byte(swebench.FormatComparisonMarkdown(res)), 0o644); err != nil {
					return err
				}
			}
			if !res.Passed {
				return fmt.Errorf("threshold regression detected: %s", strings.Join(res.Regressions, "; "))
			}
		}
		if *jsonOut != "" {
			b, err := json.MarshalIndent(reps, "", "  ")
			if err != nil {
				return err
			}
			return os.WriteFile(*jsonOut, b, 0o644)
		}
		return nil
	}

	rep := reps[0]
	if rw != nil {
		rep.RunWindow = rw
		if rep.RunID == "" {
			rep.RunID = rw.RunID
		}
	}
	if (*cacheBefore == "") != (*cacheAfter == "") {
		return fmt.Errorf("-cache-before and -cache-after go together")
	}
	if *cacheBefore != "" {
		before, errB := loadCacheSample(*cacheBefore)
		after, errA := loadCacheSample(*cacheAfter)
		if errB == nil && errA == nil {
			d := before.Delta(after)
			rep.Cache = &d
			rep.PopulateCacheMissesPerFirstCmd()
		} else if !*validate {
			if errB != nil {
				return errB
			}
			return errA
		}
	}
	if *restoreLog != "" {
		f, err := os.Open(*restoreLog)
		if err != nil {
			return err
		}
		timings, err := swebench.ParseRestoreTimings(f)
		f.Close()
		if err != nil {
			return fmt.Errorf("%s: %w", *restoreLog, err)
		}
		rep.Restores = swebench.JoinRestores(events, timings)
	}
	if *top > 0 {
		rep.Slowest = swebench.Slowest(events, *top)
	}

	var valErr error
	if *validate {
		valOpts := swebench.ValidateOptions{
			Footing:         *footing,
			MinRewardRate:   *minReward,
			CheckRewardRate: *minReward > 0,
			MinTasks:        *minTasks,
			HasTraceJob:     *traceMode,
			OrphanCount:     *orphanCount,
			CacheBeforePath: *cacheBefore,
			CacheAfterPath:  *cacheAfter,
		}
		if *maxFailCr >= 0 {
			valOpts.MaxFailedCreateRatio = *maxFailCr
			valOpts.CheckFailedCreates = true
		}
		if *schedPath != "" {
			rawSched, sErr := os.ReadFile(*schedPath)
			if sErr != nil {
				return fmt.Errorf("read schedule %q: %w", *schedPath, sErr)
			}
			sched, sErr := schema.DecodeJSONStrict[schema.Schedule](bytes.NewReader(rawSched))
			if sErr != nil {
				return fmt.Errorf("decode schedule %q: %w", *schedPath, sErr)
			}
			valOpts.Schedule = &sched
		}
		validity := swebench.Validate(rep, valOpts)
		if !validity.Valid {
			valErr = fmt.Errorf("report validation failed: %s", strings.Join(validity.Reasons, "; "))
		}
	}

	rep.WriteText(stdout)
	if *mdOut != "" {
		var mdBuf bytes.Buffer
		rep.WriteMarkdown(&mdBuf)
		if err := os.WriteFile(*mdOut, mdBuf.Bytes(), 0o644); err != nil {
			return err
		}
	}
	if *unresolved != "" {
		uf, err := os.Create(*unresolved)
		if err != nil {
			return err
		}
		rep.WriteExclusions(uf, *unresNote)
		if err := uf.Close(); err != nil {
			return err
		}
	}
	if *statsOut != "" {
		if err := swebench.WriteStatsJSONL(*statsOut, rep, rep.RunID, *testName, *tag); err != nil {
			return err
		}
	}
	if *jsonOut != "" {
		b, err := json.MarshalIndent(rep, "", "  ")
		if err != nil {
			return err
		}
		if err := os.WriteFile(*jsonOut, b, 0o644); err != nil {
			return err
		}
	}
	return valErr
}

// leadingFiles splits report's arguments into the events files, which come
// first, and the flags that follow them.
func leadingFiles(args []string) (files, rest []string) {
	for i, a := range args {
		if strings.HasPrefix(a, "-") {
			return args[:i], args[i:]
		}
	}
	return args, nil
}

func summarizeFile(path string) (*swebench.Report, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return swebench.Summarize(f)
}

func loadOrSummarizeFile(path string) (*swebench.Report, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) > 0 && trimmed[0] == '{' && !bytes.Contains(trimmed, []byte("\n{")) {
		var rep swebench.Report
		if err := json.Unmarshal(trimmed, &rep); err == nil && (len(rep.Stats) > 0 || rep.Tasks > 0 || len(rep.Jobs) > 0) {
			swebench.NormalizeLegacyReport(&rep)
			return &rep, nil
		}
	}
	return swebench.Summarize(bytes.NewReader(raw))
}
func readEventsFile(path string) ([]swebench.Event, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return swebench.ReadEvents(f)
}

func loadCacheSample(path string) (*swebench.NodeCacheSample, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	s, err := swebench.ParseNodeCacheSample(f)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return s, nil
}

// reportArms prints one column per arm, each pooled over its files, so three
// reps of one cluster become one set of exact percentiles rather than three
// columns to read by eye.
func reportArms(arms armFlags, jsonOut string, w io.Writer) error {
	names := make([]string, 0, len(arms))
	reps := make([]*swebench.Report, 0, len(arms))
	owner := map[string]string{} // run id -> the arm that has it
	for _, a := range arms {
		runs := make([][]swebench.Event, 0, len(a.files))
		for _, file := range a.files {
			evs, err := readEventsFile(file)
			if err != nil {
				return fmt.Errorf("arm %s: %s: %w", a.name, file, err)
			}
			runs = append(runs, evs)
		}
		rep, err := swebench.Pool(runs)
		if err != nil {
			return fmt.Errorf("arm %s: %w", a.name, err)
		}
		for _, id := range rep.Runs {
			if other, dup := owner[id]; dup {
				return fmt.Errorf("run %s is in arms %s and %s; a run belongs to one arm", id, other, a.name)
			}
			owner[id] = a.name
		}
		rep.RunID = a.name
		names = append(names, a.name)
		reps = append(reps, rep)
	}
	swebench.WriteComparison(w, reps, names)
	if jsonOut != "" {
		b, err := json.MarshalIndent(reps, "", "  ")
		if err != nil {
			return err
		}
		return os.WriteFile(jsonOut, b, 0o644)
	}
	return nil
}
