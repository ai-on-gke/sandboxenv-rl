# Properties: telemetry for sandboxenv-rl

Status: checked against `main` at `9b2148a` (2026-11-02) plus #221, which fixes AC-15 and merges
first. Most property tests here are proposed and not in the repository: `pgregory.net/rapid` is not
in `go.mod`, and the Hypothesis tests named "Proposed test" do not exist. Two parts exist:

- `clients/python/tests/test_warm_pool_accounting.py` (#221): the P-9 to P-11 stateful test
  `test_warm_pool_accounting` and five plain interleaving tests. `hypothesis` is in the `dev` extra
  since #221.
- [verify/](./verify/README.md): hand-written Dafny models of P-1 to P-11 and of the two bugs review
  found in #221's first version. CI does not run them.

The tests named as "example coverage on `main`" exist and run in CI.

Each property is a universal statement, _"For any …"_, over a range of generated inputs. It
validates one or more requirements, tagged **Validates: AC-n.m**. The requirement numbers are listed
in the [requirement index](#requirement-index); they refine the criteria in
[acceptance-criteria.md](./acceptance-criteria.md). A property-based test shrinks a failing input to
a minimal counter-example, which then becomes an `@example` regression.

State on `main` with #221:

| Item            | Requirement | State                                                                                                                                                                                                    |
| --------------- | ----------- | -------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| P-9, P-10, P-11 | AC-15       | Fixed by #221. Before it, a recycle or `warm_pool` that awaited across a reap pooled its actor after the teardown, and overlapping `warm_pool` calls each provisioned the full shortfall.                |
| P-14            | AC-3.4      | Open. `SandboxHandle.exec` and `exec_async` (`clients/python/src/ate_env/handle.py`) call `_note_first_command()` before `_exec_span` starts the span, so another thread's exec span can start first.    |
| claim/reap gap  | none        | Open. A warm claim pops an actor and awaits `get()`; if a reap deletes the actor meanwhile and `get()` still succeeds, `acquire` returns a deleted environment. No property covers it; see the findings. |

## Labels and cardinality (AC-9)

### P-1: Family registry stays bounded

_For any_ stream of tasks and images, the family registry holds at most `MAX_FAMILIES` (64) names,
and at most `MAX_FAMILIES + 2` distinct `family` values are emitted: the registered families,
`other` (overflow) and `unknown` (unparseable input). Neither `other` nor `unknown` counts toward
the cap.

**Validates: AC-9.1**

- Generators: `st.lists(task_or_image(), max_size=500)`, where `task_or_image()` mixes tasks with
  `metadata["repo"]` from `st.from_regex(r"[A-Za-z0-9_.-]{1,40}/[A-Za-z0-9_.-]{1,40}")`, SWE-bench
  image names (`sweb.eval.{x86_64,arm64}.<repo>__<repo>-<n>:<tag>`), the reserved names, and
  arbitrary text.
- Proposed test: Hypothesis `test_family_registry_bounded`.
- Example coverage on `main`: `test_family_label_is_capped`,
  `test_cardinality_bounded_labels_over_many_images`.

### P-2: Labels are the family or `other`

_For any_ task or image, the `family` label is the input's repo family after escaping, or `other`,
or `unknown` when no family can be parsed. Escaping appends `_repo` to a family matching `other` or
`unknown` followed by zero or more `_repo` (`other` to `other_repo`, `other_repo` to
`other_repo_repo`), so the mapping stays one-to-one.

**Validates: AC-9.2**

- Generators: as P-1, weighted toward the reserved names, their `_repo` forms and case variants.
- Proposed test: Hypothesis `test_family_label_shape`.
- Example coverage on `main`: `test_cardinality_reserved_repo_names`,
  `test_cardinality_escaped_repo_names_stay_unique`.

### P-3: `other` only on overflow, and labels are stable

_For any_ stream of tasks, `other` is returned only when the registry is full and the family is new.
The bare reserved names never enter the registry. A family seen once keeps the same label for the
rest of the process.

**Validates: AC-9.3, AC-9.4**

- Generators: as P-1, with streams longer than 64 distinct families, and repeats.
- Proposed test: Hypothesis `test_other_only_on_overflow`.
- Example coverage on `main`: `test_cardinality_full_registry_only_new_families_get_other`.

### P-18: No identifiers in label values

_For any_ task, image, environment id, actor id and run id, no emitted label value contains the
environment id, actor id, run id or image tag, and no family defines a label named `env_id`,
`instance`, `image` or `run_id`.

**Validates: AC-9.5**

- Generators: `st.uuids()`-based ids, `st.from_regex(r"sb-[0-9a-f]{16}")`, image tags from
  `st.from_regex(r"[A-Za-z0-9_.-]{1,30}")`, tasks as P-1; a mock run over them.
- Proposed test: Hypothesis `test_labels_carry_no_ids` (scrapes the registry after the run).
- Example coverage on `main`: `test_cardinality_bounded_labels_over_many_images`.

## W3C context propagation (AC-4)

P-4 to P-7 are properties of `inject_metadata` (`ate_env.telemetry.tracing`).

### P-4: Caller metadata is preserved

_For any_ caller metadata (any keys, any case, any values) and any current span, the output of
`inject_metadata` starts with the caller's metadata, unchanged and in order.

**Validates: AC-4.2**

- Generators: `st.lists(st.tuples(header_key(), st.text()), max_size=10)`, where `header_key()`
  includes `traceparent`, `TraceParent`, `baggage` and random keys; tracing on or off.
- Proposed test: Hypothesis `test_inject_preserves_metadata`.

### P-5: Only trace headers are added

_For any_ caller metadata, every entry `inject_metadata` appends is a carrier entry (`traceparent`
or `tracestate`).

**Validates: AC-4.1, AC-4.3**

- Generators: as P-4.
- Proposed test: Hypothesis `test_inject_adds_only_carrier`.

### P-6: `baggage` is never added

_For any_ caller metadata and any OpenTelemetry context, including one carrying baggage, the output
contains `baggage` only if the caller's metadata did. This holds because `_current_carrier` filters
to `_CARRIER_KEYS`.

**Validates: AC-4.3**

- Generators: as P-4, plus contexts with `opentelemetry.baggage.set_baggage` entries.
- Proposed test: Hypothesis `test_inject_never_adds_baggage`.
- Example coverage on `main`: `test_carrier_never_carries_baggage`.

### P-7: Each trace header appears exactly once

_For any_ caller metadata, each carrier key appears in the output exactly once, compared
case-insensitively: the caller's value if the caller set it, otherwise the current context's.

**Validates: AC-4.1, AC-4.2**

- Generators: as P-4.
- Proposed test: Hypothesis `test_inject_no_duplicates`.

### P-15: The context is on the wire for every call kind

_For any_ call kind (unary-unary, unary-stream, stream-unary, stream-stream), caller metadata and
tracing state, the metadata a fake `grpc.aio` server receives satisfies P-4 to P-7. With tracing
enabled its `traceparent` encodes the caller's current span id. With tracing disabled it equals the
caller's metadata.

**Validates: AC-4.1, AC-4.2, AC-4.3, AC-4.4**

- Generators: `st.sampled_from(CALL_KINDS)`, metadata as P-4, `st.booleans()` for tracing.
- Proposed test: Hypothesis `test_context_on_the_wire` (one in-process fake server per module,
  reused across examples).
- Example coverage on `main`: `test_metadata_carries_traceparent_on_all_call_kinds`,
  `test_metadata_has_no_traceparent_when_disabled`.

## Trace shape (AC-3)

### P-13: One rooted trace per run, with documented names only

_For any_ workload (1 to 20 tasks, concurrency 1 to 8, sync or async fleet, any strategy, a
`process_fn` that may raise, sleep or run 0 to 5 execs and file operations), the run emits exactly
one `ate_env_fleet.run` span. Every other span of the run reaches it through its parent chain. Every
`ate_env_client.exec`, `.read_file` and `.write_file` span's parent is the `ate_env_fleet.process`
span of the same task. Every span name and attribute key is documented in
[interfaces.md](./interfaces.md).

**Validates: AC-3.1, AC-3.2, AC-3.3, AC-3.5**

- Generators: a `workload()` composite of `st.integers(1, 20)` tasks, `st.integers(1, 8)`
  concurrency, `st.sampled_from(["sync", "async"])`, `st.sampled_from(STRATEGIES)`, and per task a
  list of operations from `st.sampled_from(["exec", "read", "write", "raise", "sleep"])`.
- Proposed test: Hypothesis `test_one_rooted_trace_per_run` (mock backend, in-memory exporter).
- Example coverage on `main`: the `test_nests_*` tests in `test_telemetry_tracing.py`.

### P-14: Exactly one first-command span per acquisition

_For any_ acquired sandbox and any sequence of exec, read and write calls on its handle, including
execs started concurrently from several threads, exactly one `ate_env_client.exec` span carries
`ate_env.first_command=true`, and it is the first exec span started on that handle.

**Validates: AC-3.4**

- Generators: `st.lists(st.sampled_from(["exec", "read", "write"]), min_size=1, max_size=15)`,
  `st.integers(1, 4)` threads.
- Proposed test: Hypothesis `test_first_command_exactly_once`.
- Example coverage on `main`: `test_first_command_marks_only_the_first_exec_per_sandbox` (sequential
  execs only).
- **Fails on `main` (open, not addressed by #221).** A run of this property against the series'
  code, with execs started concurrently on one handle, marked a span that was not the first started
  in 9 of 12 runs. No run marked more than one span, so "exactly one" held and "the first started"
  did not. The cause is in `clients/python/src/ate_env/handle.py`: `exec` and `exec_async` call
  `_note_first_command()` before `_exec_span` starts the span. `_note_first_command` is also a
  check-and-set without a lock. The fix belongs in the code; the property stays as written.

## Run report (AC-7)

### P-8: Phase statistics stay consistent

_For any_ sequence of `add_phase(name, d)` calls, with any names and any finite durations including
negative ones, every phase keeps `0 <= max_s <= total_s <= count * max_s`.

**Validates: AC-7.3**

- Generators: `st.lists(st.tuples(phase_name(), st.floats(-1e3, 1e4, allow_nan=False)))`.
- Proposed test: Hypothesis `test_phase_stats_consistent`.
- Example coverage on `main`: `test_report_clamps_negative_phase_duration`.

### P-16: The report matches its operations

_For any_ sequence of report operations (`add_phase`, `add_task("ok" | other)`, `add_claim`,
`add_release_error`, warm add and remove), `to_dict()` has exactly the documented keys
(`strategy, total_s, phases, claims, tasks_ok, tasks_err, release_errors, warm_replicas_total, warm_replicas_peak, environment`).
Each phase's `count` equals the number of `add_phase` calls for it and its `total_s` equals the sum
of the clamped durations. `tasks_ok + tasks_err` equals the number of `add_task` calls.
`release_errors` equals the number of `add_release_error` calls, and `summary()` has a
`release_errors=` line exactly when it is nonzero. The `summary()` rows follow `RunReport.ORDER`,
then other phases in first-seen order, then `TOTAL`. `json.loads(json.dumps(to_dict()))` equals
`to_dict()`.

**Validates: AC-7.1, AC-7.2, AC-7.4, AC-7.5, AC-7.7**

- Generators: `st.lists(report_op(), max_size=100)`, where `phase_name()` mixes the documented
  phases with arbitrary names.
- Proposed test: Hypothesis `test_report_matches_operations` (a model-based check against a plain
  dict that replays the same operations).
- Example coverage on `main`: `test_report_shape_and_summary_order`,
  `test_report_summary_order_has_reap`, `test_run_report_defaults_have_no_release_errors`,
  `test_fleet_run_report_and_metric_show_failed_releases`.

## Metrics registry (AC-8)

### P-17: Lazy, single registration with documented labels

_For any_ sequence of operations from {import, reload `ate_env.observability`, create
`Observer(enable_metrics=b)`, record a client timing}, nothing is registered before the first
metrics-enabled `Observer` or client timing. After it, each documented family (including
`ate_env_fleet_release_failures_total{backend}`) is registered exactly once with exactly its
documented label names. No operation raises.

**Validates: AC-8.1, AC-8.2, AC-8.3**

- Generators: a Hypothesis stateful machine over those four rules, with a fresh `CollectorRegistry`
  per example.
- Proposed test: Hypothesis `MetricsRegistryMachine`.
- Example coverage on `main`: the `test_registry_*` tests in `test_observability.py`.

## Missing extras (AC-2)

### P-12: Missing extras degrade to a logged no-op

_For any_ combination of `enable_tracing`, `enable_metrics` and installed extras (none, `tracing`,
`metrics`, both), a mock run completes and `fleet.report` exists. Each layer that is enabled but
missing its extra logs exactly one `ERROR` or `WARNING` naming that extra. No telemetry code raises.

**Validates: AC-2.1, AC-2.2, AC-2.3**

- Generators: `st.booleans()` twice for the flags, `st.sampled_from` over the four install states
  (simulated by blocking the imports in `sys.modules`), plus a small workload as in P-13.
- Proposed test: Hypothesis `test_missing_extras_noop`.
- Example coverage on `main`: `test_missing_extra_logs_one_error`,
  `test_missing_extra_logs_one_warning`, `test_observer_without_layers_is_report_only`.

## Benchmarks and Go client (AC-10, AC-11)

### P-19: Tracing does not change the JSONL, and every event has one span

_For any_ event sequence (any event names, runs, jobs, workers, environments, instances, steps,
tasks, durations, `ok` or `err`, optional `attempts`, `count`, `exit`, `reward`), the recorder's
JSONL is byte-identical with and without a tracer. With a tracer, the number of spans equals the
number of recorded events. Each span is named after its event's `event` and ends at `ts`, starting
`dur_ms` earlier. Its attribute keys are a subset of
`run, job, worker, env, instance, step, task, name, attempts, count, exit, reward`: `run`, `step`
and `task` always, the others exactly when set. Its status is `Ok` when `ok`, else `Error` with
`err` (or `"not ok"`).

**Validates: AC-10.2, AC-10.3**

- Generators: a `rapid` generator of `Event` values with random fields, recorded in a random order.
- Proposed test: Go `rapid` `TestRecorderTracingIsTransparent`.
- Example coverage on `main`: `TestRecorderGoldenJSONL`, `TestRecorderSpans`,
  `TestRecorderNoTracerRecordsNoSpans` in `internal/benchmarking/swebench/events_test.go`.

### P-20: Spans only with a provider, one per call

_For any_ sequence of Go client calls (`CreateEnvironment`, `Exec`, `ReadFile`, `WriteFile`, …) and
any `TraceServiceName`: with a nil `TracerProvider`, zero spans are recorded and no exporter is
constructed. With a provider, there is exactly one span per call, named `<TraceServiceName>.<op>`,
with only documented `ate.*` and `ate_env.*` attribute keys plus, on CLIENT spans, the `rpc.*` and
`server.*` keys in [interfaces.md](./interfaces.md).

**Validates: AC-11.1, AC-11.2**

- Generators: `rapid.SliceOf(rapid.SampledFrom(ops))`,
  `rapid.StringMatching("[a-z][a-z0-9-]{0,20}")`.
- Proposed test: Go `rapid` `TestClientSpansFollowProvider`.
- Example coverage on `main`: `TestTracingSpansWithProvider`, `TestTracingOffWithoutProvider`,
  `TestTracingServiceNamePrefix`, `TestTracingSpanKinds` in `clients/go/tracing_test.go`.

## Warm-pool accounting under concurrency (AC-15)

`SubstrateBackendDriver` coroutines share one event loop and interleave at every `await`. P-9 to
P-11 share the Hypothesis **stateful** test `test_warm_pool_accounting` in
`clients/python/tests/test_warm_pool_accounting.py` (#221). Its rules are `acquire_new`,
`acquire_warm`, `start_recycle` and `finish_recycle` (paused inside `await suspend()`),
`start_warm_pool` and `finish_warm_pools` (paused inside `await create()`), `fleet_teardown` and
`idle`. A fake client pauses at those awaits, so Hypothesis generates the interleavings as well as
the operations. The machine starts no `warm_pool` after a teardown and never makes `get()` fail, so
bugs A and B (see the findings) are covered by the plain tests
`test_warm_pool_after_teardown_ignores_the_old_reservation` and
`test_failed_warm_claim_after_teardown_deletes_the_environment` in the same file.

Dafny: `WarmPool` in [verify/warm_pool.dfy](./verify/warm_pool.dfy) and
[verify/warm_pool_generations.dfy](./verify/warm_pool_generations.dfy) prove the #221 design keeps
P-9 to P-11 for every interleaving within the model.

### P-9: The warm-replica gauge tells the truth

_For any_ sequence of acquire, recycle, `warm_pool` and teardown operations, interleaved at their
awaits, after every step `ate_env_fleet_warm_replicas{backend}` equals the number of actors in the
driver's warm pool.

**Validates: AC-15.1**

- Test: `test_warm_pool_accounting`, invariant `gauge_equals_pool`.
- Fixed by #221. Before it, in the P-10 recycle interleaving, the pool held 1 actor and the gauge
  read 0.

### P-10: Nothing survives teardown

_For any_ such sequence, once teardown has run and every paused call has resumed, no environment
from the run is alive on Substrate or tracked by the driver.

**Validates: AC-15.2**

- Test: `test_warm_pool_accounting`, invariant `nothing_survives_teardown`;
  `test_recycle_paused_across_teardown_deletes_the_environment`,
  `test_warm_pool_paused_across_teardown_deletes_its_actors`,
  `test_failed_warm_claim_after_teardown_deletes_the_environment`.
- Fixed by #221. Before it, the stateful test shrank it to two minimal counter-examples:

  ```text
  state.start_warm_pool()                    # create() paused
  state.fleet_teardown()
  state.finish_warm_pools()                  # -> 3 warm actors alive after teardown

  owned_0 = state.acquire_new()
  recycling_0 = state.start_recycle(aid=owned_0)   # suspend() paused
  state.fleet_teardown()
  state.finish_recycle(aid=recycling_0)      # -> sb-0 alive after teardown
  ```

### P-11: `warm_pool` never over-provisions

_For any_ such sequence, the warm pool never holds more than `R` actors created by `warm_pool` for
one pool key.

**Validates: AC-15.3**

- Test: `test_warm_pool_accounting`, invariant `warm_pool_never_over_provisions`;
  `test_overlapping_warm_pool_calls_provision_once`.
- Fixed by #221. Before it, the shrunk counter-example was:

  ```text
  state.start_warm_pool()
  state.start_warm_pool()
  state.finish_warm_pools()                  # -> 6 warm actors pooled, asked for 3
  ```

## Requirement index

The requirement numbers the properties validate. Each refines the criterion of the same number in
[acceptance-criteria.md](./acceptance-criteria.md).

| ID      | Requirement                                                                                                                        |
| ------- | ---------------------------------------------------------------------------------------------------------------------------------- |
| AC-1.1  | The base install depends only on `grpcio` and `protobuf` and offers the extras `tracing` and `metrics`.                            |
| AC-1.2  | Importing `ate_env`, `ate_env.observability` or `ate_env.telemetry` in the base install loads neither extra.                       |
| AC-2.1  | With a layer enabled and its extra missing, the run completes and `fleet.report` exists.                                           |
| AC-2.2  | Each enabled layer missing its extra logs exactly one `ERROR` or `WARNING` naming the extra.                                       |
| AC-2.3  | Telemetry code raises no exception.                                                                                                |
| AC-3.1  | One `ate_env_fleet.run` span per run, sync and async.                                                                              |
| AC-3.2  | Every `ate_env_fleet.*` and `ate_env_client.*` span of a run descends from its `ate_env_fleet.run` span, across threads and loops. |
| AC-3.3  | Each task's exec and file spans are children of that task's `ate_env_fleet.process` span.                                          |
| AC-3.4  | The first exec started on an acquired sandbox, and only that one, carries `ate_env.first_command=true`.                            |
| AC-3.5  | Only span names and attribute keys documented in [interfaces.md](./interfaces.md).                                                 |
| AC-4.1  | With tracing on, exactly one `traceparent` matching the current span on all four call kinds.                                       |
| AC-4.2  | Caller metadata passes through unchanged and in order; a key the caller set is not added again (case-insensitive).                 |
| AC-4.3  | No `baggage` header the caller did not send.                                                                                       |
| AC-4.4  | With tracing off, the caller's metadata is sent unchanged.                                                                         |
| AC-5.1  | `ate-env-api` with `-enable-tracing` records its span under the client's trace.                                                    |
| AC-5.2  | `ate-env-api` exports to the configured OTLP endpoint.                                                                             |
| AC-6.1  | `ate-env-api` serves exactly the documented `ate_env_api_*` families and labels.                                                   |
| AC-6.2  | Calls move `ate_env_api_requests_total` and `ate_env_api_create_to_ready_seconds`.                                                 |
| AC-7.1  | `to_dict()` keys and `phases` entry keys are exactly the documented ones.                                                          |
| AC-7.2  | `add_phase(name, d)` adds 1 to `count`, `max(d, 0)` to `total_s`, and raises `max_s` to at least `max(d, 0)`.                      |
| AC-7.3  | Every phase keeps `0 <= max_s <= total_s <= count * max_s`, including after negative durations.                                    |
| AC-7.4  | `add_task("ok")` counts `tasks_ok`; any other status counts `tasks_err`.                                                           |
| AC-7.5  | `summary()` rows follow `ORDER`, then other phases first-seen, then `TOTAL`; `to_dict()` survives a JSON round trip.               |
| AC-7.6  | A mock run records the documented phase set.                                                                                       |
| AC-7.7  | `add_release_error()` adds 1 to `release_errors`; `summary()` prints `release_errors=N` while it is nonzero.                       |
| AC-8.1  | Importing `ate_env` or `ate_env.observability` registers nothing.                                                                  |
| AC-8.2  | The first metrics-enabled `Observer` or client timing registers each documented family once, with exactly its labels.              |
| AC-8.3  | A second `Observer` or a module reload neither raises nor registers a family twice.                                                |
| AC-8.4  | `serve_metrics(port)` answers `/metrics`.                                                                                          |
| AC-9.1  | At most `MAX_FAMILIES` (64) registered families and at most `MAX_FAMILIES + 2` distinct `family` values per process.               |
| AC-9.2  | Repo families matching the reserved names (and their `_repo` forms) are escaped with `_repo`; bare reserved names never register.  |
| AC-9.3  | `other` only when the registry is full and the family is new.                                                                      |
| AC-9.4  | A registered family always gets the same label.                                                                                    |
| AC-9.5  | No ids or image tags in label values; no label named `env_id`, `instance`, `image` or `run_id`.                                    |
| AC-10.1 | With tracing off, the JSONL equals the golden file byte for byte.                                                                  |
| AC-10.2 | The JSONL is the same with tracing on and off.                                                                                     |
| AC-10.3 | One span per event, named `Event.Event`, from `ts - dur_ms` to `ts`, with the attribute and status rules in P-19.                  |
| AC-11.1 | With a nil `TracerProvider`, no span and no exporter.                                                                              |
| AC-11.2 | With a provider, one span per call named `<TraceServiceName>.<op>` with documented keys only.                                      |
| AC-15.1 | The `ate_env_fleet_warm_replicas` gauge equals the driver's warm pool after every step of any interleaving.                        |
| AC-15.2 | A recycle or `warm_pool` paused across teardown deletes its actor on resume; nothing from the run survives teardown.               |
| AC-15.3 | Overlapping `warm_pool(template, replicas=R)` calls create at most `R` warm actors for one pool key.                               |

## Requirements without a property

These are checked by examples or on the cluster (see each criterion), because they describe one
artifact or environment rather than a range of inputs:

| Requirement         | Why not a property                                                   |
| ------------------- | -------------------------------------------------------------------- |
| AC-1.1, AC-1.2      | One dependency list and one import; a static lint covers regressions |
| AC-5.1, AC-5.2      | Needs the cluster and a collector                                    |
| AC-6.1, AC-6.2      | Needs the cluster; Go unit tests check the family and label sets     |
| AC-7.6              | One mock run's phase set                                             |
| AC-8.4              | One HTTP endpoint                                                    |
| AC-10.1             | Byte-for-byte compatibility with a fixed output (golden file)        |
| AC-12, AC-13, AC-14 | Docs, the CI configuration and timings                               |

## Findings that changed the spec

**P-3, reserved `other`.** The Dafny model of `repo_family`
([verify/telemetry.dfy](./verify/telemetry.dfy)) without a rename step failed P-3 with the
counter-example of a repo literally named `other`. That repo got the overflow label, so the two
cases were indistinguishable on dashboards, and it used one of the 64 slots although the code
comment says reserved names do not count toward the cap. Reproduced against the code at the time:

```text
repo "acme/other", registry not full -> "other", registered, size 1
registry full, new repo              -> "other"
registry full, repo "acme/other"     -> "other"
```

Amendment: a repo whose family is a reserved name is labelled `<name>_repo`. `main` now escapes
every `other` or `unknown` followed by `_repo` repeats, which keeps the mapping one-to-one (AC-9,
P-2).

**P-8, negative durations.** The model verified P-8 only with the precondition `dur >= 0`, which no
document stated. Reproduced: `add_phase("acquire", 2.0); add_phase("acquire", -5.0)` gave
`count=2, total_s=-3.0, max_s=2.0`. Latent, because every caller uses `time.monotonic()`. Amendment:
`add_phase` and `task_done` clamp negative values to 0 (AC-7). `main` has the clamp.

**P-1, the bound.** The property first said `MAX_FAMILIES + 1`. A Hypothesis run found 65 distinct
repos plus one unparseable input give 66 values (64 families, `other`, `unknown`). The spec was
wrong, not the code.

**P-19, one span per event.** The property first described interleaved start and done pairs. The
code writes each event when its operation finishes and records one span per event, back-dated by
`dur_ms`. The spec was behind the code.

**P-10, recycle overlapping teardown leaks a VM.** `release(recycle=True)` removes the actor from
the owned actors, then awaits `suspend()`. If `reap()` runs during that await, it sees the actor in
neither the owned actors nor the pool, so it skips it. When `suspend()` returns, the actor goes back
into the pool. A scripted reproduction (not in the repository) that runs the real driver with a fake
client paused at its awaits printed, on `main` at `9b2148a` before #221:

```text
reaped=0  still live on Substrate=['sb-1']
driver pool=1  owned=['sb-1']  observer warm=0
```

The VM survives teardown, and the gauge says 0 while the pool holds 1 (P-9).

**P-11, overlapping `warm_pool` calls double-provision.** `warm_pool` computes
`needed = replicas - len(current)` before its awaits and does not count replicas other calls are
still creating. The same reproduction printed, before #221:

```text
pool size=6 (asked for 3)  live actors=6  observer warm=6
```

Fix for both, in #221: `reap` bumps a teardown counter (`_teardowns`) when it starts and ends, and a
recycle, `warm_pool` or failed warm claim that awaited across it deletes its actor instead of
pooling it. `warm_pool` reserves the actors it is creating in `_warm_inflight`, per pool key and
`_teardowns` value ([interfaces.md](./interfaces.md#warm-pool-accounting-ac-15)).

**Hypothesis finds the same bugs.** The stateful test finds both bugs on the code before #221
without being told where to look and shrinks each to the shortest operation sequence, shown under
P-10 and P-11. It also finds a third variant the model predicted: a `warm_pool` paused in `create()`
during teardown leaks its actors.

**Bugs A and B, in #221's first version.** An adversarial code review of #221's first version found
two more bugs. Neither Dafny nor Hypothesis found them first.

- A: reap left the reservation counter alone, so a `warm_pool` started after the teardown counted
  the old reservation and returned with an empty pool. Hypothesis missed it because the machine
  starts no `warm_pool` after a teardown. `warm_pool.dfy`'s invariant `closed ==> pending == 0` and
  `Reap`'s `pending := 0` already encode the fix: modelling the first version against it fails at
  that clause. But that model makes a teardown permanent, so it cannot express the symptom.
- B: a failed warm claim (pop, await `get()`, put back on error) put its actor back after a
  teardown. Hypothesis missed it because the fake `get()` never failed. `warm_pool.dfy` cannot
  express it: its `Acquire` is one step with no await or put-back.

`warm_pool_generations.dfy` was written after the review, to state both: `PlanFirstCovers` (A) and
`ClaimFailPutBackSafe` (B) fail, the fixed actions verify, and `BugAWitness` and `BugBWitness` check
concrete traces. #221 fixed both and added a plain test for each.

**Claim/reap gap, open.** Proving "claimed actors are live" in `warm_pool_generations.dfy` failed
(`ClaimOkReturnsLive`). A warm claim pops an actor and awaits `get()`; a reap that runs meanwhile
deletes it, since it is still owned. If `get()` returns before the delete lands, `_acquire_internal`
hands the caller a deleted environment. #221 re-checks `_teardowns` only on the failure path. No
property in this file covers it: P-10 holds (nothing survives the teardown); what breaks is that
`acquire` returns a live environment, which no property states. No test covers it.

**How the concurrency model works.** Each piece of code between two `await`s is one atomic action on
shared state (`Acquire`, `RecycleStart`, `RecycleFinish`, `WarmPlan`, `WarmCommit`, `Reap`). The
Dafny model proves that every action of the #221 design preserves the invariant, so it holds for
every interleaving of any number of concurrent calls and actors. `WarmPoolBefore`, the code before
#221, fails, and each failure is one of the interleavings above.

## Limits

- Dafny `real` is exact and Python `float` is not; P-8 holds for real numbers.
- The `repo_family` model is sequential. `_bounded_family` uses double-checked locking; its
  concurrency is not covered.
- The `repo_family` model covers parseable inputs only; the `unknown` label for unparseable input
  adds one value to the P-1 bound. It renames the bare reserved names only; `main` also escapes
  their `_repo` forms.
- The warm-pool models cover one pool key and one event loop (interleaving only at `await`s). Calls
  into the same driver from two threads or two loops are outside them.
- All models are hand-written and not tied to the code; see [verify/README.md](./verify/README.md).
