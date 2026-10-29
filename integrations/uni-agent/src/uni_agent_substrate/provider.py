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
"""uni-agent sandbox provider ``ate_env``: one Agent Substrate environment per episode.

The provider implements :class:`uni_agent.sandbox.base.Sandbox` on top of the
``ate_env`` Python client (the Substrate environment API served by ate-env-api).
Each ``start()`` creates an environment from a digest-pinned rootfs image on a base
ActorTemplate; tools run commands with ``Env.start_process`` and read/write files
with the native file RPCs; ``stop()`` deletes the environment.

Long commands (SWE test suites run for many minutes) are collected in bounded
streaming windows and re-attached with byte offsets, so no single gRPC stream
outlives the atenet router's route timeout. ``open_shell`` returns a shell handle
that persists ``cd`` and exported variables across commands without tmux, which
uni-agent would otherwise try to install inside an environment that has no egress.

Config (``sandbox_kwargs`` in the task config, all optional):
  endpoint         ate-env-api address (default ``ate-env-api.ate-env.svc:7777``)
  atespace         atespace of the environments (default ``ate-env``)
  template         base ActorTemplate name (default: the server default template)
  image_digests    JSON file mapping tag refs to digest-pinned refs (default
                   ``$ATE_ENV_IMAGE_DIGESTS``); images already pinned pass through
  actor_prefix     environment id prefix (default ``ua``)
  stream_window_s  max seconds one output stream stays open (default 240)
  auth_token_env   env var holding a bearer token for ate-env-api (default ``SUBSTRATE_ENV_AUTH_TOKEN``, the repo's name)
"""

from __future__ import annotations

import asyncio
import functools
import json
import logging
import os
import posixpath
import shlex
import time
import uuid
import weakref
from pathlib import Path
from typing import Any

from ate_env import Client, Env
from ate_env.errors import EnvError, NotFoundError, RpcError
from ate_env.types import EnvironmentStatus, Signal
from uni_agent.sandbox.base import ExecResult, Sandbox, SandboxConfig
from uni_agent.sandbox.registry import register_sandbox

logger = logging.getLogger(__name__)

DEFAULT_ENDPOINT = "ate-env-api.ate-env.svc:7777"
DEFAULT_ATESPACE = "ate-env"
_ALIVE = {
    EnvironmentStatus.RESUMING,
    EnvironmentStatus.RUNNING,
    EnvironmentStatus.SUSPENDING,
    EnvironmentStatus.SUSPENDED,
    EnvironmentStatus.PAUSING,
    EnvironmentStatus.PAUSED,
}
_STREAM_RETRIES = 3

# One client per (event loop, endpoint, token): uni-agent runs tasks in Ray task processes, each with
# its own loop, and grpc.aio channels are bound to a loop. Keyed on the loop object itself (weakly) so
# a garbage-collected loop takes its clients with it instead of leaving a closed client under a reused id.
_CLIENTS: weakref.WeakKeyDictionary[asyncio.AbstractEventLoop, dict[tuple[str, str], Client]] = weakref.WeakKeyDictionary()


def _client_for(endpoint: str, token: str | None) -> Client:
    per_loop = _CLIENTS.setdefault(asyncio.get_running_loop(), {})
    key = (endpoint, token or "")
    client = per_loop.get(key)
    if client is None:
        client = Client(endpoint, auth_token=token) if token else Client(endpoint)
        per_loop[key] = client
    return client


@functools.lru_cache(maxsize=16)
def _load_digest_map(path: str | None) -> dict[str, str]:
    """tag -> digest-pinned reference map; read once per path per process (one sandbox per episode)."""
    if not path:
        return {}
    with open(path, encoding="utf-8") as fh:
        data = json.load(fh)
    if not isinstance(data, dict):
        raise ValueError(f"image digest map {path!r} must be a JSON object")
    return {str(k): str(v) for k, v in data.items()}


def resolve_image(image: str, digests: dict[str, str]) -> str:
    """Return a digest-pinned reference for ``image`` or raise.

    Substrate rejects tag references for ``rootfs_image``; the recipe's
    ``resolve_images.sh`` writes the tag -> digest map this consults.
    """
    if "@sha256:" in image:
        return image
    pinned = digests.get(image)
    if pinned is None and "/" in image and not image.startswith("docker.io/"):
        pinned = digests.get("docker.io/" + image)
    if pinned is None:
        raise RuntimeError(
            f"image {image!r} is not digest-pinned and not in the image digest map; "
            "run integrations/uni-agent/scripts/resolve_images.sh for this dataset slice"
        )
    return pinned


@register_sandbox("ate_env")
class AteEnvSandbox(Sandbox):
    """Agent Substrate environment as a uni-agent sandbox."""

    supports_shell = True

    def __init__(
        self,
        *,
        image: str,
        runtime_timeout: float = 3600.0,
        endpoint: str = DEFAULT_ENDPOINT,
        atespace: str = DEFAULT_ATESPACE,
        template: str | None = None,
        image_digests: str | None = None,
        actor_prefix: str = "ua",
        stream_window_s: float = 240.0,
        auth_token_env: str = "SUBSTRATE_ENV_AUTH_TOKEN",
        **ignored: Any,
    ) -> None:
        if not image:
            raise ValueError("ate_env provider requires a sandbox image")
        if ignored:
            logger.debug("ate_env provider ignoring sandbox kwargs %s", sorted(ignored))
        self.image = image
        self.runtime_timeout = float(runtime_timeout)
        self.endpoint = endpoint
        self.atespace = atespace
        self.template = template or None
        self._digests = _load_digest_map(image_digests or os.environ.get("ATE_ENV_IMAGE_DIGESTS"))
        self.actor_prefix = actor_prefix
        self.stream_window_s = float(stream_window_s)
        self._token = os.environ.get(auth_token_env) or None
        self._env: Env | None = None
        self.env_id: str | None = None

    @classmethod
    def from_config(cls, config: SandboxConfig) -> AteEnvSandbox:
        if getattr(config, "image_mounts", None):
            raise ValueError("ate_env provider does not support image_mounts; bake tools into the rootfs image")
        return cls(image=config.image or "", runtime_timeout=config.runtime_timeout, **config.sandbox_kwargs)

    # ----- lifecycle -----

    async def start(self) -> None:
        client = _client_for(self.endpoint, self._token)
        rootfs = resolve_image(self.image, self._digests)
        env_id = f"{self.actor_prefix}-{uuid.uuid4().hex[:12]}"
        t0 = time.monotonic()
        self._env = await client.create(
            env_id, atespace=self.atespace, template_name=self.template, rootfs_image=rootfs
        )
        self.env_id = env_id
        # First command wakes the environment (cold images pay the template derivation and pull).
        res = await self._exec(["true"], timeout=min(300.0, self.runtime_timeout))
        if res.exit_code != 0:
            raise RuntimeError(f"environment {env_id} did not become ready: exit {res.exit_code} {res.stderr[-300:]}")
        logger.info("ate_env sandbox %s ready in %.1fs (image %s)", env_id, time.monotonic() - t0, rootfs)

    async def stop(self) -> None:
        env, self._env = self._env, None
        if env is None:
            return
        try:
            await env.delete()
        except NotFoundError:
            pass
        except EnvError as exc:
            logger.warning("ate_env sandbox %s delete failed: %s", self.env_id, exc)

    async def is_alive(self) -> bool:
        if self._env is None:
            return False
        try:
            return (await self._env.info()).status in _ALIVE
        except Exception:
            return False

    def _require_env(self) -> Env:
        if self._env is None:
            raise RuntimeError("ate_env sandbox is not started")
        return self._env

    # ----- data plane -----

    async def _exec(
        self,
        argv: list[str],
        *,
        timeout: float | None = None,
        workdir: str | None = None,
        env: dict[str, str] | None = None,
    ) -> ExecResult:
        limit = float(timeout) if timeout else self.runtime_timeout
        proc = await self._require_env().start_process(list(argv), cwd=workdir or "", env=env, timeout=limit)
        deadline = time.monotonic() + limit + 5.0
        out, err = bytearray(), bytearray()
        exit_info = None
        retries = 0
        while exit_info is None:
            remaining = deadline - time.monotonic()
            if remaining <= 0:
                await _kill_quietly(proc)
                raise TimeoutError(f"command exceeded {limit:g}s: {argv[:3]}")
            window = min(self.stream_window_s, remaining)
            try:
                async with asyncio.timeout(window):
                    async for msg in proc.output(follow=True, stdout_offset=len(out), stderr_offset=len(err)):
                        if msg.stdout:
                            out += msg.stdout
                        if msg.stderr:
                            err += msg.stderr
                        if msg.exit is not None:
                            exit_info = msg.exit
                            break
                retries = 0
            except TimeoutError:
                continue  # window elapsed while the process runs: re-attach with offsets
            except RpcError as exc:
                retries += 1
                if retries > _STREAM_RETRIES:
                    raise
                logger.warning("ate_env output stream for %s interrupted (%s); re-attaching", self.env_id, exc)
                await asyncio.sleep(min(2.0 * retries, 10.0))
        code = exit_info.exit_code
        if code == 137 and time.monotonic() >= deadline - 5.0 - 1.0:
            raise TimeoutError(f"command killed at its {limit:g}s timeout: {argv[:3]}")
        return ExecResult(exit_code=code, stdout=_text(out), stderr=_text(err))

    async def read_file(self, path: str) -> bytes:
        return await self._require_env().read_file_bytes(path)

    async def write_file(self, path: str, content: bytes | str) -> None:
        data = content.encode("utf-8") if isinstance(content, str) else bytes(content)
        parent = posixpath.dirname(path)
        if parent and parent != "/":
            await self._exec(["mkdir", "-p", parent], timeout=30)   # awaited: write_file must not race the mkdir
        await self._require_env().write_file(path, data)

    async def upload_file(self, local_file: Path | str, remote_file: str) -> None:
        await self.write_file(remote_file, Path(local_file).read_bytes())

    async def download_file(self, remote_file: str, local_file: Path | str) -> None:
        data = await self.read_file(remote_file)
        dst = Path(local_file)
        dst.parent.mkdir(parents=True, exist_ok=True)
        dst.write_bytes(data)

    async def open_shell(self, *, cwd: str | None = None, env: dict[str, str] | None = None) -> AteEnvShell:
        return AteEnvShell(self, cwd=cwd or "/", env=env or {})


class AteEnvShell:
    """Shell handle for :meth:`AteEnvSandbox.open_shell`.

    Each ``run`` executes one ``bash -c`` command that first restores the working
    directory and exported variables saved by the previous command, then saves them
    again, so ``cd`` and ``export`` persist across tool calls. Background jobs and
    shell functions do not survive between commands.
    """

    def __init__(self, sandbox: AteEnvSandbox, *, cwd: str, env: dict[str, str]) -> None:
        self._sandbox = sandbox
        self._cwd = cwd
        self._env = dict(env)
        self._state = f"/tmp/.ua_shell_{uuid.uuid4().hex[:8]}"

    async def start(self) -> None:
        return None

    async def run(self, command: str, *, timeout: float = 120.0) -> ExecResult:
        state = shlex.quote(self._state)
        script = "\n".join(
            [
                f"__s={state}; mkdir -p \"$__s\"",
                f"cd \"$(cat \"$__s/cwd\" 2>/dev/null || printf %s {shlex.quote(self._cwd)})\" 2>/dev/null || cd /",
                "[ -f \"$__s/env\" ] && . \"$__s/env\" 2>/dev/null",
                "unset __s",
                command,
                "__rc=$?",
                f"pwd > {state}/cwd",
                f"declare -px 2>/dev/null | grep -vE '^declare -x (PWD|OLDPWD|SHLVL|_|__rc|__s)=' > {state}/env",
                "exit $__rc",
            ]
        )
        return await self._sandbox.exec(["bash", "-c", script], timeout=timeout, env=self._env or None)

    async def close(self) -> None:
        try:
            await self._sandbox.exec(["rm", "-rf", self._state], timeout=30)
        except Exception:  # best effort; the environment is deleted at stop()
            pass


def _text(data: bytearray) -> str:
    return bytes(data).decode("utf-8", errors="replace")


async def _kill_quietly(proc: Any) -> None:
    """SIGKILL without waiting: the guest reaps the process, and waiting would reopen a stream."""
    try:
        await proc.signal(Signal.KILL)
    except Exception:
        pass
