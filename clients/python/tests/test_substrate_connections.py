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

"""SubstrateBackendDriver spreads actors over several gRPC connections."""

from __future__ import annotations

import grpc
import grpc.aio
import pytest

from ate_env import AsyncSandboxFleet, FleetConfig, Task
from ate_env._gen.ateenv.v1alpha import env_pb2_grpc
from ate_env.backend.substrate import SubstrateBackendDriver
from ate_env.config import ValidationError

from .fakes import FakeEnvironmentService


class _PeerRecordingEnvironments(FakeEnvironmentService):
    """Records the client address (one per TCP connection) of every call."""

    def __init__(self):
        super().__init__()
        self.peers: dict[str, set[str]] = {}

    def _record(self, request, context):
        self.peers.setdefault(request.id, set()).add(context.peer())

    async def CreateEnvironment(self, request, context):
        self._record(request, context)
        return await super().CreateEnvironment(request, context)

    async def SuspendEnvironment(self, request, context):
        self._record(request, context)
        return await super().SuspendEnvironment(request, context)

    async def DeleteEnvironment(self, request, context):
        self._record(request, context)
        return await super().DeleteEnvironment(request, context)


@pytest.fixture
async def recording_server():
    server = grpc.aio.server()
    envs = _PeerRecordingEnvironments()
    env_pb2_grpc.add_EnvironmentServiceServicer_to_server(envs, server)
    port = server.add_insecure_port("127.0.0.1:0")
    await server.start()
    try:
        yield f"127.0.0.1:{port}", envs
    finally:
        await server.stop(None)


async def test_actors_spread_over_separate_connections(recording_server):
    endpoint, envs = recording_server
    driver = SubstrateBackendDriver(api_endpoint=endpoint, connections=3)
    try:
        instances = [await driver.acquire_async("tmpl", run_id="r") for _ in range(24)]
        # Every call for one actor uses the same connection ...
        for inst in instances:
            assert driver._client_for(inst.instance_id) is driver._client_for(inst.instance_id)
            await driver.release_async(inst.instance_id, recycle=True)
            await driver.release_async(inst.instance_id)
            assert len(envs.peers[inst.instance_id]) == 1
        # ... and the actors use three distinct TCP connections between them.
        peers = set().union(*envs.peers.values())
        assert len(peers) == 3
        clients = {id(driver._client_for(i.instance_id)) for i in instances}
        assert len(clients) == 3
    finally:
        await driver.close_async()
    assert driver._client is None and driver._extra_clients == []


async def test_runtime_uses_the_actors_client(recording_server):
    endpoint, _envs = recording_server
    driver = SubstrateBackendDriver(api_endpoint=endpoint, connections=4)
    try:
        inst = await driver.acquire_async("tmpl", run_id="r")
        runtime, _ = driver.build_runtime(inst)
        assert runtime._client is driver._client_for(inst.instance_id)
    finally:
        await driver.close_async()


async def test_single_connection_keeps_one_client(recording_server):
    endpoint, envs = recording_server
    driver = SubstrateBackendDriver(api_endpoint=endpoint)
    try:
        for _ in range(8):
            inst = await driver.acquire_async("tmpl", run_id="r")
            assert driver._client_for(inst.instance_id) is driver.client
            await driver.release_async(inst.instance_id)
        assert driver._extra_clients == []
        assert len(set().union(*envs.peers.values())) == 1
    finally:
        await driver.close_async()


async def test_caller_client_ignores_connections(fake_stack):
    client, _fakes = fake_stack
    driver = SubstrateBackendDriver(client=client, connections=4)
    assert driver.connections == 1
    inst = await driver.acquire_async("tmpl", run_id="r")
    assert driver._client_for(inst.instance_id) is client
    await driver.release_async(inst.instance_id)


def test_connections_validation():
    with pytest.raises(ValueError):
        SubstrateBackendDriver(connections=0)
    with pytest.raises(ValidationError):
        FleetConfig(connections=0)
    with pytest.raises(ValidationError):
        FleetConfig(connections=True)


@pytest.mark.parametrize(
    ("max_concurrent", "connections", "expected"),
    [(64, None, 1), (200, None, 1), (201, None, 2), (512, None, 3), (512, 1, 1), (8, 5, 5)],
)
def test_fleet_sizes_the_pool(max_concurrent, connections, expected):
    fleet = AsyncSandboxFleet(
        FleetConfig(max_concurrent=max_concurrent, connections=connections)
    )
    try:
        assert fleet.backend.connections == expected
    finally:
        fleet.close()


async def test_fleet_runs_over_the_pool(recording_server):
    endpoint, envs = recording_server
    fleet = AsyncSandboxFleet(FleetConfig(endpoint=endpoint, template="tmpl", connections=2))
    try:
        img = "docker.io/library/python@sha256:" + "e" * 64
        sandboxes = [await fleet.acquire(Task(id=f"t{i}", image=img)) for i in range(12)]
        for sb in sandboxes:
            await fleet.release(sb)
    finally:
        fleet.close()
    assert len(set().union(*envs.peers.values())) == 2


def test_concurrent_first_client_access_creates_one_client():
    import threading
    import time

    driver = SubstrateBackendDriver(api_endpoint="127.0.0.1:1", connections=4)
    created = []

    def slow_new_client():
        time.sleep(0.01)  # widen the window between the check and the set
        client = object()
        created.append(client)
        return client

    driver._new_client = slow_new_client
    start = threading.Barrier(16)
    got = []

    def worker():
        start.wait()
        got.append(driver.client)

    threads = [threading.Thread(target=worker) for _ in range(16)]
    for t in threads:
        t.start()
    for t in threads:
        t.join()
    assert len(created) == 1
    assert all(c is created[0] for c in got)


class _ClosingClient:
    def __init__(self, tracker, fail=False):
        self.tracker = tracker
        self.fail = fail
        self.closed = False

    async def close(self):
        import asyncio

        self.tracker["in_flight"] += 1
        self.tracker["max_in_flight"] = max(self.tracker["max_in_flight"], self.tracker["in_flight"])
        await asyncio.sleep(0.02)
        self.tracker["in_flight"] -= 1
        self.closed = True
        if self.fail:
            raise RuntimeError("close failed")


def _driver_with_clients(n, fail_index=None):
    tracker = {"in_flight": 0, "max_in_flight": 0}
    driver = SubstrateBackendDriver(api_endpoint="127.0.0.1:1", connections=n)
    clients = [_ClosingClient(tracker, fail=(i == fail_index)) for i in range(n)]
    driver._client, driver._extra_clients = clients[0], clients[1:]
    return driver, clients, tracker


async def test_close_async_closes_pooled_clients_concurrently():
    driver, clients, tracker = _driver_with_clients(4)
    await driver.close_async()
    assert all(c.closed for c in clients)
    assert tracker["max_in_flight"] == 4
    assert driver._client is None and driver._extra_clients == []


async def test_close_async_closes_every_client_when_one_fails():
    driver, clients, _ = _driver_with_clients(4, fail_index=1)
    with pytest.raises(RuntimeError, match="close failed"):
        await driver.close_async()
    assert all(c.closed for c in clients)


def test_close_closes_pooled_clients_concurrently():
    from ate_env.loop_runner import LoopRunner

    driver, clients, tracker = _driver_with_clients(4)
    driver._loop_runner = LoopRunner(name="test-close")
    driver._owns_loop_runner = True
    driver.close()
    assert all(c.closed for c in clients)
    assert tracker["max_in_flight"] == 4
