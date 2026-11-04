// Copyright 2026 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// Models of the third review round of #221, which warm_pool.dfy and
// warm_pool_generations.dfy do not cover:
//
//   G. unwarm_pool, and a resize_pool below what is pooled plus in flight,
//      start a new pool generation (_pool_gens), so warm_pool calls still
//      creating actors delete them instead of pooling them.  Module
//      WarmPoolDrain.
//   E. A delete that fails after the run's reap already ran re-tags the actor
//      with WARM_POOL_RUN_ID, which every reap deletes.  Module
//      WarmPoolRunIds, which also has AC-15.9 (a cancelled delete, and a
//      release whose delete fails or is cancelled after a reap ran, re-tag
//      too) and AC-15.8 (a warm_pool cancelled during its creates owns what
//      it made under WARM_POOL_RUN_ID).
//
// As in the other models, each action is the code between two awaits of
// clients/python/src/ate_env_rl/backend/substrate.py, for one pool key and one
// event loop. Ghost fields record facts the code does not store.

// ============================================================== G: drains
//
// `era` stands for the pair (_teardowns, _pool_gens[key]): a warm_pool call
// pools its actors only if neither changed since it planned, and its
// reservation lives under that pair, so one counter models both. Teardown is
// in warm_pool_generations.dfy; here an era ends by unwarm_pool or by a
// resize_pool that cancels.
//
//   P-9    gauge == |pool|
//   P-11   pool actors warm_pool created + current reservations <= R, where R
//          is the size last asked for (resize_pool changes it)
//   DRAIN  every pool actor warm_pool created comes from a call planned after
//          the last unwarm_pool: an unwarm is not undone by calls in flight
module WarmPoolDrain {

  type Actor = nat

  datatype State = State(
    live: set<Actor>,          // environments that exist on Substrate
    owned: set<Actor>,         // driver._owned_actors
    pool: set<Actor>,          // driver._warm_paused_pool (one key)
    gauge: int,                // observer warm-replica share
    era: nat,                  // (_teardowns, _pool_gens[key])
    reserved: nat,             // what the code subtracts: _warm_inflight lookup
    ghost cur: nat,            // reservations held by calls planned in `era`
    ghost stale: nat,          // reservations held by calls from older eras
    ghost epoch: map<Actor, nat>, // era in which the call that pooled the actor planned
    ghost fromWarm: set<Actor>,   // pool actors warm_pool created
    ghost unwarmedAt: nat,        // era the last unwarm_pool started
    R: nat
  )

  ghost predicate Inv(s: State) {
    && s.gauge == |s.pool|                                            // P-9
    && s.pool <= s.owned && s.owned <= s.live
    && s.fromWarm <= s.pool
    && s.unwarmedAt <= s.era
    && (forall a :: a in s.fromWarm ==>
          a in s.epoch && s.unwarmedAt <= s.epoch[a] <= s.era)      // DRAIN
    && |s.fromWarm| + s.cur <= s.R                                    // P-11
  }

  // The code #221 ships: the reservation key carries the pool generation.
  ghost predicate Fixed(s: State) { s.reserved == s.cur }

  lemma CardSub(a: set<Actor>, b: set<Actor>)
    requires a <= b
    ensures |a| <= |b|
  {
    if a != {} { var x :| x in a; CardSub(a - {x}, b - {x}); }
  }

  // ------------------------------------------------------------ warm_pool
  // Part 1, under the lock: needed = R - |pool| - reserved; reserve it.
  function Plan(s: State): State {
    if s.R <= |s.pool| + s.reserved then s
    else
      var needed := s.R - |s.pool| - s.reserved;
      s.(reserved := s.reserved + needed, cur := s.cur + needed)
  }

  // Part 2 for a call planned in the current era: pool the fresh actor.
  function CommitCurrent(s: State, a: Actor): State
    requires s.cur > 0 && s.reserved > 0 && a !in s.live
  {
    s.(reserved := s.reserved - 1, cur := s.cur - 1,
       live := s.live + {a}, owned := s.owned + {a}, pool := s.pool + {a},
       gauge := s.gauge + 1, fromWarm := s.fromWarm + {a},
       epoch := s.epoch[a := s.era])
  }

  // Part 2 for a call planned in an older era: `drained` is true, so it
  // deletes what it created (create + delete; a failed delete is module E).
  function CommitStaleFixed(s: State): State
    requires s.stale > 0
  { s.(stale := s.stale - 1) }

  // ------------------------------------------------------------ unwarm_pool
  // Pop the pool, bump the generation (_cancel_warm_calls), delete the pool.
  function UnwarmFixed(s: State): State {
    s.(live := s.live - s.pool, owned := s.owned - s.pool, pool := {}, gauge := 0,
       fromWarm := {}, era := s.era + 1, unwarmedAt := s.era + 1,
       reserved := 0, stale := s.stale + s.cur, cur := 0)
  }

  // ------------------------------------------------------------ resize_pool
  // Shrink part, under the lock. `keep` is pool[:n]: the whole pool when
  // n >= |pool|, else n of its actors. A target below pooled plus in flight
  // cancels the calls in flight. Growing then calls warm_pool (Plan) with
  // R = n.
  ghost predicate KeepOk(s: State, n: nat, keep: set<Actor>) {
    if n >= |s.pool| then keep == s.pool else keep <= s.pool && |keep| == n
  }

  function ResizeFixed(s: State, n: nat, keep: set<Actor>): State {
    var cancel := n < |s.pool| + s.reserved;
    var s1 := if cancel then s.(era := s.era + 1, reserved := 0,
                                stale := s.stale + s.cur, cur := 0)
              else s;
    s1.(live := s.live - (s.pool - keep), owned := s.owned - (s.pool - keep),
        pool := keep, gauge := |keep|, fromWarm := s.fromWarm * keep, R := n)
  }

  // ------------------------------------------------- before the third round
  // unwarm_pool and resize_pool left warm_pool calls in flight alone: the
  // reservation key had no generation and a commit checked only _teardowns.
  // `era`, `cur`, `stale` and `unwarmedAt` are kept as ghosts so the
  // properties can still be stated.
  function UnwarmBefore(s: State): State {
    s.(live := s.live - s.pool, owned := s.owned - s.pool, pool := {}, gauge := 0,
       fromWarm := {}, era := s.era + 1, unwarmedAt := s.era + 1,
       stale := s.stale + s.cur, cur := 0)                        // reserved kept
  }

  // A call planned in era `e`, before the last unwarm, pools its actor.
  function CommitStaleBefore(s: State, a: Actor, e: nat): State
    requires s.stale > 0 && s.reserved > 0 && a !in s.live
  {
    s.(reserved := s.reserved - 1, stale := s.stale - 1,
       live := s.live + {a}, owned := s.owned + {a}, pool := s.pool + {a},
       gauge := s.gauge + 1, fromWarm := s.fromWarm + {a},
       epoch := s.epoch[a := e])
  }

  function ResizeBefore(s: State, n: nat, keep: set<Actor>): State {
    s.(live := s.live - (s.pool - keep), owned := s.owned - (s.pool - keep),
       pool := keep, gauge := |keep|, fromWarm := s.fromWarm * keep, R := n)
  }

  // ======================================================= #221 verifies
  lemma PlanDrainSafe(s: State)
    requires Inv(s) && Fixed(s)
    ensures Inv(Plan(s)) && Fixed(Plan(s))
    ensures |Plan(s).pool| + Plan(s).cur >= s.R                       // PLAN
  { CardSub(s.fromWarm, s.pool); }

  lemma CommitCurrentDrainSafe(s: State, a: Actor)
    requires Inv(s) && Fixed(s) && s.cur > 0 && a !in s.live
    ensures Inv(CommitCurrent(s, a)) && Fixed(CommitCurrent(s, a))
  {}

  lemma CommitStaleDrainSafe(s: State)
    requires Inv(s) && Fixed(s) && s.stale > 0
    ensures Inv(CommitStaleFixed(s)) && Fixed(CommitStaleFixed(s))
  {}

  lemma UnwarmFixedSafe(s: State)
    requires Inv(s) && Fixed(s)
    ensures Inv(UnwarmFixed(s)) && Fixed(UnwarmFixed(s))
    ensures UnwarmFixed(s).pool == {}
  {}

  lemma ResizeFixedSafe(s: State, n: nat, keep: set<Actor>)
    requires Inv(s) && Fixed(s) && KeepOk(s, n, keep)
    ensures Inv(ResizeFixed(s, n, keep)) && Fixed(ResizeFixed(s, n, keep))
    ensures |ResizeFixed(s, n, keep).pool| <= n
  {
    CardSub(s.fromWarm * keep, keep);
    CardSub(s.fromWarm, s.pool);
  }

  // ==================================== before the third round: these FAIL
  // A call planned before an unwarm_pool pools its actor afterwards.
  lemma CommitStaleBeforeSafe(s: State, a: Actor, e: nat)
    requires Inv(s) && s.stale > 0 && s.reserved > 0 && a !in s.live
    requires e < s.unwarmedAt
    ensures Inv(CommitStaleBefore(s, a, e))                           // fails
  {}

  // A resize_pool below pooled plus in flight leaves the calls in flight
  // free to fill the pool past the new size.
  lemma ResizeBeforeSafe(s: State, n: nat, keep: set<Actor>)
    requires Inv(s) && KeepOk(s, n, keep)
    ensures Inv(ResizeBefore(s, n, keep))                             // fails
  {}

  // ============================== concrete witnesses (these verify)
  // warm_pool(3) paused in create(); unwarm_pool; the 3 creates return and
  // the pool holds 3 actors after the unwarm.
  lemma UnwarmRefillWitness() {
    var s0 := State({}, {}, {}, 0, 0, 0, 0, 0, map[], {}, 0, 3);
    var s1 := Plan(s0);
    assert s1.reserved == 3 && s1.cur == 3;
    var s2 := UnwarmBefore(s1);
    assert s2.pool == {} && s2.stale == 3 && s2.reserved == 3;
    var s3 := CommitStaleBefore(s2, 10, 0);
    var s4 := CommitStaleBefore(s3, 11, 0);
    var s5 := CommitStaleBefore(s4, 12, 0);
    assert s5.pool == {10, 11, 12};
    assert !Inv(s5);
  }

  // warm_pool(3) paused in create(); resize_pool(1); the 3 creates return:
  // the pool holds 3 after a resize to 1.
  lemma ResizeOverfillWitness() {
    var s0 := State({}, {}, {}, 0, 0, 0, 0, 0, map[], {}, 0, 3);
    var s1 := Plan(s0);
    var s2 := ResizeBefore(s1, 1, {});
    assert s2.R == 1 && s2.cur == 3;
    assert !Inv(s2);
    var s3 := CommitCurrent(s2, 10);
    var s4 := CommitCurrent(s3, 11);
    var s5 := CommitCurrent(s4, 12);
    assert s5.pool == {10, 11, 12} && s5.R == 1;
  }
}

// ============================================================== E: run ids
//
// An owned actor is reaped by `reap(run)` when its run id is `run` or
// WARM_POOL_RUN_ID. A fleet reaps its run once, at teardown, so an actor
// left owned under a run that was already reaped is never deleted.
//
//   ORPHAN   every owned actor that no delete is pending for carries a run id
//            that some later reap still matches: WARM_POOL_RUN_ID, or a run
//            not reaped yet
//   TRACKED  every live actor is owned or being released: none is alive with
//            no reference in the driver (AC-15.8)
//
// A reap's own failed deletes are out of scope: the code drops those actors
// and raises InfrastructureError with their ids.
module WarmPoolRunIds {

  type Actor = nat
  type Run = nat

  const WARM: Run := 0   // WARM_POOL_RUN_ID; FleetConfig rejects it as a run id

  datatype State = State(
    live: set<Actor>,
    owned: map<Actor, Run>,       // driver._owned_actors and each one's run_id
    deleting: set<Actor>,         // owned, a _delete_unpooled in flight
    releasing: map<Actor, (Run, nat)>, // popped by release, delete in flight:
                                       // run id and _teardowns at the pop
    td: nat,                      // driver._teardowns
    ghost reaped: set<Run>        // runs whose reap has run
  )

  ghost predicate Inv(s: State) {
    // No `owned.Keys <= live`: a cancelled warm_pool owns ids whose create()
    // may never have landed; a reap or release finds those NotFound, which
    // counts as gone.
    && s.live <= s.owned.Keys + s.releasing.Keys                      // TRACKED
    && s.deleting <= s.owned.Keys
    && s.owned.Keys !! s.releasing.Keys
    && WARM !in s.reaped
    && (forall a :: a in s.owned && a !in s.deleting ==> s.owned[a] !in s.reaped)  // ORPHAN
    && (forall a :: a in s.releasing ==> s.releasing[a].1 <= s.td)
    // A release that started after the run's reap is a caller bug (the
    // fleet is closed); it is outside the model.
    && (forall a :: a in s.releasing && s.releasing[a].1 == s.td ==> s.releasing[a].0 !in s.reaped)
  }

  // reap(r) of a run other than WARM: delete every owned actor of r or WARM.
  function Reap(s: State, r: Run): State
    requires r != WARM
  {
    var gone := set a | a in s.owned && (s.owned[a] == r || s.owned[a] == WARM);
    s.(owned := map a | a in s.owned && a !in gone :: s.owned[a],
       live := s.live - gone, deleting := s.deleting - gone,
       td := s.td + 1, reaped := s.reaped + {r})
  }

  // warm_pool, after a teardown or drain: the fresh actor is owned, and its
  // delete may start next (StartDelete) or never, if a cancel lands first.
  // AC-15.9: owned under WARM from the start.
  function WarmCommitDrained(s: State, a: Actor): State
    requires a !in s.live && a !in s.releasing
  { s.(live := s.live + {a}, owned := s.owned[a := WARM]) }

  // Before AC-15.9's fix: owned under the call's run id, which may already
  // be reaped; only a delete that started and failed re-tagged it.
  function WarmCommitDrainedBefore(s: State, a: Actor, r: Run): State
    requires a !in s.live && a !in s.releasing
  { s.(live := s.live + {a}, owned := s.owned[a := r]) }

  // A failed warm claim or a drained pool actor: an owned actor whose
  // _delete_unpooled starts.
  function StartDelete(s: State, a: Actor): State
    requires a in s.owned
  { s.(deleting := s.deleting + {a}) }

  function DeleteOk(s: State, a: Actor): State
    requires a in s.deleting
  {
    s.(owned := map b | b in s.owned && b != a :: s.owned[b],
       live := s.live - {a}, deleting := s.deleting - {a})
  }

  // #221: a failed delete re-tags the actor (_keep_for_any_reap).
  function DeleteFailFixed(s: State, a: Actor): State
    requires a in s.deleting
  { s.(owned := s.owned[a := WARM], deleting := s.deleting - {a}) }

  // Before the third round: the actor stayed owned under its run id.
  function DeleteFailBefore(s: State, a: Actor): State
    requires a in s.deleting
  { s.(deleting := s.deleting - {a}) }

  // Before AC-15.9's fix: a cancelled _delete_unpooled left the actor owned
  // as it was.
  function DeleteCancelled(s: State, a: Actor): State
    requires a in s.deleting
  { s.(deleting := s.deleting - {a}) }

  // AC-15.9: a cancelled _delete_unpooled re-tags the actor, as a failed one does.
  function DeleteCancelledFixed(s: State, a: Actor): State
    requires a in s.deleting
  { s.(owned := s.owned[a := WARM], deleting := s.deleting - {a}) }

  // release: pop from the owned actors, remember _teardowns, await delete().
  function ReleaseStart(s: State, a: Actor): State
    requires a in s.owned && a !in s.deleting
  {
    s.(owned := map b | b in s.owned && b != a :: s.owned[b],
       releasing := s.releasing[a := (s.owned[a], s.td)])
  }

  function ReleaseDeleteOk(s: State, a: Actor): State
    requires a in s.releasing
  {
    s.(live := s.live - {a}, releasing := map b | b in s.releasing && b != a :: s.releasing[b])
  }

  // Before AC-15.9's fix: release's delete raised or was cancelled. `missed`
  // is the code's missed_by_reap: a recycle whose suspend() returned after a
  // teardown. Then the actor was re-tagged WARM; otherwise it went back under
  // its run id.
  function ReleaseDeleteFail(s: State, a: Actor, missed: bool): State
    requires a in s.releasing
  {
    var run := if missed then WARM else s.releasing[a].0;
    s.(owned := s.owned[a := run],
       releasing := map b | b in s.releasing && b != a :: s.releasing[b])
  }

  // AC-15.9 (`_return_popped`): release's delete failed or was cancelled, or a
  // recycle was cancelled in suspend(). Own the actor again under WARM if a
  // reap ran since the pop (`not _may_pool(teardowns)`), else under its run
  // id. In the code the check and the update share one lock hold, which is
  // what makes this one atomic action.
  function ReturnPopped(s: State, a: Actor): State
    requires a in s.releasing
  {
    var run := if s.releasing[a].1 != s.td then WARM else s.releasing[a].0;
    s.(owned := s.owned[a := run],
       releasing := map b | b in s.releasing && b != a :: s.releasing[b])
  }

  // Before AC-15.9's fix: a recycle cancelled in suspend() went back under
  // its run id, even after a reap that could not see it.
  function RecycleSuspendCancelledBefore(s: State, a: Actor): State
    requires a in s.releasing
  {
    s.(owned := s.owned[a := s.releasing[a].0],
       releasing := map b | b in s.releasing && b != a :: s.releasing[b])
  }

  // warm_pool cancelled while its creates are in flight: `made` is the ids it
  // sent a create() for, of which `created` exist now. AC-15.8: own them all
  // under WARM (one never created is NotFound for the reap, which counts as
  // gone).
  function WarmCancelledFixed(s: State, made: set<Actor>, created: set<Actor>): State
    requires created <= made && made !! s.owned.Keys && made !! s.releasing.Keys
  {
    s.(live := s.live + created,
       owned := map a | a in s.owned.Keys + made :: if a in made then WARM else s.owned[a])
  }

  // Before AC-15.8's fix: the ids were dropped.
  function WarmCancelledBefore(s: State, made: set<Actor>, created: set<Actor>): State
    requires created <= made
  { s.(live := s.live + created) }

  // warm_pool(wait=True): create() returned but suspend() raised. AC-15.8:
  // own the actor under WARM and start its delete (a failed delete re-tags,
  // and it is WARM already).
  function WarmSuspendFailed(s: State, a: Actor): State
    requires a !in s.live && a !in s.owned && a !in s.releasing
  { s.(live := s.live + {a}, owned := s.owned[a := WARM], deleting := s.deleting + {a}) }

  // Before AC-15.8's fix: the actor was neither pooled, returned nor owned.
  function WarmSuspendFailedBefore(s: State, a: Actor): State
    requires a !in s.live
  { s.(live := s.live + {a}) }

  // ======================================================= #221 verifies
  lemma ReapSafe(s: State, r: Run)
    requires Inv(s) && r != WARM
    ensures Inv(Reap(s, r))
  {}

  lemma WarmCommitDrainedSafe(s: State, a: Actor)
    requires Inv(s) && a !in s.live && a !in s.releasing
    ensures Inv(WarmCommitDrained(s, a))
  {}

  lemma StartDeleteSafe(s: State, a: Actor)
    requires Inv(s) && a in s.owned
    ensures Inv(StartDelete(s, a))
  {}

  lemma DeleteOkSafe(s: State, a: Actor)
    requires Inv(s) && a in s.deleting
    ensures Inv(DeleteOk(s, a))
  {}

  lemma DeleteFailFixedSafe(s: State, a: Actor)
    requires Inv(s) && a in s.deleting
    ensures Inv(DeleteFailFixed(s, a))
  {}

  lemma ReleaseStartSafe(s: State, a: Actor)
    requires Inv(s) && a in s.owned && a !in s.deleting
    ensures Inv(ReleaseStart(s, a))
  {}

  lemma ReleaseDeleteOkSafe(s: State, a: Actor)
    requires Inv(s) && a in s.releasing
    ensures Inv(ReleaseDeleteOk(s, a))
  {}

  // A re-tagged actor goes with the next reap of any run.
  lemma AnyReapDeletesRetagged(s: State, a: Actor, r: Run)
    requires Inv(s) && a in s.owned && s.owned[a] == WARM && r != WARM
    ensures a !in Reap(s, r).owned && a !in Reap(s, r).live
  {}

  lemma WarmCancelledFixedSafe(s: State, made: set<Actor>, created: set<Actor>)
    requires Inv(s) && created <= made && made !! s.live
    requires made !! s.owned.Keys && made !! s.releasing.Keys
    ensures Inv(WarmCancelledFixed(s, made, created))
  {}

  lemma WarmSuspendFailedSafe(s: State, a: Actor)
    requires Inv(s) && a !in s.live && a !in s.owned && a !in s.releasing
    ensures Inv(WarmSuspendFailed(s, a))
  {}

  lemma DeleteCancelledFixedSafe(s: State, a: Actor)
    requires Inv(s) && a in s.deleting
    ensures Inv(DeleteCancelledFixed(s, a))
  {}

  lemma ReturnPoppedSafe(s: State, a: Actor)
    requires Inv(s) && a in s.releasing
    ensures Inv(ReturnPopped(s, a))
  {}

  // ==================== before the third round and before AC-15.9's fix: FAIL
  // Bug E: a failed delete after the run's reap keeps the reaped run id.
  lemma DeleteFailBeforeSafe(s: State, a: Actor)
    requires Inv(s) && a in s.deleting
    ensures Inv(DeleteFailBefore(s, a))                               // fails
  {}

  // Before AC-15.9's fix: a warm_pool cancelled after a teardown, before its
  // deletes started, left its actors under the reaped run id.
  lemma WarmCommitDrainedBeforeSafe(s: State, a: Actor, r: Run)
    requires Inv(s) && a !in s.live && a !in s.releasing
    ensures Inv(WarmCommitDrainedBefore(s, a, r))                     // fails
  {}

  // Before AC-15.9's fix: a recycle cancelled in suspend() after a reap went
  // back under the reaped run id.
  lemma RecycleSuspendCancelledBeforeSafe(s: State, a: Actor)
    requires Inv(s) && a in s.releasing
    ensures Inv(RecycleSuspendCancelledBefore(s, a))                  // fails
  {}

  // Before AC-15.8's fix: a warm actor whose suspend() failed stayed alive
  // and untracked.
  lemma WarmSuspendFailedBeforeSafe(s: State, a: Actor)
    requires Inv(s) && a !in s.live
    ensures Inv(WarmSuspendFailedBefore(s, a))                        // fails
  {}

  // Before AC-15.8's fix: a cancelled warm_pool left the actors it had
  // created alive and untracked.
  lemma WarmCancelledBeforeSafe(s: State, made: set<Actor>, created: set<Actor>)
    requires Inv(s) && created <= made && made !! s.live
    ensures Inv(WarmCancelledBefore(s, made, created))                // fails
  {}

  // Before AC-15.9's fix: a cancelled delete after the run's reap kept the
  // reaped run id.
  lemma DeleteCancelledSafe(s: State, a: Actor)
    requires Inv(s) && a in s.deleting
    ensures Inv(DeleteCancelled(s, a))                                // fails
  {}

  // Before AC-15.9's fix: a release whose delete failed or was cancelled
  // after a reap that ran during that delete, other than a recycle that saw
  // the teardown first, put the actor back under the reaped run id.
  lemma ReleaseDeleteFailSafe(s: State, a: Actor, missed: bool)
    requires Inv(s) && a in s.releasing
    ensures Inv(ReleaseDeleteFail(s, a, missed))                      // fails
  {}

  // ============================== concrete witnesses (these verify)
  // Before AC-15.9's fix. Run 1 releases actor 5 (delete in flight), reaps,
  // and the delete fails: actor 5 is owned under run 1, which no later reap
  // matches. With the fix, run 2's reap deletes it.
  lemma ReleaseOrphanWitness() {
    var s0 := State({5}, map[5 := 1], {}, map[], 0, {});
    assert Inv(s0);
    var s1 := ReleaseStart(s0, 5);
    var s2 := Reap(s1, 1);
    assert 5 in s2.live && 5 !in s2.owned;
    var s3 := ReleaseDeleteFail(s2, 5, false);
    assert s3.owned[5] == 1 && 1 in s3.reaped;
    assert !Inv(s3);
    var s4 := Reap(s3, 2);
    assert 5 in s4.owned && 5 in s4.live;      // run 2's reap skips it
    var f3 := ReturnPopped(s2, 5);
    assert f3.owned[5] == WARM && Inv(f3);
    var f4 := Reap(f3, 2);
    assert 5 !in f4.owned && 5 !in f4.live;    // run 2's reap deletes it
  }
}
