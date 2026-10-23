#!/usr/bin/env bash
#
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

# Collect atelet's "Restore timing breakdown" records for one run. atelet logs
# one JSON line per actor restore with the seconds each phase took (manifest
# fetch, download, image unpack, worker restore, total), keyed by the actor's
# name, which is the environment's name. The loader's events file has the
# same names, so `swebench-loader report RUN.jsonl -restore-log OUT` joins the
# two and splits the first-command window into atelet's phases and the rest.
#
#   benchmarking/swebench/harvest-restore-timings.sh RUN_ID OUT
#
# Every environment of a run is named RUN_ID-..., so RUN_ID selects the
# records. SINCE (default 24h) bounds how far back each atelet log is read;
# the kubelet rotates container logs, so harvest soon after the run, and
# before another large run on the same cluster. NAMESPACE defaults to
# ate-system; CONTEXT selects the kubectl context as in run-in-cluster.sh;
# PARALLEL (default 16) bounds the concurrent log reads. A pod whose log
# could not be read is reported and the script exits 1.

set -o errexit -o nounset -o pipefail

if [[ $# -ne 2 ]]; then
  echo "usage: $0 RUN_ID OUT" >&2
  exit 2
fi
RUN_ID="$1"
OUT="$2"
NS="${NAMESPACE:-ate-system}"
CONTEXT="${CONTEXT:-}"
SINCE="${SINCE:-24h}"
PARALLEL="${PARALLEL:-16}"
kc() { kubectl ${CONTEXT:+--context "${CONTEXT}"} "$@"; }

TMP="$(mktemp -d)"
trap 'rm -rf "${TMP}"' EXIT
mkdir -p "${TMP}/pods"
kc -n "${NS}" get pods -l app=atelet \
  -o jsonpath='{range .items[*]}{.metadata.name} {.status.containerStatuses[?(@.name=="atelet")].restartCount}{"\n"}{end}' > "${TMP}/list"
[[ -s "${TMP}/list" ]] || { echo "no atelet pods in ${NS}" >&2; exit 1; }

# harvest POD RESTARTS: the restore records of this run from one atelet.
# kubectl logs reads the current container only, so when atelet restarted
# the previous container's log is read too (one restart back is all the
# kubelet keeps). grep's exit status 1 (no match) is a valid empty result; a
# failed log read is not.
harvest() {
  local raw="${TMP}/pods/$1.raw"
  if ! kc -n "${NS}" logs "$1" -c atelet --since="${SINCE}" > "${raw}" 2>/dev/null; then
    echo "log read of $1 failed" >&2
    rm -f "${raw}"
    return 1
  fi
  if [[ "${2:-0}" != "0" ]]; then
    echo "atelet on $1 restarted ${2} times; reading the previous container's log too" >&2
    kc -n "${NS}" logs "$1" -c atelet --previous --since="${SINCE}" >> "${raw}" 2>/dev/null || true
  fi
  { grep -F '"Restore timing breakdown"' "${raw}" | grep -F "\"ate.actor.name\":\"${RUN_ID}-" || true; } > "${TMP}/pods/$1"
  rm -f "${raw}"
}
export -f harvest kc
export NS CONTEXT SINCE RUN_ID TMP
xargs -P "${PARALLEL}" -L1 bash -c 'harvest "$0" "$1"' < "${TMP}/list" || true

want="$(wc -l < "${TMP}/list" | tr -d ' ')"
got="$(find "${TMP}/pods" -type f ! -name '*.raw' | wc -l | tr -d ' ')"
find "${TMP}/pods" -type f ! -name '*.raw' -exec cat {} + > "${OUT}"
echo "read ${got} of ${want} atelet logs; $(wc -l < "${OUT}" | tr -d ' ') restore records for ${RUN_ID} in ${OUT}"
[[ "${got}" == "${want}" ]]
