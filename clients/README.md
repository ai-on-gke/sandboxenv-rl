# Clients

Two clients of `ate-env-api`, both supported ([docs/support.md](../docs/support.md)).
Both speak the `ateenv.v1alpha` API in [`proto/ateenv/v1alpha`](../proto/ateenv/v1alpha)
over gRPC and send the caller token when one is configured.

| Client | Language | Package | What it covers |
|---|---|---|---|
| [`go/`](go) | Go | `github.com/agent-substrate/env/clients/go`, package `env` | `Client`, `Env` and `Process`: environment lifecycle, shell and process streams, file transfer, `ServerInfo`; OpenTelemetry spans behind `tracing.go`. The CLI and the SWE-bench loader are built on it. |
| [`python/`](python/README.md) | Python 3.10+ | distribution `ate-env-client`, import `ate_env` | The async client (`Client`, `Env`, `Process`) and the fleet SDK (`SandboxFleet`, `AsyncSandboxFleet`, `SandboxHandle`) for RL jobs: acquire sandboxes by task image, run strategies, warm pools, park and wake, run reports, metrics and tracing. The integrations are built on it. |

## Go client files

| File | Contents |
|---|---|
| `client.go` | `Client` and `ClientOptions` (endpoint, bearer token, a caller-supplied connection): `Create`, `Get`, `Suspend`, `Delete`, `ServerInfo`, `Env`; the error mapping (`ErrNotFound`, `ErrProcessExited`). |
| `env.go` | `Env`: `Shell` and `Run`, `ReadFile`, `WriteFile` and `WriteFileAt`, `Suspend`, `Delete`. |
| `process.go` | `Env.StartProcess`, `Env.Process`, `Env.FindProcess`, and `Process`: `Output` with offsets, `Stdin`, `Signal`, `Resize`, `Kill`, `Wait`. |
| `types.go` | `ShellRequest` and `ShellResponse` (exit codes: 128 plus the signal number for a signalled process). |
| `tracing.go` | Span names and attributes of the OpenTelemetry instrumentation. |

## Python client layout

| Path | Contents |
|---|---|
| [`python/README.md`](python/README.md) | Install, connect, the fleet SDK, pool sizing, errors, observability. |
| [`python/USER_GUIDE.md`](python/USER_GUIDE.md) | RL rollouts and benchmark adapters: overlapping claims with policy decoding on VeRL and Ray, grading with `ate_env.adapters`. |
| `python/src/ate_env/client.py`, `env.py`, `types.py`, `errors.py` | The async client: `Client`, `Env`, `Process`, the public dataclasses and enums, the exception hierarchy (`exceptions.py` is the deprecated alias). |
| `python/src/ate_env/fleet.py`, `async_fleet.py`, `handle.py`, `strategies.py`, `sizing.py`, `lookahead.py`, `config.py` | The fleet SDK: `SandboxFleet` and `AsyncSandboxFleet`, `SandboxHandle`, the `none`, `naive`, `sliding` and `pipelined` strategies, warm-pool sizing, `LookaheadWarmer` for a trainer's batch stream, `FleetConfig`. |
| `python/src/ate_env/backend/` | Fleet drivers: `SubstrateBackendDriver` (the real one, through `ate-env-api`) and `MockBackendDriver` for tests. |
| `python/src/ate_env/runtime/` | Guest hooks a fleet runs commands through: `SubstrateEnvClientRuntime`, `MockRuntimeHook`. |
| `python/src/ate_env/adapters/` | Benchmark task adapters: `SweBenchAdapter` converts a SWE-bench row to a task and grades a patch in the sandbox. |
| `python/src/ate_env/observability.py`, `telemetry/` | The run report (`Observer`, `RunReport`), Prometheus series behind the `metrics` extra, OpenTelemetry tracing behind the `tracing` extra, and a JSONL trace recorder ([docs/observability.md](../docs/observability.md)). |
| `python/src/ate_env/loop_runner.py` | `LoopRunner`: the one background event loop that synchronous callers dispatch coroutines through. |
| `python/src/ate_env/_gen/` | Generated protobuf and gRPC stubs. Never hand-edit; regenerate with `make python-protos` ([`_gen/README.md`](python/src/ate_env/_gen/README.md)). |
| `python/examples/quickstart.py` | The client end to end against a running `ate-env-api`. |
| `python/scripts/gen-protos.sh` | What `make python-protos` runs. |
| `python/tests/` | The unit tests (fakes of the API in `fakes.py`) and `tests/e2e/`: `test_guest_daemon.py` against a standalone guest and `test_full_stack.py` against a live `ate-env-api`. |

Install the Python client with the extras you need:

```bash
pip install -e 'clients/python[dev]'                    # the tests
pip install -e 'clients/python[tracing,metrics]'        # OpenTelemetry and Prometheus
```
