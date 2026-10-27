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

"""make_handle_workspace and make_fleet_workspace: the workspace binds a
sandbox the fleet owns. A mock-backend fleet hands out handles; the fake
ate-env-api knows the same environment ids, so the workspace attaches to them
and reaches the real agent-server through the fake port proxy."""

from __future__ import annotations

from types import SimpleNamespace

import pytest
from ate_env import FleetConfig, SandboxFleet, Task

from ate_env_openhands import (
    AteEnvWorkspace,
    make_fleet_workspace,
    make_handle_workspace,
)
from ate_env_openhands.fleet import FleetWorkspace, _defaults_from_handle

from .conftest import IMAGE, SESSION_KEY
from .fakes import DEFAULT_ATESPACE

TASK = Task(id="t1", image=IMAGE)


class _RegisteringFleet(SandboxFleet):
    """A mock-backend fleet whose acquired environments also exist in the fake
    ate-env-api, the way a Substrate fleet's would."""

    def __init__(self, state):
        super().__init__(FleetConfig(backend="mock", template="oh-agent-server"))
        self._state = state
        self.released: list[str] = []

    def acquire(self, task, timeout_s=None):  # type: ignore[override]
        handle = super().acquire(task, timeout_s=timeout_s)
        self._state.envs[handle.sandbox_id] = {
            "atespace": DEFAULT_ATESPACE, "template": "oh-agent-server",
            "template_atespace": DEFAULT_ATESPACE, "status": 2,  # RUNNING
        }
        return handle

    def release(self, handle):  # type: ignore[override]
        self.released.append(handle.sandbox_id)
        super().release(handle)


def kw(api_url, state, tmp_path, **extra):
    args = dict(api_url=api_url, proxy_url=state.proxy_url, api_key=SESSION_KEY,
                working_dir=str(tmp_path / "work"), health_check_timeout=15)
    args.update(extra)
    return args


def test_handle_workspace_binds_without_creating_or_deleting(stack, tmp_path):
    api_url, state = stack
    fleet = _RegisteringFleet(state)
    handle = fleet.acquire(TASK)
    ws = make_handle_workspace(handle, **kw(api_url, state, tmp_path))
    try:
        assert isinstance(ws, AteEnvWorkspace) and ws.env_id == handle.sandbox_id
        assert state.creates == []
        result = ws.execute_command("echo bound", timeout=30)
        assert (result.exit_code, result.stdout.strip()) == (0, "bound")
    finally:
        ws.cleanup()
    assert state.deletes == [] and handle.sandbox_id in state.envs
    assert fleet.released == []  # the caller (or fleet.run) releases, not the workspace
    fleet.release(handle)
    assert fleet.released == [handle.sandbox_id]


def test_handle_workspace_inside_fleet_run(stack, tmp_path):
    api_url, state = stack
    fleet = _RegisteringFleet(state)
    fleet.load_tasks([TASK, Task(id="t2", image=IMAGE)])
    seen: list[str] = []

    def rollout(task, handle):
        ws = make_handle_workspace(handle, **kw(api_url, state, tmp_path / task.id))
        try:
            seen.append(ws.execute_command(f"echo {task.id}", timeout=30).stdout.strip())
        finally:
            ws.cleanup()
        return task.id

    assert sorted(fleet.run(rollout, concurrency=2)) == ["t1", "t2"]
    assert sorted(seen) == ["t1", "t2"]
    assert state.creates == [] and state.deletes == []
    assert sorted(fleet.released) == sorted(e for e in state.envs)  # fleet.run released both


def test_fleet_workspace_acquires_and_releases_on_exit(stack, tmp_path):
    api_url, state = stack
    fleet = _RegisteringFleet(state)
    with make_fleet_workspace(fleet, TASK, **kw(api_url, state, tmp_path)) as ws:
        assert isinstance(ws, FleetWorkspace)
        env_id = ws.env_id
        assert env_id in state.envs and fleet.released == []
        assert ws.execute_command("echo fleet", timeout=30).stdout.strip() == "fleet"
    assert fleet.released == [env_id]
    assert state.deletes == [] and env_id in state.envs  # the mock backend forgot it; the API did not
    ws.cleanup()  # a second cleanup releases nothing twice
    assert fleet.released == [env_id]


def test_a_copy_of_a_fleet_workspace_never_releases(stack, tmp_path):
    api_url, state = stack
    fleet = _RegisteringFleet(state)
    ws = make_fleet_workspace(fleet, TASK, **kw(api_url, state, tmp_path))
    env_id = ws.env_id
    copy = ws.model_copy()
    assert isinstance(copy, FleetWorkspace) and copy._release is None
    copy.cleanup()
    assert fleet.released == [] and env_id in state.envs
    ws.cleanup()
    assert fleet.released == [env_id]


def test_fleet_workspace_releases_when_the_start_fails(stack, tmp_path):
    api_url, state = stack
    fleet = _RegisteringFleet(state)
    with pytest.raises(PermissionError):
        make_fleet_workspace(fleet, TASK, **kw(api_url, state, tmp_path, api_key="wrong"))
    assert len(fleet.released) == 1 and state.deletes == []


@pytest.mark.parametrize("field", ["template", "image", "env_id", "delete_on_exit"])
def test_fleet_owned_fields_are_refused(field):
    handle = SimpleNamespace(sandbox_id="sb-1", data_plane=None, backend=None, task=TASK)
    with pytest.raises(ValueError, match="fleet-owned"):
        make_handle_workspace(handle, **{field: "x"})
    with pytest.raises(ValueError, match="fleet-owned"):
        make_fleet_workspace(SimpleNamespace(), TASK, **{field: "x"})


def test_defaults_come_from_the_handle_and_yield_to_kwargs():
    handle = SimpleNamespace(
        sandbox_id="sb-1",
        data_plane=SimpleNamespace(address="ate-env-api.ate-env:7777"),
        backend=SimpleNamespace(atespace="team-a"),
    )
    assert _defaults_from_handle(handle) == {"api_url": "ate-env-api.ate-env:7777", "atespace": "team-a"}
    bare = SimpleNamespace(sandbox_id="sb-2", data_plane=None, backend=None)
    assert _defaults_from_handle(bare) == {}
