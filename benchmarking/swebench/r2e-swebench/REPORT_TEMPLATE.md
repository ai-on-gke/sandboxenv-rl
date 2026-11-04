# <Scale Rung>: <Workload Summary> on <Backend>

Copy this template to `$ARTIFACT_DIR/BENCHMARK_REPORT.md` after a run finishes (do not overwrite
`$ARTIFACT_DIR/report.md` or `$ARTIFACT_DIR/comparison.md`). Replace every `<placeholder>` from the
artifact bundle (`comparison.json`, `report.json`, `*-summary.json`, `*-fleet.json`,
`system_description.json`, `measurements.json`, `restore_timings.jsonl`, `cache_before.prom`,
`cache_after.prom`, and `events/reap.jsonl`) and the pre-run cluster checks in
[`../SKILL.md`](../SKILL.md). Write `not collected` when a telemetry source was not enabled and
`not applicable (simulated RL)` for trainer-only GPU fields on simulated runs. Never write `0` for
an uncollected metric.

| Field                  | Value                                                                                                     | Source                                                           |
| :--------------------- | :-------------------------------------------------------------------------------------------------------- | :--------------------------------------------------------------- |
| **Run ID**             | `<run-id>`                                                                                                | `report.json` (`run` / `run_id`)                                 |
| **Date (UTC)**         | `<YYYY-MM-DD HH:MM to HH:MM UTC>`                                                                         | `run_window.json` (`start_ts`, `end_ts`)                         |
| **Scale rung**         | `<Smoke \| Fast \| Representative (ML Bench) \| Representative (High Cardinality) \| Frontier (1M / 3M)>` | Scale ladder target                                              |
| **Track**              | `<Simulated RL \| Real RL>` (`trace_captured=<false\|true>`)                                              | `report.json` (`trace_captured`)                                 |
| **Duration**           | `<wall-seconds s (Xh Ym Zs)>`                                                                             | `report.json` (`run_ms`) or `comparison.json`                    |
| **Cluster and region** | `<cluster-name>` (`<region-or-zone>`)                                                                     | `system_description.json` (`cluster`, `zone_topology`)           |
| **Backend and access** | `<substrate \| agent-sandbox>` via `<go \| python \| go-direct \| sweep>`                                 | `system_description.json` (`backend`, `access`), `labels.client` |
| **Substrate build**    | `<tag-or-commit-sha>`                                                                                     | `system_description.json` (`substrate_version`)                  |
| **`sandboxenv-rl`**    | `<commit-sha>`                                                                                            | `system_description.json` (`git_sha`, `sdk_version`)             |
| **Results bundle**     | `<local $ARTIFACT_DIR or gs://benchmark-bucket/YYYY-MM-DD_.../>`                                          | `$ARTIFACT_DIR` / `bench upload`                                 |

## Summary

- **Workload**: `<tasks>` rollouts attempted across `<distinct_images>` distinct images
  (`<r2e_images>` R2E-Gym + `<swe_images>` SWE-bench Verified) at `<max_concurrent>` max concurrent
  sandboxes (`<per_node_concurrency>` concurrent starts per sandbox node); `<rewarded>` rewarded
  (`reward_rate=<0.000-1.000>`), `<failed>` failed.
- **Start footing**: `<cold boot \| cold restore \| warm restore \| wake>`; golden snapshots
  `<pre-baked before t0 \| baked in-band>`, node image cache `<misses_per_first_cmd>` pulls per
  first command.
- **Headline latency and throughput**: `create` `<p50>` ms p50 / `<p95>` ms p95; `ttfi` `<p50>` ms
  p50 / `<p95>` ms p95 (`<dominant sub-phase and % share>`); `wake` `<p50>` ms p50 / `<p95>` ms p95;
  sustained `<tasks_per_min>` tasks/min (`<sustained_claims_per_sec>` claims/s, peak
  `<peak_claims_per_sec>` claims/s).
- **Warm-pool efficiency**: `<working_set_hit_rate %>` warm hit rate with `<standing_pool_size>`
  standing sandboxes (`<peak_parked_sandboxes>` peak parked, `<peak_active_sandboxes>` peak active)
  under strategy `<strategy>`.
- **Cluster health and errors**: Control plane peaked at `<cores>` vCPU and `<memory>` GiB per pod
  with `<restarts>` restarts; `<orphan_count>` orphaned sandboxes after reap; `<failure summary>`.

## 1. Setup identification and system specification

### 1.1 Configurable workload and load parameters

Populate from `report.json` (`labels`, `load`, `run_tags`, `lifecycle_policy`) and `comparison.md`.

| Parameter                           | Value                                               | Source / CLI flag                                                      |
| :---------------------------------- | :-------------------------------------------------- | :--------------------------------------------------------------------- |
| **Dataset manifests**               | `<r2e.jsonl (@sha256), swebench.jsonl (@sha256)>`   | `--r2e-dataset`, `--swe-dataset`, `system_description.dataset_digest`  |
| **Exclusion list**                  | `<offline-exclusions.txt@hash>`                     | `labels.exclude_file`, `system_description.exclusion_list_revision`    |
| **Distinct images**                 | `<total>` (`<r2e>` R2E-Gym + `<swe>` SWE-bench)     | `--images`, `load.distinct_images`, `distinct_images`                  |
| **Unique tasks**                    | `<tasks>`                                           | `load.tasks`                                                           |
| **Group size (rollouts per image)** | `<rollouts>` (`<r2e_rollouts>` / `<swe_rollouts>`)  | `--rollouts-per-image`, `labels.rollouts`, `load.group_size`           |
| **Steps**                           | `<steps>`                                           | `--steps`, `labels.steps`, `load.steps`                                |
| **Tasks per step (batch size)**     | `<tasks_per_step>`                                  | `load.tasks_per_step`                                                  |
| **Trajectories per step**           | `<trajectories_per_step>` (`group_size * batch`)    | `load.trajectories_per_step`                                           |
| **Total experiments (repeats)**     | `<reps>`                                            | `reps`, `load.repeat`                                                  |
| **Max concurrent sandboxes**        | `<max_concurrent>` (`r2e=<w1>`, `swe=<w2>`)         | `--concurrency`, `labels.width`, `load.max_concurrent`                 |
| **Warm-pool strategy and window**   | `<none \| naive \| sliding \| pipelined \| all>`    | `--strategy`, `--window-size`, `labels.strategy`, `labels.window_size` |
| **Park policy**                     | `<suspend-sync \| suspend-async \| delete \| none>` | `--park`, `park_mode`, `lifecycle_policy.park_mode`                    |
| **Client / API chain**              | `<go \| go-direct \| python \| all>`                | `--client`, `labels.client`                                            |
| **Trajectory profile**              | `<graded (r2e-gold + swebench-gold) \| io>`         | `--workload`, `labels.trajectory`                                      |
| **Execution mode**                  | `<in-cluster Job \| local port-forward>`            | `--execution`                                                          |

### 1.2 Cluster, node pools, and networking

Populate from `system_description.json` and the pre-run checks in [`../SKILL.md`](../SKILL.md)
section 1.

| Field                     | Value                                                                     |
| :------------------------ | :------------------------------------------------------------------------ |
| **Kubernetes version**    | `<kubernetes_version>` (node OS / kernel `<os_kernel>`, `<storage_fs>`)   |
| **Zones and networking**  | `<zone_topology>`, `<private/public nodes, Cloud NAT, VPC details>`       |
| **Registry and bucket**   | Registry in `<region>`, snapshots bucket in `<bucket_region>`             |
| **Telemetry collector**   | `<OTel collector enabled/disabled; Prometheus scrape status>`             |
| **CPU family uniformity** | `<single CPU family (<platform>) \| mixed families (explain in Caveats)>` |
| **Per-node concurrency**  | `<max_concurrent / sandbox_nodes>` concurrent active sandboxes per node   |

| Node pool       | Machine type and CPU family      | vCPU / RAM     |   Count | Boot disk type and size   | Role                                        | Labels and taints                     |
| :-------------- | :------------------------------- | :------------- | ------: | :------------------------ | :------------------------------------------ | :------------------------------------ |
| `<worker-pool>` | `<e.g. n2-standard-32, Cascade>` | `<vcpu / gib>` | `<cnt>` | `<e.g. 500 GB pd-ssd>`    | Sandbox nodes (`atelet` + `ateworker` pods) | `<labels / runtime NoSchedule taint>` |
| `<ctrl-pool>`   | `<e.g. e2-standard-16>`          | `<vcpu / gib>` | `<cnt>` | `<e.g. 200 GB pd-ssd>`    | Substrate control plane + `ate-env-api`     | `<control-plane taint>`               |
| `<db-pool>`     | `<e.g. n2-standard-16>`          | `<vcpu / gib>` | `<cnt>` | `<disk + PVC size/class>` | Bundled Postgres                            | `<postgres taint>`                    |
| `<driver-pool>` | `<e.g. e2-standard-32>`          | `<vcpu / gib>` | `<cnt>` | `<disk>`                  | Benchmark loader `Job` / RL trainer pods    | `<labels / taints>`                   |

### 1.3 Substrate, workers, and actor template

Populate from `system_description.json` and `kubectl` / `kubectl-ate` inspection
([`../SKILL.md`](../SKILL.md) sections 1 and 4).

| Component or resource    | Build / image digest       | Replicas / count                  | Placement     | Key settings                                                                                |
| :----------------------- | :------------------------- | :-------------------------------- | :------------ | :------------------------------------------------------------------------------------------ |
| `ate-api-server`         | `<substrate_version>`      | `<replicas>`                      | `<pool>`      | `ATE_API_POSTGRES_POOL_MAX_CONNS=<N>`                                                       |
| `atenet-router`          | `<substrate_version>`      | `<replicas>`                      | `<pool>`      | `<settings or default>`                                                                     |
| `atelet` (`DaemonSet`)   | `<substrate_version>`      | `<node_count>`                    | Sandbox nodes | `--image-cache-gc-period=<d> --image-cache-high-percent=<h> --image-cache-low-percent=<l>`  |
| `Postgres`               | `<image / bundled>`        | `<replicas>`                      | `<pool>`      | `<PVC size/class>`, `max_connections=<N>`                                                   |
| `WorkerPool`             | `<ateworker image@sha256>` | `<worker_pod_count>` (`<k>`/node) | Sandbox nodes | `worker_actor_capacity=<N>`, `max_concurrent_sandboxes_cap=<M>`, container limits `<specs>` |
| `SandboxConfig`          | `<runtime_class>`          | `<name>`                          | Sandbox nodes | `<gvisor or microvm; streaming on/off>`                                                     |
| Base `ActorTemplate`     | `<guest_image@sha256>`     | `1` (`<base_template>`)           | `<atespace>`  | Footprint `<cpu>` vCPU / `<memory_gib>` GiB / `<disk_gib>` GiB; golden snapshots `<on/off>` |
| Derived `ActorTemplate`s | `<base_template>-<12hex>`  | `<derived_count>`                 | `<atespace>`  | `<N/N goldenTags ready before t0, 0 errorMessage>`                                          |

### 1.4 Environment API, SDK, and telemetry

| Component            | Configuration                                                                                                                                                                                   |
| :------------------- | :---------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| **`ate-env-api`**    | Image `<api_image>`, `<replicas>` replicas, Service `<api_service>`, `-wake-budget=<d>`, `-wake-running-budget=<d>`, `-max-active-pods=<N>`, `-idle-park-timeout=<d>`, `-enable-tracing=<bool>` |
| **Go / Python SDK**  | Commit `<git_sha>`, `connections=<N>` gRPC channels, `create_timeout=<d>`, `step_timeout=<d>`, `wake_budget=<d>`, `wake_backoff=<d>`                                                            |
| **Telemetry scopes** | `<rl, user-sandbox, substrate, k8s>` (`ENV_BENCH_MODE=<measure \| trace>`)                                                                                                                      |

### 1.5 Non-default settings and feature overlays

List every change from default installation manifests and any `overlay/overlay.json` applied
(`system_description.overlays`).

| Setting or overlay     | Default           | Used              | Reason                                              |
| :--------------------- | :---------------- | :---------------- | :-------------------------------------------------- |
| `<setting-or-overlay>` | `<default-value>` | `<applied-value>` | `<why this run required it; cite base_recipe_hash>` |

## 2. Dataset and image corpus

Populate image counts from the manifests and `report.json` (`distinct_images`), compressed sizes
from Artifact Registry metadata if sampled, and pull totals from `cache_before.prom` and
`cache_after.prom`.

| Dataset / shard       | Distinct images | Layers per image | Compressed size min / p50 / p95 / max (GB) | Unpacked multiplier | Layer sharing           | Registry region | Excluded instances         |
| :-------------------- | --------------: | :--------------- | :----------------------------------------- | :------------------ | :---------------------- | :-------------- | :------------------------- |
| R2E-Gym (`job=r2e`)   |  `<r2e_images>` | `<e.g. ~14>`     | `<min / p50 / p95 / max or not sampled>`   | `~2.0x to 2.5x`     | Minimal across repos    | `<region>`      | `<count>`                  |
| SWE-bench (`job=swe`) |  `<swe_images>` | `<layers>`       | `<min / p50 / p95 / max or not sampled>`   | `~2.0x to 2.5x`     | Shared base + env tiers | `<region>`      | `<count>` (`exclude_file`) |
| **Combined**          |   **`<total>`** |                  | **`<logical / unique TB or not sampled>`** |                     |                         |                 |                            |

- **Aggregate data movement**: `<cold_misses>` cold layer/image pulls and `<cache_hits>` cache hits
  across `<nodes_differenced>` nodes (`<misses_per_first_cmd>` pulls per first command), moving an
  estimated `<compressed_gb_or_tb>` compressed from the registry (`<gb_per_s>` cluster-wide) and
  unpacking `<unpacked_gb_or_tb>` onto node disks (`<mb_per_s_per_node>` per node).

## 3. Reproducible commands and flags

Record the exact commands used to prewarm, execute, measure, reap, and validate the run:

```bash
# Prewarm (outside timed window)
<exact prewarm command>

# Timed benchmark run
<exact run-r2e-swebench.sh or swebench-loader / bench command>

# Optional cluster telemetry, reap, and bundle validation
<exact bench measure / reap / report / validate commands>
```

| Flag or environment variable | Value     | Effect in this run                                       |
| :--------------------------- | :-------- | :------------------------------------------------------- |
| `--concurrency`              | `<value>` | Caps concurrent active sandboxes across both shards      |
| `--images`                   | `<value>` | Selects `<value>` images per dataset (`<2*value>` total) |
| `--rollouts-per-image`       | `<value>` | Replays `<value>` trajectories per selected image        |
| `--strategy`                 | `<value>` | Selects warm-pool lifecycle orchestration policy         |
| `--client`                   | `<value>` | Selects SDK and API execution path                       |
| `--workload`                 | `<value>` | Selects `graded` (`pytest` evaluation) or `io` probe     |

## 4. Results

### 4.1 Multi-arm strategy and client chain comparison

Populate from `$ARTIFACT_DIR/comparison.json` (or `$ARTIFACT_DIR/comparison.md`) and each arm's
`report.json` (`headline_baseline`). On single-arm runs, keep one row.

| Arm     | Client     | Strategy     | Tasks | Failed | Reward rate | Wall clock (s) | Throughput (tasks/min) | Claims/s (sustained / peak) | Warm hit rate | Peak active | Standing pool | TTFI p50 / p90 / p95 / max (ms) | Wake p50 / p90 / p95 / max (ms) |
| :------ | :--------- | :----------- | ----: | -----: | ----------: | -------------: | ---------------------: | --------------------------: | ------------: | ----------: | ------------: | ------------------------------: | ------------------------------: |
| `<arm>` | `<client>` | `<strategy>` | `<n>` |  `<f>` |    `<rate>` |        `<sec>` |          `<tasks_min>` |           `<sust> / <peak>` |      `<pct>%` |  `<active>` |      `<pool>` |  `<p50 / p90 / p95 / max or ->` |  `<p50 / p90 / p95 / max or ->` |

- **Client and SDK layer delta** (when sweeping `--client`): Compare `python` (`SandboxFleet` ->
  `ate-env-api`), `go` (Go SDK -> `ate-env-api`), and `go-direct` (direct `ateapi` +
  `atenet-router`) on matched strategies across `create`, `ttfi`, `wake`, `step/*`, and `delete`.

### 4.2 Sandbox performance and lifecycle stage latencies

Populate operation latencies from `report.json` (`stats[]` and `headline_baseline`) for the primary
arm (or replicate per arm when needed).

| Event            | Name                  | Count | Failed | Retried | p50 (ms) | p90 (ms) | p95 (ms) | Max (ms) |
| :--------------- | :-------------------- | ----: | -----: | ------: | -------: | -------: | -------: | -------: |
| `create`         |                       | `<n>` |  `<f>` |   `<r>` |  `<p50>` |  `<p90>` |  `<p95>` |  `<max>` |
| `prewarm`        |                       | `<n>` |  `<f>` |   `<r>` |  `<p50>` |  `<p90>` |  `<p95>` |  `<max>` |
| `ttfi`           |                       | `<n>` |  `<f>` |   `<r>` |  `<p50>` |  `<p90>` |  `<p95>` |  `<max>` |
| `ttfi` sub-phase | `sdk_slot_wait`       | `<n>` |    `-` |     `-` |  `<p50>` |      `-` |  `<p95>` |      `-` |
| `ttfi` sub-phase | `wire_claim_to_ready` | `<n>` |    `-` |     `-` |  `<p50>` |      `-` |  `<p95>` |      `-` |
| `ttfi` sub-phase | `ready_to_first_cmd`  | `<n>` |    `-` |     `-` |  `<p50>` |      `-` |  `<p95>` |      `-` |
| `suspend`        |                       | `<n>` |  `<f>` |   `<r>` |  `<p50>` |  `<p90>` |  `<p95>` |  `<max>` |
| `wake`           |                       | `<n>` |  `<f>` |   `<r>` |  `<p50>` |  `<p90>` |  `<p95>` |  `<max>` |
| `step`           | `baseline`            | `<n>` |  `<f>` |   `<r>` |  `<p50>` |  `<p90>` |  `<p95>` |  `<max>` |
| `step`           | `explore`             | `<n>` |  `<f>` |   `<r>` |  `<p50>` |  `<p90>` |  `<p95>` |  `<max>` |
| `step`           | `read_files`          | `<n>` |  `<f>` |   `<r>` |  `<p50>` |  `<p90>` |  `<p95>` |  `<max>` |
| `step`           | `apply_patch`         | `<n>` |  `<f>` |   `<r>` |  `<p50>` |  `<p90>` |  `<p95>` |  `<max>` |
| `step`           | `run_tests`           | `<n>` |  `<f>` |   `<r>` |  `<p50>` |  `<p90>` |  `<p95>` |  `<max>` |
| `step`           | `grade`               | `<n>` |  `<f>` |   `<r>` |  `<p50>` |  `<p90>` |  `<p95>` |  `<max>` |
| `task`           | `r2e-gold`            | `<n>` |  `<f>` |   `<r>` |  `<p50>` |  `<p90>` |  `<p95>` |  `<max>` |
| `task`           | `swebench-gold`       | `<n>` |  `<f>` |   `<r>` |  `<p50>` |  `<p90>` |  `<p95>` |  `<max>` |
| `delete`         |                       | `<n>` |  `<f>` |   `<r>` |  `<p50>` |  `<p90>` |  `<p95>` |  `<max>` |
| `step_barrier`   |                       | `<n>` |  `<f>` |   `<r>` |  `<p50>` |  `<p90>` |  `<p95>` |  `<max>` |
| `run`            |                       | `<n>` |  `<f>` |   `<r>` |  `<p50>` |  `<p90>` |  `<p95>` |  `<max>` |

Populate sandbox lifecycle stage timings from `measurements.json`
(`events/substrate_lifecycle.jsonl`), `report.json` (`steps[]`, `acquisition_sources`), and
`events/reap.jsonl`:

| Lifecycle stage          | Measured values                                                                                                       | Source                                   |
| :----------------------- | :-------------------------------------------------------------------------------------------------------------------- | :--------------------------------------- |
| **Prepare**              | `workerpool_spinup_wall_ms=<val>`, `worker_pod_total_ready_ms` p50/p95=`<p50>/<p95>`, prewarm wall=`<s or not timed>` | `bench spinup`, `$ARTIFACT_DIR/prewarm/` |
| **Per-RL-batch warm**    | `working_set_hit_rate=<pct>%`, `standing_pool_size=<n>`, `lead_margin_ms=<ms or not applicable>`                      | `report.json` (`steps[]`)                |
| **Active sandbox use**   | Acquisition sources `warm_pool=<n>`, `cold_create=<n>`; `peak_active_sandboxes=<n>`, `peak_parked_sandboxes=<n>`      | `report.json` (`acquisition_sources`)    |
| **Per-RL-batch release** | Fleet `park_ms=<ms>`, `retired=<n>`, step-boundary leaked resources=`<n>`                                             | `report.json` (`steps[]`, `retired`)     |
| **Reap and spindown**    | Reap wall=`<ms>`, `orphan_count=<0>`, `workerpool_spindown_wall_ms=<val or not run>`                                  | `bench reap`, `bench spindown`           |

### 4.3 Restore phase breakdown and node image cache

Populate from `restore_timings.jsonl` (`HARVEST_RESTORES=1`) and `cache_before.prom` /
`cache_after.prom` (`SAMPLE_CACHE=1`).

- **Restore sample coverage**: `<paired_records>` of `<total_starts>` starts paired
  (`<coverage_pct>%`; note any kubelet log rotation gap).
- **Node image-cache delta**: `<nodes_differenced>` nodes differenced; `<misses>` cold misses,
  `<hits>` hits, `<errors>` errors, `<evictions>` evictions; `<misses_per_first_cmd>` misses per
  first command.

| Restore phase                                                     | p50 (ms) | p95 (ms) | Max (ms) | Share of median TTFI / wake |
| :---------------------------------------------------------------- | -------: | -------: | -------: | --------------------------: |
| `manifest_fetch_ms`                                               |  `<p50>` |  `<p95>` |  `<max>` |                    `<pct>%` |
| `download_ms` (golden or suspend snapshot)                        |  `<p50>` |  `<p95>` |  `<max>` |                    `<pct>%` |
| `oci_unpack_ms` (containerd pull + unpack when missing on node)   |  `<p50>` |  `<p95>` |  `<max>` |                    `<pct>%` |
| `worker_restore_ms` (`gVisor` or `microVM` restore)               |  `<p50>` |  `<p95>` |  `<max>` |                    `<pct>%` |
| `persist_ms`                                                      |  `<p50>` |  `<p95>` |  `<max>` |                    `<pct>%` |
| **`total_ms` (`atelet`)**                                         |  `<p50>` |  `<p95>` |  `<max>` |                    `<pct>%` |
| **Outside `atelet`** (create, placement, router, guest readiness) |  `<p50>` |  `<p95>` |  `<max>` |                    `<pct>%` |
| **Failed restores**                                               |  `<cnt>` |      `-` |      `-` |                         `-` |

### 4.4 RL step and rollout decomposition

Populate from `report.json` (`steps[]`, `rollout_breakdown[]`, `rl_step_breakdown`, `jobs`,
`interference`, and `convergence`).

| Step |  Live | Tasks | Rewarded | Step barrier (ms) | Park (ms) | Rollout p50 / p100 (ms) | Straggler skew (`p100/p50`) | LLM think p50 / p100 (ms) | Env wait p50 / p100 (ms) | Env overhead (%) |
| ---: | ----: | ----: | -------: | ----------------: | --------: | ----------------------: | --------------------------: | ------------------------: | -----------------------: | ---------------: |
|  `1` | `<n>` | `<n>` |    `<n>` |    `<barrier_ms>` |  `<park>` |        `<p50> / <p100>` |                   `<skew>x` |          `<p50> / <p100>` |         `<p50> / <p100>` |         `<pct>%` |

- **Dominant bottleneck**: `<rl_step_breakdown.dominant_bottleneck>`.
- **Multi-shard breakdown and interference**: `job=r2e` (`<tasks>` tasks, `<reward_rate>` reward
  rate) + `job=swe` (`<tasks>` tasks, `<reward_rate>` reward rate); `overlapping_steps=<n>`,
  `slot_wait_p50_ms=<p50>`, `slot_wait_p95_ms=<p95>`, `slot_wait_max_ms=<max>`.
- **Trainer throughput and convergence** (`ScopeRL`):
  `<tokens/s/GPU, actor MFU, initial -> final reward_mean, or not applicable (simulated RL)>`.

### 4.5 Cluster health and sandbox hardware cost

Populate from `measurements.json` (`prom_metrics`, `server_summary`, `custom_metrics`),
`system_description.json`, and live cluster checks (`bench measure -k8s`).

| Subsystem or metric                       | Observed value                                                               | Source                                       |
| :---------------------------------------- | :--------------------------------------------------------------------------- | :------------------------------------------- |
| **`ate-api-server` CPU / memory**         | `<cores>` vCPU, `<MiB/GiB>` per replica (`<replicas>` pods)                  | `measurements.json` / `kubectl top`          |
| **`atenet-router` CPU / memory**          | `<cores>` vCPU, `<MiB/GiB>` per replica (`<replicas>` pods)                  | `measurements.json` / `kubectl top`          |
| **`Postgres` CPU / memory / connections** | `<cores>` vCPU, `<MiB/GiB>`, peak `<conns>/<max_connections>`                | `measurements.json` / `kubectl top`          |
| **`ate-env-api` CPU / memory**            | `<cores>` vCPU, `<MiB/GiB>` per replica (`<replicas>` pods)                  | `measurements.json` / `kubectl top`          |
| **Sandbox node pressure and disk**        | CPU/mem/IO PSI `<vals>`, max disk used `<pct>%`, `DiskPressure=<0>`          | `ScopeK8s` / node conditions                 |
| **Evictions, OOMKills, and restarts**     | Evictions `<0>`, OOMKills `<0>`, pod restarts `<0>`, `429` responses `<0>`   | `ScopeK8s` / API & registry metrics          |
| **Sandbox density (active / total)**      | `<per_node>`/node, `<per_worker>`/worker, `<per_vcpu>`/vCPU, `<per_gib>`/GiB | `ScopeSubstrate` + `system_description.json` |
| **Requested vs actual sandbox WSS**       | Requested `<cpu>` vCPU / `<gib>` GiB; actual peak `<cpu>` vCPU / `<gib>` GiB | `sandbox_footprint` vs `metrics.k8s.io`      |
| **Node-hours per episode**                | Worker `<nh>` node-hours/task; control-plane + router `<nh>` node-hours/task | Node count * `run_ms` / `tasks`              |
| **Snapshot size and throughput**          | `size_mb` p50/p95=`<p50>/<p95>` MB, `<cnt>` checkpoints, `<mb_s>` MB/s       | `ScopeSubstrate` snapshot metrics            |

### 4.6 Failures and retries

Group every failed operation (`failed > 0`) and retry (`retried > 0`) by root cause and owning
layer. Write `0 failures and 0 retries across all <tasks> tasks.` when clean.

|   Count | Operation / phase | Symptom or status code            | Root cause and layer                                    | Fix or tracking issue |
| ------: | :---------------- | :-------------------------------- | :------------------------------------------------------ | :-------------------- |
| `<cnt>` | `<op>`            | `<gRPC code / message / timeout>` | `<client \| ate-env-api \| substrate \| node/registry>` | `<PR or issue>`       |

## 5. Findings and next steps

### 5.1 Key findings

1. **Primary latency driver**:
   `<state what dominated TTFI, wake, or step_barrier with exact ms and % share>`.
2. **Strategy trade-off**:
   `<compare none, naive, sliding, and pipelined on wall clock vs standing_pool_size>`.
3. **SDK and API overhead**:
   `<quantify python vs go vs go-direct overhead per phase when multiple clients ran>`.
4. **Placement and cache behavior**:
   `<explain misses_per_first_cmd and node cache coverage across steps>`.
5. **Control-plane and node headroom**:
   `<state how close worker slots, node disks, or control-plane pods came to saturation>`.

### 5.2 Next steps

1. `<next experiment, scale rung, or configuration change>`.
2. `<follow-up bug fix or optimization with component owner>`.

## 6. Artifacts, validation gates, and caveats

### 6.1 Artifact inventory

| Artifact                       | Path or URI                                                        | Contents                                                   |
| :----------------------------- | :----------------------------------------------------------------- | :--------------------------------------------------------- |
| **Human narrative report**     | `$ARTIFACT_DIR/BENCHMARK_REPORT.md`                                | This populated report                                      |
| **Cross-arm comparison**       | `$ARTIFACT_DIR/comparison.{md,json,txt}`                           | Side-by-side multi-arm latency, throughput, and pool table |
| **Per-arm `v1` report**        | `$ARTIFACT_DIR/[arms/<arm>/]report.{json,md}`                      | Machine-validated `bench report` bundle                    |
| **Raw event streams**          | `$ARTIFACT_DIR/[arms/<arm>/]events/{r2e,swe}.jsonl`                | Per-shard `v1` operation events                            |
| **System and cluster metrics** | `$ARTIFACT_DIR/system_description.json`, `measurements.json`       | Cluster topology, versions, and scraped server/K8s metrics |
| **Cache and restore logs**     | `$ARTIFACT_DIR/cache_{before,after}.prom`, `restore_timings.jsonl` | Node image-cache counters and `atelet` restore phases      |
| **OTel traces / HTML viewer**  | `<spans.jsonl, metrics.jsonl, report.html, or not collected>`      | Server-side span waterfall (`build_report.py`)             |
| **Archived GCS bundle**        | `<gs://benchmark-bucket/folder/ or local only>`                    | Immutable uploaded bundle (`bench upload`)                 |

### 6.2 `bench validate` gates

- [ ] **Gate 1 (Footing match)**: Declared `warm` or `cold` footing matches `prewarm` events and
      node-cache deltas.
- [ ] **Gate 2 (Reward rate)**: `reward_rate` meets the scenario threshold (`1.0` on
      `--workload     graded` gold patches).
- [ ] **Gate 3 (Task count)**: Completed `tasks` meets expected `images * rollouts * steps`.
- [ ] **Gate 4 (Failed-create cap)**: Failed `create` ratio is within the `< 5%` validity cap.
- [ ] **Gate 5 (Schedule completion)**: Every scheduled work item in both `r2e` and `swe` shards
      completed.
- [ ] **Gate 6 (Trace provenance)**: `trace_captured` matches trace files, and any uploaded folder
      carries `_traced` iff `trace_captured: true`.
- [ ] **Gate 7 (Zero orphans)**: `reap` verified `orphan_count == 0` leaked actors on the cluster.

### 6.3 Caveats

- `<List any condition that affects comparison against other runs: mixed node CPU families, unbaked goldens, kubelet log rotation gaps in restore_timings.jsonl, local port-forward RTT vs in-cluster Job execution, trace_captured overhead, or uncollected optional telemetry scopes.>`
