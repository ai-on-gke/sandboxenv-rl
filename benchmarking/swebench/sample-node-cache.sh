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

# Sample the image-cache counters of every atelet in the cluster. atelet is a
# distroless image with no shell to exec into, but it serves Prometheus
# metrics on :9090, and the API server's pod proxy reaches that port with
# kubectl alone.
#
#   benchmarking/swebench/sample-node-cache.sh OUT
#
# OUT gets one line per series: NODE, a tab, then the Prometheus text line,
# for ate_imagecache_requests_total and process_start_time_seconds, plus a
# "# sampled <unix seconds>" line per node. Take one sample before a run and
# one after; `swebench-loader report RUN.jsonl -cache-before BEFORE
# -cache-after AFTER` turns the difference into the run's cold-pull count.
# Reading the metrics needs RBAC get on pods/proxy in NAMESPACE. NAMESPACE defaults to ate-system; CONTEXT selects
# the kubectl context as in run-in-cluster.sh; PARALLEL (default 16) bounds
# the concurrent scrapes. A node whose scrape fails is reported and left out,
# and the script exits 1 so the gap is not missed.

set -o errexit -o nounset -o pipefail

OUT="${1:?usage: $0 OUT}"
NS="${NAMESPACE:-ate-system}"
CONTEXT="${CONTEXT:-}"
PARALLEL="${PARALLEL:-16}"
kc() { kubectl ${CONTEXT:+--context "${CONTEXT}"} "$@"; }

TMP="$(mktemp -d)"
trap 'rm -rf "${TMP}"' EXIT
mkdir -p "${TMP}/nodes"
kc -n "${NS}" get pods -l app=atelet \
  -o jsonpath='{range .items[?(@.status.phase=="Running")]}{.metadata.name} {.spec.nodeName}{"\n"}{end}' > "${TMP}/pods"
[[ -s "${TMP}/pods" ]] || { echo "no running atelet pods in ${NS}" >&2; exit 1; }

# sample POD NODE: fetch the pod's metrics and keep the cache families. A
# node with no lookups yet has no series, which is a valid (empty) sample.
sample() {
  local raw="${TMP}/nodes/$2.raw"
  if ! kc -n "${NS}" get --raw "/api/v1/namespaces/${NS}/pods/$1:9090/proxy/metrics" > "${raw}" 2>/dev/null; then
    echo "scrape of $1 on $2 failed" >&2
    rm -f "${raw}"
    return 1
  fi
  # The first line names the node and stamps the sample time even when the
  # node has no series yet, so the report can tell "no lookups" from "not
  # sampled". process_start_time_seconds lets it tell a restarted or newly
  # added atelet from one whose counters merely grew.
  {
    printf '%s\t# sampled %s\n' "$2" "$(date +%s)"
    { grep -E '^(ate_imagecache_requests_total\{|process_start_time_seconds)' "${raw}" || true; } | sed "s/^/$2\t/"
  } > "${TMP}/nodes/$2"
  rm -f "${raw}"
}
export -f sample kc
export NS CONTEXT TMP
# xargs reports a nonzero status when any sample failed; the count below says which.
xargs -P "${PARALLEL}" -L1 bash -c 'sample "$0" "$1"' < "${TMP}/pods" || true

want="$(wc -l < "${TMP}/pods" | tr -d ' ')"
got="$(find "${TMP}/nodes" -type f ! -name '*.raw' | wc -l | tr -d ' ')"
find "${TMP}/nodes" -type f ! -name '*.raw' -exec cat {} + | sort > "${OUT}"
echo "sampled ${got} of ${want} atelet nodes into ${OUT}"
[[ "${got}" == "${want}" ]]
