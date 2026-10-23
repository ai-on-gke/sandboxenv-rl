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

"""verl tool that executes Python in Agent Substrate environments via ate-env-api.

Drop-in replacement for the tutorial's SandboxTool (same tool schema, so the
same prompts/data work). Each code execution runs inside an isolated
environment created through `ate-env-api` (gRPC, Substrate environment API):
a Substrate actor, i.e. a gVisor sandbox on a Substrate worker, restored from
the template's snapshot.

Lifecycle: verl's ToolAgentLoop calls create() -> execute() -> release() for
every tool call, so creating an environment per call would cost a create and a
wake each time. Instead the tool keeps a warm pool: an environment is acquired for
the duration of one execute(), the snippet runs in a fresh temp dir, and the
environment goes back to the pool. Pool size = `max_envs` (config).

Config keys (tool_config.yaml):
  endpoint      ate-env-api address, e.g. ate-env-api.ate-env.svc:7777
  max_envs      warm-pool size (default 16)
  timeout_s     per-execution timeout (default 30)
  rootfs_image  optional digest-pinned image to run instead of the guest image
  python        interpreter inside the environment (default python3)
"""
from __future__ import annotations

import asyncio
import logging
import re
import uuid
from typing import Any

from transformers.utils import get_json_schema
from verl.tools.base_tool import BaseTool, OpenAIFunctionToolSchema, ToolResponse

logger = logging.getLogger(__name__)


class _EnvPool:
    """Warm pool of ate-env environments shared by all tool instances in a process."""

    def __init__(self, endpoint: str, max_envs: int, rootfs_image: str | None):
        from ate_env import Client  # grpc client, installed in the rollout image

        self._client = Client(endpoint)
        self._rootfs = rootfs_image or None
        self._sem = asyncio.Semaphore(max_envs)
        self._idle: list[Any] = []
        self._all: list[Any] = []
        self._lock = asyncio.Lock()

    async def acquire(self):
        await self._sem.acquire()
        async with self._lock:
            if self._idle:
                return self._idle.pop()
        try:
            env = await self._client.create(f"verl-{uuid.uuid4().hex[:12]}", rootfs_image=self._rootfs)
        except Exception:
            self._sem.release()
            raise
        async with self._lock:
            self._all.append(env)
        return env

    async def release(self, env, broken: bool = False):
        if broken:
            try:
                await env.delete()
            except Exception as e:  # noqa: BLE001
                logger.warning("ate_env: delete failed: %s", e)
            async with self._lock:
                self._all = [e for e in self._all if e is not env]
        else:
            async with self._lock:
                self._idle.append(env)
        self._sem.release()

    async def close(self):
        async with self._lock:
            envs, self._all, self._idle = list(self._all), [], []
        for env in envs:
            try:
                await env.delete()
            except Exception:  # noqa: BLE001
                pass
        await self._client.close()


_POOLS: dict[tuple, _EnvPool] = {}


def _pool(endpoint: str, max_envs: int, rootfs_image: str | None) -> _EnvPool:
    key = (endpoint, max_envs, rootfs_image or "")
    if key not in _POOLS:
        _POOLS[key] = _EnvPool(endpoint, max_envs, rootfs_image)
    return _POOLS[key]


class AteEnvCodeTool(BaseTool):
    def __init__(self, config: dict, tool_schema: OpenAIFunctionToolSchema):
        super().__init__(config, tool_schema)
        self.code_pattern = re.compile(r"```py(?:thon)?(.*?)```", re.DOTALL)
        self.endpoint = config.get("endpoint", "ate-env-api.ate-env.svc:7777")
        self.max_envs = int(config.get("max_envs", 16))
        self.timeout_s = float(config.get("timeout_s", 30))
        self.rootfs_image = config.get("rootfs_image") or None
        self.python = config.get("python", "python3")

    async def code_interpreter(self, code: str) -> str:
        """Execute the code in the sandbox.

        Args:
            code: The code to be executed.

        Returns:
            str: The output of the code execution.
        """
        pool = _pool(self.endpoint, self.max_envs, self.rootfs_image)
        env = await pool.acquire()
        broken = False
        try:
            # fresh working dir per execution; heredoc keeps quoting simple
            cmd = f"d=$(mktemp -d) && cd \"$d\" && {self.python} - <<'VERL_EOF'\n{code}\nVERL_EOF"
            r = await env.shell(cmd, timeout=self.timeout_s)
            out = (r.stdout or "") + (r.stderr or "")
            return out if out.strip() else f"(no output, exit code {r.exit_code})"
        except asyncio.TimeoutError:
            broken = True
            return f"TimeoutError: execution exceeded {self.timeout_s}s"
        except Exception as e:  # noqa: BLE001
            broken = True
            logger.warning("ate_env tool failure: %s", e)
            return f"SandboxError: {type(e).__name__}: {e}"
        finally:
            await pool.release(env, broken=broken)

    def get_openai_tool_schema(self) -> OpenAIFunctionToolSchema:
        return OpenAIFunctionToolSchema(**get_json_schema(self.code_interpreter))

    async def execute(self, instance_id: str, parameters: dict, **kwargs) -> tuple[ToolResponse, float, dict]:
        code = parameters.get("code", "")
        m = self.code_pattern.findall(code)
        if m:
            code = m[0].strip()
        # Same convenience as the tutorial: print the last bare expression.
        lines = code.split("\n")
        for i, line in reversed(list(enumerate(lines))):
            if line.strip() == "":
                continue
            if not line.lstrip().startswith("print") and not line.startswith((" ", "\t")):
                lines[i] = f"print({line})"
            break
        result = await self.code_interpreter("\n".join(lines))
        return ToolResponse(text=result[-4000:]), 0.0, {}
