# PRD: telemetry for sandboxenv-rl

Status: delivered. Drafted 2026-10-21; revised 2026-10-29 to `main` at `3b03b03`. Companion
documents: [README.md](./README.md) (design and mapping), [execution-plan.md](./execution-plan.md),
[acceptance-criteria.md](./acceptance-criteria.md), [test-plan.md](./test-plan.md),
[interfaces.md](./interfaces.md) and [decisions.md](./decisions.md).

## Problem

Before this work, an RL trainer on sandboxenv-rl could not see where an episode's time went. The
Python client, the fleet, `ate-env-api` and the Go client emitted no traces and no metrics. The
three benchmark drivers each wrote their own JSONL with their own field names and clocks, so a slow
step could not be attributed to the client, the API server's wake wait, the Substrate control plane
or the guest. Substrate itself exports `ate_actor_*` metrics and continues OpenTelemetry traces
through `ateapi`, so the gap was entirely on the environment side. agent-sandbox solved the same
problem with a client trace manager, a controller instrumenter, a run report and bounded Prometheus
series; the tunix and NeMo Gym teams read those reports and expected the same from the Substrate
path.

## Users

- RL platform engineers (tunix, NeMo Gym, the GRPO and VeRL examples) who need per-phase timings per
  run and a trace per episode when a step is slow.
- Operators of `ate-env-api` who need request rates, error codes, wake-wait and create-to-ready
  latency on a `/metrics` endpoint next to Substrate's.
- Benchmark authors (`cmd/benchmarking/bench`, the swebench loader) who need one telemetry source
  instead of a second copy of it.

## Scope, as delivered

1. Python tracing (#93, #164): `ate_env.telemetry.tracing`; `ate_env_client.*` spans on every
   client, runtime, handle and driver operation; the W3C context in gRPC metadata; context carried
   across `LoopRunner` and `process_parallel`; otelgrpc's `rpc.*` keys on `Client` spans.
2. Python observability (#109, #113, #115): `RunReport`, `Observer`, Prometheus `ate_env_fleet_*`
   and `ate_env_client_*` series behind the `metrics` extra, `serve_metrics`, hooks in the fleet,
   the async fleet, the strategies and the Substrate and mock drivers; `ate_env_fleet.<phase>`
   spans.
3. `ate-env-api` (#114): `-enable-tracing`, `-metrics-listen-addr`, otelgrpc on the server and on
   the `ateapi` and guest connections, the `ate_env_api_*` families including wake-wait and
   create-to-ready, the `x-ate-env-requested-at` header, `ate-env manifest --enable-tracing`,
   `--otlp-endpoint`, `--otlp-insecure` and `--metrics-port`.
4. Go client and guest (#150): `ClientOptions.TracerProvider` and `TraceServiceName`, `Attr*`
   constants, spans; guest `-enable-tracing` and `-enable-metrics` with
   `ate_env_guest_process_duration_seconds`.
5. Benchmarks and docs (#168): one span per swebench loader event under `swebench-loader.run`, the
   GRPO example and the NeMo Gym rollout benchmark on the `Observer`, `REPORT_DIR` emitters,
   `benchmarking/telemetry_report/build_report.py`, [docs/observability.md](../../observability.md),
   generated [docs/metrics.md](../../metrics.md) with a CI freshness check, `CHANGELOG.md` and the
   Python client README.
6. In review: telemetry in the VeRL SWE-bench example (#186): one run report per RayJob, one trace
   per iteration, an end-to-end script against the acceptance collector.

### Accepted limitations

The trace boundary is the `ateapi` RPC span unless Substrate exports to the same backend; the guest
is instrumented (the size gate held); no dashboards, alerts or production collector ship with the
repository, only the acceptance fixture `tests/telemetry/otel-collector.yaml` and the collector
guidance in the observability guide; metrics are Prometheus pull only; the JSONL formats of the
swebench loader and the GRPO example are unchanged.

## Non-goals

- Changes to Substrate (`agent-substrate/substrate`) or to agent-sandbox.
- OTLP metrics export, logs correlation, sampling policy beyond the SDK defaults.
- Dashboards, alerting, SLOs.
- Replacing the swebench loader's JSONL or the GRPO run files.
- Any new transport, auth or API change in `ateenv.v1alpha`. The one wire addition is the optional
  `x-ate-env-requested-at` metadata header.

The JSONL trace recorder (`ate_env.telemetry.trace_recorder`, `ENV_BENCH_MODE`) came from the
benchmarking series (#173). It consumes the client spans this work added and is specified in
`internal/benchmarking/PLAN.md`.

## Success metrics

A trainer that installs `ate-env-client[tracing,metrics]` and sets `OTEL_EXPORTER_OTLP_ENDPOINT`
sees one trace from `ate_env_fleet.run` down to `ate-env-api` (and to `ateapi` when Substrate
exports), a `/metrics` endpoint on its process and on `ate-env-api`, and a run report in the same
shape as agent-sandbox-rl's. A trainer that installs the base package sees nothing new and pays
nothing.

Evidence: the unit and integration suites in CI (`clients/python/tests/test_telemetry_tracing.py`,
`test_observability.py`, `test_telemetry_metrics.py`, `internal/telemetry`,
`internal/apiservice/telemetry_test.go`, `clients/go/tracing_test.go`,
`internal/benchmarking/swebench/events_test.go`), the Tests section of each pull request, and #186's
end-to-end run (622 spans in 6 traces, 0 orphan roots, every `verl.iteration` trace reaching
`ate-env-api`).

Not recorded: the overhead of telemetry off against on for a mock-backend run. The procedure stays
in [test-plan.md](./test-plan.md) section 7.

## Risks

See the execution plan's risk table and its outcomes. The one that changed scope, the `ateapi` trace
join, is documented as conditional on Substrate's exporter in the observability guide.
