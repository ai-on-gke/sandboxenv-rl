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

from __future__ import annotations

import itertools
import shutil
import subprocess
from pathlib import Path
from types import SimpleNamespace

import pytest
from harbor.models.task.config import EnvironmentConfig
from harbor.models.trial.paths import TrialPaths

from ate_env_harbor import AteEnvEnvironment
from ate_env_harbor._client import close_current_loop_clients

from .fakes import DockerExecutor, FakeState, LocalExecutor, serve

ECHO_TASK = Path(__file__).parent / "tasks" / "echo"
ALPINE = "alpine@sha256:d9e853e87e55526f6b2917df91a2115c36dd7c696a35be12163d44e6e2a4b6bc"
SESSION = "echo__AbCdEf__env"


def docker_available() -> bool:
    if shutil.which("docker") is None:
        return False
    try:
        return subprocess.run(["docker", "info"], capture_output=True, timeout=30).returncode == 0
    except (OSError, subprocess.SubprocessError):
        return False


@pytest.fixture
async def fake(tmp_path):
    """The fake API with commands running on this machine; ``state.child_path`` sets their PATH."""
    state = FakeState(executor_factory=None)
    state.child_path = None  # tests set a PATH for the commands the fake runs
    state.executor_factory = lambda image, env_id: LocalExecutor(
        tmp_path / "envs" / env_id, path=state.child_path
    )
    async with serve(state) as addr:
        yield SimpleNamespace(addr=addr, state=state)
    await close_current_loop_clients()


@pytest.fixture
async def docker_fake():
    """The fake API with commands running in a container per environment."""
    if not docker_available():
        pytest.skip("needs a docker daemon")
    state = FakeState(executor_factory=lambda image, env_id: DockerExecutor(image))
    async with serve(state) as addr:
        yield SimpleNamespace(addr=addr, state=state)
    await close_current_loop_clients()


@pytest.fixture
def task_dir(tmp_path) -> Path:
    """A copy of tests/tasks/echo the test can edit."""
    copy = tmp_path / "task"
    shutil.copytree(ECHO_TASK, copy)
    return copy


@pytest.fixture
def make_env(fake, tmp_path, task_dir):
    """Builds AteEnvEnvironment instances against the local fake.

    The task's workdir is a host directory, because the local fake runs the
    commands on this machine.
    """
    counter = itertools.count(1)
    work = tmp_path / "work"
    work.mkdir()

    def _make(
        *,
        config: EnvironmentConfig | None = None,
        session_id: str | None = None,
        environment_dir: Path | None = None,
        **kwargs,
    ) -> AteEnvEnvironment:
        n = next(counter)
        cfg = config or EnvironmentConfig(
            docker_image=ALPINE, cpus=1, memory_mb=512, workdir=str(work)
        )
        paths = TrialPaths(trial_dir=tmp_path / f"trial-{n}")
        paths.mkdir()
        options = {
            "api_url": fake.addr,
            "build": "none",
            "create_timeout_sec": 10,
            "scratch_dir": str(tmp_path / "scratch"),
        }
        options.update(kwargs)
        return AteEnvEnvironment(
            environment_dir=environment_dir or task_dir / "environment",
            environment_name="echo",
            session_id=session_id or f"{SESSION}{n}",
            trial_paths=paths,
            task_env_config=cfg,
            **options,
        )

    _make.work = work
    return _make
