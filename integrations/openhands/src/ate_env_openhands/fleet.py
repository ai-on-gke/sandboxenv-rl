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

"""Fleet adapters: ``AteEnvWorkspace`` on sandboxes a fleet acquires.

The same names and semantics as ``agent_sandbox_rl.adapters.openhands``, so a
rollout written for agent-sandbox changes only its imports: the **fleet**
acquires and releases sandboxes (pools, placement, accounting) and the
workspace merely binds one. The workspace never creates or deletes an
environment here.

    from ate_env import SandboxFleet, FleetConfig
    from ate_env_openhands.fleet import make_handle_workspace

    fleet = SandboxFleet(FleetConfig(backend="substrate", endpoint=API, template=BASE))
    fleet.load_tasks(tasks)
    fleet.setup()

    def rollout(task, handle):
        workspace = make_handle_workspace(handle, api_key=SESSION_KEY)
        try:
            ...                       # Conversation(agent, workspace) or execute_command
        finally:
            workspace.cleanup()       # HTTP state only; the fleet releases the sandbox

    fleet.run(rollout)

``make_fleet_workspace(fleet, task)`` is the other form: it acquires on
construction and releases through ``fleet.release(handle)`` when the
workspace is cleaned up, so ``with make_fleet_workspace(...) as ws:`` owns
one episode end to end.
"""

from __future__ import annotations

import logging
from typing import Any

from pydantic import PrivateAttr

from .workspace import AteEnvWorkspace

logger = logging.getLogger("ate_env_openhands.fleet")

# Workspace fields that decide which environment is bound and who deletes it.
# With a fleet in charge they are not the caller's to set.
_FLEET_OWNED_KWARGS = ("template", "template_atespace", "image", "env_id", "delete_on_exit")


def _reject_fleet_owned(kwargs: dict[str, Any], what: str) -> None:
    for key in _FLEET_OWNED_KWARGS:
        if key in kwargs:
            raise ValueError(
                f"{key!r} is fleet-owned when using {what}: configure it on the fleet, "
                "not the workspace"
            )


def _defaults_from_handle(handle: Any) -> dict[str, Any]:
    """What the handle knows that the workspace needs: the ate-env-api
    address its data plane points at, and the atespace of its backend. A
    caller's explicit kwargs win."""
    defaults: dict[str, Any] = {}
    data_plane = getattr(handle, "data_plane", None)
    address = getattr(data_plane, "address", None)
    if address:
        defaults["api_url"] = address
    atespace = getattr(getattr(handle, "backend", None), "atespace", None)
    if atespace:
        defaults["atespace"] = atespace
    return defaults


def make_handle_workspace(handle: Any, **workspace_kwargs: Any) -> AteEnvWorkspace:
    """An ``AteEnvWorkspace`` bound to an environment the fleet has already
    acquired: ``env_id=handle.sandbox_id``, nothing created, nothing deleted.

    The form for ``fleet.run(process_fn)``: the fleet hands ``(task, handle)``
    to the function and releases the handle when it returns, so
    ``workspace.cleanup()`` only drops HTTP state. ``api_url``, ``proxy_url``,
    ``api_key`` and the timeouts pass through; ``api_url`` and ``atespace``
    default from the handle when it carries them. Fleet-owned fields
    (``template``, ``image``, ``env_id``, ``delete_on_exit``) raise.
    """
    _reject_fleet_owned(workspace_kwargs, "make_handle_workspace")
    kwargs = {**_defaults_from_handle(handle), **workspace_kwargs}
    return AteEnvWorkspace(env_id=handle.sandbox_id, delete_on_exit=False, **kwargs)


class FleetWorkspace(AteEnvWorkspace):
    """An ``AteEnvWorkspace`` whose cleanup releases its handle to the fleet.

    Built by ``make_fleet_workspace``; not for direct construction.
    """

    _release: Any = PrivateAttr(default=None)

    def cleanup(self) -> None:
        """Drop HTTP state, then release the handle to the fleet, once."""
        try:
            super().cleanup()
        finally:
            release, self._release = self._release, None
            if release is not None:
                release()


def make_fleet_workspace(fleet: Any, task: Any, **workspace_kwargs: Any) -> FleetWorkspace:
    """Acquire a sandbox for ``task`` from ``fleet`` and bind a workspace to it.

    The fleet owns the sandbox's lifecycle: the workspace's ``cleanup()`` (and
    thus ``__exit__``) calls ``fleet.release(handle)``; nothing is deleted by
    the workspace itself. A failure while the workspace starts (health,
    session key) releases the handle before the error is raised.
    """
    _reject_fleet_owned(workspace_kwargs, "make_fleet_workspace")
    handle = fleet.acquire(task)
    kwargs = {**_defaults_from_handle(handle), **workspace_kwargs}
    released = False

    def release() -> None:
        nonlocal released
        if released:
            return
        released = True
        fleet.release(handle)

    try:
        workspace = FleetWorkspace(env_id=handle.sandbox_id, delete_on_exit=False, **kwargs)
    except BaseException:
        try:
            release()
        except Exception:  # the original error is the one to report
            logger.warning("release after a failed workspace start", exc_info=True)
        raise
    workspace._release = release
    return workspace
