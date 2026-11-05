# Mixed R2E-Gym and SWE-bench Benchmark Report (`<run-id>`)

Copy this template to `$ARTIFACT_DIR/BENCHMARK_REPORT.md` after a run finishes (keep
`$ARTIFACT_DIR/report.md` and `$ARTIFACT_DIR/comparison.md` intact). Populate cluster, node, and
version tables from `system_description.json` (`bench measure -k8s`) and the pre-run checks in
[`../SKILL.md`](../SKILL.md), and populate results from `comparison.json`, `report.json`, and
`run_window.json`.

| Field                         | Value                                                                                                               | Source                                                 |
| :---------------------------- | :------------------------------------------------------------------------------------------------------------------ | :----------------------------------------------------- |
| **Run ID**                    | `<run-id>`                                                                                                          | `report.json` (`run`)                                  |
| **Date (UTC)**                | `<start_ts to end_ts>`                                                                                              | `run_window.json`                                      |
| **Duration**                  | `<wall_s> s`                                                                                                        | `report.json` (`run_ms`) or `comparison.json`          |
| **Cluster and zone**          | `<cluster-name>` (`<zone_topology>`)                                                                                | `system_description.json` (`cluster`, `zone_topology`) |
| **Kubernetes version**        | `<kubernetes_version>`                                                                                              | `system_description.json` (`kubernetes_version`)       |
| **`ate` (Substrate) version** | `<substrate_version>`                                                                                               | `system_description.json` (`substrate_version`)        |
| **`ate-env` version**         | `<git_sha>` (`api_image`, `guest_image`)                                                                            | `system_description.json` (`git_sha`, `sdk_version`)   |
| **Artifact bundle**           | `<$ARTIFACT_DIR or gs://<YOUR_BENCHMARK_BUCKET>/<YYYY-MM-DD>_<model-family>-<size>_<dataset>_<nickname>[_traced]/>` | Local path or `bench upload`                           |

## Headline Outcome & Time Attribution Summary

> [!IMPORTANT]
>
> - **Key numbers (`<reference-arm>`; drill-down in Sections `3.1`–`3.4`)**:
>   - **Max concurrency observed**: `<active_sandboxes>` concurrent in-flight sandboxes
>     (`standing_pool=<n>`).
>   - **`TTFI` start** (claim start -> sandbox handle ready / 1st cmd issued, `p50 / p90`):
>     `<ttfi_start_p50> / <ttfi_start_p90> ms` (`<prewarm_p50> / <prewarm_p90> ms` summed upfront
>     prewarm when `0 ms` warm-pool hit).
>   - **Time to restore (`p50 / p90`)**: `<restore_total_p50> / <restore_total_p90> ms` in `atelet`.
> - **Workload, cache footing, and reward**: `<tasks>` rollouts across `<distinct_images>` distinct
>   images (`<r2e_images>` R2E-Gym + `<swe_images>` SWE-bench),
>   `<cold boot | cold restore | warm restore | wake>` (goldens `<pre-baked | baked in-band>`,
>   `<misses_per_first_cmd>` cold `ate_imagecache` misses/start, `<working_set_hit_rate>%` warm-pool
>   hit rate), `<rewarded>/<tasks>` rewarded (`reward_rate=<rate>`), `<failed>` failed.
> - **Where time is actually spent (`<reference-arm>` `p50`)**:
>   - **Host Python SDK (`ate_env_rl`)**: `<py_sdk_ms> ms (<py_sdk_pct>%)` (`0 ms` when
>     `--client go` / `go-direct` drives the run; `python` minus `go` delta when `--client python`
>     runs).
>   - **Host SDK + `ate-env` + `ate` infrastructure**: `~<infra_wake_p50> ms` true wake-to-ready
>     startup (`<restore_total_p50> ms` `atelet` restore + `~<wire_p50> ms` gRPC hop), totaling
>     `~<r2e_infra_ms>–<swe_infra_ms> ms (<r2e_infra_pct>%–<swe_infra_pct>%)` across an entire
>     rollout.
>   - **Simulated LLM think (`Step.Think`)**: `<r2e_think_ms> ms (<r2e_think_pct>%)` on `r2e` and
>     `<swe_think_ms> ms (<swe_think_pct>%)` on `swe` (`custom_metrics.llm_think_ms`).
>   - **In-sandbox guest workload (`bash`/`git` + container `Python` `pytest` & graders)**:
>     `~<r2e_guest_ms> ms (<r2e_guest_pct>%)` on `r2e` and `~<swe_guest_ms> ms (<swe_guest_pct>%)`
>     on `swe`.
> - **Strategy and client takeaway**:
>   `<1-2 sentences comparing tested strategies (wall-clock vs standing-pool reduction and window-drain barriers) and Go / Go-direct / Python chains, plus dominant_bottleneck>`.

## 1. Cluster, node, and version specification

### 1.1 Cluster spec

| Field                     | Value                                                                                               |
| :------------------------ | :-------------------------------------------------------------------------------------------------- |
| **Platform and K8s**      | `<GKE / K8s version>`, node image / kernel `<os_kernel>`, `<storage_driver_and_fs>`                 |
| **Zones and networking**  | `<zone_topology>`, `<private/public nodes, Cloud NAT egress, VPC>`                                  |
| **Registry and bucket**   | Image registry in `<region>`, snapshots bucket in `<bucket_region>`                                 |
| **Telemetry**             | `-enable-tracing=<bool>`, OTel collector `<on/off>`, `SAMPLE_CACHE=<0/1>`, `HARVEST_RESTORES=<0/1>` |
| **CPU family uniformity** | `<single CPU family (<platform>) \| mixed families (note in Caveats)>`                              |

### 1.2 Node pool spec

| Node pool       | Machine type and CPU family      | vCPU / RAM     |   Nodes | Boot disk type and size | Role                                        | Labels and taints                     |
| :-------------- | :------------------------------- | :------------- | ------: | :---------------------- | :------------------------------------------ | :------------------------------------ |
| `<worker-pool>` | `<e.g. n2-standard-32, Cascade>` | `<vcpu / gib>` | `<cnt>` | `<e.g. 500 GB pd-ssd>`  | Sandbox nodes (`atelet` + `ateworker` pods) | `<labels / runtime NoSchedule taint>` |
| `<ctrl-pool>`   | `<e.g. e2-standard-16>`          | `<vcpu / gib>` | `<cnt>` | `<e.g. 200 GB pd-ssd>`  | `ate` control plane + `ate-env-api`         | `<control-plane taint>`               |
| `<db-pool>`     | `<e.g. n2-standard-16>`          | `<vcpu / gib>` | `<cnt>` | `<disk + PVC>`          | Bundled Postgres                            | `<postgres taint>`                    |
| `<driver-pool>` | `<e.g. e2-standard-32>`          | `<vcpu / gib>` | `<cnt>` | `<disk>`                | Benchmark loader `Job` (when `in-cluster`)  | `<labels / taints>`                   |

### 1.3 `ate` (Substrate) and `ate-env` versions and settings

| Component                | Version / image digest     | Replicas / count                  | Key settings                                                                               |
| :----------------------- | :------------------------- | :-------------------------------- | :----------------------------------------------------------------------------------------- |
| `ate-api-server`         | `<substrate_version>`      | `<replicas>`                      | `ATE_API_POSTGRES_POOL_MAX_CONNS=<N>`                                                      |
| `atenet-router`          | `<substrate_version>`      | `<replicas>`                      | `<default or overrides>`                                                                   |
| `atelet` (`DaemonSet`)   | `<substrate_version>`      | `<sandbox_nodes>`                 | `--image-cache-gc-period=<d> --image-cache-high-percent=<h> --image-cache-low-percent=<l>` |
| `WorkerPool`             | `<ateworker image@sha256>` | `<worker_pod_count>` (`<k>`/node) | `<pool_name>`, `worker_actor_capacity=<N>`, `max_concurrent_sandboxes_cap=<M>`             |
| `SandboxConfig`          | `<runtime_class>`          | `<config_name>`                   | `<gvisor or microvm>`                                                                      |
| Base `ActorTemplate`     | `<guest_image@sha256>`     | `1` (`<base_template>`)           | `<cpu>` vCPU / `<memory_gib>` GiB / `<disk_gib>` GiB, golden snapshots `<on/off>`          |
| Derived `ActorTemplate`s | `<base_template>-<12hex>`  | `<derived_count>`                 | `<N/N goldenTags ready before t0>`                                                         |
| `ate-env-api`            | `<api_image@sha256>`       | `<replicas>`                      | Service `<api_service>`, `-wake-budget=<d>`, `-wake-running-budget=<d>`                    |
| `ate-env` SDK / loader   | `<sandboxenv-rl git_sha>`  | `<connections>` gRPC channels     | Go `clients/go`, Python `ate_env_rl`, `--execution <in-cluster \| local>`                  |

## 2. Concurrency, workload parameters, and command

| Parameter                   | Value                                                         | Source / flag                                                                          |
| :-------------------------- | :------------------------------------------------------------ | :------------------------------------------------------------------------------------- |
| **Configured concurrency**  | `<max_concurrent>` total (`r2e=<w1>`, `swe=<w2>`, `<k>`/node) | `--concurrency`, `--r2e-width`, `--swe-width`                                          |
| **Observed pool footprint** | `peak_active=<n>`, `standing_pool=<n>`, `peak_parked=<n>`     | `report.json` (`peak_active_sandboxes`, `standing_pool_size`, `peak_parked_sandboxes`) |
| **Distinct images**         | `<total>` (`<r2e>` R2E-Gym + `<swe>` SWE-bench, `<exclude>`)  | `--images`, `distinct_images`, `labels.exclude_file`                                   |
| **Rollouts per image**      | `<rollouts>` (`steps=<steps>`, `mode=<mode>`, `park=<park>`)  | `--rollouts-per-image`, `--steps`, `--mode`, `--park`                                  |
| **Total tasks**             | `<tasks>` (`r2e=<r2e_tasks>`, `swe=<swe_tasks>`)              | `report.json` (`tasks`, `jobs`)                                                        |
| **Strategies and window**   | `<none \| naive \| sliding \| pipelined \| all>` (`win=<w>`)  | `--strategy`, `--window-size`                                                          |
| **Client chains**           | `<go \| go-direct \| python \| all>`                          | `--client`                                                                             |
| **Workload trajectory**     | `<graded (r2e-gold + swebench-gold) \| io>`                   | `--workload`, `labels.trajectory`                                                      |

```bash
<exact prewarm and run-r2e-swebench.sh commands>
```

## 3. Results

### 3.1 Multi-arm comparison

Populate from `$ARTIFACT_DIR/comparison.json` / `comparison.md`, per-arm `report.json`, and
`{r2e,swe}-summary.json`. `Wall (s)` is in **seconds (`s`)**; every latency column is `p50/p95/p99`
(or `p50` where noted) in **milliseconds (`ms`)**:

- **`TTFI` handle (`ms`)**: Claim start -> sandbox handle returned (`wire_claim_to_ready_*` on
  `none`; `0` at rollout start on warm-pool hits after upfront `Create` + `Suspend` prewarm).
- **`Infra boot/wake` (`p50 ms`)**: Actual host client + `ate-env` + `ate` time until the container
  is restored and running guest PID 1 (`atelet` `total` restore + gRPC proxy hop, plus `Create` on
  `none`).
- **`TTFC / Wake` 1st cmd (`ms`)**: Client `stats[event="ttfi"]` (`none`) or `stats[event="wake"]`
  (warm-pool strategies), which **includes execution of `step=baseline`** inside the sandbox.

| Arm     | Client     | Strategy     |       Active / Pool | Tasks (fail) |   Reward | Wall (`s`) | Tasks/min | Warm hit | Prewarm `Create` (`ms`) | Prewarm `Suspend` (`ms`) | `TTFI` handle (`ms`) | Infra boot/wake (`p50 ms`) | `TTFC` / `Wake` combined (`ms`) | `TTFC` / `Wake` (`p50 ms`: `r2e` / `swe`) |     `Delete` (`ms`) |
| :------ | :--------- | :----------- | ------------------: | -----------: | -------: | ---------: | --------: | -------: | ----------------------: | -----------------------: | -------------------: | -------------------------: | ------------------------------: | ----------------------------------------: | ------------------: |
| `<arm>` | `<client>` | `<strategy>` | `<active> / <pool>` |  `<n> (<f>)` | `<rate>` |    `<sec>` |   `<tpm>` | `<pct>%` |     `<p50>/<p95>/<p99>` |      `<p50>/<p95>/<p99>` |  `<p50>/<p95>/<p99>` |                    `<p50>` |             `<p50>/<p95>/<p99>` |                     `<r2e_p50>/<swe_p50>` | `<p50>/<p95>/<p99>` |

### 3.2 Where time is spent: layer attribution (`Go` vs `Python` vs `ate` infra vs guest workload)

Decompose latencies by execution layer so host SDK (`Go` `clients/go` vs `Python` `ate_env_rl`),
`ate-env` / `ate` infrastructure, simulated LLM think pauses, and in-sandbox guest commands (`bash`
/ `git` / container `Python` `pytest`) are never conflated. Anchor single-RPC data-plane wire hop on
zero-compute `write_*` steps (`stats[event="step", name="write_patch"]`) or OTel spans.

#### A. First-command (`TTFC` / `Wake`) decomposition (`p50 ms`)

| Execution layer                     | Runtime / component                           | What happens                                                            | `<cold/none> r2e` (`ms`) | `<cold/none> swe` (`ms`) | `<warm/ref> r2e` (`ms`) | `<warm/ref> swe` (`ms`) | Share of `TTFC`/`Wake` |
| :---------------------------------- | :-------------------------------------------- | :---------------------------------------------------------------------- | -----------------------: | -----------------------: | ----------------------: | ----------------------: | ---------------------: |
| **Host Python SDK**                 | Host `Python` (`ate_env_rl`)                  | `0.0` on `--client go`/`go-direct`; `python` minus `go` delta if run    |                   `<ms>` |                   `<ms>` |                  `<ms>` |                  `<ms>` |               `<pct>%` |
| **Host client queue**               | Host `Go` / `Python` driver                   | Concurrency slot wait (`custom_metrics.sdk_slot_wait_ms`)               |                   `<ms>` |                   `<ms>` |                  `<ms>` |                  `<ms>` |               `<pct>%` |
| **Control-plane claim (`TTFI`)**    | Client -> `ate-env-api` -> `ateapi`           | `CreateActor` handle allocation (`0.0` in-band on warm-pool hits)       |                   `<ms>` |                   `<ms>` |                  `<ms>` |                  `<ms>` |               `<pct>%` |
| **Node image cache check**          | `atelet` (`Go` node daemon)                   | `manifest_fetch` + `download` + `oci_unpack` (`restores.*.phases`)      |                   `<ms>` |                   `<ms>` |                  `<ms>` |                  `<ms>` |               `<pct>%` |
| **Sandbox snapshot restore**        | `atelet` + `ateworker` (`ate` infra)          | Restore container rootfs + memory (`worker_restore` + `sandbox_assets`) |                   `<ms>` |                   `<ms>` |                  `<ms>` |                  `<ms>` |               `<pct>%` |
| **Data-plane proxy & route**        | Client -> `ate-env-api` -> `atenet` -> guest  | mTLS demand-wake trigger + gRPC RTT (`write_patch` `p50` or OTel)       |                   `<ms>` |                   `<ms>` |                  `<ms>` |                  `<ms>` |               `<pct>%` |
| **Subtotal — True infra startup**   | **Host SDK + `ate-env` + `ate` infra**        | **Sandbox restored, routed, and running guest PID 1**                   |               **`<ms>`** |               **`<ms>`** |              **`<ms>`** |              **`<ms>`** |           **`<pct>%`** |
| **In-sandbox 1st cmd (`baseline`)** | **Guest `bash` / `git` / container `Python`** | `outside` minus gRPC wire (`git add -A && git commit` / `.venv` prep)   |                   `<ms>` |                   `<ms>` |                  `<ms>` |                  `<ms>` |               `<pct>%` |
| **Total `TTFC` / `Wake` (`p50`)**   | **End-to-end measured**                       | **`stats[event="ttfi"]` or `stats[event="wake"]`**                      |               **`<ms>`** |               **`<ms>`** |              **`<ms>`** |              **`<ms>`** |             **`100%`** |

#### B. Full single-rollout (`task`) time budget (`<reference-arm>` `p50`)

| Execution layer                               | Runtime                      | Operations included                                                          | `r2e` rollout (`p50 ms` / `%`) | `swe` rollout (`p50 ms` / `%`) |
| :-------------------------------------------- | :--------------------------- | :--------------------------------------------------------------------------- | -----------------------------: | -----------------------------: |
| **1. Host Python SDK**                        | Host `Python` (`ate_env_rl`) | `0 ms` on `--client go`; client overhead delta when `--client python` is run |             `<ms> ms (<pct>%)` |             `<ms> ms (<pct>%)` |
| **2. Host client + `ate-env-api` + `atenet`** | `Go` + `Envoy`               | Per-step gRPC `WriteFile` + `StartProcess` wire hops across trajectory steps |             `<ms> ms (<pct>%)` |             `<ms> ms (<pct>%)` |
| **3. `ate` node restore infra (in-rollout)**  | `atelet` + runtime           | Node cache check + snapshot/checkpoint restore (`restores.*.phases[total]`)  |             `<ms> ms (<pct>%)` |             `<ms> ms (<pct>%)` |
| **4. Simulated LLM think pause**              | Host loader timer            | Intentional `Step.Think` sleep (`custom_metrics.llm_think_ms`)               |             `<ms> ms (<pct>%)` |             `<ms> ms (<pct>%)` |
| **5. In-sandbox shell / `git` commands**      | Guest `bash` / `git`         | `baseline`, `explore`, `read_files`, `apply_patch` (net of gRPC wire)        |             `<ms> ms (<pct>%)` |             `<ms> ms (<pct>%)` |
| **6. In-sandbox test suite & grader**         | **Guest `Python`**           | Container `.venv` / conda `pytest` (`run_tests`) + JSON grader (`grade`)     |             `<ms> ms (<pct>%)` |             `<ms> ms (<pct>%)` |
| **Total in-rollout span (`task` `p50`)**      | **All layers**               | **Rollout start -> reward verdict (`stats[event="task"]`)**                  |           **`<ms> ms (100%)`** |           **`<ms> ms (100%)`** |
| _Out-of-band pool lifecycle_                  | Host SDK + `ate` infra       | Upfront `create` + `suspend` + post-rollout `delete` outside `task` span     |        `+<ms> ms outside task` |        `+<ms> ms outside task` |

### 3.3 Raw sandbox lifecycle and command latencies (`<reference-arm>`)

Populate from `$ARTIFACT_DIR/[arms/<arm>/]report.json` (`stats[]`, `headline_baseline`,
`custom_metrics`), plus OTel `spans.jsonl` when `-enable-tracing` is enabled. Write `-` when an
operation does not occur in the selected strategy (for example `ttfc` on `100%` warm strategies or
`wake` on `none`).

| Metric                   | Span measured                                          | Bundle source                                                   | Count | Failed | p50 (ms) | p90 (ms) | p95 (ms) | p99 (ms) | Max (ms) |
| :----------------------- | :----------------------------------------------------- | :-------------------------------------------------------------- | ----: | -----: | -------: | -------: | -------: | -------: | -------: |
| `sdk_slot_wait`          | Concurrency slot wait before wire claim                | `custom_metrics.sdk_slot_wait_ms` / `headline_baseline`         | `<n>` |    `-` |  `<p50>` |  `<p90>` |  `<p95>` |  `<p99>` |  `<max>` |
| `actor_create`           | Substrate `ateapi` `CreateActor` RPC                   | `stats[create]` on `go-direct`, or OTel `CreateActor` span      | `<n>` |  `<f>` |  `<p50>` |  `<p90>` |  `<p95>` |  `<p99>` |  `<max>` |
| `sandbox_create`         | `CreateEnvironment` / SDK sandbox claim                | `stats[event="create"]`                                         | `<n>` |  `<f>` |  `<p50>` |  `<p90>` |  `<p95>` |  `<p99>` |  `<max>` |
| `prewarm`                | Warm-pool sandbox create + suspend                     | `stats[event="prewarm"]`                                        | `<n>` |  `<f>` |  `<p50>` |  `<p90>` |  `<p95>` |  `<p99>` |  `<max>` |
| `ttfi` (cmd start)       | Claim start -> sandbox handle ready / first cmd issued | `custom_metrics.wire_claim_to_ready_ms` / `headline_baseline`   | `<n>` |    `-` |  `<p50>` |  `<p90>` |  `<p95>` |  `<p99>` |  `<max>` |
| `first_cmd_exec`         | First command issued -> first command finished         | `custom_metrics.ready_to_first_cmd_ms` / `stats[step/baseline]` | `<n>` |  `<f>` |  `<p50>` |  `<p90>` |  `<p95>` |  `<p99>` |  `<max>` |
| `ttfc` (cmd finished)    | Claim start -> first command finished                  | `stats[event="ttfi"]`                                           | `<n>` |  `<f>` |  `<p50>` |  `<p90>` |  `<p95>` |  `<p99>` |  `<max>` |
| `suspend`                | `SuspendEnvironment` checkpoint                        | `stats[event="suspend"]`                                        | `<n>` |  `<f>` |  `<p50>` |  `<p90>` |  `<p95>` |  `<p99>` |  `<max>` |
| `wake`                   | First command on parked sandbox (issue -> finished)    | `stats[event="wake"]`                                           | `<n>` |  `<f>` |  `<p50>` |  `<p90>` |  `<p95>` |  `<p99>` |  `<max>` |
| `step` (`explore`)       | In-sandbox repository exploration command              | `stats[event="step", name="explore"]`                           | `<n>` |  `<f>` |  `<p50>` |  `<p90>` |  `<p95>` |  `<p99>` |  `<max>` |
| `step` (`read_files`)    | In-sandbox file reads                                  | `stats[event="step", name="read_files"]`                        | `<n>` |  `<f>` |  `<p50>` |  `<p90>` |  `<p95>` |  `<p99>` |  `<max>` |
| `step` (`apply_patch`)   | Gold patch write + `git apply`                         | `stats[event="step", name="apply_patch"]`                       | `<n>` |  `<f>` |  `<p50>` |  `<p90>` |  `<p95>` |  `<p99>` |  `<max>` |
| `step` (`run_tests`)     | Evaluation `pytest` suite execution                    | `stats[event="step", name="run_tests"]`                         | `<n>` |  `<f>` |  `<p50>` |  `<p90>` |  `<p95>` |  `<p99>` |  `<max>` |
| `step` (`grade`)         | Test output grading                                    | `stats[event="step", name="grade"]`                             | `<n>` |  `<f>` |  `<p50>` |  `<p90>` |  `<p95>` |  `<p99>` |  `<max>` |
| `task` (`r2e-gold`)      | End-to-end R2E-Gym rollout                             | `stats[event="task", name="r2e-gold"]`                          | `<n>` |  `<f>` |  `<p50>` |  `<p90>` |  `<p95>` |  `<p99>` |  `<max>` |
| `task` (`swebench-gold`) | End-to-end SWE-bench rollout                           | `stats[event="task", name="swebench-gold"]`                     | `<n>` |  `<f>` |  `<p50>` |  `<p90>` |  `<p95>` |  `<p99>` |  `<max>` |
| `delete`                 | `DeleteEnvironment` / `DeleteActor`                    | `stats[event="delete"]`                                         | `<n>` |  `<f>` |  `<p50>` |  `<p90>` |  `<p95>` |  `<p99>` |  `<max>` |
| `step_barrier`           | First rollout start -> last rollout finish in step     | `stats[event="step_barrier"]`                                   | `<n>` |  `<f>` |  `<p50>` |  `<p90>` |  `<p95>` |  `<p99>` |  `<max>` |

### 3.4 Step barrier, shard split, and optional node telemetry

| Step |  Live | Tasks | Barrier (ms) | Park (ms) | Rollout p50 / p99 / p100 (ms) | Skew (`p100/p50`) | LLM think (ms) | Env wait (ms) | Env overhead (%) |
| ---: | ----: | ----: | -----------: | --------: | ----------------------------: | ----------------: | -------------: | ------------: | ---------------: |
|  `1` | `<n>` | `<n>` |  `<barrier>` |  `<park>` |      `<p50> / <p99> / <p100>` |         `<skew>x` |   `<think_ms>` |   `<wait_ms>` |         `<pct>%` |

- **Shard split and contention**: `job=r2e` `<r2e_rewarded>/<r2e_tasks>` (`reward_rate=<rate>`),
  `job=swe` `<swe_rewarded>/<swe_tasks>` (`reward_rate=<rate>`), `slot_wait_p95_ms=<p95>`, dominant
  bottleneck `<rl_step_breakdown.dominant_bottleneck>`.
- **Node cache and `atelet` restore sub-phases** (when `SAMPLE_CACHE=1` or `HARVEST_RESTORES=1`
  ran): `<cache.misses_per_first_cmd>` misses/start, `restores.{first,wakes}.phases[]`
  (`p50/p90/p95`: `manifest_fetch=<p50>/<p90>/<p95>`, `download=<p50>/<p90>/<p95>`,
  `oci_unpack=<p50>/<p90>/<p95>`, `worker_restore=<p50>/<p90>/<p95>`, `total=<p50>/<p90>/<p95>`),
  and `outside=<p50>/<p90>/<p95>` ms.

## 4. Failures, findings, and caveats

- **Failures and retries**: `<0 failures and 0 retries, or count + root-cause layer per error>`.
- **Findings**:
  `<2-3 bullets on strategy wall-clock vs standing-pool trade-off and Go vs Go-direct vs Python overhead>`.
- **Caveats**:
  `<cluster or execution caveats affecting comparisons, such as local port-forward RTT, mixed node CPU families, or uncollected OTel spans for actor_create>`.
