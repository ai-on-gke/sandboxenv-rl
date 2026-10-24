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
#
# Deploy / follow / tear down the Qwen3.5-35B-A3B SWE recipe.
#
#   ./deploy.sh up             # two-node training run (head + worker Jobs)
#   ./deploy.sh logs
#   ./deploy.sh down           # Jobs, Service, ConfigMap and leftover environments
#
# Environment (nothing cluster-specific is baked in):
#   KUBE_CONTEXT      kube context, or PROJECT_ID + ZONE + CLUSTER
#   GPU_POOL          optional node-pool label value for the GPU pods (default: any GPU node)
#   GPU_TOLERATIONS   optional extra NoSchedule taints on GPU nodes, "key=value,key2=value2"
#   IMAGES_JSON       tag -> digest map from integrations/uni-agent/scripts/resolve_images.sh (default ./images.json)
#   KUBECTL_ATE       kubectl-ate binary; `up`/`down` delete leftover environments (actors ua-*)
#   HF_TOKEN          optional, forwarded to the pods for model/dataset downloads
set -euo pipefail
HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO="$(cd "$HERE/../.." && pwd)"
if [ -z "${KUBE_CONTEXT:-}" ]; then
  : "${PROJECT_ID:?set KUBE_CONTEXT, or PROJECT_ID+ZONE+CLUSTER}"; : "${ZONE:?set ZONE}"; : "${CLUSTER:?set CLUSTER}"
  KUBE_CONTEXT="gke_${PROJECT_ID}_${ZONE}_${CLUSTER}"
fi
export KUBE_CONTEXT
K="kubectl --context=$KUBE_CONTEXT"
JOB=verl-qwen3p5-35b-swe
IMAGES_JSON="${IMAGES_JSON:-$HERE/images.json}"

render_job() {  # GPU placement knobs into the Job manifest
  local sel="" tol=""
  [ -n "${GPU_POOL:-}" ] && sel="cloud.google.com/gke-nodepool: ${GPU_POOL}"
  if [ -n "${GPU_TOLERATIONS:-}" ]; then
    local kv; for kv in ${GPU_TOLERATIONS//,/ }; do
      tol="${tol}${tol:+\n      }- {key: ${kv%%=*}, operator: Equal, value: \"${kv#*=}\", effect: NoSchedule}"
    done
  fi
  sed -e "s|__GPU_NODE_SELECTOR__|${sel}|" -e "s|__GPU_EXTRA_TOLERATIONS__|${tol}|" "$HERE/job-fully-async.yaml"
}
purge_envs() {  # environments the provider left behind (sessions cut off mid-episode)
  if [ -n "${KUBECTL_ATE:-}" ]; then
    for a in $("$KUBECTL_ATE" --context "$KUBE_CONTEXT" get actors -a ate-env 2>/dev/null | awk 'NR>1 && ($2 ~ /^ua-/ || $4 == "ACTOR_STATE_CRASHED") {print $2}'); do
      "$KUBECTL_ATE" --context "$KUBE_CONTEXT" delete actor "$a" -a ate-env --any-state >/dev/null 2>&1 || true
    done
  else
    echo "note: set KUBECTL_ATE=<path to kubectl-ate> to delete leftover ua-* environments in atespace ate-env"
  fi
}
check_substrate() {
  $K -n ate-env get deploy ate-env-api >/dev/null 2>&1 || { echo "ate-env-api not found in namespace ate-env: install Substrate + ate-env first (top-level README, Quickstart)"; exit 1; }
  $K -n ate-env rollout status deploy/ate-env-api --timeout=120s
  [ -s "$IMAGES_JSON" ] || { echo "missing $IMAGES_JSON: run uni_agent_substrate.datasets --images-out and integrations/uni-agent/scripts/resolve_images.sh first"; exit 1; }
}
up_job() {  # ConfigMap: run script, task config, image map, and the code the pods install
  local tmp; tmp=$(mktemp -d); trap 'rm -rf "$tmp"' RETURN
  tar -C "$REPO/integrations" -czf "$tmp/uni-agent-substrate.tgz" uni-agent
  $K -n default create configmap "$JOB" \
      --from-file=run_fully_async.sh="$HERE/run_fully_async.sh" --from-file=task_config.yaml="$HERE/task_config.yaml" \
      --from-file=images.json="$IMAGES_JSON" \
      --from-file=uni-agent-substrate.tgz="$tmp/uni-agent-substrate.tgz" \
      --from-file=offline-exclusions.txt="$REPO/benchmarking/swebench/offline-exclusions.txt" \
      --dry-run=client -o yaml | $K -n default apply -f -
  if [ -n "${HF_TOKEN:-}" ]; then
    $K -n default create secret generic hf-token --from-literal=token="$HF_TOKEN" --dry-run=client -o yaml | $K -n default apply -f -
  fi
  render_job | $K -n default apply -f -
}
down() {
  $K -n default delete job "$JOB" "$JOB-worker" --ignore-not-found --wait=true
  $K -n default delete service verl-ray-head --ignore-not-found
  $K -n default delete configmap "$JOB" --ignore-not-found
  purge_envs
}
case "${1:-up}" in
  up)   check_substrate; purge_envs; up_job; echo "started $JOB; follow with: $0 logs" ;;
  logs) $K -n default logs -f "job/$JOB" ;;
  down) down ;;
  *) echo "usage: $0 [up|logs|down]"; exit 2 ;;
esac
