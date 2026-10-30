# Decisions and deviations log

Append-only. Each entry: date, who, what, why.

- 2026-10-21, orchestrator: the target asked for as "substrateenv-rl" is `ai-on-gke/sandboxenv-rl`;
  no repository of the other name exists. All plan documents name the real repository.
- 2026-10-21, orchestrator: plan documents live in a notes repository rather than in the target,
  because the target is a shared public project; only code, tests and user-facing docs go there.
- 2026-10-21, lead (seeded): W3C trace context travels in gRPC metadata rather than in object
  annotations, because every boundary in ate-env is gRPC and Substrate's `ateapi` already reads it
  with otelgrpc. The annotation pattern from agent-sandbox stays an open question for Substrate's
  asynchronous hops.
- 2026-10-21, lead (seeded): histograms in seconds with one bucket set; prefixes `ate_env_client_*`,
  `atefleet_*`, `ate_env_api_*`, `ate_env_guest_*`, next to Substrate's `ate_*`. agent-sandbox's SDK
  uses milliseconds; not copied.
- 2026-10-21, lead (seeded): configuration stays frozen dataclasses with the repository's
  unknown-field rejection; no pydantic dependency.
- 2026-10-21, lead (seeded): `prometheus-client` and the OpenTelemetry packages are extras
  (`metrics`, `tracing`); the base install stays `grpcio` and `protobuf`. agent-sandbox has
  `prometheus-client` in its base dependencies; not copied, because `ate_env` is imported by the
  NeMo Gym provider in processes that did not ask for a registry.
- 2026-10-21, orchestrator: kickoff verification of the acceptance cluster: Substrate in
  `ate-system` (ate-api-server, ate-controller, atenet-router, atenet-egress, ate-idp-server,
  postgres) and ate-env in `ate-env` (Deployment and Service `ate-env-api`, a WorkerPool). No
  telemetry namespace existed yet.
- 2026-10-29, orchestrator: the entries below record, after the fact, where the merged pull requests
  (#93, #109, #113, #114, #115, #150, #164, #168) depart from the 2026-10-21 contracts. The
  developer made these calls in the pull requests; the reviews accepted them.
- 2026-10-29, orchestrator: the plan documents move into the target repository under
  `docs/plans/telemetry/` at the owner's request, which reverses the 2026-10-21 entry. The notes
  repository keeps the 2026-10-21 snapshot. Cluster names, project ids and local paths were removed
  from the documents for the public repository.
- 2026-10-29, recorded from #109: metric prefix `ate_env_fleet_*` and span prefix
  `ate_env_fleet.<phase>` instead of `atefleet_*` and `atefleet.*`, so every family and span in the
  stack shares the `ate_env` prefix next to `ate_env_client_*`, `ate_env_api_*` and
  `ate_env_guest_*`.
- 2026-10-29, recorded from #93 and #150: span attribute keys are Substrate's `ateattr` keys for
  identities (`ate.actor.name`, `ate.atespace`, `ate.template.name`, `ate.template.atespace`) and
  `ate_env.*` for client-only fields, instead of the planned `env.*`, so spans join Substrate's logs
  and traces without a mapping and the Go and Python clients share one set of constants.
- 2026-10-29, recorded from #164: Python `Client` spans carry otelgrpc 0.72's `rpc.system.name` and
  the full `rpc.method`, with `OTEL_SEMCONV_STABILITY_OPT_IN` (`rpc/old`, `rpc/dup`) selecting the
  older keys, so a trace backend sees one vocabulary from Python, Go and `ate-env-api`.
- 2026-10-29, recorded from #93: `start_span` is a context manager; the planned `trace_span` and
  `async_trace_span` decorators were not ported. OpenTelemetry loads lazily through `_load_otel` and
  a module `__getattr__`; the planned `OPENTELEMETRY_AVAILABLE` flag and `Mock*` classes were not
  ported.
- 2026-10-29, recorded from #93: provider ownership. The client never replaces an application's
  `TracerProvider`; a `TracerManager` resolves the tracer at span time so a provider installed
  before or after the client wins. The client builds its own OTLP provider only when
  `OTEL_EXPORTER_OTLP_ENDPOINT` or `OTEL_EXPORTER_OTLP_TRACES_ENDPOINT` is set and no provider is
  installed. With neither, no exporter exists. The plan had one global provider owned by the client.
- 2026-10-29, recorded from #93: context crosses `LoopRunner` because the runner executes each
  coroutine in a copy of the caller's `contextvars` context, which carries the OpenTelemetry
  context; `process_parallel` copies the context into its pool threads. No `run_with_context` helper
  was added.
- 2026-10-29, recorded from #93: the `tracing` extra pins `opentelemetry-api`, `opentelemetry-sdk`
  and `opentelemetry-exporter-otlp-proto-grpc` at `>=1.43` with no upper bound. The planned
  `>=1.39,<1.42` cannot work: 1.43 is the first release whose `opentelemetry-proto` accepts protobuf
  7, which the client requires, and a cap conflicts with applications on a newer SDK.
- 2026-10-29, recorded from #109: `ObservabilityConfig.enable_metrics` defaults to `None`, meaning
  on when the `metrics` extra is installed and one INFO message otherwise; `True` warns when the
  extra is missing. The plan had `True`.
- 2026-10-29, recorded from #109 and #113: `RunReport.write(report_dir)` writes the `.txt` and
  `.json` pair and never overwrites; `reap` is in `RunReport.ORDER`; negative durations clamp to 0;
  collector caches are keyed by registry through weak references; a repo named `other` or `unknown`
  is labelled `other_repo` or `unknown_repo`; `MAX_FAMILIES` (64) bounds `family` with an `other`
  overflow.
- 2026-10-29, recorded from #115: `ate_env_fleet_release_failures_total{backend}` and
  `RunReport.release_errors` record failed releases instead of swallowing them. Not in the plan.
- 2026-10-29, recorded from #109: client histograms are
  `ate_env_client_{acquire,exec,suspend,warm_claim,release}_seconds{status}` plus the claim
  sub-phases `ate_env_client_{slot_wait,wire_claim,ready_to_first_command}_seconds{status,source}`.
  The planned `ate_env_client_wake_seconds` was not added; wake is an `ate_env_client.wake` span and
  a GRPO phase.
- 2026-10-29, recorded from #114: `internal/telemetry` has no `Instrumenter` interface. otelgrpc's
  server and client handlers cover every boundary, so `SetupOTel(ctx, serviceName)` returns a
  shutdown func only and `telemetry.Untraced(ctx)` keeps the scrape-time `ListActors` out of traces.
- 2026-10-29, recorded from #114: `-metrics-listen-addr` defaults to empty (metrics off) instead of
  `:9090`, because the listener has no authentication and no Host check and must be opt-in;
  `ate-env manifest --metrics-port` turns it on and adds the container and Service port.
  `-metrics-environments-ttl` (60 s) caches the control-plane read behind
  `ate_env_api_environments`.
- 2026-10-29, recorded from #114: `atespace` is a label on `ate_env_api_create_to_ready_seconds` and
  `ate_env_api_environments`; the open question is closed as bounded in practice.
- 2026-10-29, recorded from #114 and #168: the acceptance collector manifest lives in the repository
  as `tests/telemetry/otel-collector.yaml`, with OTLP/JSON file output, the `debug` exporter and an
  optional forward to a managed collector, and the observability guide documents the Cloud Trace
  routes. The plan left the location open.
- 2026-10-29, recorded from #150: the guest shipped. The dependencies add 2.6 MiB to the stripped
  binary (13.82 MB to 16.56 MB), under the 5 MB gate. Metrics have their own flag,
  `-enable-metrics`, separate from `-enable-tracing`, because the guest's `/metrics` is served
  without authentication on the guest listener and must be a choice of its own.
- 2026-10-29, recorded from #150: `Client.Get` was added to the Go client to back `get_environment`.
  With a caller-supplied `GRPCConn`, the client puts the trace carrier in each call's metadata
  instead of a stats handler on the connection.
- 2026-10-29, recorded from #168: the swebench `Recorder` takes an OpenTelemetry `trace.Tracer`
  (`WithTracer`, `SetTracer`) instead of an `Instrumenter`, and records each span after the fact
  from `ts - dur_ms` to `ts`, because the loader writes an event when its operation finishes and has
  no start/done pair. `swebench-loader run -enable-tracing` opens one `swebench-loader.run` span per
  run.
- 2026-10-29, recorded from #168: the metrics-docs freshness check is a step in the Go CI job and
  the Python extras run is a second step in the Python job, rather than new jobs or a matrix,
  because the `main` ruleset requires the jobs named `Go build, vet, test and license headers` and
  `Python client and integration tests`. #168's first revision renamed them and was blocked.
- 2026-10-29, recorded from #168: `benchmarking/telemetry_report/build_report.py` (an HTML report of
  one run's report, spans and scrapes) was added. Not in the plan.
- 2026-10-29, orchestrator: process. The series ran as reviewed pull requests from one developer
  branch per PR, with the repository's maintainers as reviewers. No `tests/RESULTS.md` or
  `review/findings.md` were produced; the evidence is each pull request's Tests section and CI. #113
  was the one pass driven directly by the criteria in this directory.
- 2026-10-29, orchestrator: `ate_env.telemetry.trace_recorder` (#173, benchmark series 4/8) writes
  the client spans as v1 `TraceEvent` JSONL under `ENV_BENCH_MODE=trace`. It consumes this plan's
  spans and is specified in `internal/benchmarking/PLAN.md`; it is outside this plan's scope.
- 2026-10-29, orchestrator: #186 (telemetry in the VeRL SWE-bench example) is open as the sixth and
  last pull request of the series. Its end-to-end run on GKE: 622 spans in 6 traces, 0 orphan roots,
  every `verl.iteration` trace joining `verl-trainer`, `ate-env-fleet` and `ate-env-api`.
- 2026-10-29, orchestrator: still open. A trace-context label on actors so Substrate's scheduler and
  atelet restore join the client trace; `docs/glossary.md` has no telemetry section (the phase
  glossary is in `docs/observability.md`); no overhead measurement of telemetry off against on is
  recorded.
- 2026-10-30, orchestrator: the Substrate trace join is closed, from reading Substrate at
  `28520c0d`. The atenet router's ingress extracts `traceparent` from the request metadata and calls
  `ResumeActor` under it; `ateapi` stamps actor identity on its server span, opens a `step.<name>`
  span per workflow step and calls atelet `Restore` with otelgrpc; ateworker's spans follow through
  the atelet relay. Every component samples parent-based, so a trace the client samples is complete
  through to the worker once Substrate exports to the same backend. No trace-context label on actors
  is needed. Substrate's `Actor state changed` records carry the trace id as well.
