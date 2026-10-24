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
"""uni-agent task ``r2e_gym``: an agent fixes ``/testbed``; hidden tests grade it afterwards.

Lifecycle inside one Substrate environment (the R2E-Gym image of the instance):

1. expose the testbed virtualenv on PATH and drop stale bytecode;
2. hide the grading tests: archive ``/r2e_tests``, download the archive to the
   driver, delete the directory (the agent never sees the tests);
3. run the agent on ``/testbed`` (oracle mode applies the gold patch instead);
4. restore the tests to ``/testbed/r2e_tests`` and run ``run_tests.sh``;
5. reward 1.0 iff the pytest outcome map equals ``expected_output_json``.

Steps 3 and 4 are the two sandbox uses of agentic coding RL: in-rollout tool execution
and the post-hoc reward executor, both in the same environment.
"""

from __future__ import annotations

import logging
import tempfile
from pathlib import Path
from typing import Any

from pydantic import Field
from uni_agent.tasks.base import Task, TaskConfig, TaskResult
from uni_agent.tasks.registry import register_task

from .reward import calculate_reward, extract_gold_patch

logger = logging.getLogger(__name__)

TESTBED = "/testbed"
HIDDEN_TESTS_DIR = "/r2e_tests"
ARCHIVE = "/tmp/r2e_tests.tar.gz"
RESTORED_ARCHIVE = "/tmp/r2e_tests.restore.tar.gz"
GOLD_PATCH = "/tmp/r2e_gold.patch"

# The testbed virtualenv first, then the usual system paths; quiet non-interactive tools.
TASK_ENV = {
    "PATH": "/testbed/.venv/bin:/root/.local/bin:/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin",
    "PAGER": "cat",
    "GIT_PAGER": "cat",
    "MANPAGER": "cat",
    "PIP_PROGRESS_BAR": "off",
    "TQDM_DISABLE": "1",
    "CI": "1",
}

SETUP = f"""
set -e
test -d {TESTBED}
if [ -d {TESTBED}/.venv/bin ]; then
  mkdir -p /root/.local/bin
  for f in {TESTBED}/.venv/bin/*; do [ -f "$f" ] && [ -x "$f" ] && ln -sfn "$f" "/root/.local/bin/$(basename "$f")"; done
  ln -sfn {TESTBED}/.venv/bin/python /usr/local/bin/python 2>/dev/null || true
  ln -sfn {TESTBED}/.venv/bin/python /usr/local/bin/python3 2>/dev/null || true
fi
cd {TESTBED} && (timeout 60 find . -name '__pycache__' -prune -exec rm -rf {{}} + 2>/dev/null || true)
"""
HIDE = f"test -d {HIDDEN_TESTS_DIR} && tar -C / -czf {ARCHIVE} {HIDDEN_TESTS_DIR.lstrip('/')} && rm -rf {HIDDEN_TESTS_DIR}"
RESTORE = f"rm -rf {TESTBED}/r2e_tests && tar -C {TESTBED} -xzf {RESTORED_ARCHIVE} && rm -f {RESTORED_ARCHIVE}"
RUN_TESTS = "/bin/bash run_tests.sh 2>&1"


class R2EGymTaskConfig(TaskConfig):
    name: str = "r2e_gym"
    eval_timeout: float = Field(default=900.0, description="Timeout (s) for the hidden-test run.")
    setup_timeout: float = Field(default=300.0, description="Timeout (s) for each setup/hide/restore step.")
    run_oracle_solution: bool = Field(
        default=False, description="Skip the agent and apply the gold patch; the hidden tests must then pass."
    )


@register_task("r2e_gym")
class R2EGymTask(Task):
    name = "r2e_gym"
    config_model = R2EGymTaskConfig

    async def run(self) -> TaskResult:
        cfg: R2EGymTaskConfig = self.config  # type: ignore[assignment]
        sample: dict[str, Any] = cfg.metadata
        instance = sample.get("instance_id") or sample.get("commit_hash") or "?"
        expected = sample.get("expected_output_json")
        if not expected:
            raise ValueError(f"r2e_gym instance {instance}: metadata lacks expected_output_json")
        logger.info("r2e_gym start instance=%s oracle=%s", instance, cfg.run_oracle_solution)

        async with self.build_sandbox() as sandbox:
            await self._step(sandbox, SETUP, "setup", cfg.setup_timeout)
            await self._step(sandbox, HIDE, "hide tests", cfg.setup_timeout)
            with tempfile.NamedTemporaryFile(prefix="r2e_tests_", suffix=".tar.gz", delete=False) as fh:
                local_archive = Path(fh.name)
            try:
                await sandbox.download_file(ARCHIVE, local_archive)
                await sandbox.exec(["rm", "-f", ARCHIVE])

                if cfg.run_oracle_solution:
                    patch = extract_gold_patch(sample.get("parsed_commit_content", ""))
                    if patch.strip():
                        await sandbox.write_file(GOLD_PATCH, patch)
                        await self._step(sandbox, f"git apply --whitespace=fix {GOLD_PATCH}", "gold patch", cfg.setup_timeout)
                    else:  # no gold patch: the hidden tests run against the unmodified tree (expected reward 0.0)
                        logger.warning("r2e_gym instance %s: empty gold patch, grading the unmodified tree", instance)
                    finished = True
                else:
                    agent = self.build_agent()
                    agent_result = await agent.run(sandbox=sandbox, messages=cfg.prompt, workdir=TESTBED)
                    finished = bool(agent_result.finished)

                await sandbox.upload_file(local_archive, RESTORED_ARCHIVE)
                await self._step(sandbox, RESTORE, "restore tests", cfg.setup_timeout)
                tests = await sandbox.exec(
                    ["bash", "-c", RUN_TESTS], env=TASK_ENV, workdir=TESTBED, timeout=cfg.eval_timeout
                )
            finally:
                local_archive.unlink(missing_ok=True)

        output = tests.stdout or ""
        reward = calculate_reward(output, expected)
        logger.info("r2e_gym done instance=%s reward=%s finished=%s exit=%s", instance, reward, finished, tests.exit_code)
        return TaskResult(
            reward=reward,
            accuracy=reward,
            finished=finished,
            extra_info={
                "resolved": bool(reward),
                "test_exit_code": tests.exit_code,
                "test_timed_out": tests.exit_code == -1,
                "test_output_tail": output[-2000:],
            },
        )

    @staticmethod
    async def _step(sandbox: Any, script: str, what: str, timeout: float) -> str:
        result = await sandbox.exec(["bash", "-c", script], env=TASK_ENV, timeout=timeout, workdir=TESTBED)
        if result.exit_code != 0:
            detail = (result.stderr or result.stdout or "").strip()[-500:]
            raise RuntimeError(f"r2e_gym {what} failed (exit {result.exit_code}): {detail}")
        return result.stdout
