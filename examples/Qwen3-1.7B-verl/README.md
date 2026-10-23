# Qwen3-1.7B agentic GRPO with verl: async, disaggregated, Substrate sandboxes

Qwen3-1.7B solves MATH as a tool agent: it writes Python, the code runs in an
Agent Substrate environment through `ate-env-api`, and the final answer is
rewarded. GRPO, full-weight FSDP, verl `fully_async_policy`: trainer on one
8-GPU node, vLLM rollout engines on a second 8-GPU node.

Pins: verl `60546ef2`, image `verlai/verl:vllm020.dev2`, client
`ai-on-gke/sandboxenv-rl@2eccd896` (`clients/python`).

## Prerequisites

- Agent Substrate and `ate-env-api` installed in namespace `ate-env` with
  template `default-template` (top-level README, Quickstart). Substrate must
  be at the commit this repo's `go.mod` pins; newer Substrate changed the API
  and this ate-env cannot talk to it.
- Two GKE GPU nodes with 8 GPUs each (validated on a3-ultragpu-8g / H200).
  Standard `nvidia.com/gpu` taint is tolerated; extra taints go in
  `GPU_TOLERATIONS`.
- CPU for the environments: the run keeps 32 environments (2 CPU / 4 GiB
  limits each) on Substrate's workers, which schedule on untainted nodes.
- GPU pod egress to GitHub, PyPI and Hugging Face; `kubectl` rights for Jobs,
  Services and ConfigMaps in `default`. No Ray operator needed.

## Run

```bash
export KUBE_CONTEXT=<context>          # or PROJECT_ID ZONE CLUSTER
# export GPU_POOL=<node-pool>          # pin GPU pods to a pool (default: any GPU node)
# export GPU_TOLERATIONS=key=value     # extra NoSchedule taints on GPU nodes
# export KUBECTL_ATE=<path>            # lets up/down delete leftover environments
./deploy.sh up
./deploy.sh logs
./deploy.sh down
```

Expect in the log: `ate-env-api probe ok`, `ray nodes with GPUs: 2/2`, then
`step:1`, `step:2`, `=== DONE rc=0 ===`. About 10 minutes end to end on H200
(image pull, install and model download on both nodes, ~10 s per step).

Knobs (Job env in `job-fully-async.yaml`): `LOCAL_UPDATES` (4),
`PPO_MINI_BATCH` (16), `N_SAMPLES` (4), `SYNC_EVERY` (2), `STALENESS` (0.5),
`NGPUS_TRAIN`/`NGPUS_ROLLOUT` (8/8), `NODE_WAIT_S` (3600), `MODEL_ID`.
Tool settings in `tool_config.yaml`: `endpoint`, `max_envs`, `timeout_s`,
optional `rootfs_image`.

## Files

| File | Purpose |
|---|---|
| `deploy.sh` | `up` / `logs` / `down`; renders the manifest, checks `ate-env-api` is up |
| `job-fully-async.yaml` | Ray head Job + worker Job + headless Service, pod anti-affinity, 8 GPUs each |
| `run_fully_async.sh` | runs in both pods: install, download, `ray start`; the head waits for the worker and trains |
| `ate_env_tool.py`, `tool_config.yaml` | verl `BaseTool` that runs code in Substrate environments, warm pool per rollout worker |

## What it covers

| Property | How |
|---|---|
| Agentic | `tool_agent` loop, up to 5 assistant turns, Python tool |
| GRPO, full weights | `adv_estimator=grpo`, n=4, FSDP actor, no LoRA, no offload |
| Async | rollouter streams into a queue, `staleness_threshold=0.5`, `partial_rollout=True`, NCCL weight push every 2 updates |
| Disaggregated | `hybrid_engine=False`; trainer and rollout pools each one whole node, pods anti-affine, so they never share a machine |
| Substrate sandbox | each environment is a Substrate actor; tool-call mean 0.3 to 0.6 s in validation |

## Scope note

MATH instead of SWE-bench because a 1.7B model solves essentially no SWE
instances and GRPO needs mixed outcomes per group. The sandbox is therefore
used only during the rollout; the reward is verl's rule-based MATH scorer, not
hidden tests run in a fresh environment. For both halves, switch to MBPP+ or
difficulty-filtered KodCode and add a verl `custom_reward_function` that runs
hidden tests through the `ate_env` client; nothing else changes.

## Notes

- Jobs do not retry; Spot preemption or node upgrades mean `./deploy.sh up`
  again. Add PVC checkpointing for anything beyond a smoke run.
- Environments outlive the job; `up` and `down` delete them when
  `KUBECTL_ATE` is set, otherwise `kubectl-ate delete actor <id> -a ate-env --any-state`.
- The guest image is Python 3.11 standard library; set `rootfs_image` for
  other images.
