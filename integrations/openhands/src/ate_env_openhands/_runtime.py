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

"""One background event loop, and one ate-env client per endpoint, for all workspaces.

The OpenHands workspace API is synchronous and ``ate_env.Client`` is asyncio
gRPC. A fleet of workspaces shares one loop thread and one channel per
(endpoint, token) instead of a thread and a channel each. ``close_all``,
registered with ``atexit``, closes the channels and stops the thread.
"""

from __future__ import annotations

import atexit
import concurrent.futures
import os
import threading
from collections import OrderedDict
from collections.abc import Coroutine
from typing import Any

from ate_env import Client
from ate_env.loop_runner import LoopRunner

_lock = threading.Lock()
_runner: LoopRunner | None = None
# Least recently used first. Each token rotation adds a client for the
# endpoint; at most MAX_CLIENTS_PER_ENDPOINT stay open (the current token and
# the previous one) and the least recently used one beyond that is closed.
_clients: OrderedDict[tuple[str, str | None], Client] = OrderedDict()
MAX_CLIENTS_PER_ENDPOINT = 2
# An evicted client can carry a call of a workspace still on its token. Its
# channel refuses new calls at once and gives the calls in progress this long
# to finish; the close ends as soon as they do. Longer than the workspace's
# create and health-check timeouts (600 s by default).
EVICTED_CLIENT_GRACE = 3600.0
# Evicted clients whose graceful close has not finished; close_all ends them.
_retired: set[Client] = set()


def _get_runner() -> LoopRunner:
    global _runner
    with _lock:
        if _runner is None:
            _runner = LoopRunner(name="ate-env-openhands")
        return _runner


def on_loop_thread() -> bool:
    """True when the caller runs on the shared loop's thread, where a blocking
    ``run`` would wait for itself."""
    runner = _runner
    return runner is not None and runner.on_loop_thread()


def submit[T](coro: Coroutine[Any, Any, T]) -> concurrent.futures.Future[T]:
    """Schedule a coroutine on the shared loop without waiting for it."""
    return _get_runner().submit(coro)


def run[T](coro: Coroutine[Any, Any, T], timeout: float | None = None) -> T:
    """Run a coroutine on the shared loop and block for its result.

    With ``timeout``, raise ``TimeoutError`` and cancel the coroutine when it
    does not finish in time. Raises ``RuntimeError`` on the loop thread
    itself, where waiting would deadlock.
    """
    if on_loop_thread():
        coro.close()
        raise RuntimeError(
            "ate-env call made from the shared loop thread; it would deadlock. "
            "Use _runtime.submit() and do not wait for the result."
        )
    future = _get_runner().submit(coro)
    try:
        return future.result(timeout=timeout)
    except concurrent.futures.TimeoutError:
        future.cancel()
        raise TimeoutError(f"ate-env call did not finish within {timeout}s") from None


def client(endpoint: str, token: str | None) -> Client:
    """The shared client for an ate-env-api endpoint and bearer token."""
    key = (endpoint, token)
    with _lock:
        existing = _clients.get(key)
        if existing is not None:
            _clients.move_to_end(key)
    if existing is not None:
        return existing

    async def make() -> Client:
        return Client(endpoint, auth_token=token)

    made = run(make())  # the channel binds to the shared loop
    with _lock:
        kept = _clients.setdefault(key, made)
        _clients.move_to_end(key)
        same = [k for k in _clients if k[0] == endpoint]
        evicted = [_clients.pop(k) for k in same[: max(len(same) - MAX_CLIENTS_PER_ENDPOINT, 0)]]
    if kept is not made:
        # Another thread made a client for this key first: close the spare
        # channel instead of leaking it. No call has used it.
        try:
            run(made.close())
        except Exception:
            pass  # a channel that does not close is dropped
    for old in evicted:
        _retire(old)
    return kept


def _retire(old: Client) -> None:
    """Close an evicted client without cancelling its calls, and do not wait."""
    with _lock:
        _retired.add(old)
    try:
        closing = submit(old.close(grace=EVICTED_CLIENT_GRACE))
    except Exception:
        with _lock:
            _retired.discard(old)
        return  # a channel that does not close is dropped

    def forget(_: concurrent.futures.Future[None]) -> None:
        with _lock:
            _retired.discard(old)

    closing.add_done_callback(forget)


def close_all(timeout: float = 5.0) -> None:
    """Close the shared clients and stop the loop thread (idempotent).

    Registered with ``atexit``. A later call to ``run`` or ``client`` starts a
    new loop thread.
    """
    global _runner
    with _lock:
        runner, clients = _runner, list(_clients.values()) + list(_retired)
        _runner = None
        _clients.clear()
        _retired.clear()
    if runner is None:
        return
    for c in clients:
        try:
            runner.run(c.close(), timeout=timeout)
        except Exception:
            pass  # shutting down: a channel that does not close is dropped
    runner.stop()


def _forget_after_fork() -> None:
    # A forked child has no loop thread: the parent's runner and the channels
    # bound to it are unusable there. Start fresh on first use.
    global _lock, _runner
    _lock = threading.Lock()
    _runner = None
    _clients.clear()
    _retired.clear()


atexit.register(close_all)
if hasattr(os, "register_at_fork"):
    os.register_at_fork(after_in_child=_forget_after_fork)
