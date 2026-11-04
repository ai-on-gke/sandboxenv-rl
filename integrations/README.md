# Integrations

Adapters that let an agent or RL framework run its sandboxes as Agent Substrate
environments through `ate-env-api`. Each is a Python distribution of its own that
depends on the Python client, [`ate-env-client`](../clients/python/README.md),
and registers with the framework the way the framework expects, with no change
to the framework. They are supported ([docs/support.md](../docs/support.md)).

| Integration | Framework | Distribution / import | What it provides | Install |
|---|---|---|---|---|
| [`harbor/`](./harbor/README.md) | [Harbor](https://github.com/harbor-framework/harbor) | `ate-env-harbor` / `ate_env_harbor` | `AteEnvEnvironment`, a Harbor environment provider (`harbor run ... -e ate_env_harbor:AteEnvEnvironment`) that runs each trial in an environment from a digest-pinned or built task image, with optional runtime tool layers on `PATH`. | `pip install -e integrations/harbor` |
| [`nemo-gym/`](./nemo-gym/README.md) | [NeMo Gym](https://github.com/NVIDIA-NeMo/Gym) | `nemo-gym-substrate` / `nemo_gym_substrate` | Sandbox provider `substrate` (`SubstrateSandboxProvider`), registered through the `nemo_gym.sandbox_providers` entry point. `endpoint()` reaches a daemon inside the sandbox through the port proxy. | `pip install -e integrations/nemo-gym` |
| [`openhands/`](./openhands/README.md) | [OpenHands](https://github.com/OpenHands/software-agent-sdk) | `ate-env-openhands` / `ate_env_openhands` | `AteEnvWorkspace`, an OpenHands `RemoteWorkspace` that mirrors `AgentSandboxWorkspace` and runs the agent-server in any unmodified task image (glibc 2.31 or newer); fleet adapters (`make_handle_workspace`, `make_fleet_workspace`); an R2E-Gym adapter; agent-sandbox-rl's fleet API on ate-env (`agent_sandbox_rl/`). | `pip install -e integrations/openhands` |
| [`uni-agent/`](./uni-agent/README.md) | [uni-agent](https://github.com/verl-project/uni-agent) | `uni-agent-substrate` / `uni_agent_substrate` | Sandbox provider `ate_env` (one environment per episode) and the `r2e_gym` task that hides the tests, lets the agent work, then grades in the same environment; dataset and image tooling for R2E-Gym, SWE-reBench and SWE-bench rows. | `pip install -e integrations/uni-agent` |

Each integration's `README.md` covers its configuration, runtime layers (`nemo-gym/layer`
and `openhands/layer`; see [docs/rootfs-images/RUNTIMES.md](../docs/rootfs-images/RUNTIMES.md)),
template helper scripts, unit fakes, and live `tests/test_e2e.py` targets. See
[docs/dev/code-layout.md](../docs/dev/code-layout.md) for repository placement rules.

## Tests in CI

The `Python client and integration tests` job runs the `nemo-gym` and `uni-agent` tests
(`uni-agent` is installed at a pinned commit without its `verl` submodule). The
`OpenHands integration` and `Harbor integration` jobs run in dedicated `devShell`
environments because `openhands-sdk` and `harbor` pull large dependency sets (and Harbor's
trial test starts containers through Docker). See
[`.github/workflows/ci.yaml`](../.github/workflows/ci.yaml).

## Used by

- `harbor`: [`examples/harbor`](../examples/harbor/README.md) and [`examples/harbor-tools-layer`](../examples/harbor-tools-layer/README.md).
- `nemo-gym`: the [NeMo Gym rollout benchmark](./nemo-gym/benchmarks).
- `openhands`: [`examples/openhands`](../examples/openhands/README.md).
- `uni-agent`: [`examples/Qwen3.5-35B-A3B-verl`](../examples/Qwen3.5-35B-A3B-verl/README.md).
