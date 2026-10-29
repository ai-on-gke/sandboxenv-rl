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

import threading
import time

import pytest

from ate_env.async_fleet import AsyncSandboxFleet
from ate_env.backend.mock import MockBackendDriver
from ate_env.config import FleetConfig, ValidationError
from ate_env.fleet import SandboxFleet
from ate_env.sizing import compute_replicas, plan_replicas
from ate_env.types import Task


@pytest.mark.parametrize("args, kwargs, want", [
    # An image's share of the concurrency budget, clamped to [1, min(tasks, cap)].
    ((10, 100, 64, 32), {}, 6),
    ((10, 100, 1, 32), {}, 1),      # never below one replica
    ((10, 100, 640, 32), {}, 10),   # never above the task count
    ((50, 100, 640, 32), {}, 32),   # never above the cap
    ((10, 100, 64, 32), {"per_task": True}, 10),
    ((50, 100, 64, 32), {"per_task": True}, 32),
    ((0, 100, 64, 32), {}, 0),      # no tasks, no pool
    ((10, 100, 64, 0), {}, 0),      # max_pool 0: claims start from the template
])
def test_compute_replicas(args, kwargs, want):
    assert compute_replicas(*args, **kwargs) == want


def test_plan_replicas_shares_the_budget_among_selected_images():
    counts = {"a": 8, "b": 8, "c": 8, "d": 8}
    assert plan_replicas(counts, 8, 32) == {"a": 2, "b": 2, "c": 2, "d": 2}
    # A window of two images serves the same 8 claims, so each gets twice the depth.
    assert plan_replicas(counts, 8, 32, images=["a", "b"]) == {"a": 4, "b": 4}


def test_plan_sizes_pools_by_concurrency_not_task_count():
    tasks = [Task(id=f"t{i}", image=f"img-{i % 4}") for i in range(40)]  # 10 per image
    fleet = SandboxFleet(FleetConfig(backend="mock", max_concurrent=8, max_warmpool_replicas=16))
    fleet.load_tasks(tasks)
    plan = fleet.plan()
    assert {e.image: e.replicas for e in plan.entries} == {f"img-{i}": 2 for i in range(4)}
    assert plan.total_replicas == 8


def test_warm_per_task_gives_every_task_a_replica_up_to_the_cap():
    tasks = [Task(id=f"t{i}", image=f"img-{i % 4}") for i in range(40)]
    fleet = SandboxFleet(FleetConfig(backend="mock", max_concurrent=8,
                                     max_warmpool_replicas=6, warm_per_task=True))
    fleet.load_tasks(tasks)
    assert {e.replicas for e in fleet.plan().entries} == {6}


def test_warm_images_sizes_a_window_by_its_tasks():
    driver = MockBackendDriver()
    tasks = [Task(id="a1", image="a")] + [Task(id=f"b{i}", image="b") for i in range(3)]
    fleet = SandboxFleet(FleetConfig(backend="mock", max_concurrent=4, max_warmpool_replicas=8),
                         driver=driver)
    fleet.load_tasks(tasks)
    fleet.warm_images(["a", "b"])
    # Before, every image of a window got max_warmpool_replicas (8) whatever its tasks.
    assert driver.warm_pools[("default-template", "a")] == 1
    assert driver.warm_pools[("default-template", "b")] == 3


def test_warm_images_explicit_replicas_and_unknown_images():
    driver = MockBackendDriver()
    fleet = SandboxFleet(FleetConfig(backend="mock", max_warmpool_replicas=3), driver=driver)
    fleet.load_tasks([Task(id="a1", image="a")])
    fleet.warm_images(["unplanned"])  # no task to size by: the cap
    assert driver.warm_pools[("default-template", "unplanned")] == 3
    fleet.warm_images(["a"], replicas=5)
    assert driver.warm_pools[("default-template", "a")] == 5


class _SlowDriver(MockBackendDriver):
    """Records how many warm_pool calls overlap; fails for chosen templates."""

    def __init__(self, fail: tuple[str, ...] = ()):
        super().__init__()
        self.fail = fail
        self.active = 0
        self.peak = 0
        self.warmed: list[str] = []
        self._mu = threading.Lock()

    def warm_pool(self, template_id, replicas, wait=True, run_id=None, rootfs_image=None):
        with self._mu:
            self.active += 1
            self.peak = max(self.peak, self.active)
        try:
            time.sleep(0.05)
            if rootfs_image in self.fail:
                raise RuntimeError(f"cannot warm {rootfs_image}")
            super().warm_pool(template_id, replicas, wait, run_id, rootfs_image)
            with self._mu:
                self.warmed.append(rootfs_image)
        finally:
            with self._mu:
                self.active -= 1


def test_setup_warms_images_in_parallel_up_to_warm_parallelism():
    driver = _SlowDriver()
    fleet = SandboxFleet(FleetConfig(backend="mock", warm_parallelism=3), driver=driver)
    fleet.setup([Task(id=f"t{i}", image=f"img-{i}") for i in range(8)])
    assert sorted(driver.warmed) == sorted(f"img-{i}" for i in range(8))
    assert driver.peak == 3


def test_warm_parallelism_one_warms_serially():
    driver = _SlowDriver()
    fleet = SandboxFleet(FleetConfig(backend="mock", warm_parallelism=1), driver=driver)
    fleet.load_tasks([Task(id=f"t{i}", image=f"img-{i}") for i in range(3)])
    fleet.warm_images([f"img-{i}" for i in range(3)])
    assert driver.peak == 1


def test_a_failed_warm_is_raised_after_every_image_was_tried():
    driver = _SlowDriver(fail=("img-1",))
    fleet = SandboxFleet(FleetConfig(backend="mock"), driver=driver)
    fleet.load_tasks([Task(id=f"t{i}", image=f"img-{i}") for i in range(4)])
    with pytest.raises(RuntimeError, match="img-1"):
        fleet.warm_images([f"img-{i}" for i in range(4)])
    assert sorted(driver.warmed) == ["img-0", "img-2", "img-3"]


async def test_async_setup_warms_in_parallel_and_sizes_like_the_sync_fleet():
    driver = _SlowDriver()
    cfg = FleetConfig(backend="mock", max_concurrent=8, max_warmpool_replicas=16,
                      warm_parallelism=4)
    fleet = AsyncSandboxFleet(cfg, driver)
    await fleet.setup([Task(id=f"t{i}", image=f"img-{i % 8}") for i in range(80)])
    assert driver.peak == 4
    assert {driver.warm_pools[("default-template", f"img-{i}")] for i in range(8)} == {1}


async def test_async_warm_images_raises_after_every_image_was_tried():
    driver = _SlowDriver(fail=("img-0",))
    fleet = AsyncSandboxFleet(FleetConfig(backend="mock"), driver)
    fleet.load_tasks([Task(id=f"t{i}", image=f"img-{i}") for i in range(3)])
    with pytest.raises(RuntimeError, match="img-0"):
        await fleet.warm_images([f"img-{i}" for i in range(3)])
    assert sorted(driver.warmed) == ["img-1", "img-2"]


@pytest.mark.parametrize("kwargs", [
    {"warm_parallelism": 0},
    {"warm_parallelism": True},
    {"warm_per_task": "yes"},
])
def test_invalid_warm_settings_are_rejected(kwargs):
    with pytest.raises(ValidationError):
        FleetConfig(**kwargs)
