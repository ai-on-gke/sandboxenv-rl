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

# Run swebench-loader inside the cluster, next to ate-env-api, and bring the
# events file back. The dataset, offline-exclusions.txt and any -trajectory
# file (with the files next to it) are baked into the loader image (ko's
# kodata directory), so the pod needs no bucket access; a
# sidecar keeps the pod alive after the loader exits so the results can be
# copied out with kubectl cp. The Job is deleted when the script exits, on
# success or failure.
#
#   KO_DOCKER_REPO=gcr.io/PROJECT/repo \
#   benchmarking/swebench/run-in-cluster.sh RUN_ID DATASET.jsonl [swebench-loader run flags...]
#
# RUN_ID must be a DNS label (lowercase letters, digits, '-', at most 20
# characters). Results land in ./RUN_ID.jsonl and ./RUN_ID.json in the
# directory the script is run from, which can be outside the repository.
# NAMESPACE defaults to ate-env; CONTEXT selects the kubectl context (default
# the current one), so one shell can drive the same run on several clusters
# in turn: put the cluster in RUN_ID and compare the events files with
# `swebench-loader report a.jsonl b.jsonl`. START_TIMEOUT (seconds, default
# 600) bounds the wait for the pod to start. A failed kubectl call while
# waiting for the loader's exit code is retried; only a pod that is gone
# (NotFound) or Failed ends the wait early. If the script is killed before
# its trap runs, the Job ends by itself after 48 hours (activeDeadlineSeconds)
# and is removed an hour after it finishes; Ctrl-C runs the trap and deletes
# the Job at once, results included.
# AUTH_TOKEN_SECRET names a Secret in NAMESPACE whose key `token` holds
# ate-env-api's bearer token; the loader then reads it from
# SUBSTRATE_ENV_AUTH_TOKEN. Leave it unset for an API without a token.

set -o errexit -o nounset -o pipefail

if [[ $# -lt 2 ]]; then
  echo "usage: $0 RUN_ID DATASET.jsonl [swebench-loader run flags...]" >&2
  exit 2
fi
RUN_ID="$1"
DATASET="$(cd "$(dirname "$2")" && pwd)/$(basename "$2")"
shift 2
NS="${NAMESPACE:-ate-env}"
CONTEXT="${CONTEXT:-}"
START_TIMEOUT="${START_TIMEOUT:-600}"
# Every kubectl call goes through kc so CONTEXT applies to all of them. The
# context in use is logged so each run's log names its cluster.
kc() { kubectl ${CONTEXT:+--context "${CONTEXT}"} "$@"; }
echo "kubectl context ${CONTEXT:-$(kubectl config current-context 2>/dev/null || echo unknown)}"
: "${KO_DOCKER_REPO:?set KO_DOCKER_REPO to the registry path the cluster can pull from}"
[[ "${RUN_ID}" =~ ^[a-z0-9]([-a-z0-9]*[a-z0-9])?$ && ${#RUN_ID} -le 20 ]] || { echo "RUN_ID must be a DNS label of at most 20 characters" >&2; exit 2; }
[[ -f "${DATASET}" ]] || { echo "dataset ${DATASET} not found" >&2; exit 2; }
AUTH_TOKEN_SECRET="${AUTH_TOKEN_SECRET:-}"
# Checked as a DNS subdomain name, so the value cannot break the manifest.
[[ -z "${AUTH_TOKEN_SECRET}" || ( "${AUTH_TOKEN_SECRET}" =~ ^[a-z0-9]([-a-z0-9.]*[a-z0-9])?$ && ${#AUTH_TOKEN_SECRET} -le 253 ) ]] || { echo "AUTH_TOKEN_SECRET must be a Secret name" >&2; exit 2; }
LOADER_ENV=""
if [[ -n "${AUTH_TOKEN_SECRET}" ]]; then
  LOADER_ENV="env:
            - name: SUBSTRATE_ENV_AUTH_TOKEN
              valueFrom:
                secretKeyRef:
                  name: ${AUTH_TOKEN_SECRET}
                  key: token"
fi
command -v python3 >/dev/null || { echo "python3 is needed to encode the loader arguments" >&2; exit 2; }
OUT_DIR="$(pwd)"

# The repository is found from the script's own location, so the script can
# run from any directory and the results land where it was invoked.
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
cd "${ROOT}"
KODATA=cmd/benchmarking/swebench-loader/kodata
rm -rf "${KODATA}" && mkdir -p "${KODATA}"
trap 'rm -rf "${KODATA}"' EXIT
cp "${DATASET}" "${KODATA}/dataset.jsonl"
cp benchmarking/swebench/offline-exclusions.txt "${KODATA}/offline-exclusions.txt"

# A -trajectory value that names a file on this machine, by an absolute path
# or one relative to the directory the script was invoked in, is baked into
# the image too, with the files its write steps name (a grader script next
# to its trajectory) and nothing else from its directory, and the flag is
# rewritten to the baked path. Built-in names pass through. A comma-separated
# mix is handled element by element. The result is left in BAKED, not
# printed, so the counter survives.
TRAJ_N=0
BAKED=""
bake_trajectory() {
  local -a parts out=()
  local el path dir base asset
  IFS=',' read -r -a parts <<< "$1"
  for el in "${parts[@]}"; do
    path="${el}"
    [[ "${path}" = /* ]] || path="${OUT_DIR}/${path}"
    if [[ -f "${path}" ]]; then
      dir="$(cd "$(dirname "${path}")" && pwd)"
      base="$(basename "${path}")"
      TRAJ_N=$((TRAJ_N + 1))
      mkdir -p "${KODATA}/traj/${TRAJ_N}"
      cp "${dir}/${base}" "${KODATA}/traj/${TRAJ_N}/"
      while IFS= read -r asset; do
        [[ -n "${asset}" ]] || continue
        mkdir -p "$(dirname "${KODATA}/traj/${TRAJ_N}/${asset}")"
        cp "${dir}/${asset}" "${KODATA}/traj/${TRAJ_N}/${asset}"
      done < <(sed -n 's/^[[:space:]]*file:[[:space:]]*//p' "${dir}/${base}" | tr -d "\"'")
      el="/var/run/ko/traj/${TRAJ_N}/${base}"
    fi
    out+=("${el}")
  done
  local IFS=','
  BAKED="${out[*]}"
}
ARGS=()
while (( $# )); do
  case "$1" in
    -trajectory|--trajectory)
      [[ $# -ge 2 ]] || { echo "$1 needs a value" >&2; exit 2; }
      bake_trajectory "$2"; ARGS+=("$1" "${BAKED}"); shift 2 ;;
    -trajectory=*|--trajectory=*)
      bake_trajectory "${1#*=}"; ARGS+=("${1%%=*}=${BAKED}"); shift ;;
    *)
      ARGS+=("$1"); shift ;;
  esac
done
IMAGE="$(KO_DOCKER_REPO="${KO_DOCKER_REPO}/swebench-loader" ko build --bare ./cmd/benchmarking/swebench-loader)"
rm -rf "${KODATA}"
echo "loader image ${IMAGE}"

JOB="swebench-loader-${RUN_ID}"
# Every argument is encoded as a JSON string, so quotes and other YAML
# metacharacters in a flag value cannot break the manifest.
ARGS_JSON="$(python3 -c 'import json, sys; print(json.dumps(sys.argv[1:]))' \
  run "-endpoint=ate-env-api.${NS}.svc:7777" "-dataset=/var/run/ko/dataset.jsonl" \
  "-exclude-file=/var/run/ko/offline-exclusions.txt" "-run-id=${RUN_ID}" "-out=/work/${RUN_ID}.jsonl" ${ARGS[@]+"${ARGS[@]}"})"

KUBECTL_ERR="$(mktemp)"
# shellcheck disable=SC2329 # invoked by the EXIT trap
cleanup() {
  kc -n "${NS}" delete job "${JOB}" --ignore-not-found --wait=false >/dev/null 2>&1 || true
  rm -rf "${KODATA}"
  rm -f "${KUBECTL_ERR}"
}
trap cleanup EXIT

kc -n "${NS}" delete job "${JOB}" --ignore-not-found --wait=true >/dev/null
# busybox 1.36, pinned by digest; Docker Hub rate-limits unauthenticated pulls.
cat <<EOF_JOB | kc apply -f -
apiVersion: batch/v1
kind: Job
metadata:
  name: ${JOB}
  namespace: ${NS}
spec:
  backoffLimit: 0
  # Bounds for a Job whose script died before its trap ran: the keep sidecar
  # sleeps 48 hours, and a finished Job is removed after an hour.
  activeDeadlineSeconds: 172800
  ttlSecondsAfterFinished: 3600
  template:
    metadata:
      labels:
        app: swebench-loader
    spec:
      restartPolicy: Never
      automountServiceAccountToken: false
      securityContext:
        runAsNonRoot: true
        runAsUser: 65532
        seccompProfile:
          type: RuntimeDefault
      volumes:
        - name: work
          emptyDir: {}
      containers:
        - name: loader
          image: ${IMAGE}
          args: ${ARGS_JSON}
          ${LOADER_ENV}
          securityContext:
            allowPrivilegeEscalation: false
            capabilities:
              drop: ["ALL"]
          volumeMounts:
            - name: work
              mountPath: /work
          resources:
            requests:
              cpu: "2"
              memory: 2Gi
            limits:
              memory: 4Gi
        - name: keep
          image: busybox:1.36@sha256:73aaf090f3d85aa34ee199857f03fa3a95c8ede2ffd4cc2cdb5b94e566b11662
          command: ["sh", "-c", "sleep 172800"]
          securityContext:
            allowPrivilegeEscalation: false
            capabilities:
              drop: ["ALL"]
          volumeMounts:
            - name: work
              mountPath: /work
          resources:
            requests:
              cpu: 10m
              memory: 16Mi
            limits:
              memory: 64Mi
EOF_JOB

echo "waiting up to ${START_TIMEOUT}s for the pod"
POD=""
deadline=$((SECONDS + START_TIMEOUT))
while (( SECONDS < deadline )); do
  POD="$(kc -n "${NS}" get pod -l "job-name=${JOB}" -o jsonpath='{.items[0].metadata.name}' 2>/dev/null || true)"
  if [[ -n "${POD}" ]] && kc -n "${NS}" get pod "${POD}" -o jsonpath='{.status.containerStatuses[?(@.name=="loader")].state}' 2>/dev/null | grep -q 'running\|terminated'; then
    break
  fi
  sleep 5
done
if [[ -z "${POD}" ]] || ! kc -n "${NS}" get pod "${POD}" -o jsonpath='{.status.containerStatuses[?(@.name=="loader")].state}' 2>/dev/null | grep -q 'running\|terminated'; then
  echo "loader did not start within ${START_TIMEOUT}s" >&2
  if [[ -n "${POD}" ]]; then
    kc -n "${NS}" describe pod "${POD}" | tail -15 >&2 || true
  else
    echo "no pod was created for job ${JOB}; its events:" >&2
    kc -n "${NS}" describe job "${JOB}" | tail -15 >&2 || true
  fi
  exit 1
fi
echo "pod ${POD}; following the loader log"
kc -n "${NS}" logs -f "${POD}" -c loader || true

# The log stream can end before the container does (or drop with the
# connection); wait for the exit code, with the pod's own state as the
# deadline check. One failed kubectl call is not a lost pod: only NotFound
# means gone, and other errors are retried, MAX_API_ERRORS times in a row.
MAX_API_ERRORS="${MAX_API_ERRORS:-30}"
api_errors=0
until EXIT="$(kc -n "${NS}" get pod "${POD}" -o jsonpath='{.status.containerStatuses[?(@.name=="loader")].state.terminated.exitCode}' 2>/dev/null)" && [[ -n "${EXIT}" ]]; do
  if PHASE="$(kc -n "${NS}" get pod "${POD}" -o jsonpath='{.status.phase}' 2>"${KUBECTL_ERR}")"; then
    api_errors=0
  elif grep -q 'NotFound' "${KUBECTL_ERR}"; then
    PHASE=gone
  else
    api_errors=$((api_errors + 1))
    echo "kubectl get pod failed (${api_errors}/${MAX_API_ERRORS}): $(head -c 300 "${KUBECTL_ERR}")" >&2
    if (( api_errors >= MAX_API_ERRORS )); then
      exit 1
    fi
    sleep 10
    continue
  fi
  if [[ "${PHASE}" == "gone" || "${PHASE}" == "Failed" ]]; then
    echo "pod ${POD} is ${PHASE} before the loader reported an exit code" >&2
    exit 1
  fi
  sleep 10
done
echo "loader exit code ${EXIT}"
if ! kc -n "${NS}" cp -c keep "${POD}:/work/${RUN_ID}.jsonl" "${OUT_DIR}/${RUN_ID}.jsonl"; then
  echo "no events file to copy; the loader exited before writing it" >&2
  exit "$(( EXIT == 0 ? 1 : EXIT ))"
fi
echo "events in ${OUT_DIR}/${RUN_ID}.jsonl"
go run ./cmd/benchmarking/swebench-loader report "${OUT_DIR}/${RUN_ID}.jsonl" -json "${OUT_DIR}/${RUN_ID}.json"
exit "${EXIT}"
