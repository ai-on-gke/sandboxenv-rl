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
from collections import OrderedDict

import pytest

from ate_env_openhands import _runtime

from .fakes import DEFAULT_ATESPACE, FakeAteEnv, serve


class _FakeClient:
    made: list[_FakeClient] = []

    def __init__(self, endpoint, auth_token=None):
        self.closed = False
        self.grace: float | None = None
        _FakeClient.made.append(self)

    async def close(self, grace=None):
        self.closed = True
        self.grace = grace


def _let_closes_run():
    """Wait for the closes that client() scheduled on the loop before now."""
    _runtime.run(asyncio.sleep(0))


def test_a_client_that_loses_the_race_is_closed(monkeypatch):
    key = ("localhost:1", "tok")
    winner = _FakeClient("localhost:1")
    monkeypatch.setattr(_runtime, "_clients", OrderedDict())

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
    monkeypatch.setattr(_runtime, "_clients", OrderedDict())
    monkeypatch.setattr(_runtime, "Client", _FakeClient)
    first = _runtime.client("localhost:2", None)
    assert _runtime.client("localhost:2", None) is first
    assert not first.closed


@pytest.fixture
def own_runtime(monkeypatch):
    """A private runner, lock and client cache, so the shared ones stay intact."""
    monkeypatch.setattr(_runtime, "_runner", None)
    monkeypatch.setattr(_runtime, "_lock", threading.Lock())
    monkeypatch.setattr(_runtime, "_clients", OrderedDict())
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


def test_run_refuses_the_loop_thread_and_submit_does_not(own_runtime):
    seen: dict[str, object] = {}

    async def on_loop():
        seen["on_loop_thread"] = _runtime.on_loop_thread()
        try:
            _runtime.run(asyncio.sleep(0))
        except RuntimeError as e:
            seen["error"] = str(e)
        seen["future"] = _runtime.submit(asyncio.sleep(0, result="later"))

    assert not _runtime.on_loop_thread()
    _runtime.run(on_loop(), timeout=5)
    assert seen["on_loop_thread"] is True
    assert "loop thread" in str(seen["error"])
    assert seen["future"].result(timeout=5) == "later"  # scheduled, finished by the loop


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


def test_token_rotation_keeps_two_clients_per_endpoint(monkeypatch):
    monkeypatch.setattr(_runtime, "_clients", OrderedDict())
    monkeypatch.setattr(_runtime, "Client", _FakeClient)
    t1 = _runtime.client("localhost:4", "t1")
    t2 = _runtime.client("localhost:4", "t2")
    other = _runtime.client("localhost:5", "t1")
    assert not t1.closed and not t2.closed, "the current and the previous token stay open"
    t3 = _runtime.client("localhost:4", "t3")
    _let_closes_run()
    assert t1.closed, "the least recently used client of the endpoint is closed"
    assert t1.grace == _runtime.EVICTED_CLIENT_GRACE, "its calls in progress can finish"
    assert not t2.closed and not t3.closed and not other.closed
    assert set(_runtime._clients) == {("localhost:4", "t2"), ("localhost:4", "t3"),
                                      ("localhost:5", "t1")}


def test_a_used_client_is_not_the_one_evicted(monkeypatch):
    monkeypatch.setattr(_runtime, "_clients", OrderedDict())
    monkeypatch.setattr(_runtime, "Client", _FakeClient)
    t1 = _runtime.client("localhost:6", "t1")
    t2 = _runtime.client("localhost:6", "t2")
    assert _runtime.client("localhost:6", "t1") is t1  # t1 is now the most recent
    _runtime.client("localhost:6", "t3")
    _let_closes_run()
    assert t2.closed and not t1.closed


def test_eviction_lets_a_call_in_progress_finish(own_runtime):
    """A workspace on the oldest token is in a call when two newer tokens
    arrive. Its client is evicted; the call must still return."""
    state = FakeAteEnv(get_delay=1.0)
    state.envs["e"] = {"atespace": DEFAULT_ATESPACE, "template": "t",
                       "template_atespace": DEFAULT_ATESPACE, "status": 1}
    with serve(state) as (api_url, _):
        oldest = _runtime.client(api_url, "t1")
        call = _runtime.submit(oldest.get("e", atespace=DEFAULT_ATESPACE))
        for _ in range(500):
            if state.gets:
                break
            threading.Event().wait(0.01)
        assert state.gets == 1, "the call is in progress"
        _runtime.client(api_url, "t2")
        _runtime.client(api_url, "t3")  # evicts t1; does not wait for its close
        assert (api_url, "t1") not in _runtime._clients
        assert oldest in _runtime._retired
        assert call.result(timeout=5).id == "e"
        for _ in range(500):  # the close ends when the call does
            if oldest not in _runtime._retired:
                break
            threading.Event().wait(0.01)
        assert oldest not in _runtime._retired
        with pytest.raises(Exception):
            _runtime.run(oldest.get("e", atespace=DEFAULT_ATESPACE), timeout=5)


def test_close_all_ends_a_graceful_close_in_progress(own_runtime):
    state = FakeAteEnv(get_delay=30.0)
    state.envs["e"] = {"atespace": DEFAULT_ATESPACE, "template": "t",
                       "template_atespace": DEFAULT_ATESPACE, "status": 1}
    with serve(state) as (api_url, _):
        oldest = _runtime.client(api_url, "t1")
        call = _runtime.submit(oldest.get("e", atespace=DEFAULT_ATESPACE))
        for _ in range(500):
            if state.gets:
                break
            threading.Event().wait(0.01)
        _runtime.client(api_url, "t2")
        _runtime.client(api_url, "t3")
        assert oldest in _runtime._retired
        _runtime.close_all(timeout=5)
        assert not _runtime._retired
        with pytest.raises(Exception):
            call.result(timeout=5)  # cancelled by the shutdown, not left hanging
