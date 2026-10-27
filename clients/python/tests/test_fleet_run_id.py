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

import pytest

from ate_env.async_fleet import AsyncSandboxFleet
from ate_env.backend.mock import MockBackendDriver
from ate_env.config import WARM_POOL_RUN_ID, FleetConfig, ValidationError
from ate_env.fleet import SandboxFleet
from ate_env.types import Task


def test_run_id_defaults_to_a_random_id_per_fleet():
    a = SandboxFleet(FleetConfig(backend="mock"))
    b = SandboxFleet(FleetConfig(backend="mock"))
    assert a.run_id and b.run_id
    assert a.run_id != b.run_id


def test_configured_run_id_is_shared_by_every_fleet_of_the_job():
    cfg = FleetConfig(backend="mock", run_id="job-42")
    orchestrator = SandboxFleet(cfg)
    worker = SandboxFleet(FleetConfig.from_dict(cfg.model_dump()))
    assert orchestrator.run_id == worker.run_id == "job-42"


def test_handles_and_teardown_use_the_configured_run_id():
    driver = MockBackendDriver()
    fleet = SandboxFleet(FleetConfig(backend="mock", run_id="job-42"), driver=driver)
    handle = fleet.acquire(Task(id="t", image="img"))
    assert handle.run_id == "job-42"
    assert driver.instances[handle.sandbox_id].run_id == "job-42"
    fleet.teardown()
    assert driver.reaped_runs == {"job-42"}


async def test_async_fleet_uses_the_configured_run_id():
    fleet = AsyncSandboxFleet(FleetConfig(backend="mock", run_id="job-42"))
    handle = await fleet.acquire(Task(id="t", image="img"))
    assert handle.run_id == "job-42"
    await fleet.teardown()


def test_run_report_environment_names_the_run_id():
    fleet = SandboxFleet(FleetConfig(backend="mock", strategy="none", run_id="job-42"))
    fleet.load_tasks([Task(id="t", image="img")])
    fleet.run(lambda task, h: None)
    assert fleet.report.to_dict()["environment"]["mock"]["run_id"] == "job-42"


@pytest.mark.parametrize("run_id", ["", "Job_42", "-job", "x" * 64, 42])
def test_invalid_run_id_is_rejected(run_id):
    with pytest.raises(ValidationError, match="run_id"):
        FleetConfig(run_id=run_id)


def test_the_warm_pool_sentinel_is_reserved():
    # The Substrate backend tags warm actors created without a run id with
    # it, and reap() deletes those for any run.
    with pytest.raises(ValidationError, match="reserved"):
        FleetConfig(run_id=WARM_POOL_RUN_ID)


async def test_warm_pool_environments_carry_the_fleet_run_id(fake_stack):
    # Review of #124: setup() and warm_images() did not pass the fleet's run
    # id, so the Substrate driver tagged the warm actors "warm-pool".
    import asyncio

    client, _fakes = fake_stack
    cfg = FleetConfig(backend="substrate", endpoint=client.endpoint, template="tmpl",
                      max_warmpool_replicas=2, run_id="job-42")
    img_a = "docker.io/library/python@sha256:" + "a" * 64
    img_b = "docker.io/library/python@sha256:" + "b" * 64

    def _drive():
        fleet = SandboxFleet(cfg)
        try:
            fleet.load_tasks([Task(id="t1", image=img_a), Task(id="t2", image=img_a)])
            fleet.setup()
            fleet.warm_images([img_b], replicas=1)
            owned = fleet.backend._owned_actors
            assert len(owned) == 3
            assert {inst.run_id for inst in owned.values()} == {"job-42"}
        finally:
            fleet.close()

    await asyncio.to_thread(_drive)


async def test_async_warm_pool_environments_carry_the_fleet_run_id(fake_stack):
    client, _fakes = fake_stack
    cfg = FleetConfig(backend="substrate", endpoint=client.endpoint, template="tmpl",
                      max_warmpool_replicas=2, run_id="job-42")
    img = "docker.io/library/python@sha256:" + "c" * 64
    fleet = AsyncSandboxFleet(cfg)
    try:
        await fleet.setup([Task(id="t1", image=img), Task(id="t2", image=img)])
        owned = fleet.backend._owned_actors
        assert len(owned) == 2
        assert {inst.run_id for inst in owned.values()} == {"job-42"}
    finally:
        await fleet.teardown()
        fleet.close()
