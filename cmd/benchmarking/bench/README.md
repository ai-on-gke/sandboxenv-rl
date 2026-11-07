# `bench`: scenario runs and real RL training runs

`bench` orchestrates two kinds of benchmark and writes every artifact into one
directory (`-dest "$ARTIFACT_DIR"`). Scenario runs (`bench run -scenario`,
`benchmarking/scenarios/*.yaml`) run in process against a local scenario
client: provisioning is render-only, `Create` returns `env-N`, `Exec` returns
exit 0, `Snapshot` returns `snap-<name>`; no sandbox is created and no cluster
is called, so they exercise the orchestration and the report, not Substrate.
Real RL training runs are a verl trainer launched from `examples/`, which
`bench` measures around: `spinup`, `measure`, `reap`, `report` run outside the
training Job and read what it writes.

The simulated benchmark, `swebench-loader`, is a separate tool with its own
page: [`benchmarking/swebench/README.md`](../../../benchmarking/swebench/README.md).
It does not use `bench run`. Its documented flow runs `bench measure -k8s`
beside `swebench-loader report` (`benchmarking/swebench/SKILL.md`); `bench
report` reads `events/*.jsonl` under `-dest`, which the loader writes when run
with `-events-dir`.

## Where Ray is

Only the real RL path runs Ray, by two launch paths. `examples/Qwen3-1.7B-verl`
and `examples/Qwen3.5-35B-A3B-verl` run `ray start` inside the training Job
(`run_fully_async.sh`), launched by their `deploy.sh`. `examples/verl_swebench`
is a KubeRay `RayJob` (`ray-job.async-verl.yaml`), applied with `kubectl apply`
or `run-telemetry-e2e.sh`, and KubeRay creates the cluster. In both the
trainer's rollout workers call the ate-env API from Ray tasks. `bench` itself
does not start or depend on Ray: `bench spinup`, `measure`, `reap` and
`report` run outside the Job and read what it writes (`train.log`,
`traces/*.jsonl`). Scenario runs and the simulated loader have no Ray in their
path at all.

## Subcommands

`cmd/benchmarking/bench` is the unified orchestrator for both scenario-driven
synthetic workloads and real RL training recipes (`examples/*-verl`). Every
subcommand communicates through a single artifact directory (`-dest "$ARTIFACT_DIR"`):

| Subcommand | Purpose |
|---|---|
| `bench run` | Runs a declarative `scenario.yaml` in process (resolve -> prewarm -> execute -> measure -> reap -> report) against the local scenario client: no sandbox is created and no cluster is called |
| `bench resolve` | Resolve a `scenario.yaml` and dataset into a deterministic `schedule.json` |
| `bench render` | Render Kubernetes Job manifests from a `scenario.yaml` |
| `bench prewarm` | Prewarm templates or standing sandbox pools prior to the timed run window |
| `bench spinup` | Scale `WorkerPool` (`0 -> N` `ateworker` pods), wait for `ate-env-api` + all worker pods `Ready` (`-timeout` cap, `-stall-timeout` for no-progress failure), and record `workerpool_spinup_wall_ms` and `worker_pod_*_ms` into `measurements.json` and `events/substrate_lifecycle.jsonl`; on timeout the Ready pods' metrics are still recorded with `ok=false` |
| `bench spindown` | Scale `WorkerPool` (`N -> 0` `ateworker` pods), wait for all worker pods to terminate, and record `workerpool_spindown_wall_ms` and `worker_pod_terminate_ms` into `measurements.json` and `events/substrate_lifecycle.jsonl` |
| `bench measure` | Collect K8s (`-k8s`), Substrate (`-ate-endpoint`), Prometheus (`-prom-url`), node cache (`-sample-node-cache`), and restore timings (`-restore-log`) into `measurements.json` and `system_description.json`; with `-interval` it samples into `measurements_snapshots/` until signalled or `-duration` elapses, then writes both files from the samples |
| `bench reap` | Verify zero orphaned environments remain post-run and emit `events/reap.jsonl` |
| `bench report` | Ingest `events/*.jsonl`, `traces/*.jsonl`, `train.log`, `measurements.json`, and `system_description.json` into `report.json` and `report.md` |
| `bench validate` | Validate `report.json`, `measurements.json`, and `system_description.json` against strict `v1` schemas, and with `-folder` the archive folder name |
| `bench upload` | Validate, then copy `$ARTIFACT_DIR` to `gs://<bucket>/<folder>/` |
| `bench compare` | Diff two or more `report.json` files or artifact directories side-by-side |

### 1. Synthetic / Scenario-Driven Run

```bash
ARTIFACT_DIR=/tmp/bench-run
go run ./cmd/benchmarking/bench run \
  -scenario benchmarking/scenarios/multi-job-swebench.yaml \
  -dest "$ARTIFACT_DIR"
go run ./cmd/benchmarking/bench validate -dest "$ARTIFACT_DIR"
```

### 2. Real RL Training Run (`examples/*-verl`)

Build the binary once: the sampler in step 3 runs in the background and is
stopped with a signal, and `go run` neither forwards `SIGINT` to its child nor
exits on it (its `signalsToIgnore`), so a `go run` sampler would keep going.

```bash
ARTIFACT_DIR=/tmp/rl-bench-run
go build -o bin/bench ./cmd/benchmarking/bench

# 1. Spin up Substrate WorkerPool (0 -> N ateworker pods) & capture pre-run node image-cache snapshot.
#    -timeout caps the whole wait; -stall-timeout fails fast only when no new pod has gone Ready
#    for that long, so a 0 -> 30 cold autoscale (nodes provisioning for ~10 min) finishes while a
#    stuck pool is reported in minutes. On timeout the Ready pods' metrics are still recorded.
bin/bench spinup -namespace ate-env -workerpool default-template-workerpool -replicas 32 \
  -timeout 40m -stall-timeout 5m -dest "$ARTIFACT_DIR"
bin/bench measure -sample-node-cache "$ARTIFACT_DIR/cache_before.prom"

# 2. Launch the RL recipe (with ATE_ENV_TRACE_DIR=/workspace/results/traces enabled in the Job)
(cd examples/Qwen3-1.7B-verl && ./deploy.sh up)

# 3. Sample live K8s + Substrate state for the whole training window. A single
#    post-run measure sees an idle cluster (active_sandboxes 0, packing 0%).
#    When the sampler stops it writes system_description.json from its last
#    sample and rolls measurements_snapshots/*.json up into measurements.json
#    (keeping what spinup merged in); bench report rolls them up again.
kubectl -n ate-env port-forward deploy/ate-env-api 18443:8443 &
bin/bench measure -k8s -ate-endpoint 127.0.0.1:18443 \
  -atespace ate-env -dest "$ARTIFACT_DIR" -interval 30s &
SAMPLER_PID=$!

# 4. After training completes, stop the sampler, copy train.log + traces/*.jsonl into
#    $ARTIFACT_DIR, then reap, sample post-cache, & spin down WorkerPool (N -> 0)
kill -INT "$SAMPLER_PID"; wait "$SAMPLER_PID"
bin/bench reap -ate-endpoint 127.0.0.1:18443 -atespace ate-env -prefix verl- -dest "$ARTIFACT_DIR"
bin/bench measure -sample-node-cache "$ARTIFACT_DIR/cache_after.prom"
benchmarking/swebench/harvest-restore-timings.sh <run-id> "$ARTIFACT_DIR/restore_timings.jsonl"
bin/bench spindown -namespace ate-env -workerpool default-template-workerpool -dest "$ARTIFACT_DIR"

# 5. Generate and validate report.json + report.md. Both read only $ARTIFACT_DIR and never touch
#    the cluster: after a failed or stale report, fix the artifact and rerun these two, never `bench run`.
bin/bench report -dest "$ARTIFACT_DIR" -scenario scenario.yaml
bin/bench validate -dest "$ARTIFACT_DIR"
```

`bench measure -interval` exits non-zero when every sample failed (a scrape
that never answered), so a broken sampler does not pass as a quiet run; a
sample that fails here and there is reported on stderr and skipped.

Where the report's load parameters (`tasks_per_step`, `group_size`, `steps`,
`max_concurrent`) come from, in order: the scenario's **first real job** when
it declares them (`-scenario`; a multi-job scenario is described by that job,
declare the aggregate there), then the trainer's resolved config dump in
`train.log` (`'train_batch_size': 64`, the post-override values), then the
last CLI-form occurrence (`data.train_batch_size=64`). A banner or wrapper
that echoes profile defaults at the top of the log therefore never wins;
declare the values in the scenario to make them independent of log format
altogether.

Trace shards copied while a worker was still writing (a `cat` of a live
`trace.jsonl`) end in a torn record. `bench report` and `bench validate` drop
a torn **last** line, count it (`trace_truncated_lines` in `report.json`,
`trace_truncated` in `validation.json`, a stderr note naming file and line)
and carry on; a malformed line anywhere else is corruption and still fails.
Do not pre-filter shards with `grep '}$'`: it hides that corruption and
discards valid records whose last field is not an object.

#### Driving real jobs from `bench run`

A `kind: real` job in `scenario.yaml` is waited on, logged, and traced by
name. Nothing in `bench` assumes a particular Job, RayJob, or JobSet naming
scheme; declare what the manifest creates:

```yaml
jobs:
  - name: job-0
    kind: real
    real_mode: trace
    trainer_manifest: manifests/job-0.yaml
    head_job: trainer-0-head            # Job to wait on; default: name
    pod_selector: ray.io/cluster=trainer-0   # pods for logs; default: job-name=<head_job>
    timeout: 4h                         # bound on the wait; default 1h
    trace_collection:                   # pull traces out of every matching pod
      pod_selector: ray.io/cluster=trainer-0 # default: pod_selector above
      container: keepalive              # container to exec into (shares the trace volume)
      trainer_container: trainer        # wait for this container to exit, then harvest
      remote_dir: /tmp/ate-traces       # absolute in-pod dir holding *.jsonl
    env_vars:
      TOTAL_STEPS: "40"
```

- **Multi-job scenarios must set `head_job` per job.** Every driver waits on
  its own head Job, so `bench run` returns only when the last job finishes.
  Without it the VeRL example falls back to `verl-qwen3-1p7b-fully-async`.
  A Job that ends `Failed` fails the run at once (its logs are still read);
  it is not waited on until the timeout. Set `timeout` for runs longer than
  an hour.
- **`trace_collection` needs the pod to outlive the trainer.** A completed
  pod cannot be exec'd into, so give the Job a keepalive sidecar (for example
  `busybox sleep`) that mounts the same `emptyDir` as the trainer, name it in
  `container`, and name the trainer in `trainer_container`. `bench run` then
  waits for the trainer container to terminate in every pod of the trace
  selector (`trace_collection.pod_selector`, else `pod_selector`, else
  `job-name=<head_job>`; pods under it that do not run the trainer, Ray
  workers for instance, neither block the wait nor are read for logs), reads
  the trainer's logs pod by pod, streams `tar -ch` out of the sidecar (`-h`
  dereferences symlinks so only regular files land), writes
  `traces/<job>/<pod>/*.jsonl`, and deletes the head Job. A pod that is
  `Failed` without a trainer exit code (evicted, OOM-killed) fails the job
  like a non-zero exit. A pod without `remote_dir` is skipped; a `tar` that
  fails (a file changed under it) fails the harvest naming the pod. The
  head Job is deleted on every way out, a timed-out wait or an interrupted
  run included, because the sidecar would otherwise hold it open for ever;
  if that delete itself fails, `bench run` says so and the Job is yours to
  delete. The verl example's pods carry no `job-name` label, so
  `trainer_container` on it needs a `pod_selector`.
  Each job already gets its own `ATE_ENV_TRACE_DIR=<dest>/traces/<job>`;
  wrappers that write their own event files should likewise key the path on
  `$ATE_BENCH_RUN_ID` and `$ATE_BENCH_JOB_NAME`, never a fixed path on a
  shared mount.
- **`env_vars` arrive in the process environment** of `trainer_command` /
  the `kubectl` calls and are meant to win. A wrapper that `source`s a
  profile afterwards must preserve them (save and re-apply, or `: "${VAR:=default}"`).
- **Lifecycle timings come from `bench spinup` / `bench spindown -dest`**,
  which append `events/substrate_lifecycle.jsonl` and merge
  `workerpool_spinup_wall_ms` / `workerpool_spindown_wall_ms` into
  `measurements.json`; `bench run` keeps that file's contents (and refuses
  to run over one it cannot decode). Wrappers that manage the WorkerPool
  themselves should call these instead of injecting numbers afterwards; a
  phase that did not run is omitted, never written as `0`.
- **`-snapshot-interval 30s -k8s [-prom-url …]`** makes `bench run` sample
  telemetry itself for exactly the job window and roll it into
  `measurements.json` before the report is built. When not one sample
  succeeded, `bench run` exits non-zero after writing the run's artifacts: the
  events and report are there to inspect, but a run without its telemetry is
  not a result. `bench measure -interval` with `-dest` writes its samples to
  `<dest>/measurements_snapshots`; a `-snapshots-dir` anywhere else is refused,
  since nothing would read it.

### 3. Archiving Results & Further Guides

Archive completed `$ARTIFACT_DIR` directories to object storage under a
consistent folder name, with `_traced` iff the run captured an SDK trace
(`report.json` `trace_captured: true`; traced latencies include trace overhead):

```text
gs://<YOUR_BENCHMARK_BUCKET>/<YYYY-MM-DD>_<model-family>-<size>_<dataset>_<nickname>/
gs://<YOUR_BENCHMARK_BUCKET>/<YYYY-MM-DD>_<model-family>-<size>_<dataset>_<nickname>_traced/
```

- **Telemetry scopes & feature debug plugins**: [`internal/benchmarking/TELEMETRY_GUIDE.md`](../../../internal/benchmarking/TELEMETRY_GUIDE.md)
- **Agent integration & execution playbook**: [`benchmarking/SKILL.md`](../../../benchmarking/SKILL.md)
