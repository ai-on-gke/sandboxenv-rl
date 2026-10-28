---
name: rl-sandbox-benchmarking
description: Hooks up RL training recipes (verl, uni-agent, or custom trainers) and synthetic scale-ladder workloads to the cmd/benchmarking/bench harness, collects full 4-scope telemetry (rl, user-sandbox, substrate, k8s), validates artifacts, and archives runs.
---

# RL Sandbox Benchmarking Skill (`cmd/benchmarking/bench`)

Use this skill whenever you are asked to:
1. Hook up a new or existing RL training recipe (e.g. under `examples/`) to the `bench` benchmarking harness.
2. Execute a synthetic scale-ladder or real RL benchmark run on a Kubernetes cluster with Agent Substrate (`ate-env`).
3. Collect complete post-audit telemetry (`report.json`, `report.md`, `system_description.json`, `measurements.json`) and archive the artifacts.

---

## 1. Hooking Up an RL Recipe (`examples/<recipe>/`)

To make any RL recipe compatible with `bench report` without modifying the trainer core:

1. **Enable Passive SDK Trace Emission**:
   Set these three environment variables in the Kubernetes Job manifest (and forward them into Ray worker `runtime_env.env_vars` if using Ray):
   ```yaml
   - name: ATE_ENV_TRACE_DIR
     value: "/workspace/results/traces"
   - name: ATE_BENCH_RUN_ID
     value: "<run-id>"
   - name: ATE_BENCH_JOB_NAME
     value: "<k8s-job-name>"
   ```
   Every `create`, `ttfi`, `exec`, `read_file`, `write_file`, `suspend`, `wake`, and `delete` operation must append a `v1` JSONL record to `$ATE_ENV_TRACE_DIR/<worker>.jsonl`.

2. **Tag In-Sandbox Grading vs. Rollout Tool Calls**:
   When the reward executor or evaluation harness runs final test grading inside the sandbox (e.g. `run_tests.sh` or `/eval.sh`), tag that trace record with:
   ```python
   tags={"phase": "grade"}
   ```
   This allows `bench report` to separate `step/grade` latency from rollout `phase/agent_exec` calls.

3. **Gather Distributed Traces & Hold Before Pod Exit**:
   At the end of the entrypoint script (before `ray stop` and pod termination):
   - Gather `$ATE_ENV_TRACE_DIR/*.jsonl` from all Ray worker nodes onto the head pod's `/workspace/results/traces/`.
   - Tee the trainer's stdout/stderr to `/workspace/results/train.log`.
   - Hold the head pod briefly (`for _ in $(seq 1 360); do [ -f /tmp/harvested ] && break; sleep 5; done`) so the benchmark driver can `kubectl cp` `/workspace/results/train.log` and `/workspace/results/traces/` before the pod exits.

---

## 2. End-to-End Execution Protocol (Real RL Recipe)

Always execute all five phases so no telemetry section (`restores`, `cache`, `reap`, `prom_metrics`, `max_concurrent_sandboxes_cap`, `active/paused_sandboxes`, `lifecycle_policy`) is null:

### Phase 1: Pre-Run Cluster Check, Substrate Spin-Up, & Node Cache Baseline
1. Verify `ate-env-api` in namespace `ate-env` is configured for the target concurrency and lifecycle mode:
   - **Mode 1 (`baseline` ephemeral)**: `-max-active-pods` $\ge$ `trajectories_per_step` (e.g. `160` for `128` trajectories/step), `-idle-park-timeout=0s`.
   - **Mode 2 (`prewarmed` / warm pool)**: set `-idle-park-timeout` / warm pool flags as required, and run `bench prewarm` if prewarming templates at job start.
2. Create `$ARTIFACT_DIR`, time the cold `0 -> N` spin-up of Substrate's `WorkerPool` (`workerpool.ate.dev/default-template-workerpool`), and capture the pre-run node image-cache counters:
   ```bash
   mkdir -p "$ARTIFACT_DIR/measurements_snapshots"
   # If workerpool is already scaled up and you want a clean cold 0->N spin-up timing, run `bench spindown` first.
   go run ./cmd/benchmarking/bench spinup \
     -namespace ate-env \
     -workerpool default-template-workerpool \
     -replicas <N> \
     -dest "$ARTIFACT_DIR"
   go run ./cmd/benchmarking/bench measure -sample-node-cache "$ARTIFACT_DIR/cache_before.prom"
   ```
   `bench spinup` records `workerpool_spinup_wall_ms`, per-`ateworker`-pod startup sub-phases (`worker_pod_schedule_ms`, `worker_pod_init_ms`, `worker_pod_container_start_ms`, `worker_pod_total_ready_ms`), and `worker_pod_count` into `$ARTIFACT_DIR/measurements.json` and `$ARTIFACT_DIR/events/substrate_lifecycle.jsonl`.

### Phase 2: Launch the RL Job
```bash
(cd examples/<recipe> && ./deploy.sh up)
```

### Phase 3: Sample Live Cluster & Substrate Telemetry During Active Steps
**Critical**: You must run `bench measure -k8s -ate-endpoint ...` *while sandboxes are actively running* (not just after the job finishes), otherwise `active_sandboxes` and live pod CPU/memory WSS will read `0`:
```bash
kubectl -n ate-env port-forward deploy/ate-env-api 18443:8443 &
PF_PID=$!

# Run periodically (e.g. every 5-10 minutes) while the RL job is in flight:
go run ./cmd/benchmarking/bench measure \
  -k8s \
  -ate-endpoint 127.0.0.1:18443 \
  -atespace ate-env \
  -run-id "<run-id>" \
  -dest "$ARTIFACT_DIR"
cp "$ARTIFACT_DIR/measurements.json" \
   "$ARTIFACT_DIR/measurements_snapshots/meas_$(date -u +%Y%m%dT%H%M%SZ).json"
```

### Phase 4: Harvest Logs/Traces, Reap Orphans, & Capture Post-Run Counters
Once the head pod logs `=== DONE rc=0 ===`:
```bash
HEAD_POD=$(kubectl -n default get pods -l app=<k8s-job-name>,role=head -o jsonpath='{.items[0].metadata.name}')
kubectl -n default cp "$HEAD_POD:/workspace/results/train.log" "$ARTIFACT_DIR/verl-head.log"
kubectl -n default cp "$HEAD_POD:/workspace/results/traces" "$ARTIFACT_DIR/traces"
kubectl -n default exec "$HEAD_POD" -- touch /tmp/harvested

# Post-run orphan check (writes $ARTIFACT_DIR/events/reap.jsonl)
go run ./cmd/benchmarking/bench reap \
  -ate-endpoint 127.0.0.1:18443 \
  -atespace ate-env \
  -prefix "<env-prefix>" \
  -dest "$ARTIFACT_DIR"

# Post-run node image-cache snapshot, restore sub-phases, & WorkerPool spin-down (N -> 0)
go run ./cmd/benchmarking/bench measure -sample-node-cache "$ARTIFACT_DIR/cache_after.prom"
benchmarking/swebench/harvest-restore-timings.sh "<run-id>" "$ARTIFACT_DIR/restore_timings.jsonl"
go run ./cmd/benchmarking/bench spindown \
  -namespace ate-env \
  -workerpool default-template-workerpool \
  -dest "$ARTIFACT_DIR"
kill "$PF_PID" 2>/dev/null || true
```
`bench spindown` scales `workerpool/default-template-workerpool` to `0`, waits until all `ateworker` pods terminate, and records `workerpool_spindown_wall_ms`, `worker_pod_terminate_ms`, and `workerpool_drained_pods` into `$ARTIFACT_DIR/measurements.json` and `$ARTIFACT_DIR/events/substrate_lifecycle.jsonl`.

### Phase 5: Build & Validate the Report
```bash
go run ./cmd/benchmarking/bench report -dest "$ARTIFACT_DIR"
go run ./cmd/benchmarking/bench validate -dest "$ARTIFACT_DIR"
```

---

## 3. End-to-End Execution Protocol (Synthetic / Scenario Run)

For synthetic scale-ladder runs driven by `scenario.yaml`:
```bash
go run ./cmd/benchmarking/bench run \
  -scenario benchmarking/scenarios/multi-job-swebench.yaml \
  -dest "$ARTIFACT_DIR"
go run ./cmd/benchmarking/bench validate -dest "$ARTIFACT_DIR"
```

---

## 4. Verification Checklist & Cloud Storage Archival

Before declaring a benchmark run complete, verify in `$ARTIFACT_DIR`:
1. `system_description.json` contains populated `load` (`max_concurrent`, `trajectories_per_step`), `max_concurrent_sandboxes_cap`, and `lifecycle_policy` (`mode`, `park_mode`, `idle_auto_park_enabled`, `prewarm_at_start`, `per_step_warming`).
2. `report.json` contains `reap_seen: true` (`orphan_count: 0`), `hardware_cost.active_sandboxes`, `hardware_cost.paused_sandboxes`, and `convergence.curve` (for RL runs).
3. Recommend uploading `$ARTIFACT_DIR` to the user's benchmark storage bucket using the 4-part folder naming convention:
   ```text
   gs://<YOUR_BENCHMARK_BUCKET>/<YYYY-MM-DD>_<model-family>-<size>_<dataset>_<nickname>/
   ```
   - **LLM RL runs**: `<model-family>-<size>` = e.g. `qwen3.5-35b`, `qwen3-1.7b`; `<dataset>` = e.g. `r2e-gym-easy`, `swebench-verified`, `lighteval-math`; `<nickname>` = e.g. `baseline`, `prewarmed`, `c36-cap`.
   - **Synthetic runs**: `<model-family>-<size>` = `synthetic-w<N>` (where `<N>` is concurrent trajectories/step); `<dataset>` = `<trajectory-type>-<7char-sha256>` of the trajectory YAML (e.g. `math-bb4dcec`); `<nickname>` = e.g. `suspend-sync`, `baseline`.

For scoping telemetry to specific subsystems (`-scopes rl,user-sandbox,substrate,k8s`) or adding custom feature debug metrics (`-plugin-prom`, `-plugin-log`, `-plugin-metric`, `-plugin-cmd`), see [`internal/benchmarking/TELEMETRY_GUIDE.md`](../internal/benchmarking/TELEMETRY_GUIDE.md).
