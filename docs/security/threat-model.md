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
- **ate-env-guest** runs as PID 1 in each actor. It serves processes and files on port 80, reaps
  orphaned processes, and starts the sidecars that the template names.
- **`ate-env manifest`** deploys the namespace, the WorkerPool, and the `ate-env-api`
  ServiceAccount, Deployment and Service. `ate-env manifest access-policy` grants that
  ServiceAccount the `editor` role on an atespace.

## Assets

- The contents of each environment: code, data, and the credentials that callers put in it.
- The control-plane credential of `ate-env-api`: a projected ServiceAccount token, audience
  `api.ate-system.svc`, valid for 7200 s.
- The caller token in the Secret that `ate-env manifest --auth-token-secret` names, when it is
  configured.
- ActorTemplates, golden snapshots and suspended environments in the snapshots bucket.
- Worker capacity: the CPU, memory and actors of the WorkerPools.

## Attackers

| Attacker | Capability |
|---|---|
| A network peer | Reaches the `ate-env-api` listener: a workload in the cluster, or anyone behind a port-forward or a LoadBalancer. |
| A token holder | Has the caller token, when one is configured. Has all the powers of the API. |
| Code in an environment | Runs as the guest user inside an actor: agent actions, model output, task code. Treat it as untrusted. |
| A web page | Runs in the browser of a developer who port-forwards the API to localhost. |
| A template author | Registers ActorTemplates with `kubectl-ate`. Trusted: chooses the image, its command, the sidecars and the resources. |
| A dependency or image | A compromised Go module, Python package, base image or GitHub Action. |

## Boundary 1: callers to ate-env-api

| Threat | Current mitigation | Gap |
|---|---|---|
| Unauthenticated use of the API | `-auth-token-file` makes the gRPC API, MCP and the port proxy require `Authorization: Bearer <token>`. The port proxy also accepts the token in `X-Env-Authorization` and does not forward it. The check uses a constant-time compare. The server reads the file again when it changes. A token file that it cannot read gives the caller only `internal error`. Without a token, `ate-env-api` logs a warning when it listens on a non-loopback address. | Authentication is off unless configured. `ate-env manifest` mounts a token only with `--auth-token-secret`. |
| Token theft on the network | The manifest Service sets no `type`, thus it is `ClusterIP`. | The listener has no TLS: the token and all data travel in clear text. The manifest ships no NetworkPolicy. |
| One caller acts on the environments of another caller | None. | One shared token. No per-caller identity and no authorization: each token holder can use each environment. |
| Reach beyond the environments of ate-env | Lifecycle calls use the `ate-env-api` identity, which the access policy limits to its atespaces. | Guest calls, MCP and the port proxy go to the router. The control plane is asked only while an environment wakes. Thus these paths reach any actor that the router serves, in any atespace, including actors that ate-env did not create. |
| A web page drives the API through a port-forward (CSRF) | The port proxy path form refuses a state-changing request or a WebSocket handshake from another origin (`--port-proxy-trusted-origins` adds origins). The header form needs custom headers, and MCP needs a JSON content type: a browser sends either one to another origin only after a CORS preflight, which the API never approves. | — |
| DNS rebinding: a page on an attacker's name resolves to the forwarded listener and becomes same-origin | None. | The listener does not check the `Host` header. The MCP handler sets `DisableLocalhostProtection: true`. |
| The port proxy reaches the guest port | The port proxy refuses port 80, where the guest listens. | — |
| Resource exhaustion | The port proxy bounds idle tunnels: 4 per port, 1024 in total, 10 s idle. | The MCP handler keeps one server for each environment, and the client keeps one guest connection for each environment. Neither is evicted while the process runs. |
| A client built from another fork sends a field that this server does not know | None. | `ate-env-api` accepts unknown protobuf fields and ignores them. |

## Boundary 2: ate-env-api to the Substrate control plane

| Threat | Current mitigation | Gap |
|---|---|---|
| Theft of the control-plane credential | A projected token with audience `api.ate-system.svc` and a 7200 s lifetime. The ServiceAccount mounts no Kubernetes API token. | `-skip-verify` defaults to `true`, and the manifest does not set it. Thus `ate-env-api` does not verify the TLS certificate of the control plane. A peer that intercepts the connection can read the token. |
| Excess privilege | A dedicated ServiceAccount. The access policy grants `editor` on each atespace that an operator adds. Creating an atespace takes a global owner, which `ate-env-api` is not. | `editor` includes creating ActorTemplates. Refer to on-demand rootfs images. |

## Boundary 3: the router and the actors

| Threat | Current mitigation | Gap |
|---|---|---|
| Use of a guest without the API | The router runs in `ate-system`, and the guest listens only inside the actor. | The connections to the router are plaintext. The guest does not authenticate callers: a client that reaches an actor through the router can run commands as the guest user. |
| Code in an environment attacks the node or other environments | Each actor runs in gVisor, the sandbox class of the WorkerPool. | ate-env sets no actor egress policy. Egress follows the Substrate defaults. |
| Processes escape supervision | When the guest is PID 1, it runs as an init: it reaps orphans and forwards signals. A process timeout kills the process group. The guest refuses PTY window sizes above 65535. | — |
| A sidecar gains extra privilege | Only a template author sets sidecars (`--sidecar`, `--sidecar-readyz`), and that author already controls the image and its command. Sidecars run inside the CPU and memory limits of the actor. | — |

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

## Supply chain and dependencies

| Item | Current state | Gap |
|---|---|---|
| Images | A template mounts the guest into a rootfs image only when the guest is pinned by digest. `.ko.yaml` pins the guest base image by digest. The README asks for the worker image by digest. | — |
| Releases | `.github/workflows/release.yaml` signs the images with cosign keyless signing (GitHub Actions OIDC). Its actions are pinned by commit SHA. | This repository has published no release yet. `docs/release.md` names the identity of the old repository. |
| Dependencies | Dependabot covers Go modules (`/`), Python (`/clients/python`) and GitHub Actions. | Dependabot does not cover `integrations/nemo-gym`. |
| Integrations and examples | The code in `integrations/` is maintained. | The code in `examples/` is not supported: it gets no security fixes and no compatibility promise. |

## Known gaps

1. Caller authentication is off by default, it uses one shared token, and the listener has no
   TLS.
2. Guest calls, MCP and the port proxy reach any actor that the router serves; the access policy
   does not limit them.
3. DNS rebinding is not blocked: the listener has no `Host` check, and MCP localhost protection is
   off.
4. `ate-env-api` does not verify the TLS certificate of the control plane by default.
5. The MCP server cache and the guest connection pool grow with each environment.
6. `ate-env-api` accepts unknown protobuf fields and ignores them.
7. The manifest ships no NetworkPolicy and no `securityContext`.
8. ate-env sets no actor egress policy.
9. On-demand rootfs images accept any registry and create templates without a quota or garbage
   collection (accepted for now).
10. Dependabot does not cover `integrations/nemo-gym`, and `docs/release.md` names the old
    repository.
