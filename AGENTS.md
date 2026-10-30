# sandboxenv-rl

<!-- Last full audit: 2026-10-29 (commit 23c3647) -->

Sandboxed execution environments for reinforcement learning and evaluation on Agent Substrate. Each
environment runs as an isolated Substrate actor inside gVisor, supporting on-demand rootfs image
derivation, interactive terminals, streaming file transfers, Model Context Protocol tools, and
high-throughput Python fleet scheduling. See [README.md](./README.md) for usage and
[docs/architecture.md](./docs/architecture.md) for design.

## Rules for contributors

- Do not add AI co-author or assisted-by trailers to commit messages.
- Review your diff before requesting review. For substantial changes, have a fresh agent review the
  diff adversarially, then confirm and address what it identifies.
- One change per pull request. Submit refactors and follow-ups in separate pull requests. Search
  open pull requests and issues before starting.
- Be brief. Reviewer time and context windows are limited. Write short, direct sentences, subject
  first, active voice. Avoid em-dashes, chains of semicolons, and contrast pairs ("X, not Y").

## Code

- Search the repository and its dependencies before writing a helper, cache, client, flag set, or
  test fixture. Reuse or extend what exists, and handle cases the way nearby code already does.
- Build only what the change needs. Do not add options, interfaces, wrappers, flags, exports, or API
  fields for speculative future use. Prefer a sensible default to a new knob. Delete code your
  change makes unused.
- Pass dependencies through constructors. Do not add package globals or setters for testing.
- Fail loudly. Ignore only the specific errors you expect (such as `os.ErrNotExist`). Do not fall
  back silently to an insecure or no-op mode.
- Check every claim in comments, documentation, and pull request descriptions against the code. When
  you change behavior, update the documentation that describes it.
- Find the root cause before fixing a symptom. Back performance claims with a measurement.
- Planned: Do not assume kind or GKE, or a specific CPU architecture; dev scripts must support both
  clusters.
- Put shell logic in standalone `.sh` files under `hack/` or `benchmarking/`, not inline in workflow
  YAML.

## Architecture invariants

- `internal/apiservice` owns host adapter translation and proxy routing; `guest/process` and
  `guest/filesystem` own in-environment execution; callers forward through `s.guestConn` with
  `ensureAwake` and must not buffer full files or track process state inside `ate-env-api`; because
  `ate-env-api` is an unbuffered horizontal adapter (not enforced).
- `apiservice.(*Server).guestConn` owns per-call guest gRPC dialing with wake verification; new
  proxy handlers call it and must not dial `atenet` directly; because `ensureAwake` handles actor
  restoration from storage (not enforced). `internal/ate.Client.DialGuest` dials `atenet` for
  `internal/mcp` without `ensureAwake` (known gap; not enforced). `internal/portproxy` owns raw
  `CONNECT` dialing and manages its own wake loop (not enforced).
- `proto/ateenv/v1alpha` owns the interface contract between `ate-env-api` and `ate-env-guest`;
  `internal/` and `clients/` call generated stubs and must not import `guest/` outside tests
  (`guest/` and `cmd/ate-env-guest` must not import `internal/ate` or `go.ate.dev/substrate`);
  because `ate-env-guest` runs inside task images without control-plane dependencies (not enforced).
- `internal/apiservice` owns on-demand `ActorTemplate` derivation; `apiservice.Server` calls
  `DeriveImageTemplate` and `ImageTemplateName` and callers must not construct derived templates
  outside them (base templates come from `ate-env manifest template`, `buildActorTemplate` in
  `cmd/ate-env/manifest.go`); because derived names (`<base>-<digest:12>`) must be deterministic and
  idempotent across racing calls (not enforced).
- `internal/mcp` and `internal/portproxy` own external protocol translation; `internal/mcp`
  translates tool calls to guest RPCs via `ate.Client.DialGuest` (bypassing `apiservice`), while
  `internal/portproxy` tunnels ports; callers must not bypass these adapters; because external
  clients need translation and origin filtering (not enforced).
- `internal/hostcheck` owns Host header validation for HTTP endpoints, and
  `internal/apiservice.BearerAuth` owns token authentication; HTTP handlers wrap with
  `hostcheck.(*Checker).Wrap` (and `BearerAuth` when `-auth-token-file` is configured) and must not
  implement bespoke checks; because browsers can mount DNS-rebinding attacks against local daemons
  (not enforced; `GET /healthz` is exempt from bearer auth).
- `cmd/ate-env-guest` (`init_linux.go` and `sidecar.go`) owns PID 1 orphan reaping and sidecar
  supervision, and `guest/process.Tracker` owns command subprocess lifecycle; callers invoke
  `Tracker` methods and must not manage child processes directly; because PID 1's
  `wait4(-1, WNOHANG)` loop reaps orphaned grandchildren, and non-PTY process groups
  (`Setpgid: true`, PTY `Setsid: true`) must be signaled via negative PID (`-pid`) to prevent leaked
  child trees (enforced by `runIfPID1`, `supervise`, and `guest/process.Tracker`).
- `clients/python/src/ate_env`'s `LoopRunner` owns sync-to-async bridging; synchronous callers
  (`SubstrateBackendDriver`, `SubstrateEnvClientRuntime`, `SandboxFleet`, `SandboxHandle`) dispatch
  coroutines through `LoopRunner.run()` and must not call `asyncio.run()`; because `asyncio.run()`
  destroys the thread event loop and corrupts cached HTTP/2 gRPC channels (not enforced).

Full architecture: [docs/architecture.md](./docs/architecture.md),
[docs/glossary.md](./docs/glossary.md), and
[docs/dev/concurrency-and-pitfalls.md](./docs/dev/concurrency-and-pitfalls.md).

## Repository layout

See [docs/dev/code-layout.md](./docs/dev/code-layout.md) for package responsibilities, decision
rules, and directory layout. Agent skills in `.agents/skills/`:

- [extend-env-surface](./.agents/skills/extend-env-surface/SKILL.md): Add or extend an RPC, guest
  capability, MCP tool, or SDK method.
- [maintain-docs](./.agents/skills/maintain-docs/SKILL.md): Author, update, and validate repository
  documentation against code.

Python naming conventions: repository directory `clients/python`, distribution package
`ate-env-client`, import module `ate_env`. The direct environment handle is `ate_env.Env`; the fleet
handle is `ate_env.SandboxHandle`.

## Build, test, verify

Run before opening a pull request:

```bash
make verify-boilerplate
make verify-gofmt
make verify-docs
make vet
make test
make python-test
make nemo-gym-test
python -m pytest integrations/openhands/tests benchmarking/swebench benchmarking/telemetry_report internal/benchmarking/swebench/trajectories examples/Qwen3-1.7B-verl/tests
ruff check clients/python examples/verl_swebench benchmarking/telemetry_report
ruff check --config clients/python/pyproject.toml examples/Qwen3-1.7B-verl
```

Build targets: `make build` (Go packages), `make build-cli` (`bin/ate-env`), `make images` (via
`ko`), `make format-docs` (Markdown docs), `make clean` (`bin/`).

Regenerate protobuf stubs with `buf generate` (Go) and `make python-protos` (Python); never
hand-edit generated files.

Test doubles: use `internal/internaltest` (`fakecontrol`, `fakerouter`, `testcerts`) instead of
custom mocks; see [docs/dev/code-layout.md](./docs/dev/code-layout.md).

## Pull requests and commits

- Fill out `.github/pull_request_template.md`, including the `Security impact` checklist. If any
  security box is checked, update `docs/security/threat-model.md`.
- Start the pull request description with why the change is needed. Write `Fixes #N` only if the
  pull request fully resolves the issue.
- Pull requests merge as a single squashed commit whose subject ends in `(#N)` and whose body lists
  the pull request's commits, with no AI co-author or attribution trailers.
