# Mixed R2E-Gym and SWE-bench Benchmark Report (`<run-id>`)

Copy this template to `$ARTIFACT_DIR/BENCHMARK_REPORT.md` after a run finishes (keep
`$ARTIFACT_DIR/report.md` and `$ARTIFACT_DIR/comparison.md` intact). Every field below maps directly
to `$ARTIFACT_DIR/comparison.json`, `$ARTIFACT_DIR/[arms/<arm>/]report.json`,
`$ARTIFACT_DIR/[arms/<arm>/]run_window.json`, and `system_description.json` (from
`bench measure -k8s`).

| Field               | Value                                                   | Source                                                 |
| :------------------ | :------------------------------------------------------ | :----------------------------------------------------- |
| **Run ID**          | `<run-id>`                                              | `report.json` (`run`)                                  |
| **Date (UTC)**      | `<start_ts to end_ts>`                                  | `run_window.json`                                      |
| **Duration**        | `<wall_s> s`                                            | `report.json` (`run_ms`) or `comparison.json`          |
| **Cluster**         | `<cluster-name>` (`<zone_topology>`)                    | `system_description.json` (`cluster`, `zone_topology`) |
| **Versions**        | K8s `<k8s_ver>`, Substrate `<sub_ver>`, env `<git_sha>` | `system_description.json`                              |
| **Artifact bundle** | `<$ARTIFACT_DIR or gs://benchmark-bucket/folder/>`      | Local path or `bench upload`                           |

## Summary

- **Workload**: `<tasks>` rollouts across `<distinct_images>` images (`<r2e_images>` R2E-Gym +
  `<swe_images>` SWE-bench) at `<max_concurrent>` max concurrency; `<rewarded>/<tasks>` rewarded
  (`reward_rate=<rate>`), `<failed>` failed.
- **Start footing**: `<cold boot | cold restore | warm restore | wake>` (goldens
  `<pre-baked | baked in-band>`).
- **Strategy and client headline**:
  `<summarize wall clock, warm hit rate, and standing pool across tested (client, strategy) arms>`.
- **Latency and bottleneck**: `create` `<p50>/<p95>` ms, `ttfi` `<p50>/<p95>` ms, `wake`
  `<p50>/<p95>` ms; dominant bottleneck `<rl_step_breakdown.dominant_bottleneck>`.

## 1. Setup

Populate from CLI flags, `report.json` (`labels`, `distinct_images`), and `system_description.json`.

| Parameter            | Value                                             | Cluster component    | Configuration                                              |
| :------------------- | :------------------------------------------------ | :------------------- | :--------------------------------------------------------- |
| **Datasets**         | `<r2e>` R2E-Gym + `<swe>` SWE-bench (`<exclude>`) | **Sandbox nodes**    | `<count> x <machine_type>` (`<per_node>` active/node)      |
| **Concurrency**      | `<max_concurrent>` (`r2e=<w1>`, `swe=<w2>`)       | **`WorkerPool`**     | `<worker_pod_count>` worker pods (`cap=<sandboxes_cap>`)   |
| **Rollouts / image** | `<rollouts>` (`steps=<steps>`, `mode=<mode>`)     | **Base template**    | `<base_template>` (`<cpu>` vCPU, `<mem>` GiB, `<runtime>`) |
| **Strategies**       | `<strategy list>` (`window_size=<win>`)           | **`ate-env-api`**    | Service `<api_service>`, `<api_image>`                     |
| **Clients**          | `<client list>` (`<execution>` mode)              | **Workload profile** | `<graded (r2e-gold + swebench-gold) \| io>`                |

## 2. Command

```bash
<exact run-r2e-swebench.sh command>
```

## 3. Results

### 3.1 Arm comparison

Populate from `$ARTIFACT_DIR/comparison.md` (or one row from `report.json` on single-arm runs).

| Arm     | Client     | Strategy     | Tasks | Failed | Reward rate | Wall (s) | Tasks/min | Warm hit rate | Peak active | Standing pool | TTFI p50/p95 (ms) | Wake p50/p95 (ms) |
| :------ | :--------- | :----------- | ----: | -----: | ----------: | -------: | --------: | ------------: | ----------: | ------------: | ----------------: | ----------------: |
| `<arm>` | `<client>` | `<strategy>` | `<n>` |  `<f>` |    `<rate>` |  `<sec>` |   `<tpm>` |      `<pct>%` |  `<active>` |      `<pool>` |     `<p50>/<p95>` |     `<p50>/<p95>` |

### 3.2 Operation latencies (`<reference-arm>`)

Populate from `$ARTIFACT_DIR/[arms/<arm>/]report.json` (`stats[]`). Include only events emitted in
the run.

| Event          | Name            | Count | Failed | p50 (ms) | p90 (ms) | p95 (ms) | Max (ms) |
| :------------- | :-------------- | ----: | -----: | -------: | -------: | -------: | -------: |
| `create`       |                 | `<n>` |  `<f>` |  `<p50>` |  `<p90>` |  `<p95>` |  `<max>` |
| `prewarm`      |                 | `<n>` |  `<f>` |  `<p50>` |  `<p90>` |  `<p95>` |  `<max>` |
| `ttfi`         |                 | `<n>` |  `<f>` |  `<p50>` |  `<p90>` |  `<p95>` |  `<max>` |
| `suspend`      |                 | `<n>` |  `<f>` |  `<p50>` |  `<p90>` |  `<p95>` |  `<max>` |
| `wake`         |                 | `<n>` |  `<f>` |  `<p50>` |  `<p90>` |  `<p95>` |  `<max>` |
| `step`         | `baseline`      | `<n>` |  `<f>` |  `<p50>` |  `<p90>` |  `<p95>` |  `<max>` |
| `step`         | `run_tests`     | `<n>` |  `<f>` |  `<p50>` |  `<p90>` |  `<p95>` |  `<max>` |
| `step`         | `grade`         | `<n>` |  `<f>` |  `<p50>` |  `<p90>` |  `<p95>` |  `<max>` |
| `task`         | `r2e-gold`      | `<n>` |  `<f>` |  `<p50>` |  `<p90>` |  `<p95>` |  `<max>` |
| `task`         | `swebench-gold` | `<n>` |  `<f>` |  `<p50>` |  `<p90>` |  `<p95>` |  `<max>` |
| `delete`       |                 | `<n>` |  `<f>` |  `<p50>` |  `<p90>` |  `<p95>` |  `<max>` |
| `step_barrier` |                 | `<n>` |  `<f>` |  `<p50>` |  `<p90>` |  `<p95>` |  `<max>` |

### 3.3 Step barrier and shard split (`<reference-arm>`)

Populate from `report.json` (`rollout_breakdown[]`, `rl_step_breakdown`, and `jobs`).

| Step | Tasks | Barrier (ms) | Rollout p50 / p100 (ms) | Skew (`p100/p50`) | LLM think (ms) | Env wait (ms) | Env overhead (%) |
| ---: | ----: | -----------: | ----------------------: | ----------------: | -------------: | ------------: | ---------------: |
|  `1` | `<n>` |  `<barrier>` |        `<p50> / <p100>` |         `<skew>x` |   `<think_ms>` |   `<wait_ms>` |         `<pct>%` |

- **Per-shard completion**: `job=r2e` `<r2e_rewarded>/<r2e_tasks>` (`reward_rate=<rate>`), `job=swe`
  `<swe_rewarded>/<swe_tasks>` (`reward_rate=<rate>`), `slot_wait_p95_ms=<p95>`.
- **Optional node cache and `atelet` restore phases** (include only when `SAMPLE_CACHE=1` or
  `HARVEST_RESTORES=1` ran):
  `<misses_per_first_cmd> misses/start; oci_unpack_ms=<p50>/<p95>, worker_restore_ms=<p50>/<p95>, outside_atelet_ms=<p50>/<p95>`.

## 4. Findings and caveats

- **Findings**:
  `<2-3 bullets on strategy wall-clock vs standing-pool trade-offs, Go vs Python overhead, and any failures>`.
- **Caveats**:
  `<cluster or execution caveats affecting cross-run comparisons, such as local port-forward RTT, CPU family mix, or unbaked goldens>`.
