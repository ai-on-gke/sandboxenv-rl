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
"""R2EGymTask flow against a scripted sandbox: hide tests before the agent, restore after, grade the run."""

from __future__ import annotations

import contextlib
import json
from dataclasses import dataclass, field
from pathlib import Path

from uni_agent.sandbox.base import ExecResult

from uni_agent_substrate.tasks.r2e_gym import task as t

from .test_r2e_reward import LOG

EXPECTED = {"TestA.test_one": "PASSED", "test_two": "PASSED", "test_three": "FAILED", "test_err": "ERROR"}
GOLD = {"file_diffs": [{"header": {"file": {"path": "pkg/mod.py"}}, "hunks": [{"content": "@@ -1 +1 @@\n-a\n+b"}]}]}


@dataclass
class ScriptedSandbox:
    """Records every call; the hidden-test run returns test_output."""

    test_output: str
    calls: list[tuple] = field(default_factory=list)
    files: dict[str, str] = field(default_factory=dict)

    async def exec(self, argv, timeout=None, workdir=None, env=None):
        self.calls.append(("exec", argv, workdir))
        script = argv[2] if argv[:2] == ["bash", "-c"] else " ".join(argv)
        if script == t.RUN_TESTS:
            return ExecResult(exit_code=1, stdout=self.test_output, stderr="")
        return ExecResult(exit_code=0, stdout="", stderr="")

    async def download_file(self, remote, local):
        self.calls.append(("download", remote))
        Path(local).write_bytes(b"tar")

    async def upload_file(self, local, remote):
        self.calls.append(("upload", remote))

    async def write_file(self, path, content):
        self.calls.append(("write", path))
        self.files[path] = content


class FakeAgent:
    def __init__(self, finished):
        self.finished = finished
        self.seen = None

    async def run(self, sandbox, messages, workdir):
        self.seen = (sandbox, messages, workdir)
        sandbox.calls.append(("agent",))
        return type("R", (), {"finished": self.finished})()


def make_task(monkeypatch, sandbox, agent=None, **cfg):
    task = t.R2EGymTask(
        t.R2EGymTaskConfig(
            sandbox={"provider": "ate_env", "image": "registry.example/numpy_final@sha256:" + "a" * 64},
            prompt=[{"role": "user", "content": "fix it"}],
            metadata={"instance_id": "repo-abc", "expected_output_json": json.dumps(EXPECTED),
                      "parsed_commit_content": json.dumps(GOLD)},
            **cfg,
        )
    )

    @contextlib.asynccontextmanager
    async def fake_build_sandbox():
        yield sandbox

    monkeypatch.setattr(task, "build_sandbox", fake_build_sandbox)
    monkeypatch.setattr(task, "build_agent", lambda: agent)
    return task


async def test_agent_episode_hides_tests_restores_and_grades(monkeypatch):
    sb, agent = ScriptedSandbox(LOG), FakeAgent(finished=True)
    result = await make_task(monkeypatch, sb, agent).run()

    assert result.reward == 1.0 and result.finished is True and result.extra_info["resolved"]
    assert agent.seen == (sb, [{"role": "user", "content": "fix it"}], t.TESTBED)
    kinds = [c[0] if c[0] != "exec" else c[1][2] if c[1][:2] == ["bash", "-c"] else c[1][0] for c in sb.calls]
    # order: setup, hide, archive out, archive deleted, agent, archive back, restore, hidden tests
    assert kinds == [t.SETUP, t.HIDE, "download", "rm", "agent", "upload", t.RESTORE, t.RUN_TESTS]
    assert all(c[2] == t.TESTBED for c in sb.calls if c[0] == "exec" and c[1][:2] == ["bash", "-c"])


async def test_wrong_outcome_scores_zero_and_unfinished_is_reported(monkeypatch):
    sb = ScriptedSandbox(LOG.replace("PASSED r2e_tests/test_x.py::test_two", "FAILED r2e_tests/test_x.py::test_two"))
    result = await make_task(monkeypatch, sb, FakeAgent(finished=False)).run()
    assert result.reward == 0.0 and result.finished is False and result.extra_info["test_exit_code"] == 1


async def test_oracle_applies_gold_patch_instead_of_agent(monkeypatch):
    sb = ScriptedSandbox(LOG)
    result = await make_task(monkeypatch, sb, agent=None, run_oracle_solution=True).run()
    assert result.reward == 1.0 and result.finished is True
    assert ("write", t.GOLD_PATCH) in sb.calls and "pkg/mod.py" in sb.files[t.GOLD_PATCH]
    assert any(c[0] == "exec" and "git apply" in c[1][2] for c in sb.calls)
    assert ("agent",) not in sb.calls
