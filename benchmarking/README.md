# Benchmarking

The benchmark inputs, scripts and report tools. The binaries live in
[`cmd/benchmarking`](../cmd/benchmarking) and the packages in
[`internal/benchmarking`](../internal/benchmarking); together they are maintained
tooling with no compatibility promise ([docs/support.md](../docs/support.md)): CI
runs their tests, flags can change at any time, and a change to the events file or
the report JSON is called out in the PR description.

| Path | What it is |
|---|---|
| [`swebench/`](swebench/README.md) | The Fast rung of the RL benchmark ladder: `swebench-loader` drives SWE-bench Verified task images through `ate-env-api` the way a simulated RL job does (`eval` and `steps` modes), and reports what each phase cost. |
| [`scenarios/`](scenarios) | Scenario files for the `bench` harness (`schema_version: v1`): `multi-job-swebench.yaml` (a closed-loop step-barrier job and an open-loop rollout stream on one cluster) and `qwen3-1.7b-verl.yaml` (the real Qwen3-1.7B verl recipe of [`examples/Qwen3-1.7B-verl`](../examples/Qwen3-1.7B-verl/README.md) as a benchmark). |
| [`telemetry_report/`](telemetry_report) | `build_report.py` (standard library only) builds a self-contained `report.html` and `run.json` from run reports, OTLP/JSON spans and Prometheus scrapes: one page for one run ([docs/observability.md](../docs/observability.md)). |
| [`SKILL.md`](SKILL.md) | The agent skill for the `bench` harness: hook a recipe up, run a scale ladder or a real RL benchmark, read the bundles. |
| [`swebench/SKILL.md`](swebench/SKILL.md) | The agent skill for simulated runs with `swebench-loader`, with the cluster checks that silently change what a run measures. |

## `swebench/`

| File | What it does |
|---|---|
| `make-dataset.py` | Builds the JSONL manifest `swebench-loader` reads from a SWE-bench dataset; `test_make_dataset.py` and `test_make_dataset_images.py` test it. |
| `mirror-images.sh` | Copies SWE-bench task images into a registry the cluster's nodes can pull from, digest-pinned. |
| `run-in-cluster.sh` | Runs `swebench-loader` as a Job inside the cluster, next to `ate-env-api`, follows it, and brings the events file and the report back. |
| `harvest-restore-timings.sh` | Collects atelet's per-actor restore timing records for one run, for `swebench-loader report -restore-log`. |
| `sample-node-cache.sh` | Samples the image-cache counters of every atelet, to tell a warm node from a cold one. |
| `offline-exclusions.txt` | SWE-bench Verified instances whose evaluation needs network egress, excluded from offline runs. |

## The binaries and packages

| Path | What it is |
|---|---|
| [`cmd/benchmarking/swebench-loader`](../cmd/benchmarking/swebench-loader) | `run`, `resolve`, `convert-trace`, `report`, `compare` and `trajectory-assets`: the loader, its JSONL event recorder (`-enable-tracing` also exports each event as a span) and the report. |
| [`cmd/benchmarking/bench`](../cmd/benchmarking/bench) | The composable harness: `resolve`, `render`, `prewarm`, `measure`, `reap`, `report` over a scenario file. |
| [`internal/benchmarking`](../internal/benchmarking) | The packages behind both: `schema` (the v1 data contracts), `swebench` (trajectories, events, the golden file), `job` (the multi-job scenario runner), `sut` (system-under-test manifests and provisioning), `measure` (Prometheus and node-cache harvesting), `overlay` (feature overlays and A/B comparison), `trace` (SDK call traces to replayable form). |
| [`internal/benchmarking/DESIGN.md`](../internal/benchmarking/DESIGN.md) | The system design and work spec; [`PLAN.md`](../internal/benchmarking/PLAN.md) the implementation plan; [`TELEMETRY_GUIDE.md`](../internal/benchmarking/TELEMETRY_GUIDE.md) the telemetry scopes and feature plugins. |
| [`tests/telemetry/otel-collector.yaml`](../tests/telemetry/otel-collector.yaml) | The acceptance collector: OTLP in, spans to a file, a scrape of `ate-env-api`'s metrics. |

Related: the NeMo Gym provider's [rollout benchmark and driver
simulator](../integrations/nemo-gym/benchmarks), and the run report every fleet
run prints ([`clients/python`](../clients/python/README.md)).
