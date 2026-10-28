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

from __future__ import annotations

import asyncio
import threading

import pytest

from ate_env import FleetConfig, SandboxFleet, Task
from ate_env.async_fleet import AsyncSandboxFleet
from ate_env.backend.base import BackendDriver
from ate_env.backend.mock import MockBackendDriver
from ate_env.backend.substrate import SubstrateBackendDriver
from ate_env.lookahead import LookaheadWarmer, WarmReport

T = "default-template"


class _PoolDriver(MockBackendDriver):
    """Substrate-like pools: warm_pool tops up, a claim takes a replica for good."""

    def __init__(self, fail_images: tuple[str, ...] = ()):
        super().__init__()
        self.pools: dict[tuple[str, str | None], int] = {}
        self.fail_images = set(fail_images)
        self.calls: list[tuple[str, str | None, int]] = []
        self._mu = threading.Lock()

    def warm_pool(self, template_id, replicas, wait=True, run_id=None, rootfs_image=None):
        if rootfs_image in self.fail_images:
            raise RuntimeError(f"cannot warm {rootfs_image}")
        with self._mu:
            key = (template_id, rootfs_image)
            self.pools[key] = max(self.pools.get(key, 0), replicas)
            self.calls.append(("warm", rootfs_image, replicas))

    def unwarm_pool(self, template_id, rootfs_image=None):
        with self._mu:
            self.pools.pop((template_id, rootfs_image), None)
            self.calls.append(("unwarm", rootfs_image, 0))

    def pool_size(self, template_id, rootfs_image=None):
        with self._mu:
            return self.pools.get((template_id, rootfs_image), 0)

    def resize_pool(self, template_id, replicas, wait=True, rootfs_image=None):
        if rootfs_image in self.fail_images:
            raise RuntimeError(f"cannot warm {rootfs_image}")
        with self._mu:
            self.pools[(template_id, rootfs_image)] = replicas
            self.calls.append(("scale", rootfs_image, replicas))

    def acquire(self, template_id, run_id, timeout_s=180.0, rootfs_image=None):
        with self._mu:
            key = (template_id, rootfs_image)
            if self.pools.get(key, 0) > 0:
                self.pools[key] -= 1
        return super().acquire(template_id, run_id, timeout_s, rootfs_image)

    def idle(self, img: str) -> int:
        return self.pools.get((T, img), 0)


def _setup(cap=8, **kw):
    driver = _PoolDriver(**kw.pop("driver_kw", {}))
    fleet = SandboxFleet(FleetConfig(backend="mock", max_warmpool_replicas=cap), driver=driver)
    return fleet, driver


def test_add_warms_each_image_for_its_demand_capped():
    fleet, driver = _setup(cap=8)
    warmer = LookaheadWarmer(fleet)
    report = warmer.add(0, {"a": 4, "b": 16})
    assert report.ok and report.changes == {"a": (0, 4), "b": (0, 8)}
    assert (driver.idle("a"), driver.idle("b")) == (4, 8)
    assert warmer.requested == {"a": 4, "b": 8}


def test_lookahead_steps_add_up_and_finish_releases_them():
    fleet, driver = _setup()
    warmer = LookaheadWarmer(fleet)
    warmer.add(0, {"a": 2})
    warmer.add(1, {"a": 3, "b": 1})
    assert warmer.requested == {"a": 5, "b": 1}
    assert driver.idle("a") == 5

    warmer.finish(0)
    assert warmer.requested == {"a": 3, "b": 1}
    assert driver.idle("a") == 3
    warmer.finish(1)
    assert warmer.requested == {}
    assert driver.idle("a") == driver.idle("b") == 0


def test_keep_finished_holds_pools_for_stale_claims():
    fleet, driver = _setup()
    warmer = LookaheadWarmer(fleet, keep_finished=1)
    warmer.add(0, {"a": 2})
    warmer.add(1, {"b": 2})
    warmer.finish(0)                    # kept: max_staleness claims may still arrive
    assert warmer.requested == {"a": 2, "b": 2}
    warmer.add(2, {"c": 2})
    warmer.finish(1)                    # pushes step 0 out
    assert warmer.requested == {"b": 2, "c": 2}
    assert driver.idle("a") == 0


def test_claimed_replicas_are_not_warmed_again():
    fleet, driver = _setup()
    warmer = LookaheadWarmer(fleet)
    warmer.add(0, {"a": 4})
    for i in range(3):                  # step 0 claims three of its four replicas
        fleet.acquire(Task(id=f"t{i}", image="a"))
    assert driver.idle("a") == 1

    warmer.add(1, {"a": 2})             # +2 on top of what is idle, not back to 6
    assert driver.idle("a") == 3
    warmer.finish(0)                    # -4 drains only what is idle
    assert driver.idle("a") == 0


def test_errors_are_reported_and_retried_on_the_next_call():
    fleet, driver = _setup(driver_kw={"fail_images": ("bad",)})
    warmer = LookaheadWarmer(fleet)
    report = warmer.add(0, {"good": 2, "bad": 2})
    assert set(report.errors) == {"bad"}
    assert report.changes == {"good": (0, 2)}
    assert warmer.requested == {"good": 2}

    driver.fail_images.clear()
    report = warmer.add(1, {"good": 1})
    assert report.changes == {"good": (2, 3), "bad": (0, 2)}


def test_fail_fast_raises():
    fleet, _ = _setup(driver_kw={"fail_images": ("bad",)})
    warmer = LookaheadWarmer(fleet, fail_fast=True)
    with pytest.raises(RuntimeError, match="cannot warm bad"):
        warmer.add(0, {"bad": 1})


def test_close_drains_everything_and_refuses_more():
    fleet, driver = _setup()
    warmer = LookaheadWarmer(fleet)
    warmer.add(0, {"a": 2, "b": 3})
    report = warmer.close()
    assert report.changes == {"a": (2, 0), "b": (3, 0)}
    assert driver.idle("a") == driver.idle("b") == 0
    assert warmer.close() == WarmReport()
    with pytest.raises(RuntimeError, match="closed"):
        warmer.add(1, {"a": 1})


@pytest.mark.parametrize("call, match", [
    (lambda w: w.add(0, {"a": 1}), "already added"),
    (lambda w: w.finish(7), "not live"),
    (lambda w: w.add(1, {"a": -1}), "int >= 0"),
])
def test_misuse_is_rejected(call, match):
    fleet, _ = _setup()
    warmer = LookaheadWarmer(fleet)
    warmer.add(0, {"a": 1})
    with pytest.raises(ValueError, match=match):
        call(warmer)


def test_constructor_validation():
    fleet, _ = _setup()
    with pytest.raises(ValueError):
        LookaheadWarmer(fleet, keep_finished=-1)
    with pytest.raises(ValueError):
        LookaheadWarmer(fleet, cap=-1)
    assert LookaheadWarmer(fleet, cap=3).cap == 3


async def test_async_fleet_and_async_calls():
    driver = _PoolDriver()
    fleet = AsyncSandboxFleet(FleetConfig(backend="mock"), driver)
    warmer = LookaheadWarmer(fleet)
    await warmer.add_async(0, {"a": 2}, wait=True)
    assert driver.idle("a") == 2
    await warmer.finish_async(0)
    assert driver.idle("a") == 0
    await warmer.close_async()


def test_set_pool_replicas_and_pool_size_on_the_fleet():
    fleet, driver = _setup()
    assert fleet.pool_size("a") == 0            # never warmed
    fleet.set_pool_replicas("a", 3)
    assert fleet.pool_size("a") == 3
    fleet.set_pool_replicas("a", 1)
    assert fleet.pool_size("a") == 1


def test_base_driver_pool_size_is_unknown():
    class _Bare(MockBackendDriver):
        pool_size = BackendDriver.pool_size

    assert _Bare().pool_size(T, rootfs_image="a") is None


async def test_substrate_pool_size_counts_idle_paused_actors(fake_stack):
    client, fakes = fake_stack
    driver = SubstrateBackendDriver(client=client, atespace="ate-env")
    img = "docker.io/library/python@sha256:" + "d" * 64
    await driver.resize_pool_async("tmpl", 3, rootfs_image=img)
    assert driver.pool_size("tmpl", rootfs_image=img) == 3
    assert len(fakes.environments.environments) == 3

    claimed = await driver.acquire_async("tmpl", run_id="r", rootfs_image=img)
    assert driver.pool_size("tmpl", rootfs_image=img) == 2

    await driver.resize_pool_async("tmpl", 1, rootfs_image=img)
    assert driver.pool_size("tmpl", rootfs_image=img) == 1
    remaining = {aid for (_ns, aid) in fakes.environments.environments}
    assert claimed.instance_id in remaining and len(remaining) == 2

    await driver.resize_pool_async("tmpl", 0, rootfs_image=img)
    assert driver.pool_size("tmpl", rootfs_image=img) == 0
    assert {aid for (_ns, aid) in fakes.environments.environments} == {claimed.instance_id}


async def test_lookahead_on_a_substrate_fleet(fake_stack):
    client, fakes = fake_stack
    cfg = FleetConfig(backend="substrate", endpoint=client.endpoint, template="tmpl",
                      max_warmpool_replicas=8)
    img = "docker.io/library/python@sha256:" + "e" * 64

    def _drive():
        fleet = SandboxFleet(cfg)
        try:
            warmer = LookaheadWarmer(fleet, keep_finished=1)
            warmer.add(0, {img: 4}, wait=True)
            assert fleet.pool_size(img) == 4
            handles = [fleet.acquire(Task(id=f"t{i}", image=img)) for i in range(4)]
            assert fleet.pool_size(img) == 0
            warmer.add(1, {img: 2})
            assert fleet.pool_size(img) == 2
            fleet.release(handles)
            warmer.finish(0)                  # kept (keep_finished=1): nothing drained
            assert fleet.pool_size(img) == 2
            warmer.finish(1)                  # step 0 leaves: -4, only 2 idle
            assert fleet.pool_size(img) == 0
            warmer.close()
        finally:
            fleet.close()
        assert fakes.environments.environments == {}

    await asyncio.to_thread(_drive)
