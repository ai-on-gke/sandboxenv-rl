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

// Concurrency model of SubstrateBackendDriver's warm-pool bookkeeping.
//
// The driver runs coroutines on one asyncio loop, so code between two `await`s
// runs atomically, but coroutines interleave at every `await`. We model each
// such segment as one atomic action on a shared State. Proving that every
// action preserves the invariant proves it for ALL interleavings of any number
// of concurrent acquire / recycle / warm_pool / reap calls.
//
// Spec: ../properties.md (P-9..P-11), ../acceptance-criteria.md (AC-15).
// warm_pool_generations.dfy extends it to teardowns that do not end the driver.
// Code: clients/python/src/ate_env_rl/backend/substrate.py (fixed in #221).

module WarmPool {

  type Actor = nat

  datatype State = State(
    live: set<Actor>,       // environments that exist on Substrate
    owned: set<Actor>,      // driver._owned_actors
    pool: set<Actor>,       // driver._warm_paused_pool (one key)
    gauge: int,             // Observer._warm[backend] -> ate_env_fleet_warm_replicas
    recycling: set<Actor>,  // release(recycle=True) popped from owned, awaiting suspend()
    fromWarm: set<Actor>,   // pooled actors that warm_pool created (not recycled ones)
    pending: nat,           // warm_pool: replicas reserved but not yet created
    closed: bool,           // reap()/teardown ran; models "the driver's teardown counter
                            // changed since this call started" (interfaces.md)
    R: nat                  // replicas requested for this pool key
  )

  // ---------------------------------------------------------------- invariant
  ghost predicate Inv(s: State) {
    && s.gauge == |s.pool|                                   // P-9  gauge tells the truth
    && s.pool <= s.owned && s.owned <= s.live                // bookkeeping is consistent
    && s.recycling !! s.owned && s.recycling <= s.live
    && s.fromWarm <= s.pool
    && |s.fromWarm| + s.pending <= s.R                       // P-11 warm_pool never over-provisions
    && (s.closed ==> s.pool == {} && s.owned == {}           // P-10 nothing survives teardown
                     && s.pending == 0)
  }

  // ------------------------------------------------------- actions (fixed code)
  // Each action is the code between two awaits, with the fixes #221 shipped.

  // acquire: pop a warm actor and count it out (_acquire_internal, under the lock).
  function Acquire(s: State, a: Actor): State
    requires a in s.pool
  { s.(pool := s.pool - {a}, fromWarm := s.fromWarm - {a}, gauge := s.gauge - 1) }

  // release(recycle=True), part 1: pop from owned, then `await suspend()`.
  function RecycleStart(s: State, a: Actor): State
    requires a in s.owned && a !in s.pool
  { s.(owned := s.owned - {a}, recycling := s.recycling + {a}) }

  // part 2, after the await. FIX: if teardown ran meanwhile, delete the actor
  // instead of putting it back in the pool.
  function RecycleFinish(s: State, a: Actor): State
    requires a in s.recycling
  {
    if s.closed then
      s.(recycling := s.recycling - {a}, live := s.live - {a})
    else
      s.(recycling := s.recycling - {a}, pool := s.pool + {a},
         owned := s.owned + {a}, gauge := s.gauge + 1)
  }

  // warm_pool, part 1 (before the awaits). FIX: reserve the replicas under the
  // lock, counting replicas other calls are already creating.
  function WarmPlan(s: State): State {
    if s.closed || s.R <= |s.pool| + s.pending then s
    else s.(pending := s.pending + (s.R - |s.pool| - s.pending))
  }

  // warm_pool, part 2: one create() returned; pool it. FIX: give up the
  // reservation (and delete) if teardown ran meanwhile.
  function WarmCommit(s: State, a: Actor): State
    requires s.pending > 0 && a !in s.live
  {
    if s.closed then s.(pending := s.pending - 1)
    else s.(pending := s.pending - 1, live := s.live + {a},
            owned := s.owned + {a}, pool := s.pool + {a},
            fromWarm := s.fromWarm + {a}, gauge := s.gauge + 1)
  }

  // reap + warm_reset (teardown): delete everything owned, empty the pool.
  function Reap(s: State): State {
    s.(live := s.live - s.owned, owned := {}, pool := {}, fromWarm := {},
       gauge := 0, pending := 0, closed := true)
  }

  // ------------------------------------------- the proof: every action is safe
  lemma CardSub(a: set<Actor>, b: set<Actor>)
    requires a <= b
    ensures |a| <= |b|
  {
    if a != {} { var x :| x in a; CardSub(a - {x}, b - {x}); }
  }

  lemma AcquireSafe(s: State, a: Actor)
    requires Inv(s) && a in s.pool
    ensures Inv(Acquire(s, a))
  {}

  lemma RecycleStartSafe(s: State, a: Actor)
    requires Inv(s) && a in s.owned && a !in s.pool
    ensures Inv(RecycleStart(s, a))
  {}

  lemma RecycleFinishSafe(s: State, a: Actor)
    requires Inv(s) && a in s.recycling
    ensures Inv(RecycleFinish(s, a))
  {}

  lemma WarmPlanSafe(s: State)
    requires Inv(s)
    ensures Inv(WarmPlan(s))
  { CardSub(s.fromWarm, s.pool); }

  lemma WarmCommitSafe(s: State, a: Actor)
    requires Inv(s) && s.pending > 0 && a !in s.live
    ensures Inv(WarmCommit(s, a))
  {}

  lemma ReapSafe(s: State)
    requires Inv(s)
    ensures Inv(Reap(s))
  {}
}

// --------------------------------------------------------------------------
// The code before #221, same model without the fixes. These lemmas FAIL; each
// failure is an interleaving that breaks a property (reproduced in Python).
module WarmPoolBefore {
  import opened WarmPool

  // Before #221: after `await suspend()`, the actor is always pooled.
  function RecycleFinishNow(s: State, a: Actor): State
    requires a in s.recycling
  {
    s.(recycling := s.recycling - {a}, pool := s.pool + {a},
       owned := s.owned + {a}, gauge := s.gauge + 1)
  }

  // Before #221: `needed = replicas - len(pool)` is read before the awaits, and
  // other in-flight warm_pool calls are not counted.
  function WarmPlanNow(s: State): State {
    s.(pending := s.pending + (if s.R >= |s.pool| then s.R - |s.pool| else 0))
  }

  lemma RecycleFinishNowSafe(s: State, a: Actor)   // fails: recycle after reap leaks (P-10)
    requires Inv(s) && a in s.recycling
    ensures Inv(RecycleFinishNow(s, a))
  {}

  lemma WarmPlanNowSafe(s: State)                  // fails: double warm_pool over-provisions (P-11)
    requires Inv(s)
    ensures Inv(WarmPlanNow(s))
  {}
}
