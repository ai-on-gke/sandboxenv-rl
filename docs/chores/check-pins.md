# Dependency pins and how to update them

This repository pins development toolchains, language runtimes, package dependencies, container base images, and GitHub Actions across several files. Most automated updates arrive via Dependabot pull requests, while others require manual updates or helper commands.

## Pin inventory

| What | Where | Updated by | Checked by |
|---|---|---|---|
| Nix flake inputs (`nixpkgs`, `flake-parts`, `pyproject-nix`) | [`flake.lock`](../../flake.lock), [`flake.nix`](../../flake.nix) | Dependabot (`nix`) or `make update-pins` | [`hack/ci/check-pins.py`](../../hack/ci/check-pins.py), `flake-checker` |
| Go toolchain (`pkgs.go_X_Y`) | [`nix/ci.nix`](../../nix/ci.nix) | Derived from [`go.mod`](../../go.mod) `go` directive; patch from `nixpkgs` | [`hack/ci/check-pins.py`](../../hack/ci/check-pins.py) |
| Python interpreter (`pkgs.python313`) | [`nix/python.nix`](../../nix/python.nix) (`python = pkgs.python313;`); [`nix/ci.nix`](../../nix/ci.nix) reuses it for the OpenHands venv | Manual | [`hack/ci/check-pins.py`](../../hack/ci/check-pins.py) (against `requires-python` of the Nix-env projects) |
| uni-agent venv interpreter (`pkgs.python312`) | [`nix/ci.nix`](../../nix/ci.nix) (`python312Bare`) | Manual; stays on 3.12 because upstream uni-agent declares `requires-python <3.13` | pip, when it installs uni-agent into the venv |
| Python dev and runtime packages | [`nix/python.nix`](../../nix/python.nix) (`pythonEnvFull`, plus `pythonEnvBase` for the client tests without optional extras) | Derived automatically from the `clients/python` and `integrations/nemo-gym` `pyproject.toml` files via `pyproject-nix` (base + all extras, PEP 508 markers) | [`hack/ci/check-pins.py`](../../hack/ci/check-pins.py) (installed versions vs. each requirement) |
| Pinned CI CLI tools | [`nix/ci.nix`](../../nix/ci.nix) (`ciPackages`) | Manual | [`nix/ci.nix`](../../nix/ci.nix) evaluation, CI jobs |
| Go dependencies | [`go.mod`](../../go.mod), [`go.sum`](../../go.sum) | Dependabot (`gomod`; `go.ate.dev/substrate` excluded) | `make ci-go` (go build, vet, test -race, format, docs, boilerplate, verify-protos) |
| Python client dependencies | [`clients/python/pyproject.toml`](../../clients/python/pyproject.toml) | Dependabot (`pip`; ignores `ruff`) | [`hack/ci/check-pins.py`](../../hack/ci/check-pins.py) |
| NeMo Gym integration | [`integrations/nemo-gym/pyproject.toml`](../../integrations/nemo-gym/pyproject.toml) | Manual (Dependabot disabled: local path source) | [`hack/ci/check-pins.py`](../../hack/ci/check-pins.py) (its `gym` extra and its `ate-env-client` dependency are skipped by [`nix/python.nix`](../../nix/python.nix)) |
| uni-agent and OpenHands integrations | [`integrations/uni-agent`](../../integrations/uni-agent), [`integrations/openhands`](../../integrations/openhands) | Floating PyPI deps via `ci-venv` and the `.#uni-agent` / `.#openhands` devShells (not pinned by Nix; uni-agent on Python 3.12, openhands on Python 3.13) | `ci-venv` run in CI / `make ci-openhands` |
| uni-agent upstream commit | [`nix/ci.nix`](../../nix/ci.nix) (`uniAgentRev`) | Manual | Nothing automated (the uni-agent suite in `ci-python` exercises it) |
| GitHub Actions commit SHAs | [`.github/workflows/*.yaml`](../../.github/workflows), [`.github/actions/*/action.yml`](../../.github/actions) | Dependabot (`github-actions`) | [`hack/ci/lint-actions-locally.sh`](../../hack/ci/lint-actions-locally.sh), org zizmor scan |
| CI Docker base image digest | [`Dockerfile.ci`](../../Dockerfile.ci) | Manual or `make update-pins` (crane digest resolution) | [`hack/ci/check-pins.py`](../../hack/ci/check-pins.py) (`@sha256:` digest check) |
| Guest container base image | [`.ko.yaml`](../../.ko.yaml) | Manual (`baseImageOverrides`) | Nothing automated |
| Generated protobuf stubs | [`proto/`](../../proto), [`clients/python/src/ate_env/_gen`](../../clients/python/src/ate_env/_gen) | `make protos` (Go stubs embed the generator versions pinned in `go.mod`; Python stubs embed the `grpcio-tools` version from the Nix env) | [`hack/verify/protos.sh`](../../hack/verify/protos.sh) (`make ci-go`) |
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

### Python client dependencies (`pip`)

- Scope: updates dependencies in [`clients/python/pyproject.toml`](../../clients/python/pyproject.toml).
- Exclusions: Dependabot ignores `ruff` because its exact patch release comes from [`flake.lock`](../../flake.lock), while `pyproject.toml` pins only a compatible range (`~=0.17.2`).
- NeMo Gym note: [`integrations/nemo-gym/pyproject.toml`](../../integrations/nemo-gym/pyproject.toml) is not managed by Dependabot because its `ate-env-client` dependency is a local relative editable path (`../../clients/python`) that Dependabot cannot resolve.
- CI execution: runs `ci-python` in `ci.yaml`, and `check-pins` and the devShell smoke checks in `ci-devenv.yaml`.
- Failure remediation: if Dependabot bumps a package to a version not yet in Nixpkgs, `check-pins` fails. Either run `make update-pins` to advance Nixpkgs or relax the version constraint in `pyproject.toml`.

### Nix flake inputs (`nix`)

- Scope: bumps flake inputs in [`flake.lock`](../../flake.lock), every two weeks.
- CI execution: runs `check-pins` and the devShell smoke checks in [`.github/workflows/ci-devenv.yaml`](../../.github/workflows/ci-devenv.yaml).
- Python stub regeneration watch-out: updating `nixpkgs` updates `grpcio-tools`. A nixpkgs bump may require `make protos` to regenerate Python stubs if header versions or code generation change. Go generator tools are decoupled from nixpkgs and pinned directly in [`go.mod`](../../go.mod) via `tool` directives. If `make ci-go` fails on `verify-protos`, pull the branch, run `make protos`, and commit the generated changes.

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

- Bumping Python minor version: change `python = pkgs.python313;` in [`nix/python.nix`](../../nix/python.nix) (for example to `pkgs.python314`). That one line moves the Nix envs and the OpenHands venv, because [`nix/ci.nix`](../../nix/ci.nix) reuses the same interpreter. Check `requires-python` in [`clients/python/pyproject.toml`](../../clients/python/pyproject.toml), [`integrations/nemo-gym/pyproject.toml`](../../integrations/nemo-gym/pyproject.toml) and [`integrations/openhands/pyproject.toml`](../../integrations/openhands/pyproject.toml). The uni-agent venv stays on `pkgs.python312` (`python312Bare` in `nix/ci.nix`) until upstream uni-agent supports a newer Python. Verify with `make check-pins`, `make ci-python`, and `make ci-openhands`.
- Adding a Python dependency: Just add the dependency to [`clients/python/pyproject.toml`](../../clients/python/pyproject.toml) or [`integrations/nemo-gym/pyproject.toml`](../../integrations/nemo-gym/pyproject.toml). [`nix/python.nix`](../../nix/python.nix) uses `pyproject-nix` to take every dependency (base and all extras) whose marker matches from Nixpkgs, leaving out with a warning anything Nixpkgs lacks, removed or marks broken (evaluation never fails on this, so `make update-pins` always runs). `make check-pins` then reports every requirement the environment does not satisfy. Dependencies on our own projects (such as `ate-env-client`) are skipped automatically, because they're imported from the checkout. If a new optional extra needs packages Nixpkgs doesn't have and the tests don't need them, add the extra to `skippedExtras` in `nix/python.nix`; `check-pins` then reports its packages as skipped, with the reason. For `integrations/uni-agent` (Python 3.12) and `integrations/openhands` (Python 3.13), which run in pip venvs (`ci-venv` and the `.#uni-agent` / `.#openhands` devShells) with PyPI dependencies not pinned by Nix, just edit that project's `pyproject.toml`: the venv cache key hashes it, so the next run rebuilds the venv. `check-pins` does not check these projects.
- Adding a CLI tool: Add any new tool required by CI scripts to `ciPackages` in [`nix/ci.nix`](../../nix/ci.nix) so that it is included in `nix develop` shells and the [`Dockerfile.ci`](../../Dockerfile.ci) image layer.
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
- Note for AI agents: Never edit [`flake.lock`](../../flake.lock) directly. Always run `make update-pins`, verify with `make check-pins`, and validate with `make ci`.

## Commands reference

- `make check-pins`: runs [`hack/ci/check-pins.py`](../../hack/ci/check-pins.py) to verify toolchains and dependency declarations against `flake.lock`, followed by `flake-checker --no-telemetry`. Automatically selects Nix or Docker via [`hack/ci-compat.sh`](../../hack/ci-compat.sh).
- `make update-pins`: updates all flake inputs, re-pins [`Dockerfile.ci`](../../Dockerfile.ci) base image digest, prints a before/after tool version table, and runs `check-pins`.
- `make update-pins ARGS=nixpkgs`: updates only the named flake input(s) and leaves the others alone (same as `nix run .#update-pins -- nixpkgs`).
- `make lint-actions-locally`: runs `actionlint` and offline `zizmor` on workflow files in [`.github/workflows`](../../.github/workflows) at medium+ severity threshold.

For detailed failure modes (Python package mismatches, Go version selection, or zizmor action tag rules), see the [Troubleshooting guide](troubleshooting.md).
