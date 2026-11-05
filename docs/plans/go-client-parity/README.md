# Plan: Go client parity with the Python client

Status: proposal, 2026-11-04, written against `main` at `8e07189f` with the open pull requests #209,
#230, #235 and #242 read. The evidence is in [parity-matrix.md](./parity-matrix.md).

## 1. Goal

`clients/go` and `clients/python` are both supported clients of `ate-env-api`
([docs/support.md](../../support.md)). Most client work since the repository started went into
Python: a fleet SDK, a typed error contract, metrics, a README and about 9,000 lines of tests. The
Go client kept RPC coverage (each new `EnvironmentService` RPC lands in both clients in the same
pull request, as #209 and #235 do) and tracing, and stopped there. Its main consumer, the SWE-bench
loader, carries client logic of its own: a connection pool and a polled command runner, with a
comment that the runner belongs in the SDK.

This plan brings the Go client to the same capabilities, the same wire behavior and the same
telemetry names as the Python client, in Go idiom, for everything a Go consumer can use. It does not
make the two APIs look alike, and it does not port the fleet SDK by default (section 8).

## 2. The gap in one screen

- **Packaging.** `go get` of the client pulls 404 packages, among them Substrate's API module,
  through one constant imported from `internal/ate`.
- **Errors.** The `ate-env-start-failure` trailer, which the server has sent since #222 and the
  Python client turns into a retryable `SandboxStartError` with a reason, is dropped. Go callers see
  a bare FAILED_PRECONDITION or RESOURCE_EXHAUSTED.
- **Long commands.** `Env.Run` holds one stream open for the whole command, so the router's route
  timeout cuts long test runs. The loader polls instead and asks for that option in the SDK. There
  is no `TimedOut` and no `Duration` on the result.
- **Connections.** The loader round-robins N clients; the client has one connection.
- **Observability.** No metrics. Spans miss `start_process` and `ate_env.first_command`.
- **Docs and tests.** No README, no runnable example, no e2e against a daemon or a cluster; the
  cross-client attribute tables are mirrored by hand.
- **Fleet.** Nothing in `clients/go`; the loader's `WorkingSet` is the only Go pool.

Where Go is ahead: PTY and resize (the Python client exposes neither), and a unit fixture that runs
the real `apiservice` handler and guest services against fake Substrate.

## 3. Scope: three tiers

| Tier | Content                                                        | Recommendation                                                      |
| ---- | -------------------------------------------------------------- | ------------------------------------------------------------------- |
| 1    | the client: packaging, errors, run modes, connections (PR 1-5) | do now                                                              |
| 2    | observability, parity vectors, docs, e2e (PR 6-9)              | do now                                                              |
| 3    | a fleet SDK in Go (section 8)                                  | decide; the recommendation is to defer until a Go consumer needs it |

Tier 1 and 2 are nine small pull requests, each one concern, in the order of section 5. Tier 3 is a
separate decision because it is five to six pull requests on its own, the Python fleet is still
moving (#242, #230, and issue #123 items 3, 8, 10, 13, 22, 23 and 26 are open), and the only Go
consumer today has a pool shaped for benchmarks (fork, step barriers, park policies) that the fleet
does not model.

## 4. Design decisions

- **D1. Same wire behavior, Go idiom.** Parity means the same capabilities, the same bytes on the
  wire, the same span, attribute, metric and trailer names, and the same error mapping rules. It
  does not mean the same method names or types: Go keeps proto request types, `context`,
  `io.Reader`, `errors.Is`, `errors.As` and `status.Code`.
- **D2. The client owns what Python keeps in the fleet when Go has no fleet.** The start error, the
  retryable classification, `TimedOut`, `Duration`, polled execution and the connection pool go into
  `clients/go` as options on `ClientOptions`, `ShellRequest` and `ShellResponse`. The loader then
  uses them and deletes its copies (`EnvBackend.pick`, `runPolledProcess`).
- **D3. `clients/go` imports nothing under `internal/`.** New architecture invariant in `AGENTS.md`,
  checked by a verify script in the Go CI job. The proto package is the only in-repository import.
- **D4. Shared conformance vectors.** The tables both clients must agree on (image reference
  attribute, executable attribute, `rpc.*` attributes per semconv mode, error mapping with the
  start-failure trailer, metric families and labels, RPC coverage) live once, as JSON under
  `proto/testdata/client-parity/`, next to the contract they describe, and both test suites read
  them. This closes AC-17 and part of AC-18 of the telemetry plan and replaces the hand-mirrored
  tables.
- **D5. `timed_out` comes from the guest.** The guest knows when its timer killed the process (it
  already labels `ate_env_guest_process_duration_seconds{exit_class="timeout"}`); the clients guess
  from exit 137 and elapsed time. PR 4 adds `Process.timed_out` to `guest.proto`, sets it in the
  guest, and makes both clients prefer it. Until PR 4 merges, PR 3 uses the Python heuristic.
  Decision needed: the field, or the heuristic alone (section 9).
- **D6. Span names.** Go keeps `exec` for `Run` and adds a `start_process` child, as the Python
  fleet handle and [docs/observability.md](../../observability.md) do. The Python direct path's
  `shell` name is a Python follow-up.
- **D7. Metrics are opt-in through a `prometheus.Registerer`.** Nothing registers on the default
  registry unless the caller passes it. The families, labels and buckets are the Python ones, with
  `source="direct"`. Registration is idempotent per registry, as `telemetry/metrics.py` does it.
- **D8. Out of this plan.** The Go module rename to the `ai-on-gke` path (item 7 of the hardening
  work), TLS to `ate-env-api`, a port-proxy client, the MCP surface and the benchmark trace-file
  mode.

## 5. Pull request sequence

| #   | Branch                   | Scope                                                                                                                                                                                                                                           | Depends on | Size |
| --- | ------------------------ | ----------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- | ---------- | ---- |
| 1   | `goclient/deps`          | `env.DefaultAtespace`; drop the `internal/ate` import; `hack/verify/go-client-deps.sh` in the Go CI job; the invariant in `AGENTS.md` and the rule in `docs/dev/code-layout.md`                                                                 | none       | S    |
| 2   | `goclient/errors`        | `StartError{Reason, Code}` from the `ate-env-start-failure` trailer on unary, server-stream and client-stream calls; `Retryable(err) bool` with the Python code table; a test that `status.Code` reads through the wrap; `errors.go`            | none       | M    |
| 3   | `goclient/run-poll`      | `ShellRequest.Poll` (start, poll `GetProcess` with backoff, read the output once, kill on abandon) lifted from the loader with its tests; `ShellResponse.Duration` and `TimedOut` (heuristic until PR 4); the loader's `envSandbox.Run` uses it | none       | M    |
| 4   | `guest/timed-out`        | `Process.timed_out` in `guest.proto`, set by `guest/process`; `buf generate`, `make python-protos`; Go `TimedOut` and Python `ProcessInfo.timed_out`, `ExecResult.timed_out` prefer the field; CHANGELOG                                        | 3          | M    |
| 5   | `goclient/connections`   | `ClientOptions.Connections`: N connections, round robin on `Create` and `Env`, an `Env` keeps its connection; `Close` closes all; rejected with `GRPCConn`; the loader's `EnvBackend` keeps one client                                          | 1          | S    |
| 6   | `goclient/metrics`       | `ClientOptions.MetricsRegisterer`; `ate_env_client_exec_seconds`, `_suspend_seconds`, `_ready_to_first_command_seconds`; the `start_process` child span; `ate_env.first_command` and `ate_env.timed_out`; the Go row of `docs/observability.md` | 2, 3       | M    |
| 7   | `clients/parity-vectors` | `proto/testdata/client-parity/*.json`; `clients/go/parity_test.go` and `clients/python/tests/test_parity_vectors.py`; delete the mirrored tables; step 5 and 6 of `.agents/skills/extend-env-surface/SKILL.md` name the vectors                 | 2, 6       | M    |
| 8   | `goclient/readme`        | `clients/go/README.md` with the Python README's sections for the shared operations; `example_test.go` compiled by `make test`; the rows in `clients/README.md` and `docs/observability.md`; CHANGELOG                                           | 2, 3, 5, 6 | M    |
| 9   | `goclient/e2e`           | `clients/go/e2e_test.go` gated by `ATE_ENV_GUEST_TARGET` and `ATE_ENV_API_TARGET`, the case list of `tests/e2e/test_guest_daemon.py` and `test_full_stack.py`; `hack/ci/e2e.py` runs it after the Python full-stack tests                       | 3          | S    |

PR 8 lands after what it documents, because the READMEs describe merged code only. PR 2, 3 and 1 can
run in parallel. PR 4 is the one proto change and can be dropped if D5 goes the other way; PR 3 then
keeps the heuristic and PR 6 records `ate_env.timed_out` from it.

### PR 1: packaging

- `clients/go/client.go`: `const DefaultAtespace = "ate-env"`; `Env()` uses it. `internal/ate` keeps
  its own constant.
- `hack/verify/go-client-deps.sh`: fails when `go list -deps ./clients/go` names a package under
  `github.com/agent-substrate/env/internal/`, `go.ate.dev/` or `k8s.io/`.
- `AGENTS.md`, "Architecture invariants": `clients/go` imports only `proto/ateenv/v1alpha` from this
  repository, because an external module that imports the client must not pull the Substrate
  control-plane module (enforced by the verify script).

### PR 2: errors

- `clients/go/errors.go`: `type StartError struct { Reason string; Code codes.Code; msg string }`
  with `Error`, `Unwrap` to the status error, and the constants `StartFailureCrashed`,
  `StartFailureNoCapacity`, `StartFailureKey` (the same strings as
  `internal/apiservice/wakeretry.go:69-78` and `errors.py:79-84`; the client does not import them).
- Every guest call captures trailers: `grpc.Trailer(&md)` on unary calls, `stream.Trailer()` after
  the error on streams, in `env.go` and `process.go`. `fromGRPCError(err, md)` returns `StartError`
  when the key is present, whatever the code, before the `ErrNotFound` and `ErrProcessExited` rules.
- `Retryable(err) bool`: true for `StartError`, UNAVAILABLE, DEADLINE_EXCEEDED, INTERNAL, ABORTED
  and NOT_FOUND on an environment; false for INVALID_ARGUMENT, PERMISSION_DENIED and the rest. The
  table is the one in `runtime/substrate_env_client.py:245-262` and becomes a parity vector in PR 7.
- Tests in `client_test.go` with the fixture: a crashed actor and a no-capacity router answer, on
  `StartProcess`, `ReadFile` and `WriteFile`; `status.Code(err)` after the wrap.

### PR 3: run modes

- `ShellRequest.Poll bool`: when set, `Run` starts the process, polls `FindProcess` from 50 ms
  doubling to 2 s, tolerates 10 consecutive UNAVAILABLE or DEADLINE_EXCEEDED answers, reads the
  output in one non-following stream after exit, and kills the process when `ctx` ends first. The
  bounds and the retry count are the loader's; expose `PollInterval` only if a caller needs it.
- `ShellResponse.Duration time.Duration` (start to exit, measured by the client) and `TimedOut bool`
  (PR 3: exit 137 and `Duration >= Timeout`, as Python; PR 4: the guest's field).
- `internal/benchmarking/swebench/backend.go`: `envSandbox.Run` calls `Run` with `Poll: true`;
  `runPolledProcess` and its test cases move into the client.

### PR 4: `timed_out` from the guest

- `proto/ateenv/v1alpha/guest.proto`: `bool timed_out = 8;` on `Process`, "true when the guest
  killed the process because `StartProcessRequest.timeout` elapsed". `guest/process` sets it from
  the tracker's `timedOut` flag (`guest/process/tracker.go:176`), which already drives
  `exit_class="timeout"`. `ate-env-api` forwards the message unchanged.
- Go: `ShellResponse.TimedOut` reads the field; `Process.Wait` and `FindProcess` return it in the
  proto. Python: `ProcessInfo.timed_out`; `ExecResult.timed_out` prefers it and keeps the heuristic
  for a guest that predates the field.
- Follow `.agents/skills/extend-env-surface/SKILL.md`. No server flag; the field is zero on an older
  guest, and `GetServerInfo` cannot speak for the guest.

### PR 5: connections

- `ClientOptions.Connections int` (0 and 1 mean one). The client dials N connections with the same
  options; `Create` and `Env` pick the next one round robin and the `Env` keeps it for every call,
  so one environment's streams stay on one connection, as the loader does today. Lifecycle calls on
  `Client` round robin. `Connections > 1` with `GRPCConn` is an error from `NewClient`.
- The loader's `EnvBackend` becomes one `env.Client` with `Connections`; `pick` goes away.

### PR 6: metrics and the span deltas

- `ClientOptions.MetricsRegisterer prometheus.Registerer`. Families and labels:
  `ate_env_client_exec_seconds{status}`, `ate_env_client_suspend_seconds{status}`,
  `ate_env_client_ready_to_first_command_seconds{status,source}` with `source="direct"`; the buckets
  of `internal/telemetry` and `telemetry/metrics.py` (`0.05` to `600`). `status` is `ok`, `error` or
  `cancelled` (`ctx` done), as `metrics.status_of`.
- `start_process` child span under `exec`; `ate_env.first_command=true` on the first `Run` or
  `StartProcess` of an `Env`; `ate_env.timed_out` on `exec`.
- `docs/observability.md`: the Go row of the enabling table and the trainer families list.

### PR 7: parity vectors

- `proto/testdata/client-parity/`: `image_ref.json`, `executable.json`, `rpc_attrs.json` (one block
  per semconv mode), `error_mapping.json` (status code, message, trailer, expected class or
  sentinel, retryable), `metric_families.json` (name, labels, buckets), `rpc_coverage.json` (each
  `EnvironmentService` RPC and the Go and Python method that calls it).
- `clients/go/parity_test.go` and `clients/python/tests/test_parity_vectors.py` read them; the
  Python test checks the method names by reflection. `attrs_test.go`, `executable_test.go` and the
  mirrored Python tables go.

### PR 8: README and example

- `clients/go/README.md`: install (`go get`), connect, create, shell and `Run` options, processes,
  stdin and signals, PTY, files, suspend and delete, readiness (wake on first call, `StartError`,
  `Retryable`), errors (`errors.Is`, `errors.As`, `status.Code`), connections, tracing and metrics,
  the local guest daemon. Same section order as `clients/python/README.md` so a reader can switch.
- `clients/go/example_test.go`: `Example*` functions that compile under `make test` and show the
  README's snippets.

### PR 9: e2e

- `clients/go/e2e_test.go`: skipped without the variables; against the guest daemon the 24 cases of
  `test_guest_daemon.py`, against a cluster the 5 client cases of `test_full_stack.py`, same names.
- `hack/ci/e2e.py` runs `go test ./clients/go -run TestE2E` with `ATE_ENV_API_TARGET` set, next to
  the Python full-stack tests and the loader smoke test.

## 6. Acceptance criteria

| ID    | PR  | Criterion and command                                                                                                                                                                                                                                                                                           |
| ----- | --- | --------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| AC-G1 | 1   | `go list -deps ./clients/go \| grep -E '/internal/\|go\.ate\.dev\|k8s\.io'` prints nothing; `hack/verify/go-client-deps.sh` runs in the Go CI job and fails on a violation                                                                                                                                      |
| AC-G2 | 2   | With the fixture, a guest call on a CRASHED actor returns an error where `errors.As(err, &se)` gives `se.Reason == "crashed"`; with `-wake-no-capacity-grace`, `"no-capacity"`; on `StartProcess`, `ReadFile` and `WriteFile`. `status.Code(err)` returns the code; `errors.Is(err, ErrNotFound)` is unchanged  |
| AC-G3 | 3   | `Run` with `Poll` never holds a stream while the process runs (the fixture router gains a count of open guest streams); the loader's `runPolledProcess` cases pass against the client; `Duration` is set; cancelling `ctx` kills the process                                                                    |
| AC-G4 | 4   | `sh -c 'sleep 10'` with a 1 s timeout gives `TimedOut=true, ExitCode=137`; `sh -c 'kill -9 $$'` gives `TimedOut=false, ExitCode=137`; the Python e2e asserts the same two on `ExecResult.timed_out`                                                                                                             |
| AC-G5 | 5   | `Connections: 3` opens three TCP connections to the fixture; every call of one `Env` arrives on the same one; `Close` closes all; `Connections: 2` with `GRPCConn` fails in `NewClient`                                                                                                                         |
| AC-G6 | 6   | With a `prometheus.NewRegistry()`, one `Run` and one `Suspend` produce the three families with the documented labels and buckets, `ready_to_first_command` once per `Env`; without the option the registry stays empty; `start_process` nests under `exec`; `ate_env.first_command` is on the first `exec` only |
| AC-G7 | 7   | Both suites read `proto/testdata/client-parity/*.json`; changing one expected value fails a Go test and a Python test; `rpc_coverage.json` lists every RPC of `env.proto` and CI fails when one has no method in either client                                                                                  |
| AC-G8 | 8   | `clients/go/README.md` has one section per operation both clients have; `go vet ./clients/go` compiles `example_test.go`; `make verify-docs` passes; `clients/README.md` and `docs/observability.md` describe the merged state                                                                                  |
| AC-G9 | 9   | `ATE_ENV_GUEST_TARGET=127.0.0.1:8090 go test ./clients/go -run TestE2E` passes the 24 cases; `hack/ci/e2e.py` runs the cluster cases in the `e2e-kind` job                                                                                                                                                      |

## 7. Keeping parity after this plan

- `.agents/skills/extend-env-surface/SKILL.md`, step 5 and 6: a new RPC or option adds its row to
  `rpc_coverage.json` and, when it has a mapped error or a span attribute, to the other vectors. CI
  then fails until both clients have it (AC-G7).
- `CHANGELOG.md`: a user-visible client change names its client, "Go client:" or "Python client:",
  and a change to a shared contract (a span, an attribute, a metric, a trailer) names both.
- A feature that one consumer needs in Go goes into `clients/go` when a second consumer could use
  it, as `docs/dev/code-layout.md` already says; the loader is not the place for client logic.

## 8. Tier 3: a fleet SDK in Go

Not in the pull request sequence above. If the decision is to build it, this is the shape:

- `clients/go/fleet`: `Config` (the `FleetConfig` fields that apply to Go), a `Driver` interface
  with the methods of `backend/base.py` (`Preflight`, `EnsureTemplate`, `WarmPool`, `ResizePool`,
  `PoolSize`, `Acquire`, `Release` with recycle, `Reap`), a Substrate driver on `env.Client` and a
  mock; `Fleet` with `Setup`, `WarmImages`, `SetPoolReplicas`, `Acquire`, `AcquireBatch`, `Release`,
  `Recycle`, `Teardown`, `Run` with the four strategies; `Handle` with `Exec` returning
  `ExecResult{ExitCode, Stdout, Stderr, Duration, TimedOut}`; `sizing` as a pure port of
  `sizing.py`, vector-tested; `Lookahead`; `RunReport`, the `ate_env_fleet_*` families and the
  `ate_env_fleet.*` spans.
- Source material: the loader's `WorkingSet` for the parked pool and prefetch; the Dafny models in
  `docs/plans/telemetry/verify/` and `test_warm_pool_accounting.py` for the accounting rules, which
  are language neutral and become Go property tests.
- Five to six pull requests: driver and handle; warm pool and accounting; strategies and sizing;
  report, metrics and spans; lookahead; the loader as a consumer.
- Trigger: a Go consumer other than the loader (a Go trainer or an integration), or a decision to
  delete `WorkingSet` in favor of the fleet.

## 9. Open questions

1. Tier 3 now or on the trigger above. The recommendation is the trigger.
2. D5: the `timed_out` proto field (PR 4) or the heuristic alone. The recommendation is the field;
   it also removes a documented guess from the Python client.
3. The Python direct path's `shell` span name (D6): rename to `exec`, alias, or leave.
4. Whether `Run` should default to `Poll` when `Timeout` exceeds the router's route timeout. The
   plan keeps streaming as the default and lets the caller opt in, as the loader does.

## 10. Risks

| Risk                                                                                         | Mitigation                                                                                                                                                            |
| -------------------------------------------------------------------------------------------- | --------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| A call path misses the trailer capture and drops the start reason silently                   | AC-G2 covers one call of each shape; the error-mapping vector in PR 7 is run through the fixture for every guest method                                               |
| Two `Client`s on one registry register the same family twice                                 | idempotent registration per registry (D7), tested with two clients                                                                                                    |
| `Poll` changes `Run`'s output semantics (output read once after exit)                        | `Run` buffers in both modes already; the README says `Poll` is for commands longer than the route timeout and that `Output` streams live output                       |
| The proto field crosses an older `ate-env-api`                                               | protobuf-go keeps unknown fields through the proxy's unmarshal and marshal; verify once in the fixture with a stale `Process` type before relying on it               |
| The module rename changes every import path after PR 8 documents one                         | PR 8 writes the import path in one place per file and the rename pull request updates it                                                                              |
| The nine pull requests spread over weeks while #209, #230 and #235 keep touching `client.go` | PR 1, 2 and 3 touch `errors.go`, `env.go`, `process.go` and the test files; conflicts with the RPC additions are confined to the `op*` constant block in `tracing.go` |
