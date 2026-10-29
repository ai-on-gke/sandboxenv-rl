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

"""Harbor's own trial runner on this provider, against the docker-backed fake.

``harbor run`` is driven as a user would, as a subprocess; the fake API runs
in the test's event loop and each environment is a container started from
the task's image, thus ``/logs``, ``/tests`` and ``/app`` are real paths and
the verifier's root-only steps run as the image's root.
"""

from __future__ import annotations

import asyncio
import json
import os
import subprocess
import sys
from pathlib import Path

import pytest

from .conftest import ALPINE, ECHO_TASK, docker_available

pytestmark = pytest.mark.skipif(not docker_available(), reason="needs a docker daemon")

HARBOR = Path(sys.executable).parent / "harbor"


async def run_harbor(addr: str, jobs_dir: Path, agent: str, *extra: str, trials: int = 1) -> tuple[list[tuple[dict, Path]], str]:
    jobs_dir.mkdir(parents=True, exist_ok=True)
    cmd = [
        str(HARBOR), "run",
        "-p", str(ECHO_TASK),
        "-a", agent,
        "-e", "ate_env_harbor:AteEnvEnvironment",
        "--ek", f"api_url={addr}",
        "--ek", "build=none",
        "-n", "1",
        "--jobs-dir", str(jobs_dir),
        "-q", "-y",
        *extra,
    ]
    env = {**os.environ, "HARBOR_TELEMETRY": "off", "SUBSTRATE_ENV_API": addr}
    proc = await asyncio.create_subprocess_exec(
        *cmd, env=env, cwd=jobs_dir, stdout=subprocess.PIPE, stderr=subprocess.STDOUT
    )
    out, _ = await asyncio.wait_for(proc.communicate(), 600)
    text = out.decode(errors="replace")
    # jobs/<job>/result.json is the job's; jobs/<job>/<trial>/result.json the trial's.
    results = sorted(jobs_dir.glob("*/*/result.json"))
    assert len(results) == trials, f"exit {proc.returncode}; output:\n{text}"
    return [(json.loads(r.read_text()), r.parent) for r in results], text


def reward(result: dict) -> set[float]:
    verifier = result.get("verifier_result") or {}
    return set((verifier.get("rewards") or {}).values())


def diagnose(state, text: str) -> str:
    """The harbor output plus the fake's exec log, for an assertion message."""
    lines = [f"{e['env_id']} exit={e['code']} {e['command'][-1][:120]!r}" for e in state.execs[-20:]]
    return text + "\n--- last execs ---\n" + "\n".join(lines)


async def test_oracle_trial_rewards_one(docker_fake, tmp_path):
    [(result, trial_dir)], text = await run_harbor(docker_fake.addr, tmp_path / "jobs", "oracle")
    assert result.get("exception_info") is None, diagnose(docker_fake.state, text)
    assert reward(result) == {1}, diagnose(docker_fake.state, text)
    assert (trial_dir / "verifier" / "reward.txt").read_text().strip() == "1"
    record = json.loads((trial_dir / "ate_env.json").read_text())
    assert record["template"] == "harbor-c1-m512"
    assert record["rootfs_image"] == ALPINE
    assert record["image_user"] == "root"
    state = docker_fake.state
    assert state.creates[0]["rootfs_image"] == ALPINE
    assert state.deletes == [record["id"]]
    assert state.envs == {}
    # Root is the image's user, thus the verifier's root-only steps ran unwrapped.
    assert not any("sudo" in e["command"][-1] or e["command"][-1].startswith("su ") for e in state.execs)


async def test_nop_trial_rewards_zero(docker_fake, tmp_path):
    [(result, _)], text = await run_harbor(docker_fake.addr, tmp_path / "jobs", "nop")
    assert result.get("exception_info") is None, text
    assert reward(result) == {0}, text
    assert docker_fake.state.envs == {}


async def test_concurrent_attempts_share_the_channel(docker_fake, tmp_path):
    results, text = await run_harbor(
        docker_fake.addr, tmp_path / "jobs", "oracle", "-k", "3", "-n", "3", trials=3
    )
    for result, trial_dir in results:
        assert result.get("exception_info") is None, diagnose(docker_fake.state, text)
        assert reward(result) == {1}, diagnose(docker_fake.state, text)
        assert (trial_dir / "ate_env.json").exists()
    state = docker_fake.state
    assert len(state.creates) == 3 and len({c["id"] for c in state.creates}) == 3
    assert sorted(state.deletes) == sorted(c["id"] for c in state.creates)
    assert state.envs == {}


async def test_no_delete_keeps_the_environment_suspended(docker_fake, tmp_path):
    [(result, _)], text = await run_harbor(docker_fake.addr, tmp_path / "jobs", "nop", "--no-delete")
    assert result.get("exception_info") is None, text
    state = docker_fake.state
    assert len(state.suspends) == 1 and state.deletes == []
    assert len(state.envs) == 1  # serve() removes the container at teardown
