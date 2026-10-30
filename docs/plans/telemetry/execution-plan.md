# Execution plan: telemetry and the OpenTelemetry adapter for sandboxenv-rl

Status: complete for pull requests 1 to 5; pull request 6 (#186) in review. Written 2026-10-21;
revised 2026-10-29 to `main` at `3b03b03`.

Goal: `ai-on-gke/sandboxenv-rl` gains the three telemetry layers agent-sandbox has (always-on run
report, Prometheus metrics, OpenTelemetry traces) across the Python client, the fleet,
`ate-env-api`, the Go client, the guest and the benchmarks, with the same design properties:
optional dependencies behind complete no-op fallbacks, one trace from the trainer to the Substrate
control plane, bounded metric labels, and docs generated from the definitions. Design and mapping:
[README.md](./README.md).

Stop condition: every acceptance criterion in [acceptance-criteria.md](./acceptance-criteria.md)
holds on `main`, the reviewer has no open blocking finding, and the pull requests are merged with
green CI. Met on 2026-10-29 with #168.

## Method

As planned: one orchestrator drives five roles (product manager, tech lead, developer, reviewer,
tester), each in its own git worktree, with code landing as small pull requests on feature branches
and the plan documents kept apart from the code.

As run: the series landed as six pull requests from one developer branch per PR (`telemetry-pr1` to
`telemetry-pr6`, plus `telemetry-pr2-ac-fixes` and `python-rpc-semconv`), each reviewed by the
repository's maintainers before merge. The acceptance fixes in #113 were driven by the criteria in
this directory. No `tests/RESULTS.md` or `review/findings.md` were produced; the evidence is the
Tests section of each pull request and CI. [decisions.md](./decisions.md) records the deviations.

## Target repository and environment

| Item                     | Value                                                                                                                                                                                                                                                                                                                                                         |
| ------------------------ | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| Repository               | `https://github.com/ai-on-gke/sandboxenv-rl`, default branch `main`. Plan written against `8ff5c3e` (2026-10-21); this revision describes `3b03b03` (2026-10-29).                                                                                                                                                                                             |
| Branches                 | `telemetry-pr1` (#93), `telemetry-pr2` (#109), `telemetry-pr2-ac-fixes` (#113), `telemetry-pr3` (#114), `telemetry-pr4` (#150), `python-rpc-semconv` (#164), `telemetry-pr5` (#168), `telemetry-pr6` (#186).                                                                                                                                                  |
| Python                   | 3.12 in CI; floor 3.10 in `clients/python/pyproject.toml`. Venvs: `pip install -e 'clients/python[dev]'` and `pip install -e 'clients/python[dev,tracing,metrics]'`.                                                                                                                                                                                          |
| Go                       | the module's `go.mod` toolchain; `go test ./...` from the repository root.                                                                                                                                                                                                                                                                                    |
| Lint and checks          | `make verify-boilerplate`, `make verify-gofmt`, `make verify-docs`, `make vet`, `ruff check clients/python`; the full list is in [AGENTS.md](../../../AGENTS.md).                                                                                                                                                                                             |
| Cluster for acceptance   | A GKE cluster with Substrate in `ate-system` and ate-env in `ate-env`, deployed by `ate-env manifest --enable-tracing --otlp-endpoint ... --otlp-insecure --metrics-port 9090`. Re-check with `kubectl -n ate-system get deploy` and `kubectl -n ate-env get deploy,svc` before a run.                                                                        |
| OTLP sink for acceptance | `tests/telemetry/otel-collector.yaml` in namespace `ate-env-telemetry`: OTLP on 4317 and 4318, a `prometheus` receiver scraping `ate-env-api`'s `/metrics`, spans and metrics as OTLP/JSON lines under `/data`, the `debug` exporter, and an optional forward to another OTLP endpoint.                                                                       |
| Upstream facts           | Substrate's `ateapi` serves with `otelgrpc.NewServerHandler()` and dials with `NewClientHandler()`; `internal/serverboot` picks exporters from the environment; Prometheus families `ate_actor_*`, `ate_scheduler_*`, `ate_workerpool_*` exist. Substrate moved from `ddb46651` to v0.6.0 (#110) and to `475f49b0` with `ateapi.v1` (#163) during the series. |

## Pinned sources

- agent-sandbox at `7d956bce`:
  `clients/python/agentic-sandbox-client/k8s_agent_sandbox/trace_manager.py`, `metrics.py`,
  `models.SandboxTracerConfig`; `clients/go/sandbox/tracing.go`; `internal/metrics/tracing.go` and
  `metrics.go`; `cmd/metrics-docs-gen`;
  `examples/agent-sandbox-rl/agent_sandbox_rl/observability.py` and `config.ObservabilityConfig`;
  `GCP.md`; the RL README Observability section and `performance_reports/README.md`.
- sandboxenv-rl at `8ff5c3e`:
  `clients/python/src/ate_env/{client,fleet,async_fleet,strategies,handle,loop_runner,config}.py`,
  `backend/{base,substrate}.py`, `runtime/substrate_env_client.py`; `cmd/ate-env-api/main.go`;
  `internal/apiservice/{server,wakeretry}.go`; `internal/ate/client.go`; `clients/go/client.go`;
  `internal/benchmarking/swebench/events.go`; `examples/grpo/{grpo_substrate_train,report}.py`;
  `integrations/nemo-gym/benchmarks/rollout_bench.py`; `.github/workflows/ci.yaml`.
- Substrate at `ddb46651`: `cmd/ateapi/main.go` (otelgrpc), `internal/serverboot/exporters.go`.

## Accepted limitations

- Substrate's resume and suspend workflows join the client trace when Substrate exports to the same
  backend: the router resumes under the caller's `traceparent`, and `ateapi`, atelet and ateworker
  continue it. The plan treated this as an open question; reading Substrate `28520c0d` on 2026-10-30
  closed it, see `decisions.md`.
- Guest instrumentation was gated on the `ate-env-guest` image growing by less than 5 MB. It grew by
  2.6 MiB (13.82 MB to 16.56 MB stripped) and shipped.
- No dashboards, alert rules or production collector; `tests/telemetry/otel-collector.yaml` is a
  test fixture. The observability guide points at GKE managed OpenTelemetry and the Google-built
  collector for production.
- Metrics are Prometheus pull only; no OTLP metrics export.
- The swebench loader and the GRPO example keep writing their JSONL. They gained spans and the
  report; their file formats did not change.

## Deliverables (target repository paths, as merged)

```
clients/python/src/ate_env/telemetry/__init__.py     public names re-exported
clients/python/src/ate_env/telemetry/tracing.py      start_span, TracerManager, interceptors, rpc_attrs
clients/python/src/ate_env/telemetry/metrics.py      ate_env_client_* histograms (lazy)
clients/python/src/ate_env/telemetry/trace_recorder.py   JSONL trace events on the client spans (#173, benchmark series)
clients/python/src/ate_env/observability.py          RunReport, Observer, repo_family, serve_metrics
clients/python/src/ate_env/config.py                 TracerConfig, ObservabilityConfig, FleetConfig.tracing / .observability
clients/python/src/ate_env/{client,fleet,async_fleet,strategies,loop_runner,handle}.py   hooks
clients/python/src/ate_env/backend/{substrate,mock}.py, runtime/substrate_env_client.py           hooks
clients/python/tests/test_telemetry_tracing.py, test_observability.py, test_telemetry_metrics.py
clients/python/pyproject.toml                        extras tracing, metrics
clients/python/README.md                             Observability section
internal/telemetry/{doc.go,otel.go,metrics.go,metrics_test.go,otel_test.go}
cmd/ate-env-api/main.go, cmd/ate-env-api/metrics_test.go     -enable-tracing, -metrics-listen-addr, -metrics-environments-ttl
internal/ate/client.go, internal/apiservice/server.go, wakeretry.go, telemetry_test.go, telemetry_internal_test.go
cmd/ate-env/manifest.go                              --enable-tracing, --otlp-endpoint, --otlp-insecure, --metrics-port
clients/go/{client.go,tracing.go,tracing_test.go}    TracerProvider option, Attr*, spans, Client.Get
cmd/ate-env-guest/main.go, guest/server.go           -enable-tracing, -enable-metrics
internal/benchmarking/swebench/events.go, events_test.go, testdata/events.golden.txt
cmd/benchmarking/swebench-loader                     run -enable-tracing
examples/grpo/{grpo_substrate_train,report}.py, integrations/nemo-gym/benchmarks/rollout_bench.py
benchmarking/telemetry_report/{build_report.py,template.html,test_build_report.py}
hack/metrics-docs/{main.go,main_test.go}, docs/metrics.md, docs/observability.md, CHANGELOG.md
tests/telemetry/otel-collector.yaml                  acceptance collector fixture
.github/workflows/ci.yaml                            second Python run with the extras, metrics-docs diff step
```

## Roles and handoffs, as planned

| Phase | Role            | Reads                                             | Produces                                                                                                             | Done when                                                                                   |
| ----- | --------------- | ------------------------------------------------- | -------------------------------------------------------------------------------------------------------------------- | ------------------------------------------------------------------------------------------- |
| 1     | Product manager | the design plan, the target repository, this file | `prd.md`, `acceptance-criteria.md` (AC-1 to AC-14), `test-plan.md`                                                   | every criterion is measurable, has a command and an evidence form                           |
| 2     | Tech lead       | the PM documents, both code bases                 | `interfaces.md`: module paths, public names, span names and attributes, metric families and labels, flags, variables | the developer can implement PR 1 without a naming question; the tester can assert the names |
| 3     | Developer       | `interfaces.md`, the criteria, agent-sandbox      | PRs 1 to 5 on their branches, each with tests and docs, each green in CI                                             | local suites pass with and without the extras; lint and boilerplate pass                    |
| 4a    | Tester          | `test-plan.md`, the PR branch                     | per-criterion verdicts with evidence                                                                                 | every criterion the PR claims has a verdict                                                 |
| 4b    | Reviewer        | the PR diff, `interfaces.md`                      | findings ranked by severity with file and line                                                                       | blocking findings are explicit, each with a failure scenario and a fix                      |
| 5     | Developer       | the findings and failed criteria                  | fixes on the same branch; replies on the PR threads                                                                  | tester re-run passes; reviewer clears the blockers                                          |
| 6     | Tech lead       | everything                                        | a closing note in `decisions.md`                                                                                     | stop condition met                                                                          |
| 7     | Orchestrator    |                                                   | marks the PRs ready, pings the maintainers, merges when approved                                                     |                                                                                             |

The lead is the arbiter between developer and reviewer. A disputed finding is settled in
`decisions.md` rather than in a PR thread.

## Rules for all roles

- Work only in your own worktree. The developer owns the feature branches; the reviewer and tester
  never push to them.
- Cluster blast radius: the `ate-env` and `ate-env-telemetry` namespaces of the acceptance cluster
  only. Never change node pools, cluster settings, `ate-system`, IAM or buckets.
- Commits: no co-author or generated-by trailers ([AGENTS.md](../../../AGENTS.md)), Apache header on
  every new file (`make verify-boilerplate`), one concern per commit, message leads with the
  motivation.
- Telemetry off must be a no-op: no new import at module load outside the base dependencies, no
  global registry side effects at import, no behavior change in any existing test.
- Never put an environment id, actor id, run id, command line, file path or image tag into a metric
  label or a span name. Attributes carry ids; labels carry the bounded vocabulary in
  [interfaces.md](./interfaces.md).
- Every deviation from `interfaces.md` or this plan is an entry in `decisions.md` (date, who, what,
  why), appended, never edited.

## PR sequence, as merged

| #   | Branch                                    | Pull request     | Scope                                                                                                                                                                                              | Depends on |
| --- | ----------------------------------------- | ---------------- | -------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- | ---------- |
| 1   | `telemetry-pr1`                           | #93              | `ate_env.telemetry.tracing`, `TracerConfig`, the `tracing` extra, spans on `Client`, runtime, handle, driver; `TraceContextInterceptor`; context across `LoopRunner`; tests                        | none       |
| 2   | `telemetry-pr2`, `telemetry-pr2-ac-fixes` | #109, #113, #115 | `ate_env.observability`, `ObservabilityConfig`, the `metrics` extra, `ate_env.telemetry.metrics`, hooks in fleet, async fleet, `process_parallel`, drivers; `serve_metrics`; README section; tests | 1          |
| 3   | `telemetry-pr3`                           | #114             | `internal/telemetry`, `-enable-tracing`, `-metrics-listen-addr`, otelgrpc server and client handlers, `ate_env_api_*`, `x-ate-env-requested-at`, manifest flags; tests                             | none       |
| 4   | `telemetry-pr4`                           | #150             | `ClientOptions.TracerProvider` and `TraceServiceName`, `Attr*`, client handler, `Client.Get`; guest `-enable-tracing` and `-enable-metrics`; tests                                                 | 3          |
| f   | `python-rpc-semconv`                      | #164             | otelgrpc's `rpc.system.name` and full `rpc.method` on Python `Client` spans; `OTEL_SEMCONV_STABILITY_OPT_IN`                                                                                       | 1, 4       |
| 5   | `telemetry-pr5`                           | #168             | swebench event spans, GRPO and NeMo Gym on the `Observer`, `REPORT_DIR`, HTML report tool, `docs/observability.md`, `hack/metrics-docs` CI step, CHANGELOG, Python extras run in CI                | 2, 3, 4    |
| 6   | `telemetry-pr6`                           | #186 (open)      | VeRL SWE-bench example: one run report per RayJob, one trace per iteration, `run-telemetry-e2e.sh` against the acceptance collector                                                                | 5          |

## Risks and outcomes

| Risk                                                   | Effect                                                   | Mitigation planned                                                                  | Outcome                                                                                                                                                                              |
| ------------------------------------------------------ | -------------------------------------------------------- | ----------------------------------------------------------------------------------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------ |
| OTel context does not cross the `LoopRunner` thread    | client spans detach from fleet spans in the sync paths   | capture the context on the calling thread and attach it inside the coroutine        | `LoopRunner` runs the coroutine in a copy of the caller's `contextvars` context; `test_nests_sync_fleet_through_loop_runner` pins the parent (#93)                                   |
| `grpc.aio` interceptor ordering                        | the trace header is injected on only some call kinds     | one interceptor per call kind, after the auth interceptors                          | four `TraceContextInterceptor` classes; `test_metadata_carries_traceparent_on_all_call_kinds` (#93)                                                                                  |
| `prometheus-client` imported by the NeMo Gym provider  | registry side effects where nobody asked for metrics     | lazy registration; the `metrics` extra; the provider never constructs an `Observer` | as planned; `test_provider_lifecycle_has_no_telemetry_side_effects` runs the provider in a subprocess (#168)                                                                         |
| Guest image growth                                     | every actor pulls a larger image                         | size gate in PR 4                                                                   | 2.6 MiB on the stripped binary, under the 5 MB gate; shipped (#150)                                                                                                                  |
| Substrate's exporter not reachable on the test cluster | the client trace cannot show `ateapi` spans              | assert the parent at the `ate-env-api` boundary and record why                      | the guide documents the join as conditional on Substrate's exporter; #186's run shows `verl-trainer` to `ate-env-fleet` to `ate-env-api` in one trace                                |
| Maintainer conventions unknown                         | review churn on style                                    | read the last 20 merged PRs before `interfaces.md`                                  | [AGENTS.md](../../../AGENTS.md) and `.agents/skills/maintain-docs` now state the conventions (#126)                                                                                  |
| Required CI job names                                  | a renamed or added job cannot satisfy the `main` ruleset | none planned                                                                        | #168's first revision renamed the jobs and was blocked; the merged revision keeps `Go build, vet, test and license headers` and `Python client and integration tests` and adds steps |
