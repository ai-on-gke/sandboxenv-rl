# Threat model

This document describes the trust boundaries of ate-env as the code on `main` implements them:
what each boundary protects, who can attack it, what mitigates each threat today, and the known
gaps. It describes merged behavior only.

A pull request that adds network surface, privileges, wire or API fields, or dependencies must
update this document in the same pull request. The pull request template asks for it.

## System

```
 callers ──(1)──▶ ate-env-api ──(2)──▶ Substrate control plane (ateapi)
 (CLI, SDKs,       │  gRPC (h2c)
  MCP clients,     │  MCP (HTTP)                ┌───────────── actor (gVisor) ─────────────┐
  port proxy       │  port proxy (HTTP)         │ ate-env-guest (PID 1, port 80)           │
  clients)         └──────(3)──▶ atenet router ─┼─▶  processes, files, sidecars, daemons   │
                                                └──────────────────────────────────────────┘
```

- **ate-env-api** serves three things on one plaintext listener (`-listen`, default
  `0.0.0.0:7777`): the gRPC API (`EnvironmentService`, `ProcessService`, `FileSystemService`), the
  MCP endpoint `/v1alpha/envs/{id}/mcp`, and the port proxy (header form `X-Env-Id` and
  `X-Env-Port`, path form `/v1alpha/envs/{id}/ports/{port}/...`).
- **Lifecycle calls** (create, get, suspend, delete) go to the Substrate control plane over TLS
  with a bearer token.
- **Guest calls, MCP and the port proxy** go to the atenet router over plaintext gRPC or HTTP
  CONNECT. The router forwards them to the actor that the `ate-target-actor` header names.
- **Telemetry**, off by default: with `--metrics-listen-addr`, `ate-env-api` serves Prometheus
  metrics on a second plaintext listener; with `--enable-tracing`, it exports traces over OTLP and
  passes the caller's W3C trace context on to the control plane and the guests (see
  [Server telemetry](#server-telemetry)).
- **ate-env-guest** runs as PID 1 in each actor. It serves processes and files on port 80, reaps
  orphaned processes, and starts the sidecars that the template names.
- **`ate-env manifest`** deploys the namespace, the WorkerPool, and the `ate-env-api`
  ServiceAccount, Deployment and Service. `ate-env manifest access-policy` grants that
  ServiceAccount the `editor` role on an atespace.

## Assets

- The contents of each environment: code, data, and the credentials that callers put in it.
- The control-plane credential of `ate-env-api`: a projected ServiceAccount token, audience
  `api.ate-system.svc`, that expires after `tokenExpiration` in `cmd/ate-env/manifest.go`.
- The caller token in the Secret that `ate-env manifest` mounts: `ate-env-api-token`, or the one
  that `--auth-token-secret` names. There is no token only with `--insecure-no-auth`.
- The session key of the OpenHands agent-server (`OH_SESSION_API_KEYS_0`), in the container
  environment of each template that `integrations/openhands` registers, and of each template that
  `ate-env-api` derives from one.
- ActorTemplates, golden snapshots and suspended environments in the snapshots bucket.
- Worker capacity: the CPU, memory and actors of the WorkerPools.

## Attackers

| Attacker | Capability |
|---|---|
| A network peer | Reaches the `ate-env-api` listener: a workload in the cluster, or anyone behind a port-forward or a LoadBalancer. |
| A token holder | Has the caller token. Has all the powers of the API. |
| Code in an environment | Runs as the guest user inside an actor: agent actions, model output, task code. Treat it as untrusted. |
| A web page | Runs in the browser of a developer who port-forwards the API to localhost. |
| A template author | Registers ActorTemplates with `kubectl-ate`. Trusted: chooses the image, its command, the sidecars and the resources. |
| A dependency or image | A compromised Go module, Python package, base image or GitHub Action. |

## Boundary 1: callers to ate-env-api

| Threat | Current mitigation | Gap |
|---|---|---|
| Unauthenticated use of the API | `-auth-token-file` makes the gRPC API, MCP and the port proxy require `Authorization: Bearer <token>`. The port proxy also accepts the token in `X-Env-Authorization` and does not forward it. The check uses a constant-time compare. The server reads the file again when it changes. A token file that it cannot read gives the caller only `internal error`. Without a token, `ate-env-api` logs a warning when it listens on a non-loopback address. The CLI, the Go and Python clients, the SWE-bench loader, the MCP example and the NeMo Gym provider can send the token. | `ate-env manifest` mounts the token by default, and `--insecure-no-auth` turns authentication off. The `ate-env-api` binary itself has no token unless `-auth-token-file` is set: a deployment that is not made with `ate-env manifest` must set it. |
| Token theft on the network | The manifest Service sets no `type`, thus it is `ClusterIP`. | The listener has no TLS: the token and all data travel in clear text. The manifest ships no NetworkPolicy. |
| One caller acts on the environments of another caller | None. | One shared token. No per-caller identity and no authorization: each token holder can use each environment. |
| Reach beyond the environments of ate-env | Lifecycle calls use the `ate-env-api` identity, which the access policy limits to its atespaces. The port proxy forwards only to the atespaces that `--port-proxy-atespaces` names (default `ate-env`) and answers 403 for the others. | Guest calls and MCP go to the router. The control plane is asked only while an environment wakes. Thus these paths reach any actor that the router serves, in any atespace, including actors that ate-env did not create. With `--port-proxy-atespaces='*'`, the port proxy does the same. |
| A web page drives the API through a port-forward (CSRF) | The port proxy path form refuses a state-changing request or a WebSocket handshake from another origin (`--port-proxy-trusted-origins` adds origins). The header form needs custom headers, and MCP needs a JSON content type: a browser sends either one to another origin only after a CORS preflight, which the API never approves. | — |
| DNS rebinding: a page on an attacker's name resolves to the forwarded listener and becomes same-origin | None. | The listener does not check the `Host` header. The MCP handler sets `DisableLocalhostProtection: true`. |
| The port proxy reaches the guest port | The port proxy refuses port 80, where the guest listens. | — |
| Resource exhaustion | The port proxy bounds its idle tunnels with `idleTunnelsPerPort`, `idleTunnelsTotal` and `idleTunnelTimeout` in `internal/portproxy/portproxy.go`. Worker capacity bounds the running actors. | A token holder can create environments without a quota or a rate limit; suspended environments keep their snapshots in the bucket. The MCP handler keeps one server for each environment, and the client keeps one guest connection for each environment. Neither is evicted while the process runs. |

## Boundary 2: ate-env-api to the Substrate control plane

| Threat | Current mitigation | Gap |
|---|---|---|
| Theft of the control-plane credential | A projected token with audience `api.ate-system.svc` and the lifetime that `tokenExpiration` sets. The ServiceAccount mounts no Kubernetes API token. | `-skip-verify` defaults to `true`, and the manifest does not set it. Thus `ate-env-api` does not verify the TLS certificate of the control plane. A peer that intercepts the connection can read the token. |
| Excess privilege | A dedicated ServiceAccount. The access policy grants `editor` on each atespace that an operator adds. Creating an atespace takes a global owner, which `ate-env-api` is not. | `editor` includes creating ActorTemplates. Refer to on-demand rootfs images. |

## Boundary 3: the router and the actors

| Threat | Current mitigation | Gap |
|---|---|---|
| Use of a guest without the API | The router runs in `ate-system`, and the guest listens only inside the actor. | The connections to the router are plaintext. The guest does not authenticate callers: a client that reaches an actor through the router can run commands as the guest user. |
| Code in an environment attacks the node or other environments | Each actor runs in gVisor, the sandbox class of the WorkerPool. | ate-env sets no actor egress policy. Egress follows the Substrate defaults. |
| Processes escape supervision | When the guest is PID 1, it runs as an init: it reaps orphans and forwards signals. A process timeout kills the process group. The guest refuses PTY window sizes that do not fit the 16-bit kernel fields. | — |
| A sidecar gains extra privilege | Only a template author sets sidecars (`--sidecar`, `--sidecar-readyz`), and that author already controls the image and its command. Sidecars run inside the CPU and memory limits of the actor. | A sidecar runs in the same container as the code in the environment. A shared credential in the environment of a sidecar, such as the OpenHands session key, is thus readable by that code. Refer to the OpenHands agent-server. |

## The OpenHands agent-server

`integrations/openhands` runs the OpenHands agent-server as a sidecar from a runtime layer. The
base template sets its session key in the container environment (`ate-env manifest template --env
OH_SESSION_API_KEYS_0=<key>`). The workspace reaches the agent-server through the port proxy.

| Threat | Current mitigation | Gap |
|---|---|---|
| A caller reads the session key from a template | Reading a template needs viewer access to the atespace. Substrate logs redact the values of environment variables. Use a dedicated atespace for these environments, as `integrations/openhands/README.md` says. | Each caller with viewer access to the atespace reads the key with `GetActorTemplate`: reads do not redact it. Each template that `ate-env-api` derives from the base copies it. A template cannot refer to a secret. |
| A holder of the session key drives the agent-server | With `-auth-token-file`, the port proxy also asks for the caller token. Thus the key alone does not pass the port proxy. | The agent-server checks only the session key (`check_session_api_key` in `openhands/agent_server/dependencies.py`). Without `-auth-token-file`, each network peer of `ate-env-api` that has the key controls the agent-server of each environment whose id it knows. |
| Code in one environment drives the agent-server of another environment | Each actor runs in gVisor. The agent-server removes the key from the environment of the commands that it starts. With `-auth-token-file`, the key alone does not pass the port proxy. | All environments of one base template share one key. The key is in the environment of the guest. Each command that the guest starts through `ate-env-api` inherits that environment (`guest/process/tracker.go`): `ate-env exec`, the Go and Python clients, the fleet's `handle.exec`, and the run and copy of the R2E-Gym adapter. Such a command reads the key with `env`; it does not need `/proc`. Thus on the OpenHands base template, each R2E-Gym agent action gets the key. The guest starts the agent-server with its full environment (`cmd/ate-env-guest/sidecar.go`). The commands that the agent-server starts do not get the key: the bash tool, the commands of the CodeAct agent, and the terminal steps of `run_trajectory.py`. They run in the same container, thus they can read the key from `/proc/<pid>/environ` of the agent-server when they run as the same user. This read is not verified on a cluster. In both cases, the code needs a route to `ate-env-api` to reach the agent-server of another environment. With that route, it needs nothing more when authentication is off, and a credential that the port proxy accepts when authentication is on. |
| The workspace exposes the caller token to the agent-server or a log | With `api_token`, REST requests carry the token in `X-Env-Authorization`, which the port proxy removes before it forwards them. The event WebSocket carries only a port capability for the agent-server port of its environment, never the token; an ate-env-api with port capability support does not forward it. The client default for `ws_capability_ttl` is one day. `repr`, the model dumps, error messages and the workspace's loggers mask the capability. The agent-server is a FastAPI application on uvicorn, thus a capability opens only its WebSocket routes. | A leaked capability opens WebSocket handshakes to that port until it expires, at most seven days less five minutes after the workspace made it. |

## Snapshots and the snapshots bucket

A snapshot holds the memory and the files of an environment, and thus the credentials that callers
put in it. Golden snapshots of templates and the snapshots of suspended environments go to the
bucket that `ate-env manifest template --snapshots-bucket` names.

What ate-env controls: `ate-env manifest template` writes the bucket URL into the template's
`snapshotConfig.storageLocation`, and templates that `ate-env-api` derives keep it. `ate-env-api`
does not read or write the bucket itself.

What ate-env does not control: Substrate writes and reads the snapshots. The operator owns the
access control, the encryption and the retention of the bucket. What stays in the bucket after an
environment or a template is deleted follows Substrate.

Operator requirements:

- Grant access to the bucket only to the Substrate identities that write and read snapshots. Block
  public access.
- Use encryption with a customer-managed key if your data requires it.
- Set a lifecycle rule that matches your retention policy.
- Use a separate bucket or prefix for each cluster.

| Threat | Current mitigation | Gap |
|---|---|---|
| A reader of the bucket gets the memory, files and credentials of environments | The operator's bucket settings. | ate-env does not check the access control, the encryption or the retention of the bucket. |

## On-demand rootfs images

`CreateEnvironment` with `rootfs_image` makes `ate-env-api` derive an ActorTemplate named
`<base>-<first 12 hex digits of the digest>` from the base template. The image replaces the
container, and the guest is mounted into it as a read-only image volume.

| Threat | Current mitigation | Gap |
|---|---|---|
| A caller runs an image of its choice under the settings of the base template (worker selector, snapshots bucket, resources) | The image must be pinned by digest. A template with that name that runs another image is refused with `FailedPrecondition`. | Any registry is accepted. |
| Template growth | None. | Derived templates have no quota and no garbage collection. |

Accepted risk for now: this hardening is not needed for RL at the moment. Revisit it before the
API serves callers other than trusted RL jobs.

## Client tracing

The Python client emits OpenTelemetry spans when `TracerConfig(enable_tracing=True)` is set. The
spans go to the application's `TracerProvider`. Without one, the client builds an OTLP exporter
only when `OTEL_EXPORTER_OTLP_ENDPOINT` or `OTEL_EXPORTER_OTLP_TRACES_ENDPOINT` is set; otherwise
the spans go nowhere.

| Item | Current state | Gap |
|---|---|---|
| Data in spans | Span names and attributes: environment, atespace and template names, the image reference without its tag, the run id, the executable name (without arguments and without leading `NAME=value` assignments), the exit code, a first-command marker, and file paths and sizes. No command arguments, no file content and no token. | The trace backend learns environment names and file paths. |
| Data on the wire | With tracing on, each gRPC call on a channel that the client creates carries the W3C `traceparent` and `tracestate`, never baggage. Without `--enable-tracing`, `ate-env-api` ignores them; with it, see [Server telemetry](#server-telemetry). | — |
| The export connection | The `OTEL_EXPORTER_OTLP_*` environment variables configure the endpoint, its TLS and its credentials. | An endpoint without TLS sends the spans in clear text. |

## Server telemetry

`ate-env-api` serves metrics only with `--metrics-listen-addr` and traces only with
`--enable-tracing`. `ate-env manifest` sets them with `--metrics-port` and `--enable-tracing`.

| Item | Current state | Gap |
|---|---|---|
| The metrics listener | Off by default. When on, it serves `GET /metrics` on its own listener, with a 5 s header read timeout. It serves request counts and latencies by gRPC method and status code, `create_to_ready` and wake-wait latencies by atespace and outcome, atespace names with environment counts by state, and the build version and commit. Labels never carry environment ids, actor ids, run ids, paths, image references or tokens. Environment counts come from the control plane at most once per `--metrics-environments-ttl` (default 1m). | The listener has no authentication and no `Host` check. Anyone who reaches it (with the manifest, any pod that reaches the pod IP or the Service port) reads the atespace names and the counts. |
| Incoming trace context | With `--enable-tracing`, the otelgrpc server handler continues the W3C `traceparent` and `tracestate` that a caller sends, and the server passes them on to ateapi and the guests as is. Only W3C Trace Context is propagated, never baggage. | The caller is not trusted: it chooses the trace id and the sampled flag, thus it can join the server's spans to its own trace or ask every hop to sample. |
| `x-ate-env-requested-at` | A caller may send this gRPC metadata on `CreateEnvironment` to start `create_to_ready` at its own clock. A value in the future or not in RFC 3339 is ignored. | A caller can send a time in the past and lengthen its own `create_to_ready` samples. |
| The export connection | The `OTEL_EXPORTER_OTLP_*` environment variables configure the endpoint, its TLS and its credentials. Control-plane reads for the environments gauge are not traced. | An endpoint without TLS (`--otlp-insecure`) sends the spans in clear text. |

## Harbor integration

`integrations/harbor` is a Harbor environment provider: `harbor run` creates one ate-env
environment per trial, runs the agent's and the verifier's commands through the guest, and moves
files through the file API. It runs in the operator's Harbor process, next to the agents' model
keys, as a client of the existing gRPC listener; nothing new listens and `ate-env-api` gains no
privilege.

| Item | Current state | Gap |
|---|---|---|
| Caller token | Read from `SUBSTRATE_ENV_AUTH_TOKEN` or the `api_token` kwarg and sent as gRPC metadata. The provider does not log it and does not write it to `trial_dir/ate_env.json`. | Harbor writes the environment kwargs into each trial's `result.json`, thus a token passed with `--ek api_token=` lands on disk with the results. The provider warns when it sees the kwarg; the README says to set the variable. |
| Registry credentials | `build=local` uses the operator's docker login, `build=cloud-build` the operator's gcloud account; the provider reads neither. Images are pushed as `<registry>/<task>:<environment_id>`. | The worker nodes need read access to that registry; granting it is an operator step outside this repository. |
| Task images | A dataset's task images run as actors, pinned by digest, the way any rootfs image does; the provider does not inspect them, and the actor's isolation is the boundary. | Egress follows the Substrate defaults (known gap 8), and most tasks and installed agents download packages at run time. |
| Commands as root | Harbor's verifier removes its reward files and `chmod`s its script as `root`. On an image that runs as root that is the image's own user, and the command runs as is. On another image the provider uses `sudo -n`, or `su` from root, only when the image offers it; otherwise the call fails and says why. | — |
| Dependencies | `harbor`, pinned to one minor, and `dockerfile-parse`, in `integrations/harbor` only; neither is a dependency of the client, the CLI or `ate-env-api`. | Not covered by Dependabot (the local path source, as for `integrations/nemo-gym`); bumped by hand with the trial test as the gate. |

## Compatibility of the API

A protobuf message with fields that the server does not know is valid proto3: the server keeps the
unknown fields and ignores them. Thus a client from another version or fork of this API, which uses
the same `ateenv.v1alpha` names, can get a behavior other than the one it asked for, without an
error. This is a compatibility risk, not a boundary of this threat model.

## Supply chain and dependencies

| Item | Current state | Gap |
|---|---|---|
| Images | A template mounts the guest into a rootfs image only when the guest is pinned by digest. `.ko.yaml` pins the guest base image by digest. The README asks for the worker image by digest. | — |
| Releases | `.github/workflows/release.yaml` signs the images with cosign keyless signing (GitHub Actions OIDC). Its actions are pinned by commit SHA. | This repository has published no release yet. |
| Dependencies | `ate-env-api` links `prometheus/client_golang`, the OpenTelemetry SDK and OTLP/gRPC trace exporter, and `otelgrpc`; they run only when metrics or tracing is on. `uv.lock` pins every Python dependency of `clients/python` and the `integrations/*` projects (one uv workspace; `integrations/nemo-gym` and the others get `ate-env-client` from it as a workspace member) by version and SHA-256, and the Nix envs CI runs are built from it; uni-agent, a git dependency, is pinned by commit. `flake.lock` pins the toolchain (Go, the Python interpreters, uv, ko, kind, kubectl) and the Nix inputs; `go.mod`/`go.sum` pin the Go modules and the protobuf generators. Dependabot covers Go modules (`gomod`), `uv.lock` (`uv`; `openhands-*`, `uni-agent` and `grpcio-tools` are moved by hand), GitHub Actions, the OpenHands layer's base images (`docker`) and `flake.lock` (`nix`); see `.github/dependabot.yml`. `integrations/harbor` is a workspace member too; its `harbor` dependency is pinned to one minor in its `pyproject.toml` and bumped by hand (see Harbor integration). | — |
| Integrations and examples | The code in `integrations/` is maintained. | The code in `examples/` is not supported: it gets no security fixes and no compatibility promise. |

## Known gaps

1. Caller authentication uses one shared token, and the listener has no TLS. A deployment that
   is not made with `ate-env manifest` has no authentication unless it sets `-auth-token-file`.
2. Guest calls and MCP reach any actor that the router serves; the access policy does not limit
   them. The port proxy reaches the atespaces that `--port-proxy-atespaces` names.
3. DNS rebinding is not blocked: the listener has no `Host` check, and MCP localhost protection is
   off.
4. `ate-env-api` does not verify the TLS certificate of the control plane by default.
5. Environment creation has no quota and no rate limit. The MCP server cache and the guest
   connection pool grow with each environment.
6. ate-env does not check the access control, the encryption or the retention of the snapshots
   bucket.
7. The manifest ships no NetworkPolicy and no `securityContext`.
8. ate-env sets no actor egress policy.
9. On-demand rootfs images accept any registry and create templates without a quota or garbage
   collection (accepted for now).
10. The metrics listener, when it is on, has no authentication and no `Host` check, and incoming
    trace context from callers is forwarded as is.
11. The OpenHands session key is readable from the template by each viewer of the atespace, all
    environments of one base template share it, and it is the only check of the agent-server.
