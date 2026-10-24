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
"""Provider tests against the in-process fake Substrate environment API (clients/python/tests/fakes.py)."""

from __future__ import annotations

import json

import pytest

from uni_agent_substrate import provider as prov
from uni_agent_substrate.provider import AteEnvSandbox, resolve_image

from .fakes import FakeProc

PINNED = "registry.example/repo/numpy_final@sha256:" + "a" * 64


def _sandbox(fake_client, **kw) -> AteEnvSandbox:
    sb = AteEnvSandbox(image=PINNED, endpoint="fake", **kw)
    prov._CLIENTS.clear()
    import asyncio

    prov._CLIENTS[("fake", "", id(asyncio.get_running_loop()))] = fake_client
    return sb


def test_resolve_image_requires_digest(tmp_path):
    with pytest.raises(RuntimeError):
        resolve_image("namanjain12/numpy_final:abc", {})
    assert resolve_image(PINNED, {}) == PINNED
    assert resolve_image("namanjain12/numpy_final:abc", {"namanjain12/numpy_final:abc": PINNED}) == PINNED
    assert resolve_image("namanjain12/numpy_final:abc", {"docker.io/namanjain12/numpy_final:abc": PINNED}) == PINNED


async def test_start_creates_pinned_env_and_probes(fake_stack):
    client, fakes = fake_stack
    sb = _sandbox(client, template="swe-2c6g", atespace="ate-env")
    fakes.processes.next_procs.append(FakeProc(exit_code=0))
    await sb.start()
    assert sb.env_id and sb.env_id.startswith("ua-")
    env = fakes.environments.environments[("ate-env", sb.env_id)]
    assert env.template.name.startswith("swe-2c6g-")  # derived template from the digest
    probe = next(iter(fakes.processes.procs.values()))
    assert probe.command == ["true"]
    assert await sb.is_alive()
    await sb.stop()
    assert ("ate-env", sb.env_id) not in fakes.environments.environments
    await sb.stop()  # idempotent


async def test_exec_collects_output_and_exit(fake_stack):
    client, fakes = fake_stack
    sb = _sandbox(client)
    fakes.processes.next_procs.append(FakeProc(exit_code=0))
    await sb.start()
    fakes.processes.next_procs.append(FakeProc(chunks=[("stdout", b"hello "), ("stderr", b"warn"), ("stdout", b"world")], exit_code=3))
    res = await sb.exec(["sh", "-c", "x"], workdir="/testbed", env={"A": "1"}, timeout=10)
    assert (res.exit_code, res.stdout, res.stderr) == (3, "hello world", "warn")
    started = list(fakes.processes.procs.values())[-1]
    assert started.cwd == "/testbed" and started.env == {"A": "1"} and started.timeout_seconds == 10


async def test_exec_reattaches_after_stream_window(fake_stack):
    client, fakes = fake_stack
    sb = _sandbox(client, stream_window_s=0.2)
    fakes.processes.next_procs.append(FakeProc(exit_code=0))
    await sb.start()
    # running_polls keeps the process alive across follow streams; the fake ends a follow
    # stream only after replaying chunks, so a window that expires first forces a re-attach.
    fakes.processes.next_procs.append(FakeProc(chunks=[("stdout", b"part1-"), ("stdout", b"part2")], exit_code=0, hang_follow=True))
    proc_script = fakes.processes.next_procs[-1]

    async def unhang():
        import asyncio

        await asyncio.sleep(0.5)
        proc_script.hang_follow = False

    import asyncio

    asyncio.get_running_loop().create_task(unhang())
    res = await sb.exec(["long"], timeout=10)
    assert res.exit_code == 0
    assert res.stdout.count("part1-") == 1 and res.stdout.endswith("part2")


async def test_exec_timeout_returns_minus_one(fake_stack):
    client, fakes = fake_stack
    sb = _sandbox(client, stream_window_s=0.2)
    fakes.processes.next_procs.append(FakeProc(exit_code=0))
    await sb.start()
    fakes.processes.next_procs.append(FakeProc(hang_follow=True))
    res = await sb.exec(["sleep", "999"], timeout=0.3)
    assert res.exit_code == -1
    proc = list(fakes.processes.procs.values())[-1]
    assert 9 in proc.signals  # killed


async def test_files_roundtrip(fake_stack):
    client, fakes = fake_stack
    sb = _sandbox(client)
    fakes.processes.next_procs.append(FakeProc(exit_code=0))
    await sb.start()
    await sb.write_file("/tmp/dir/a.txt", "héllo")
    assert await sb.read_file("/tmp/dir/a.txt") == "héllo".encode()
    mk = [p for p in fakes.processes.procs.values() if p.command[:2] == ["mkdir", "-p"]]
    assert mk and mk[-1].command[2] == "/tmp/dir"


async def test_shell_persists_state_script(fake_stack):
    client, fakes = fake_stack
    sb = _sandbox(client)
    fakes.processes.next_procs.append(FakeProc(exit_code=0))
    await sb.start()
    shell = await sb.open_shell(cwd="/testbed", env={"FOO": "bar"})
    fakes.processes.next_procs.append(FakeProc(chunks=[("stdout", b"ok")], exit_code=0))
    res = await shell.run("cd src && export X=1", timeout=5)
    assert res.exit_code == 0 and res.stdout == "ok"
    proc = list(fakes.processes.procs.values())[-1]
    assert proc.command[:2] == ["bash", "-c"] and proc.env == {"FOO": "bar"}
    script = proc.command[2]
    assert "cd src && export X=1" in script and "declare -px" in script and "/testbed" in script
    await shell.close()


def test_from_config_rejects_image_mounts():
    from uni_agent.sandbox.base import SandboxConfig

    cfg = SandboxConfig(provider="ate_env", image=PINNED, sandbox_kwargs={"template": "swe-2c6g"})
    sb = AteEnvSandbox.from_config(cfg)
    assert sb.template == "swe-2c6g" and sb.image == PINNED
    assert json.dumps(sb._digests) == "{}"
