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

# Simulated mixed R2E-Gym + SWE-bench benchmark recipe at max_concurrency=200
# over small image slices (default 20 R2E-Gym images + 20 SWE-bench images,
# 5 concurrent sandboxes per image = 100 + 100 = 200 active actors).
#
# Subcommands:
#   dry-run  Validate manifests, trajectories, and <63-char actor names offline.
#   prewarm  Bake golden snapshots and warm node caches (1 actor per image).
#   run      Drive both shards concurrently and emit unified report.json/report.md.
#
# Example:
#   R2E_DATASET=/tmp/manifests/r2e-20.jsonl \
#   SWE_DATASET=/tmp/manifests/swebench-20.jsonl \
#   KO_DOCKER_REPO=us-central1-docker.pkg.dev/PROJECT/repo \
#   ARTIFACT_DIR=/tmp/r2eswe20-out \
#   benchmarking/swebench/run-r2e-swebench-200.sh prewarm
#
#   R2E_DATASET=/tmp/manifests/r2e-20.jsonl \
#   SWE_DATASET=/tmp/manifests/swebench-20.jsonl \
#   KO_DOCKER_REPO=us-central1-docker.pkg.dev/PROJECT/repo \
#   ARTIFACT_DIR=/tmp/r2eswe20-out \
#   benchmarking/swebench/run-r2e-swebench-200.sh run

set -o errexit -o nounset -o pipefail

ACTION="run"
if (( $# > 0 )); then
  case "$1" in
    dry-run|prewarm|run)
      ACTION="$1"
      shift
      ;;
    -h|--help|help)
      sed -n '17,37p' "${BASH_SOURCE[0]}" | sed 's/^# \{0,1\}//'
      exit 0
      ;;
  esac
fi
EXTRA_ARGS=("$@")

INVOKED_DIR="$(pwd)"
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"

abs_path() {
  local p="$1"
  if [[ "${p}" == /* ]]; then
    printf '%s\n' "${p}"
  else
    printf '%s/%s\n' "${INVOKED_DIR}" "${p}"
  fi
}

: "${R2E_DATASET:?set R2E_DATASET to the R2E-Gym JSONL manifest path}"
: "${SWE_DATASET:?set SWE_DATASET to the SWE-bench JSONL manifest path}"
R2E_DATASET="$(abs_path "${R2E_DATASET}")"
SWE_DATASET="$(abs_path "${SWE_DATASET}")"
[[ -f "${R2E_DATASET}" ]] || { echo "R2E_DATASET not found: ${R2E_DATASET}" >&2; exit 2; }
[[ -f "${SWE_DATASET}" ]] || { echo "SWE_DATASET not found: ${SWE_DATASET}" >&2; exit 2; }

if [[ -z "${SWE_EXCLUDE_FILE+x}" ]]; then
  SWE_EXCLUDE_FILE="${ROOT}/benchmarking/swebench/offline-exclusions.txt"
elif [[ -n "${SWE_EXCLUDE_FILE}" ]]; then
  SWE_EXCLUDE_FILE="$(abs_path "${SWE_EXCLUDE_FILE}")"
fi
if [[ -n "${SWE_EXCLUDE_FILE}" && ! -f "${SWE_EXCLUDE_FILE}" ]]; then
  echo "SWE_EXCLUDE_FILE not found: ${SWE_EXCLUDE_FILE}" >&2
  exit 2
fi

R2E_EXCLUDE_FILE="${R2E_EXCLUDE_FILE:-}"
if [[ -n "${R2E_EXCLUDE_FILE}" ]]; then
  R2E_EXCLUDE_FILE="$(abs_path "${R2E_EXCLUDE_FILE}")"
  [[ -f "${R2E_EXCLUDE_FILE}" ]] || { echo "R2E_EXCLUDE_FILE not found: ${R2E_EXCLUDE_FILE}" >&2; exit 2; }
fi

RUN_ID="${RUN_ID:-r2eswe20}"
if [[ ! "${RUN_ID}" =~ ^[a-z0-9]([-a-z0-9]*[a-z0-9])?$ || ${#RUN_ID} -gt 14 ]]; then
  echo "RUN_ID must be a DNS label of at most 14 characters (got ${RUN_ID})" >&2
  exit 2
fi
SWE_RUN_ID="${RUN_ID}-swe"
R2E_RUN_ID="${RUN_ID}-r2e"

ARTIFACT_DIR="$(abs_path "${ARTIFACT_DIR:-/tmp/${RUN_ID}}")"

MAX_CONCURRENCY="${MAX_CONCURRENCY:-200}"
R2E_IMAGES="${R2E_IMAGES:-20}"
SWE_IMAGES="${SWE_IMAGES:-20}"
R2E_WIDTH="${R2E_WIDTH:-$(( MAX_CONCURRENCY / 2 ))}"
SWE_WIDTH="${SWE_WIDTH:-$(( MAX_CONCURRENCY - R2E_WIDTH ))}"

for num_var in MAX_CONCURRENCY R2E_IMAGES SWE_IMAGES R2E_WIDTH SWE_WIDTH; do
  val="${!num_var}"
  if [[ ! "${val}" =~ ^[1-9][0-9]*$ ]]; then
    echo "${num_var} must be a positive integer (got ${val})" >&2
    exit 2
  fi
done

R2E_FANOUT="${R2E_FANOUT:-$(( (R2E_WIDTH + R2E_IMAGES - 1) / R2E_IMAGES ))}"
SWE_FANOUT="${SWE_FANOUT:-$(( (SWE_WIDTH + SWE_IMAGES - 1) / SWE_IMAGES ))}"

WORKLOAD="${WORKLOAD:-graded}"
IO_TRAJECTORY="${IO_TRAJECTORY:-workload-file-read}"
case "${WORKLOAD}" in
  graded)
    SWE_TRAJECTORY="${SWE_TRAJECTORY:-swebench-gold}"
    R2E_TRAJECTORY="${R2E_TRAJECTORY:-${ROOT}/benchmarking/swebench/r2e-gold.yaml}"
    ;;
  io)
    SWE_TRAJECTORY="${SWE_TRAJECTORY:-${IO_TRAJECTORY}}"
    R2E_TRAJECTORY="${R2E_TRAJECTORY:-${IO_TRAJECTORY}}"
    ;;
  *)
    echo "WORKLOAD must be 'graded' or 'io' (got ${WORKLOAD})" >&2
    exit 2
    ;;
esac

MODE="${MODE:-steps}"
STEPS="${STEPS:-4}"
PARK="${PARK:-suspend}"
STEP_PAUSE="${STEP_PAUSE:-5s}"
ARRIVAL_RATE="${ARRIVAL_RATE:-2.0}"
CONNECTIONS="${CONNECTIONS:-8}"
WAKE_BUDGET="${WAKE_BUDGET:-5m}"
CREATE_TIMEOUT="${CREATE_TIMEOUT:-5m}"
STEP_TIMEOUT="${STEP_TIMEOUT:-15m}"

NAMESPACE="${NAMESPACE:-ate-env}"
CONTEXT="${CONTEXT:-}"
ENDPOINT="${ENDPOINT:-localhost:7777}"
ATESPACE="${ATESPACE:-${NAMESPACE}}"
BASE_TEMPLATE="${BASE_TEMPLATE:-default-template}"
AUTH_TOKEN_SECRET="${AUTH_TOKEN_SECRET:-}"
START_TIMEOUT="${START_TIMEOUT:-600}"
MAX_API_ERRORS="${MAX_API_ERRORS:-30}"
SAMPLE_CACHE="${SAMPLE_CACHE:-0}"
HARVEST_RESTORES="${HARVEST_RESTORES:-0}"
HARVEST_INTERVAL_MIN="${HARVEST_INTERVAL_MIN:-10}"
if [[ ! "${HARVEST_INTERVAL_MIN}" =~ ^[1-9][0-9]*$ ]]; then
  echo "HARVEST_INTERVAL_MIN must be a positive integer (got ${HARVEST_INTERVAL_MIN})" >&2
  exit 2
fi

if [[ -z "${EXECUTION:-}" ]]; then
  if [[ -n "${KO_DOCKER_REPO:-}" ]]; then
    EXECUTION="in-cluster"
  else
    EXECUTION="local"
  fi
fi
if [[ "${EXECUTION}" != "local" && "${EXECUTION}" != "in-cluster" ]]; then
  echo "EXECUTION must be 'local' or 'in-cluster' (got ${EXECUTION})" >&2
  exit 2
fi

if [[ -n "${AUTH_TOKEN_SECRET}" ]] && [[ ! "${AUTH_TOKEN_SECRET}" =~ ^[a-z0-9]([-a-z0-9.]*[a-z0-9])?$ || ${#AUTH_TOKEN_SECRET} -gt 253 ]]; then
  echo "AUTH_TOKEN_SECRET must be a valid Kubernetes Secret name" >&2
  exit 2
fi

kc() { kubectl ${CONTEXT:+--context "${CONTEXT}"} "$@"; }

mode_args_for() {
  local shard_mode="$1"
  local fanout="$2"
  MODE_ARGS=("-mode=${shard_mode}")
  case "${shard_mode}" in
    steps)
      MODE_ARGS+=("-rollouts=${fanout}" "-steps=${STEPS}" "-park=${PARK}" "-step-pause=${STEP_PAUSE}")
      ;;
    eval)
      MODE_ARGS+=("-tasks-per-image=${fanout}")
      ;;
    open-loop)
      MODE_ARGS+=("-tasks-per-image=${fanout}" "-arrival-rate=${ARRIVAL_RATE}")
      ;;
    *)
      echo "MODE must be 'steps', 'eval', or 'open-loop' (got ${shard_mode})" >&2
      exit 2
      ;;
  esac
}

cd "${ROOT}"

if [[ "${ACTION}" == "dry-run" ]]; then
  mkdir -p "${ARTIFACT_DIR}/events"
  mode_args_for "${MODE}" "${SWE_FANOUT}"
  SWE_DRY_ARGS=(
    run -dry-run
    "-dataset=${SWE_DATASET}"
    "-run-id=${SWE_RUN_ID}"
    "-images=${SWE_IMAGES}"
    "-width=${SWE_WIDTH}"
    "-trajectory=${SWE_TRAJECTORY}"
    "-events-dir=${ARTIFACT_DIR}"
    "-worker=swe"
    "-job=swe"
    "-connections=${CONNECTIONS}"
    "-atespace=${ATESPACE}"
    "-base-template=${BASE_TEMPLATE}"
    "-wake-budget=${WAKE_BUDGET}"
    "-create-timeout=${CREATE_TIMEOUT}"
    "-step-timeout=${STEP_TIMEOUT}"
    "${MODE_ARGS[@]}"
  )
  if [[ -n "${SWE_EXCLUDE_FILE}" ]]; then
    SWE_DRY_ARGS+=("-exclude-file=${SWE_EXCLUDE_FILE}")
  fi
  if (( ${#EXTRA_ARGS[@]} > 0 )); then
    SWE_DRY_ARGS+=("${EXTRA_ARGS[@]}")
  fi

  mode_args_for "${MODE}" "${R2E_FANOUT}"
  R2E_DRY_ARGS=(
    run -dry-run
    "-dataset=${R2E_DATASET}"
    "-run-id=${R2E_RUN_ID}"
    "-images=${R2E_IMAGES}"
    "-width=${R2E_WIDTH}"
    "-trajectory=${R2E_TRAJECTORY}"
    "-events-dir=${ARTIFACT_DIR}"
    "-worker=r2e"
    "-job=r2e"
    "-connections=${CONNECTIONS}"
    "-atespace=${ATESPACE}"
    "-base-template=${BASE_TEMPLATE}"
    "-wake-budget=${WAKE_BUDGET}"
    "-create-timeout=${CREATE_TIMEOUT}"
    "-step-timeout=${STEP_TIMEOUT}"
    "${MODE_ARGS[@]}"
  )
  if [[ -n "${R2E_EXCLUDE_FILE}" ]]; then
    R2E_DRY_ARGS+=("-exclude-file=${R2E_EXCLUDE_FILE}")
  fi
  if (( ${#EXTRA_ARGS[@]} > 0 )); then
    R2E_DRY_ARGS+=("${EXTRA_ARGS[@]}")
  fi

  go run ./cmd/benchmarking/swebench-loader "${SWE_DRY_ARGS[@]}"
  go run ./cmd/benchmarking/swebench-loader "${R2E_DRY_ARGS[@]}"
  exit 0
fi

if [[ "${EXECUTION}" == "in-cluster" && ( "${ARTIFACT_DIR}" == "${ROOT}" || "${ARTIFACT_DIR}" == "${ROOT}"/* ) ]]; then
  echo "ARTIFACT_DIR (${ARTIFACT_DIR}) must be outside the repository (${ROOT}) when EXECUTION=in-cluster" >&2
  exit 2
fi

mkdir -p "${ARTIFACT_DIR}/events" "${ARTIFACT_DIR}/prewarm"

ACTIVE_MODE="${MODE}"
ACTIVE_SWE_WIDTH="${SWE_WIDTH}"
ACTIVE_R2E_WIDTH="${R2E_WIDTH}"
ACTIVE_SWE_FANOUT="${SWE_FANOUT}"
ACTIVE_R2E_FANOUT="${R2E_FANOUT}"
ACTIVE_SWE_TRAJ="${SWE_TRAJECTORY}"
ACTIVE_R2E_TRAJ="${R2E_TRAJECTORY}"
ACTIVE_SWE_RUN_ID="${SWE_RUN_ID}"
ACTIVE_R2E_RUN_ID="${R2E_RUN_ID}"
if [[ "${ACTION}" == "prewarm" ]]; then
  ACTIVE_MODE="eval"
  ACTIVE_SWE_WIDTH="${SWE_IMAGES}"
  ACTIVE_R2E_WIDTH="${R2E_IMAGES}"
  ACTIVE_SWE_FANOUT=1
  ACTIVE_R2E_FANOUT=1
  ACTIVE_SWE_TRAJ="workload-file-read-one"
  ACTIVE_R2E_TRAJ="workload-file-read-one"
  ACTIVE_SWE_RUN_ID="${RUN_ID}-pws"
  ACTIVE_R2E_RUN_ID="${RUN_ID}-pwr"
fi

run_local_shards() {
  local swe_out="$1"
  local r2e_out="$2"

  mode_args_for "${ACTIVE_MODE}" "${ACTIVE_SWE_FANOUT}"
  local -a swe_cmd=(
    go run ./cmd/benchmarking/swebench-loader run
    "-endpoint=${ENDPOINT}"
    "-dataset=${SWE_DATASET}"
    "-run-id=${ACTIVE_SWE_RUN_ID}"
    "-images=${SWE_IMAGES}"
    "-width=${ACTIVE_SWE_WIDTH}"
    "-trajectory=${ACTIVE_SWE_TRAJ}"
    "-out=${swe_out}"
    "-worker=swe"
    "-job=swe"
    "-connections=${CONNECTIONS}"
    "-atespace=${ATESPACE}"
    "-base-template=${BASE_TEMPLATE}"
    "-wake-budget=${WAKE_BUDGET}"
    "-create-timeout=${CREATE_TIMEOUT}"
    "-step-timeout=${STEP_TIMEOUT}"
    "${MODE_ARGS[@]}"
  )
  if [[ -n "${SWE_EXCLUDE_FILE}" ]]; then
    swe_cmd+=("-exclude-file=${SWE_EXCLUDE_FILE}")
  fi
  if [[ "${ACTION}" == "run" ]] && (( ${#EXTRA_ARGS[@]} > 0 )); then
    swe_cmd+=("${EXTRA_ARGS[@]}")
  fi

  mode_args_for "${ACTIVE_MODE}" "${ACTIVE_R2E_FANOUT}"
  local -a r2e_cmd=(
    go run ./cmd/benchmarking/swebench-loader run
    "-endpoint=${ENDPOINT}"
    "-dataset=${R2E_DATASET}"
    "-run-id=${ACTIVE_R2E_RUN_ID}"
    "-images=${R2E_IMAGES}"
    "-width=${ACTIVE_R2E_WIDTH}"
    "-trajectory=${ACTIVE_R2E_TRAJ}"
    "-out=${r2e_out}"
    "-worker=r2e"
    "-job=r2e"
    "-connections=${CONNECTIONS}"
    "-atespace=${ATESPACE}"
    "-base-template=${BASE_TEMPLATE}"
    "-wake-budget=${WAKE_BUDGET}"
    "-create-timeout=${CREATE_TIMEOUT}"
    "-step-timeout=${STEP_TIMEOUT}"
    "${MODE_ARGS[@]}"
  )
  if [[ -n "${R2E_EXCLUDE_FILE}" ]]; then
    r2e_cmd+=("-exclude-file=${R2E_EXCLUDE_FILE}")
  fi
  if [[ "${ACTION}" == "run" ]] && (( ${#EXTRA_ARGS[@]} > 0 )); then
    r2e_cmd+=("${EXTRA_ARGS[@]}")
  fi

  local swe_pid r2e_pid swe_rc=0 r2e_rc=0
  "${swe_cmd[@]}" &
  swe_pid=$!
  "${r2e_cmd[@]}" &
  r2e_pid=$!
  wait "${swe_pid}" || swe_rc=$?
  wait "${r2e_pid}" || r2e_rc=$?
  SHARD_EXIT=$(( swe_rc != 0 ? swe_rc : r2e_rc ))
}

KODATA="${ROOT}/cmd/benchmarking/swebench-loader/kodata"
SWE_JOB="swebench-loader-${ACTIVE_SWE_RUN_ID}"
R2E_JOB="swebench-loader-${ACTIVE_R2E_RUN_ID}"
HARVEST_DIR=""
HARVEST_PID=""
KUBECTL_ERR=""

# shellcheck disable=SC2317,SC2329 # invoked by the EXIT trap
cleanup_cluster() {
  kc -n "${NAMESPACE}" delete job "${SWE_JOB}" "${R2E_JOB}" --ignore-not-found --wait=false >/dev/null 2>&1 || true
  if [[ -n "${HARVEST_PID}" ]]; then
    kill "${HARVEST_PID}" 2>/dev/null || true
  fi
  rm -rf "${KODATA}"
  if [[ -n "${HARVEST_DIR}" ]]; then
    rm -rf "${HARVEST_DIR}"
  fi
  if [[ -n "${KUBECTL_ERR}" ]]; then
    rm -f "${KUBECTL_ERR}"
  fi
}

TRAJ_N=0
BAKED=""
bake_trajectory() {
  local -a parts out=()
  local el path dir base assets asset
  IFS=',' read -r -a parts <<< "$1"
  for el in "${parts[@]}"; do
    path="${el}"
    [[ "${path}" == /* ]] || path="${INVOKED_DIR}/${path}"
    if [[ -f "${path}" ]]; then
      dir="$(cd "$(dirname "${path}")" && pwd)"
      base="$(basename "${path}")"
      TRAJ_N=$((TRAJ_N + 1))
      mkdir -p "${KODATA}/traj/${TRAJ_N}"
      cp "${dir}/${base}" "${KODATA}/traj/${TRAJ_N}/"
      assets="$(go run ./cmd/benchmarking/swebench-loader trajectory-assets "${dir}/${base}")" || exit 2
      while IFS= read -r asset; do
        [[ -n "${asset}" ]] || continue
        case "${asset}" in
          */*|..) echo "${base}: write.file ${asset} must be a plain file name" >&2; exit 2 ;;
        esac
        [[ -f "${dir}/${asset}" && ! -L "${dir}/${asset}" ]] || {
          echo "${base}: write.file ${asset} is not a regular file next to the trajectory" >&2
          exit 2
        }
        cp "${dir}/${asset}" "${KODATA}/traj/${TRAJ_N}/${asset}"
      done <<< "${assets}"
      el="/var/run/ko/traj/${TRAJ_N}/${base}"
    fi
    out+=("${el}")
  done
  local IFS=','
  BAKED="${out[*]}"
}

wait_for_pod() {
  local job_name="$1"
  local pod=""
  local deadline=$((SECONDS + START_TIMEOUT))
  while (( SECONDS < deadline )); do
    pod="$(kc -n "${NAMESPACE}" get pod -l "job-name=${job_name}" -o jsonpath='{.items[0].metadata.name}' 2>/dev/null || true)"
    if [[ -n "${pod}" ]] && kc -n "${NAMESPACE}" get pod "${pod}" -o jsonpath='{.status.containerStatuses[?(@.name=="loader")].state}' 2>/dev/null | grep -q 'running\|terminated'; then
      printf '%s\n' "${pod}"
      return 0
    fi
    sleep 5
  done
  echo "loader pod for ${job_name} did not start within ${START_TIMEOUT}s" >&2
  if [[ -n "${pod}" ]]; then
    kc -n "${NAMESPACE}" describe pod "${pod}" | tail -15 >&2 || true
  else
    kc -n "${NAMESPACE}" describe job "${job_name}" | tail -15 >&2 || true
  fi
  return 1
}

wait_for_exit() {
  local pod="$1"
  local exit_code=""
  local phase=""
  local api_errors=0
  until exit_code="$(kc -n "${NAMESPACE}" get pod "${pod}" -o jsonpath='{.status.containerStatuses[?(@.name=="loader")].state.terminated.exitCode}' 2>/dev/null)" && [[ -n "${exit_code}" ]]; do
    if phase="$(kc -n "${NAMESPACE}" get pod "${pod}" -o jsonpath='{.status.phase}' 2>"${KUBECTL_ERR}")"; then
      api_errors=0
    elif grep -q 'NotFound' "${KUBECTL_ERR}"; then
      phase="gone"
    else
      api_errors=$((api_errors + 1))
      echo "kubectl get pod ${pod} failed (${api_errors}/${MAX_API_ERRORS}): $(head -c 300 "${KUBECTL_ERR}")" >&2
      if (( api_errors >= MAX_API_ERRORS )); then
        return 1
      fi
      sleep 10
      continue
    fi
    if [[ "${phase}" == "gone" || "${phase}" == "Failed" ]]; then
      echo "pod ${pod} is ${phase} before reporting an exit code" >&2
      return 1
    fi
    sleep 10
  done
  printf '%s\n' "${exit_code}"
}

apply_loader_job() {
  local job_name="$1"
  local image="$2"
  local args_json="$3"
  local loader_env=""
  if [[ -n "${AUTH_TOKEN_SECRET}" ]]; then
    loader_env="env:
            - name: SUBSTRATE_ENV_AUTH_TOKEN
              valueFrom:
                secretKeyRef:
                  name: ${AUTH_TOKEN_SECRET}
                  key: token"
  fi
  kc -n "${NAMESPACE}" delete job "${job_name}" --ignore-not-found --wait=true >/dev/null
  cat <<EOF_JOB | kc apply -f -
apiVersion: batch/v1
kind: Job
metadata:
  name: ${job_name}
  namespace: ${NAMESPACE}
spec:
  backoffLimit: 0
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
          image: ${image}
          args: ${args_json}
          ${loader_env}
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
}

harvest_both_parts() {
  local tag="$1"
  local harvest="${ROOT}/benchmarking/swebench/harvest-restore-timings.sh"
  local window="$(( 2 * HARVEST_INTERVAL_MIN ))m"
  NAMESPACE=ate-system CONTEXT="${CONTEXT}" SINCE="${window}" "${harvest}" "${ACTIVE_SWE_RUN_ID}" "${HARVEST_DIR}/swe-${tag}.tmp" >&2 || true
  if [[ -s "${HARVEST_DIR}/swe-${tag}.tmp" ]]; then
    mv "${HARVEST_DIR}/swe-${tag}.tmp" "${HARVEST_DIR}/part-swe-${tag}.jsonl"
  fi
  NAMESPACE=ate-system CONTEXT="${CONTEXT}" SINCE="${window}" "${harvest}" "${ACTIVE_R2E_RUN_ID}" "${HARVEST_DIR}/r2e-${tag}.tmp" >&2 || true
  if [[ -s "${HARVEST_DIR}/r2e-${tag}.tmp" ]]; then
    mv "${HARVEST_DIR}/r2e-${tag}.tmp" "${HARVEST_DIR}/part-r2e-${tag}.jsonl"
  fi
}

run_in_cluster_shards() {
  local swe_out="$1"
  local r2e_out="$2"

  : "${KO_DOCKER_REPO:?set KO_DOCKER_REPO to the registry path the cluster can pull from}"
  command -v python3 >/dev/null || { echo "python3 is required to encode Job args" >&2; exit 2; }
  echo "kubectl context ${CONTEXT:-$(kubectl config current-context 2>/dev/null || echo unknown)}"

  HARVEST_DIR="$(mktemp -d)"
  KUBECTL_ERR="$(mktemp)"
  rm -rf "${KODATA}" && mkdir -p "${KODATA}"
  trap cleanup_cluster EXIT

  cp "${SWE_DATASET}" "${KODATA}/swe-dataset.jsonl"
  cp "${R2E_DATASET}" "${KODATA}/r2e-dataset.jsonl"
  if [[ -n "${SWE_EXCLUDE_FILE}" ]]; then
    cp "${SWE_EXCLUDE_FILE}" "${KODATA}/swe-exclude.txt"
  fi
  if [[ -n "${R2E_EXCLUDE_FILE}" ]]; then
    cp "${R2E_EXCLUDE_FILE}" "${KODATA}/r2e-exclude.txt"
  fi

  bake_trajectory "${ACTIVE_SWE_TRAJ}"
  local baked_swe_traj="${BAKED}"
  bake_trajectory "${ACTIVE_R2E_TRAJ}"
  local baked_r2e_traj="${BAKED}"

  local image
  image="$(KO_DOCKER_REPO="${KO_DOCKER_REPO}/swebench-loader" ko build --bare ./cmd/benchmarking/swebench-loader)"
  rm -rf "${KODATA}"
  echo "loader image ${image}"

  mode_args_for "${ACTIVE_MODE}" "${ACTIVE_SWE_FANOUT}"
  local -a swe_args=(
    run
    "-endpoint=ate-env-api.${NAMESPACE}.svc:7777"
    "-dataset=/var/run/ko/swe-dataset.jsonl"
    "-run-id=${ACTIVE_SWE_RUN_ID}"
    "-images=${SWE_IMAGES}"
    "-width=${ACTIVE_SWE_WIDTH}"
    "-trajectory=${baked_swe_traj}"
    "-out=/work/swe.jsonl"
    "-worker=swe"
    "-job=swe"
    "-connections=${CONNECTIONS}"
    "-atespace=${ATESPACE}"
    "-base-template=${BASE_TEMPLATE}"
    "-wake-budget=${WAKE_BUDGET}"
    "-create-timeout=${CREATE_TIMEOUT}"
    "-step-timeout=${STEP_TIMEOUT}"
    "${MODE_ARGS[@]}"
  )
  if [[ -n "${SWE_EXCLUDE_FILE}" ]]; then
    swe_args+=("-exclude-file=/var/run/ko/swe-exclude.txt")
  fi
  if [[ "${ACTION}" == "run" ]] && (( ${#EXTRA_ARGS[@]} > 0 )); then
    swe_args+=("${EXTRA_ARGS[@]}")
  fi

  mode_args_for "${ACTIVE_MODE}" "${ACTIVE_R2E_FANOUT}"
  local -a r2e_args=(
    run
    "-endpoint=ate-env-api.${NAMESPACE}.svc:7777"
    "-dataset=/var/run/ko/r2e-dataset.jsonl"
    "-run-id=${ACTIVE_R2E_RUN_ID}"
    "-images=${R2E_IMAGES}"
    "-width=${ACTIVE_R2E_WIDTH}"
    "-trajectory=${baked_r2e_traj}"
    "-out=/work/r2e.jsonl"
    "-worker=r2e"
    "-job=r2e"
    "-connections=${CONNECTIONS}"
    "-atespace=${ATESPACE}"
    "-base-template=${BASE_TEMPLATE}"
    "-wake-budget=${WAKE_BUDGET}"
    "-create-timeout=${CREATE_TIMEOUT}"
    "-step-timeout=${STEP_TIMEOUT}"
    "${MODE_ARGS[@]}"
  )
  if [[ -n "${R2E_EXCLUDE_FILE}" ]]; then
    r2e_args+=("-exclude-file=/var/run/ko/r2e-exclude.txt")
  fi
  if [[ "${ACTION}" == "run" ]] && (( ${#EXTRA_ARGS[@]} > 0 )); then
    r2e_args+=("${EXTRA_ARGS[@]}")
  fi

  local swe_json r2e_json
  swe_json="$(python3 -c 'import json, sys; print(json.dumps(sys.argv[1:]))' "${swe_args[@]}")"
  r2e_json="$(python3 -c 'import json, sys; print(json.dumps(sys.argv[1:]))' "${r2e_args[@]}")"

  apply_loader_job "${SWE_JOB}" "${image}" "${swe_json}"
  apply_loader_job "${R2E_JOB}" "${image}" "${r2e_json}"

  local swe_pod r2e_pod
  swe_pod="$(wait_for_pod "${SWE_JOB}")"
  r2e_pod="$(wait_for_pod "${R2E_JOB}")"
  echo "started pods: swe=${swe_pod} r2e=${r2e_pod}"

  if [[ "${ACTION}" == "run" && "${HARVEST_RESTORES}" == "1" ]]; then
    (
      n=0
      while :; do
        sleep "$(( HARVEST_INTERVAL_MIN * 60 ))" || true
        n=$(( n + 1 ))
        harvest_both_parts "${n}"
      done
    ) &
    HARVEST_PID=$!
  fi

  kc -n "${NAMESPACE}" logs -f "${swe_pod}" -c loader > "${ARTIFACT_DIR}/swe-loader.log" 2>&1 &
  local log_swe_pid=$!
  kc -n "${NAMESPACE}" logs -f "${r2e_pod}" -c loader > "${ARTIFACT_DIR}/r2e-loader.log" 2>&1 &
  local log_r2e_pid=$!

  local swe_rc r2e_rc
  swe_rc="$(wait_for_exit "${swe_pod}")" || swe_rc=1
  r2e_rc="$(wait_for_exit "${r2e_pod}")" || r2e_rc=1
  wait "${log_swe_pid}" 2>/dev/null || true
  wait "${log_r2e_pid}" 2>/dev/null || true
  echo "loader exit codes: swe=${swe_rc} r2e=${r2e_rc}"

  if ! kc -n "${NAMESPACE}" cp -c keep "${swe_pod}:/work/swe.jsonl" "${swe_out}"; then
    echo "failed to copy swe events from ${swe_pod}" >&2
    swe_rc=$(( swe_rc == 0 ? 1 : swe_rc ))
  fi
  if ! kc -n "${NAMESPACE}" cp -c keep "${r2e_pod}:/work/r2e.jsonl" "${r2e_out}"; then
    echo "failed to copy r2e events from ${r2e_pod}" >&2
    r2e_rc=$(( r2e_rc == 0 ? 1 : r2e_rc ))
  fi

  if [[ "${ACTION}" == "run" && "${HARVEST_RESTORES}" == "1" ]]; then
    kill "${HARVEST_PID}" 2>/dev/null || true
    wait "${HARVEST_PID}" 2>/dev/null || true
    HARVEST_PID=""
    harvest_both_parts final
    local -a parts=()
    shopt -s nullglob
    parts=("${HARVEST_DIR}"/part-*.jsonl)
    shopt -u nullglob
    if (( ${#parts[@]} > 0 )); then
      sort -u "${parts[@]}" > "${ARTIFACT_DIR}/restore_timings.jsonl"
    else
      : > "${ARTIFACT_DIR}/restore_timings.jsonl"
    fi
  fi

  SHARD_EXIT=$(( swe_rc != 0 ? swe_rc : r2e_rc ))
}

SHARD_EXIT=0

if [[ "${ACTION}" == "prewarm" ]]; then
  if [[ "${EXECUTION}" == "in-cluster" ]]; then
    run_in_cluster_shards "${ARTIFACT_DIR}/prewarm/swe.jsonl" "${ARTIFACT_DIR}/prewarm/r2e.jsonl"
  else
    run_local_shards "${ARTIFACT_DIR}/prewarm/swe.jsonl" "${ARTIFACT_DIR}/prewarm/r2e.jsonl"
  fi
  echo "prewarm finished (swe=${SWE_IMAGES} images, r2e=${R2E_IMAGES} images) -> ${ARTIFACT_DIR}/prewarm/"
  exit "${SHARD_EXIT}"
fi

if [[ "${SAMPLE_CACHE}" == "1" ]]; then
  NAMESPACE=ate-system CONTEXT="${CONTEXT}" "${ROOT}/benchmarking/swebench/sample-node-cache.sh" "${ARTIFACT_DIR}/cache_before.prom"
fi

if [[ "${EXECUTION}" == "in-cluster" ]]; then
  run_in_cluster_shards "${ARTIFACT_DIR}/events/swe.jsonl" "${ARTIFACT_DIR}/events/r2e.jsonl"
else
  run_local_shards "${ARTIFACT_DIR}/events/swe.jsonl" "${ARTIFACT_DIR}/events/r2e.jsonl"
fi

if [[ "${SAMPLE_CACHE}" == "1" ]]; then
  NAMESPACE=ate-system CONTEXT="${CONTEXT}" "${ROOT}/benchmarking/swebench/sample-node-cache.sh" "${ARTIFACT_DIR}/cache_after.prom"
fi

REPORT_EXTRA=()
if [[ -f "${ARTIFACT_DIR}/restore_timings.jsonl" ]]; then
  REPORT_EXTRA+=("-restore-log" "${ARTIFACT_DIR}/restore_timings.jsonl")
fi

if [[ -s "${ARTIFACT_DIR}/events/swe.jsonl" ]]; then
  go run ./cmd/benchmarking/swebench-loader report "${ARTIFACT_DIR}/events/swe.jsonl" \
    -json "${ARTIFACT_DIR}/swe-summary.json" ${REPORT_EXTRA[@]+"${REPORT_EXTRA[@]}"}
fi
if [[ -s "${ARTIFACT_DIR}/events/r2e.jsonl" ]]; then
  go run ./cmd/benchmarking/swebench-loader report "${ARTIFACT_DIR}/events/r2e.jsonl" \
    -json "${ARTIFACT_DIR}/r2e-summary.json" ${REPORT_EXTRA[@]+"${REPORT_EXTRA[@]}"}
fi
if [[ -s "${ARTIFACT_DIR}/events/swe.jsonl" || -s "${ARTIFACT_DIR}/events/r2e.jsonl" ]]; then
  go run ./cmd/benchmarking/bench report -dest "${ARTIFACT_DIR}"
fi

exit "${SHARD_EXIT}"
