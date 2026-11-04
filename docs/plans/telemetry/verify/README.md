# Dafny models

Hand-written [Dafny](https://dafny.org) models of the telemetry code's pure functions and of
`SubstrateBackendDriver`'s warm-pool bookkeeping. They back the properties in
[../properties.md](../properties.md). CI's `Dafny models` job runs them (`make ci-dafny` locally)
and checks the results against this file. Nothing ties them to the Python code: a change to
`substrate.py` or `observability.py` does not fail a model.

## Models

| File                        | Models                                                                                                                                                                                                                                                           | Expected result       |
| --------------------------- | ---------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- | --------------------- |
| `telemetry.dfy`             | `repo_family`'s reserved-name rename and `_bounded_family` (P-1 to P-3), `inject_metadata` (P-4 to P-7), `RunReport.add_phase` with the negative-duration clamp (P-8)                                                                                            | 14 verified, 0 errors |
| `warm_pool.dfy`             | Warm-pool bookkeeping with one teardown that ends the driver. Module `WarmPool` is the design #221 ships; module `WarmPoolBefore` is the code before #221 (P-9 to P-11)                                                                                          | 16 verified, 3 errors |
| `warm_pool_generations.dfy` | Teardowns that bump a generation (`_teardowns`) and do not end the driver, a warm claim in two steps, ghost current and stale reservations, and the generation each pooled actor entered in                                                                      | 24 verified, 3 errors |
| `warm_pool_drain.dfy`       | #221's third review round. Module `WarmPoolDrain`: `unwarm_pool` and a shrinking `resize_pool` that start a pool generation (`_pool_gens`), P-9, P-11 and DRAIN. Module `WarmPoolRunIds`: run ids, `reap(run)`, failed and cancelled deletes, `release` (ORPHAN) | 51 verified, 5 errors |

Each piece of code between two `await`s is one atomic action on a shared state. Proving that every
action keeps the invariant proves it for every interleaving of any number of concurrent calls.

### Lemmas that fail by design

| File                        | Lemma                   | Why it fails                                                                                                                                                                                       |
| --------------------------- | ----------------------- | -------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `warm_pool.dfy`             | `RecycleFinishNowSafe`  | Before #221 a recycle that awaited `suspend()` across a reap pools its actor after the teardown (P-10). One error.                                                                                 |
| `warm_pool.dfy`             | `WarmPlanNowSafe`       | Before #221 `warm_pool` ignores reservations of overlapping calls (P-11). Two errors.                                                                                                              |
| `warm_pool_generations.dfy` | `PlanFirstCovers`       | Bug A in #221's first version: reap kept the reservation counter, so a `warm_pool` started after the teardown counted it and returned short.                                                       |
| `warm_pool_generations.dfy` | `ClaimFailPutBackSafe`  | Bug B in #221's first version: a failed warm claim put its actor back in the pool after a teardown.                                                                                                |
| `warm_pool_generations.dfy` | `ClaimOkReturnsLive`    | Open after #221: a warm claim whose `get()` succeeds after an overlapping reap deleted the actor hands the caller a deleted environment.                                                           |
| `warm_pool_drain.dfy`       | `CommitStaleBeforeSafe` | Before the third round, a `warm_pool` call planned before an `unwarm_pool` pooled its actors afterwards (DRAIN).                                                                                   |
| `warm_pool_drain.dfy`       | `ResizeBeforeSafe`      | Before the third round, a `resize_pool` below pooled plus in flight left the calls in flight free to fill the pool past the new size (P-11).                                                       |
| `warm_pool_drain.dfy`       | `DeleteFailBeforeSafe`  | Before the third round, a delete that failed after the run's reap kept the reaped run id (ORPHAN).                                                                                                 |
| `warm_pool_drain.dfy`       | `DeleteCancelledSafe`   | Before #238 (AC-15.9): a cleanup delete (`_delete_unpooled`) cancelled after the run's reap left the actor under the reaped run id (ORPHAN).                                                       |
| `warm_pool_drain.dfy`       | `ReleaseDeleteFailSafe` | Before #238 (AC-15.9): a `release` whose delete failed or was cancelled while a reap ran put the actor back under the reaped run id, unless it was a recycle that saw the teardown first (ORPHAN). |

In `warm_pool_generations.dfy`, `PlanFixedSafe` (including "after a plan, the pool plus the current
generation's reservations covers `R`"), `CommitCurrentSafe`, `CommitStaleFixedSafe`,
`ReapFixedSafe`, `ClaimStartSafe` and `ClaimFailFixedSafe` verify the #221 design. `BugAWitness` and
`BugBWitness` verify concrete traces of bugs A and B.

In `warm_pool_drain.dfy`, `PlanDrainSafe`, `CommitCurrentDrainSafe`, `CommitStaleDrainSafe`,
`UnwarmFixedSafe` and `ResizeFixedSafe` verify the generations #221 ships; `ReapSafe`,
`WarmCommitDrainedSafe`, `StartDeleteSafe`, `DeleteOkSafe`, `DeleteFailFixedSafe`,
`ReleaseStartSafe`, `ReleaseDeleteOkSafe` and `AnyReapDeletesRetagged` verify the re-tag.
`DeleteCancelledFixedSafe` and `ReleaseDeleteFailFixedSafe` verify #238's fix for AC-15.9: a
cancelled delete re-tags, and a release re-tags whenever `_teardowns` changed since it popped the
actor. `UnwarmRefillWitness`, `ResizeOverfillWitness` and `ReleaseOrphanWitness` verify concrete
traces.

## Run

```bash
dafny verify telemetry.dfy                # 14 verified, 0 errors
dafny verify warm_pool.dfy                # 16 verified, 3 errors, all in WarmPoolBefore
dafny verify warm_pool_generations.dfy    # 24 verified, 3 errors: the three lemmas above
dafny verify warm_pool_drain.dfy          # 51 verified, 5 errors: the five lemmas above
```

## CI

[hack/verify/dafny.py](../../../../hack/verify/dafny.py) runs `dafny verify` on every `.dfy` file
here and fails unless each file is in the Run block above, its verified and error counts match that
block, and its errors fall in exactly the lemmas the "Lemmas that fail by design" table lists for
it. So a change that makes a listed lemma verify (a fix), or breaks one that verified, has to update
this file in the same pull request. CI runs it in the `.#dafny` devShell, whose Dafny comes from the
pinned nixpkgs; `make ci-dafny` does the same locally, and `hack/verify/dafny.py --dafny <path>`
uses any other Dafny binary.

Checked with Dafny 4.11.0, the version the pinned nixpkgs packages. The GitHub release zip for
Ubuntu is self-contained and runs without a separate .NET install; nixpkgs also packages `dafny`.
Dafny is not a dependency of the repository.

To see a finding come back, revert its amendment in `telemetry.dfy` and run `dafny verify` again:

- In `BoundedFamily`, replace `var name := Unreserve(candidate);` with `var name := candidate;`.
  `BoundedFamily` then fails (two errors), with a repo named `other` as the counter-example (P-3).
- In `AddPhase`, replace the clamp with `var d := dur;`. The P-8 proof then fails on a negative
  duration.

## Limits

- The models are hand-written from reading the code. A code change can make them stale without any
  check failing.
- `telemetry.dfy` models the rename of the bare reserved names only. `main` escapes `other` or
  `unknown` followed by any number of `_repo` (`other_repo` becomes `other_repo_repo`), which keeps
  the mapping one-to-one; the model does not cover that step. It models parseable inputs only:
  `LabelStream` proves `MAX_FAMILIES + 1` labels, and `unknown` adds the second value in P-1.
- `telemetry.dfy` is sequential. `_bounded_family`'s double-checked locking is not covered. Dafny
  `real` is exact; Python `float` is not.
- The warm-pool models cover one pool key and one event loop, interleaving only at `await`s. Calls
  from two threads or two loops are outside them.
- `warm_pool.dfy` makes a teardown permanent (`closed`). Its invariant `closed ==> pending == 0` and
  `Reap`'s `pending := 0` say a reap drops reservations, so modelling #221's first version (reap
  keeps them) against it fails at that clause. It cannot express bug A's symptom (a `warm_pool`
  after the teardown) or bug B (its `Acquire` is one step with no await or put-back).
  `warm_pool_generations.dfy` was written after review found both.
- `Reap` is one atomic step in every warm-pool model. So none covers work that runs while `reap`
  waits on its deletes (`_reaping`, from the second review of #221), nor a `warm_pool` cancelled
  during its `create()` calls, which can leak the actors already created. `warm_pool_drain.dfy`
  models one kind of cancellation: a delete in flight that ends without deleting.
- `WarmPoolDrain` folds `_teardowns` and the pool generation into one counter and has no reap or
  claim; those are in `warm_pool_generations.dfy`. `WarmPoolRunIds` has no pool, and a reap's own
  failed deletes are outside it (the code drops those actors and raises `InfrastructureError` with
  their ids). The gauge update inside `reap` is covered by the plain tests only.
