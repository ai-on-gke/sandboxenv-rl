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

# Verifies sandboxenv-rl build, vet, and race tests against substrate@HEAD or a custom ref/dir.
#
# What it does:
#   1. Backs up go.mod, go.sum, and any active go.work to a temporary directory.
#   2. Installs an EXIT trap ensuring original module and workspace files are restored on exit.
#   3. For local directories: initializes a temporary go.work pointing to the local checkout.
#   4. For remote references: updates go.mod via `go get go.ate.dev/substrate@<ref>` (with GOWORK=off).
#   5. Executes `go build ./...`, `go vet ./...`, and `go test -race ./...`.
#   6. Restores original go.mod, go.sum, and workspace state cleanly.
#
# When it runs:
#   Local only / manual compatibility verification. Wrapped by `make check-substrate-head`
#   and flake app `nix run .#check-substrate-head`.
#
# How to run:
#   - `make check-substrate-head` (preferred; auto-selects Nix or Docker)
#   - `nix run .#check-substrate-head -- [args]`
#   - Direct: `./hack/ci/check-substrate-head.sh [PATH_OR_REF]`
#   Examples:
#     make check-substrate-head
#     make check-substrate-head ARGS=main
#     make check-substrate-head ARGS=../substrate
#     ./hack/ci/check-substrate-head.sh /path/to/substrate
#
# Prerequisites:
#   - Go toolchain (matching go.mod / Nix devShell).
#   - Network access to go.ate.dev / GitHub (if testing remote ref) or local substrate directory.
#
# Environment variables:
#   GOWORK  (unset)  Optional path to go.work workspace (overridden internally during test)
#
# Outputs / exit codes:
#   - Outputs go build, vet, and test logs to stdout/stderr.
#   - Side effects: temporary go.work or go.mod change (always restored before exit).
#   - Exits with code 0 on success, or the failure code of the failing go step.
#
# Troubleshooting:
#   - See docs/chores/troubleshooting.md for working tree restoration guarantees
#     and Substrate breaking API change analysis.
#
# See also:
#   hack/ci/ensure-substrate.sh, Makefile, go.mod.

set -euo pipefail

if [[ "${1:-}" == "-h" || "${1:-}" == "--help" ]]; then
  echo "Usage: $0 [PATH_OR_REF]"
  echo "Verifies sandboxenv-rl compatibility against substrate@HEAD (or specified path/ref)."
  echo "Defaults to checking remote go.ate.dev/substrate@main."
  exit 0
fi

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/../.." && pwd)"

cd "${REPO_ROOT}"

# Preserve go.mod, go.sum, and any existing go.work on exit so runs do not dirty the workspace
TMP_DIR="$(mktemp -d)"
cp go.mod "${TMP_DIR}/go.mod"
cp go.sum "${TMP_DIR}/go.sum"

HAD_GOWORK=0
if [[ -f go.work ]]; then
  cp go.work "${TMP_DIR}/go.work"
  HAD_GOWORK=1
  if [[ -f go.work.sum ]]; then
    cp go.work.sum "${TMP_DIR}/go.work.sum"
  fi
fi

cleanup() {
  local exit_code=$?
  echo "==> Restoring original go.mod, go.sum, and workspace state..."
  cp "${TMP_DIR}/go.mod" go.mod
  cp "${TMP_DIR}/go.sum" go.sum
  if [[ "${HAD_GOWORK}" == "1" ]]; then
    cp "${TMP_DIR}/go.work" go.work
    if [[ -f "${TMP_DIR}/go.work.sum" ]]; then
      cp "${TMP_DIR}/go.work.sum" go.work.sum
    else
      rm -f go.work.sum
    fi
  else
    rm -f go.work go.work.sum
  fi
  rm -rf "${TMP_DIR}"
  exit "${exit_code}"
}
trap cleanup EXIT

SUBSTRATE_TARGET="${1:-}"

if [[ -n "${SUBSTRATE_TARGET}" && -d "${SUBSTRATE_TARGET}" ]]; then
  TARGET_PATH="$(cd "${SUBSTRATE_TARGET}" && pwd)"
  COMMIT_INFO="$(git -C "${TARGET_PATH}" rev-parse --short HEAD 2>/dev/null || echo "local")"
  echo "==> Checking against local substrate checkout at ${TARGET_PATH} (commit: ${COMMIT_INFO})..."
  # Use a temporary go.work workspace instead of editing go.mod directly
  rm -f go.work go.work.sum
  go work init . "${TARGET_PATH}"
else
  TARGET_REF="${SUBSTRATE_TARGET:-main}"
  echo "==> Checking against remote substrate@${TARGET_REF}..."
  # Disable workspace mode so go get updates go.mod directly
  export GOWORK=off
  go get "go.ate.dev/substrate@${TARGET_REF}"
fi

echo "==> Running go build..."
go build ./...

echo "==> Running go vet..."
go vet ./...

echo "==> Running go test (-race)..."
go test -race ./...

echo "==> Substrate HEAD compatibility check passed!"
