# Substrate RL Benchmark: Technical Implementation Plan

> **Note**: Authoritative system requirements, artifact contracts, and validity
> rules live in [`DESIGN.md`](./DESIGN.md). This `PLAN.md` is a technical
> implementation plan generated from `DESIGN.md` and may change as needed during
> implementation. It does not repeat the specification in `DESIGN.md`.

---

## 1. Baseline State on `main`

The following building blocks are already merged on `main` and serve as the
foundation for all remaining work:

| Area | Existing Files on `main` | Capabilities Already Present |
|---|---|---|
| **Load generator & single-job runner** | `cmd/benchmarking/swebench-loader/main.go`<br>`internal/benchmarking/swebench/{backend,dataset,events,report,run,trajectory}.go` | Closed-loop `eval` and fixed-fleet `steps` execution against `ate-env-api` (`EnvBackend` with bearer-token auth); Go template trajectory rendering; SWE-bench Python grader (`trajectories/swebench-grade.py`). |
| **Multi-run comparison** | `internal/benchmarking/swebench/report.go` (`WriteComparison`)<br>`cmd/benchmarking/swebench-loader/main.go` (`-compare`) | Side-by-side markdown diff of 2+ `report.json` files across task outcomes, step wall times, and operation latency percentiles. |
| **Node image-cache attribution** | `benchmarking/swebench/sample-node-cache.sh`<br>`internal/benchmarking/swebench/report.go` (`NodeCacheSample`, `CacheDelta`) | Before/after node `atelet` image-cache counter harvesting (`-cache-before`, `-cache-after`) and report delta calculation. |
| **Server restore-phase breakdown** | `benchmarking/swebench/harvest-restore-timings.sh`<br>`internal/benchmarking/swebench/report.go` (`RestoreTiming`, `JoinRestores`) | Harvesting `atelet` structured restore logs (`-restore-log`) and splitting client `ttfi` (first restore) and `wake` (subsequent restores) into `manifest_fetch`, `download`, `oci_unpack`, `worker_restore`, and `outside`. |
| **Python SDK OTel spans** | `clients/python/src/ate_env/telemetry/tracing.py` | OpenTelemetry `ate_env_client.*` client/handle/driver spans with `ate_env.first_command`, `ate_env.exit_code`, `ate_env.timed_out`, and `ate_env.file.size` attributes. |
| **Python SDK fleet observability & Prometheus metrics** | `clients/python/src/ate_env/observability.py`<br>`clients/python/src/ate_env/telemetry/metrics.py` | `Observer` and dependency-free `RunReport`; lazy Prometheus histograms/counters (`ate_env_fleet_{phase,task,run}_latency_seconds`, `ate_env_fleet_{claims,tasks}_total`, `ate_env_fleet_warm_replicas`, `ate_env_client_{acquire,exec,suspend,warm_claim,release}_seconds`); OTel `ate_env_fleet.<phase>` parent spans. |

---

## 2. Final Repository & Package Structure

Rather than creating a parallel load-generator tree, `internal/benchmarking/swebench` evolves in-place as the core execution, working-set, reporting, validation, and comparison library, while sibling packages under `internal/benchmarking/` implement orthogonal `DESIGN.md` stages that communicate through `$ARTIFACT_DIR` file contracts.

```text
cmd/
├── ate-env-api/
│   └── main.go                           # Extended with Suspend DurabilityMode (async/sync) & ForkEnvironment RPCs
└── benchmarking/
    ├── swebench-loader/
    │   └── main.go                       # Load generator + standalone subcommands (resolve, convert-trace, compare)
    └── bench/
        └── main.go                       # Composable orchestrator CLI (resolve, render, prewarm, measure, reap, report, run)

internal/
├── apiservice/
│   └── server.go                         # Plumbs DurabilityMode and ForkEnvironment to internal/ate/client.go
├── ate/
│   └── client.go                         # Calls Substrate SuspendActor(DurabilityMode) and ForkActor
└── benchmarking/
    ├── DESIGN.md                         # System design and specification
    ├── PLAN.md                           # This technical implementation plan
    ├── schema/                           # Versioned file-contract structs, strict JSON/JSONL codecs, embedded JSON Schemas
    │   ├── schema.go                     # Shared codec helpers (DecodeJSONStrict, ForEachJSONL, ValidateAgainstJSONSchema)
    │   ├── schedule.go                   # Schedule, ScheduledJob, ScheduledStep, ScheduledTask, PlacementPlan
    │   ├── event.go                      # Event (with sub-phases, source, tags/metrics maps), RunWindow
    │   ├── report.go                     # Report, ReportValidity, MetricSummary, StatsRow, SystemDescription
    │   ├── measurements.go               # Measurements, PrometheusSeries, NodeCacheArtifact, RestoreSample
    │   ├── scenario.go                   # Scenario, JobSpec, SUTSpec, FeatureOverlay, Thresholds
    │   ├── trace.go                      # TraceEvent (SDK trace.jsonl contract)
    │   ├── validate_schema.py            # Python helper validating emitted JSON/JSONL against v1/*.schema.json
    │   ├── v1/                           # Canonical JSON Schema files (*.schema.json), embedded via //go:embed
    │   └── testdata/                     # Golden fixtures for every artifact contract
    ├── swebench/                         # Core load generator, working-set manager, dataset adapters, reporter & validator
    │   ├── dataset.go                    # Dataset-agnostic Instance + Adapter/Grader registry + SWEBenchAdapter
    │   ├── dataset_command.go            # Generic CommandAdapter for non-SWE-bench JSONL RL tasks
    │   ├── trajectory.go                 # Trajectory template rendering, Step.Think, per-kind RequireGrading
    │   ├── schedule.go                   # Deterministic schedule resolver (explicit, seeded sampler, fixed)
    │   ├── workingset.go                 # Client-side parked working set (match by image, wake/reset/fork, park/retire, async prefetch)
    │   ├── backend.go                    # Backend/Sandbox interfaces + EnvBackend (ate-env-api) + DirectSubstrateBackend + AgentSandboxBackend
    │   ├── run.go                        # Modes: eval, steps (via WorkingSet + step_pause), open-loop (Poisson arrivals)
    │   ├── events.go                     # Buffered JSONL Recorder writing per-worker events/<worker>.jsonl shards + run_window.json
    │   ├── report.go                     # Streaming Builder, shard glob-merger, restore/cache joins, rollout & cost tables, stats.jsonl writer
    │   ├── validate.go                   # Validity Rules 1–7 evaluator over Report, Measurements, Schedule, and Scenario
    │   ├── compare.go                    # Report & custom_metrics comparator with threshold gating (exit code != 0 on regression)
    │   └── trajectories/                 # Embedded default trajectories & graders (swebench-gold.yaml, swebench-grade.py, workload-*.yaml)
    ├── trace/                            # Offline SDK trace-to-simulation converter
    │   ├── convert.go                    # Converts trace.jsonl -> Trajectory YAMLs (with Step.Think) + explicit schedule.json (with step_pause_ms)
    │   └── convert_test.go               # Round-trip chain test: trace.jsonl -> convert -> swebench.Run -> report
    ├── sut/                              # System Under Test provisioner, prewarmer, and server-truth reaper
    │   ├── provisioner.go                # Provisioner interface (Render, Provision, Prewarm, Reap, Teardown, DescribeSystem)
    │   ├── substrate.go                  # Substrate Helm/manifest renderer, ActorTemplate/golden snapshot prewarmer, server-queried reaper
    │   ├── agentsandbox.go               # Agent Sandbox manifest renderer, SandboxWarmPool prewarmer, CRD reaper
    │   └── reap.go                       # Timed polling loop verifying 0 remaining claims/sandboxes/templates/pools + events/reap.jsonl
    ├── measure/                          # Config-driven cluster & server telemetry harvester
    │   ├── collector.go                  # Orchestrates Prometheus queries, steady-state window detection, node-cache & restore harvesting, pod logs
    │   ├── prometheus.go                 # PromQL range/instant client preserving 0 vs null/error (never defaulting failed scrapes to 0)
    │   ├── logs.go                       # Unconditional controller/API/worker/atelet log harvester (including restarted --previous containers)
    │   └── queries.yaml                  # Default PromQL query catalog (readiness, router, PSI, OOM/evictions, packing, WSS, snapshots)
    ├── runner/                           # Job Runner implementations for bench orchestrator
    │   ├── runner.go                     # JobRunner interface (Render, Setup, Start, Wait, Collect, Teardown)
    │   ├── simulated.go                  # Renders/deploys in-cluster swebench-loader K8s Job/JobSet (replaces run-in-cluster.sh)
    │   ├── real.go                       # Attaches to or deploys live RL trainer jobs in measure or trace mode
    │   └── trainer_adapter.go            # Pluggable parsers converting :::MLLOG, TensorBoard events, or step JSONL -> trainer Events.jsonl
    └── overlay/                          # Declarative A/B Feature Overlay engine
        ├── overlay.go                    # Applies Helm values & JSON6902/Kustomize patches, runs lifecycle hooks, harvests custom_metrics
        └── overlay_test.go

clients/python/src/ate_env/
├── observability.py                      # Existing Observer & RunReport; extended with optional step-boundary events/*.jsonl flush
└── telemetry/
    ├── __init__.py
    ├── metrics.py                        # Existing ate_env_fleet_* & ate_env_client_* Prometheus metrics; extended with slot_wait/source attribution
    ├── tracing.py                        # Existing OTel span helpers
    └── trace_recorder.py                 # Lightweight OTel SpanProcessor / hook writing schema-compliant trace.jsonl when ENV_BENCH_MODE=trace
```

---

## 3. Component Interactions & Dependency Boundaries

### 3.1 Compile-Time Import Rules (Strict DAG)

1. **One-Way Dependency from Benchmark to SDK/Service**:
   - Benchmark packages (`cmd/benchmarking/*`, `internal/benchmarking/*`) may depend on `ateenv.v1alpha` protos and client libraries.
   - Core service and SDK packages (`internal/apiservice`, `internal/ate`, `clients/python/src/ate_env`) **never** import benchmark packages.
   - In Python, `ate_env` exposes general-purpose observability (`observability.py`, `telemetry/metrics.py`, `telemetry/tracing.py`, and the opt-in `telemetry/trace_recorder.py` span exporter). It has zero knowledge of SWE-bench, datasets, or `bench` scenarios.
2. **Leaf Schema Package (`internal/benchmarking/schema`)**:
   - `internal/benchmarking/schema` depends only on the Go standard library, `sigs.k8s.io/yaml`, and `github.com/google/jsonschema-go/jsonschema`.
   - Every stage package (`swebench`, `trace`, `sut`, `measure`, `runner`, `overlay`) imports `internal/benchmarking/schema` for wire types, **never** importing another stage package to pass in-memory state across stage boundaries.

```mermaid
flowchart TD
    subgraph CLI ["CLI Binaries (cmd/benchmarking/)"]
        BENCH["cmd/benchmarking/bench"]
        LOADER["cmd/benchmarking/swebench-loader"]
    end

    subgraph Stages ["Stage Packages (internal/benchmarking/)"]
        OVERLAY["overlay\n(A/B variant engine)"]
        RUNNER["runner\n(simulated & real K8s jobs +\ntrainer telemetry adapters)"]
        SUT["sut\n(provision, prewarm, reap)"]
        MEASURE["measure\n(Prometheus, node-cache,\nrestore logs, pod logs)"]
        SWEBENCH["swebench\n(schedule, workingset, backend,\nrun, report, validate, compare)"]
        TRACE["trace\n(trace.jsonl -> trajectory + schedule)"]
    end

    SCHEMA["internal/benchmarking/schema\n(versioned JSON/JSONL/YAML contracts + v1/*.schema.json)"]

    BENCH --> OVERLAY & RUNNER & SUT & MEASURE & SWEBENCH & SCHEMA
    LOADER --> SWEBENCH & TRACE & SCHEMA
    OVERLAY --> SCHEMA
    RUNNER --> SCHEMA
    SUT --> SCHEMA
    MEASURE --> SCHEMA
    SWEBENCH --> SCHEMA
    TRACE --> SCHEMA
```

### 3.2 Runtime Stage Interaction (File-Only Contracts in `$ARTIFACT_DIR`)

At runtime, stages never pass Go structs in memory across lifecycle boundaries; every stage reads its inputs from `$ARTIFACT_DIR` (or `--dest`) and writes its outputs back to `$ARTIFACT_DIR`:

```mermaid
sequenceDiagram
    autonumber
    participant Op as Operator / CI Harness
    participant Bench as bench CLI
    participant SUT as sut.Provisioner
    participant Loader as swebench-loader / Real Trainer
    participant Meas as measure.Collector
    participant Rep as swebench.Report & Compare

    Op->>Bench: bench run scenario.yaml [--overlay feat.yaml]
    Bench->>Loader: bench resolve -> writes $ARTIFACT_DIR/schedule.json
    Bench->>SUT: bench render -> writes $ARTIFACT_DIR/manifests/
    Bench->>SUT: Provision + bench prewarm -> writes system_description.json, cache_before.json, events/prewarm.jsonl
    par For each Job i at start_offset_ms
        Bench->>Loader: Start job i (reads schedule.json + dataset.jsonl)
        Loader-->>Bench: Writes events/<job>-<worker>.jsonl + run_window.json
    end
    Bench->>Meas: bench measure (reads run_window.json, waits --telemetry-lag)
    Meas-->>Bench: Writes cache_after.json, restore_timings.jsonl, measurements.json, logs/
    Bench->>SUT: bench reap (queries server API until 0 live objects)
    SUT-->>Bench: Writes events/reap.jsonl
    Bench->>Rep: bench report --report-only (merges events/*.jsonl + measurements.json)
    Rep-->>Op: Writes report.json + report.md + stats.jsonl (exits != 0 if invalid)
    opt Multiple variants (baseline vs overlay)
        Bench->>Rep: swebench-loader compare baseline/report.json feat/report.json
        Rep-->>Op: Writes comparison.md (exits != 0 on threshold regression)
    end
```

---

## 4. Technical Design of Key Subsystems

### 4.1 Park Policies & Backend Driver Plumbing (`internal/ate`, `internal/apiservice`, `swebench/backend.go`)
- **Proto & API Service**:
  - Add `DurabilityMode` (`DURABILITY_MODE_UNSPECIFIED`, `DURABILITY_MODE_SYNCHRONOUS`, `DURABILITY_MODE_ASYNCHRONOUS`) to `SuspendEnvironmentRequest` and add `ForkEnvironment(ForkEnvironmentRequest) returns (ForkEnvironmentResponse)` in `ateenv.v1alpha`.
  - Update `internal/ate/client.go` so `SuspendEnvironment` forwards the requested durability mode to Substrate's `SuspendActor` instead of hardcoding `SYNCHRONOUS`, and implement `ForkEnvironment`.
- **Loader `Backend` Interface (`internal/benchmarking/swebench/backend.go`)**:
  - Extend `Sandbox` / `Backend` with `Suspend(ctx, mode ParkPolicy) error`, `Fork(ctx, newID string) (Sandbox, error)`, and `Prewarm(ctx, images []string) error`.
  - Provide drivers for `ate-env-api` (`EnvBackend`), direct Substrate control plane (`DirectSubstrateBackend`), and Kubernetes Agent Sandbox CRDs (`AgentSandboxBackend`).

### 4.2 Schedule Resolver & Steps-Mode Working Set (`swebench/schedule.go`, `swebench/workingset.go`)
- **Schedule Resolution (`schedule.go`)**:
  - Accepts `explicit` per-step task lists, `sampled` (deterministic `math/rand/v2` seeded PRNG sampling `tasks_per_step` instances $\times$ `group_size` rollouts per step from the dataset manifest and trajectory pool), or `fixed` (repeating the same slice every step).
  - Always writes `$ARTIFACT_DIR/schedule.json` before execution so repeated runs or baseline-vs-overlay variants replay the exact same per-step image sequence.
- **Client-Side Working Set (`workingset.go`)**:
  - Maintains `parkedByImage map[string][]*parkedSandbox` across steps.
  - At step $s$:
    1. **Match**: Partitions step $s$'s scheduled tasks into **hits** (popping a parked sandbox with matching `Image`) and **misses**.
    2. **Async Prefetch & Retire (Off Critical Path)**: If `-prefetch` is enabled and step $s+1$ exists in `schedule.json`, spawns a background goroutine at step start that computes the image delta `need(s+1) \ active(s)` and calls `Backend.Prewarm`, recording `prefetch_lead_margin_ms = step_s_end_ts - prewarm_ready_ts`. Surplus sandboxes not needed in $s$ or $s+1$ are retired asynchronously, tracking `release_overlap_ms`.
    3. **Execute Rollouts**: Wakes + resets (or forks when `park == "fork"`) hit sandboxes and creates miss sandboxes up to `-max-concurrent`, recording `slot_wait_ms` (time waiting on the concurrency semaphore) separately from wire `create`/`wake` and `ttfi`.
    4. **Step Barrier & Park**: Waits for all rollouts in step $s$ to complete, parks active sandboxes according to `ParkPolicy` (`suspend-async`, `suspend-sync`, `fork`, `delete`, `none`), sleeps `step_pause_ms` to simulate trainer gradient + weight sync, and emits a coarse `step` event with working-set hit rate, standing pool size, and churn counts.

### 4.3 SDK Telemetry & Trace-to-Trajectory Converter (`clients/python`, `internal/benchmarking/trace`)
- **Reusing Existing Python SDK Observability (`measure` mode)**:
  - `clients/python/src/ate_env/observability.py` and `telemetry/metrics.py` already provide `Observer`, `RunReport`, `ate_env_fleet_*`, and `ate_env_client_*` Prometheus collectors, and `telemetry/tracing.py` already instruments every SDK call with OpenTelemetry spans.
  - Rather than adding a parallel metrics stack, we extend `observability.py` / `metrics.py` with:
    1. Sub-phase attribution on acquire (`slot_wait_seconds` vs. `wire_claim_seconds` vs. `ready_to_first_command_seconds`) and acquisition `source` label (`warm_pool`, `cold_create`, `golden_restore`, `fork`, `resume`).
    2. Optional step-boundary JSONL flushing when `ENV_BENCH_EVENTS_PATH` is set.
- **SDK Call Trace Recorder (`trace` mode)**:
  - `clients/python/src/ate_env/telemetry/trace_recorder.py` attaches an OpenTelemetry `SpanProcessor` to `tracing.py`'s existing spans when `ENV_BENCH_MODE=trace`, serializing each completed `ate_env_client.*` and `ate_env_fleet.*` span as a `TraceEvent` line in `$ARTIFACT_DIR/trace.jsonl`.
- **Trace Converter (`internal/benchmarking/trace/convert.go`)**:
  - Streams `trace.jsonl`, groups calls by `(job, step, env_id)`, computes `Step.Think` as the timestamp gap between consecutive sandbox operations on the same environment (capturing LLM/sampler think time), computes `step_pause_ms` as the gap between step rollout barriers, writes one replayable `Trajectory` YAML per distinct call pattern, and emits `$ARTIFACT_DIR/schedule.json`.

### 4.4 Streaming Reporter, Validator & Comparator (`swebench/report.go`, `validate.go`, `compare.go`)
- **Streaming Aggregation**:
  - `SummarizeDir(eventsGlob)` streams all `$ARTIFACT_DIR/events/*.jsonl` worker shards + `prewarm.jsonl` + `reap.jsonl` through `Builder.Add(ev)` without loading full event slices into memory, retaining raw duration slices per `(job, op, source)` to compute exact `p50`/`p90`/`p95`/`p99`/`max` percentiles.
- **Rollout Decomposition & Cost Joins**:
  - Joins `Measurements` (`cache_before.json`, `cache_after.json`, `restore_timings.jsonl`, and `measurements.json` Prometheus series) with raw events to populate TTFI sub-phase tables, per-step `p50`/`p100` straggler skew and `env_overhead_pct`, active vs. parked sandbox density, node-hours per completed task, and cross-job interference buckets (grouping operation latencies by the number of concurrently active jobs `[1, 2, 3+]`).
- **Dual Output (`report.json` + `stats.jsonl`)**:
  - Emits structured `report.json`, human-readable `report.md`, and flat `StatsRow` lines in `stats.jsonl` (`trial_summary`, `server_summary`, `phase_breakdown_summary`, and `custom_metrics`).

---

## 5. High-Level Vertical PR Breakdown

To keep reviews small, self-contained, and easy to verify, **every PR delivers one vertical end-to-end capability** (CLI flag/subcommand $\to$ core execution $\to$ report output + **only** the `internal/benchmarking/schema` types that PR reads or writes). No PR introduces unused schema files or interfaces without a caller.

### PR Sequence & Dependency Graph

```mermaid
flowchart LR
    MAIN["main"]
    PR1["PR 1\nPluggable Dataset/Grader &\nStreaming Event Recorder/Report"]
    PR2["PR 2\nDynamic Step Schedules,\nWorking-Set Reuse & Prefetch"]
    PR3["PR 3\nMulti-Worker Shard Merge,\nValidity Gates, Threshold Compare\n& stats.jsonl"]
    PR4["PR 4\nPython SDK trace.jsonl Recorder\n+ Go Trace-to-Trajectory Replay"]
    PR5["PR 5\nate-env-api Async Suspend & Fork\n+ Direct/AgentSandbox Drivers"]
    PR6["PR 6\nSUT Provisioner, Reaper &\nPrometheus Measurements CLI"]
    PR7["PR 7\nMulti-Job Scenario Runner (bench)\n& Trainer Telemetry Adapters"]
    PR8["PR 8\nDeclarative A/B Feature Overlays\n(bench run --overlay)"]

    MAIN --> PR1 --> PR2 --> PR3
    MAIN --> PR4
    PR2 --> PR4
    PR1 --> PR5
    PR3 & PR5 --> PR6 --> PR7 --> PR8
    PR4 --> PR7
```

### Detailed PR Plan

| PR | Title / Vertical Slice | End-to-End Capability Added | Files & Packages Touched | Schema Slice Included | End-to-End Verification Gate |
|---|---|---|---|---|---|
| **PR 1** | **Pluggable Dataset/Grader Registry & Streaming Event/Report Engine** | Decouples `swebench` execution/reporting from SWE-bench-specific JSON fields via per-kind `Adapter`/`Grader` registry; adds generic `command` dataset adapter alongside `swebench`; makes `Recorder` buffered and `Summarize` streaming. | `internal/benchmarking/swebench/{dataset,dataset_command,trajectory,events,report,run,backend}.go` | Existing `Event`, `Summary`, `Instance` (in-place). | Run `swebench-loader` on both a SWE-bench manifest and a non-SWE-bench command manifest; verify grading and streaming `Summarize`. |
| **PR 2** | **Dynamic Per-Step Schedules, Parked Working Set & Async Prefetch** | Adds `swebench-loader resolve` and `-schedule` / `-tasks-per-step` / `-group-size` / `-step-pause` / `-prefetch` / `-park`. Replaces static `steps` fleet with image-matched parked `WorkingSet` and off-critical-path next-step prefetch; reports working-set hit rate, churn, and prefetch lead margin. | `cmd/benchmarking/swebench-loader/main.go`<br>`internal/benchmarking/schema/{schema,schedule}.go`<br>`internal/benchmarking/swebench/{schedule,workingset,run,report}.go` | `schedule.schema.json` (`Schedule`, `ScheduledJob`, `ScheduledStep`, `ScheduledTask`, `ParkPolicy`) + working-set `Event`/`Summary` fields. | Resolve a seeded multi-step schedule with changing images per step $\to$ run `swebench-loader -mode steps -schedule schedule.json -prefetch` against mock/fake backend $\to$ verify positive prefetch lead margin, working-set reuse hit rate, and `go test -bench` throughput. |
| **PR 3** | **Multi-Worker Shard Merging, Validity Gates, Regression Compare & `stats.jsonl`** | Adds glob-merging of `events/*.jsonl` shards, `run_window.json` emission, Validity Rules 1–7 (`-validate`), threshold-gated `swebench-loader -compare -thresholds T.yaml` (non-zero exit on regression), per-step rollout `p50`/`p100` straggler decomposition, and `-stats-out stats.jsonl`. | `cmd/benchmarking/swebench-loader/main.go`<br>`internal/benchmarking/schema/{event,report}.go`<br>`internal/benchmarking/swebench/{events,report,validate,compare}.go` | `event.schema.json`, `run_window.schema.json`, `report.schema.json`, `stats_row.schema.json`. | Feed multi-worker `events/*.jsonl` + `cache_before/after.json` + `restore_timings.jsonl` into `swebench-loader -report-only -validate -stats-out stats.jsonl` $\to$ verify validity pass/fail reasons, `stats.jsonl` schema compliance, and non-zero exit when `compare` detects a planted regression. |
| **PR 4** | **Python SDK `trace.jsonl` Recorder & Trace-to-Trajectory Replay** | Extends `ate_env` (`observability.py` / `tracing.py`) with `ENV_BENCH_MODE=trace` span recorder writing `trace.jsonl` (zero duplicated Prometheus logic) + `swebench-loader convert-trace` converting `trace.jsonl` into `Trajectory` YAMLs (`Step.Think`) and `schedule.json` (`step_pause_ms`). | `clients/python/src/ate_env/{observability.py,telemetry/metrics.py,telemetry/trace_recorder.py}`<br>`internal/benchmarking/schema/{trace.go,validate_schema.py}`<br>`internal/benchmarking/trace/convert.go`<br>`cmd/benchmarking/swebench-loader/main.go` | `trace_event.schema.json` (`TraceEvent`). | End-to-end chain test: Python `ate_env` session in `trace` mode emits `trace.jsonl` (validated against `trace_event.schema.json`) $\to$ `swebench-loader convert-trace` emits `Trajectory` + `schedule.json` $\to$ `swebench-loader` replays it and reproduces step count, concurrency curve, and think times. |
| **PR 5** | **Async/Sync Suspend & Fork in `ate-env-api` + Backend Drivers** | Plumbs `SuspendEnvironmentRequest.DurabilityMode` (`ASYNCHRONOUS` vs `SYNCHRONOUS`) and `ForkEnvironment` through `ateenv.v1alpha`, `cmd/ate-env-api`, and `EnvBackend`, and adds direct Substrate / Agent Sandbox `Backend` implementations so `swebench-loader -park suspend-async|suspend-sync|fork` works against live clusters. | `api/ateenv/v1alpha/env.proto`<br>`internal/ate/client.go`<br>`internal/apiservice/server.go`<br>`internal/benchmarking/swebench/backend*.go` | None (uses `ParkPolicy` from PR 2). | Unit and fake-control-plane tests exercising `suspend-async`, `suspend-sync`, and `fork` end-to-end from `swebench-loader` through `apiservice` to `fakecontrol`. |
| **PR 6** | **Composable SUT Provisioner, Reaper & Measurements CLI (`bench`)** | Introduces `cmd/benchmarking/bench` with standalone `render`, `prewarm`, `measure`, and `reap` subcommands: renders SUT manifests, prewarms templates/pools (`events/prewarm.jsonl`), scrapes Prometheus + node-cache + restore timings + pod logs (`measurements.json` with `0` vs `null` enforcement), and runs timed server-queried `reap` to zero orphans (`events/reap.jsonl`). | `cmd/benchmarking/bench/main.go`<br>`internal/benchmarking/schema/measurements.go`<br>`internal/benchmarking/sut/*.go`<br>`internal/benchmarking/measure/*.go` | `measurements.schema.json`, `node_cache.schema.json`, `restore_sample.schema.json`, `system_description.schema.json`. | Hermetic test against fake Prometheus + fake K8s/Substrate API verifying `bench prewarm`, `bench measure` (including failed-scrape rejection), and `bench reap` converging to 0 server-side orphans and attaching to `bench report`. |
| **PR 7** | **Multi-Job Scenario Runner (`bench run`) & Live Trainer Telemetry Adapters** | Adds `bench run scenario.yaml` to orchestrate 1..N staggered simulated or real jobs (`internal/benchmarking/runner`), cross-job interference tables in `report.go`, open-loop Poisson arrivals (`-mode open-loop`), and trainer step-log adapters (`:::MLLOG`, TensorBoard, structured step JSONL). | `cmd/benchmarking/bench/main.go`<br>`internal/benchmarking/schema/scenario.go`<br>`internal/benchmarking/runner/*.go`<br>`internal/benchmarking/swebench/{run,report}.go` | `scenario.schema.json` (`Scenario`, `JobSpec`, `SUTSpec`). | Run `bench run` with two offset simulated jobs + a `:::MLLOG` real-trainer fixture $\to$ verify rendered K8s `Job` manifests, cross-job interference latency buckets, trainer step breakdown (`rollout`/`weight_sync`/`train`, `env_overhead_pct`), and zero-orphan reap. |
| **PR 8** | **Declarative A/B Feature Overlays (`bench run --overlay`)** | Adds `--overlay feature.yaml` and `variants:` support (`internal/benchmarking/overlay`): resolves `schedule.json` once, applies Helm/Kustomize/JSON6902 `sut_overrides`, runs `pre_provision`/`post_provision`/`pre_reap`/`post_teardown` hooks, harvests overlay `diagnostics` into `custom_metrics`, and runs `compare` across variants. | `internal/benchmarking/overlay/*.go`<br>`cmd/benchmarking/bench/main.go`<br>`internal/benchmarking/sut/provisioner.go`<br>`internal/benchmarking/measure/collector.go` | `FeatureOverlay` fields in `scenario.schema.json`. | End-to-end A/B test over base scenario + feature overlay fixture verifying shared `schedule.json`, manifest patching, custom `reap_resources` reaching zero, `custom_metrics` in `report.json`/`stats.jsonl`, and side-by-side `compare` output. |
