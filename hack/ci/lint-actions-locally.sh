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

# Lints GitHub Actions workflow definitions locally using actionlint and zizmor.
#
# What it does:
#   1. Resolves repository root and navigates to it.
#   2. Runs actionlint on all .github/workflows/*.y*ml files to validate syntax and expressions.
#   3. Runs zizmor in offline security audit mode (--offline) for medium+ severity issues.
#   4. Emits remediation advice and exit status based on audit findings.
#
# When it runs:
#   Local only. Intended as a pre-commit / pre-push verification step for workflow edits.
#   Wrapped by `make lint-actions-locally` and flake app `nix run .#lint-actions-locally`.
#
# How to run:
#   - `make lint-actions-locally` (preferred; auto-selects Nix or Docker)
#   - `nix run .#lint-actions-locally` / `nix develop -c lint-actions-locally`
#   - Direct: `./hack/ci/lint-actions-locally.sh`
#   Examples:
#     make lint-actions-locally
#     CI_RUNNER=docker make lint-actions-locally
#     ./hack/ci/lint-actions-locally.sh
#
# Prerequisites:
#   - actionlint and zizmor installed on PATH (provided automatically by Nix devShell and Docker).
#
# Environment variables:
#   None read directly.
#
# Outputs / exit codes:
#   - Prints actionlint syntax errors and zizmor security findings to stdout/stderr.
#   - Exits with 0 if all workflows pass; non-zero if actionlint or zizmor reports issues.
#
# Troubleshooting:
#   - Offline audit scope: Local runs use zizmor `--offline`, which skips online GitHub API checks
#     such as ref-version-mismatch and impostor-commit. The organization-required scan covers them
#     on PRs.
#   - False positives or accepted risks: Consult https://docs.zizmor.sh/audits/ for remediation or
#     https://docs.zizmor.sh/usage/#ignoring-results to configure inline ignore annotations.
#
# See also:
#   .github/workflows/ci.yaml, .github/workflows/ci-devenv.yaml, Makefile, nix/ci.nix.

set -euo pipefail

REPO_ROOT="$(git rev-parse --show-toplevel 2>/dev/null || pwd)"
cd "${REPO_ROOT}"

echo "==> Running actionlint on .github/workflows..."
# shellcheck disable=SC2086
actionlint -ignore 'SC2016' .github/workflows/*.y*ml

echo "==> Running zizmor security audit locally (--offline)..."
echo "Note: Online audits (e.g. ref-version-mismatch, impostor-commit) are covered by the org's required GitHub Actions scan on PRs."
set +e
zizmor --offline --min-severity medium --min-confidence medium --format plain .github/workflows .github/actions
ZIZMOR_RC=$?
set -e

if [[ $ZIZMOR_RC -ne 0 ]]; then
  echo "" >&2
  echo "========================================================================" >&2
  echo "zizmor audit failed! How to fix:" >&2
  echo "  - Audit rule docs & remediations: https://docs.zizmor.sh/audits/" >&2
  echo "  - Ignoring false positives or accepted risks: https://docs.zizmor.sh/usage/#ignoring-results" >&2
  echo "========================================================================" >&2
  exit "$ZIZMOR_RC"
fi

echo "==> All local workflow checks passed!"
