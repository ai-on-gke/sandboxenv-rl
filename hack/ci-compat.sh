#!/usr/bin/env bash

# Copyright 2026 Google LLC
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.

# Universal runner selecting Nix or Docker container environments for reproducible local CI.
#
# What it does:
#   1. Resolves the repository root and normalizes the working directory.
#   2. Parses runner flags (--script to run a script file, e.g. a GitHub Actions `shell:`; --shell for
#      alternate devShell, --docker-socket for socket sharing, --host-network for host networking and kubeconfig mount, --write-back for diff extraction).
#   3. Selects runner engine (auto detects Nix, falls back to Docker; or nix/docker/host).
#   4. Executes the target command inside `nix develop [shell] -c` or `docker run` in sandboxenv-rl-ci:local.
#   5. In Docker mode: builds a copy of the repository tree into the image (including uncommitted
#      changes, excluding .git and .dockerignore patterns) and initializes a clean baseline git
#      commit, avoiding bind-mount ownership issues and chown workarounds.
#   6. For write-back commands (such as `update-pins` or `make protos`), extracts the container
#      git diff as a binary patch and applies it on the host as the host user.
#
# When it runs:
#   Local only. Invoked as the backend runner by Makefile targets: `make ci`, `make ci-go`,
#   `make ci-python`, `make ci-openhands`, `make ci-e2e`, `make smoke`, `make check-pins`, and `make dev-shell`.
#
# How to run:
#   - `make <target>` (preferred; auto-selects Nix or Docker via hack/ci-compat.sh)
#   - Direct: `hack/ci-compat.sh [--shell <target>] [--docker-socket] [--host-network] [--write-back] <command> [args...]`
#   - Script file: `hack/ci-compat.sh --script <file>` (runs it with `bash -euo pipefail`)
#   Examples:
#     make ci
#     CI_RUNNER=docker make ci-python
#     CI_RUNNER=nix hack/ci-compat.sh ci-go
#     hack/ci-compat.sh --docker-socket ci-e2e
#     hack/ci-compat.sh --host-network smoke
#
# Prerequisites:
#   - Nix with flakes enabled, OR Docker daemon running.
#   - Local host tools if running with CI_RUNNER=host.
#
# Environment variables:
#   CI_RUNNER      auto    Runner engine: auto (detects nix, falls back to docker), nix, docker, or host
#   CI_SHELL       (unset) Optional devShell attribute to enter (e.g. .#openhands)
#
# Outputs / exit codes:
#   - Prints `ci-compat: runner=<runner>` to stderr.
#   - Builds Docker image sandboxenv-rl-ci:local (or the devShell-free sandboxenv-rl-ci:update-pins
#     stage for update-pins) and volume sandboxenv-rl-cache if using Docker.
#   - Passes through the exit code of the executed command.
#
# Troubleshooting:
#   - See docs/chores/troubleshooting.md#3-docker-runner--local-ci-environment for
#     Docker workspace isolation, write-back patches, host network, and socket access details.
#
# See also:
#   Makefile, Dockerfile.ci, flake.nix, nix/ci.nix, README.md "Local CI" section.

set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "${REPO_ROOT}"

DOCKER_SOCKET=0
HOST_NETWORK=0
WRITE_BACK=0
CI_SHELL="${CI_SHELL:-}"
SCRIPT=""

while [[ $# -gt 0 ]]; do
  case "$1" in
    --script)
      # Run a script file with `bash -euo pipefail`, feeding it on stdin so it
      # also works in the Docker runner, which can't see files outside the
      # tree. Lets GitHub Actions steps use `shell: bash hack/ci-compat.sh --script {0}`.
      SCRIPT="$2"
      shift 2
      ;;
    --shell)
      CI_SHELL="$2"
      shift 2
      ;;
    --docker-socket)
      DOCKER_SOCKET=1
      shift
      ;;
    --host-network)
      HOST_NETWORK=1
      shift
      ;;
    --write-back)
      WRITE_BACK=1
      shift
      ;;
    *)
      break
      ;;
  esac
done

if [[ -n "${SCRIPT}" ]]; then
  if [[ $# -gt 0 ]]; then
    echo "ERROR: --script takes no command" >&2
    exit 1
  fi
  exec <"${SCRIPT}"
  set -- bash -euo pipefail -s
fi

if [[ $# -eq 0 ]]; then
  echo "Usage: $0 [--script <file> | [--shell <target>] [--docker-socket] [--host-network] [--write-back] <command> [args...]]" >&2
  exit 1
fi

CI_RUNNER="${CI_RUNNER:-auto}"

case "${CI_RUNNER}" in
  auto)
    if command -v nix >/dev/null 2>&1; then
      RUNNER="nix"
    elif command -v docker >/dev/null 2>&1; then
      RUNNER="docker"
    else
      echo "ERROR: Neither Nix nor Docker was found. Please install Nix (https://nixos.org/download) or Docker (https://docs.docker.com/get-docker/)," >&2
      echo "or set CI_RUNNER=host to use the Go and Python on your PATH (works for targets such as 'make protos'; see README \"Runners and Environment\")." >&2
      exit 1
    fi
    ;;
  nix)
    RUNNER="nix"
    ;;
  docker)
    RUNNER="docker"
    ;;
  host)
    RUNNER="host"
    ;;
  *)
    echo "ERROR: Invalid CI_RUNNER='${CI_RUNNER}': expected auto, nix, docker, or host" >&2
    exit 1
    ;;
esac

echo "ci-compat: runner=${RUNNER}" >&2

if [[ "${RUNNER}" == "host" ]]; then
  if ! command -v "$1" >/dev/null 2>&1; then
    echo "ERROR: '$1' is not on PATH. It is one of the scripts nix/ci.nix provides, so CI_RUNNER=host" >&2
    echo "only finds it inside the devShell ('nix develop'). Use CI_RUNNER=nix or docker instead." >&2
    exit 127
  fi
  exec "$@"
fi

if [[ "${RUNNER}" == "nix" ]]; then
  if [[ "$1" == "update-pins" ]]; then
    exec nix run .#update-pins -- "${@:2}"
  fi
  if [[ -n "${CI_SHELL}" ]]; then
    exec nix develop "${CI_SHELL}" -c "$@"
  fi
  exec nix develop -c "$@"
fi

if [[ "${RUNNER}" == "docker" ]]; then
  # update-pins gets an image without the devShell (Dockerfile.ci's
  # `update-pins` stage), so it still works when the devShell doesn't evaluate.
  if [[ "${1:-}" == "update-pins" ]]; then
    DOCKER_IMAGE="sandboxenv-rl-ci:update-pins"
    docker build -q -f Dockerfile.ci --target update-pins -t "${DOCKER_IMAGE}" . >/dev/null
  else
    DOCKER_IMAGE="sandboxenv-rl-ci:local"
    docker build -q -f Dockerfile.ci -t "${DOCKER_IMAGE}" . >/dev/null
  fi

  DOCKER_RUN_ARGS=(
    run --rm
    -v sandboxenv-rl-cache:/root/.cache
  )
  if [[ "${DOCKER_SOCKET}" -eq 1 ]]; then
    DOCKER_RUN_ARGS+=(
      -v /var/run/docker.sock:/var/run/docker.sock
    )
  fi
  if [[ "${DOCKER_SOCKET}" -eq 1 || "${HOST_NETWORK}" -eq 1 ]]; then
    DOCKER_RUN_ARGS+=(
      --network host
    )
    # Both modes talk to a cluster through the host's kubeconfig: smoke against
    # an existing cluster, and ci-e2e with REUSE_CLUSTER. Only a single file can
    # be mounted; a colon-separated KUBECONFIG list is skipped with a warning.
    host_kubeconfig="${KUBECONFIG:-${HOME:-}/.kube/config}"
    if [[ "${host_kubeconfig}" == *:* ]]; then
      echo "ci-compat: KUBECONFIG lists several files; not mounting it into the container" >&2
    elif [[ -n "${host_kubeconfig}" && -f "${host_kubeconfig}" ]]; then
      DOCKER_RUN_ARGS+=(
        -v "${host_kubeconfig}:/root/.kube/config:ro"
        -e KUBECONFIG=/root/.kube/config
      )
    fi
  fi

  is_write_back_cmd() {
    if [[ "${WRITE_BACK}" -eq 1 ]]; then
      return 0
    fi
    if [[ "${1:-}" == "update-pins" ]]; then
      return 0
    fi
    # `make protos` invokes: sh -c 'make python-protos && go tool buf generate'
    if [[ "${1:-}" == "sh" && "${2:-}" == "-c" && "${3:-}" == *"make python-protos && go tool buf generate"* ]]; then
      return 0
    fi
    if [[ "${1:-}" == "protos" ]]; then
      return 0
    fi
    return 1
  }

  DOCKER_TARGET_SHELL="path:///workspace"
  if [[ -n "${CI_SHELL}" ]]; then
    target_attr="${CI_SHELL#.#}"
    target_attr="${target_attr#\#}"
    DOCKER_TARGET_SHELL="path:///workspace#${target_attr}"
  fi

  if ! is_write_back_cmd "$@"; then
    DOCKER_TTY_FLAG=("-i")
    if [[ -t 0 ]]; then
      DOCKER_TTY_FLAG=("-it")
    fi
    exec docker "${DOCKER_RUN_ARGS[@]}" "${DOCKER_TTY_FLAG[@]}" \
      --entrypoint nix \
      "${DOCKER_IMAGE}" develop "${DOCKER_TARGET_SHELL}" -c "$@"
  fi

  # Write-back execution: execute the command in the container with stdout/stderr
  # redirected to stderr so stdout carries only the git diff patch stream.
  #
  # Why: Applying the container's git diff on the host as the invoking user
  # sidesteps system/OS-specific uid/gid mapping behaviour of Docker (root-owned
  # files on Linux bind mounts, differing remapping in rootless Docker/Podman and
  # Docker Desktop on macOS) when running code generation tools inside the container.
  patch_file="$(mktemp -t sandboxenv-rl-patch-XXXXXX.patch)"
  # shellcheck disable=SC2329 # invoked indirectly via trap
  cleanup_patch() {
    rm -f "${patch_file}"
  }
  trap cleanup_patch EXIT INT TERM

  set +e
  docker "${DOCKER_RUN_ARGS[@]}" \
    --entrypoint sh \
    "${DOCKER_IMAGE}" \
    -c '
      if [ "$1" = "update-pins" ]; then
        shift
        nix run path:///workspace#update-pins -- "$@" >&2
      else
        nix develop "'"${DOCKER_TARGET_SHELL}"'" -c "$@" >&2
      fi
      rc=$?
      if [ $rc -ne 0 ]; then
        exit $rc
      fi
      git add -A
      git diff --binary --cached
    ' -- "$@" > "${patch_file}"
  cmd_rc=$?
  set -e

  if [[ $cmd_rc -ne 0 ]]; then
    exit $cmd_rc
  fi

  if [[ -s "${patch_file}" ]]; then
    if ! git apply "${patch_file}"; then
      echo "ERROR: Failed to apply container changes cleanly to the host working tree." >&2
      echo "The patch has been saved to: ${patch_file}" >&2
      trap - EXIT INT TERM
      exit 1
    fi
  fi

  exit 0
fi
