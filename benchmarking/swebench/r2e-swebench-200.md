# Mixed R2E-Gym and SWE-bench 200-Concurrency Recipe

[`./run-r2e-swebench-200.sh`](./run-r2e-swebench-200.sh) drives 200 concurrent sandboxes
(`MAX_CONCURRENCY=200`) across a small slice of R2E-Gym and SWE-bench Verified task images without
GPUs or external model endpoints. Two concurrent `swebench-loader` shards share one `$ARTIFACT_DIR`
and produce a unified multi-job report through `bench report`.

## Workload geometry

`swebench-loader` decouples concurrency (`-width`) from dataset cardinality (`-images`) through
per-image fan-out (`-rollouts` in `steps` mode and `-tasks-per-image` in `eval` and `open-loop`
modes). [`./run-r2e-swebench-200.sh`](./run-r2e-swebench-200.sh) splits `MAX_CONCURRENCY` across two
tagged jobs (`job=r2e` and `job=swe`) and computes each shard's per-image fan-out as
`ceil(shard_width / shard_images)`.

| Slice                            | Environment settings                              | Active actors                                                         | Distinct images |
| -------------------------------- | ------------------------------------------------- | --------------------------------------------------------------------- | --------------- |
| 20 R2E + 20 SWE-bench (default)  | `MAX_CONCURRENCY=200 R2E_IMAGES=20 SWE_IMAGES=20` | `100` (`5` per R2E image) + `100` (`5` per SWE-bench image) = `200`   | `40`            |
| 10 R2E + 10 SWE-bench (20 total) | `MAX_CONCURRENCY=200 R2E_IMAGES=10 SWE_IMAGES=10` | `100` (`10` per R2E image) + `100` (`10` per SWE-bench image) = `200` | `20`            |

Two workload profiles share the same manifests and runner:

- `WORKLOAD=graded` (default): replays [`./r2e-gold.yaml`](./r2e-gold.yaml) on the `r2e` shard and
  `swebench-gold` on the `swe` shard. Both trajectories include `explore` and `read_files` commands,
  simulated policy think pauses (`1s`, `2s`, `3s`), gold patch application, full `pytest` harness
  execution, and reward verification (`expected reward_rate = 1.0`).
- `WORKLOAD=io`: replays `IO_TRAJECTORY` (default `workload-file-read`, configurable to
  `workload-file-read-one`, `workload-compile`, or `workload-read-all`) across both shards to
  isolate rootfs pull, snapshot restore, and filesystem read throughput.

## Cluster prerequisites

1. Size the Substrate `WorkerPool` in `ate-env` so `replicas * per_worker_actor_capacity >= 200`.
   Running `graded` rollouts executes `pytest` concurrently across active sandboxes, so allocate CPU
   and memory headroom on the worker node pool for 200 simultaneous Python test processes.
2. Run [`./run-r2e-swebench-200.sh`](./run-r2e-swebench-200.sh) `prewarm` once per cluster before
   the timed pass. On a cold cluster, creating 5 or 10 sandboxes per image simultaneously queues
   behind each image's initial golden snapshot bake. The `prewarm` subcommand boots 1 environment
   per selected image (`workload-file-read-one`), waits for every derived `ActorTemplate` golden
   snapshot to finish baking, warms node image caches, and deletes the probe environments.
3. Keep `CONNECTIONS=8` (or at least the replica count of `ate-env-api`) so gRPC streams spread
   evenly across `ate-env-api` pods.

## 1. Build the 20-image manifests

Set your Artifact Registry repository paths and output directory outside the git checkout:

```bash
export PROJECT=YOUR_GCP_PROJECT
export REGION=us-central1
export DOCKERHUB_REMOTE="${REGION}-docker.pkg.dev/${PROJECT}/dockerhub-remote"
export SWEBENCH_REPO="${REGION}-docker.pkg.dev/${PROJECT}/swebench/swebench-verified"
export MANIFEST_DIR=/tmp/r2eswe-manifests
mkdir -p "${MANIFEST_DIR}"
```

Build the 20-image R2E-Gym `CommandAdapter` manifest with
[`./make-r2e-dataset.py`](./make-r2e-dataset.py) and
[`../../integrations/uni-agent/scripts/resolve_images.sh`](../../integrations/uni-agent/scripts/resolve_images.sh):

```bash
./benchmarking/swebench/make-r2e-dataset.py \
  --source r2e_gym_easy \
  --limit 20 \
  --images-out "${MANIFEST_DIR}/r2e-images.txt"

./integrations/uni-agent/scripts/resolve_images.sh \
  "${MANIFEST_DIR}/r2e-images.txt" \
  "${DOCKERHUB_REMOTE}" \
  "${MANIFEST_DIR}/r2e-images.json"

./benchmarking/swebench/make-r2e-dataset.py \
  --source r2e_gym_easy \
  --limit 20 \
  --image-digests "${MANIFEST_DIR}/r2e-images.json" \
  --out "${MANIFEST_DIR}/r2e-20.jsonl"
```

Use `MODE=mirror ./integrations/uni-agent/scripts/resolve_images.sh ...` when copying into a
standard Artifact Registry repository instead of a Docker Hub remote repository.

Build the SWE-bench Verified manifest with [`./make-dataset.py`](./make-dataset.py). Build at least
25 rows when filtering against [`./offline-exclusions.txt`](./offline-exclusions.txt) at runtime so
20 offline-compatible instances remain after exclusions:

```bash
./benchmarking/swebench/make-dataset.py \
  --registry "${SWEBENCH_REPO}" \
  --limit 30 \
  --out "${MANIFEST_DIR}/swebench-30.jsonl"
```

## 2. Validate offline (`dry-run`)

Verify manifest schemas, trajectory template rendering, and the 63-character actor name bound
without contacting a cluster:

```bash
R2E_DATASET="${MANIFEST_DIR}/r2e-20.jsonl" \
SWE_DATASET="${MANIFEST_DIR}/swebench-30.jsonl" \
ARTIFACT_DIR=/tmp/r2eswe20-dry \
./benchmarking/swebench/run-r2e-swebench-200.sh dry-run
```

## 3. Prewarm and run at `max_concurrency=200`

### In-cluster execution (recommended for 200 width)

When `KO_DOCKER_REPO` is set, [`./run-r2e-swebench-200.sh`](./run-r2e-swebench-200.sh) stages both
manifests, [`./offline-exclusions.txt`](./offline-exclusions.txt), and
[`./r2e-gold.yaml`](./r2e-gold.yaml) into one `ko` image build and launches two Kubernetes Jobs
(`swebench-loader-<run>-swe` and `swebench-loader-<run>-r2e`) next to `ate-env-api`:

```bash
export KO_DOCKER_REPO="${REGION}-docker.pkg.dev/${PROJECT}/bench"
export R2E_DATASET="${MANIFEST_DIR}/r2e-20.jsonl"
export SWE_DATASET="${MANIFEST_DIR}/swebench-30.jsonl"
export ARTIFACT_DIR=/tmp/r2eswe20-graded

# 1. Bake golden snapshots and warm node caches for all 40 images
./benchmarking/swebench/run-r2e-swebench-200.sh prewarm

# 2. Run 4 synchronous training steps at 200 concurrent sandboxes (suspend/wake between steps)
SAMPLE_CACHE=1 HARVEST_RESTORES=1 \
./benchmarking/swebench/run-r2e-swebench-200.sh run
```

To run with 20 total images (`10` R2E-Gym + `10` SWE-bench, `10` sandboxes per image):

```bash
R2E_IMAGES=10 SWE_IMAGES=10 ARTIFACT_DIR=/tmp/r2eswe10x10 \
./benchmarking/swebench/run-r2e-swebench-200.sh run
```

To run the rootfs I/O stress variant (`workload-file-read` in `eval` mode):

```bash
WORKLOAD=io MODE=eval ARTIFACT_DIR=/tmp/r2eswe20-io \
./benchmarking/swebench/run-r2e-swebench-200.sh run
```

### Local port-forward execution

Omit `KO_DOCKER_REPO` (or set `EXECUTION=local`) to drive both shards from your workstation against
`ENDPOINT`:

```bash
kubectl -n ate-env port-forward svc/ate-env-api 7777:7777 &
EXECUTION=local ENDPOINT=localhost:7777 \
R2E_DATASET="${MANIFEST_DIR}/r2e-20.jsonl" \
SWE_DATASET="${MANIFEST_DIR}/swebench-30.jsonl" \
ARTIFACT_DIR=/tmp/r2eswe20-local \
./benchmarking/swebench/run-r2e-swebench-200.sh run
```

## 4. Outputs in `$ARTIFACT_DIR`

[`./run-r2e-swebench-200.sh`](./run-r2e-swebench-200.sh) writes per-shard and combined artifacts
into `$ARTIFACT_DIR`:

- `events/swe.jsonl` and `events/r2e.jsonl`: raw operation events tagged with `job` and `worker`.
- `swe-summary.json` and `r2e-summary.json`: per-shard `swebench-loader report` JSON summaries.
- `report.json` and `report.md`: unified `bench report` bundle reporting `peak_active_sandboxes`,
  `distinct_instances`, `distinct_envs`, per-step barrier/park/wake latencies, `jobs.r2e` and
  `jobs.swe` breakdowns, and cross-job `interference`.
- `cache_before.prom`, `cache_after.prom`, and `restore_timings.jsonl`: node image-cache snapshots
  and `atelet` restore phase records when `SAMPLE_CACHE=1` and `HARVEST_RESTORES=1` are enabled.

See [`./README.md`](./README.md) for `swebench-loader` event definitions and `bench validate` /
`bench compare` commands.
