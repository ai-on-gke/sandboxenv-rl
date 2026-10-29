# Telemetry Scopes & Feature Plugin Guide

This guide explains how to control which subsystem telemetry scopes are collected/reported and how to capture feature-specific metrics or debug logs without modifying core benchmarking code.

---

## 1. Telemetry Scopes (`-scopes` / `telemetry.scopes`)

Telemetry is partitioned into four independent scopes (default: `all`):

| Scope | Aliases | What It Collects & Reports |
| :--- | :--- | :--- |
| **`rl`** | `trainer`, `rl-trainer` | **RL Trainer & Rollout Perspective**: Step timings (`timing_s/step`, `timing_s/gen`, `timing_s/update_actor`, `timing_s/old_log_prob`, `timing_s/param_sync`/`update_weights`), rewards, hold-out `val-core` / `val-aux` scores, entropy/KL, convergence, and `llm_think_ms` vs `env_wait_ms` rollout decomposition. |
| **`user-sandbox`** | `user`, `client`, `client-sandbox` | **User / Agent / SDK Perspective**: Client-side lifecycle RPCs (`create`, `ttfi` and its 3 sub-phases `sdk_slot_wait_ms` / `wire_claim_to_ready_ms` / `ready_to_first_cmd_ms`, `wake`, `agent_exec`, `read_file`, `write_file`, `suspend`, `delete`), working-set hit rate, and peak/sustained acquisition throughput. |
| **`substrate`** | `substrate-sandbox` | **Substrate Sandbox Perspective**: `ate-api` / `ate-k8s-bridge` active vs parked actors, `WorkerPool` spin-up (`workerpool_spinup_wall_ms`, `worker_pod_schedule_ms`, `worker_pod_init_ms`, `worker_pod_container_start_ms`, `worker_pod_total_ready_ms`, `workerpool_rollout_span_ms`) and spin-down (`workerpool_spindown_wall_ms`, `worker_pod_terminate_ms`), template resource footprint, `atelet` node image-cache deltas (`cache_before.json`/`cache_after.json`), and `restore_timings.jsonl` sub-phases. |
| **`k8s`** | `k8s-health`, `cluster`, `kubernetes` | **Kubernetes Cluster Health & Hardware Cost**: Node/pod bin-packing, `metrics.k8s.io` actual CPU & memory WSS, pod startup sub-phases (`pod_schedule_ms`, `pod_init_ms`, `pod_container_start_ms`, `pod_total_ready_ms`, and `worker_pod_*_ms` for standing `WorkerPool` pods), and `oom_kills` / `pod_evictions` / `container_restarts` (applies to any K8s sandbox runtime). |

### CLI Usage
```bash
# Collect only Kubernetes cluster health (e.g. when benchmarking a non-Substrate sandbox system):
bench measure -dest "$ARTIFACT_DIR" -k8s -scopes k8s

# Generate a report with only User-Sandbox and K8s sections (skipping RL and Substrate):
bench report -dest "$ARTIFACT_DIR" -scopes user-sandbox,k8s
```

---

## 2. Declarative Feature Debug Plugins (No Go Changes Required)

When testing or debugging a specific feature, attach ad-hoc Prometheus queries, Kubernetes pod log harvesters, log/JSONL regex metric extractors, or diagnostic commands via CLI flags or `scenario.yaml`. All extracted metrics are written to `measurements.json` / `report.json` under `custom_metrics` and rendered in `report.md`.

### Option A: CLI Flags (`bench measure` / `bench report`)
```bash
# 1. Scrape a feature-specific Prometheus query into custom_metrics["criu_dump_ms"]:
bench measure -dest "$ARTIFACT_DIR" -prom-url http://localhost:9090 \
  -plugin-prom 'criu_dump_ms=histogram_quantile(0.95, rate(atelet_criu_dump_duration_ms_bucket[5m]))'

# 2. Harvest pod logs matching a label selector (and optional regex filter) to $ARTIFACT_DIR/plugins/<name>.log
#    and record the matching line count in custom_metrics["page_fault_warns"]:
bench measure -dest "$ARTIFACT_DIR" \
  -plugin-log 'page_fault_warns=ate-system/app=atelet:WARN.*uffd_page_fault'

# 3. Extract numeric latency/counter distributions from any log or JSONL file in $ARTIFACT_DIR:
bench report -dest "$ARTIFACT_DIR" \
  -plugin-metric 'snapshot_upload_ms=logs/*.log:snapshot_upload_ms=(?P<value>[0-9.]+)'

# 4. Run a custom diagnostic command and save output to $ARTIFACT_DIR/plugins/<name>.log:
bench measure -dest "$ARTIFACT_DIR" \
  -plugin-cmd 'fd_count=kubectl exec -n ate-system deploy/ate-api -- sh -c "ls /proc/1/fd | wc -l"'
```

### Option B: Declarative `scenario.yaml`
```yaml
schema_version: v1
name: my-feature-benchmark
telemetry:
  scopes: [rl, user-sandbox, substrate, k8s]
  plugins:
    - name: criu_dump_ms
      kind: prom_query
      scope: substrate
      expr: 'histogram_quantile(0.95, rate(atelet_criu_dump_duration_ms_bucket[5m]))'
      unit: ms
    - name: atelet_uffd_warns
      kind: k8s_logs
      scope: substrate
      namespace: ate-system
      label_selector: app=atelet
      grep: 'WARN.*uffd'
    - name: custom_sync_ms
      kind: regex_metric
      scope: rl
      glob: 'verl-head.log'
      regex: 'custom_sync_ms[:=]\s*(?P<value>[0-9.]+)'
      unit: ms
```

---

## 3. Registering Custom Go Collectors or Ingesters

If a feature requires custom Go logic:

1. **Server / Cluster Collector** (`internal/benchmarking/measure/collector.go`):
   Implement `measure.Collector` and call `measure.RegisterCollector(myCollector{})` in `init()`.
2. **Log / Trace Ingester** (`internal/benchmarking/job/ingester.go`):
   Implement `job.TelemetryIngester` and call `job.RegisterIngester(myIngester{})` in `init()`.
