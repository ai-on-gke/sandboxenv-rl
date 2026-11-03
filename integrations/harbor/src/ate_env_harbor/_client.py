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

"""One ``ate_env.Client`` per (endpoint, token) per event loop.

Harbor runs every trial of a job as an asyncio task on one loop, thus a job
at ``-n 50`` shares one gRPC channel instead of opening fifty. The channel
is bound to the loop it was created on, which is why the loop is part of
the key. ``close_all`` is registered with ``atexit`` and closes what it can.
"""

from __future__ import annotations

import asyncio
import atexit
import contextlib

from ate_env import Client

_clients: dict[tuple[str, str | None, int], tuple[Client, asyncio.AbstractEventLoop]] = {}


def shared_client(endpoint: str, token: str | None) -> Client:
    """The shared client for ``endpoint`` and ``token`` on the running loop."""
    loop = asyncio.get_running_loop()
    # Loops come and go (asyncio.run per preflight, per test); drop the
    # entries of closed ones so the cache does not grow with them.
    for stale in [k for k, (_, bound) in _clients.items() if bound.is_closed()]:
        del _clients[stale]
    key = (endpoint, token, id(loop))
    entry = _clients.get(key)
    if entry is None:
        entry = (Client(endpoint, auth_token=token), loop)
        _clients[key] = entry
    return entry[0]


async def close_current_loop_clients() -> None:
    """Close the clients bound to the running loop (tests call this)."""
    loop = asyncio.get_running_loop()
    for key in [k for k, (_, bound) in _clients.items() if bound is loop]:
        client, _ = _clients.pop(key)
        with contextlib.suppress(Exception):
            await client.close()


def close_all() -> None:
    """Best effort at interpreter exit: close channels whose loop still runs tasks."""
    for client, loop in list(_clients.values()):
        if loop.is_closed() or loop.is_running():
            continue
        with contextlib.suppress(Exception):
            loop.run_until_complete(client.close())
    _clients.clear()


atexit.register(close_all)
