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

"""The shared client cache in _runtime."""

from __future__ import annotations

import asyncio
import threading

import pytest

from ate_env_openhands import _runtime


class _FakeClient:
    made: list[_FakeClient] = []

    def __init__(self, endpoint, auth_token=None):
        self.closed = False
        _FakeClient.made.append(self)

    async def close(self):
        self.closed = True


def test_a_client_that_loses_the_race_is_closed(monkeypatch):
    key = ("localhost:1", "tok")
    winner = _FakeClient("localhost:1")
    monkeypatch.setattr(_runtime, "_clients", {})

    class Racing(_FakeClient):
        def __init__(self, endpoint, auth_token=None):
            super().__init__(endpoint, auth_token)
            # Another thread stores its client while this one is being made.
            _runtime._clients[key] = winner

    monkeypatch.setattr(_runtime, "Client", Racing)
    got = _runtime.client(*key)
    assert got is winner
    loser = _FakeClient.made[-1]
    assert loser is not winner and loser.closed
    assert not winner.closed


def test_the_same_key_reuses_one_client(monkeypatch):
    monkeypatch.setattr(_runtime, "_clients", {})
    monkeypatch.setattr(_runtime, "Client", _FakeClient)
    first = _runtime.client("localhost:2", None)
    assert _runtime.client("localhost:2", None) is first
    assert not first.closed


@pytest.fixture
def own_runtime(monkeypatch):
    """A private runner, lock and client cache, so the shared ones stay intact."""
    monkeypatch.setattr(_runtime, "_runner", None)
    monkeypatch.setattr(_runtime, "_lock", threading.Lock())
    monkeypatch.setattr(_runtime, "_clients", {})
    yield
    if _runtime._runner is not None:
        _runtime._runner.stop()


def test_close_all_closes_the_clients_and_stops_the_loop(own_runtime, monkeypatch):
    monkeypatch.setattr(_runtime, "Client", _FakeClient)
    c = _runtime.client("localhost:3", None)
    runner = _runtime._runner
    assert runner is not None
    _runtime.close_all()
    assert c.closed
    assert _runtime._runner is None and _runtime._clients == {}
    assert not runner._thread.is_alive()
    _runtime.close_all()  # idempotent
    assert _runtime.run(asyncio.sleep(0, result="again")) == "again", "a new loop starts on use"


def test_run_times_out_and_cancels():
    started = asyncio.Event()
    cancelled: list[bool] = []

    async def slow():
        started.set()
        try:
            await asyncio.sleep(30)
        except asyncio.CancelledError:
            cancelled.append(True)
            raise

    with pytest.raises(TimeoutError):
        _runtime.run(slow(), timeout=0.2)
    _runtime.run(asyncio.sleep(0.1))  # let the cancellation run on the loop
    assert cancelled == [True]


def test_a_forked_child_starts_without_the_parents_loop(own_runtime):
    _runtime._clients[("x", None)] = object()
    _runtime._get_runner()
    _runtime._forget_after_fork()  # what os.register_at_fork runs in the child
    assert _runtime._runner is None and _runtime._clients == {}
