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

"""set_pool_replicas and warm_image: agent-sandbox-rl's per-image pool sizing,
on the sync and async fleets, against the mock backend and the Substrate
backend over the fake ate-env-api."""

from __future__ import annotations

import pytest

from ate_env import AsyncSandboxFleet, FleetConfig, SandboxFleet
from ate_env._gen.ateenv.v1alpha import env_pb2
from ate_env.backend.mock import MockBackendDriver
from ate_env.backend.substrate import SubstrateBackendDriver
from ate_env.observability import Observer

IMG = "example.com/task@sha256:" + "b" * 64


def test_set_pool_replicas_sizes_up_and_down_on_the_mock():
    with SandboxFleet(FleetConfig(backend="mock", template="base")) as fleet:
        warm = fleet.observer._warm  # this observer's share of the warm-replica gauge
        fleet.set_pool_replicas(IMG, 3)
        assert fleet.backend.warm_pools[("base", IMG)] == 3 and warm["mock"] == 3
        fleet.set_pool_replicas(IMG, 1)
        assert fleet.backend.warm_pools[("base", IMG)] == 1 and warm["mock"] == 1
        fleet.set_pool_replicas(IMG, -4)  # clamped to zero, not an error
        assert fleet.backend.warm_pools[("base", IMG)] == 0 and warm["mock"] == 0


def test_warm_image_is_warm_images_for_one_image():
    with SandboxFleet(FleetConfig(backend="mock", template="base", max_warmpool_replicas=2)) as fleet:
        fleet.warm_image(IMG)
        assert fleet.backend.warm_pools[("base", IMG)] == 2
        fleet.warm_image(IMG, replicas=5)
        assert fleet.backend.warm_pools[("base", IMG)] == 5


class _GrowOnly(MockBackendDriver):
    """A driver without resize_pool: the fleet falls back to warm/unwarm."""

    def resize_pool(self, *a, **k):  # type: ignore[override]
        raise NotImplementedError


def test_set_pool_replicas_falls_back_without_resize_pool():
    fleet = SandboxFleet(FleetConfig(backend="mock", template="base"), driver=_GrowOnly())
    fleet.set_pool_replicas(IMG, 2)
    assert fleet.backend.warm_pools[("base", IMG)] == 2
    fleet.set_pool_replicas(IMG, 0)
    assert ("base", IMG) not in fleet.backend.warm_pools  # unwarm_pool dropped it


async def test_async_fleet_set_pool_replicas_and_warm_image():
    fleet = AsyncSandboxFleet(FleetConfig(backend="mock", template="base", max_warmpool_replicas=4))
    try:
        await fleet.warm_image(IMG)
        assert fleet.backend.warm_pools[("base", IMG)] == 4
        await fleet.set_pool_replicas(IMG, 1)
        assert fleet.backend.warm_pools[("base", IMG)] == 1
    finally:
        fleet.close()


async def test_substrate_resize_pool_deletes_only_surplus_paused_actors(fake_stack):
    client, fakes = fake_stack
    driver = SubstrateBackendDriver(client=client, atespace="ate-env", worker_family="c2")
    driver.observer = Observer()
    warm = driver.observer._warm  # the observer's share of the warm-replica gauge
    await driver.warm_pool_async("tmpl", replicas=3)
    pool = driver._warm_paused_pool
    assert len(pool["tmpl"]) == 3 and warm["substrate"] == 3
    before = set(pool["tmpl"])

    # A claimed actor leaves the pool and is never a resize candidate.
    claimed = await driver.acquire_async("tmpl", run_id="run-1")
    assert claimed.instance_id in before and len(pool["tmpl"]) == 2
    assert warm["substrate"] == 2

    await driver.resize_pool_async("tmpl", 1)
    assert len(pool["tmpl"]) == 1
    assert warm["substrate"] == 1, "a shrink moves the gauge like unwarm_pool does"
    kept = pool["tmpl"][0]
    deleted = before - {claimed.instance_id, kept}
    assert len(deleted) == 1
    for aid in deleted:
        assert ("ate-env", aid) not in fakes.environments.environments
        assert aid not in driver._owned_actors
    assert ("ate-env", claimed.instance_id) in fakes.environments.environments
    assert fakes.environments.environments[("ate-env", kept)].status == env_pb2.ENVIRONMENT_STATUS_SUSPENDED

    # Growing again provisions the difference; zero drains the pool.
    await driver.resize_pool_async("tmpl", 3)
    assert len(pool["tmpl"]) == 3 and warm["substrate"] == 3
    await driver.resize_pool_async("tmpl", 0)
    assert len(pool["tmpl"]) == 0 and warm["substrate"] == 0
    assert ("ate-env", claimed.instance_id) in fakes.environments.environments


@pytest.mark.parametrize("replicas", [2, 0])
async def test_substrate_resize_pool_keys_by_image(fake_stack, replicas):
    client, fakes = fake_stack
    driver = SubstrateBackendDriver(client=client, atespace="ate-env", worker_family="c2")
    await driver.warm_pool_async("base", replicas=2, rootfs_image=IMG)
    await driver.warm_pool_async("base", replicas=1)
    await driver.resize_pool_async("base", replicas, rootfs_image=IMG)
    assert len(driver._warm_paused_pool[("base", IMG)]) == replicas
    assert len(driver._warm_paused_pool["base"]) == 1  # the other pool is untouched
