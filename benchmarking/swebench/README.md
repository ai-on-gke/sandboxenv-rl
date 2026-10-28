# Fast Substrate RL benchmark (SWE-bench Verified)

A loader that drives real SWE-bench task images through the ate-env API the
way a simulated RL job does, and a report over what every phase cost. It is
the "Fast" rung of the RL benchmark ladder: regular runs that catch
regressions in the sandbox SDK, Substrate or the controller, on a workload
that is the real thing rather than a Python image printing READY.

Code: `internal/benchmarking/swebench` (library, tested against an in-memory
backend) and `cmd/benchmarking/swebench-loader` (binary). The layout follows
substrate's `internal/benchmarking/boomer` so a user class can be lifted into
that harness later; Go does not allow importing another module's `internal`
packages, so nothing is imported from it.

## Workloads

Both are parameters of one binary. Images, fan-out, width, steps and the
trajectory are all flags, so the smoke test and the 500-wide run differ only
in numbers.

| Shape | Flags | What happens |
|---|---|---|
| eval (SWE-bench Verified evaluation) | `-mode eval -images 500 -tasks-per-image 6 -width 500` | 500 in flight. Each task: create from the image, replay the trajectory, record the reward, delete. With the offline exclusion list the 500 become 445 images and 2,670 tasks; the loader prints the reduced count. |
| steps (training loop) | `-mode steps -images 500 -rollouts 2 -steps 8 -park suspend` | 1,000 environments created once. Each step replays the trajectory on all of them, waits for the last (the barrier), then parks the fleet. `-park` is `suspend`, `delete` or `none`. |
| steps with a window (pools per step) | `-mode steps -images-per-step 32 -rollouts 16 -steps 6 -park delete` | Each step takes the next 32 images (wrapping around the dataset), creates 16 fresh environments per image, 512 in all, runs the step behind a barrier, and retires them. Nothing survives a step, so every task pays the cold path, and a new image is a new derived template and a golden bake on first use: the shape of a trainer that creates and retires pools every step. Run several loaders at once, one run id each, for several jobs on one cluster. |

The trajectory is YAML (`internal/benchmarking/swebench/trajectories/`,
or `-trajectory FILE`). The built-in `swebench-gold` does what a SWE agent
does on a task, with the gold patch as the model's edit: explore, read, write
the patch, `git apply`, write the harness eval script, run it. The reward is
the SWE-bench verdict: `swebench-grade.py` (written into the sandbox by the
trajectory) parses the log with the repo's harness parser and checks the
instance's FAIL_TO_PASS and PASS_TO_PASS tests, the same rules the harness
applies. Neither the script's nor the test command's exit code is the
verdict: unrelated tests in the same files fail routinely. A healthy run
rewards every task and a drop in reward rate is a correctness regression.
`python3 -m pytest benchmarking/swebench/tests` covers the grader. Steps carry think times, the
simulated policy latency between commands.

## What is measured

One JSONL event per operation, wall time as the loader sees it (network and
API server included, which is what a trainer sees):

| Event | Meaning |
|---|---|
| `create` | CreateEnvironment call |
| `ttfi` | create start to the first command's return (time to first instruction) |
| `wake` | first command after a suspend park, including the wake it triggers |
| `step` (by name) | each trajectory step, with exit code; the reward step carries the reward and an output tail |
| `task` | one replay end to end, ok and reward |
| `suspend`, `delete` | the calls |
| `step_barrier` | steps mode: first task start to last task end in a training step |
| `park` | steps mode: parking the whole fleet |
| `run` | the whole run |

`swebench-loader report RUN.jsonl -json summary.json` prints count, failed,
p50, p95 and max per event, a per-step table (tasks, barrier, park, wake
p50/p95) and the most frequent errors. The JSON is the artifact to diff
between runs. Given several files, `report a.jsonl b.jsonl` prints one table
with a column per run: p50/p95 per event, and per step the live environments,
tasks, failures, barrier, park and wake p50/p95. `-json` then writes the
summaries as an array. Files go before the flags.

Every run event carries labels: the loader's own (mode, width, images,
trajectory, dataset, the shape flags, connections, base template, exclude
file, instances and the timeouts) plus any `run -label KEY=VALUE`, which is
where the Substrate commit, the cluster and the arm go. A `-label` with a key
the loader records itself is refused, so the same-workload check below cannot
be overridden. The report prints
them under its header, and the comparison shows one row per label key, so a
column's provenance is in the table. `report -arm pull=p1.jsonl,p2.jsonl
-arm stream=s1.jsonl,s2.jsonl` gives one column per arm, in flag order. Two
rules pool an arm's reps: counts add up (tasks, failed, rewarded, a step's
live environments, and the `n` of every event row, whose percentiles are
computed over the merged raw events, never over per-run percentiles), and
durations take the median across the reps (a step's barrier and park, and
the run), the mean of the two middle values for an even count. A `reps` row
says how many runs each column pools, and the JSON lists their run ids. The
reps of an arm must be complete runs of one workload: each with a run event
and the loader's labels, no run id twice or in two arms, and the loader's
own labels equal (`exclude_file` and `instances` compare by name or count
plus a digest of their ids); a `-label` value that differs across them shows
every value `|`-joined, so a column that mixes commits says so. An operator
label named like a built-in row (`tasks`, say) prints as `label.tasks`. A
path in `-arm` may not contain a comma. `report RUN.jsonl
-top 10` lists the ten slowest successful first commands and the ten slowest
successful wakes by environment, for finding the straggler.

## How many actors, and on which clusters

Every environment the loader creates is one Substrate actor. Three flags
set how many:

| Flag | Controls |
|---|---|
| `-width` | actors in flight at once; also the cap on parallel creates |
| `-tasks-per-image` | eval: actors created per image, each a full create, replay, delete |
| `-rollouts` | steps: actors per image kept alive across the training steps |

So `-images 445` is many images with one actor each, `-images 1
-tasks-per-image 2000 -width 500` is one image with 2,000 actors 500 at a
time, and `-mode steps -rollouts 2 -steps 8` is both with a park between
steps. Above a few hundred in flight, raise the API server's database
connection pool: a burst of about 900 creates exhausted the default.

The loader does not decide placement. Whether several actors share one
worker pod is the WorkerPool's capacity on the cluster; configure it there
and read the packing from Substrate's metrics, not from the report.

The loader never talks to Kubernetes. `-endpoint` is an ate-env API address,
so "which cluster" is "which API". To compare clusters (builds, node images,
image delivery) run the same manifest, trajectory and flags once per
cluster, with `CONTEXT=<kubectl context>` pointing the in-cluster script at
each cluster and the cluster's name in the run id, then put the events files
side by side with `swebench-loader report a.jsonl b.jsonl`. Each cluster
needs the full setup below: Substrate and the ate-env API installed, the
images reachable from it, and the golden tags baked there (a golden tag is
per cluster, so the first run on a fresh cluster pays the bake for every
image).
One run never spans clusters.

Telemetry is the JSONL file only. There is no OpenTelemetry export yet; the
events map one to one onto spans and that export is planned, with the file
staying the source of truth so a run does not depend on a collector.

## Prepare once per project

1. Put the task images in a registry the nodes can pull from. If the nodes
   can read your mirror directly, skip this step. Otherwise copy them by
   digest with crane; `tags.txt` has one tag per line, for example every
   `sweb.eval.x86_64.*` tag of the source repository without the `-zran`
   variants:

   ```bash
   SRC=SOURCE_REGISTRY/swebench-verified
   DST=us-central1-docker.pkg.dev/PROJECT/swebench/swebench-verified
   crane ls $SRC | grep -v -- '-zran$' > tags.txt
   benchmarking/swebench/mirror-images.sh $SRC $DST tags.txt 8
   ```

2. Build the manifest: gold patch, test patch, harness eval script, log
   parser and the digest-pinned image per instance. `--registry` is the
   repository the images live in, the same `$DST` as above (or the source
   repository when the nodes can read it).

   ```bash
   pip install datasets 'swebench>=5'
   benchmarking/swebench/make-dataset.py --registry $DST \
     --revision <dataset commit> --out swebench-verified.jsonl
   ```

   `--image-ref` sets how an instance maps to an image reference (default:
   one tag per instance, `sweb.eval.x86_64.<instance_id>`, in one
   repository); the script lists instances whose image it cannot resolve. It
   loads the dataset at a resolved commit (`--revision`, else the current
   head) and writes that commit (`resolved_revision`; the file's sha256 for a
   local dataset) and the swebench version to `OUT.meta.json`.

3. Install ate-env with a base template (`ate-env manifest template`). Per
   image, the API derives one template from it on first use and bakes the
   golden snapshot then; the first create of each image pays that.

## Run

From a machine with a port-forward (fine for the smoke test and the step
loop):

```bash
kubectl -n ate-env port-forward svc/ate-env-api 7777:7777 &
go run ./cmd/benchmarking/swebench-loader run -dataset swebench-verified.jsonl \
  -images 5 -tasks-per-image 1 -width 5 -run-id smoke
go run ./cmd/benchmarking/swebench-loader report smoke.jsonl
```

`-dry-run` loads and renders everything and prints the plan without touching
the API. `-keep` leaves the fleet in place for inspection.

When ate-env-api requires a bearer token (`-auth-token-file` on the API), give
the loader the same token: `-auth-token-file PATH`, or the
`SUBSTRATE_ENV_AUTH_TOKEN` environment variable that the `ate-env` CLI also
reads. The file wins when both are set. Without either, the loader sends no
token.

For the 500-wide shapes run inside the cluster so the loader sits next to the
API and spreads over its replicas (`-connections`, default 4):

```bash
KO_DOCKER_REPO=gcr.io/PROJECT/repo benchmarking/swebench/run-in-cluster.sh swe-eval500x6 swebench-verified.jsonl \
  -mode eval -images 500 -tasks-per-image 6 -width 500 -connections 8
```

The script bakes the dataset, the exclusion list and any `-trajectory` file
you pass (with the files its `write` steps name, so a grader script travels
with its trajectory, and nothing else from its directory) into the loader image, runs a Job next to ate-env-api, follows the full loader log (step failures
included), copies the events file back to the current directory, prints the
report and deletes the Job. `CONTEXT=<kubectl context>` selects the cluster;
the same command with a different context and run id is how two clusters are
compared. For an API with a token, set `AUTH_TOKEN_SECRET`
to a Secret in the namespace whose key `token` holds it; the Job passes it to
the loader as `SUBSTRATE_ENV_AUTH_TOKEN`.

## Image-delivery workloads

Three built-in trajectories exercise image delivery rather than SWE-bench
grading, for comparing conventional pull, image streaming and other loaders:
`workload-file-read` (read a sample of files), `workload-compile`
(byte-compile every Python file, which reads each one and writes next to it)
and `workload-read-all` (read every file, the worst case for lazy loading).
Each starts with a `probe` step that runs `true`, so `ttfi` is the cold start
alone and the workload's own time is its own `step` row. They need no grading
material, so the manifest can be image-only:

```bash
benchmarking/swebench/make-dataset.py --images-file images.txt --out images.jsonl
swebench-loader run -dataset images.jsonl -trajectory workload-read-all -images 200 -width 200 -run-id pull-baseline
```

The workloads take their knobs from `run -set KEY=VALUE`, through the
trajectories' `var` function, and each file states its defaults, which fit a
SWE-bench image. A value lands in the command as one quoted shell word. Each
`-set` is recorded as a `set.KEY` label, which the reps of an arm must agree
on, and a key that no trajectory reads is an error:

| Workload | Knobs | Default | What it reads |
|---|---|---|---|
| `workload-file-read-one` | `READ_FILE` | `/testbed/.git/HEAD` (tiny; set a real file to measure a read) | one file |
| `workload-file-read` | `READ_ROOT`, `READ_FILES` | `/`, `200` | a sample of Python files |
| `workload-compile` | `PYTHON`, `COMPILE_DIR` | `python3`, `/testbed` | every Python file under the directory, compiled with the image's own Python |
| `workload-read-all` | `READ_ROOT` | `/` | every regular file, the worst case for lazy loading |

The reading steps print the bytes read, which the reward step's output tail
keeps in the events file, and fail when nothing was read or a read reported
an error, so a wrong path or a lazy-loading fault scores 0, not 1. `-trajectory workload-file-read-one,workload-compile,workload-read-all`
mixes them: each environment draws one, with `-seed` fixing the draw so every
rep gives the same environment the same workload; the task event's `name` is
the trajectory it replayed. In steps mode the first step's work runs after the
create and every later step's after a wake, which is the boot-then-resume
shape of the profiling scripts these workloads were ported from.

`images.txt` holds one reference per line, a tag or `repo@sha256:...`; tags
are resolved with `crane`. The instance id is the image name, the tag if one
was given, and the first 12 digits of the digest, so two pinned images of one
repository stay distinct. A line that resolves to a reference already
written is skipped. `--limit` applies; `--instances` does not.

Run the same command against each delivery mechanism and diff the reports;
the loader sees wall time per phase as a trainer would, and the split of a
cold start into pull, unpack and restore comes from Substrate's own metrics
over the run window, joined on the run id.

To know whether those first pulls were cold, sample the node image caches
before and after the run and hand both files to the report:

```bash
benchmarking/swebench/sample-node-cache.sh before.txt
benchmarking/swebench/run-in-cluster.sh pull-baseline images.jsonl -trajectory workload-read-all -images 200 -width 200
benchmarking/swebench/sample-node-cache.sh after.txt
swebench-loader report pull-baseline.jsonl -cache-before before.txt -cache-after after.txt
```

The sample reads each atelet's `ate_imagecache_requests_total` counters and
its process start time through the API server's pod proxy (atelet has no
shell), so it needs kubectl with RBAC `get` on `pods/proxy` in `ate-system`.
The report then adds a line with the run's misses, which are its cold pulls,
the nodes they landed on, hits, errors, and misses per first command: 1.0
means every environment pulled its image, 0 means every start was served
from a node's cache. The count is cluster-wide for the window, so it also
holds golden bakes and wakes that landed on a node without the image. A node
whose atelet restarted between the samples, and a node the autoscaler added
in between, count from zero; a node removed in between is left out; a node
present only in the after sample with an atelet older than the before sample
had a failed before scrape and is reported and left out. The report cannot
label one environment cold or warm, because the ate-env API does not say
which node an environment ran on.

To split the first-command window itself, harvest atelet's restore records
for the run and give them to the report:

```bash
benchmarking/swebench/harvest-restore-timings.sh pull-baseline restores.jsonl
swebench-loader report pull-baseline.jsonl -restore-log restores.jsonl
```

atelet logs one record per actor restore with the seconds each phase took:
manifest fetch, snapshot download, image unpack, the worker's restore, and
the total. The actor's name is the environment's name, so the report pairs
each record with the `ttfi` or `wake` event of that environment whose window
(the event's end minus its duration, up to its end) contains the record's
time: a record inside a `ttfi` window is a first start, one inside a `wake`
window is a wake. The table gives p50/p95/max per phase plus an `outside`
row, the event's span minus atelet's total, which is ateapi, the scheduler,
the router and the command itself. The report does not call a restore cold
or warm: atelet's `oci_unpack` phase times the bundle preparation, which runs
on every restore whether or not the node had the layers, so the record
carries no cache signal; the cache samples above give the run's misses until
atelet records the image cache outcome per restore. Failed restores are
counted and left out of the percentiles; restores whose event failed are
counted and left out of `outside`. Records before the run's first event or after its last (a rerun
with the same run id), for actors the run never created, or inside no window
(a restore after an eviction or a failed suspend) are counted, not paired.
The records come from the kubelet's container logs, which rotate, so harvest
right after the run; when atelet restarted, the previous container's log is
read too, one restart back.

## Network and the offline exclusion list

Actors have no network until an egress policy is created for them, which the
env SDK cannot do yet, and the SWE-bench harness assumes network (matplotlib's
eval scripts pip-install, requests and django have tests that open sockets).
`offline-exclusions.txt` lists the Verified instances that do not resolve
without egress, with the grader's verdict as the reason; pass it with
`-exclude-file` and every remaining instance should resolve. Regenerate it
from a full pass when the dataset, the images or the sandbox network change,
and review the diff: a new entry is either a regression or a new
network-dependent test, never silently accepted. Egress at scale is a
benchmark of its own.

## Two things the first runs showed

- Commands run as start, poll, read rather than through the SDK's streaming
  shell: the router's route timeout (five minutes by default) cuts a single
  streamed request, and SWE-bench test runs regularly exceed it.
- On a large pool a new actor rarely lands on a node that already has its
  image, so time to first instruction is the cold pull plus the golden bake
  (about 20 s) for nearly every task. Report cold and warm passes separately.

## Record with every run

Substrate and env commits, cluster shape (machine type, node count, disk,
sandbox class), bucket region, the dataset manifest's digest, and the loader
flags. Pass them as `run -label` values so they travel with the events file. The
loader records its own flags itself: mode, width, images, trajectory,
dataset, tasks per image or rollouts, steps and park, connections, base
template, exclude file, instances, and the create, step, wake-budget and
wake-backoff timeouts. The report alone is not reproducible without them.
