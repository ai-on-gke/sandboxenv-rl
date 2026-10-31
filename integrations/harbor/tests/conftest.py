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

import hashlib
import itertools
import json
import shutil
import subprocess
import textwrap
from pathlib import Path
from types import SimpleNamespace

import pytest
from harbor.models.task.config import EnvironmentConfig
from harbor.models.trial.paths import TrialPaths

from ate_env_harbor import AteEnvEnvironment, images
from ate_env_harbor._client import close_current_loop_clients

from .fakes import DockerExecutor, FakeState, LocalExecutor, serve

ECHO_TASK = Path(__file__).parent / "tasks" / "echo"
ALPINE = "alpine@sha256:d9e853e87e55526f6b2917df91a2115c36dd7c696a35be12163d44e6e2a4b6bc"
SESSION = "echo__AbCdEf__env"


STUB_DOCKER = textwrap.dedent(
    """\
    #!/usr/bin/env python3
    import hashlib, json, os, sys, time
    args = sys.argv[1:]
    with open(os.environ["STUB_LOG"], "a") as f:
        f.write(json.dumps(args) + "\\n")
    exists_file = os.environ["STUB_EXISTS"]
    existing = set(open(exists_file).read().split()) if os.path.exists(exists_file) else set()
    digest = lambda ref: "sha256:" + hashlib.sha256(ref.encode()).hexdigest()
    if args[:3] == ["buildx", "imagetools", "inspect"]:
        ref = args[-1]
        if ref in existing or "@sha256:" in ref or not ref.startswith("reg.example/"):
            print(json.dumps(digest(ref)))
            sys.exit(0)
        sys.stderr.write("ERROR: %s: not found\\n" % ref)
        sys.exit(1)
    if args[:2] == ["buildx", "build"]:
        tag = args[args.index("-t") + 1]
        meta = args[args.index("--metadata-file") + 1]
        time.sleep(float(os.environ.get("STUB_BUILD_SLEEP", "0")))
        if os.environ.get("STUB_BUILD_FAIL"):
            sys.stderr.write("boom: the Dockerfile is broken\\n")
            sys.exit(1)
        with open(meta, "w") as f:
            json.dump({"containerimage.digest": digest(tag)}, f)
        with open(exists_file, "a") as f:
            f.write(tag + "\\n")
        sys.exit(0)
    sys.stderr.write("unexpected docker call: %r\\n" % (args,))
    sys.exit(2)
    """
)

STUB_GCLOUD = textwrap.dedent(
    """\
    #!/usr/bin/env python3
    import hashlib, json, os, sys
    args = sys.argv[1:]
    with open(os.environ["STUB_LOG"], "a") as f:
        f.write(json.dumps(args) + "\\n")
    tag = args[args.index("--tag") + 1]
    print(json.dumps({"results": {"images": [
        {"name": tag, "digest": "sha256:" + hashlib.sha256(tag.encode()).hexdigest()}]}}))
    """
)


def _digest(ref: str) -> str:
    return "sha256:" + hashlib.sha256(ref.encode()).hexdigest()


@pytest.fixture
def stubs(tmp_path, monkeypatch):
    bin_dir = tmp_path / "bin"
    bin_dir.mkdir()
    for name, body in (("docker", STUB_DOCKER), ("gcloud", STUB_GCLOUD)):
        path = bin_dir / name
        path.write_text(body)
        path.chmod(0o755)
    log = tmp_path / "calls.log"
    monkeypatch.setenv("STUB_LOG", str(log))
    monkeypatch.setenv("STUB_EXISTS", str(tmp_path / "exists.txt"))
    monkeypatch.delenv("STUB_BUILD_SLEEP", raising=False)
    monkeypatch.delenv("STUB_BUILD_FAIL", raising=False)

    class Stubs:
        docker = str(bin_dir / "docker")
        gcloud = str(bin_dir / "gcloud")

        @staticmethod
        def calls() -> list[list[str]]:
            if not log.exists():
                return []
            return [json.loads(line) for line in log.read_text().splitlines()]

        @staticmethod
        def builds() -> list[list[str]]:
            return [c for c in Stubs.calls() if c[:2] == ["buildx", "build"]]

    return Stubs



@pytest.fixture(autouse=True)
def _fresh_image_cache():
    """The digest cache and the build locks are per process; tests start clean."""
    images.reset_cache()
    yield
    images.reset_cache()


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
