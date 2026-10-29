---
name: swebench-loader-simulated-run
description: Runs one simulated RL measurement with swebench-loader on a cluster with Agent Substrate and ate-env (cold boot, cold or warm restore, suspend and wake, burst, density), after checking the cluster conditions that silently change what a run measures.
---

# Simulated runs with swebench-loader

Use this skill when asked to measure startup, restore, wake or density on Substrate without a
model: the Fast, ML Bench, high-cardinality and density rows of the RL benchmark plan. For
hooking up a real RL recipe or the `bench` harness and its bundles, use `benchmarking/SKILL.md`.
For which run shape measures what, read "Cold boot, cold and warm restore, suspend and wake" in
`README.md` in this directory; this skill does not repeat it.

## 1. Check the cluster before the first run

Each check below changed a result this week without any error in the loader. Run them all; each
is one command.

1. **Templates declare limits.** `kubectl-ate get actor-template BASE -a ATESPACE -o yaml` must
   show `resources.limits` with cpu and memory. A base template with `limits: {}` (and every
   template derived from it) is admitted without any resource check, up to the worker's actor cap
   of 1000; slot arithmetic is then meaningless. Newer servers refuse such templates; older ones
   keep the ones they have. Create a new base template with `ate-env manifest template --cpu
   --memory` and run with `-base-template NAME`.
2. **Worker capacity is what you think.** The API log's GetWorker response, or `kubectl-ate get
   workers -o yaml`, shows each worker's `capacity`. With no limits on the WorkerPool template
   each worker registers the node's allocatable, so N workers on one node each claim the whole
   node. Set `spec.template.resources.limits` on the pool for a fixed slot count.
3. **Every template has a golden before a restore measurement.** Count templates against
   templates with a golden tag (`kubectl-ate get actor-templates -a ATESPACE`, third column).
   Goldens bake at 5 concurrent per ateapi replica, about two minutes each; thousands of new
   templates mean hours. An actor whose template has no golden boots from the image, silently.
   Wait for the counts to match, or scale `ate-api-server` replicas, or label the run a cold
   boot.
4. **atelet's image cache GC is ahead of kubelet's eviction.** `kubectl -n ate-system get ds
   atelet -o jsonpath='{.spec.template.spec.containers[0].args}'` should show
   `--image-cache-gc-period`, `--image-cache-high-percent` and `--image-cache-low-percent`. The
   defaults (5 min, 85, 80) start evicting at the same 85% where kubelet declares DiskPressure,
   and kubelet cannot free atelet's cache. For churn, 30s / 70 / 60. Budget node disk as images
   in flight per node times unpacked size (2 to 2.5 times compressed).
5. **One CPU family in the pool.** `gcloud compute instances list --format='value(name,cpuPlatform)'`
   over the worker nodes. A golden restores with memory only on the family it was baked on;
   elsewhere it relaxes to a rootfs restore, in effect a boot, with nothing in the events saying
   so. Label one family into the pool, or pin `--min-cpu-platform` on the node pools.
6. **Nodes can pull the pause image.** Private nodes without Cloud NAT cannot reach
   `registry.k8s.io`; every sandbox start fails with an i/o timeout in atelet's log and the
   golden bake never completes. Editing the SandboxConfig's `pauseImage` does not take effect on
   running builds; add a NAT scoped to the cluster's subnet.
7. **The worker selector matches something.** `ate-env manifest template --workerpool` writes a
   `workerSelector`; on clusters whose workers carry no labels it matches nothing and placement
   fails with "no worker has room". Compare with a working template; drop the selector if it has
   none.
8. **The manifest has one reference per image.** `make-dataset.py --images-file` drops exact
   digest duplicates. References that resolve to the same platform image under different index
   digests still collide in the API ("exists but runs <other reference>"); such creates are
   refused and counted as failed, not run.

## 2. Run

From the directory where results should land, never from inside the repository (the events
file must not dirty the tree ko builds from):

```bash
export KO_DOCKER_REPO=<registry the cluster pulls from>
benchmarking/swebench/sample-node-cache.sh before-RUN.txt
RESTORE_LOG=restores-RUN.jsonl benchmarking/swebench/run-in-cluster.sh RUN DATASET.jsonl <shape flags> -label arm=NAME
benchmarking/swebench/sample-node-cache.sh after-RUN.txt
swebench-loader report RUN.jsonl -restore-log restores-RUN.jsonl -cache-before before-RUN.txt -cache-after after-RUN.txt -unresolved unresolved-RUN.txt
```

- `RESTORE_LOG` harvests atelet's restore records every ten minutes during the run; kubelet
  rotates a busy atelet's log within the hour and one harvest at the end loses most of a long run.
- Do not chain the sampler with `&&`: it exits non-zero when any node scrape fails, and at
  hundreds of nodes one failed scrape is normal. Run the pieces one after another.
- A run in tmux survives a dropped connection; a run in a plain shell does not.
- One run id per arm, the same shape flags across arms, and `report -arm NAME=FILE` to compare;
  it refuses to pool runs whose shape labels differ.

Shape flags per measurement are in the README section named above. Two that are easy to get
wrong:

- Width above the slot count means admission queueing counted into `ttfi`; width equal to the
  slot count means none. Say which one the run is.
- Many cold restores per node at once measure the node's disk and CPU under contention, not the
  per-restore cost. 0.65 unpacks per 8-vCPU node gave 15.8 s median; 13 per node gave 50 s; 160
  per 16-vCPU node gave 243 s. Choose the per-node concurrency on purpose and state it.

## 3. Read the report

- `ttfi` is create start to first command answered; `retried` on that row counts first commands
  that waited on a bake or a wake.
- The `restore first` block (needs `RESTORE_LOG`) splits each first command: `download` is the
  golden from the bucket, `oci_unpack` is the image onto the node, `worker_restore` is gVisor,
  `outside` is everything atelet does not do (create, placement, router, guest readiness). No
  restore records at all means the actors booted; see check 3.
- The cache line, `misses per first command`, near 1.0 means every environment pulled its own
  image: placement is image-blind, and 16 rollouts of one image land on 13 to 16 nodes.
- `crashed while waking` on a boot is an image that does not start under the guest; on a restore
  it is a golden gVisor could not bring back. Both are deterministic per image; exclude and file.

## 4. Record with every run

The README's "Record with every run" list, plus: node machine type and CPU family, boot disk
type and size, workers per node, declared actor limits, ateapi replicas, atelet GC flags, and
whether the templates had goldens at run start. Each of these moved a number this week.
