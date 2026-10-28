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
"""Unit tests for ate_env_tool: the parts that do not need verl, a GPU or a cluster.

verl, transformers and the ate_env client are replaced by small stand-ins so the module imports
anywhere; the pool logic is exercised against a fake client that records creates and deletes.
"""

from __future__ import annotations

import asyncio
import sys
import types
from pathlib import Path

import pytest


def _install_stubs():
    verl = types.ModuleType("verl")
    tools = types.ModuleType("verl.tools")
    base = types.ModuleType("verl.tools.base_tool")

    class BaseTool:
        def __init__(self, config, tool_schema):
            self.config, self.tool_schema = config, tool_schema

    class OpenAIFunctionToolSchema(dict):
        def __init__(self, **kw):
            super().__init__(**kw)

    class ToolResponse:
        def __init__(self, text=""):
            self.text = text

    base.BaseTool, base.OpenAIFunctionToolSchema, base.ToolResponse = BaseTool, OpenAIFunctionToolSchema, ToolResponse
    tools.base_tool = base
    verl.tools = tools
    tutils = types.ModuleType("transformers.utils")
    tutils.get_json_schema = lambda fn: {"type": "function", "function": {"name": fn.__name__}}
    transformers = types.ModuleType("transformers")
    transformers.utils = tutils
    for name, mod in {"verl": verl, "verl.tools": tools, "verl.tools.base_tool": base,
                      "transformers": transformers, "transformers.utils": tutils}.items():
        sys.modules.setdefault(name, mod)


class FakeEnv:
    def __init__(self, client, env_id, script):
        self._client, self.id, self._script = client, env_id, script
        self.commands: list[str] = []
        self.deleted = False

    async def shell(self, command_line, *, timeout=None):
        self.commands.append(command_line)
        exit_code, stdout = self._script.pop(0) if self._script else (0, "ok\n")
        return types.SimpleNamespace(exit_code=exit_code, stdout=stdout, stderr="")

    async def delete(self):
        self.deleted = True
        self._client.deleted.append(self.id)


class FakeClient:
    """Stand-in for ate_env.Client; one shared ledger across instances so atexit cleanup is observable."""

    created: list[str] = []
    deleted: list[str] = []
    script: list[tuple[int, str]] = []
    auth_tokens: list = []
    fail_create = False

    def __init__(self, endpoint, auth_token=None):
        self.endpoint = endpoint
        FakeClient.auth_tokens.append(auth_token)

    async def create(self, env_id, rootfs_image=None):
        if FakeClient.fail_create:
            raise RuntimeError("no worker has room")
        FakeClient.created.append(env_id)
        return FakeEnv(FakeClient, env_id, FakeClient.script)

    async def delete(self, env_id, *, atespace="default"):
        FakeClient.deleted.append(env_id)

    async def close(self):
        return None


@pytest.fixture
def tool_module(monkeypatch):
    _install_stubs()
    ate_env = types.ModuleType("ate_env")
    ate_env.Client = FakeClient
    monkeypatch.setitem(sys.modules, "ate_env", ate_env)
    sys.path.insert(0, str(Path(__file__).resolve().parents[1]))
    sys.modules.pop("ate_env_tool", None)
    import ate_env_tool

    FakeClient.created, FakeClient.deleted, FakeClient.script, FakeClient.auth_tokens = [], [], [], []
    FakeClient.fail_create = False
    ate_env_tool._POOLS.clear()
    yield ate_env_tool
    ate_env_tool._POOLS.clear()


def test_last_expression_is_printed_only_when_it_is_an_expression(tool_module):
    f = tool_module.maybe_print_last_expression
    assert f("x = 1\nx + 1") == "x = 1\nprint(x + 1)"
    assert f("import os") == "import os"
    assert f("x = f()") == "x = f()"
    assert f("print(1)") == "print(1)"
    assert f("for i in range(2):\n    i") == "for i in range(2):\n    i"
    assert f("a = 1\n\n") == "a = 1\n\n"


def test_timeout_is_reported_from_exit_code_137(tool_module):
    assert tool_module.result_text(137, "", "", 30.0) == "TimeoutError: execution exceeded 30.0s"
    assert tool_module.result_text(0, "hi\n", "", 30.0) == "hi\n"
    assert tool_module.result_text(2, "", "", 30.0) == "(no output, exit code 2)"


def test_pool_is_per_event_loop_and_reuses_environments(tool_module):
    tool = tool_module.AteEnvCodeTool({"endpoint": "fake:1", "max_envs": 2, "env_prefix": "t1"}, {})

    async def two_calls():
        a = await tool.code_interpreter("print(1)")
        b = await tool.code_interpreter("print(2)")
        return a, b

    asyncio.run(two_calls())
    asyncio.run(two_calls())
    assert len(tool_module._POOLS) == 2, "one pool per event loop"
    assert all(name.startswith("t1-") for name in FakeClient.created)
    assert len(FakeClient.created) == 2, "each loop created one environment and reused it"


def test_execution_command_cleans_its_temp_dir_and_env_survives_timeouts(tool_module):
    FakeClient.script = [(137, ""), (0, "after\n")]
    tool = tool_module.AteEnvCodeTool({"endpoint": "fake:2", "max_envs": 1, "timeout_s": 5}, {})

    async def run():
        first = await tool.code_interpreter("while True: pass")
        second = await tool.code_interpreter("print('after')")
        return first, second

    first, second = asyncio.run(run())
    assert first.startswith("TimeoutError")
    assert second == "after\n"
    assert FakeClient.deleted == [], "a timed-out environment goes back to the pool"
    cmd = FakeClient.created and tool_module._POOLS[next(iter(tool_module._POOLS))]._all[0].commands[0]
    assert "mktemp -d" in cmd and "trap 'rm -rf \"$d\"' EXIT" in cmd


def test_broken_environment_is_deleted_and_replaced(tool_module):
    tool = tool_module.AteEnvCodeTool({"endpoint": "fake:3", "max_envs": 1}, {})

    async def run():
        pool = tool_module._pool("fake:3", 1, None, "verl")
        env = await pool.acquire()
        await pool.release(env, broken=True)
        return await tool.code_interpreter("print(1)")

    asyncio.run(run())
    assert len(FakeClient.deleted) == 1 and len(FakeClient.created) == 2


def test_exit_hook_deletes_pooled_environments(tool_module):
    tool = tool_module.AteEnvCodeTool({"endpoint": "fake:4", "max_envs": 2}, {})
    asyncio.run(tool.code_interpreter("print(1)"))
    assert FakeClient.deleted == []
    tool_module._delete_pools_at_exit()
    assert FakeClient.deleted == FakeClient.created
    assert tool_module._POOLS == {}


def test_auth_token_comes_from_the_environment(tool_module, monkeypatch):
    monkeypatch.setenv(tool_module.AUTH_TOKEN_ENV, "secret")

    async def run():
        await tool_module._pool("fake:5", 1, None, "verl").acquire()

    asyncio.run(run())
    assert FakeClient.auth_tokens[-1] == "secret"
