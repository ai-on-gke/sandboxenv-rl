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
environment goes back to the pool. Pool size = `max_envs` (config). Pools are
per event loop (a grpc.aio channel and an asyncio.Semaphore belong to one loop)
and are deleted at interpreter exit, so `deploy.sh down` normally finds nothing
to purge.

Config keys (tool_config.yaml):
  endpoint      ate-env-api address, e.g. ate-env-api.ate-env.svc:7777
  max_envs      warm-pool size (default 16)
  timeout_s     per-execution timeout (default 30)
  rootfs_image  optional digest-pinned image to run instead of the guest image
  python        interpreter inside the environment (default python3)
  env_prefix    environment name prefix (default verl); give each run its own so
                concurrent runs on one cluster never purge each other's pools

Environment: SUBSTRATE_ENV_AUTH_TOKEN, when set, is sent as the bearer token on
every ate-env-api call (same variable as the repository's other clients).
"""
from __future__ import annotations

import ast
import asyncio
import atexit
import logging
import os
import re
import uuid
from typing import Any

from transformers.utils import get_json_schema
from verl.tools.base_tool import BaseTool, OpenAIFunctionToolSchema, ToolResponse

logger = logging.getLogger(__name__)

AUTH_TOKEN_ENV = "SUBSTRATE_ENV_AUTH_TOKEN"
TIMEOUT_EXIT_CODE = 137  # the guest kills the process with SIGKILL at the deadline; Env.shell does not raise


def auth_token() -> str | None:
    return os.environ.get(AUTH_TOKEN_ENV) or None


def maybe_print_last_expression(code: str) -> str:
    """Wrap the last non-empty top-level line in print(...) when it is a bare expression.

    Same convenience as the tutorial's tool, limited to lines that parse as an expression:
    statements such as `import os` or `x = f()` are left alone.
    """
    lines = code.split("\n")
    for i, line in reversed(list(enumerate(lines))):
        stripped = line.strip()
        if stripped == "":
            continue
        if line.startswith((" ", "\t")) or stripped.startswith("print"):
            break
        try:
            ast.parse(stripped, mode="eval")
        except SyntaxError:
            break
        lines[i] = f"print({stripped})"
        break
    return "\n".join(lines)


def result_text(exit_code: int | None, stdout: str | None, stderr: str | None, timeout_s: float) -> str:
    """What the model sees for one execution."""
    if exit_code == TIMEOUT_EXIT_CODE:
        return f"TimeoutError: execution exceeded {timeout_s}s"
    out = (stdout or "") + (stderr or "")
    return out if out.strip() else f"(no output, exit code {exit_code})"


class _EnvPool:
    """Warm pool of ate-env environments shared by the tool instances on one event loop."""

    def __init__(self, endpoint: str, max_envs: int, rootfs_image: str | None, prefix: str):
        from ate_env import Client  # grpc client, installed in the rollout image

        self.endpoint = endpoint
        self.prefix = prefix
        self._client = Client(endpoint, auth_token=auth_token())
        self._rootfs = rootfs_image or None
        self._sem = asyncio.Semaphore(max_envs)
        self._idle: list[Any] = []
        self._all: list[Any] = []
        self._lock = asyncio.Lock()

    @property
    def env_ids(self) -> list[str]:
        return [env.id for env in self._all]

    async def acquire(self):
        await self._sem.acquire()
        async with self._lock:
            if self._idle:
                return self._idle.pop()
        try:
            env = await self._client.create(f"{self.prefix}-{uuid.uuid4().hex[:12]}", rootfs_image=self._rootfs)
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


def _pool(endpoint: str, max_envs: int, rootfs_image: str | None, prefix: str) -> _EnvPool:
    loop = asyncio.get_running_loop()
    key = (endpoint, max_envs, rootfs_image or "", prefix, id(loop))
    if key not in _POOLS:
        _POOLS[key] = _EnvPool(endpoint, max_envs, rootfs_image, prefix)
    return _POOLS[key]


def _delete_pools_at_exit() -> None:
    """Delete every pooled environment when the process exits.

    The pools' event loops are gone by now, so this uses a fresh loop and a fresh client and
    deletes the environments by id. Best effort: a SIGKILLed worker skips atexit, which is what
    `deploy.sh down` is for.
    """
    by_endpoint: dict[str, list[str]] = {}
    for pool in _POOLS.values():
        by_endpoint.setdefault(pool.endpoint, []).extend(pool.env_ids)
    _POOLS.clear()
    if not any(by_endpoint.values()):
        return
    from ate_env import Client

    async def delete_all():
        for endpoint, ids in by_endpoint.items():
            client = Client(endpoint, auth_token=auth_token())
            try:
                for env_id in ids:
                    try:
                        await client.delete(env_id)
                    except Exception:  # noqa: BLE001
                        pass
            finally:
                await client.close()

    try:
        asyncio.run(delete_all())
    except Exception as e:  # noqa: BLE001
        logger.warning("ate_env: pool cleanup at exit failed: %s", e)


atexit.register(_delete_pools_at_exit)


class AteEnvCodeTool(BaseTool):
    def __init__(self, config: dict, tool_schema: OpenAIFunctionToolSchema):
        super().__init__(config, tool_schema)
        self.code_pattern = re.compile(r"```py(?:thon)?(.*?)```", re.DOTALL)
        self.endpoint = config.get("endpoint", "ate-env-api.ate-env.svc:7777")
        self.max_envs = int(config.get("max_envs", 16))
        self.timeout_s = float(config.get("timeout_s", 30))
        self.rootfs_image = config.get("rootfs_image") or None
        self.python = config.get("python", "python3")
        self.env_prefix = str(config.get("env_prefix", "verl"))

    async def code_interpreter(self, code: str) -> str:
        """Execute the code in the sandbox.

        Args:
            code: The code to be executed.

        Returns:
            str: The output of the code execution.
        """
        pool = _pool(self.endpoint, self.max_envs, self.rootfs_image, self.env_prefix)
        env = await pool.acquire()
        broken = False
        try:
            # Fresh working dir per execution, removed when the shell exits; heredoc keeps quoting simple.
            cmd = (
                f"d=$(mktemp -d) && trap 'rm -rf \"$d\"' EXIT && cd \"$d\" && "
                f"{self.python} - <<'VERL_EOF'\n{code}\nVERL_EOF"
            )
            r = await env.shell(cmd, timeout=self.timeout_s)
            # A timed-out process was SIGKILLed by the guest (exit 137); the environment is reusable.
            return result_text(r.exit_code, r.stdout, r.stderr, self.timeout_s)
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
        result = await self.code_interpreter(maybe_print_last_expression(code))
        return ToolResponse(text=result[-4000:]), 0.0, {}
