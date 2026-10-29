# Qwen3.5-35B-A3B agentic GRPO on SWE tasks: verl + uni-agent + Substrate sandboxes

A uni-agent ReAct agent fixes real repository issues inside Agent Substrate
environments; after each episode the hidden tests run in the same environment and
decide the reward. GRPO, full-weight Megatron training, verl v1 `separate_async`:
trainer on one 8-GPU node, vLLM rollout engines on a second 8-GPU node, both the
agent's tool calls and the post-hoc grading in Substrate through `ate-env-api`.

Pins: uni-agent `174b040` (its verl submodule `3efe38c7`), image
`verlai/verl:vllm024.dev2`, client `ai-on-gke/sandboxenv-rl@2eccd896`, `swebench==4.1.0`, model
revision `59d61f3c`. Validated against Agent Substrate `ddb46651` with the plaintext router. This repo's
main now pins Substrate `v0.6.0` with router mTLS; moving the recipe to that pair is a separate change.
Provider, task, dataset and image tooling: [`integrations/uni-agent`](../../integrations/uni-agent).

## Prerequisites

- Agent Substrate `ddb46651` and `ate-env-api` built from client commit `2eccd896`, installed in
  namespace `ate-env` (Quickstart); `kubectl-ate` and `ate-env` binaries from the same commits. If
  `ate-env-api` requires a caller token, export `SUBSTRATE_ENV_AUTH_TOKEN` before `deploy.sh up`.
- Two GKE GPU nodes with 8 GPUs each (validated on a3-ultragpu-8g / H200, no RDMA).
  The standard `nvidia.com/gpu` taint is tolerated; extra taints go in `GPU_TOLERATIONS`.
- A CPU node pool for Substrate workers, labeled and tainted `ate.dev/sandboxClass=gvisor`
  (the taint the Substrate controller already adds a toleration for; atelet's DaemonSet
  tolerates it too, so nothing in Substrate is patched). Workers run runsc inside
  unprivileged pods: plain nodes, no GKE Sandbox. Each SWE environment is 2 CPU / 6 GiB;
  one worker per node books `12` CPU / `110Gi` (6 environments per 32-vCPU node). The
  6 GiB is a booking, not a hard cap: SWE test suites peak well above it, and if the sum
  exceeds the worker's limit the worker is OOM-killed with everything it hosts (watch
  `kubectl -n ate-env get pods` restarts; 10 per node still did that in validation).
- Per-instance task images reachable by the nodes as digest-pinned references: an
  Artifact Registry remote repository proxying Docker Hub (no copying) or a mirror.
- `crane`, Python 3.11+ with `datasets`, `pyarrow`, `huggingface_hub` on the machine
  that prepares the slices; GPU pod egress to GitHub, PyPI and Hugging Face.

## Run

```bash
export KUBE_CONTEXT=<context> KUBECTL_ATE=<path to kubectl-ate>

# 1. Substrate workers onto the sandbox pool + SWE template (once; replicas = nodes = ceil((SESSIONS + VAL_TASKS) * 1.15 / 6))
kubectl -n ate-env patch workerpool default-template-workerpool --type=merge -p '{"spec":{"replicas":8,"template":{
  "nodeSelector":{"ate.dev/sandboxClass":"gvisor"},
  "resources":{"requests":{"cpu":"12","memory":"110Gi"},"limits":{"cpu":"12","memory":"110Gi"}}}}}'
ate-env manifest template --template swe-2c6g --cpu 2 --memory 6Gi --workerpool default-template-workerpool \
    --atespace ate-env | kubectl-ate create actor-template -f -          # add --snapshots-bucket / --guest-image as in the Quickstart

# 2. dataset slices and digest-pinned images (once per slice)
pip install -e '../../integrations/uni-agent[datasets]'
python3 -m uni_agent_substrate.datasets --source r2e_gym_easy --max-instances 64 --images-out images.txt --out /tmp/train.parquet
python3 -m uni_agent_substrate.datasets --source swe_bench --max-instances 20 \
    --exclude-file ../../benchmarking/swebench/offline-exclusions.txt --images-out images-val.txt --out /tmp/val.parquet
../../integrations/uni-agent/scripts/resolve_images.sh images.txt     <region>-docker.pkg.dev/<project>/<dockerhub-remote-repo> images.json
../../integrations/uni-agent/scripts/resolve_images.sh images-val.txt <region>-docker.pkg.dev/<project>/<dockerhub-remote-repo> images.json

# 3. training smoke (2 steps), then cleanup
./deploy.sh up && ./deploy.sh logs          # ... ray nodes with GPUs: 2/2 ... step:1 ... step:2 ... === DONE rc=0 ===
./deploy.sh down
```

`images.json` holds your registry paths; it is read from `IMAGES_JSON` (default
`./images.json`, git-ignored) and shipped to the pods in the ConfigMap.

Knobs (Job env in `job-fully-async.yaml`): `TRAIN_SOURCE` / `TRAIN_SPLIT` /
`TRAIN_INSTANCES` / `TRAIN_DATASET`, `VAL_TASKS`, `TRAIN_BATCH`
(16) = `SYNC_STEP` (2) x `MINI_BATCH` (8), `ROLLOUT_N` (8), `SESSIONS` (24), `GATEWAYS`,
`AGENT_WORKERS`, `TOTAL_STEPS` (2), `PROMPT_LENGTH` (8192), `RESPONSE_LENGTH` (32768),
`TRAIN_CP` (1; 2 if the 40k-token step runs out of memory), `ROLLOUT_TP` (2),
`ATE_TEMPLATE`, `ATE_ENV_ENDPOINT`. Sizing: keep `SESSIONS x 2 CPU / 6 GiB` within the
worker pool, counting validation sessions too, and scale `TRAIN_BATCH` and `SESSIONS`
together (WorkerPool replicas = `ceil((SESSIONS + VAL_TASKS) * 1.15 / 6)`). Timeline on H200: ~5 min install, ~5 min model download
on each node, ~1.5 min engine start, then roughly 10 to 15 minutes per
step with 100-turn episodes.

## Files

| File | Purpose |
|---|---|
| `deploy.sh` | `up` / `logs` / `down`; renders the manifest, packs the ConfigMap, deletes leftover environments |
| `job-fully-async.yaml` | Ray head Job + worker Job + headless Service, required pod anti-affinity, 8 GPUs each |
| `run_fully_async.sh` | both pods: install pinned verl/uni-agent/provider, download the model; head: datasets, probe, Ray, verl `separate_async` |
| `task_config.yaml` | uni-agent task config: `r2e_gym` (train) and `swe_bench` (validation) on the `ate_env` provider, ReAct tools and SWE prompt |

## What it covers

| Property | How |
|---|---|
| Agentic | uni-agent ReAct agent, up to 100 steps, tools `str_replace_editor` / `stateful_shell` / `submit`, tool calls parsed with `qwen3_coder` |
| GRPO, full weights | `adv_estimator=grpo`, n=8 per prompt, Megatron actor via mbridge (TP2 EP8, CPU-offloaded optimizer), no LoRA |
| Async | `trainer.v1.trainer_mode=separate_async`, `parameter_sync_step=2`, off-policy threshold 16, token-level importance weights; rollouts stream while the trainer updates |
| Disaggregated | two Jobs with required anti-affinity; `trainer.nnodes=1`, `rollout.nnodes=1`, 8 GPUs each: Megatron on one machine, 4x TP2 vLLM on the other, weights pushed over NCCL |
| Substrate via ate-env-api | provider `ate_env` creates one environment per episode from the instance's digest-pinned image on the `swe-2c6g` template |
| In-rollout tool execution | the agent's shell and editor calls run in that environment (`Env.start_process`, native file RPCs, no egress) |
| Post-hoc reward executor | `r2e_gym`: hidden tests archived out before the agent, restored after, `run_tests.sh` graded against `expected_output_json`; `swe_bench`: uni-agent's harness eval script; both in the same environment, reward via `score_from_runner_result`. To check the grading path, set `run_oracle_solution: true` on a task in `task_config.yaml`: the gold patch must score 1.0 |
| Swappable dataset | `uni_agent_substrate.datasets --source {r2e_gym_easy,r2e_gym,swe_rebench,swe_bench}` emits one row shape; `TRAIN_SOURCE`/`TRAIN_DATASET` select it; each task name has a `task_config.yaml` entry |

## Scope notes

- Default training set is `hfilaretov/Benchmark-R2E-Gym-Easy` (Apache-2.0, 700 train /
  251 val R2E-Gym instances) because a 35B-A3B policy needs mixed outcomes per group;
  the slice is the first `TRAIN_INSTANCES` by instance id. SWE-bench Verified is
  validation only, minus the 55 instances that need network access.
- Environments have no egress: agents cannot `pip install`, and the shell handle
  persists `cd`/exports without tmux. Thinking stays on (model default); unfinished
  episodes (step or token limits) are masked.
- Only the WorkerPool is configured (node selector, capacity, replicas); the pool's
  `ate.dev/sandboxClass` taint is the one Substrate handles itself. Re-apply the patch
  after a Substrate reinstall.
- Environments created by this recipe are named `verl-qwen3p5-35b-*` (`actor_prefix` in
  `task_config.yaml`); `deploy.sh up`/`down` delete only those when `KUBECTL_ATE` is set.
