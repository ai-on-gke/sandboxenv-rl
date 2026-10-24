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
# Runs in both pods of job-fully-async.yaml.
#   ROLE=worker : install, download the model, join the Ray head, block until the head stops Ray.
#   ROLE=head   : install, download the model, build the datasets, probe ate-env-api, start Ray, wait for
#                 the worker node, then train with verl v1 separate_async:
#                 Megatron trainer (TP2 EP8, whole node) + vLLM rollout engines (whole other node),
#                 uni-agent ReAct agent whose tools and reward executor run in Substrate environments.
set -euo pipefail
export PYTHONUNBUFFERED=1 HF_HOME=/workspace/hf HF_HUB_ENABLE_HF_TRANSFER=1 CUDA_DEVICE_MAX_CONNECTIONS=1 VLLM_USE_V1=1 \
       VLLM_ALLREDUCE_USE_SYMM_MEM=0 VLLM_DISABLE_COMPILE_CACHE=1 TORCH_NCCL_AVOID_RECORD_STREAMS=1 NCCL_SOCKET_IFNAME=eth0 HYDRA_FULL_ERROR=1
: "${ROLE:=head}"; : "${NNODES:=2}"; : "${NODE_WAIT_S:=3600}"; : "${RAY_HEAD_HOST:=verl-ray-head.default.svc}"; : "${RAY_PORT:=6379}"
: "${UNI_AGENT_REF:=174b040}"                                     # uni-agent commit; its verl submodule is the verl pin (3efe38c7)
: "${ATE_ENV_CLIENT_REF:=2eccd89617ff704bb74b9ca356a24548987dd380}"
: "${MODEL_ID:=Qwen/Qwen3.5-35B-A3B}"
: "${TRAIN_SOURCE:=r2e_gym_easy}"; : "${TRAIN_SPLIT:=train}"; : "${TRAIN_INSTANCES:=64}"; : "${TRAIN_DATASET:=}"
: "${VAL_TASKS:=20}"
: "${TRAIN_BATCH:=16}"; : "${ROLLOUT_N:=8}"; : "${MINI_BATCH:=8}"; : "${SYNC_STEP:=2}"
: "${SESSIONS:=24}"; : "${GATEWAYS:=2}"; : "${AGENT_WORKERS:=2}"; : "${TOTAL_STEPS:=2}"
: "${PROMPT_LENGTH:=8192}"; : "${RESPONSE_LENGTH:=32768}"; : "${TRAIN_TP:=2}"; : "${TRAIN_CP:=1}"; : "${TRAIN_EP:=8}"; : "${ROLLOUT_TP:=2}"
: "${ROLLOUT_HYBRID_FLAGS:=1}"           # vLLM flags for the hybrid (GDN) model; set 0 if your vLLM rejects them
: "${ATE_ENV_ENDPOINT:=ate-env-api.ate-env.svc:7777}"; : "${ATE_TEMPLATE:=swe-2c6g}"
MAX_MODEL_LEN=$((PROMPT_LENGTH + RESPONSE_LENGTH))
mkdir -p /workspace/results /workspace/agent /workspace/data
cd /workspace/results   # neutral cwd: the image's working directory holds a docker/ folder that shadows the docker package

echo "=== Phase 1: install (uni-agent@$UNI_AGENT_REF + its verl submodule, Substrate client + provider) ==="
pip freeze 2>/dev/null | grep -iE '^(torch|torchvision|torchaudio|numpy|vllm|transformers|ray|megatron|triton|flash[-_]attn|transformer[-_]engine|nvidia-|cuda-)' > /workspace/pins.txt || true
git clone -q --filter=blob:none https://github.com/verl-project/uni-agent /workspace/uni-agent
( cd /workspace/uni-agent && git checkout -q "$UNI_AGENT_REF" && git submodule update --init --depth 1 verl 2>&1 | tail -1 && echo "verl submodule: $(git -C verl rev-parse --short HEAD)" )
pip install -q --no-deps -e /workspace/uni-agent/verl 2>&1 | tail -1
pip install -q -c /workspace/pins.txt --ignore-requires-python -e /workspace/uni-agent pydantic openai pyyaml httpx jsonschema fastapi 2>&1 | tail -1
pip install -q -c /workspace/pins.txt flash-linear-attention cupy-cuda12x datasets pyarrow 2>&1 | tail -1
# nvidia-modelopt (imported by the image's Megatron-Bridge) and swebench pull dependency trees that a
# full resolve would break the image over (it uninstalls pinned wheels): install them without deps,
# then their leaf dependencies under the image's pins.
pip install -q --no-deps nvidia-modelopt "swebench==4.1.0" 2>&1 | tail -1
pip install -q -c /workspace/pins.txt pulp scipy docker beautifulsoup4 ghapi unidiff tenacity gitpython chardet python-dotenv rich modal 2>&1 | tail -1   # docker: the image ships a partial module without docker.errors
python3 - <<'PY'
import importlib, subprocess, sys, traceback
PIP = {"modelopt": "nvidia-modelopt", "fla": "flash-linear-attention", "cupy": "cupy-cuda12x", "yaml": "pyyaml", "PIL": "pillow",
       "sklearn": "scikit-learn", "bs4": "beautifulsoup4", "git": "gitpython", "dotenv": "python-dotenv", "dateutil": "python-dateutil"}
for mod in ("megatron.bridge.models.conversion.auto_bridge", "swebench.harness.grading", "swebench.harness.test_spec.python",
            "fla", "cupy", "uni_agent.agents.react.agent", "verl.trainer.main_ppo"):
    tried = set()
    while True:
        try:
            importlib.import_module(mod); print("import ok:", mod, flush=True); break
        except ModuleNotFoundError as e:
            missing = (e.name or "").split(".")[0]
            pkg = PIP.get(missing, missing)
            if not missing or missing == mod.split(".")[0] or pkg in tried:
                traceback.print_exc(); raise SystemExit(f"cannot satisfy imports for {mod} (missing {missing!r})")
            tried.add(pkg); print(f"{mod}: installing missing leaf {pkg}", flush=True)
            subprocess.check_call([sys.executable, "-m", "pip", "install", "-q", "-c", "/workspace/pins.txt", pkg])
PY
pip install -q "git+https://github.com/ai-on-gke/sandboxenv-rl.git@${ATE_ENV_CLIENT_REF}#subdirectory=clients/python" 2>&1 | tail -1
tar -C /workspace/agent -xzf /workspace/cfg/uni-agent-substrate.tgz && pip install -q --no-deps -e /workspace/agent/uni-agent 2>&1 | tail -1
echo "import uni_agent_substrate._register" > "$(python3 -c 'import site; print(site.getsitepackages()[0])')/uni_agent_substrate.pth"
python3 - <<'PY'
import importlib
for m in ("verl.trainer.main_ppo", "fla", "megatron.bridge", "cupy", "swebench", "ate_env", "uni_agent_substrate.provider"):
    importlib.import_module(m)
from uni_agent.sandbox.registry import get_sandbox_cls; from uni_agent.tasks.registry import get_task_cls
import verl, vllm, transformers
print("ok:", "verl", verl.__version__, "vllm", vllm.__version__, "transformers", transformers.__version__,
      get_sandbox_cls("ate_env").__name__, get_task_cls("r2e_gym").__name__)
PY

echo "=== Phase 2: model $MODEL_ID ==="
MODEL_PATH=/workspace/models/${MODEL_ID##*/}
python3 - <<PY
from huggingface_hub import snapshot_download
snapshot_download(repo_id="$MODEL_ID", local_dir="$MODEL_PATH")
print("model ready: $MODEL_PATH")
PY

# Both pods: the uni-agent task runners are Ray tasks and run on either node, so every node needs the
# task config and the image digest map at the same paths.
cp /workspace/cfg/images.json /workspace/agent/images.json
export ATE_ENV_IMAGE_DIGESTS=/workspace/agent/images.json
sed -e "s|\${ATE_ENV_ENDPOINT}|$ATE_ENV_ENDPOINT|g" -e "s|\${ATE_TEMPLATE}|$ATE_TEMPLATE|g" /workspace/cfg/task_config.yaml > /workspace/agent/task_config.yaml

if [ "$ROLE" = "worker" ]; then
  echo "=== Phase 3 (worker): join Ray head $RAY_HEAD_HOST:$RAY_PORT ==="
  for i in $(seq 1 360); do getent hosts "$RAY_HEAD_HOST" >/dev/null && break || sleep 10; done
  ray start --address="$RAY_HEAD_HOST:$RAY_PORT" --num-gpus=8 --num-cpus=40 --block || true   # returns when the head stops Ray
  echo "=== worker done ==="; exit 0
fi

echo "=== Phase 3 (head): datasets, ate-env-api probe ==="
python3 -m uni_agent_substrate.datasets --source "$TRAIN_SOURCE" --split "$TRAIN_SPLIT" ${TRAIN_DATASET:+--dataset "$TRAIN_DATASET"} \
    --max-instances "$TRAIN_INSTANCES" --image-digests /workspace/agent/images.json --out /workspace/data/train.parquet
python3 -m uni_agent_substrate.datasets --source swe_bench --max-instances "$VAL_TASKS" --exclude-file /workspace/cfg/offline-exclusions.txt \
    --image-digests /workspace/agent/images.json --out /workspace/data/val.parquet
python3 - <<PY
import asyncio
from ate_env import Client
async def probe():
    c = Client("$ATE_ENV_ENDPOINT"); env = await c.create("ua-probe"); r = await env.shell("echo 42", timeout=120); await env.delete(); await c.close()
    assert r.exit_code == 0 and "42" in r.stdout, (r.exit_code, r.stdout, r.stderr); print("ate-env-api probe ok: $ATE_ENV_ENDPOINT")
asyncio.run(probe())
PY

echo "=== Phase 4: Ray head + wait for $NNODES GPU nodes ==="
ray start --head --port="$RAY_PORT" --num-gpus=8 --num-cpus=40 --disable-usage-stats
export RAY_ADDRESS="127.0.0.1:$RAY_PORT"
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

echo "=== Phase 5: verl separate_async (Megatron TP$TRAIN_TP CP$TRAIN_CP EP$TRAIN_EP | vLLM TP$ROLLOUT_TP x $((8 / ROLLOUT_TP))) ==="
[ $((SYNC_STEP * MINI_BATCH)) -eq "$TRAIN_BATCH" ] || { echo "TRAIN_BATCH must equal SYNC_STEP * MINI_BATCH"; exit 2; }
[ $((TRAIN_TP * TRAIN_CP)) -le 8 ] && [ "$TRAIN_EP" -le 8 ] || { echo "TP*CP and EP must stay within one 8-GPU node (no RDMA)"; exit 2; }
TOKENS_PER_GPU=$((MAX_MODEL_LEN / TRAIN_CP))
HYBRID=()
if [ "$ROLLOUT_HYBRID_FLAGS" = "1" ]; then
  HYBRID=(+actor_rollout_ref.rollout.engine_kwargs.vllm.compilation_config.cudagraph_mode=FULL_DECODE_ONLY
          +actor_rollout_ref.rollout.engine_kwargs.vllm.mamba_cache_mode=align)
fi
cd /workspace/results   # not the uni-agent checkout: its docker/ folder would shadow the docker package for Ray workers
OVERRIDES=(
  model_engine=megatron
  '+ray_kwargs.ray_init.runtime_env.env_vars.HF_HUB_OFFLINE="1"'
  '+ray_kwargs.ray_init.runtime_env.env_vars.NCCL_SOCKET_IFNAME="eth0"'
  '+ray_kwargs.ray_init.runtime_env.env_vars.SANDBOX_STARTUP_CONCURRENCY="32"'
  "+ray_kwargs.ray_init.runtime_env.env_vars.ATE_ENV_IMAGE_DIGESTS=$ATE_ENV_IMAGE_DIGESTS"
  '+ray_kwargs.ray_init.runtime_env.env_vars.VLLM_DISABLE_COMPILE_CACHE="1"'
  '+ray_kwargs.ray_init.runtime_env.env_vars.CUDA_DEVICE_MAX_CONNECTIONS="1"'
  trainer.use_v1=True
  trainer.v1.trainer_mode=separate_async
  trainer.v1.separate_async.num_warmup_batches=1
  trainer.v1.separate_async.parameter_sync_step="$SYNC_STEP"
  trainer.v1.sampler.max_off_policy_threshold=16
  transfer_queue.enable=True
  actor_rollout_ref.nccl_timeout=9600
  actor_rollout_ref.model.path="$MODEL_PATH"
  actor_rollout_ref.model.trust_remote_code=True
  actor_rollout_ref.model.use_remove_padding=False
  +actor_rollout_ref.model.override_config.model_config.max_position_embeddings=$MAX_MODEL_LEN
  data.train_files=/workspace/data/train.parquet
  data.val_files=/workspace/data/val.parquet
  data.prompt_key=prompt
  data.return_raw_chat=True
  data.truncation=left
  data.filter_overlong_prompts=True
  data.trust_remote_code=True
  data.dataloader_num_workers=0
  data.max_prompt_length=$PROMPT_LENGTH
  data.max_response_length=$RESPONSE_LENGTH
  data.train_batch_size="$TRAIN_BATCH"
  data.val_batch_size="$VAL_TASKS"
  actor_rollout_ref.rollout.name=vllm
  actor_rollout_ref.rollout.mode=async
  actor_rollout_ref.rollout.dtype=bfloat16
  actor_rollout_ref.rollout.nnodes=1
  actor_rollout_ref.rollout.n_gpus_per_node=8
  actor_rollout_ref.rollout.tensor_model_parallel_size="$ROLLOUT_TP"
  actor_rollout_ref.rollout.gpu_memory_utilization=0.8
  actor_rollout_ref.rollout.n="$ROLLOUT_N"
  actor_rollout_ref.rollout.prompt_length=$PROMPT_LENGTH
  actor_rollout_ref.rollout.response_length=$RESPONSE_LENGTH
  actor_rollout_ref.rollout.max_model_len=$MAX_MODEL_LEN
  actor_rollout_ref.rollout.max_num_batched_tokens=$MAX_MODEL_LEN
  actor_rollout_ref.rollout.enable_chunked_prefill=True
  actor_rollout_ref.rollout.calculate_log_probs=True
  actor_rollout_ref.rollout.enforce_eager=False
  actor_rollout_ref.rollout.temperature=1.0
  actor_rollout_ref.rollout.top_p=1.0
  actor_rollout_ref.rollout.val_kwargs.n=1
  actor_rollout_ref.rollout.val_kwargs.do_sample=True
  actor_rollout_ref.rollout.val_kwargs.temperature=1.0
  actor_rollout_ref.rollout.val_kwargs.top_p=0.95
  actor_rollout_ref.rollout.checkpoint_engine.backend=nccl
  actor_rollout_ref.rollout.checkpoint_engine.update_weights_bucket_megabytes=2048
  actor_rollout_ref.rollout.log_prob_micro_batch_size_per_gpu=1
  actor_rollout_ref.rollout.log_prob_use_dynamic_bsz=False
  actor_rollout_ref.rollout.log_prob_max_token_len_per_gpu=$TOKENS_PER_GPU
  "${HYBRID[@]}"
  actor_rollout_ref.rollout.multi_turn.enable=True
  actor_rollout_ref.rollout.multi_turn.max_parallel_calls=1
  ++actor_rollout_ref.rollout.multi_turn.format=qwen3_coder
  actor_rollout_ref.rollout.agent.num_workers="$AGENT_WORKERS"
  ++actor_rollout_ref.rollout.agent.agent_loop_manager_class=uni_agent.framework.entry.AgentFrameworkRolloutAdapter
  ++actor_rollout_ref.rollout.custom.agent_framework.gateway_count="$GATEWAYS"
  ++actor_rollout_ref.rollout.custom.agent_framework.log_dir=/workspace/results/agent-logs
  ++actor_rollout_ref.rollout.custom.agent_framework.allowed_request_sampling_param_keys="[temperature,top_p,top_k]"
  ++actor_rollout_ref.rollout.custom.agent_framework.agent_runners.task.runner_fqn=uni_agent.framework.task_runner.run_task
  ++actor_rollout_ref.rollout.custom.agent_framework.agent_runners.task.dispatch_mode=ray_task
  ++actor_rollout_ref.rollout.custom.agent_framework.agent_runners.task.max_concurrent_sessions="$SESSIONS"
  ++actor_rollout_ref.rollout.custom.agent_framework.agent_runners.task.session_timeout_seconds=2400
  ++actor_rollout_ref.rollout.custom.agent_framework.agent_runners.task.trajectory_selection=longest
  ++actor_rollout_ref.rollout.custom.agent_framework.agent_runners.task.runner_kwargs.task_config_path=/workspace/agent/task_config.yaml
  "++actor_rollout_ref.rollout.custom.agent_framework.agent_runners.task.runner_kwargs.model_name=${MODEL_ID##*/}"
  ++actor_rollout_ref.rollout.custom.agent_framework.mask_unfinished_episode=True
  trainer.nnodes=1
  trainer.n_gpus_per_node=8
  actor_rollout_ref.actor.megatron.use_mbridge=True
  actor_rollout_ref.actor.megatron.vanilla_mbridge=True
  actor_rollout_ref.actor.megatron.tensor_model_parallel_size="$TRAIN_TP"
  actor_rollout_ref.actor.megatron.pipeline_model_parallel_size=1
  actor_rollout_ref.actor.megatron.context_parallel_size="$TRAIN_CP"
  actor_rollout_ref.actor.megatron.expert_model_parallel_size="$TRAIN_EP"
  actor_rollout_ref.actor.megatron.expert_tensor_parallel_size=1
  actor_rollout_ref.actor.megatron.use_remove_padding=False
  actor_rollout_ref.actor.megatron.param_offload=True
  actor_rollout_ref.actor.megatron.optimizer_offload=True
  actor_rollout_ref.actor.megatron.dtype=bfloat16
  ++actor_rollout_ref.actor.megatron.override_transformer_config.attention_backend=auto
  +actor_rollout_ref.actor.megatron.override_transformer_config.moe_token_dispatcher_type=alltoall
  +actor_rollout_ref.actor.megatron.override_transformer_config.moe_grouped_gemm=True
  +actor_rollout_ref.actor.megatron.override_transformer_config.moe_permute_fusion=True
  +actor_rollout_ref.actor.megatron.override_transformer_config.recompute_method=uniform
  +actor_rollout_ref.actor.megatron.override_transformer_config.recompute_granularity=full
  +actor_rollout_ref.actor.megatron.override_transformer_config.recompute_num_layers=1
  actor_rollout_ref.actor.use_dynamic_bsz=False
  actor_rollout_ref.actor.ppo_micro_batch_size_per_gpu=1
  actor_rollout_ref.actor.ppo_max_token_len_per_gpu=$TOKENS_PER_GPU
  actor_rollout_ref.actor.ppo_mini_batch_size="$MINI_BATCH"
  actor_rollout_ref.actor.optim.lr=1e-6
  actor_rollout_ref.actor.optim.lr_decay_style=constant
  +actor_rollout_ref.actor.optim.override_optimizer_config.optimizer_offload_fraction=1.0
  +actor_rollout_ref.actor.optim.override_optimizer_config.optimizer_cpu_offload=True
  +actor_rollout_ref.actor.optim.override_optimizer_config.overlap_cpu_optimizer_d2h_h2d=True
  +actor_rollout_ref.actor.optim.override_optimizer_config.use_precision_aware_optimizer=True
  actor_rollout_ref.actor.use_kl_loss=False
  actor_rollout_ref.actor.entropy_coeff=0
  actor_rollout_ref.actor.loss_agg_mode=token-mean
  actor_rollout_ref.actor.checkpoint.strict=False
  algorithm.rollout_correction.rollout_is=token
  algorithm.rollout_correction.rollout_is_threshold=2.0
  actor_rollout_ref.ref.megatron.param_offload=True
  actor_rollout_ref.ref.megatron.tensor_model_parallel_size="$TRAIN_TP"
  actor_rollout_ref.ref.megatron.pipeline_model_parallel_size=1
  actor_rollout_ref.ref.megatron.context_parallel_size="$TRAIN_CP"
  actor_rollout_ref.ref.megatron.expert_model_parallel_size="$TRAIN_EP"
  actor_rollout_ref.ref.megatron.expert_tensor_parallel_size=1
  actor_rollout_ref.ref.log_prob_micro_batch_size_per_gpu=1
  actor_rollout_ref.ref.log_prob_use_dynamic_bsz=False
  actor_rollout_ref.ref.log_prob_max_token_len_per_gpu=$TOKENS_PER_GPU
  algorithm.adv_estimator=grpo
  algorithm.use_kl_in_reward=False
  reward.reward_manager.name=dapo
  reward.custom_reward_function.path=pkg://uni_agent.framework.task_runner
  reward.custom_reward_function.name=score_from_runner_result
  trainer.val_before_train=True
  trainer.test_freq="$TOTAL_STEPS"
  trainer.total_epochs=1
  trainer.total_training_steps="$TOTAL_STEPS"
  trainer.save_freq=-1
  trainer.resume_mode=disable
  trainer.rollout_data_dir=/workspace/results/rollouts
  trainer.validation_data_dir=/workspace/results/val-rollouts
  trainer.logger='["console"]'
  trainer.project_name=verl-agentic-swe
  trainer.experiment_name=qwen3.5-35b-a3b-react-substrate
)
if [ "${DRY_RUN:-0}" = "1" ]; then printf '%s\n' "${OVERRIDES[@]}"; exit 0; fi
python3 -m verl.trainer.main_ppo --config-name=ppo_megatron_trainer "${OVERRIDES[@]}" \
  2>&1 | tee /workspace/results/train.log
RC=${PIPESTATUS[0]}
ray stop --force || true          # releases the worker pod
echo "=== DONE rc=$RC ==="
