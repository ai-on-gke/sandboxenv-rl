# System invariants and concurrency pitfalls

Companion to [docs/architecture.md](../architecture.md) and
[docs/dev/code-layout.md](./code-layout.md).

This document catalogs concurrency boundaries, data-flow invariants, and known pitfalls across
`sandboxenv-rl`. Read this guide before modifying process execution, router networking, streaming
I/O, or client connection pooling.

## In-guest process lifecycle and signaling

### 1. PID 1 child subreaper re-exec avoids `cmd.Wait` race

Re-exec the process as a subreaper under PID 1 before starting the gRPC daemon.

- **Location**: `cmd/ate-env-guest/init_linux.go` (`runIfPID1`, `supervise`).
- **Why**: When running as the container init process (PID 1), `ate-env-guest` must reap reparented
  orphan processes to prevent PID exhaustion. However, calling `wait4(-1, ...)` in a daemon that
  also executes commands creates a race: the init reaper loop can harvest exit status for direct
  children before Go's `os/exec.Cmd.Wait()` runs in `guest/process.Tracker`, resulting in stolen
  exit codes and broken wait notifications.
- **Mechanism**: `ate-env-guest` calls Linux `prctl(PR_SET_CHILD_SUBREAPER, 1)` and re-executes
  itself via `os.StartProcess`. The outer PID 1 process executes only an orphan reaper loop
  (`syscall.Wait4(-1, &status, syscall.WNOHANG, nil)`) and signal forwarder. The supervised child
  process runs the gRPC server and tracks its own direct subprocesses through `exec.Cmd` without
  interference.

### 2. Process groups and negative-PID signal delivery

Start subprocesses in dedicated process groups and signal via negative PIDs (`-pid`).

- **Location**: `guest/process/tracker.go` (`Setpgid`), `guest/process/pty_linux.go` (`Setsid`),
  `guest/process/tracker.go` (`Tracker.Signal`), `cmd/ate-env-guest/sidecar.go`.
- **Why**: Commands launched in environments frequently spawn child worker trees (such as shell
  scripts executing Python or compilers). If the parent process is signaled or times out, signaling
  only the parent PID leaves child processes running.
- **Mechanism**: Non-PTY processes configure
  `cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}`. PTY allocations configure `Setsid: true`.
  When delivering signals via `(*Service).SignalProcess` in `guest/process/service.go` or when the
  process timeout timer fires in `guest/process.(*Tracker).Start`, `Tracker.Signal` executes
  `syscall.Kill(-pid, sig)`. The negative PID delivers the signal to the entire process group.

### 3. Spool cursor notification subscription before file read

Acquire `OutputChanged()` before reading from disk to avoid missed write notifications.

- **Location**: `guest/process/service.go` (`StreamProcessOutput`), `guest/process/tracker.go`.
- **Why**: `StreamProcessOutput` polls stdout and stderr spool files while a process executes. If a
  handler reads the file up to the current offset and then registers for the change notification
  channel (`ProcessState.OutputChanged()`), writes that occur between the read and registration are
  missed, stalling the stream until the next write or process exit.
- **Mechanism**: The handler acquires the `OutputChanged()` notification channel before reading from
  disk. If the read returns zero bytes and the process is still running, the handler waits on the
  acquired channel. When the writer flushes bytes to disk, it closes the notification channel under
  mutex, waking all waiting readers.

## Server networking and Substrate boundaries

### 4. Router `ate-target-actor` header deduplication and SPIFFE mTLS

Append `ate-target-actor` via interceptors and do not forward incoming caller metadata.

- **Location**: `internal/apiservice/server.go` (`guestConn`), `internal/ate/client.go`
  (`DialGuest`), `internal/routertls/routertls.go`.
- **Why**: When dialing an actor through `atenet`, the router requires the destination actor
  identifier in wire metadata (`ate-target-actor: <atespace>/<id>`). Forwarding incoming client gRPC
  metadata caused duplicate header entries, which `atenet-router` rejects with an invalid actor
  reference error (`agent-substrate/substrate#2308`).
- **Mechanism**: `guestConn` and `DialGuest` configure client interceptors that append
  `ate-target-actor` to outgoing context metadata without forwarding caller metadata headers.
- **mTLS**: `internal/routertls` loads the kubelet-projected pod identity certificate bundle
  (`-atenet-client-cert`) and servicedns trust bundle (`-atenet-ca-file`), reloading certificates on
  the fly during TLS handshakes when disk files change.

### 5. Actor wake budgets and direct actor deletion

Always verify actor readiness within wake budgets, and evict pooled guest connections on delete.

- **Location**: `internal/apiservice/wakeretry.go` (`ensureAwake`), `internal/ate/client.go`
  (`Delete`, `evictGuest`).
- **Wake budgets**: Sending traffic to a suspended actor causes `atenet` to trigger restoration.
  `ensureAwake` polls `GetProcess("ate-env-wake-probe")` across two budgets:
  - `DefaultWakeBudget` (2 minutes): bounds the entire wait from the start of the probe sequence.
  - `DefaultWakeRunningBudget` (20 seconds): bounds wait time starting from the first poll attempt
    that observes the actor in `RUNNING` state (giving the guest daemon time to initialize).
- **Delete sequence**: Substrate `DeleteActor` takes an actor in any state (including `CRASHED` or
  `RUNNING`), marks it `DELETING`, terminates the workload, releases the worker slot, and deletes
  snapshots. `internal/ate.Client.Delete` calls `DeleteActor` directly without suspending first
  (avoiding failed precondition errors on crashed actors) and calls `evictGuest` to close and forget
  the pooled `guestPool` connection.

### 6. Idempotent derived `ActorTemplate` creation and failed golden-snapshot recovery

Serialize derived template creation and recovery with `imageTemplateLocks`, and handle
`codes.AlreadyExists` as success.

- **Location**: `internal/apiservice/imagetemplate.go` (`DeriveImageTemplate`),
  `internal/apiservice/templaterecover.go` (`imageTemplateLocks`, `imageTemplateLock`,
  `recreateFailedImageTemplate`, `failedTemplateRecreateAfter`, `templateUsers`),
  `internal/apiservice/server.go` (`ensureImageTemplate`, `getOrCreateImageTemplate`).
- **Why**: Concurrent training workers race to create `<base>-<digest:12>` (where 12 is
  `apiservice.imageTemplateDigestLen`). In addition, Substrate does not retry a terminal golden
  snapshot build failure (`golden_snapshot_status.error_message != ""`), and deleting a template in
  use by suspended actors would strip their rootfs upon resume.
- **Mechanism**: `Server.ensureImageTemplate` acquires a per-template read lock (`imageTemplateLock`
  from `imageTemplateLocks`) held until `CreateActor` completes. `getOrCreateImageTemplate` checks
  `GetActorTemplate`, calls `CreateActorTemplate` if missing, treats `codes.AlreadyExists` as
  success, and returns `codes.FailedPrecondition` if the existing template runs a different image.
  If the template's golden snapshot failed at least `failedTemplateRecreateAfter` (10 minutes) ago,
  `ensureImageTemplate` upgrades to the write lock and calls `Server.recreateFailedImageTemplate`,
  which checks `templateUsers` and deletes/recreates the template by UID only when no actor uses it.

## Client connection pooling and event loops

### 7. Python `LoopRunner` thread bridging and HTTP/2 stream concurrency

Dispatch synchronous Python calls to a persistent `LoopRunner` background thread.

- **Location**: `clients/python/src/ate_env/loop_runner.py`, `cmd/ate-env-api/main.go`.
- **Why**: Calling async gRPC methods from synchronous Python code (such as PyTorch training loops)
  by invoking `asyncio.run()` creates a temporary event loop that destroys open gRPC channels upon
  exit, corrupting HTTP/2 channel state.
- **Mechanism**: `LoopRunner` runs a dedicated background daemon thread with a persistent event
  loop, dispatching calls via `asyncio.run_coroutine_threadsafe`.
- **Concurrency**: Go's default HTTP/2 server limits concurrent streams per connection to 250.
  `cmd/ate-env-api/main.go` sets `DefaultMaxConcurrentStreams = 4096`, and `clients/python`
  automatically pools gRPC connections (`ceil(max_concurrent / 200)`).

### 8. Python release error tracking and teardown retries

Record release errors in `RunReport` and retry unreleased actors during `fleet.teardown()`.

- **Location**: `clients/python/src/ate_env/backend/substrate.py`.
- **Why**: When releasing a sandbox in RL loops, network interruptions during `delete` must not
  crash training workers, but must not leak orphan actors in Substrate.
- **Mechanism**: `SubstrateBackendDriver` records failed deletions in `RunReport.release_errors` and
  retains the actor in `_owned_actors`. When `fleet.teardown()` runs, it retries deletion for all
  unreleased actors and raises `InfrastructureError` if any environment cannot be reclaimed.
