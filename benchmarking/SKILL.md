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

### Phase 5: Build & Validate the Report and Bundle
```bash
go run ./cmd/benchmarking/bench report -dest "$ARTIFACT_DIR"
go run ./cmd/benchmarking/bench validate -dest "$ARTIFACT_DIR"
```

---

## 3. Feature Overlays (What They Are, How to Create Them, & What `$ARTIFACT_DIR/overlay/` Must Contain)

A **Feature Overlay** is a self-contained layer of Kubernetes manifest patches, environment toggles, entrypoint hooks, and/or sandbox provider changes applied on top of a baseline RL recipe (`examples/<recipe>/`) or synthetic `scenario.yaml` to evaluate a specific lifecycle or infrastructure feature (e.g., Mode 1 `c36-cap` concurrency cap, Mode 2 `prewarmed` warm pool with `suspend`/`wake`, `snapshot-async`, etc.) **without mutating the baseline recipe in place**.

### 3.1 How to Create a Feature Overlay
1. **For Real Cluster RL Recipes (`examples/<recipe>/`)**:
   - **Gate behavioral changes behind environment variables** (e.g. `ATE_BENCH_PARK_MODE=suspend`, `ATE_WARM_POOL_URL=http://verl-ray-head.default.svc:18999`, `PREWARM_COUNT=32`) so that when unset, the code behaves identically to Mode 0 (`ephemeral` / `on_demand`).
   - **Express Job/ConfigMap overrides as a Kustomize overlay** (`kustomization.yaml` + `job-patch.yaml`) on top of the baseline `job.yaml`.
2. **For Synthetic Scenarios (`benchmarking/scenarios/*.yaml`)**:
   - Define named entries under `overlays:` in `scenario.yaml`, or pass built-in presets (`pool-off`, `pool-on`, `snapshot-sync`, `snapshot-async`, `cold-only`, `warm-pool`) via `bench run -overlay <presetA>,<presetB>`.

### 3.2 What `$ARTIFACT_DIR/overlay/` Must Contain (Enforced by `bench validate -dest`)
Whenever a cluster RL run uses a non-baseline lifecycle policy (`lifecycle_policy.mode != "ephemeral"`, `park_mode == "suspend"`, or `prewarm_at_start == true`), `$ARTIFACT_DIR/overlay/` **must** exist and include:
1. `overlay/overlay.json` (`schema.OverlayManifest`): Structured provenance metadata recording the exact baseline recipe location and hashes that the overlay was built to patch on:
   ```json
   {
     "schema_version": "v1",
     "name": "prewarmed",
     "hash": "sha256:<overlay-patch-sha256>",
     "base_recipe_path": "examples/Qwen3.5-35B-A3B-verl",
     "base_recipe_hash": "sha256:<combined-baseline-files-sha256>",
     "base_recipe_commit": "<git-commit-sha>",
     "patch_file": "overlay/mode0_to_mode2.patch",
     "base_file_hashes": {
       "examples/Qwen3.5-35B-A3B-verl/run_fully_async.sh": "sha256:...",
       "integrations/uni-agent/src/uni_agent_substrate/provider.py": "sha256:...",
       "integrations/uni-agent/src/uni_agent_substrate/tasks/r2e_gym/task.py": "sha256:..."
     },
     "env_overrides": {
       "ATE_BENCH_PARK_MODE": "suspend",
       "ATE_WARM_POOL_URL": "http://verl-ray-head.default.svc:18999",
       "PREWARM_COUNT": "32"
     }
   }
   ```
   `bench report -dest "$ARTIFACT_DIR"` automatically ingests `overlay/overlay.json` into `system_description.json`'s `overlays` array, and `bench validate -dest "$ARTIFACT_DIR"` verifies that `base_recipe_path` and `base_recipe_hash` are present.
2. `overlay/README.md`: Concise summary of the feature overlay, environment variables, and behavioral differences vs. Mode 0.
3. `overlay/kustomization.yaml` & `overlay/job-patch.yaml`: The Kustomize patch applied to the baseline Job.
4. `overlay/mode0_to_mode2.patch` (or `<baseline>_to_<overlay>.patch`): Unified diff (`diff -u` / `git diff`) from the baseline recipe (`examples/<recipe>/` and `integrations/`) to the overlay's runtime files.
5. **Plain-text copies of all modified runtime scripts and Python modules** (e.g. `overlay/run_fully_async.sh`, `overlay/uni_agent_substrate/provider.py`, `overlay/uni_agent_substrate/tasks/...`): Because Kubernetes ConfigMaps often bundle Python packages into a base64 tarball (`binaryData["uni-agent-substrate.tgz"]`), extracting the modified files in plain text under `overlay/` ensures humans and agents can inspect and diff the exact behavioral changes directly in the archived benchmark.

---

## 4. End-to-End Execution Protocol (Synthetic / Scenario Run)

For synthetic scale-ladder runs driven by `scenario.yaml`:
```bash
go run ./cmd/benchmarking/bench run \
  -scenario benchmarking/scenarios/multi-job-swebench.yaml \
  -dest "$ARTIFACT_DIR"
go run ./cmd/benchmarking/bench validate -dest "$ARTIFACT_DIR"
```

---

## 5. Standardized Bundle Layout (`bench validate`) & Cloud Storage Upload (`bench upload`)

`bench validate` and `bench upload` are intentionally separate subcommands so runs can be validated offline without uploading to GCS.

### 5.1 Standardized Bundle Layout Checked by `bench validate -dest "$ARTIFACT_DIR"`

| Category | Required Artifacts & Schema Checks |
| :--- | :--- |
| **Always Required (Every Bundle)** | <ul><li>`report.json` — valid `v1` report + passes semantic validity rules (`reap_seen: true`, `orphan_count: 0`, error rate `< 5%`).</li><li>`report.md` — non-empty rendered Markdown report.</li><li>`system_description.json` — strict `v1` schema with populated `load` (`max_concurrent > 0`, `trajectories_per_step > 0`), `max_concurrent_sandboxes_cap > 0`, and `lifecycle_policy.mode`.</li><li>`measurements.json` — strict `v1` schema.</li><li>`run_window.json` — strict `v1` schema (`start_ts`, `end_ts`, `run_id`).</li><li>`events/*.jsonl` (or `events.jsonl`) — non-empty `v1` `Event` stream.</li></ul> |
| **Cluster RL Runs** *(when `verl-head.log` / `train.log` is present)* | <ul><li>`traces/*.jsonl` — non-empty `v1` `TraceEvent` streams from Ray workers.</li><li>`configmap.yaml` & `job.yaml` (in root or `logs/`) — exact K8s manifests executed.</li><li>`cache_before.{prom,json}` & `cache_after.{prom,json}` — node image-cache snapshots.</li></ul> |
| **Warm / `suspend`-`restore` Runs** *(when `park_mode == "suspend"` or `footing == "warm"`)* | <ul><li>`restore_timings.jsonl` — non-empty `v1` `RestoreSample` sub-phase timings harvested from `atelet`.</li><li>`overlay/` — non-empty directory containing the plain-text feature overlay (`README.md`, Kustomize patch, `.patch` diff, and modified runtime scripts/modules).</li></ul> |
| **Synthetic Scenario Runs** *(`bench run`)* | <ul><li>`schedule.json` — strict `v1` `Schedule`.</li><li>`sut-manifest.yaml` — rendered SUT manifest.</li></ul> |

### 5.2 Validating and Uploading to GCS (`bench upload`)

Pass the target GCS bucket via `-bucket` (never hardcode a bucket name in scripts or code) and use the 4-part folder naming convention `<YYYY-MM-DD>_<model-family>-<size>_<dataset>_<nickname>`:
```bash
# 1. Validate bundle completeness, v1 schemas, and 4-part folder name offline:
go run ./cmd/benchmarking/bench validate \
  -dest "$ARTIFACT_DIR" \
  -folder "2026-10-28_qwen3.5-35b_r2e-gym-easy_prewarmed"

# 2. Upload to GCS (validates the bundle first unless -skip-validate is set):
go run ./cmd/benchmarking/bench upload \
  -dest "$ARTIFACT_DIR" \
  -bucket "$GCS_BENCHMARK_BUCKET" \
  -folder "2026-10-28_qwen3.5-35b_r2e-gym-easy_prewarmed"
```
- **LLM RL runs**: `<model-family>-<size>` = e.g. `qwen3.5-35b`, `qwen3-1.7b`; `<dataset>` = e.g. `r2e-gym-easy`, `swebench-verified`, `lighteval-math`; `<nickname>` = e.g. `baseline`, `prewarmed`, `c36-cap`.
- **Synthetic runs**: `<model-family>-<size>` = `synthetic-w<N>` (where `<N>` is concurrent trajectories/step); `<dataset>` = `<trajectory-type>-<7char-sha256>` of the trajectory YAML (e.g. `math-bb4dcec`); `<nickname>` = e.g. `suspend-sync`, `baseline`.

For scoping telemetry to specific subsystems (`-scopes rl,user-sandbox,substrate,k8s`) or adding custom feature debug metrics (`-plugin-prom`, `-plugin-log`, `-plugin-metric`, `-plugin-cmd`), see [`internal/benchmarking/TELEMETRY_GUIDE.md`](../internal/benchmarking/TELEMETRY_GUIDE.md).
