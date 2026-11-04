---
name: r2e-swebench-simulated-benchmark
description: >-
  Configures and runs a simulated mixed R2E-Gym and SWE-bench Verified benchmark on Agent Substrate
  and ate-env with user-selected concurrency, image counts, and replica rollouts per image, then
  verifies unified multi-job report artifacts.
---

# Mixed R2E-Gym and SWE-bench Simulated Benchmark

Use this skill when asked to run a simulated RL load mixing R2E-Gym and SWE-bench Verified task
images on Agent Substrate without GPUs or model servers.
[`../run-r2e-swebench.sh`](../run-r2e-swebench.sh) drives two concurrent `swebench-loader` shards
(`job=r2e` and `job=swe`) into one `$ARTIFACT_DIR` and builds a combined multi-job report with
`bench report`.

For the 8 cluster checks that affect every `swebench-loader` measurement, read
[`../SKILL.md`](../SKILL.md). For real RL trainer hooks and `bench` overlays, read
[`../../SKILL.md`](../../SKILL.md). For event definitions and run shapes, read
[`../README.md`](../README.md).

## 1. Choose parameters and check cluster capacity

Run the 8 cluster checks in [`../SKILL.md`](../SKILL.md) before starting. Ensure the Substrate
`WorkerPool` satisfies `replicas * per_worker_actor_capacity >= MAX_CONCURRENCY`, with CPU and
memory headroom when `--workload graded` runs concurrent `pytest` suites.

Pick the three sizing knobs with CLI flags or environment variables (CLI flags override environment
variables):

| Parameter             | CLI flags                                                                                                  | Environment variables                             | Default and resolution rule                                                                          |
| --------------------- | ---------------------------------------------------------------------------------------------------------- | ------------------------------------------------- | ---------------------------------------------------------------------------------------------------- |
| Concurrency           | `-c N`, `--concurrency N`, `--r2e-width N`, `--swe-width N`                                                | `MAX_CONCURRENCY`, `R2E_WIDTH`, `SWE_WIDTH`       | `200` total active sandboxes split proportionally across `r2e` and `swe` shards (`>= 2` when split). |
| Images                | `-i N`, `--images N`, `--r2e-images N`, `--swe-images N`                                                   | `IMAGES`, `R2E_IMAGES`, `SWE_IMAGES`              | `20` per dataset (`40` total images). Pass `--images 10` for `20` total (`10` R2E + `10` SWE-bench). |
| Rollouts per image    | `-r N`, `--rollouts-per-image N`, `--r2e-rollouts N`, `--swe-rollouts N`                                   | `ROLLOUTS_PER_IMAGE`, `R2E_FANOUT`, `SWE_FANOUT`  | Auto-computed as `ceil(shard_width / shard_images)` when omitted (`5` for `width=100, images=20`).   |
| Workload and schedule | `--workload graded\|io`, `--mode steps\|eval\|open-loop`, `--steps N`, `--park POLICY`, `--step-pause DUR` | `WORKLOAD`, `MODE`, `STEPS`, `PARK`, `STEP_PAUSE` | `WORKLOAD=graded`, `MODE=steps`, `STEPS=4`, `PARK=suspend`, `STEP_PAUSE=0s`.                         |

[`../run-r2e-swebench.sh`](../run-r2e-swebench.sh) resolves parameter combinations with three rules:

1. **Concurrency set (or defaulted) without `--rollouts-per-image`**: splits `MAX_CONCURRENCY`
   across both shards and sets each shard's replica rollouts per image to
   `ceil(shard_width / shard_images)`.
2. **`--rollouts-per-image` set without `--concurrency`**: sets each shard's active width to
   `shard_images * shard_rollouts` and sets `MAX_CONCURRENCY` to `R2E_WIDTH + SWE_WIDTH`.
3. **Both `--concurrency` and `--rollouts-per-image` set**: caps concurrent active sandboxes to
   `shard_width` while keeping `shard_images * shard_rollouts` replica rollouts per shard. When
   `shard_images * shard_rollouts > shard_width` in `steps` mode,
   [`../run-r2e-swebench.sh`](../run-r2e-swebench.sh) adds `-park-each` (requiring `--park suspend`
   or `--park delete` and `--step-pause 0s`) so each sandbox parks right after its episode and awake
   sandboxes stay within `--concurrency`.

Select the trajectory profile with `--workload`:

- `graded`: replays [`../r2e-gold.yaml`](../r2e-gold.yaml) on `job=r2e` and `swebench-gold` on
  `job=swe`. Both trajectories run `explore` and `read_files` steps, simulated think pauses (`1s`,
  `2s`, `3s`), gold patch application, `pytest` evaluation, and reward verification.
- `io`: replays `IO_TRAJECTORY` (default `workload-file-read`) on both shards to measure image pull,
  snapshot restore, and filesystem read latency without running `pytest`.

## 2. Build digest-pinned manifests outside the repository

Write manifests and run outputs outside the git checkout so `ko build` reads a clean working tree:

```bash
export ENV_REPO=/path/to/sandboxenv-rl
export PROJECT=YOUR_GCP_PROJECT
export REGION=us-central1
export DOCKERHUB_REMOTE="${REGION}-docker.pkg.dev/${PROJECT}/dockerhub-remote"
export SWEBENCH_REPO="${REGION}-docker.pkg.dev/${PROJECT}/swebench/swebench-verified"
export MANIFEST_DIR=/tmp/r2eswe-manifests
mkdir -p "${MANIFEST_DIR}"
```

Set `R2E_LIMIT` to at least your chosen `--r2e-images` count. Build the digest-pinned R2E-Gym
`CommandAdapter` manifest with [`../make-r2e-dataset.py`](../make-r2e-dataset.py) and
[`../../../integrations/uni-agent/scripts/resolve_images.sh`](../../../integrations/uni-agent/scripts/resolve_images.sh):

```bash
R2E_LIMIT=20

"${ENV_REPO}/benchmarking/swebench/make-r2e-dataset.py" \
  --source r2e_gym_easy \
  --limit "${R2E_LIMIT}" \
  --images-out "${MANIFEST_DIR}/r2e-images.txt"

"${ENV_REPO}/integrations/uni-agent/scripts/resolve_images.sh" \
  "${MANIFEST_DIR}/r2e-images.txt" \
  "${DOCKERHUB_REMOTE}" \
  "${MANIFEST_DIR}/r2e-images.json"

"${ENV_REPO}/benchmarking/swebench/make-r2e-dataset.py" \
  --source r2e_gym_easy \
  --limit "${R2E_LIMIT}" \
  --image-digests "${MANIFEST_DIR}/r2e-images.json" \
  --out "${MANIFEST_DIR}/r2e.jsonl"
```

Set `MODE=mirror` on `resolve_images.sh` when copying into a standard Artifact Registry repository
instead of a Docker Hub remote repository.

Build the SWE-bench Verified manifest with [`../make-dataset.py`](../make-dataset.py). Request extra
rows above `--swe-images` (for example `SWE_IMAGES + 10`, up to `500`) because
[`../offline-exclusions.txt`](../offline-exclusions.txt) removes instances whose tests require
internet access:

```bash
SWE_LIMIT=30

"${ENV_REPO}/benchmarking/swebench/make-dataset.py" \
  --registry "${SWEBENCH_REPO}" \
  --limit "${SWE_LIMIT}" \
  --out "${MANIFEST_DIR}/swebench.jsonl"
```

## 3. Validate the plan offline (`dry-run`)

Run `dry-run` before touching the cluster to verify manifest schemas, rendered trajectories, shard
widths, and the 63-character actor name limit:

```bash
"${ENV_REPO}/benchmarking/swebench/run-r2e-swebench.sh" dry-run \
  --r2e-dataset "${MANIFEST_DIR}/r2e.jsonl" \
  --swe-dataset "${MANIFEST_DIR}/swebench.jsonl" \
  --concurrency 200 \
  --images 20 \
  --rollouts-per-image 5 \
  --artifact-dir /tmp/r2eswe-dry
```

Confirm stderr prints the expected `resolved plan:` line and two `swebench-loader` `"msg":"plan"`
JSON lines with exit code `0`.

## 4. Prewarm and run on the cluster

Run `prewarm` once per cluster or new image slice before the timed run. `prewarm` launches `1`
`workload-file-read-one` probe per selected image, waits for every derived `ActorTemplate` golden
snapshot bake to complete, warms node caches, and writes prewarm events under
`$ARTIFACT_DIR/prewarm/` outside the timed `events/` window:

```bash
export KO_DOCKER_REPO="${REGION}-docker.pkg.dev/${PROJECT}/bench"
export ARTIFACT_DIR=/tmp/r2eswe-run

"${ENV_REPO}/benchmarking/swebench/run-r2e-swebench.sh" prewarm \
  --r2e-dataset "${MANIFEST_DIR}/r2e.jsonl" \
  --swe-dataset "${MANIFEST_DIR}/swebench.jsonl" \
  --concurrency 200 \
  --images 20 \
  --rollouts-per-image 5 \
  --artifact-dir "${ARTIFACT_DIR}"
```

Execute the timed run in-cluster (launches `swebench-loader-<run>-swe` and
`swebench-loader-<run>-r2e` Kubernetes Jobs next to `ate-env-api`):

```bash
SAMPLE_CACHE=1 HARVEST_RESTORES=1 \
"${ENV_REPO}/benchmarking/swebench/run-r2e-swebench.sh" run \
  --r2e-dataset "${MANIFEST_DIR}/r2e.jsonl" \
  --swe-dataset "${MANIFEST_DIR}/swebench.jsonl" \
  --concurrency 200 \
  --images 20 \
  --rollouts-per-image 5 \
  --artifact-dir "${ARTIFACT_DIR}"
```

Common parameter variations:

- **20 total images (`10` R2E + `10` SWE-bench) at `200` concurrency (`10` rollouts per image)**:
  pass `--concurrency 200 --images 10 --rollouts-per-image 10`.
- **Oversubscribed fleet (`320` replica rollouts across `100` active slots)**: pass
  `--concurrency 100 --images 20 --rollouts-per-image 8` (enables `-park-each` in `steps` mode).
- **Rootfs I/O probe without `pytest`**: pass `--workload io --mode eval`.
- **Extra `swebench-loader` labels**: append `-- -label arm=baseline -label commit=abc1234`.
- **Local port-forward execution**: omit `KO_DOCKER_REPO` (or pass `--execution local`) with
  `ENDPOINT=localhost:7777`.

## 5. Verify outputs in `$ARTIFACT_DIR`

Check these files and invariants after `run` exits `0`:

- `events/r2e.jsonl` and `events/swe.jsonl`: raw operation streams stamped with `job` and `worker`.
- `r2e-summary.json` and `swe-summary.json`: per-shard `swebench-loader report` summaries.
- `report.json` and `report.md`: unified `bench report` bundle. On `--workload graded`, verify
  `failed == 0` and `reward_rate == 1.0`. Check `distinct_instances`, `distinct_envs`,
  `peak_active_sandboxes`, per-step `barrier_ms` / `park_ms` / `wake_p50_ms`, `jobs.r2e`,
  `jobs.swe`, and `interference`.
- `cache_before.prom`, `cache_after.prom`, and `restore_timings.jsonl`: node cache metrics and
  `atelet` restore phases captured when `SAMPLE_CACHE=1` and `HARVEST_RESTORES=1` are set.
