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

"""agent-sandbox-rl's ``SandboxFleet`` and ``SandboxHandle`` on top of ``ate_env``.

``SandboxFleet`` takes agent-sandbox-rl's ``FleetConfig`` and runs the
``ate_env`` fleet with ``FleetConfig.to_ate_env()``. Its ``run()`` has
agent-sandbox-rl's signature (``strategy`` is the second positional
argument, default ``"naive"``); the session-reuse options (``keep_warm``,
``recycle``, ``reset``) are not supported on ate-env and raise when set.
Handles carry agent-sandbox-rl's ``cluster_name``; the Kubernetes fields
(``claim_name``, ``pod_name``, ``hostname``, ``pod_ip``) do not exist on
ate-env.
"""

from __future__ import annotations

import dataclasses
from collections.abc import Callable, Sequence
from typing import Any

from ate_env import SandboxFleet as AteSandboxFleet
from ate_env import SandboxHandle as AteSandboxHandle
from ate_env import Task

from .config import FleetConfig


@dataclasses.dataclass
class SandboxHandle(AteSandboxHandle):
    """An ``ate_env.SandboxHandle`` with agent-sandbox-rl's ``cluster_name``:
    the name of the ``ClusterConfig`` the fleet runs on."""

    cluster_name: str = "default"

    @classmethod
    def wrap(cls, handle: AteSandboxHandle, cluster_name: str) -> SandboxHandle:
        """Copy an ``ate_env`` handle into one that carries ``cluster_name``."""
        if isinstance(handle, cls):
            return handle
        fields = dataclasses.fields(handle)
        wrapped = cls(**{f.name: getattr(handle, f.name) for f in fields if f.init},
                      cluster_name=cluster_name)
        for f in fields:
            if not f.init:
                setattr(wrapped, f.name, getattr(handle, f.name))
        return wrapped


class SandboxFleet(AteSandboxFleet):
    """agent-sandbox-rl's fleet API, run by the ``ate_env`` fleet."""

    def __init__(self, config: FleetConfig | None = None, driver: Any = None):
        self.agent_sandbox_config = config or FleetConfig()
        self.cluster_name = self.agent_sandbox_config.cluster().name
        super().__init__(self.agent_sandbox_config.to_ate_env(), driver=driver)

    def acquire(self, task: Task | str | Sequence[Task | str],  # type: ignore[override]
                timeout_s: float | None = None) -> SandboxHandle | list[SandboxHandle]:
        acquired = super().acquire(task, timeout_s=timeout_s)
        if isinstance(acquired, list):
            return [SandboxHandle.wrap(h, self.cluster_name) for h in acquired]
        return SandboxHandle.wrap(acquired, self.cluster_name)

    def run(self, process_fn: Callable[[Task, SandboxHandle], Any],  # type: ignore[override]
            strategy: str = "naive", concurrency: int | None = None,
            *, epochs: int = 1, keep_warm: bool = False, recycle: bool = False,
            reset: Any = None, max_reuses: int = 32, reset_timeout: float = 5.0,
            use_session: bool = True, scale_on_hold: bool = True) -> list:
        """Run all loaded tasks under ``strategy`` (none|naive|sliding|pipelined)
        with up to ``concurrency`` parallel acquire and process.

        ``epochs > 1`` runs that many passes and returns one task-ordered list
        for each pass, as agent-sandbox-rl does. ``max_reuses``,
        ``reset_timeout``, ``use_session`` and ``scale_on_hold`` only tune
        session reuse, thus they have no effect here.
        """
        unsupported = [name for name, value, default in (
            ("keep_warm", keep_warm, False), ("recycle", recycle, False), ("reset", reset, None),
        ) if value != default]
        if unsupported:
            raise NotImplementedError(
                f"ate-env: run({', '.join(f'{n}=' for n in unsupported)}...) is not "
                "supported; each task gets a fresh environment and the fleet deletes it "
                "on release")
        if epochs < 1:
            raise ValueError("epochs must be >= 1")
        passes = [super().run(process_fn, concurrency=concurrency, strategy=strategy)
                  for _ in range(epochs)]
        return passes[0] if epochs == 1 else passes
