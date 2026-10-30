# Integrations

Adapters that let an agent or RL framework run its sandboxes as Agent Substrate
environments through `ate-env-api`. Each is a Python distribution of its own that
depends on the Python client, [`ate-env-client`](../clients/python/README.md),
and registers with the framework the way the framework expects, with no change
to the framework. They are supported ([docs/support.md](../docs/support.md)).

| Integration | Framework | Distribution / import | What it provides | Install |
|---|---|---|---|---|
| [`nemo-gym/`](nemo-gym/README.md) | [NeMo Gym](https://github.com/NVIDIA-NeMo/Gym) | `nemo-gym-substrate` / `nemo_gym_substrate` | Sandbox provider `substrate` (`SubstrateSandboxProvider`), registered through the `nemo_gym.sandbox_providers` entry point. `endpoint()` reaches a daemon inside the sandbox through the port proxy. | `pip install -e integrations/nemo-gym` |
| [`openhands/`](openhands/README.md) | [OpenHands](https://github.com/OpenHands/software-agent-sdk) | `ate-env-openhands` / `ate_env_openhands` | `AteEnvWorkspace`, an OpenHands `RemoteWorkspace` that mirrors `AgentSandboxWorkspace` and runs the agent-server in any unmodified task image (glibc 2.31 or newer); fleet adapters (`make_handle_workspace`, `make_fleet_workspace`); an R2E-Gym adapter; agent-sandbox-rl's fleet API on ate-env (`agent_sandbox_rl/`). | `pip install -e integrations/openhands` |
| [`uni-agent/`](uni-agent/README.md) | [uni-agent](https://github.com/verl-project/uni-agent) | `uni-agent-substrate` / `uni_agent_substrate` | Sandbox provider `ate_env` (one environment per episode) and the `r2e_gym` task that hides the tests, lets the agent work, then grades in the same environment; `datasets.py` builds task parquets with digest-pinned images for R2E-Gym, SWE-reBench and SWE-bench rows. | `pip install -e integrations/uni-agent` |

## What is in each directory

| Path | Contents |
|---|---|
| `nemo-gym/src/nemo_gym_substrate/` | `provider.py` (the provider), `_compat.py` (NeMo Gym base types with hermetic fallbacks). |
| `nemo-gym/layer/` | The NeMo runtime layer image: `Dockerfile`, hash-locked `requirements.txt` (`lock.sh`, `requirements.in`), `nemo_runtime/` with NeMo Skills' sandbox server and NeMo Gym's exec server, and the vendored `third_party/` sources. Mounted into any task image as a runtime layer ([docs/rootfs-images/RUNTIMES.md](../docs/rootfs-images/RUNTIMES.md)). |
| `nemo-gym/hack/` | `runtime-layer-demo.sh` (register a layered template and run it end to end), `bake-task-image.sh` (the older mode: bake the guest onto a base image). |
| `nemo-gym/benchmarks/` | `rollout_bench.py` (a mock NeMo-RL rollout benchmark on the provider, with a run report) and `driver_sim.py` (start-time and time-to-first-command simulator). |
| `nemo-gym/tests/` | Unit tests on a fake `ate-env-api` (`fakes.py`) and `test_e2e.py` against a live deployment (`SUBSTRATE_E2E_API_URL`). |
| `openhands/src/ate_env_openhands/` | `workspace.py` (`AteEnvWorkspace`), `_runtime.py` (one event loop and client per endpoint), `fleet.py` (fleet adapters), `r2egym.py` (R2E-Gym's `RepoEnv` on a fleet sandbox), `agent_sandbox_rl/` (agent-sandbox-rl's `SandboxFleet`, `SandboxHandle` and config on top of `ate_env`). |
| `openhands/layer/` | The OpenHands agent-server layer image: `Dockerfile`, hash-locked `requirements.txt` and `build-constraints.txt` (`lock.sh`). |
| `openhands/tests/` | Unit tests that start a real agent-server against a fake `ate-env-api` and port proxy (`fakes.py`), `test_agent_sandbox_rl.py` with a sample program, and `test_e2e.py` against a live deployment. |
| `uni-agent/src/uni_agent_substrate/` | `provider.py` (the provider), `tasks/r2e_gym/` (the task), `datasets.py` (task parquets), `_register.py` (registration with uni-agent's lazy registries). |
| `uni-agent/scripts/` | `resolve_images.sh`: resolve task-image tags to digest-pinned references the cluster can pull. |
| `uni-agent/tests/` | Unit tests on fakes (`fakes.py`) for the provider, the task, the reward and the datasets. |

## Tests in CI

The `Python client and integration tests` job runs the nemo-gym and uni-agent tests
(uni-agent is installed at a pinned commit without its verl submodule); the
`OpenHands integration` job runs the openhands tests, because `openhands-sdk` pulls
in a large dependency set. See [`.github/workflows/ci.yaml`](../.github/workflows/ci.yaml).

## Used by

- `nemo-gym`: the [NeMo Gym rollout benchmark](nemo-gym/benchmarks).
- `openhands`: [`examples/openhands`](../examples/openhands/README.md).
- `uni-agent`: [`examples/Qwen3.5-35B-A3B-verl`](../examples/Qwen3.5-35B-A3B-verl/README.md).
