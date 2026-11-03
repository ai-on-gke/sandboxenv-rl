# Copyright 2026 Google LLC
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.

"""Warm-pool accounting under concurrency.

For any sequence of acquire, recycle, warm_pool and teardown operations,
interleaved at their awaits: the warm-replica gauge equals the warm pool,
teardown leaves no environment alive, and warm_pool never provisions more than
it was asked for. The fake client pauses ``suspend()`` per actor, ``create()``
per warm_pool call, ``delete()`` during a teardown and, when told to, fails
``get()``, ``create()`` or ``delete()``, so the test chooses where the
coroutines interleave.
"""

from __future__ import annotations

import asyncio
import itertools

import pytest
from hypothesis import settings
from hypothesis.stateful import (
    Bundle,
    RuleBasedStateMachine,
    consumes,
    initialize,
    invariant,
    precondition,
    rule,
)

from ate_env.backend.substrate import SubstrateBackendDriver
from ate_env.config import ObservabilityConfig
from ate_env.errors import NotFoundError, SandboxStartError
from ate_env.observability import Observer

R = 3  # replicas asked for by every warm_pool call


class GatedClient:
    """A Substrate client whose calls wait on, or fail by, test-held settings."""

    def __init__(self) -> None:
        self.live: set[str] = set()
        self.suspend_gates: dict[str, asyncio.Event] = {}
        self.create_gate: asyncio.Event | None = None
        self.create_errors: dict[int, Exception] = {}  # by call number, from 0
        self.creates = 0
        self.delete_gate: asyncio.Event | None = None
        self.delete_error: Exception | None = None
        self.get_gate: asyncio.Event | None = None
        self.get_error: Exception | None = None

    async def create(self, actor_id: str, **kw: object) -> None:
        n, self.creates = self.creates, self.creates + 1
        if self.create_gate is not None:
            await self.create_gate.wait()
        if n in self.create_errors:
            raise self.create_errors[n]
        self.live.add(actor_id)

    async def suspend(self, actor_id: str, **kw: object) -> None:
        gate = self.suspend_gates.get(actor_id)
        if gate is not None:
            await gate.wait()

    async def delete(self, actor_id: str, **kw: object) -> None:
        if self.delete_gate is not None:
            await self.delete_gate.wait()
        if self.delete_error is not None:
            raise self.delete_error
        self.live.discard(actor_id)

    async def get(self, actor_id: str, **kw: object) -> None:
        if self.get_gate is not None:
            await self.get_gate.wait()
        if self.get_error is not None:
            raise self.get_error


def _driver(client: GatedClient) -> tuple[SubstrateBackendDriver, Observer]:
    driver = SubstrateBackendDriver(client=client)
    obs = Observer(ObservabilityConfig(enable_metrics=False))
    driver.observer = obs
    return driver, obs


def _pool(driver: SubstrateBackendDriver) -> list[str]:
    return [a for v in driver._warm_paused_pool.values() for a in v]


async def _spin() -> None:
    for _ in range(20):
        await asyncio.sleep(0)


async def test_recycle_paused_across_teardown_deletes_the_environment() -> None:
    client = GatedClient()
    driver, obs = _driver(client)
    client.live.add("sb-1")
    driver._owned_actors["sb-1"] = driver._make_instance("sb-1", "tmpl", run_id="run-1")
    client.suspend_gates["sb-1"] = asyncio.Event()

    recycle = asyncio.create_task(driver.release_async("sb-1", recycle=True))
    await _spin()  # paused in suspend()
    await driver.reap_async("run-1")
    obs.warm_reset()
    client.suspend_gates["sb-1"].set()
    await recycle

    assert client.live == set()
    assert _pool(driver) == []
    assert sum(obs._warm.values()) == 0


async def test_overlapping_warm_pool_calls_provision_once() -> None:
    client = GatedClient()
    driver, obs = _driver(client)
    client.create_gate = asyncio.Event()

    first = asyncio.create_task(driver.warm_pool_async("tmpl", R))
    await _spin()  # paused in create()
    second = asyncio.create_task(driver.warm_pool_async("tmpl", R))
    await _spin()
    client.create_gate.set()
    await asyncio.gather(first, second)

    assert len(_pool(driver)) == R
    assert len(client.live) == R
    assert sum(obs._warm.values()) == R


async def test_warm_pool_paused_across_teardown_deletes_its_actors() -> None:
    client = GatedClient()
    driver, obs = _driver(client)
    client.create_gate = asyncio.Event()

    warm = asyncio.create_task(driver.warm_pool_async("tmpl", R))
    await _spin()  # paused in create()
    await driver.reap_async("run-1")
    obs.warm_reset()
    client.create_gate.set()
    await warm

    assert client.live == set()
    assert _pool(driver) == []
    assert sum(obs._warm.values()) == 0


async def test_warm_pool_after_teardown_ignores_the_old_reservation() -> None:
    client = GatedClient()
    driver, obs = _driver(client)
    client.create_gate = asyncio.Event()

    old = asyncio.create_task(driver.warm_pool_async("tmpl", R))
    await _spin()  # paused in create()
    await driver.reap_async("run-1")
    obs.warm_reset()
    new = asyncio.create_task(driver.warm_pool_async("tmpl", R, run_id="run-2"))
    await _spin()
    client.create_gate.set()
    await asyncio.gather(old, new)

    assert len(_pool(driver)) == R
    assert client.live == set(_pool(driver))
    assert sum(obs._warm.values()) == R


async def test_failed_warm_claim_after_teardown_deletes_the_environment() -> None:
    client = GatedClient()
    driver, obs = _driver(client)
    # A recycled actor from another run, waiting in the pool.
    client.live.add("sb-x")
    driver._owned_actors["sb-x"] = driver._make_instance("sb-x", "tmpl", run_id="run-0")
    driver._warm_paused_pool.setdefault(("tmpl", None), []).append("sb-x")
    obs.warm_add("substrate", 1)
    client.get_gate = asyncio.Event()
    client.get_error = RuntimeError("unavailable")

    claim = asyncio.create_task(driver._acquire_internal("tmpl", "run-1"))
    await _spin()  # paused in get()
    await driver.reap_async("run-1")
    obs.warm_reset()
    client.get_gate.set()
    with pytest.raises(SandboxStartError):
        await claim

    assert client.live == set()
    assert _pool(driver) == []
    assert sum(obs._warm.values()) == 0


async def test_warm_pool_finishing_during_a_reap_deletes_its_actors() -> None:
    client = GatedClient()
    driver, obs = _driver(client)
    client.live.add("sb-1")
    driver._owned_actors["sb-1"] = driver._make_instance("sb-1", "tmpl", run_id="run-1")
    client.delete_gate = asyncio.Event()

    reap = asyncio.create_task(driver.reap_async("run-1"))
    await _spin()  # paused in the reap's delete()
    warm = asyncio.create_task(driver.warm_pool_async("tmpl", R))
    await _spin()  # created during the reap
    assert _pool(driver) == []
    client.delete_gate.set()
    await reap
    obs.warm_reset()
    await warm

    assert client.live == set()
    assert _pool(driver) == []
    assert driver._owned_actors == {}
    assert sum(obs._warm.values()) == 0


async def test_warm_pool_pools_what_it_created_before_raising() -> None:
    client = GatedClient()
    driver, obs = _driver(client)
    client.create_errors[1] = NotFoundError("no such template")

    with pytest.raises(SandboxStartError):
        await driver.warm_pool_async("tmpl", R)

    assert len(_pool(driver)) == R - 1
    assert set(_pool(driver)) == client.live
    assert sum(obs._warm.values()) == R - 1


async def test_failed_delete_after_teardown_keeps_the_actors_for_the_next_reap() -> None:
    client = GatedClient()
    driver, obs = _driver(client)
    failures: list[str] = []
    obs.release_failed = failures.append  # type: ignore[method-assign]
    client.create_gate = asyncio.Event()

    warm = asyncio.create_task(driver.warm_pool_async("tmpl", R))
    await _spin()  # paused in create()
    await driver.reap_async("run-1")
    obs.warm_reset()
    client.delete_error = RuntimeError("unavailable")
    client.create_gate.set()
    await warm

    assert failures == ["substrate"] * R
    assert len(client.live) == R
    assert set(driver._owned_actors) == client.live
    assert _pool(driver) == []

    client.delete_error = None
    await driver.reap_async("run-1")
    assert client.live == set()
    assert driver._owned_actors == {}


async def test_cancelled_delete_after_teardown_keeps_the_actor_owned() -> None:
    client = GatedClient()
    driver, obs = _driver(client)
    client.live.add("sb-x")
    driver._owned_actors["sb-x"] = driver._make_instance("sb-x", "tmpl", run_id="run-0")
    driver._warm_paused_pool.setdefault(("tmpl", None), []).append("sb-x")
    obs.warm_add("substrate", 1)
    client.get_gate = asyncio.Event()
    client.get_error = RuntimeError("unavailable")

    claim = asyncio.create_task(driver._acquire_internal("tmpl", "run-1"))
    await _spin()  # paused in get()
    await driver.reap_async("run-1")
    obs.warm_reset()
    client.delete_gate = asyncio.Event()
    client.get_gate.set()
    await _spin()  # paused in the delete after teardown
    claim.cancel()
    with pytest.raises(asyncio.CancelledError):
        await claim

    assert "sb-x" in driver._owned_actors
    client.delete_gate.set()
    await driver.reap_async("run-0")
    assert client.live == set()


async def test_recycle_cancelled_in_suspend_keeps_the_actor_owned() -> None:
    client = GatedClient()
    driver, _ = _driver(client)
    client.live.add("sb-1")
    driver._owned_actors["sb-1"] = driver._make_instance("sb-1", "tmpl", run_id="run-1")
    client.suspend_gates["sb-1"] = asyncio.Event()

    recycle = asyncio.create_task(driver.release_async("sb-1", recycle=True))
    await _spin()  # paused in suspend()
    recycle.cancel()
    with pytest.raises(asyncio.CancelledError):
        await recycle

    assert "sb-1" in driver._owned_actors
    await driver.reap_async("run-1")
    assert client.live == set()


class WarmPoolMachine(RuleBasedStateMachine):
    owned = Bundle("owned")
    recycling = Bundle("recycling")

    def __init__(self) -> None:
        super().__init__()
        self.loop = asyncio.new_event_loop()
        self.client = GatedClient()
        self.driver, self.obs = _driver(self.client)
        self.ids = itertools.count()
        self.inflight: dict[str, asyncio.Task[None]] = {}
        self.warm_tasks: list[asyncio.Task[None]] = []
        self.reap: asyncio.Task[None] | None = None
        self.claimed: set[str] = set()
        self.torn_down = False

    def teardown(self) -> None:
        tasks = [*self.inflight.values(), *self.warm_tasks]
        if self.reap is not None:
            tasks.append(self.reap)
        for t in tasks:
            t.cancel()
        if tasks:
            self.loop.run_until_complete(asyncio.gather(*tasks, return_exceptions=True))
        self.loop.close()

    def _spin(self) -> None:
        self.loop.run_until_complete(_spin())
        # Collect finished work; a task still waiting on a gate stays.
        for aid, task in list(self.inflight.items()):
            if task.done():
                task.result()
                del self.inflight[aid]
        for task in [t for t in self.warm_tasks if t.done()]:
            task.result()
            self.warm_tasks.remove(task)

    def _quiet(self) -> bool:
        return not self.torn_down and self.reap is None

    async def _fleet_teardown(self) -> None:
        # What SandboxFleet.close and AsyncSandboxFleet.teardown do: reset the
        # gauge as soon as the reap returns, with no await in between.
        await self.driver.reap_async("run-1")
        self.obs.warm_reset()
        self.torn_down = True

    # One actor up front, so a teardown always waits on a delete and other work
    # can run inside it.
    @initialize(target=owned)
    def first_actor(self) -> str:
        return self.acquire_new()

    @precondition(lambda self: self._quiet())
    @rule(target=owned)
    def acquire_new(self) -> str:
        aid = f"sb-{next(self.ids)}"
        self.client.live.add(aid)
        self.driver._owned_actors[aid] = self.driver._make_instance(aid, "tmpl", run_id="run-1")
        return aid

    @precondition(lambda self: self._quiet() and _pool(self.driver))
    @rule(target=owned)
    def acquire_warm(self) -> str:
        inst = self.loop.run_until_complete(self.driver._acquire_internal("tmpl", "run-1"))
        self.claimed.add(inst.instance_id)
        return inst.instance_id

    # Recycles and warm_pool calls may start and finish while a teardown runs.
    @precondition(lambda self: not self.torn_down)
    @rule(target=recycling, aid=consumes(owned))
    def start_recycle(self, aid: str) -> str:
        if aid not in self.driver._owned_actors or aid in self.inflight:
            return aid  # already reaped, released or recycling
        self.client.suspend_gates[aid] = asyncio.Event()
        self.inflight[aid] = self.loop.create_task(self.driver.release_async(aid, recycle=True))
        self._spin()  # paused in suspend()
        return aid

    @rule(target=owned, aid=consumes(recycling))
    def finish_recycle(self, aid: str) -> str:
        if aid in self.client.suspend_gates:
            self.client.suspend_gates[aid].set()
        self._spin()
        return aid

    @precondition(lambda self: not self.torn_down)
    @rule()
    def start_warm_pool(self) -> None:
        if self.client.create_gate is None or self.client.create_gate.is_set():
            self.client.create_gate = asyncio.Event()
        self.warm_tasks.append(self.loop.create_task(self.driver.warm_pool_async("tmpl", R)))
        self._spin()  # paused in create()

    @precondition(lambda self: self.warm_tasks)
    @rule()
    def finish_warm_pools(self) -> None:
        assert self.client.create_gate is not None
        self.client.create_gate.set()
        self._spin()

    # The teardown in two steps, so other work can run while the reap waits on
    # its deletes.
    @precondition(lambda self: self._quiet())
    @rule()
    def start_teardown(self) -> None:
        self.client.delete_gate = asyncio.Event()
        self.reap = self.loop.create_task(self._fleet_teardown())
        self._spin()  # paused in the reap's delete() calls

    @precondition(lambda self: self.reap is not None)
    @rule()
    def finish_teardown(self) -> None:
        assert self.client.delete_gate is not None and self.reap is not None
        self.client.delete_gate.set()
        self.client.delete_gate = None
        self.loop.run_until_complete(self.reap)
        self.reap = None
        self._spin()

    @rule()
    def idle(self) -> None:
        self._spin()

    @invariant()
    def gauge_equals_pool(self) -> None:  # P-9
        if self.reap is not None and not self.reap.done():
            return  # the fleet resets the gauge after the reap returns
        gauge = sum(self.obs._warm.values())
        assert gauge == len(_pool(self.driver)), f"gauge={gauge} pool={len(_pool(self.driver))}"

    @invariant()
    def nothing_survives_teardown(self) -> None:  # P-10
        if self.torn_down and not self.inflight and not self.warm_tasks:
            assert not self.client.live, f"alive after teardown: {sorted(self.client.live)}"

    @invariant()
    def warm_pool_never_over_provisions(self) -> None:  # P-11
        # Actors warm_pool created and nobody claimed yet. Recycled actors
        # (cold-created, or claimed from the pool) may take the pool above R.
        fresh = [
            a for a in _pool(self.driver) if a.startswith("warm-") and a not in self.claimed
        ]
        assert len(fresh) <= R, f"{len(fresh)} warm_pool actors pooled, asked for {R}"


WarmPoolMachine.TestCase.settings = settings(
    max_examples=500, stateful_step_count=12, deadline=None
)
test_warm_pool_accounting = WarmPoolMachine.TestCase
