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

# pylint: disable=missing-module-docstring,missing-function-docstring,line-too-long
# pylint: disable=too-many-instance-attributes,too-many-arguments,too-many-positional-arguments
# pylint: disable=too-many-locals,too-many-branches,too-many-statements
# pylint: disable=broad-exception-caught,raise-missing-from,duplicate-code

from __future__ import annotations

import asyncio
import builtins
import logging
import queue
import threading
import time
from collections.abc import AsyncIterator, Iterator
from typing import Any

import grpc

from ..client import Client as AteClient
from ..config import TracerConfig
from ..env import Env as AteEnv
from ..errors import (
    CommandStartError,
    InfrastructureError,
    InvalidArgumentError,
    NotFoundError,
    RpcError,
    SandboxProtocolError,
    SandboxUnavailableError,
)
from ..loop_runner import LoopRunner
from ..telemetry import _IN_TRACKED_EXEC, BenchTelemetry, _wrap_env_handle
from ..types import ExecResult, ShellResult
from .base import RuntimeGuestHook

logger = logging.getLogger("ate_env.runtime.substrate_env_client")


class SubstrateEnvClientRuntime(RuntimeGuestHook):
    """Substrate guest hook backed directly by the official ate_env.Env handle.

    Leverages ate_env.Client and ate_env.Env for process execution, streaming,
    and chunked file I/O over gRPC.
    """

    _supports_exec_name = True

    def __init__(
        self,
        env_id: str,
        *,
        endpoint: str | None = None,
        atespace: str = "ate-env",
        client: AteClient | None = None,
        env: AteEnv | None = None,
        loop_runner: LoopRunner | None = None,
        tracer_config: TracerConfig | None = None,
        telemetry: BenchTelemetry | None = None,
        source: str = "direct",
        cache_state: str = "unknown",
        acquired_at_monotonic: float | None = None,
    ):
        if not env_id:
            raise ValueError("env_id is required for SubstrateEnvClientRuntime")
        self.env_id = env_id
        self.endpoint = endpoint or "localhost:7777"
        self.atespace = atespace
        # Used only for a client the runtime builds itself; a shared client
        # keeps its own tracing configuration.
        self.tracer_config = tracer_config

        self._loop_runner = loop_runner
        self._owns_loop_runner = loop_runner is None
        self._lock = threading.Lock()
        self._telemetry: BenchTelemetry | None = telemetry
        self._source: str = source or "direct"
        self._cache_state: str = cache_state or "unknown"
        self._acquired_at_monotonic: float | None = acquired_at_monotonic
        self._first_exec_done: bool = False

        if env is not None:
            self._env = env
            self._client = env._client
            self._owns_client = False
        elif client is not None:
            self._client = client
            self._env = client.env(env_id, atespace=atespace)
            self._owns_client = False
        else:
            self._client = None
            self._env = None
            self._owns_client = True
        if (
            self._env is not None
            and self._telemetry is not None
            and getattr(self._telemetry, "mode", "off") != "off"
        ):
            _wrap_env_handle(self._env, self._telemetry)
        self._closed = False

    @property
    def env(self) -> AteEnv:
        self._check_open()
        with self._lock:
            if self._env is None:
                if self._client is None:
                    self._client = AteClient(self.endpoint, tracer_config=self.tracer_config)
                self._env = self._client.env(self.env_id, atespace=self.atespace)
            if (
                self._telemetry is not None
                and getattr(self._telemetry, "mode", "off") != "off"
            ):
                _wrap_env_handle(self._env, self._telemetry)
            return self._env

    def _get_loop_runner(self) -> LoopRunner:
        with self._lock:
            if self._loop_runner is None:
                self._loop_runner = LoopRunner(name="ate-substrate-runtime-sync")
                self._owns_loop_runner = True
            return self._loop_runner

    async def close_async(self) -> None:
        """Close client if owned."""
        if not self._closed:
            self._closed = True
            if self._owns_client and self._client:
                await self._client.close()
                self._client = None
            if self._owns_loop_runner and self._loop_runner:
                self._loop_runner.stop()
                self._loop_runner = None

    def close(self) -> None:
        if not self._closed:
            self._closed = True
            if self._loop_runner:
                if self._owns_client and self._client:
                    self._loop_runner.run(self._client.close())
                    self._client = None
                if self._owns_loop_runner:
                    self._loop_runner.stop()
                    self._loop_runner = None
            elif self._owns_client and self._client:
                self._sync(self._client.close())

    def _check_open(self) -> None:
        if self._closed:
            raise RuntimeError(f"runtime for environment {self.env_id} is closed")

    def _sync(self, coro: Any) -> Any:
        return self._get_loop_runner().run(coro)

    def _record_exec_telemetry(
        self,
        *,
        command: str | list[str],
        name: str,
        dur_ms: float,
        t_end: float,
        ok: bool,
        exit_code: int | None,
        timed_out: bool,
        err: str = "",
    ) -> None:
        if self._telemetry is None or getattr(self._telemetry, "mode", "off") == "off":
            return
        with self._lock:
            is_first = not self._first_exec_done
            self._first_exec_done = True

        cmd_seq: tuple[str, ...] = (
            ("sh", "-c", command) if isinstance(command, str) else tuple(str(x) for x in command)
        )
        if is_first and (
            self._acquired_at_monotonic is not None
            or self._source in ("warm_pool", "cold_create")
        ):
            first_op = "wake" if self._source == "warm_pool" else "ttfi"
            first_dur_ms = (
                max(dur_ms, (t_end - self._acquired_at_monotonic) * 1000.0)
                if self._acquired_at_monotonic is not None
                else dur_ms
            )
            self._telemetry.record_op(
                first_op,
                first_dur_ms,
                ok=ok,
                env_id=self.env_id,
                step_name=first_op,
                command=cmd_seq,
                exit_code=exit_code,
                err=err,
                source=self._source,
                cache_state=self._cache_state,
                timed_out=timed_out,
                emit_trace=False,
            )

        self._telemetry.record_op(
            "exec",
            dur_ms,
            ok=ok,
            env_id=self.env_id,
            step_name=name or "exec",
            command=cmd_seq,
            exit_code=exit_code,
            err=err,
            source=self._source,
            cache_state=self._cache_state,
            timed_out=timed_out,
        )

    async def exec_async(
        self,
        command: str | list[str],
        cwd: str = "",
        env: dict[str, str] | None = None,
        timeout_s: float = 120.0,
        *,
        name: str = "",
    ) -> ExecResult:
        """Native async execution honoring command contract: argv list runs directly, string runs in shell."""
        self._check_open()
        t0 = time.monotonic()
        grace_s = 5.0
        active_env = self.env

        token = _IN_TRACKED_EXEC.set(True)
        try:
            active_proc = None
            try:
                if isinstance(command, (list, tuple)):
                    argv = list(command)
                    if not argv:
                        raise ValueError("command must not be empty")

                    async def _run_argv() -> ShellResult:
                        nonlocal active_proc
                        proc = await active_env.start_process(
                            argv, cwd=cwd, env=env, timeout=timeout_s
                        )
                        active_proc = proc
                        stdout = bytearray()
                        stderr = bytearray()
                        exit_info = None
                        async for out in proc.output(follow=True):
                            if out.stdout is not None:
                                stdout.extend(out.stdout)
                            elif out.stderr is not None:
                                stderr.extend(out.stderr)
                            elif out.exit is not None:
                                exit_info = out.exit
                        if exit_info is None:
                            raise RuntimeError("ate_env: output stream ended before process exited")
                        return ShellResult(
                            stdout=stdout.decode("utf-8", errors="replace"),
                            stderr=stderr.decode("utf-8", errors="replace"),
                            exit_code=exit_info.exit_code,
                        )

                    if hasattr(asyncio, "timeout"):
                        async with asyncio.timeout(timeout_s + grace_s):
                            shell_res = await _run_argv()
                    else:
                        shell_res = await asyncio.wait_for(_run_argv(), timeout=timeout_s + grace_s)
                else:
                    if hasattr(asyncio, "timeout"):
                        async with asyncio.timeout(timeout_s + grace_s):
                            shell_res = await active_env.shell(
                                command, cwd=cwd, env=env, timeout=timeout_s
                            )
                    else:
                        shell_res = await asyncio.wait_for(
                            active_env.shell(command, cwd=cwd, env=env, timeout=timeout_s),
                            timeout=timeout_s + grace_s,
                        )
            except (builtins.TimeoutError, asyncio.TimeoutError):
                if active_proc is not None:
                    try:
                        await active_proc.kill()
                    except Exception:
                        pass
                t_end = time.monotonic()
                duration = t_end - t0
                self._record_exec_telemetry(
                    command=command,
                    name=name,
                    dur_ms=duration * 1000.0,
                    t_end=t_end,
                    ok=False,
                    exit_code=None,
                    timed_out=True,
                    err="TimeoutError",
                )
                return ExecResult(
                    exit_code=None,
                    stdout="",
                    stderr="",
                    duration_s=duration,
                    timed_out=True,
                )

            t_end = time.monotonic()
            duration = t_end - t0
            # 137 = 128 + 9 (SIGKILL sent by guest when guest-side timeout elapsed).
            # Note: checking exit_code == 137 and duration >= timeout_s is a heuristic;
            # external kills (like OOM) can also produce exit_code 137.
            timed_out = shell_res.exit_code == 137 and duration >= timeout_s
            res = ExecResult(
                exit_code=None if timed_out else shell_res.exit_code,
                stdout=shell_res.stdout,
                stderr=shell_res.stderr,
                duration_s=duration,
                timed_out=timed_out,
            )
            self._record_exec_telemetry(
                command=command,
                name=name,
                dur_ms=duration * 1000.0,
                t_end=t_end,
                ok=bool(res.ok),
                exit_code=res.exit_code,
                timed_out=timed_out,
                err="TimeoutError" if timed_out else "",
            )
            return res
        except asyncio.CancelledError:
            t_end = time.monotonic()
            self._record_exec_telemetry(
                command=command,
                name=name,
                dur_ms=(t_end - t0) * 1000.0,
                t_end=t_end,
                ok=False,
                exit_code=None,
                timed_out=False,
                err="CancelledError",
            )
            raise
        except InvalidArgumentError as e:
            t_end = time.monotonic()
            self._record_exec_telemetry(
                command=command,
                name=name,
                dur_ms=(t_end - t0) * 1000.0,
                t_end=t_end,
                ok=False,
                exit_code=None,
                timed_out=False,
                err=f"SandboxProtocolError: {e}",
            )
            raise SandboxProtocolError(str(e), sandbox_id=self.env_id) from e
        except NotFoundError as e:
            t_end = time.monotonic()
            self._record_exec_telemetry(
                command=command,
                name=name,
                dur_ms=(t_end - t0) * 1000.0,
                t_end=t_end,
                ok=False,
                exit_code=None,
                timed_out=False,
                err=f"NotFoundError: {e}",
            )
            msg = str(e).lower()
            if "environment" in msg or "actor" in msg:
                raise SandboxUnavailableError(str(e), sandbox_id=self.env_id) from e
            raise CommandStartError(str(e), sandbox_id=self.env_id) from e
        except RpcError as e:
            t_end = time.monotonic()
            self._record_exec_telemetry(
                command=command,
                name=name,
                dur_ms=(t_end - t0) * 1000.0,
                t_end=t_end,
                ok=False,
                exit_code=None,
                timed_out=e.code == grpc.StatusCode.DEADLINE_EXCEEDED,
                err=f"RpcError: {e}",
            )
            status_name = f"grpc {e.code.name}" if e.code else "grpc RpcError"
            if e.code in (
                grpc.StatusCode.NOT_FOUND,
                grpc.StatusCode.UNAVAILABLE,
                grpc.StatusCode.DEADLINE_EXCEEDED,
                grpc.StatusCode.INTERNAL,
                grpc.StatusCode.ABORTED,
            ):
                raise SandboxUnavailableError(
                    str(e), sandbox_id=self.env_id, status=status_name
                ) from e
            raise SandboxProtocolError(str(e), sandbox_id=self.env_id, status=status_name) from e
        except InfrastructureError as e:
            t_end = time.monotonic()
            self._record_exec_telemetry(
                command=command,
                name=name,
                dur_ms=(t_end - t0) * 1000.0,
                t_end=t_end,
                ok=False,
                exit_code=None,
                timed_out=False,
                err=f"{type(e).__name__}: {e}",
            )
            raise
        except Exception as e:
            t_end = time.monotonic()
            self._record_exec_telemetry(
                command=command,
                name=name,
                dur_ms=(t_end - t0) * 1000.0,
                t_end=t_end,
                ok=False,
                exit_code=None,
                timed_out=False,
                err=f"{type(e).__name__}: {e}",
            )
            raise InfrastructureError(str(e), sandbox_id=self.env_id) from e
        finally:
            _IN_TRACKED_EXEC.reset(token)

    def exec(
        self,
        command: str | list[str],
        cwd: str = "",
        env: dict[str, str] | None = None,
        timeout_s: float = 120.0,
        *,
        name: str = "",
    ) -> ExecResult:
        """Synchronous wrapper for exec_async."""
        return self._sync(
            self.exec_async(command, cwd=cwd, env=env, timeout_s=timeout_s, name=name)
        )

    async def stream_process_async(
        self,
        argv: list[str],
        cwd: str = "",
        env: dict[str, str] | None = None,
    ) -> AsyncIterator[str]:
        """Stream real-time output chunks from running process inside guest."""
        self._check_open()
        try:
            proc = await self.env.start_process(argv, cwd=cwd, env=env)
            async for chunk in proc.output(follow=True):
                if chunk.stdout is not None:
                    yield chunk.stdout.decode("utf-8", errors="replace")
                elif chunk.stderr is not None:
                    yield chunk.stderr.decode("utf-8", errors="replace")
        except (InvalidArgumentError, NotFoundError, RpcError) as e:
            raise SandboxUnavailableError(str(e), sandbox_id=self.env_id) from e
        except Exception as e:
            raise InfrastructureError(str(e), sandbox_id=self.env_id) from e

    def stream_process(
        self,
        argv: list[str],
        cwd: str = "",
        env: dict[str, str] | None = None,
    ) -> Iterator[str]:
        """Synchronously stream output chunks in real time."""
        self._check_open()
        q: queue.Queue[tuple[str | None, Exception | None]] = queue.Queue()

        async def _pump():
            try:
                async for item in self.stream_process_async(argv, cwd=cwd, env=env):
                    q.put((item, None))
                q.put((None, None))
            except Exception as e:
                q.put((None, e))

        fut = self._get_loop_runner().submit(_pump())
        try:
            while True:
                item, err = q.get()
                if err is not None:
                    raise err
                if item is None:
                    break
                yield item
        finally:
            fut.cancel()

    async def write_file_async(self, path: str, content: bytes | str) -> None:
        """Write file content directly into guest filesystem via FileSystemService."""
        self._check_open()
        raw = content.encode("utf-8") if isinstance(content, str) else content
        try:
            await self.env.write_file(path, raw)
        except InvalidArgumentError as e:
            raise SandboxProtocolError(str(e), sandbox_id=self.env_id) from e
        except (NotFoundError, RpcError) as e:
            raise SandboxUnavailableError(str(e), sandbox_id=self.env_id) from e
        except Exception as e:
            raise InfrastructureError(str(e), sandbox_id=self.env_id) from e

    def write_file(self, path: str, content: bytes | str) -> None:
        self._sync(self.write_file_async(path, content))

    async def read_file_bytes_async(self, path: str) -> bytes:
        """Read binary file content from guest filesystem via FileSystemService."""
        self._check_open()
        try:
            return await self.env.read_file_bytes(path)
        except NotFoundError:
            raise FileNotFoundError(f"File not found in sandbox: {path}")
        except InvalidArgumentError as e:
            raise SandboxProtocolError(str(e), sandbox_id=self.env_id) from e
        except RpcError as e:
            raise SandboxUnavailableError(str(e), sandbox_id=self.env_id) from e
        except Exception as e:
            raise InfrastructureError(str(e), sandbox_id=self.env_id) from e

    def read_file_bytes(self, path: str) -> bytes:
        return self._sync(self.read_file_bytes_async(path))
