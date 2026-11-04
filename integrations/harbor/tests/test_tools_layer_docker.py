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

"""The tool paths inside a login shell, against a real image.

terminus-2 starts its tmux session with ``bash --login``; ``/etc/profile``
on Debian, Ubuntu and Alpine images overwrites PATH, thus the prefix the
provider puts inside each command is not enough there. The provider writes a
``/etc/profile.d`` drop-in at start, and this test checks a login shell in
an Ubuntu container sees the layer first on PATH.
"""

from __future__ import annotations

import pytest
from harbor.models.task.config import EnvironmentConfig
from harbor.models.trial.paths import TrialPaths

from ate_env_harbor import AteEnvEnvironment

from .conftest import docker_available
from .fakes import DockerExecutor, FakeState, serve

pytestmark = pytest.mark.skipif(not docker_available(), reason="needs a docker daemon")

UBUNTU = "ubuntu@sha256:24737cef17f4e22b1977df7d9a05e77c4e28b7da5b8911f8d8ccda136bf3573c"
TOOLS = "/opt/harbor-tools/bin"


class _LayeredDockerExecutor(DockerExecutor):
    """A container with a tool directory in place, standing in for a mounted layer."""

    async def start(self) -> None:
        await super().start()
        await self.write(f"{TOOLS}/hb-tool", b"#!/bin/sh\necho from-layer\n", 0o755)


async def test_login_shell_sees_the_tool_paths_first(tmp_path):
    state = FakeState(executor_factory=lambda image, env_id: _LayeredDockerExecutor(image))
    async with serve(state) as addr:
        paths = TrialPaths(trial_dir=tmp_path / "trial")
        paths.mkdir()
        env = AteEnvEnvironment(
            environment_dir=tmp_path / "environment",
            environment_name="layered",
            session_id="layered__x__env",
            trial_paths=paths,
            task_env_config=EnvironmentConfig(docker_image=UBUNTU, workdir="/tmp"),
            api_url=addr,
            build="none",
            tool_paths=TOOLS,
        )
        (tmp_path / "environment").mkdir()
        await env.start(force_build=False)
        try:
            # /etc/profile on Ubuntu exports its own PATH; the drop-in restores the layer.
            result = await env.exec("bash --login -c 'echo \"$PATH\"; command -v hb-tool; hb-tool'")
            assert result.return_code == 0, result
            lines = result.stdout.strip().splitlines()
            assert lines[0].startswith(f"{TOOLS}:"), lines
            assert lines[1:] == [f"{TOOLS}/hb-tool", "from-layer"]
            # No trailing colon: the drop-in and the prefix never put the cwd on PATH.
            result = await env.exec("bash --login -c 'echo \"$PATH\"'")
            assert not result.stdout.strip().endswith(":")
            result = await env.exec("sh -c 'unset PATH; " + env._tooled('echo "$PATH"') + "'")
            assert result.stdout.strip() == TOOLS
        finally:
            await env.stop(delete=True)
        from ate_env_harbor._client import close_current_loop_clients

        await close_current_loop_clients()
