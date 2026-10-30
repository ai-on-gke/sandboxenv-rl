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

# Resolves, clones, and aligns a local checkout of agent-substrate/substrate.
#
# What it does:
#   1. Inspects the Go module graph using `go list -m -json go.ate.dev/substrate`.
#   2. If a local directory override is present (via go.work or a replace directive),
#      prints the absolute directory and exits 0 immediately without hash verification.
#   3. Otherwise, extracts the pinned revision of go.ate.dev/substrate from the module graph.
#   4. Resolves the full 40-character commit hash via `go mod download -json`.
#   5. Atomically clones agent-substrate/substrate bloblessly (--filter=blob:none) into a temporary
#      directory next to the cache directory (${CACHE_DIR}.tmp.XXXXXX).
#   6. Fetches tags from origin if the commit is not present, failing if the commit is unreachable.
#   7. Checks out the detached commit and strictly verifies HEAD matches the full expected hash.
#   8. Atomically moves the verified clone into CACHE_DIR and outputs the absolute substrate path to stdout.
#
# When it runs:
#   Invoked dynamically by hack/ci/e2e.py and nix/ci.nix to locate or clone Substrate.
#   Flake app: `nix run .#ensure-substrate`.
#
# How to run:
#   - `nix run .#ensure-substrate` / `nix develop -c ensure-substrate`
#   - Direct: `./hack/ci/ensure-substrate.sh`
#   Examples:
#     ./hack/ci/ensure-substrate.sh
#     go work init . ../substrate && ./hack/ci/ensure-substrate.sh
#
# Prerequisites:
#   - git, go, and jq on PATH.
#   - Network access to github.com/agent-substrate/substrate if not already cached.
#
# Environment variables:
#   XDG_CACHE_HOME   ${HOME}/.cache  Root cache directory for persistent Substrate clones
#   GOWORK           (unset)         Optional path to go.work workspace (or 'off')
#
# Outputs / exit codes:
#   - Prints the absolute path of the substrate directory to stdout.
#   - Informational status and git progress printed to stderr.
#   - Exits with 0 on success; exits with 1 if module resolution or git commands fail.
#
# Troubleshooting:
#   - See docs/chores/troubleshooting.md#substrate-module-resolution-and-git-fetches
#     for atomic cache population, tag resolution, and local go.work overrides.
#
# See also:
#   hack/ci/e2e.py, hack/ci/check-substrate-head.sh, go.mod, nix/ci.nix.

set -euo pipefail

REPO_ROOT="$(git rev-parse --show-toplevel 2>/dev/null || pwd)"
cd "${REPO_ROOT}"

# Warn if GOFLAGS contains -mod=vendor
if [[ "${GOFLAGS:-}" =~ -mod=vendor ]]; then
  echo "WARNING: GOFLAGS contains -mod=vendor, which disables go.work and may fail if vendor/ is missing." >&2
fi

# Resolve Substrate through the Go module graph (honours go.work, GOWORK, replace)
MODULE_JSON="$(go list -m -json go.ate.dev/substrate 2>/dev/null || true)"
if [[ -z "${MODULE_JSON}" ]]; then
  echo "ERROR: Could not resolve module go.ate.dev/substrate via 'go list -m -json'" >&2
  exit 1
fi

REPLACE_DIR="$(echo "${MODULE_JSON}" | jq -r '.Replace.Dir // empty')"
REPLACE_VER="$(echo "${MODULE_JSON}" | jq -r '.Replace.Version // empty')"
REPLACE_PATH="$(echo "${MODULE_JSON}" | jq -r '.Replace.Path // empty')"
IS_MAIN="$(echo "${MODULE_JSON}" | jq -r '.Main // false')"
MOD_DIR="$(echo "${MODULE_JSON}" | jq -r '.Dir // empty')"
MOD_VER="$(echo "${MODULE_JSON}" | jq -r '.Version // empty')"

OVERRIDE_DIR=""
if [[ -n "${REPLACE_DIR}" && -z "${REPLACE_VER}" ]]; then
  OVERRIDE_DIR="${REPLACE_DIR}"
elif [[ -n "${REPLACE_PATH}" && -n "${REPLACE_VER}" ]]; then
  echo "ERROR: Replace directive for go.ate.dev/substrate points to a module (${REPLACE_PATH}@${REPLACE_VER}); only local directory overrides are supported." >&2
  exit 1
elif [[ "${IS_MAIN}" == "true" && -n "${MOD_DIR}" ]]; then
  OVERRIDE_DIR="${MOD_DIR}"
fi

if [[ -n "${OVERRIDE_DIR}" ]]; then
  OVERRIDE_DIR="$(cd "${OVERRIDE_DIR}" 2>/dev/null && pwd || echo "${OVERRIDE_DIR}")"
  if [[ ! -d "${OVERRIDE_DIR}" ]]; then
    echo "ERROR: Local override directory does not exist: ${OVERRIDE_DIR}" >&2
    exit 1
  fi
  GOWORK_IN_EFFECT="$(go env GOWORK)"
  if [[ -n "${GOWORK_IN_EFFECT}" && "${GOWORK_IN_EFFECT}" != "off" ]]; then
    echo "==> Using local override from go.work (${GOWORK_IN_EFFECT}): ${OVERRIDE_DIR}" >&2
  else
    echo "==> Using local override from go.work/replace: ${OVERRIDE_DIR}" >&2
  fi
  echo "${OVERRIDE_DIR}"
  exit 0
fi

# No override: strictly verify the pinned version
SUBSTRATE_MOD_VER="${MOD_VER}"
if [[ -z "${SUBSTRATE_MOD_VER}" ]]; then
  echo "ERROR: go.ate.dev/substrate requirement not found in module graph" >&2
  exit 1
fi

EXPECTED_HASH="$(go mod download -json "go.ate.dev/substrate@${SUBSTRATE_MOD_VER}" | jq -r '.Origin.Hash // empty')"
if [[ -z "${EXPECTED_HASH}" ]]; then
  echo "ERROR: Could not resolve Origin.Hash from 'go mod download -json go.ate.dev/substrate@${SUBSTRATE_MOD_VER}'" >&2
  exit 1
fi

CHECKOUT_REF=""
if [[ "${SUBSTRATE_MOD_VER}" =~ -([0-9a-f]{12})$ ]]; then
  CHECKOUT_REF="${BASH_REMATCH[1]}"
elif [[ "${SUBSTRATE_MOD_VER}" =~ ^v[0-9]+ ]]; then
  CHECKOUT_REF="${SUBSTRATE_MOD_VER}"
else
  echo "ERROR: Could not parse substrate revision from version '${SUBSTRATE_MOD_VER}'" >&2
  exit 1
fi

XDG_CACHE_HOME="${XDG_CACHE_HOME:-${HOME:-/tmp}/.cache}"
export XDG_CACHE_HOME
mkdir -p "${XDG_CACHE_HOME}"

clone_substrate() {
  local target_dir="$1"
  echo "==> Cloning agent-substrate/substrate into ${target_dir}..." >&2
  mkdir -p "$(dirname "${target_dir}")"
  git clone --filter=blob:none --no-checkout https://github.com/agent-substrate/substrate.git "${target_dir}" >&2
}

checkout_ref() {
  local target_dir="$1"
  local ref="$2"
  echo "==> Aligning substrate in ${target_dir} to go.mod pin: ${SUBSTRATE_MOD_VER} (${ref})..." >&2
  if ! git -C "${target_dir}" cat-file -e "${ref}^{commit}" 2>/dev/null; then
    git -C "${target_dir}" fetch --filter=blob:none --tags origin >&2
  fi
  if ! git -C "${target_dir}" cat-file -e "${ref}^{commit}" 2>/dev/null; then
    echo "ERROR: Commit ${ref} for substrate version ${SUBSTRATE_MOD_VER} is not reachable from origin's branches or tags." >&2
    exit 1
  fi
  git -C "${target_dir}" checkout --detach "${ref}" >&2
}

verify_checkout() {
  local target_dir="$1"
  local actual_head
  actual_head="$(git -C "${target_dir}" rev-parse HEAD 2>/dev/null || echo "unknown")"

  if [[ "${actual_head}" != "${EXPECTED_HASH}" ]]; then
    echo "ERROR: Substrate checkout at ${target_dir} is at ${actual_head}, expected ${EXPECTED_HASH} (for ${SUBSTRATE_MOD_VER})." >&2
    echo "To fix: delete the directory (${target_dir}) or configure a local go.work/replace override." >&2
    exit 1
  fi
}

CACHE_DIR="${XDG_CACHE_HOME}/sandboxenv-rl/substrate-${CHECKOUT_REF}"

if [[ ! -d "${CACHE_DIR}/.git" ]]; then
  mkdir -p "$(dirname "${CACHE_DIR}")"
  if [[ -d "${CACHE_DIR}" ]]; then
    rm -rf "${CACHE_DIR}"
  fi

  TMP_CLONE="$(mktemp -d "${CACHE_DIR}.tmp.XXXXXX")"
  # shellcheck disable=SC2329 # invoked indirectly via trap
  cleanup_tmp() {
    rm -rf "${TMP_CLONE}"
  }
  trap cleanup_tmp EXIT

  clone_substrate "${TMP_CLONE}"
  checkout_ref "${TMP_CLONE}" "${EXPECTED_HASH}"
  verify_checkout "${TMP_CLONE}"

  mv -n -T "${TMP_CLONE}" "${CACHE_DIR}" 2>/dev/null || true
  if [[ -d "${TMP_CLONE}" ]]; then
    rm -rf "${TMP_CLONE}"
  fi
  trap - EXIT
fi

verify_checkout "${CACHE_DIR}"
echo "${CACHE_DIR}"
