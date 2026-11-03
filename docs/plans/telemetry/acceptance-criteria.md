# Acceptance criteria: telemetry for sandboxenv-rl

Status: describes `main` at `3b03b03` (2026-10-29). Each criterion names the test or evidence that
backs it on `main`. Commands are the ones to re-run; [test-plan.md](./test-plan.md) orders them.

Conventions:

- `$VENV_BASE` = a venv with `pip install -e 'clients/python[dev]'`; `$VENV_FULL` = a venv with
  `pip install -e 'clients/python[dev,tracing,metrics]'`. Run from the repository root.
- `$K` = `kubectl -n ate-env`; `$KT` = `kubectl -n ate-env-telemetry` on the acceptance cluster.
- "In-memory exporter" =
  `opentelemetry.sdk.trace.export.in_memory_span_exporter.InMemorySpanExporter` behind a
  `SimpleSpanProcessor` (the `spans` fixture in `clients/python/tests`).
- Names in backticks are the contracts in [interfaces.md](./interfaces.md); a criterion fails if the
  implementation uses another name.

| ID    | Title                                                                               | PR      | Status at `3b03b03`                                                                                           |
| ----- | ----------------------------------------------------------------------------------- | ------- | ------------------------------------------------------------------------------------------------------------- |
| AC-1  | Base install carries no telemetry dependency and imports clean                      | 1, 2    | pass: `pyproject.toml`, `test_registry_import_does_not_load_prometheus`                                       |
| AC-2  | Telemetry enabled without the extras is a logged no-op                              | 1, 2    | pass: `test_missing_extra_logs_one_error`, `test_observer_without_layers_is_report_only`                      |
| AC-3  | One in-process trace from `ate_env_fleet.run` to `ate_env_client.*`, sync and async | 1, 2    | pass: `test_nests_*` in `test_telemetry_tracing.py`                                                           |
| AC-4  | The W3C context is on the wire for all four gRPC call kinds                         | 1       | pass: `test_metadata_carries_traceparent_on_all_call_kinds`, `test_metadata_has_no_traceparent_when_disabled` |
| AC-5  | `ate-env-api` continues the client trace and exports it                             | 3       | pass: `internal/apiservice/telemetry_test.go`; #114 and #186 cluster runs                                     |
| AC-6  | `ate-env-api` serves the documented `ate_env_api_*` families                        | 3       | pass: `internal/telemetry/metrics_test.go`, `cmd/ate-env-api/metrics_test.go`, `docs/metrics.md`              |
| AC-7  | `RunReport` has the agent-sandbox-rl shape                                          | 2       | pass: `test_report_shape_and_summary_order`, `test_report_summary_order_has_reap`                             |
| AC-8  | Prometheus series register lazily with exactly the documented labels                | 2       | pass: `test_registry_*` in `test_observability.py`, `test_telemetry_metrics.py`                               |
| AC-9  | Labels stay bounded under many images and environments                              | 2       | pass: `test_cardinality_*`                                                                                    |
| AC-10 | swebench loader JSONL is unchanged and every event has a span                       | 5       | pass: `events_test.go` with `testdata/events.golden.txt`                                                      |
| AC-11 | Go client: provider injection, span names, no exporter by default                   | 4       | pass: `clients/go/tracing_test.go`                                                                            |
| AC-12 | Docs: `docs/observability.md`, generated `docs/metrics.md`, CI freshness check      | 5       | pass: CI step `docs/metrics.md is generated from internal/telemetry`                                          |
| AC-13 | CI runs the suites with and without the extras; lint and boilerplate pass           | 1, 2, 5 | pass: `.github/workflows/ci.yaml`                                                                             |
| AC-14 | Blast radius: existing tests unchanged pass; telemetry-off overhead recorded        | all     | partial: suites green; overhead not recorded                                                                  |

## AC-1: Base install carries no telemetry dependency

- Commands: in `$VENV_BASE`: `pip list | grep -iE 'opentelemetry|prometheus'` and
  `python -c "import ate_env, ate_env.observability, ate_env.telemetry; print('ok')"`.
- Expected: the grep prints nothing; the import prints `ok`.
- Pass rule: both as expected; `clients/python/pyproject.toml` base `dependencies` lists only
  `grpcio` and `protobuf`; the extras `tracing` and `metrics` exist.
- On `main`: `dependencies = ["grpcio>=1.84", "protobuf>=7.35"]`; extras `tracing` (three
  `opentelemetry-*>=1.43` packages) and `metrics` (`prometheus-client>=0.20`).

## AC-2: Telemetry enabled without the extras is a no-op

- Command: in `$VENV_BASE`, run the mock-backend quickstart from the test plan with
  `FleetConfig(tracing=TracerConfig(enable_tracing=True), observability=ObservabilityConfig(enable_metrics=True))`.
- Expected: the run completes; one ERROR for the missing `tracing` extra and one WARNING for the
  missing `metrics` extra; `fleet.report.summary()` still prints.
- Pass rule: no exception, the report exists, each message appears once per process.

## AC-3: One in-process trace, sync and async

- Command: `pytest clients/python/tests/test_telemetry_tracing.py -k nests` in `$VENV_FULL`.
- Expected: with the in-memory exporter, `SandboxFleet.run` on the mock backend yields
  `ate_env_fleet.run` to `ate_env_fleet.acquire` to `ate_env_client.create_environment`, and
  `ate_env_fleet.process` to `ate_env_client.exec`, each child's `parent.span_id` equal to its
  parent's `span_id`; the same tree under `AsyncSandboxFleet`, through `SubstrateBackendDriver`'s
  `LoopRunner` path against the threaded fake server, and under a user span that parents the run.
- Pass rule: the assertions hold for the sync, async, runner and user-span cases.

## AC-4: The W3C context is on the wire

- Command: `pytest clients/python/tests/test_telemetry_tracing.py -k metadata`.
- Expected: the recorder server sees request metadata for one unary-unary, unary-stream,
  stream-unary and stream-stream call; each carries `traceparent` matching the client's current span
  and no `baggage` key; with tracing disabled no `traceparent` is sent.
- Pass rule: four call kinds covered, both states.

## AC-5: `ate-env-api` continues the client trace and exports it

- Setup: `ate-env-api` deployed with
  `ate-env manifest --enable-tracing --otlp-endpoint http://otel-collector.ate-env-telemetry:4317 --otlp-insecure --metrics-port 9090`;
  the collector from `tests/telemetry/otel-collector.yaml`.
- Command: run the quickstart from `$VENV_FULL` with the same `OTEL_*` variables and
  `ObservabilityConfig(enable_tracing=True)`; then
  `$KT cp -c files deploy/otel-collector:/data ./otel-data` and read `./otel-data/spans.jsonl`, or
  `$KT logs deploy/otel-collector | grep -A3 'Trace ID'`.
- Expected: spans from service `ate-env-fleet` (`ate_env_fleet.acquire`,
  `ate_env_client.create_environment`) and from service `ate-env-api`
  (`ateenv.v1alpha.EnvironmentService/CreateEnvironment`) share one trace id; the server span's
  parent id is the client span id. When Substrate exports to the same collector, `ateapi.Control/*`
  spans share the trace id; otherwise record that the join stops at `ate-env-api`.
- Pass rule: one trace id across client and server spans, parent id matching.
- Unit coverage on `main`: `TestTracingContinuesClientTrace` asserts the server span's parent from
  incoming metadata; `TestCreateToReadyUsesRequestedAtHeader` and `TestWakeWaitOutcomes` cover the
  two histograms.

## AC-6: `ate-env-api` serves the documented metric families

- Command: `$K port-forward deploy/ate-env-api 9090:9090 &` then
  `curl -s localhost:9090/metrics | grep -E '^ate_env_api_' | cut -d'{' -f1 | sort -u`.
- Expected: exactly `ate_env_api_requests_total`, `ate_env_api_request_duration_seconds`,
  `ate_env_api_create_to_ready_seconds`, `ate_env_api_wake_wait_seconds`, `ate_env_api_environments`
  and `ate_env_api_build_info` (with the `_bucket`, `_sum` and `_count` series of the histograms);
  after one quickstart run, `requests_total` has increased for `CreateEnvironment` and
  `DeleteEnvironment` and `create_to_ready_seconds_count` is at least 1.
- Pass rule: the family set matches [docs/metrics.md](../../metrics.md); counters moved.

## AC-7: `RunReport` has the agent-sandbox-rl shape

- Command: `pytest clients/python/tests/test_observability.py -k report`.
- Expected: `to_dict()` keys are exactly
  `strategy, total_s, phases, claims, tasks_ok, tasks_err, warm_replicas_total, warm_replicas_peak, environment`;
  `phases` holds `count, total_s, max_s` per phase; `summary()` prints one row per phase in
  `RunReport.ORDER` and a `TOTAL` row; negative durations clamp to 0; `write()` produces the `.txt`
  and `.json` pair without overwriting.
- Pass rule: assertions hold.

## AC-8: Prometheus series register lazily with the documented labels

- Command: `pytest clients/python/tests/test_observability.py -k registry` and
  `pytest clients/python/tests/test_telemetry_metrics.py`.
- Expected: importing `ate_env` and `ate_env.observability` adds nothing to
  `prometheus_client.REGISTRY` and does not import `prometheus_client`; constructing an `Observer`
  with metrics on registers the `ate_env_fleet_*` families once; a second `Observer` and a new
  registry do not raise or lose samples; each family's label names are exactly those in
  [interfaces.md](./interfaces.md); `ate_env_client_*` histograms register on first use only;
  `serve_metrics(port)` answers `/metrics`.
- Pass rule: assertions hold.

## AC-9: Labels stay bounded

- Command: `pytest clients/python/tests/test_observability.py -k cardinality`.
- Expected: a mock run over 50 distinct images from 5 repo families and 200 tasks produces at most 5
  distinct `family` values; no label value contains an environment id, actor id, run id or image
  tag; no label named `env_id`, `instance`, `image` or `run_id` on any family; a repo named `other`
  or `unknown` does not collide with the overflow and no-family values; more than `MAX_FAMILIES`
  families collapse into `other`.
- Pass rule: assertions hold.

## AC-10: swebench loader JSONL unchanged, one span per event

- Command: `go test ./internal/benchmarking/swebench/... -run 'Event|Recorder|Golden' -v`.
- Expected: the `Recorder` output for the fixed event sequence equals `testdata/events.golden.txt`
  byte for byte with tracing off and on; with a test exporter, each event yields one span named
  after `Event.Event` with `run`, `env`, `instance`, `step`, `task` as attributes, status from `OK`,
  start `ts - dur_ms` and end `ts`.
- Pass rule: golden file identical; span count equals event count.

## AC-11: Go client provider injection and span names

- Command: `go test ./clients/go/... -run Trac -v`.
- Expected: with `ClientOptions{TracerProvider: tp}` and an in-memory exporter, calls produce
  `ate-env-go-client.create_environment`, `.exec`, `.read_file`, `.write_file` with the `Attr*`
  attributes; without a provider no span is recorded and no exporter is constructed;
  `TraceServiceName` changes the prefix; a caller-supplied `GRPCConn` still carries `traceparent`.
- Pass rule: assertions hold.

## AC-12: Docs

- Commands: `go run ./hack/metrics-docs | diff -u docs/metrics.md -`; open
  [docs/observability.md](../../observability.md).
- Expected: the diff is empty; the guide covers enabling flags and variables, what nests under what
  with a span-tree example, the metric families or a link to `docs/metrics.md`, the cardinality
  rule, the run report format and phase glossary, the collector and Cloud Trace routes, and the HTML
  report tool; `clients/python/README.md` has an Observability section; `CHANGELOG.md` lists the
  user-visible additions.
- Pass rule: all present; CI runs the diff.

## AC-13: CI

- Expected: `.github/workflows/ci.yaml` runs the Python client suite once with `[dev]` and once with
  `[dev,tracing,metrics]`, `ruff check`, the boilerplate check, `go test ./...`, and the
  metrics-docs diff; all green on each PR. The job names `Go build, vet, test and license headers`
  and `Python client and integration tests` stay as the `main` ruleset requires.
- Pass rule: the workflow shows the steps; the PR's checks are green.

## AC-14: Blast radius and overhead

- Commands: `pytest clients/python/tests -q` in `$VENV_BASE` (telemetry off); the overhead script
  from the test plan (200 tasks, mock backend, `max_concurrent=8`) three times each with telemetry
  off, with the report only, and with tracing and metrics on into the in-memory exporter.
- Expected: every pre-existing test passes unchanged; the three wall-clock medians are recorded.
- Status: the suites are green in CI on every PR of the series. No overhead measurement is recorded
  in the pull requests; the procedure stays in [test-plan.md](./test-plan.md) section 7.
