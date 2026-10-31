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
# Fully-async, disaggregated agentic GRPO (verl.experimental.fully_async_policy) across two
# GPU nodes. ROLE=head: start Ray head, wait for NNODES Ray nodes, run the driver. ROLE=worker:
# join the head and block. Needs the ate-env-api of this repository's main (Substrate v0.6.0 or later,
# installed per the Quickstart); the Python client is installed from ATE_ENV_CLIENT_REF. Trainer pool and rollout pool are each one whole node, so verl's
# STRICT_PACK placement groups put the FSDP trainer on one machine and the vLLM rollout engines
# on the other. Rollouts stream into a queue with a staleness bound, partial rollout resumes
# aborted generations after a weight sync, weights move trainer -> vLLM over NCCL.
set -euxo pipefail
export HF_HOME=/workspace/hf PYTHONUNBUFFERED=1 VLLM_USE_V1=1 NCCL_CUMEM_ENABLE=0 NCCL_NVLS_ENABLE=0 HYDRA_FULL_ERROR=1
: "${ROLE:=head}"                     # head | worker
: "${NNODES:=2}"                      # Ray nodes the head waits for before training
: "${NODE_WAIT_S:=3600}"
: "${RAY_HEAD_HOST:=verl-qwen3-1p7b-fully-async-head.default.svc}"
: "${RAY_PORT:=6379}"
: "${VERL_COMMIT:=60546ef2a7464a158cd170f58f852a62a4e552ba}"
: "${MODEL_ID:=Qwen/Qwen3-1.7B}"
: "${NGPUS_TRAIN:=8}"                 # trainer pool (whole node)
: "${NGPUS_ROLLOUT:=8}"               # rollout pool (whole node; vLLM replicas, TP=1 each)
: "${PPO_MINI_BATCH:=16}"             # prompts per local update (global)
: "${N_SAMPLES:=4}"                   # rollouts per prompt
: "${LOCAL_UPDATES:=24}"              # local updates; verl logs one step per SYNC_EVERY of them, so 24 = 12 logged steps =
                                      # performance numbers (1 warmup + 11 measured). 4 = smoke (2 logged steps). Full = one
                                      # epoch = floor(7489 MATH prompts / PPO_MINI_BATCH) = 468 local updates (234 logged steps)
: "${SYNC_EVERY:=2}"                  # trigger_parameter_sync_step
: "${STALENESS:=0.5}"
: "${ATE_ENV_CLIENT_REF:=eedeb63}"      # ai-on-gke/sandboxenv-rl main (clients/python); move to the merge commit of this PR
: "${MODEL_REVISION:=70d244cc86ccca08cf5af4e1e306ecf908b1ad5e}"   # Qwen/Qwen3-1.7B
: "${MATH_REVISION:=a960589a2102b8434523fe247d6edadc12d56a05}"    # verl-team/lighteval-MATH-preprocessed
export MODEL_ID MODEL_REVISION MATH_REVISION                       # read by the download step below
mkdir -p /workspace/results /workspace/agent

echo "=== Phase 1: verl @ $VERL_COMMIT ==="
# Constrain every install to the image's own versions of the stack (torch, transformers, huggingface-hub, ...):
# unconstrained, `datasets` pulls huggingface-hub 2.x (rejected by the image's transformers), cupy 14 pulls
# numpy 2 and TransferQueue pulls a tensordict that wants a newer torch.
pip freeze 2>/dev/null | grep -iE '^(torch|torchvision|torchaudio|numpy|vllm|transformers|huggingface[-_]hub|tensordict|ray|triton|flash[-_]attn|nvidia-|cuda-)' > /workspace/pins.txt || true
pip install -q -c /workspace/pins.txt "cupy-cuda12x==13.6.0" 2>&1 | tail -5     # cupy 14 needs numpy 2; the image pins numpy 1.26
pip install -q --no-deps "TransferQueue==0.1.11" 2>&1 | tail -5                     # its deps (tensordict, pyzmq, msgspec, ...) ship in the image; resolving them backtracks forever
python3 -c "import datasets, tensordict, zmq, msgspec, hydra, omegaconf, prometheus_client, psutil, transfer_queue" 2>/dev/null || python3 -c "import datasets, tensordict, zmq, msgspec, hydra, omegaconf, prometheus_client, psutil"   # datasets 4.8.5 is in the image   # cupy: required by the NCCL checkpoint engine (trainer->vLLM weight sync)
git clone --filter=blob:none https://github.com/verl-project/verl.git /workspace/verl
cd /workspace/verl && git checkout --detach "$VERL_COMMIT"
pip install --no-deps -e . 2>&1 | tail -2
python3 -c "import verl, cupy, verl.experimental.fully_async_policy.fully_async_main; from verl.checkpoint_engine.base import CheckpointEngineRegistry as R; assert 'nccl' in R._registry, R._registry; print('verl', verl.__version__, 'fully_async + nccl checkpoint engine ok')"

echo "=== Phase 2: model + data ==="
python3 - <<'PY'
import os
from huggingface_hub import snapshot_download
snapshot_download(repo_id=os.environ["MODEL_ID"], revision=os.environ["MODEL_REVISION"], local_dir="/workspace/models/" + os.environ["MODEL_ID"].split("/")[-1])
snapshot_download(repo_id="verl-team/lighteval-MATH-preprocessed", repo_type="dataset", revision=os.environ["MATH_REVISION"], local_dir="/workspace/data/math")
PY
MODEL_PATH=/workspace/models/${MODEL_ID##*/}

echo "=== Phase 3: tool + Substrate environment API client ==="
cp /workspace/cfg/*.py /workspace/agent/ 2>/dev/null || true
export PYTHONPATH=/workspace/agent:/workspace/verl:${PYTHONPATH:-}
# Substrate environment API client (gRPC). Pulls grpcio>=1.84 / protobuf>=7.35, which this image tolerates.
pip install -q -c /workspace/pins.txt "git+https://github.com/ai-on-gke/sandboxenv-rl.git@${ATE_ENV_CLIENT_REF}#subdirectory=clients/python" 2>&1 | tail -2
cp /workspace/cfg/tool_config.yaml /workspace/agent/tool_config.yaml
[ "$ROLE" = "head" ] && python3 - <<'PY'     # one probe per run, unique name (head only)
import asyncio, os, socket, yaml
from ate_env import Client
ep = yaml.safe_load(open("/workspace/agent/tool_config.yaml"))["tools"][0]["config"]["endpoint"]
async def probe():
  cfg = yaml.safe_load(open("/workspace/agent/tool_config.yaml"))["tools"][0]["config"]
  c = Client(ep, auth_token=os.environ.get("SUBSTRATE_ENV_AUTH_TOKEN") or None)
  env = await c.create(f"{cfg.get('env_prefix', 'verl')}-probe-{socket.gethostname()[-5:]}"); r = await env.shell("python3 -c 'print(6*7)'", timeout=120); await env.delete(); await c.close()
  assert r.exit_code == 0 and "42" in r.stdout, (r.exit_code, r.stdout, r.stderr); print("ate-env-api probe ok:", ep)
asyncio.run(probe())
PY

NGPUS_LOCAL=$(nvidia-smi -L | wc -l)
if [ "$ROLE" = "worker" ]; then
  echo "=== Phase 4 (worker): join Ray head $RAY_HEAD_HOST:$RAY_PORT with $NGPUS_LOCAL GPUs ==="
  for i in $(seq 1 360); do getent hosts "$RAY_HEAD_HOST" && break || sleep 10; done
  ray start --address="$RAY_HEAD_HOST:$RAY_PORT" --num-gpus="$NGPUS_LOCAL" --block || true   # returns when the head stops
  echo "=== worker done ==="; exit 0
fi

echo "=== Phase 4 (head): Ray head + wait for $NNODES GPU nodes ==="
ray start --head --port="$RAY_PORT" --num-gpus="$NGPUS_LOCAL" --disable-usage-stats
export RAY_ADDRESS="127.0.0.1:$RAY_PORT"      # verl's ray.init() joins this cluster instead of starting a local one
python3 - <<'WAIT'
import os, time, ray
ray.init(address=os.environ["RAY_ADDRESS"], log_to_driver=False)
need, deadline = int(os.environ["NNODES"]), time.time() + int(os.environ["NODE_WAIT_S"])
while True:
    alive = [n for n in ray.nodes() if n["Alive"] and n["Resources"].get("GPU", 0) > 0]
    print(f"ray nodes with GPUs: {len(alive)}/{need}", [(n["NodeManagerAddress"], n["Resources"].get("GPU")) for n in alive], flush=True)
    if len(alive) >= need:
        break
    assert time.time() < deadline, f"only {len(alive)} GPU nodes joined within NODE_WAIT_S"
    time.sleep(15)
WAIT

echo "=== Phase 5: fully-async disaggregated agentic GRPO ==="
TOTAL_ROLLOUT_STEPS=$(( PPO_MINI_BATCH * LOCAL_UPDATES ))
cd /workspace/verl
set +e   # the pipeline's status must reach RC= below; with -e a failed trainer would exit before the DONE line
python3 -m verl.experimental.fully_async_policy.fully_async_main \
  algorithm.adv_estimator=grpo \
  algorithm.use_kl_in_reward=False \
  data.train_files=/workspace/data/math/train.parquet \
  data.val_files=/workspace/data/math/test.parquet \
  data.return_raw_chat=True \
  data.train_batch_size=0 \
  data.gen_batch_size=1 \
  data.max_prompt_length=1024 \
  data.max_response_length=1024 \
  data.filter_overlong_prompts=True \
  +data.apply_chat_template_kwargs.enable_thinking=False \
  actor_rollout_ref.hybrid_engine=False \
  actor_rollout_ref.model.path="$MODEL_PATH" \
  actor_rollout_ref.model.use_remove_padding=True \
  actor_rollout_ref.model.enable_gradient_checkpointing=True \
  actor_rollout_ref.actor.use_kl_loss=False \
  actor_rollout_ref.actor.ppo_mini_batch_size="$PPO_MINI_BATCH" \
  actor_rollout_ref.actor.ppo_micro_batch_size_per_gpu=4 \
  actor_rollout_ref.actor.fsdp_config.param_offload=False \
  actor_rollout_ref.actor.fsdp_config.optimizer_offload=False \
  actor_rollout_ref.ref.log_prob_micro_batch_size_per_gpu=8 \
  actor_rollout_ref.rollout.name=vllm \
  actor_rollout_ref.rollout.mode=async \
  actor_rollout_ref.rollout.tensor_model_parallel_size=1 \
  actor_rollout_ref.rollout.gpu_memory_utilization=0.7 \
  actor_rollout_ref.rollout.n="$N_SAMPLES" \
  actor_rollout_ref.rollout.max_model_len=3072 \
  actor_rollout_ref.rollout.calculate_log_probs=True \
  actor_rollout_ref.rollout.multi_turn.enable=True \
  actor_rollout_ref.rollout.multi_turn.max_assistant_turns=5 \
  actor_rollout_ref.rollout.multi_turn.tool_config_path=/workspace/agent/tool_config.yaml \
  actor_rollout_ref.rollout.agent.default_agent_loop=tool_agent \
  actor_rollout_ref.rollout.agent.num_workers=2 \
  actor_rollout_ref.rollout.log_prob_micro_batch_size_per_gpu=8 \
  trainer.nnodes=1 \
  trainer.n_gpus_per_node="$NGPUS_TRAIN" \
  rollout.nnodes=1 \
  rollout.n_gpus_per_node="$NGPUS_ROLLOUT" \
  rollout.total_rollout_steps="$TOTAL_ROLLOUT_STEPS" \
  async_training.staleness_threshold="$STALENESS" \
  async_training.trigger_parameter_sync_step="$SYNC_EVERY" \
  async_training.require_batches=1 \
  async_training.partial_rollout=True \
  async_training.use_trainer_do_validate=False \
  trainer.val_before_train=False \
  trainer.test_freq=-1 \
  trainer.save_freq=-1 \
  trainer.resume_mode=disable \
  trainer.total_epochs=100 \
  trainer.logger='["console"]' \
  trainer.project_name=verl-agentic \
  trainer.experiment_name=qwen3-1.7b-tool-agent-fully-async-substrate \
  2>&1 | tee /workspace/results/train.log
RC=${PIPESTATUS[0]}
set -e
ray stop --force || true          # releases the worker pod (its ray start --block returns)
echo "=== DONE rc=$RC ==="
exit "$RC"
