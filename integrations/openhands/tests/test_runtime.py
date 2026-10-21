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
