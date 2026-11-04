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

// Extension of warm_pool.dfy for the two bugs review found in the first
// version of the AC-15 fix (#221), which the original model cannot express:
//
//   A. A warm_pool reservation outlives a teardown, so a warm_pool started
//      after the teardown counts the stale reservation and returns short.
//      The original model makes `closed` permanent: no warm_pool can start
//      after a teardown, so the symptom has no state to live in.
//   B. A warm claim pops an actor, awaits get(), and on failure puts the actor
//      back in the pool, even if a teardown ran during the await. The original
//      model's Acquire is one atomic step with no await and no put-back.
//
// Here a teardown bumps a generation counter instead of closing the driver,
// and a claim is two steps. Each action is the code between two awaits, as in
// warm_pool.dfy. Ghost fields record facts the code does not store, so the
// properties can be stated:
//
//   P-9   gauge == |pool|
//   P-10  (multi-generation form) every pooled actor entered the pool, or was
//         claimed from it, in the current generation: nothing from before the
//         last teardown is handed out again
//   P-11  pool actors warm_pool created + reservations <= R
//   PLAN  (bug A) after a warm_pool plans, the pool plus the current
//         generation's reservations covers R

module WarmPoolGenerations {

  type Actor = nat

  datatype State = State(
    live: set<Actor>,          // environments that exist on Substrate
    owned: set<Actor>,         // driver._owned_actors
    pool: set<Actor>,          // driver._warm_paused_pool (one key)
    gauge: int,                // observer warm-replica share
    gen: nat,                  // driver._teardowns
    reserved: nat,             // what the code subtracts: _warm_inflight lookup
    ghost cur: nat,            // reservations held by calls of generation `gen`
    ghost stale: nat,          // reservations held by calls from older generations
    claiming: map<Actor, nat>, // popped by a warm claim, awaiting get(); value = gen at pop
    ghost epoch: map<Actor, nat>, // generation in which each pooled or claimed actor
                                  // entered the pool or left it
    ghost fromWarm: set<Actor>,   // pooled actors warm_pool created
    R: nat
  )

  ghost predicate Inv(s: State) {
    && s.gauge == |s.pool|                                          // P-9
    && s.pool <= s.owned && s.owned <= s.live
    // No `claiming.Keys <= live`: a reap can delete an actor whose claim is
    // still awaiting get() (see ClaimOkReturnsLive below). A claim from the
    // current generation is live: no reap ran since its pop (AC-15.7).
    && (forall a :: a in s.claiming ==> s.claiming[a] <= s.gen)
    && (forall a :: a in s.claiming && s.claiming[a] == s.gen ==> a in s.live)
    && s.pool !! s.claiming.Keys
    && (forall a :: a in s.pool ==> a in s.epoch && s.epoch[a] == s.gen)   // P-10
    && (forall a :: a in s.claiming ==> a in s.epoch && s.claiming[a] == s.epoch[a])
    && s.fromWarm <= s.pool
    && |s.fromWarm| + s.cur <= s.R                                  // P-11
  }

  // The code #221 ships: reservations are keyed by generation, so
  // `reserved` is the current generation's count, and reap drops them.
  ghost predicate Fixed(s: State) { s.reserved == s.cur }

  // The first version: one counter per pool key that reap leaves alone, so
  // stale calls' reservations still count.
  ghost predicate First(s: State) { s.reserved == s.cur + s.stale }

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

  // Part 2, a current-generation call's create() returned: pool the actor.
  // `a` is a fresh id (warm-<uuid>): never live, never claimed before.
  function CommitCurrent(s: State, a: Actor): State
    requires s.cur > 0 && s.reserved > 0 && a !in s.live && a !in s.claiming
  {
    s.(reserved := s.reserved - 1, cur := s.cur - 1,
       live := s.live + {a}, owned := s.owned + {a}, pool := s.pool + {a},
       gauge := s.gauge + 1, fromWarm := s.fromWarm + {a},
       epoch := s.epoch[a := s.gen])
  }

  // Part 2 for a call that started before the last reap: it sees the
  // generation changed and deletes what it created (create + delete). In the
  // first version its release still decrements the shared counter.
  function CommitStaleFixed(s: State): State
    requires s.stale > 0
  { s.(stale := s.stale - 1) }

  function CommitStaleFirst(s: State): State
    requires s.stale > 0 && s.reserved > 0
  { s.(stale := s.stale - 1, reserved := s.reserved - 1) }

  // ------------------------------------------------------------ teardown
  // reap + warm_reset. `keep` is owned actors reap does not delete: those of
  // other runs (a recycled actor from another run, claimed and in flight, say).
  // Every in-flight reservation becomes stale.
  function ReapFixed(s: State, keep: set<Actor>): State
    requires keep <= s.owned - s.pool
  {
    s.(live := s.live - (s.owned - keep), owned := keep, pool := {}, gauge := 0,
       fromWarm := {}, gen := s.gen + 1,
       reserved := 0, stale := s.stale + s.cur, cur := 0)      // dict cleared
  }

  function ReapFirst(s: State, keep: set<Actor>): State
    requires keep <= s.owned - s.pool
  {
    s.(live := s.live - (s.owned - keep), owned := keep, pool := {}, gauge := 0,
       fromWarm := {}, gen := s.gen + 1,
       stale := s.stale + s.cur, cur := 0)                      // counter kept
  }

  // ------------------------------------------------------------ warm claim
  // Pop under the lock, count it out, then await get().
  function ClaimStart(s: State, a: Actor): State
    requires a in s.pool
  {
    s.(pool := s.pool - {a}, gauge := s.gauge - 1, fromWarm := s.fromWarm - {a},
       claiming := s.claiming[a := s.gen], epoch := s.epoch[a := s.gen])
  }

  // get() raised: put the actor back and count it in (before #221, and its first version).
  function ClaimFailPutBack(s: State, a: Actor): State
    requires a in s.claiming && a in s.owned
  {
    s.(claiming := s.claiming - {a}, pool := s.pool + {a}, gauge := s.gauge + 1)
  }

  // #221: put back only if no reap ran since the pop; otherwise delete it.
  function ClaimFailFixed(s: State, a: Actor): State
    requires a in s.claiming
  {
    if s.claiming[a] == s.gen && a in s.owned then
      s.(claiming := s.claiming - {a}, pool := s.pool + {a}, gauge := s.gauge + 1)
    else
      s.(claiming := s.claiming - {a}, owned := s.owned - {a}, live := s.live - {a})
  }

  // get() succeeded. AC-15.7: the claim ends either way; the actor is handed
  // out only if no reap ran since the pop, otherwise acquire claims or creates
  // another, and the reap deletes this one.
  function ClaimOk(s: State, a: Actor): State
    requires a in s.claiming
  { s.(claiming := s.claiming - {a}) }

  ghost predicate HandsOut(s: State, a: Actor)
    requires a in s.claiming
  { s.claiming[a] == s.gen }

  // ========================================================== #221 verifies
  lemma PlanFixedSafe(s: State)
    requires Inv(s) && Fixed(s)
    ensures Inv(Plan(s)) && Fixed(Plan(s))
    ensures |Plan(s).pool| + Plan(s).cur >= s.R                     // PLAN
  { CardSub(s.fromWarm, s.pool); }

  lemma CommitCurrentSafe(s: State, a: Actor)
    requires Inv(s) && Fixed(s) && s.cur > 0 && a !in s.live && a !in s.claiming
    ensures Inv(CommitCurrent(s, a)) && Fixed(CommitCurrent(s, a))
  {}

  lemma CommitStaleFixedSafe(s: State)
    requires Inv(s) && Fixed(s) && s.stale > 0
    ensures Inv(CommitStaleFixed(s)) && Fixed(CommitStaleFixed(s))
  {}

  lemma ReapFixedSafe(s: State, keep: set<Actor>)
    requires Inv(s) && Fixed(s) && keep <= s.owned - s.pool
    ensures Inv(ReapFixed(s, keep)) && Fixed(ReapFixed(s, keep))
  {}

  lemma ClaimStartSafe(s: State, a: Actor)
    requires Inv(s) && a in s.pool
    ensures Inv(ClaimStart(s, a))
  {}

  lemma ClaimFailFixedSafe(s: State, a: Actor)
    requires Inv(s) && a in s.claiming
    ensures Inv(ClaimFailFixed(s, a))
  {}

  // AC-15.7: what a successful claim hands out is live.
  lemma ClaimOkFixedReturnsLive(s: State, a: Actor)
    requires Inv(s) && a in s.claiming && HandsOut(s, a)
    ensures a in s.live
  {}

  lemma ClaimOkSafe(s: State, a: Actor)
    requires Inv(s) && a in s.claiming
    ensures Inv(ClaimOk(s, a))
  {}

  // ======== the first version of #221 and the code before AC-15.7's fix: FAIL
  // Bug A. A reap leaves the counter, so a warm_pool planned afterwards counts
  // stale reservations and the pool plus its own reservations falls short.
  lemma PlanFirstCovers(s: State)
    requires Inv(s) && First(s)
    ensures |Plan(s).pool| + Plan(s).cur >= s.R                     // fails
  {}

  // Before AC-15.7's fix: get() succeeded after a reap deleted the claimed
  // actor, and acquire handed the caller a deleted environment.
  lemma ClaimOkReturnsLive(s: State, a: Actor)
    requires Inv(s) && a in s.claiming
    ensures a in s.live                                             // fails
  {}

  // Bug B. A failed claim puts back an actor popped before a reap.
  lemma ClaimFailPutBackSafe(s: State, a: Actor)
    requires Inv(s) && a in s.claiming && a in s.owned
    ensures Inv(ClaimFailPutBack(s, a))                             // fails
  {}

  // ============================== concrete witnesses (these verify)
  // Bug A: warm_pool(R=3) paused in create(), reap, then a new warm_pool(3).
  lemma BugAWitness() {
    var s0 := State({}, {}, {}, 0, 0, 0, 0, 0, map[], map[], {}, 3);
    assert |s0.pool| == 0;
    var s1 := Plan(s0);              // first call reserves 3
    assert s1.reserved == 3 && s1.cur == 3;
    var s2 := ReapFirst(s1, {});     // teardown, counter kept
    assert s2.pool == {} && s2.reserved == 3 && s2.cur == 0 && s2.stale == 3;
    var s3 := Plan(s2);              // new call after teardown: needed = 3 - 0 - 3
    assert s3 == s2;
    assert |s3.pool| + s3.cur == 0;  // nothing will fill the pool for this call
  }

  // Bug B: actor 7 of another run sits in the pool; a claim pops it, a reap
  // runs (keeps it: other run), get() fails, the actor is put back.
  lemma BugBWitness()
    ensures var s0 := State({7}, {7}, {7}, 1, 0, 0, 0, 0, map[], map[7 := 0], {}, 3);
            var s1 := ClaimStart(s0, 7);
            var s2 := ReapFixed(s1, {7});
            var s3 := ClaimFailPutBack(s2, 7);
            Inv(s2) && !Inv(s3) && 7 in s3.pool && s3.epoch[7] < s3.gen
  {}
}
