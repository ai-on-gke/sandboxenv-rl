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

"""A failed delete on release is recorded, not swallowed.

``release`` keeps not raising (cleanup must not crash a trainer), but the
failure reaches the RunReport, the Prometheus series, the release span and
the run's reap, which retries the delete and raises if it still fails.
"""

from __future__ import annotations

import pytest

from ate_env import AsyncSandboxFleet, FleetConfig, Task
from ate_env.backend.substrate import SubstrateBackendDriver
from ate_env.config import ObservabilityConfig
from ate_env.errors import FailedPreconditionError, InfrastructureError, NotFoundError
from ate_env.observability import Observer, RunReport

IMG = "docker.io/library/python@sha256:" + "c" * 64


def _failing_delete(client, exc):
    real = client.delete
    calls: list[str] = []

    async def delete(id, *, atespace="ate-env"):
        calls.append(id)
        if exc is not None:
            raise exc
        return await real(id, atespace=atespace)

    client.delete = delete
    return calls, real


async def _driver_with_actor(client):
    driver = SubstrateBackendDriver(client=client, atespace="ate-env", worker_family="c2")
    obs = Observer(ObservabilityConfig(enable_metrics=False))
    driver.observer = obs
    inst = await driver.acquire_async("tmpl-x", run_id="run-1")
    return driver, obs, inst


async def test_failed_delete_does_not_raise_and_is_counted(fake_stack):
    client, fakes = fake_stack
    driver, obs, inst = await _driver_with_actor(client)
    _failing_delete(client, FailedPreconditionError("ReleaseExternalSnapshots: 403"))

    with obs.run("t") as report:
        await driver.release_async(inst.instance_id)  # must not raise

    assert report.release_errors == 1
    assert report.to_dict()["release_errors"] == 1
    assert "release_errors=1" in report.summary()
    # Still owned, so the run's reap tries again.
    assert inst.instance_id in driver._owned_actors


async def test_reap_retries_a_failed_release_and_raises_if_it_still_fails(fake_stack):
    client, fakes = fake_stack
    driver, _, inst = await _driver_with_actor(client)
    calls, _ = _failing_delete(client, FailedPreconditionError("403"))

    await driver.release_async(inst.instance_id)
    with pytest.raises(InfrastructureError, match="Failed to delete 1 environments"):
        await driver.reap_async("run-1")
    assert calls == [inst.instance_id, inst.instance_id]


async def test_reap_cleans_up_after_a_transient_release_failure(fake_stack):
    client, fakes = fake_stack
    driver, _, inst = await _driver_with_actor(client)
    _, real = _failing_delete(client, FailedPreconditionError("transient"))
    await driver.release_async(inst.instance_id)

    client.delete = real  # the cause is fixed before teardown
    assert await driver.reap_async("run-1") == 1
    assert ("ate-env", inst.instance_id) not in fakes.environments.environments
    assert inst.instance_id not in driver._owned_actors


async def test_not_found_on_delete_counts_as_released(fake_stack):
    client, fakes = fake_stack
    driver, obs, inst = await _driver_with_actor(client)
    _failing_delete(client, NotFoundError("environment already gone"))

    with obs.run("t") as report:
        await driver.release_async(inst.instance_id)
    assert report.release_errors == 0
    assert inst.instance_id not in driver._owned_actors
    assert await driver.reap_async("run-1") == 0


async def test_successful_release_records_no_error(fake_stack):
    client, fakes = fake_stack
    driver, obs, inst = await _driver_with_actor(client)
    with obs.run("t") as report:
        await driver.release_async(inst.instance_id)
    assert report.release_errors == 0
    assert "release_errors" not in report.summary()
    assert ("ate-env", inst.instance_id) not in fakes.environments.environments


async def test_fleet_run_report_and_metric_show_failed_releases(fake_stack):
    prom = pytest.importorskip("prometheus_client")
    client, fakes = fake_stack
    cfg = FleetConfig(backend="substrate", endpoint=client.endpoint, template="tmpl-test",
                      observability=ObservabilityConfig(enable_metrics=True))
    fleet = AsyncSandboxFleet(cfg)
    fleet.load_tasks([Task(id=f"t{i}", image=IMG) for i in range(3)])
    before = prom.REGISTRY.get_sample_value(
        "ate_env_fleet_release_failures_total", {"backend": "substrate"}) or 0.0
    _failing_delete(fleet.backend.client, FailedPreconditionError("403"))
    try:
        await fleet.run(lambda task, handle: None)
        assert fleet.report.release_errors == 3
        assert fleet.report.tasks_ok == 3  # the work itself succeeded
        after = prom.REGISTRY.get_sample_value(
            "ate_env_fleet_release_failures_total", {"backend": "substrate"})
        assert after - before == 3
        with pytest.raises(InfrastructureError):
            await fleet.teardown()
    finally:
        fleet.close()


async def test_release_span_is_marked_error(fake_stack):
    pytest.importorskip("opentelemetry.sdk")
    from opentelemetry import trace
    from opentelemetry.sdk.trace import TracerProvider
    from opentelemetry.sdk.trace.export import SimpleSpanProcessor
    from opentelemetry.sdk.trace.export.in_memory_span_exporter import (
        InMemorySpanExporter,
    )

    # A local provider: never install the global one, which other tests own.
    provider = TracerProvider()
    exporter = InMemorySpanExporter()
    provider.add_span_processor(SimpleSpanProcessor(exporter))

    client, _ = fake_stack
    driver = SubstrateBackendDriver(client=client, atespace="ate-env", worker_family="c2")
    driver.tracer = provider.get_tracer("test")
    inst = await driver.acquire_async("tmpl-x", run_id="run-1")
    _failing_delete(client, FailedPreconditionError("gs://bucket/secret-path 403"))
    exporter.clear()
    await driver.release_async(inst.instance_id)

    release = [s for s in exporter.get_finished_spans() if s.name == "ate_env_client.release"]
    assert len(release) == 1
    assert release[0].status.status_code == trace.StatusCode.ERROR
    assert release[0].attributes["error.type"] == "FailedPreconditionError"
    assert "secret-path" not in (release[0].status.description or "")


def test_run_report_defaults_have_no_release_errors():
    r = RunReport("s")
    assert r.to_dict()["release_errors"] == 0


async def test_cancelled_release_propagates_and_keeps_the_environment_owned(fake_stack):
    import asyncio

    client, _fakes = fake_stack
    driver, obs, inst = await _driver_with_actor(client)
    _failing_delete(client, asyncio.CancelledError())
    with pytest.raises(asyncio.CancelledError):
        with obs.run("t") as report:
            await driver.release_async(inst.instance_id)
    # Not recorded as a release failure, and still owned so reap retries it.
    assert report.release_errors == 0
    assert inst.instance_id in driver._owned_actors
