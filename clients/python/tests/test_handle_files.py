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

"""SandboxHandle file helpers: the sync variants mirror the async ones."""

import pytest

from ate_env import FleetConfig, SandboxFleet, Task


def _handle():
    return SandboxFleet(FleetConfig(backend="mock")).acquire(Task(id="t", image="img"))


def test_sync_write_then_read_round_trips():
    h = _handle()
    h.write_file("/tmp/a.txt", "hello")
    assert h.read_file_bytes("/tmp/a.txt") == b"hello"
    h.write_file("/tmp/b.bin", b"\x00\x01")
    assert h.read_file_bytes("/tmp/b.bin") == b"\x00\x01"
    h.release()


@pytest.mark.asyncio
async def test_sync_and_async_helpers_share_the_sandbox_filesystem():
    h = _handle()
    h.write_file("/tmp/x", "sync")
    assert await h.read_file_bytes_async("/tmp/x") == b"sync"
    await h.write_file_async("/tmp/y", b"async")
    assert h.read_file_bytes("/tmp/y") == b"async"
    h.release()
