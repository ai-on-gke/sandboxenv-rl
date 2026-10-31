# Dependency pins and how to update them

This repository pins development toolchains, language runtimes, package dependencies, container base images, and GitHub Actions across several files. Most automated updates arrive via Dependabot pull requests, while others require manual updates or helper commands.

## Pin inventory

| What | Where | Updated by | Checked by |
|---|---|---|---|
| Nix flake inputs (`nixpkgs`, `flake-parts`, `pyproject-nix`, `uv2nix`, `pyproject-build-systems`) | [`flake.lock`](../../flake.lock), [`flake.nix`](../../flake.nix) | Dependabot (`nix`) or `make update-pins` | [`hack/ci/check-pins.py`](../../hack/ci/check-pins.py), `flake-checker` |
| Go toolchain (`pkgs.go_X_Y`) | [`nix/ci.nix`](../../nix/ci.nix) | Derived from [`go.mod`](../../go.mod) `go` directive; patch from `nixpkgs` | [`hack/ci/check-pins.py`](../../hack/ci/check-pins.py) |
| Python interpreters (`pkgs.python313`; `pkgs.python312` for uni-agent) | [`nix/python.nix`](../../nix/python.nix) (`python = pkgs.python313;`, `python312Set`) | Manual; uni-agent stays on 3.12 because upstream declares `requires-python <3.13` | uv2nix at evaluation (the interpreter must satisfy `uv.lock`'s `requires-python`) |
| uv | [`flake.lock`](../../flake.lock) (`pkgs.uv` in the devShell) | With nixpkgs | `[tool.uv] required-version` in [`pyproject.toml`](../../pyproject.toml), a floor only (Dependabot brings its own uv) |
| Python packages (every project's dependencies, ruff, pytest) | [`uv.lock`](../../uv.lock), from the uv workspace in [`pyproject.toml`](../../pyproject.toml) and each project's `pyproject.toml` | Dependabot (`uv`), `uv lock` after a `pyproject.toml` edit, or `make update-pins` (`uv lock --upgrade`) | [`hack/ci/check-pins.py`](../../hack/ci/check-pins.py) (`uv lock --check`); [`nix/python.nix`](../../nix/python.nix) builds the envs from it, so there's nothing else to drift |
| Pinned CI CLI tools | [`nix/ci.nix`](../../nix/ci.nix) (`ciPackages`) | Manual | [`nix/ci.nix`](../../nix/ci.nix) evaluation, CI jobs |
| Go dependencies | [`go.mod`](../../go.mod), [`go.sum`](../../go.sum) | Dependabot (`gomod`; `go.ate.dev/substrate` excluded) | `make ci-go` (go build, vet, test -race, format, docs, boilerplate, verify-protos) |
| OpenHands SDK, tools and agent-server (`==` pins) | [`integrations/openhands/pyproject.toml`](../../integrations/openhands/pyproject.toml), moving with [`layer/requirements.in`](../../integrations/openhands/layer/requirements.in) | Manual (Dependabot ignores `openhands-*`), then `uv lock` | `make ci-openhands` |
| uni-agent upstream commit | [`integrations/uni-agent/pyproject.toml`](../../integrations/uni-agent/pyproject.toml) (`[tool.uv.sources]` `rev`; the `uni-agent` dependency group) | Manual: edit the `rev`, then `uv lock`. Dependabot ignores it | The uni-agent suite in `ci-python` |
| GitHub Actions commit SHAs | [`.github/workflows/*.yaml`](../../.github/workflows), [`.github/actions/*/action.yml`](../../.github/actions) | Dependabot (`github-actions`) | [`hack/ci/lint-actions-locally.sh`](../../hack/ci/lint-actions-locally.sh), org zizmor scan |
| CI Docker base image digest | [`Dockerfile.ci`](../../Dockerfile.ci) | Manual or `make update-pins` (crane digest resolution) | [`hack/ci/check-pins.py`](../../hack/ci/check-pins.py) (`@sha256:` digest check) |
| Guest container base image | [`.ko.yaml`](../../.ko.yaml) | Manual (`baseImageOverrides`) | Nothing automated |
| Generated protobuf stubs | [`proto/`](../../proto), [`clients/python/src/ate_env/_gen`](../../clients/python/src/ate_env/_gen) | `make protos` (Go stubs embed the generator versions pinned in `go.mod`; Python stubs embed the `grpcio-tools` version pinned in `clients/python/pyproject.toml` and `uv.lock`) | [`hack/verify/protos.sh`](../../hack/verify/protos.sh) (`make ci-go`) |
| Protobuf tools and generator plugins | [`go.mod`](../../go.mod), [`go.sum`](../../go.sum) | Dependabot (`gomod`; `tool` directives) | [`hack/verify/protos.sh`](../../hack/verify/protos.sh) (`make verify-protos`) |
| Substrate platform version | [`README.md`](../../README.md) ("Built against Substrate vX.Y.Z") | Manual (see [check-substrate-head.md](check-substrate-head.md)) | Manual verification |
| E2E rootfs test image | [`hack/ci/targets.py`](../../hack/ci/targets.py) | Manual (refresh digest with `crane digest docker.io/library/python:3.13-slim`) | E2E (`test_create_from_image`) |

## When Dependabot opens a PR

[`.github/dependabot.yml`](../../.github/dependabot.yml) groups dependency updates by ecosystem with a 7-day cooldown, weekly except for Nix flake inputs, which are bumped every two weeks (on the 1st and 15th) while `flake.nix` follows `nixos-unstable`:

### Go dependencies (`gomod`)

- Scope: updates Go modules and tool dependencies in [`go.mod`](../../go.mod) and [`go.sum`](../../go.sum). Tools (`buf`, `protoc-gen-go`, `protoc-gen-go-grpc`) are pinned via `tool` directives in `go.mod`.
- Tool versions: `protoc-gen-go` lives in `google.golang.org/protobuf`, so its version is automatically identical to the runtime dependency; `protoc-gen-go-grpc` is pinned separately; `buf` is pinned to a compatible release.
- Exclusions: `go.ate.dev/substrate` is explicitly excluded from Dependabot; see [check-substrate-head.md](check-substrate-head.md).
- CI execution: runs the `go` job (`ci-go`) in [`.github/workflows/ci.yaml`](../../.github/workflows/ci.yaml) (including `verify-protos` and `verify-docs`) and runs [`.github/workflows/ci-devenv.yaml`](../../.github/workflows/ci-devenv.yaml) (`check-pins` and the devShell smoke checks) because `go.mod` changed.
- Failure remediation: if a dependency update prompts a bump to the `go` directive in `go.mod` that exceeds what the current `nixpkgs` input in [`flake.lock`](../../flake.lock) provides, Nix evaluation throws with instructions to lower the `go` directive or run `make update-pins ARGS=nixpkgs` (or `nix flake update nixpkgs`). If a tool bump changes generated stub formatting or headers, run `make protos` and commit the updated stubs.

### Python dependencies (`uv`)

- Scope: updates [`uv.lock`](../../uv.lock) (and a `pyproject.toml` lower bound when a new version falls outside it) for the whole uv workspace: `clients/python` and the `integrations/*` projects.
- Exclusions: `grpcio-tools` (see the stub generator note below), `openhands-*` (pinned with `==` to the agent-server the OpenHands layer ships; move both by hand) and `uni-agent` (a git source pinned by commit, which Dependabot would look up on PyPI instead, [dependabot-core#16129](https://github.com/dependabot/dependabot-core/issues/16129)). `ruff` is not excluded: patch releases land in the lock, and the `~=0.17.2` range in `clients/python/pyproject.toml` makes a minor (rule-changing) bump a deliberate edit.
- CI execution: runs `ci-python` and `openhands` in `ci.yaml` on the new lock (the Nix envs are built from it), and `check-pins` and the devShell smoke checks in `ci-devenv.yaml`.
- Failure remediation: if a bumped package only ships an sdist, or a new git dependency appears, the Nix build fails on the missing build system: add it to `buildSystemOverrides` in [`nix/python.nix`](../../nix/python.nix). If a wheel needs a native library that isn't there, add it to the package's `buildInputs` the same way (wheels are patched with `autoPatchelfHook`).
- Python stub generator: `grpcio-tools` is pinned with `==` in [`clients/python/pyproject.toml`](../../clients/python/pyproject.toml) and ignored by Dependabot, because the stubs it writes embed its version (`GRPC_GENERATED_VERSION`) and refuse to run on an older grpcio. Move it together with the client's `grpcio` floor: edit both, `uv lock`, `make protos`, and commit the regenerated stubs (`make ci-go`'s `verify-protos` fails until you do).

### Nix flake inputs (`nix`)

- Scope: bumps flake inputs in [`flake.lock`](../../flake.lock), every two weeks.
- CI execution: runs `check-pins` and the devShell smoke checks in [`.github/workflows/ci-devenv.yaml`](../../.github/workflows/ci-devenv.yaml).
- Python: a nixpkgs bump moves the interpreters (patch releases) and uv, not the Python packages, which come from [`uv.lock`](../../uv.lock). Go generator tools are decoupled from nixpkgs too, pinned in [`go.mod`](../../go.mod) via `tool` directives.

### GitHub Actions (`github-actions`)

- Scope: bumps action versions in [`.github/workflows/*.yaml`](../../.github/workflows).
- Version tag comments: actions must be pinned to immutable commit SHAs with a trailing version comment. The comment must specify an immutable release tag (e.g. `# 31.11.3` on `cachix/install-nix-action`), rather than a mutable major tag like `# v31`. Using a moving tag like `# v31` triggers zizmor's `ref-version-mismatch` rule and fails the required security scan on PRs.
- Validation: run `make lint-actions-locally` before merging.

If `check-pins` fails on any Dependabot PR, pull the branch locally and run:

```bash
make update-pins
make check-pins
make ci
git commit -a -m "chore: align pins and stubs"
```

## Manual chores and watch-outs

- Bumping Python minor version: change `python = pkgs.python313;` in [`nix/python.nix`](../../nix/python.nix) (for example to `pkgs.python314`). That one line moves every env except uni-agent's, which stays on `pkgs.python312` (`python312Set`) until upstream uni-agent supports a newer Python. `uv.lock` is universal (it covers every Python its `requires-python`, `>=3.12`, allows), so it needs no re-lock unless a package has no wheel for the new version. Verify with `make check-pins`, `make ci-python`, and `make ci-openhands`.
- Adding a Python dependency: add it to the project's `pyproject.toml` and run `uv lock` in the devShell (`nix develop -c uv lock`); commit both files. The next `nix develop` builds the envs with it. Which extras each env gets is listed in [`nix/python.nix`](../../nix/python.nix): the full env (the devShell's Python) takes every extra of `clients/python` and `integrations/nemo-gym` except nemo-gym's `gym` (the real nemo-gym; the tests use its local fallback types), so a new extra there is picked up automatically. A package that only ships an sdist, or a git dependency, needs its build system in `buildSystemOverrides` there (`uv.lock` doesn't record build systems, [astral-sh/uv#5190](https://github.com/astral-sh/uv/issues/5190)). A new workspace project goes into `members` in `pyproject.toml` and in `nix/python.nix` (its editable sources).
- Working without Nix: `uv sync --all-packages --all-extras` at the repository root installs the locked versions into `.venv` (`--frozen` skips the check that the lock is current). For one integration: `uv sync --package ate-env-openhands --extra test`, or `uv sync --python 3.12 --package uni-agent-substrate --extra test --group uni-agent`. The root `pyproject.toml` names PyPI as the index, so a machine whose uv config points at a mirror still locks against PyPI and writes URLs that work everywhere. It also requires uv 0.13.1 or newer (`required-version`); with an older uv, run `uv self update`, or run uv through `nix develop`.
- Adding a CLI tool: Add any new tool required by CI scripts to `ciPackages` in [`nix/ci.nix`](../../nix/ci.nix) so that it is included in `nix develop` shells, which the Docker runner uses too.
- Regenerating protobuf stubs: Generator tool updates often alter code generation formatting or header versions. When [`hack/verify/protos.sh`](../../hack/verify/protos.sh) fails during `make ci-go`:
  ```bash
  make protos
  ```
  This invokes `make python-protos` ([`clients/python/scripts/gen-protos.sh`](../../clients/python/scripts/gen-protos.sh)) and `go tool buf generate` ([`buf.gen.yaml`](../../buf.gen.yaml)). Commit the updated stubs in `proto/` and `clients/python/src/ate_env/_gen/`.
- Refreshing Dockerfile.ci digest: Run `make update-pins`. It uses `crane` to resolve `nixos/nix:latest` and updates [`Dockerfile.ci`](../../Dockerfile.ci).
- Bumping go.mod go directive: When upgrading the `go` directive in [`go.mod`](../../go.mod), [`nix/ci.nix`](../../nix/ci.nix) maps the major and minor version to `pkgs.go_X_Y`. If the locked `nixpkgs` lacks that Go release, evaluation aborts with:
  ```text
  nix/ci.nix: go.mod asks for Go X.Y, but the locked nixpkgs has no pkgs.go_X_Y.
  Lower the `go` directive in go.mod to a Go version the locked nixpkgs packages, or wait until nixpkgs packages Go X.Y.
  If a newer nixpkgs already has it, run `make update-pins ARGS=nixpkgs` (or `nix flake update nixpkgs`) to move the lock forward.
  ```
  Lower the `go` directive in `go.mod`, wait for nixpkgs to package it, or run `make update-pins ARGS=nixpkgs` if a newer nixpkgs already includes it.
- Note for AI agents: Never edit [`flake.lock`](../../flake.lock) or [`uv.lock`](../../uv.lock) directly (`uv lock` rewrites the latter). Always run `make update-pins`, verify with `make check-pins`, and validate with `make ci`.

## Commands reference

- `make check-pins`: runs [`hack/ci/check-pins.py`](../../hack/ci/check-pins.py) to verify the toolchain against `go.mod`, `uv.lock` against the `pyproject.toml` files and the `Dockerfile.ci` digest, followed by `flake-checker --no-telemetry`. Automatically selects Nix or Docker via [`hack/ci-compat.sh`](../../hack/ci-compat.sh).
- `make update-pins`: updates all flake inputs, upgrades every package in [`uv.lock`](../../uv.lock) (`uv lock --upgrade`, with the uv of the just-updated nixpkgs, run through `nix shell` rather than the devShell so it works even when the devShell doesn't evaluate), re-pins [`Dockerfile.ci`](../../Dockerfile.ci) base image digest, prints a before/after version table (the tools, a few key Python packages, and how many lock entries changed), and runs `check-pins`.
- `make update-pins ARGS=nixpkgs`: updates only the named flake input(s) and leaves the others and `uv.lock` alone (same as `nix run .#update-pins -- nixpkgs`). `ARGS=uv` upgrades only `uv.lock`.
- `make lint-actions-locally`: runs `actionlint` and offline `zizmor` on workflow files in [`.github/workflows`](../../.github/workflows) at medium+ severity threshold.

For detailed failure modes (a stale `uv.lock`, Go version selection, or zizmor action tag rules), see the [Troubleshooting guide](troubleshooting.md).
