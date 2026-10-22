# OpenHands on ate-env: `ate-env-openhands`

Runs the [OpenHands](https://github.com/OpenHands/software-agent-sdk) agent-server inside any
unmodified task image (glibc 2.31 or newer) and connects the OpenHands SDK to it.
`AteEnvWorkspace` is an OpenHands `RemoteWorkspace` that mirrors OpenHands' own
`AgentSandboxWorkspace` (`openhands-workspace[agent-sandbox]`, kubernetes-sigs/agent-sandbox):
code written against that workspace moves to Agent Substrate by changing the constructor.

```python
from ate_env_openhands import AteEnvWorkspace

with AteEnvWorkspace(
    template="oh-agent-server",                      # base template with the agent-server layer
    image="docker.io/swebench/sweb.eval.x86_64.django_1776_django-11099@sha256:…",
    api_key=SESSION_KEY,                             # the template's OH_SESSION_API_KEYS_0
    working_dir="/testbed",
) as workspace:
    print(workspace.execute_command("git status").stdout)
    conversation = Conversation(agent=agent, workspace=workspace)   # RemoteConversation
```

How it fits together:

- The agent-server comes from a runtime layer mounted read-only at `/opt/oh`
  ([docs/rootfs-images/RUNTIMES.md](../../docs/rootfs-images/RUNTIMES.md)). The task image is not
  rebuilt; agent-sandbox, by contrast, runs the stock agent-server image.
- The guest starts the agent-server as a sidecar and holds the actor's readiness until its
  `/health` answers.
- The workspace reaches it through ate-env-api's port proxy, at
  `/v1alpha/atespaces/<atespace>/envs/<env>/ports/<port>/...`, with the agent-server's own session
  key, and with `api_token` in `X-Env-Authorization` when ate-env-api requires its bearer token.
  The SDK uses that URL as its workspace host, so commands, files and `RemoteConversation`
  (the agent loop and its event WebSocket) work unchanged. No `kubectl port-forward`.

| path | what it is |
|---|---|
| [`src/ate_env_openhands/`](src/ate_env_openhands) | `AteEnvWorkspace`, `port_url` |
| [`layer/`](layer) | the runtime layer: Dockerfile, hash-locked dependencies, `lock.sh` |
| [`../../examples/openhands`](../../examples/openhands) | the examples: OpenHands' agent-sandbox example 17, CodeAct, trajectory execution, a plain-HTTP demo |
| [`tests/`](tests) | unit and integration tests: a fake ate-env-api and port proxy in front of a real agent-server |

## 1. Install

```bash
pip install ./clients/python ./integrations/openhands            # the workspace
pip install './integrations/openhands[examples]'                 # + openhands-tools, for the agent examples
```

`ate-env-client` (this repo's Python client) is not on PyPI yet, thus install it from the checkout.
The package pins `openhands-sdk` to the agent-server version in the layer (see
[Versions](#versions)). Python 3.12 or newer, as openhands-sdk requires.

## 2. Build the layer

```bash
docker buildx build --platform linux/amd64,linux/arm64 \
  -t <registry>/oh-agent-server-layer:1.55.0 --push integrations/openhands/layer
crane digest --platform linux/amd64 <registry>/oh-agent-server-layer:1.55.0
```

The layer does not reuse the binary in the published
`ghcr.io/openhands/agent-server` images: that is a PyInstaller build made on Debian bookworm, it needs
glibc 2.36, and on older distributions it fails at start, for example on Ubuntu 22.04 (glibc 2.35):

```
ImportError: /lib/x86_64-linux-gnu/libc.so.6: version `GLIBC_2.36' not found (required by /tmp/_MEI…/libstdc++.so.6)
```

Starting it through its own image's loader does not help either, because PyInstaller re-executes
itself. The layer is [python-build-standalone](https://github.com/astral-sh/python-build-standalone)
CPython (built for glibc 2.17, relocatable) with `openhands-agent-server` and its dependencies
installed by uv for the target platform `manylinux_2_31`, so every wheel runs on glibc 2.31 and newer.
From the image it is mounted into it uses the kernel, the C library and `/bin/sh`. The C library
must be glibc 2.31 or newer: Debian 11, Ubuntu 20.04, RHEL 9 and later work; Alpine (musl), Ubuntu
18.04 (glibc 2.27) and CentOS 7 (glibc 2.17) do not. The layer's bytecode is precompiled
(`unchecked-hash`), because Python cannot write a cache on the read-only mount; the agent-server
answers `/health` in about 3.5 s instead of 6.5 s (arm64, local Docker). The layer is about 250 MB
compressed and 900 MB on disk.

For benchmarks: the layer goes through atelet's image cache like any image. A node pulls it once and
shares it among all task images on the base. The cache evicts images that no live actor mounts, thus on
a run that churns many task images the layer can be evicted and pulled again on the same node. A slow
first command then comes from the layer pull, not from the task image.

The dependency set is hash-locked: [`layer/requirements.txt`](layer/requirements.txt) (the agent-server and
everything it pulls in, the same for linux/amd64 and linux/arm64) and
[`layer/build-constraints.txt`](layer/build-constraints.txt) (setuptools, to build the one source-only
dependency, func-timeout). Base images are pinned by digest in the layer's Dockerfile. To move to another
agent-server release, change the versions in [`layer/requirements.in`](layer/requirements.in) (the agent-server,
`openhands-sdk` and `openhands-tools`, which the agent-server does not pin itself), run
[`layer/lock.sh`](layer/lock.sh) (needs uv; it re-resolves for both architectures and fails if they differ),
and build with `--build-arg AGENT_SERVER_VERSION=<version>`; the build checks that the installed
agent-server, sdk and tools are that version. Change the client's `openhands-sdk` to the same version (see
[Versions](#versions)).

## 3. Register the base template

```bash
ate-env manifest template --template oh-agent-server \
  --guest-image    <registry>/ate-env-guest@sha256:<digest> \
  --layer          oh=<registry>/oh-agent-server-layer@sha256:<digest>=/opt/oh \
  --sidecar        '/opt/oh/python/bin/python3 -m openhands.agent_server --host 0.0.0.0 --port 8000' \
  --sidecar-readyz http://127.0.0.1:8000/health \
  --env            OH_SESSION_API_KEYS_0=<key> \
  --snapshots-bucket <object-storage-url> | kubectl-ate create actor-template -f -
```

This is the counterpart of agent-sandbox's SandboxTemplate and warm pool. Further agent-server
settings are more `--env` flags. The first environment created from an image derives a template
for it (layer, sidecar and settings included); later ones reuse it.

## 4. Use `AteEnvWorkspace`

`AteEnvWorkspace(template=..., image=...)` creates an environment, waits until the agent-server
answers `/health` through the proxy, checks the session key, creates `working_dir`, and deletes the
environment on exit. `AteEnvWorkspace(env_id=...)` attaches to an existing environment and leaves
it in place. Any failure after the create deletes the environment before the error reaches the
caller.

| Field | Default | Meaning | `AgentSandboxWorkspace` |
|---|---|---|---|
| `template` | required to create | base template with the agent-server layer | `warmpool` |
| `image` | the template's image | task image, `repo@sha256:…` | none (fixed in the SandboxTemplate) |
| `atespace` | `ate-env` | atespace of the environment | `namespace` |
| `api_url` | `127.0.0.1:7777` | ate-env-api, host:port or URL | `host` in `direct` mode |
| `proxy_url` | `api_url` | port proxy base URL, when it differs | none |
| `api_token` | none | ate-env-api's bearer token: on gRPC calls, and in `X-Env-Authorization` on proxy requests, which ate-env-api removes before it forwards them | none |
| `env_id` | generated | attach to this environment instead of creating one | none |
| `server_port` | `8000` | agent-server port | `server_port` |
| `working_dir` | `/workspace` (created) | where commands run; `/testbed` on SWE-bench images | `working_dir` |
| `api_key` | none | the agent-server's session key | `api_key` |
| `create_timeout` | `600` | seconds for the create (a new image pulls and snapshots) | `sandbox_ready_timeout` |
| `health_check_timeout` | `600` | seconds for `/health`; the first use of a task image on a cluster bakes its golden snapshot and pulls the image (minutes for a large image), later uses take seconds | `health_check_timeout` |
| `delete_on_exit` | created: yes, attached: no | delete in `cleanup()` | always deletes the claim |

- `pause()` suspends the environment; its filesystem stays in Substrate's snapshot.
  `resume()` waits for `/health`, which also wakes it; any request wakes it as well. The host does
  not change, unlike a port-forward. To park an environment between agent turns, close the
  conversation first and reattach it after `resume()`; the agent-server keeps the conversation:

  ```python
  conversation_id = conversation.state.id
  conversation.close()  # stops the event WebSocket
  ws.pause()
  ...
  ws.resume()
  conversation = Conversation(agent=agent, workspace=ws, conversation_id=conversation_id)
  ```
- `port_url(port)` is the proxy URL of another port in the same environment. A request to it from
  outside the SDK adds `proxy_headers()` (empty without `api_token`).
- A rejected session key fails the constructor with `PermissionError`; a version difference between
  `openhands-sdk` and the agent-server is logged as a warning.
- The health wait stops at once on an answer that waiting cannot change: 401 or 403 from the proxy
  (`PermissionError`: a missing or wrong `api_token`, or a refused origin), 400 (`ValueError`: the
  proxy refused the port or the id), and 404 when the environment no longer exists. One attempt can
  wait for the rest of `health_check_timeout`, because the proxy holds a request while the
  environment wakes (ate-env-api's `--wake-budget`).
- A 401 or 403 from the port proxy in the session-key check names `api_token`, not the session key,
  for example when the token changes after the health wait.
- Only the workspace that created or attached the environment deletes it. A failed constructor
  deletes only what it created, a `model_copy()` never deletes and opens its own HTTP client, and
  `__del__` does nothing during interpreter shutdown. A create that fails ambiguously (a timeout,
  a lost answer) deletes the environment by name and retries for 10 s, because the server can still
  be creating it. Get, suspend and delete have a 30 s deadline.

From `AgentSandboxWorkspace`:

```diff
-from openhands.workspace import AgentSandboxWorkspace
+from ate_env_openhands import AteEnvWorkspace
-with AgentSandboxWorkspace(warmpool="openhands-pool", namespace="default",
-                           shutdown_after_seconds=30 * 60) as workspace:
+with AteEnvWorkspace(template="oh-agent-server", image=TASK_IMAGE,
+                     api_url="ate-env-api.ate-env:7777", api_key=SESSION_KEY) as workspace:
     agent = get_default_agent(llm=llm, cli_mode=True)
     conversation = Conversation(agent=agent, workspace=workspace)
```

Not mapped yet: `shutdown_after_seconds` (ate-env has no environment TTL), `labels`,
`pod_labels` and `pod_annotations` (environments have no labels), and agent-sandbox-rl's fleet
adapters (`make_handle_workspace`, `make_fleet_workspace`) and R2E-Gym adapter, which follow in
later changes.

## 5. Examples

The scripts that use the package are in [`examples/openhands`](../../examples/openhands), next to
the repository's other examples:

| file | what it is |
|---|---|
| [`convo_with_ate_env.py`](../../examples/openhands/convo_with_ate_env.py) | OpenHands' agent-sandbox example 17 with the workspace line changed |
| [`codeact_agent.py`](../../examples/openhands/codeact_agent.py) | OpenHands' CodeAct agent in an environment, with a model or `--scripted` |
| [`run_trajectory.py`](../../examples/openhands/run_trajectory.py) | executes a CodeAct trajectory from outside and records the observations |
| [`demo.sh`](../../examples/openhands/demo.sh) | registers the base template and drives the agent-server over plain HTTP |
| [`fleet_rollout.py`](../../examples/openhands/fleet_rollout.py) | an agent-sandbox-rl style rollout: `fleet.run` with `make_handle_workspace` |

## 6. Fleet adapters: the Tunix shape

`ate_env_openhands.fleet` has the two helpers of `agent_sandbox_rl.adapters.openhands`, with the
same names and ownership: the **fleet** (`ate_env.SandboxFleet`) acquires and releases sandboxes;
the workspace only binds one and never creates or deletes an environment.

```python
from ate_env import FleetConfig, SandboxFleet, Task
from ate_env_openhands import make_handle_workspace

fleet = SandboxFleet(FleetConfig(backend="substrate", endpoint="127.0.0.1:7777",
                                 template="oh-agent-server"))
fleet.load_tasks([Task(id="t1", image="docker.io/library/python@sha256:...")])
fleet.set_pool_replicas("docker.io/library/python@sha256:...", 4)   # idle sandboxes per image

def rollout(task, handle):
    workspace = make_handle_workspace(handle, api_key=SESSION_KEY, api_url="127.0.0.1:7777")
    try:
        return workspace.execute_command("python3 -m pytest -q", timeout=600).exit_code
    finally:
        workspace.cleanup()          # HTTP state only; fleet.run releases the sandbox

results = fleet.run(rollout)
```

- `make_handle_workspace(handle, **kw)`: an `AteEnvWorkspace` on `handle.sandbox_id`,
  `delete_on_exit=False`. `api_url` and `atespace` default from the handle (its data plane and
  its backend's atespace); `api_key`, `proxy_url`, `api_token` and the timeouts pass through.
- `make_fleet_workspace(fleet, task, **kw)`: acquires on construction and calls
  `fleet.release(handle)` from `cleanup()`, so `with make_fleet_workspace(...) as ws:` owns one
  episode; a failed start (health, session key) releases the handle before the error is raised.
- `template`, `template_atespace`, `image`, `env_id` and `delete_on_exit` are fleet-owned and
  raise `ValueError`, as agent-sandbox-rl's `_FLEET_OWNED_KWARGS` do.
- Pool sizing for a prewarm loop: `SandboxFleet.set_pool_replicas(image, n)` sizes one image's
  warm pool up or down without dropping it (idle, suspended environments; claimed ones are never
  touched), and `warm_image(image, replicas)` warms one image. Both exist on `AsyncSandboxFleet`
  too.

## 7. R2E-Gym adapter

`ate_env_openhands.r2egym` is `agent_sandbox_rl.adapters.r2egym` for ate-env: R2E-Gym's `RepoEnv`
on a sandbox the fleet acquired, so Tunix's `r2egym` and `sweagent` scaffolds change only their
imports. No agent-server is involved: the runtime's exec and file copy go through the handle.

```python
from ate_env_openhands.r2egym import fresh_eval_env, make_fleet_repo_env, r2egym_command_files

def rollout(task, handle):                        # task.metadata["ds"] is the dataset row
    env = make_fleet_repo_env(handle, command_files=r2egym_command_files())
    try:
        obs, reward, done, info = env.step(action)
        patch = env.runtime.get_patch()
    finally:
        env.close()                               # no-op on the sandbox; the fleet releases
    with fresh_eval_env(fleet, handle) as (_eval_handle, grader):   # a clean second sandbox
        grader.runtime.apply_patch(patch)
        return grader.compute_reward()
```

`AteEnvRuntime(DockerRuntime)` overrides the four `kubernetes-sandbox` hooks: bind the handle,
no-op teardown, `handle.exec` for `run` (with R2E-Gym's `(output, exit_code)` strings and
timeouts) and `handle.write_file` plus `tar` for `copy_to_container`. Everything else in the
runtime (setup, patches, reward grading) runs unchanged. R2E-Gym is not on PyPI: install it from
its checkout (`pip install -e path/to/R2E-Gym`); the module imports without it and raises a clear
`RuntimeError` on first use. R2E-Gym's runtime loads a kubeconfig for this backend; the adapter
stubs that during construction, so no Kubernetes access is needed.

## Versions

The workspace talks to the agent-server's REST and WebSocket API through the OpenHands SDK, and that
API changes between releases, so the client's `openhands-sdk` (and `openhands-tools`, for the agent
examples) must be the same version as the agent-server in the layer: 1.55.0 here. To change both,
set the new versions in [`layer/requirements.in`](layer/requirements.in), run
[`layer/lock.sh`](layer/lock.sh), rebuild and push the layer, register the template with the new
layer digest, and change the pins in [`pyproject.toml`](pyproject.toml).

## Testing

```bash
pip install ./clients/python './integrations/openhands[test]'
python -m pytest integrations/openhands/tests -q
```

The tests run `AteEnvWorkspace` against an in-process fake of ate-env-api (environment lifecycle
over gRPC) and of its port proxy (path form, wake on request, upgrade relay), in front of a real
agent-server started on localhost, as the layer runs it. The fake proxy also answers as ate-env-api
does: 400 for port 80, and with a bearer token 401 for a request without it, and it removes
`X-Env-Authorization` before it forwards. The tests cover the create arguments, the health wait and
the answers that stop it at once (400, 401, 403, 404), cleanup after each failure (lost create answer,
a create that lands after its timeout, refused create, health timeout, wrong session key), ownership
(a failed attach or validation error deletes nothing, a copy never deletes), the token at the proxy,
attach, pause and resume, exit ordering, files, the shared loop (`close_all`, the call timeout, a
fork), and a `RemoteConversation` whose event WebSocket goes through the proxy.

## Verified

GKE with gVisor workers, Substrate from 2026-10-12, ate-env-api with the port proxy, agent-server
1.55.0 from this layer, 2026-10-15:

| image | base | agent-server answering after create |
|---|---|---|
| `ubuntu:20.04` | glibc 2.31 | 16 s |
| `swebench/sweb.eval.x86_64.django_1776_django-11099` | Ubuntu 22.04, glibc 2.35 | 16 to 23 s |
| `namanjain12/pandas_final` (R2E-Gym) | Ubuntu 22.04, glibc 2.35 | 20 s |
| `node:22-slim` | Debian 12, glibc 2.36 | 16 s |
| `python:3.12-slim` | Debian 13, glibc 2.41 | 16 s |

- `demo.sh` on each: commands ran with the image's own shell and tools, file download worked, a call
  without the key got 401; also with `PORT=3000` and an extra `--env` setting.
- `codeact_agent.py --scripted` in the SWE-bench environment: the CodeAct run above, its event
  WebSocket through ate-env-api and the Substrate router.
- The SDK workspace on `node:22-slim` saw the image's own `node v22`; on the SWE-bench image a
  command saw the image's Python 3.11.5, not the layer's 3.12. After a suspend, the next SDK call woke
  the environment and returned within 9 s.
- On that cluster actors had no route to the internet (connection reset), hence `--scripted`.
- `run_trajectory.py` in the same SWE-bench environment: `django-11099.json` fixed the issue in 9
  steps and 5.2 s (file edits 0.3 s each once the tool runner was up), and `django-11099-grade.json`
  showed the instance's target tests failing without the fix and passing with it. `hello.json` ran on
  the same image.
- The arm64 build of the layer: the agent-server answered `/health` within 9 s in an arm64 Ubuntu 22.04
  container (local Docker); the CodeAct flow ran the same way locally before the cluster run.

- 2026-10-16, after the hash-locked rebuild, local Docker on arm64 only (the cluster runs above used
  the earlier layer): the layer mounted read-only at `/opt/oh` in `debian:bullseye-slim` (glibc
  2.31) and `ubuntu:24.04` answered `/health` and ran a command with the key (401 without);
  `run_trajectory.py` ran `hello.json` and recorded a failed step in `--out`; `codeact_agent.py
  --scripted` finished and its conversation was gone afterwards. The amd64 build is not yet verified.

## Limits

- `api_token` needs an ate-env-api whose port proxy checks `X-Env-Authorization` and removes it
  (ate-env-api with `-auth-token-file`). An older ate-env-api forwards the header to the
  agent-server: do not set `api_token` with it. Without `-auth-token-file`, whoever reaches
  ate-env-api reaches the agent-server port and needs only the session key.
- With `-auth-token-file`, the conversation event WebSocket gets 401: `RemoteConversation` opens it
  with `websockets.connect(ws_url)` and sends no extra headers. Commands, files and the REST calls of
  a conversation work. The fix is a follow-up: a query-parameter token in the port proxy, or a
  header option in the SDK.
- With a live `RemoteConversation`, `pause()` does not keep the environment suspended: the suspend
  breaks the event WebSocket, the SDK reconnects after about a second, and the proxy wakes the
  environment for that request. Thus a paused environment with a live conversation is a running
  actor, and it costs a worker. Close the conversation before `pause()` (see `pause()` above). The
  follow-up is in the OpenHands SDK: hold or close the event stream across a pause.

- One API key per template, and in practice one per atespace: the key is in the template's container
  environment. Each viewer of the atespace can read it with `GetActorTemplate`, and each template
  derived per task image copies it. Substrate's `EnvVar` has a name and a value only: logs redact
  the value, reads do not, and a template cannot refer to a secret. Use a dedicated atespace for
  these environments. The follow-up is a request to Substrate: a template field that uses its
  credential-provider path to inject the key out of band.
- The agent-server runs as the image's user and keeps its state under that user's home. The first
  environment on a node pulls the layer.
- A daemon reached through ate-env-api must listen on all interfaces, not 127.0.0.1: the router
  delivers to the actor's address, and a loopback-only listener answers `502 bad gateway`.
- The examples have their own limits (model calls from inside the environment, the model key, the
  tool runner): see [`examples/openhands`](../../examples/openhands/README.md#limits).
- `fresh_eval_env` costs one more acquire per graded episode: a Substrate actor has one container,
  so the clean grading container of the MLPerf setup is a second environment from the warm pool.
- The R2E-Gym adapter is tested against a stand-in for R2E-Gym (it is not on PyPI); a live
  SWE-bench episode on a cluster is the acceptance step still open.
