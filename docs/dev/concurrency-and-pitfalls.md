# System invariants and concurrency pitfalls

Companion to [docs/architecture.md](../architecture.md) and
[docs/dev/code-layout.md](./code-layout.md). Catalogs concurrency invariants across `sandboxenv-rl`.

## In-guest process lifecycle and signaling

### 1. PID 1 child subreaper re-exec avoids `cmd.Wait` race

Re-exec the process as a subreaper under PID 1 before starting the gRPC daemon.

- **Location**: `cmd/ate-env-guest/init_linux.go` (`runIfPID1`, `supervise`).
- **Why**: Direct `wait4(-1, ...)` calls inside the gRPC daemon race with `os/exec.Cmd.Wait()` in
  `guest/process.Tracker`, stealing exit codes.
- **Mechanism**: `ate-env-guest` sets `prctl(PR_SET_CHILD_SUBREAPER, 1)` and re-execs via
  `os.StartProcess`. The outer PID 1 process runs `syscall.Wait4(-1, &status, syscall.WNOHANG, nil)`
  and forwards signals while the child runs `exec.Cmd`.

### 2. Process groups and negative-PID signal delivery

Start subprocesses in dedicated process groups and signal via negative PIDs (`-pid`).

- **Location**: `guest/process/tracker.go` (`Setpgid`, `Tracker.Signal`, `Tracker.Start`),
  `guest/process/pty_linux.go` (`Setsid`), `cmd/ate-env-guest/sidecar.go`.
- **Why**: Signaling only the parent PID leaves child worker trees running on cancellation.
- **Mechanism**: Non-PTY processes set `Setpgid: true` and PTYs set `Setsid: true`.
  `(*Service).SignalProcess` and process timeout timers call `Tracker.Signal`, which runs
  `syscall.Kill(-pid, sig)` to signal the entire process group.

### 3. Spool cursor notification subscription before file read

Acquire `OutputChanged()` before reading from disk to avoid missed write notifications.

- **Location**: `guest/process/service.go` (`StreamProcessOutput`), `guest/process/tracker.go`.
- **Why**: Subscribing to `ProcessState.OutputChanged()` after reading misses concurrent disk
  writes, stalling the stream until the next write or exit.
- **Mechanism**: `StreamProcessOutput` acquires `OutputChanged()` before reading. If zero bytes are
  read while running, it waits on that channel; writers close the channel under mutex after
  flushing.

## Server networking and Substrate boundaries

### 4. Router `ate-target-actor` header deduplication and SPIFFE mTLS

Append `ate-target-actor` via interceptors and do not forward incoming caller metadata.

- **Location**: `internal/apiservice/server.go` (`guestConn`, `routerCredentials`),
  `internal/apiservice/wakeretry.go` (`ensureAwake`), `internal/ate/client.go` (`DialGuest`),
  `internal/routertls/routertls.go` (`TransportCredentials`, `RejectedCertificate`).
- **Why**: Forwarding caller gRPC metadata duplicates `ate-target-actor: <atespace>/<id>`, which
  `atenet-router` rejects (`agent-substrate/substrate#2308`).
- **Mechanism**: `guestConn` and `DialGuest` append `ate-target-actor` without forwarding caller
  metadata. `internal/routertls` hot-reloads kubelet-projected `-atenet-client-cert` and
  `servicedns` `-atenet-ca-file` bundles on handshakes. `routertls.TransportCredentials`
  (`alertConn.Write`) reads pending post-handshake TLS 1.3 alerts on initial HTTP/2 write failures
  (`EPIPE`), letting `ensureAwake` match `routertls.RejectedCertificate` on attempt 1 while
  `DialGuest` callers receive the TLS alert error.

### 5. Actor wake budgets, fail-fast start errors, and actor deletion

Verify readiness within wake budgets, fail fast on unrecoverable errors, and evict on delete.

- **Location**: `internal/apiservice/wakeretry.go` (`ensureAwake`, `StartFailureKey`),
  `internal/ate/client.go` (`Delete`, `evictGuest`).
- **Wake budgets**: Traffic to a suspended actor triggers restoration in `atenet`. `ensureAwake`
  polls `GetProcess("ate-env-wake-probe")` under `DefaultWakeBudget` (5 minutes overall) and
  `DefaultWakeRunningBudget` (20 seconds once `RUNNING`).
- **Fail-fast start errors**: `ensureAwake` exits early on unrecoverable states:
  `routertls.RejectedCertificate` or `CRASHED` returns `codes.FailedPrecondition` (with
  `ActorStatus.Crash.Message` and trailer `ate-env-start-failure: crashed`), `DELETING` returns
  `codes.NotFound`, and `"no worker has room for the actor"` persisting across `WakeNoCapacityGrace`
  while not `RUNNING` (default `0` waits `WakeBudget`) returns `codes.ResourceExhausted`
  (`ate-env-start-failure: no-capacity`).
- **Delete sequence**: `internal/ate.Client.Delete` calls `DeleteActor` directly in any state
  including `CRASHED` (terminating workloads, releasing worker slots, and deleting snapshots without
  pre-suspending) and calls `evictGuest` to close and drop the pooled `guestPool` connection.

### 6. Idempotent derived `ActorTemplate` creation and failed golden-snapshot recovery

Serialize derived template creation and recovery with `imageTemplateLocks`, and treat
`codes.AlreadyExists` as success.

- **Location**: `internal/apiservice/imagetemplate.go` (`DeriveImageTemplate`),
  `internal/apiservice/templaterecover.go` (`imageTemplateLocks`, `imageTemplateLock`,
  `recreateFailedImageTemplate`, `failedTemplateRecreateAfter`, `templateUsers`, `keptUID`),
  `internal/apiservice/server.go` (`ensureImageTemplate`, `getOrCreateImageTemplate`,
  `sameImageDigest`).
- **Why**: Concurrent workers race to create `<base>-<digest:12>` (`imageTemplateDigestLen = 12`),
  Substrate leaves terminal `golden_snapshot_status.error_message` failures unretried, and deleting
  an in-use template strips rootfs volumes upon resume.
- **Mechanism**: `ensureImageTemplate` holds a per-template read lock (`imageTemplateLocks`) through
  `CreateActor`. `getOrCreateImageTemplate` reuses an existing template as is when `sameImageDigest`
  matches across repository paths, returns `codes.FailedPrecondition` on mismatch, and treats
  `codes.AlreadyExists` on `CreateActorTemplate` as success. If a golden snapshot failed at least
  `failedTemplateRecreateAfter` (10 minutes) ago, `ensureImageTemplate` upgrades to the write lock
  and calls `recreateFailedImageTemplate`, which verifies zero `templateUsers` and deletes/recreates
  by UID.

## Client connection pooling and event loops

### 7. Python `LoopRunner` thread bridging and HTTP/2 stream concurrency

Dispatch synchronous Python calls to a persistent `LoopRunner` background thread.

- **Location**: `clients/python/src/ate_env_rl/loop_runner.py`,
  `clients/python/src/ate_env_rl/fleet.py`, `cmd/ate-env-api/main.go`.
- **Why**: Calling async gRPC methods via `asyncio.run()` creates a short-lived loop that destroys
  open gRPC channels on exit and corrupts HTTP/2 channel state.
- **Mechanism**: `LoopRunner` runs a daemon thread with a persistent event loop, dispatching
  coroutines via `asyncio.run_coroutine_threadsafe` over pooled gRPC channels
  (`ceil(max_concurrent / 200)`) against `DefaultMaxConcurrentStreams = 4096` (vs. 250 default).

### 8. Python release error tracking and teardown retries

Record release errors in `RunReport` and retry unreleased actors during `fleet.teardown()`.

- **Location**: `clients/python/src/ate_env_rl/backend/substrate.py`.
- **Why**: Network interruptions during `delete` must not crash training workers, yet orphan actors
  must not leak in Substrate.
- **Mechanism**: `SubstrateBackendDriver` records failed deletions in `RunReport.release_errors` and
  keeps the actor in `_owned_actors`; `fleet.teardown()` retries all unreleased actors and raises
  `InfrastructureError` if any cannot be reclaimed.

### 9. Python warm-pool bookkeeping across awaits

Re-check teardown state after each await before pooling an actor, and reserve warm slots upfront.

- **Location**: `clients/python/src/ate_env_rl/backend/substrate.py` (`_release_internal`,
  `_warm_pool_internal`, `_acquire_internal`, `_reap_internal`, `_return_popped`, `_may_pool`),
  `clients/python/tests/test_warm_pool_accounting.py`.
- **Why**: Recycles pop actors from `_owned_actors` while awaiting `suspend()`; pooling after a
  concurrent `reap` leaks live actors past teardown with the warm-replica gauge reset, and
  overlapping `warm_pool` calls reading an unreserved shortfall over-provision.
- **Teardown & claim checks**: `reap` increments `_teardowns` on entry and exit, and `_reaping`
  counts active reaps. Recycles, `warm_pool`, and warm claims read `_teardowns` before awaiting and
  pool only when unchanged with `_reaping == 0` (`_may_pool`); otherwise they delete the actor. A
  warm claim whose `get()` succeeds checks `_may_pool` and takes ownership in the same lock hold; if
  stale, it deletes the actor if still owned (a reap of another run, or one cancelled before its
  delete, left it alive) and claims or creates another.
- **Ownership & generations**: Actors stay in `_owned_actors` until deletion succeeds. A cold
  `acquire` whose create overlaps its own run's reap owns the actor under `WARM_POOL_RUN_ID` too;
  its handle keeps the run id. Failed or cancelled deletes and recycles cancelled in `suspend()`
  restore ownership via `_return_popped` in one lock hold (under `WARM_POOL_RUN_ID` if a reap ran
  since the pop, else the actor's run id). Torn-down or cancelled `warm_pool` creates (deleted in
  background tasks if a reap ran since planning) and warm actors whose `suspend()` failed are
  likewise owned under `WARM_POOL_RUN_ID` so any run's `reap` deletes them. `_warm_inflight` tracks
  in-flight creations per pool key, `_teardowns`, and generation (`_pool_gens`), subtracting them
  from shortfalls; `unwarm_pool` and shrinking `resize_pool` calls delete drained actors and bump
  `_pool_gens`. `reap` updates the warm-replica gauge alongside the cleared pool, and
  `tests/test_warm_pool_accounting.py` exercises these interleavings with a Hypothesis state
  machine.
