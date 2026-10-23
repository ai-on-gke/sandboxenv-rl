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
# Deploy / follow / tear down the Qwen3-1.7B verl agentic-RL recipe.
#
#   ./deploy.sh up      # fully-async disaggregated: trainer on one 8-GPU node, rollout engines on another
#                       # (job-fully-async.yaml = Ray head Job + worker Job + headless Service, run_fully_async.sh)
#   ./deploy.sh logs
#   ./deploy.sh down    # removes the Jobs, Service, ConfigMap and the warm-pool environments
#
# Sandboxes are Agent Substrate environments: the cluster must already run Substrate and `ate-env-api`
# in namespace `ate-env` (see this repository's top-level README, "Quickstart").
#
# Environment (nothing cluster-specific is baked in):
#   KUBE_CONTEXT      kube context to use, or PROJECT_ID + ZONE + CLUSTER to derive gke_<project>_<zone>_<cluster>
#   GPU_POOL          optional node-pool label value to pin GPU pods to (default: any GPU node)
#   GPU_TOLERATIONS   optional extra NoSchedule taints on your GPU nodes, "key=value,key2=value2"
#   KUBECTL_ATE       optional path to kubectl-ate (Substrate repo, `go build ./cmd/kubectl-ate`); `down` uses it to
#                     delete the warm-pool environments the tool leaves behind (actors named verl-* in atespace ate-env)
set -euo pipefail
HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
if [ -z "${KUBE_CONTEXT:-}" ]; then
  : "${PROJECT_ID:?set KUBE_CONTEXT, or PROJECT_ID+ZONE+CLUSTER}"; : "${ZONE:?set ZONE}"; : "${CLUSTER:?set CLUSTER}"
  KUBE_CONTEXT="gke_${PROJECT_ID}_${ZONE}_${CLUSTER}"
fi
K="kubectl --context=$KUBE_CONTEXT"
JOB=verl-qwen3-1p7b-fully-async; JOB_YAML=job-fully-async.yaml

render_job() {  # GPU placement knobs into the Job manifest
  local sel="" tol=""
  [ -n "${GPU_POOL:-}" ] && sel="cloud.google.com/gke-nodepool: ${GPU_POOL}"
  if [ -n "${GPU_TOLERATIONS:-}" ]; then
    local kv; for kv in ${GPU_TOLERATIONS//,/ }; do
      tol="${tol}${tol:+\n      }- {key: ${kv%%=*}, operator: Equal, value: \"${kv#*=}\", effect: NoSchedule}"
    done
  fi
  sed -e "s|__GPU_NODE_SELECTOR__|${sel}|" -e "s|__GPU_EXTRA_TOLERATIONS__|${tol}|" "$HERE/$1"
}
purge_envs() {  # environments the tool's warm pool left behind
  if [ -n "${KUBECTL_ATE:-}" ]; then
    for a in $("$KUBECTL_ATE" --context "$KUBE_CONTEXT" get actors -a ate-env 2>/dev/null | awk 'NR>1 && $2 ~ /^verl-/ {print $2}'); do
      "$KUBECTL_ATE" --context "$KUBE_CONTEXT" delete actor "$a" -a ate-env --any-state >/dev/null 2>&1 || true   # running actors need --any-state
    done
  else
    echo "note: set KUBECTL_ATE=<path to kubectl-ate> to delete leftover verl-* environments in atespace ate-env"
  fi
}
check_substrate() {
  $K -n ate-env get deploy ate-env-api >/dev/null 2>&1 || { echo "ate-env-api not found in namespace ate-env: install Substrate + ate-env first (top-level README, Quickstart)"; exit 1; }
  $K -n ate-env rollout status deploy/ate-env-api --timeout=120s
}
up_job() {
  $K -n default create configmap "$JOB" \
      --from-file=run_fully_async.sh="$HERE/run_fully_async.sh" \
      --from-file=tool_config.yaml="$HERE/tool_config.yaml" \
      --from-file=ate_env_tool.py="$HERE/ate_env_tool.py" \
      --dry-run=client -o yaml | $K -n default apply -f -
  render_job "$JOB_YAML" | $K -n default apply -f -
}
case "${1:-up}" in
  up)   check_substrate; purge_envs; up_job; echo "started $JOB; follow with: $0 logs" ;;
  logs) $K -n default logs -f "job/$JOB" ;;
  down)
    $K -n default delete job verl-qwen3-1p7b-fully-async verl-qwen3-1p7b-fully-async-worker --ignore-not-found --wait=true
    $K -n default delete service verl-ray-head --ignore-not-found
    $K -n default delete configmap verl-qwen3-1p7b-fully-async --ignore-not-found
    purge_envs ;;
  *) echo "usage: $0 [up|logs|down]"; exit 2 ;;
esac
