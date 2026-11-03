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
per warm_pool call and, when told to, fails ``get()``, so the test chooses
where the coroutines interleave.
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
    invariant,
    precondition,
    rule,
)

from ate_env.backend.substrate import SubstrateBackendDriver
from ate_env.config import ObservabilityConfig
from ate_env.errors import SandboxStartError
from ate_env.observability import Observer

R = 3  # replicas asked for by every warm_pool call


class GatedClient:
    """A Substrate client whose suspend and create wait on test-held events."""

    def __init__(self) -> None:
        self.live: set[str] = set()
        self.suspend_gates: dict[str, asyncio.Event] = {}
        self.create_gate: asyncio.Event | None = None
        self.get_gate: asyncio.Event | None = None
        self.get_error: Exception | None = None

    async def create(self, actor_id: str, **kw: object) -> None:
        if self.create_gate is not None:
            await self.create_gate.wait()
        self.live.add(actor_id)

    async def suspend(self, actor_id: str, **kw: object) -> None:
        gate = self.suspend_gates.get(actor_id)
        if gate is not None:
            await gate.wait()

    async def delete(self, actor_id: str, **kw: object) -> None:
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
        self.claimed: set[str] = set()
        self.torn_down = False

    def teardown(self) -> None:
        tasks = [*self.inflight.values(), *self.warm_tasks]
        for t in tasks:
            t.cancel()
        if tasks:
            self.loop.run_until_complete(asyncio.gather(*tasks, return_exceptions=True))
        self.loop.close()

    def _spin(self) -> None:
        self.loop.run_until_complete(_spin())

    @precondition(lambda self: not self.torn_down)
    @rule(target=owned)
    def acquire_new(self) -> str:
        aid = f"sb-{next(self.ids)}"
        self.client.live.add(aid)
        self.driver._owned_actors[aid] = self.driver._make_instance(aid, "tmpl", run_id="run-1")
        return aid

    @precondition(lambda self: not self.torn_down and _pool(self.driver))
    @rule(target=owned)
    def acquire_warm(self) -> str:
        inst = self.loop.run_until_complete(self.driver._acquire_internal("tmpl", "run-1"))
        self.claimed.add(inst.instance_id)
        return inst.instance_id

    @precondition(lambda self: not self.torn_down)
    @rule(target=recycling, aid=consumes(owned))
    def start_recycle(self, aid: str) -> str:
        if aid not in self.driver._owned_actors:
            return aid  # already reaped or released
        self.client.suspend_gates[aid] = asyncio.Event()
        self.inflight[aid] = self.loop.create_task(self.driver.release_async(aid, recycle=True))
        self._spin()  # paused in suspend()
        return aid

    @rule(target=owned, aid=consumes(recycling))
    def finish_recycle(self, aid: str) -> str:
        task = self.inflight.pop(aid, None)
        if task is not None:
            self.client.suspend_gates[aid].set()
            self.loop.run_until_complete(task)
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
        self.loop.run_until_complete(asyncio.gather(*self.warm_tasks))
        self.warm_tasks.clear()

    @precondition(lambda self: not self.torn_down)
    @rule()
    def fleet_teardown(self) -> None:
        # What SandboxFleet.close and AsyncSandboxFleet.teardown do.
        self.loop.run_until_complete(self.driver.reap_async("run-1"))
        self.obs.warm_reset()
        self.torn_down = True

    @rule()
    def idle(self) -> None:
        self._spin()

    @invariant()
    def gauge_equals_pool(self) -> None:  # P-9
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
    max_examples=200, stateful_step_count=12, deadline=None
)
test_warm_pool_accounting = WarmPoolMachine.TestCase
