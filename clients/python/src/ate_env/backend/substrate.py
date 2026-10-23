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
# pylint: disable=too-many-locals,too-many-branches,too-many-statements,no-else-break
# pylint: disable=broad-exception-caught,unused-variable,import-outside-toplevel,duplicate-code

from __future__ import annotations

import asyncio
import logging
import os
import threading
import time
import uuid
from collections.abc import Mapping
from typing import Any

from ..client import DEFAULT_ATESPACE, Client
from ..config import TracerConfig
from ..errors import (
    FailedPreconditionError,
    InfrastructureError,
    NotFoundError,
    PreflightError,
    SandboxStartError,
)
from ..loop_runner import LoopRunner
from ..telemetry import (
    _CURRENT_SLOT_WAIT_MS,
    _CURRENT_TASK_META,
    _IN_TRACKED_LIFECYCLE,
    BenchTelemetry,
    instrument_client,
)
from ..telemetry.tracing import (
    CLIENT_SPAN_PREFIX,
    set_attributes,
    start_span,
    tracer_from_config,
)
from ..types import DataPlaneEndpoint, EnvironmentSpec, RawSandboxInstance
from .base import BackendDriver

logger = logging.getLogger("ate_env.backend.substrate")


class _WarmPoolDict(dict):
    """Warm paused pool dictionary keyed by (template, image).

    Allows string keys for backwards compatibility, treating 'template' as (template, None).
    """

    def _normalize(self, key: Any) -> Any:
        if isinstance(key, str):
            return (key, None)
        return key

    def __getitem__(self, key: Any) -> Any:
        return super().__getitem__(self._normalize(key))

    def __setitem__(self, key: Any, value: Any) -> None:
        super().__setitem__(self._normalize(key), value)

    def __contains__(self, key: Any) -> bool:
        return super().__contains__(self._normalize(key))

    def get(self, key: Any, default: Any = None) -> Any:
        return super().get(self._normalize(key), default)

    def pop(self, key: Any, *args: Any) -> Any:
        return super().pop(self._normalize(key), *args)

    def setdefault(self, key: Any, default: Any = None) -> Any:
        return super().setdefault(self._normalize(key), default)


class SubstrateBackendDriver(BackendDriver):
    """Agent Substrate Backend Driver.

    Manages environment lifecycle directly against the ate-env-api gRPC service
    via ate_env.Client: creating environments, suspending for pre-warmed pools,
    and deleting on release.

    tracer_config enables ``ate_env_client.ensure_template`` / ``.warm_pool`` /
    ``.acquire`` / ``.release`` / ``.reap`` spans and is handed to the Client
    the driver builds (needs the ``tracing`` extra).
    """

    def __init__(
        self,
        api_endpoint: str | None = None,
        atespace: str = DEFAULT_ATESPACE,
        worker_family: str = "c2",
        auth_token: str | None = None,
        *,
        client: Client | None = None,
        image_templates: Mapping[str, str] | None = None,
        default_template: str | None = None,
        labels: Mapping[str, str] | None = None,
        acquire_timeout_s: float = 180.0,
        ready_timeout_s: float = 180.0,
        loop_runner: LoopRunner | None = None,
        tracer_config: TracerConfig | None = None,
        telemetry: BenchTelemetry | None = None,
    ):
        target = (
            api_endpoint
            or os.environ.get("ATE_ENV_API_TARGET")
            or os.environ.get("SUBSTRATE_API_ENDPOINT")
            or "127.0.0.1:7777"
        )
        self.api_endpoint = target
        self.atespace = atespace or DEFAULT_ATESPACE
        self.worker_family = worker_family
        self.auth_token = auth_token
        self.image_templates = dict(image_templates or {})
        self.default_template = default_template
        self.labels = dict(labels or {})
        self.acquire_timeout_s = acquire_timeout_s
        self.ready_timeout_s = ready_timeout_s
        self.tracer_config = tracer_config or TracerConfig()
        self.tracer = tracer_from_config(self.tracer_config)
        self.telemetry: BenchTelemetry | None = telemetry

        self._loop_runner = loop_runner
        self._owns_loop_runner = loop_runner is None

        if client is not None:
            self._client = client
            self._owns_client = False
        else:
            self._client = None
            self._owns_client = True

        self._lock = threading.Lock()
        self._owned_actors: dict[str, RawSandboxInstance] = {}
        self._warm_paused_pool: _WarmPoolDict = _WarmPoolDict()
        self._background_tasks: set[asyncio.Task[Any]] = set()

    def _get_loop_runner(self) -> LoopRunner:
        with self._lock:
            if self._loop_runner is None:
                self._loop_runner = LoopRunner(name="ate-substrate-driver-sync")
                self._owns_loop_runner = True
            return self._loop_runner

    @property
    def client(self) -> Client:
        if self._client is None:
            self._client = Client(
                self.api_endpoint, auth_token=self.auth_token, tracer_config=self.tracer_config
            )
        if self.telemetry is not None and getattr(self.telemetry, "mode", "off") != "off":
            instrument_client(self._client, self.telemetry)
        return self._client

    def _sync(self, coro: Any) -> Any:
        return self._get_loop_runner().run(coro)

    async def close_async(self) -> None:
        if self._owns_client and self._client:
            await self._client.close()
            self._client = None
        if self._owns_loop_runner and self._loop_runner:
            self._loop_runner.stop()
            self._loop_runner = None

    def close(self) -> None:
        if self._loop_runner:
            if self._owns_client and self._client:
                self._loop_runner.run(self._client.close())
                self._client = None
            if self._owns_loop_runner:
                self._loop_runner.stop()
                self._loop_runner = None
        elif self._owns_client and self._client:
            self._sync(self.close_async())

    async def preflight_async(self) -> None:
        """Validate ate-env-api connectivity."""
        logger.info(
            "Running Substrate preflight checks (Atespace: %s, Endpoint: %s)...",
            self.atespace,
            self.api_endpoint,
        )
        token = _IN_TRACKED_LIFECYCLE.set(True)
        try:
            await self.client.get(f"preflight-probe-{uuid.uuid4().hex[:6]}", atespace=self.atespace)
        except NotFoundError:
            # NotFoundError confirms the gRPC server is reachable and active.
            logger.debug("Preflight probe successfully reached ate-env-api")
        except Exception as e:
            raise PreflightError(
                f"Substrate preflight check failed to reach ate-env-api at {self.api_endpoint}: {e}"
            ) from e
        finally:
            _IN_TRACKED_LIFECYCLE.reset(token)

    def preflight(self) -> None:
        self._sync(self.preflight_async())

    def ensure_template(self, env: EnvironmentSpec | str) -> str:
        """Resolve ActorTemplate name from template ID or task image.

        Note: ResourceLimits, runtime_bundle, snapshot_storage_uri, and placement
        fields in EnvironmentSpec are reserved for future scheduling extensions.
        Current Substrate environments are provisioned from pre-registered ActorTemplates.
        """
        with start_span(self.tracer, f"{CLIENT_SPAN_PREFIX}.ensure_template") as span:
            template = self._resolve_template(env)
            set_attributes(span, {"ate.template.name": template})
            return template

    def _resolve_template(self, env: EnvironmentSpec | str) -> str:
        if isinstance(env, str):
            image = env
            if image in self.image_templates:
                return self.image_templates[image]
            return self.default_template or "default-template"

        image = env.image or ""
        mapped = self.image_templates.get(image)
        if mapped:
            return mapped
        if self.default_template:
            return self.default_template
        if isinstance(env, EnvironmentSpec):
            return env.template_key()
        return "default-template"

    def _make_instance(
        self, actor_id: str, template_id: str, run_id: str, rootfs_image: str | None = None
    ) -> RawSandboxInstance:
        planes = {
            "ate_env": DataPlaneEndpoint(
                protocol="grpc",
                address=self.api_endpoint,
                headers={"x-env-id": actor_id, "x-env-atespace": self.atespace},
            ),
        }
        metadata = {
            "created_at": time.time(),
            "template": template_id,
            "atespace": self.atespace,
        }
        if rootfs_image:
            metadata["rootfs_image"] = rootfs_image
        if self.labels:
            metadata["labels"] = dict(self.labels)
        return RawSandboxInstance(
            instance_id=actor_id,
            endpoint=self.api_endpoint,
            template_id=template_id,
            run_id=run_id,
            metadata=metadata,
            data_planes=planes,
            rootfs_image=rootfs_image,
        )

    async def acquire_async(
        self,
        template_id: str,
        run_id: str,
        timeout_s: float = 180.0,
        rootfs_image: str | None = None,
        *,
        instance: str = "",
    ) -> RawSandboxInstance:
        """Acquire a live sandbox instance within timeout_s."""
        to_s = timeout_s or self.acquire_timeout_s
        slot_wait_ms = float(_CURRENT_SLOT_WAIT_MS.get() or 0.0)
        task_id_ctx, task_img_ctx = _CURRENT_TASK_META.get()
        effective_instance = instance or task_id_ctx
        effective_image = rootfs_image or task_img_ctx or template_id
        t_wire0 = time.perf_counter()
        state: dict[str, Any] = {
            "source": "cold_create",
            "retries": 0,
            "actor_id": "",
        }
        with start_span(
            self.tracer,
            f"{CLIENT_SPAN_PREFIX}.acquire",
            {"ate.template.name": template_id, "ate_env.run_id": run_id},
        ) as span:
            token = _IN_TRACKED_LIFECYCLE.set(True)
            try:
                inst = await asyncio.wait_for(
                    self._acquire_internal(
                        template_id, run_id, rootfs_image=rootfs_image, _telemetry_state=state
                    ),
                    timeout=to_s,
                )
            except asyncio.CancelledError:
                wire_dur_ms = (time.perf_counter() - t_wire0) * 1000.0
                if self.telemetry is not None and getattr(self.telemetry, "mode", "off") != "off":
                    src = str(state.get("source") or "cold_create")
                    actor_id = str(state.get("actor_id") or "")
                    self.telemetry.record_op(
                        "acquire",
                        wire_dur_ms,
                        ok=False,
                        env_id=actor_id,
                        image=effective_image,
                        instance=effective_instance,
                        source=src,
                        cache_state="hit" if src == "warm_pool" else "miss",
                        slot_wait_ms=slot_wait_ms,
                        retried=int(state.get("retries") or 0),
                        timed_out=False,
                        err="CancelledError",
                        trace_op="wake" if src == "warm_pool" else "create",
                        emit_trace=bool(actor_id),
                    )
                raise
            except asyncio.TimeoutError as e:
                wire_dur_ms = (time.perf_counter() - t_wire0) * 1000.0
                if self.telemetry is not None and getattr(self.telemetry, "mode", "off") != "off":
                    src = str(state.get("source") or "cold_create")
                    actor_id = str(state.get("actor_id") or "")
                    self.telemetry.record_op(
                        "acquire",
                        wire_dur_ms,
                        ok=False,
                        env_id=actor_id,
                        image=effective_image,
                        instance=effective_instance,
                        source=src,
                        cache_state="hit" if src == "warm_pool" else "miss",
                        slot_wait_ms=slot_wait_ms,
                        retried=int(state.get("retries") or 0),
                        timed_out=True,
                        err=f"SandboxStartError: Timed out acquiring sandbox for template {template_id}",
                        trace_op="wake" if src == "warm_pool" else "create",
                        emit_trace=bool(actor_id),
                    )
                raise SandboxStartError(
                    f"Timed out acquiring sandbox for template {template_id} after {to_s:g}s"
                ) from e
            except Exception as e:
                wire_dur_ms = (time.perf_counter() - t_wire0) * 1000.0
                if self.telemetry is not None and getattr(self.telemetry, "mode", "off") != "off":
                    src = str(state.get("source") or "cold_create")
                    actor_id = str(state.get("actor_id") or "")
                    timed_out = "timed out" in str(e).lower()
                    self.telemetry.record_op(
                        "acquire",
                        wire_dur_ms,
                        ok=False,
                        env_id=actor_id,
                        image=effective_image,
                        instance=effective_instance,
                        source=src,
                        cache_state="hit" if src == "warm_pool" else "miss",
                        slot_wait_ms=slot_wait_ms,
                        retried=int(state.get("retries") or 0),
                        timed_out=timed_out,
                        err=f"{type(e).__name__}: {e}",
                        trace_op="wake" if src == "warm_pool" else "create",
                        emit_trace=bool(actor_id),
                    )
                raise
            finally:
                _IN_TRACKED_LIFECYCLE.reset(token)

            set_attributes(span, {"ate.actor.name": inst.instance_id})

        wire_dur_ms = (time.perf_counter() - t_wire0) * 1000.0
        src = str(state.get("source") or "cold_create")
        cache_state = "hit" if src == "warm_pool" else "miss"
        inst.metadata["source"] = src
        inst.metadata["cache_state"] = cache_state
        inst.metadata["acquired_at_monotonic"] = time.monotonic()

        if self.telemetry is not None and getattr(self.telemetry, "mode", "off") != "off":
            self.telemetry.record_op(
                "acquire",
                wire_dur_ms,
                ok=True,
                env_id=inst.instance_id,
                image=effective_image,
                instance=effective_instance,
                source=src,
                cache_state=cache_state,
                slot_wait_ms=slot_wait_ms,
                retried=int(state.get("retries") or 0),
                trace_op="wake" if src == "warm_pool" else "create",
            )
        return inst

    async def _acquire_internal(
        self,
        template_id: str,
        run_id: str,
        rootfs_image: str | None = None,
        _telemetry_state: dict[str, Any] | None = None,
    ) -> RawSandboxInstance:
        """Acquire or claim an environment.

        Note on readiness: Substrate resumes suspended environments implicitly on first
        guest traffic, so ready_timeout_s does not measure readiness here.
        """
        pool_key = (template_id, rootfs_image)
        while True:
            actor_id: str | None = None
            with self._lock:
                warm_list = self._warm_paused_pool.get(pool_key)
                if warm_list:
                    actor_id = warm_list.pop()

            if actor_id:
                if _telemetry_state is not None:
                    _telemetry_state["source"] = "warm_pool"
                    _telemetry_state["actor_id"] = actor_id
                logger.info(
                    "Claimed warm paused actor %s for template %s (rootfs_image=%s)",
                    actor_id,
                    template_id,
                    rootfs_image,
                )
                # Verify warm actor still exists on the server
                try:
                    await asyncio.wait_for(
                        self.client.get(actor_id, atespace=self.atespace),
                        timeout=min(self.ready_timeout_s, 15.0),
                    )
                except NotFoundError:
                    logger.warning("Warm actor %s not found on server, dropping from pool", actor_id)
                    with self._lock:
                        self._owned_actors.pop(actor_id, None)
                    continue
                except Exception as e:
                    # Put back on unexpected error and raise
                    with self._lock:
                        self._warm_paused_pool.setdefault(pool_key, []).append(actor_id)
                    raise SandboxStartError(f"Error checking warm actor {actor_id}: {e}") from e
                break
            else:
                actor_id = f"sb-{uuid.uuid4().hex[:16]}"
                if _telemetry_state is not None:
                    _telemetry_state["source"] = "cold_create"
                    _telemetry_state["actor_id"] = actor_id
                logger.info(
                    "Creating environment %s (template=%s, atespace=%s, rootfs_image=%s)...",
                    actor_id,
                    template_id,
                    self.atespace,
                    rootfs_image,
                )
                last_err: Exception | None = None
                for attempt in range(4):
                    if _telemetry_state is not None:
                        _telemetry_state["retries"] = attempt
                    try:
                        await self.client.create(
                            actor_id,
                            template_name=template_id,
                            atespace=self.atespace,
                            rootfs_image=rootfs_image,
                        )
                        last_err = None
                        break
                    except (NotFoundError, FailedPreconditionError) as e:
                        raise SandboxStartError(
                            f"ActorTemplate '{template_id}' invalid or does not exist on Substrate server: {e}"
                        ) from e
                    except Exception as e:
                        last_err = e
                        if "already exists" in str(e).lower():
                            last_err = None
                            break
                        if attempt < 3:
                            await asyncio.sleep(0.05 * (attempt + 1))
                if last_err is not None:
                    raise SandboxStartError(f"Failed to create environment {actor_id}: {last_err}") from last_err
                break

        instance = self._make_instance(actor_id, template_id, run_id, rootfs_image=rootfs_image)
        with self._lock:
            self._owned_actors[actor_id] = instance
        return instance

    def acquire(
        self,
        template_id: str,
        run_id: str,
        timeout_s: float = 180.0,
        rootfs_image: str | None = None,
        *,
        instance: str = "",
    ) -> RawSandboxInstance:
        return self._sync(
            self.acquire_async(
                template_id,
                run_id,
                timeout_s=timeout_s,
                rootfs_image=rootfs_image,
                instance=instance,
            )
        )

    async def warm_pool_async(
        self,
        template_id: str,
        replicas: int,
        wait: bool = True,
        run_id: str | None = None,
        rootfs_image: str | None = None,
    ) -> None:
        """Pre-warm a pool of actors by creating and suspending them."""
        with start_span(self.tracer, f"{CLIENT_SPAN_PREFIX}.warm_pool", {
            "ate.template.name": template_id,
            "ate_env.replicas": replicas,
            "ate_env.run_id": run_id,
        }):
            await self._warm_pool_internal(
                template_id, replicas, wait=wait, run_id=run_id, rootfs_image=rootfs_image
            )

    async def _warm_pool_internal(
        self,
        template_id: str,
        replicas: int,
        wait: bool,
        run_id: str | None,
        rootfs_image: str | None,
    ) -> None:
        logger.info(
            "Sizing warm pool for template %s (rootfs_image=%s) to %d paused actors...",
            template_id,
            rootfs_image,
            replicas,
        )
        pool_key = (template_id, rootfs_image)
        with self._lock:
            current = list(self._warm_paused_pool.get(pool_key, []))
        needed = replicas - len(current)
        if needed <= 0:
            return

        t_prewarm0 = time.perf_counter()
        sem = asyncio.Semaphore(16)
        effective_run_id = run_id or "warm-pool"

        async def _warm_one() -> str | None:
            actor_id = f"warm-{uuid.uuid4().hex[:10]}"
            effective_image = rootfs_image or template_id
            async with sem:
                token = _IN_TRACKED_LIFECYCLE.set(True)
                try:
                    t_c0 = time.perf_counter()
                    await self.client.create(
                        actor_id,
                        template_name=template_id,
                        atespace=self.atespace,
                        rootfs_image=rootfs_image,
                    )
                    if self.telemetry is not None and getattr(self.telemetry, "mode", "off") == "trace":
                        self.telemetry.record_op(
                            "create",
                            (time.perf_counter() - t_c0) * 1000.0,
                            ok=True,
                            env_id=actor_id,
                            image=effective_image,
                            source="warm_pool",
                            trace_only=True,
                        )
                    if wait:
                        t_s0 = time.perf_counter()
                        await self.client.suspend(actor_id, atespace=self.atespace)
                        if self.telemetry is not None and getattr(self.telemetry, "mode", "off") == "trace":
                            self.telemetry.record_op(
                                "suspend",
                                (time.perf_counter() - t_s0) * 1000.0,
                                ok=True,
                                env_id=actor_id,
                                image=effective_image,
                                source="warm_pool",
                                trace_only=True,
                            )
                    else:
                        async def _pause_bg(aid: str, img: str) -> None:
                            bg_token = _IN_TRACKED_LIFECYCLE.set(True)
                            t_s0 = time.perf_counter()
                            try:
                                await self.client.suspend(aid, atespace=self.atespace)
                                if (
                                    self.telemetry is not None
                                    and getattr(self.telemetry, "mode", "off") == "trace"
                                ):
                                    self.telemetry.record_op(
                                        "suspend",
                                        (time.perf_counter() - t_s0) * 1000.0,
                                        ok=True,
                                        env_id=aid,
                                        image=img,
                                        source="warm_pool",
                                        trace_only=True,
                                    )
                            except Exception as exc:
                                if (
                                    self.telemetry is not None
                                    and getattr(self.telemetry, "mode", "off") == "trace"
                                ):
                                    self.telemetry.record_op(
                                        "suspend",
                                        (time.perf_counter() - t_s0) * 1000.0,
                                        ok=False,
                                        env_id=aid,
                                        image=img,
                                        source="warm_pool",
                                        err=f"{type(exc).__name__}: {exc}",
                                        trace_only=True,
                                    )
                                raise
                            finally:
                                _IN_TRACKED_LIFECYCLE.reset(bg_token)

                        task = asyncio.create_task(_pause_bg(actor_id, effective_image))
                        self._background_tasks.add(task)
                        def _done(t: asyncio.Task[Any]) -> None:
                            self._background_tasks.discard(t)
                            if not t.cancelled() and t.exception():
                                logger.warning("Background suspend for warm actor %s failed: %s", actor_id, t.exception())
                        task.add_done_callback(_done)
                    return actor_id
                except (NotFoundError, FailedPreconditionError) as e:
                    raise SandboxStartError(
                        f"ActorTemplate '{template_id}' invalid or does not exist on Substrate server: {e}"
                    ) from e
                except Exception as e:
                    logger.warning("Failed to provision warm actor %s: %s", actor_id, e)
                    return None
                finally:
                    _IN_TRACKED_LIFECYCLE.reset(token)

        tasks = [_warm_one() for _ in range(needed)]
        results = await asyncio.gather(*tasks, return_exceptions=True)

        prewarm_err: Exception | None = None
        for res in results:
            if isinstance(res, Exception):
                if isinstance(res, SandboxStartError):
                    prewarm_err = res
                    break
                logger.warning("Error provisioning warm actor: %s", res)
            elif isinstance(res, str):
                aid = res
                with self._lock:
                    self._warm_paused_pool.setdefault(pool_key, []).append(aid)
                    self._owned_actors[aid] = self._make_instance(
                        aid, template_id, run_id=effective_run_id, rootfs_image=rootfs_image
                    )

        dur_ms = (time.perf_counter() - t_prewarm0) * 1000.0
        if self.telemetry is not None and getattr(self.telemetry, "mode", "off") != "off":
            self.telemetry.record_op(
                "prewarm",
                dur_ms,
                ok=prewarm_err is None,
                env_id=template_id,
                source="warm_pool",
                cache_state="miss",
                err=f"{type(prewarm_err).__name__}: {prewarm_err}" if prewarm_err else "",
            )
        if prewarm_err is not None:
            raise prewarm_err

    def warm_pool(
        self,
        template_id: str,
        replicas: int,
        wait: bool = True,
        run_id: str | None = None,
        rootfs_image: str | None = None,
    ) -> None:
        self._sync(
            self.warm_pool_async(
                template_id,
                replicas,
                wait=wait,
                run_id=run_id,
                rootfs_image=rootfs_image,
            )
        )

    async def unwarm_pool_async(self, template_id: str, rootfs_image: str | None = None) -> None:
        """Drain and delete paused actors in the warm pool."""
        logger.info("Unwarming pool for template %s (rootfs_image=%s)...", template_id, rootfs_image)
        t_retire0 = time.perf_counter()
        with self._lock:
            if rootfs_image is not None:
                to_delete = self._warm_paused_pool.pop((template_id, rootfs_image), [])
            else:
                keys_to_pop = [
                    k for k in list(self._warm_paused_pool.keys())
                    if (isinstance(k, tuple) and k[0] == template_id)
                ]
                to_delete = []
                for k in keys_to_pop:
                    to_delete.extend(self._warm_paused_pool.pop(k, []))

        sem = asyncio.Semaphore(16)

        async def _delete_one(actor_id: str) -> None:
            async with sem:
                with self._lock:
                    inst = self._owned_actors.pop(actor_id, None)
                effective_image = (
                    (inst.rootfs_image or inst.template_id)
                    if inst is not None
                    else (rootfs_image or template_id)
                )
                token = _IN_TRACKED_LIFECYCLE.set(True)
                t_d0 = time.perf_counter()
                try:
                    await self.client.delete(actor_id, atespace=self.atespace)
                    if self.telemetry is not None and getattr(self.telemetry, "mode", "off") == "trace":
                        self.telemetry.record_op(
                            "delete",
                            (time.perf_counter() - t_d0) * 1000.0,
                            ok=True,
                            env_id=actor_id,
                            image=effective_image,
                            source="warm_pool",
                            trace_only=True,
                        )
                except Exception as e:
                    if self.telemetry is not None and getattr(self.telemetry, "mode", "off") == "trace":
                        self.telemetry.record_op(
                            "delete",
                            (time.perf_counter() - t_d0) * 1000.0,
                            ok=False,
                            env_id=actor_id,
                            image=effective_image,
                            source="warm_pool",
                            err=f"{type(e).__name__}: {e}",
                            trace_only=True,
                        )
                    logger.warning("Failed deleting warm actor %s: %s", actor_id, e)
                finally:
                    _IN_TRACKED_LIFECYCLE.reset(token)

        await asyncio.gather(*[_delete_one(aid) for aid in to_delete], return_exceptions=True)
        dur_ms = (time.perf_counter() - t_retire0) * 1000.0
        if (
            to_delete
            and self.telemetry is not None
            and getattr(self.telemetry, "mode", "off") != "off"
        ):
            self.telemetry.record_op(
                "retire",
                dur_ms,
                ok=True,
                env_id=template_id,
                source="warm_pool",
            )

    def unwarm_pool(self, template_id: str, rootfs_image: str | None = None) -> None:
        self._sync(self.unwarm_pool_async(template_id, rootfs_image=rootfs_image))

    async def release_async(self, instance_id: str, recycle: bool = False) -> None:
        """Release actor: deletes environment, or suspends if recycling.

        Note: Recycling suspends the actor and returns it to the warm pool
        without resetting guest filesystem or processes.
        """
        with start_span(self.tracer, f"{CLIENT_SPAN_PREFIX}.release", {"ate.actor.name": instance_id}):
            await self._release_internal(instance_id, recycle)

    async def _release_internal(self, instance_id: str, recycle: bool) -> None:
        with self._lock:
            instance = self._owned_actors.pop(instance_id, None)

        inst_source = (
            str(instance.metadata.get("source") or "direct") if instance else "direct"
        )
        inst_cache = (
            str(instance.metadata.get("cache_state") or "unknown") if instance else "unknown"
        )

        if recycle and instance:
            t_susp0 = time.perf_counter()
            token = _IN_TRACKED_LIFECYCLE.set(True)
            try:
                await self.client.suspend(instance_id, atespace=self.atespace)
                dur_ms = (time.perf_counter() - t_susp0) * 1000.0
                if self.telemetry is not None and getattr(self.telemetry, "mode", "off") != "off":
                    self.telemetry.record_op(
                        "suspend",
                        dur_ms,
                        ok=True,
                        env_id=instance_id,
                        source=inst_source,
                        cache_state=inst_cache,
                    )
                pool_key = (instance.template_id, instance.rootfs_image)
                with self._lock:
                    self._warm_paused_pool.setdefault(pool_key, []).append(instance_id)
                    self._owned_actors[instance_id] = instance
                logger.info("Recycled actor %s to paused warm pool", instance_id)
                return
            except Exception as e:
                dur_ms = (time.perf_counter() - t_susp0) * 1000.0
                if self.telemetry is not None and getattr(self.telemetry, "mode", "off") != "off":
                    self.telemetry.record_op(
                        "suspend",
                        dur_ms,
                        ok=False,
                        env_id=instance_id,
                        source=inst_source,
                        cache_state=inst_cache,
                        err=f"{type(e).__name__}: {e}",
                    )
                logger.warning("Failed to suspend actor %s for recycle, deleting: %s", instance_id, e)
            finally:
                _IN_TRACKED_LIFECYCLE.reset(token)

        t_del0 = time.perf_counter()
        token = _IN_TRACKED_LIFECYCLE.set(True)
        try:
            await self.client.delete(instance_id, atespace=self.atespace)
            dur_ms = (time.perf_counter() - t_del0) * 1000.0
            if self.telemetry is not None and getattr(self.telemetry, "mode", "off") != "off":
                self.telemetry.record_op(
                    "delete",
                    dur_ms,
                    ok=True,
                    env_id=instance_id,
                    source=inst_source,
                    cache_state=inst_cache,
                )
            logger.info("Deleted environment %s", instance_id)
        except Exception as e:
            dur_ms = (time.perf_counter() - t_del0) * 1000.0
            if self.telemetry is not None and getattr(self.telemetry, "mode", "off") != "off":
                self.telemetry.record_op(
                    "delete",
                    dur_ms,
                    ok=False,
                    env_id=instance_id,
                    source=inst_source,
                    cache_state=inst_cache,
                    err=f"{type(e).__name__}: {e}",
                )
            logger.warning("Failed to delete environment %s: %s", instance_id, e)
        finally:
            _IN_TRACKED_LIFECYCLE.reset(token)

    def release(self, instance_id: str, recycle: bool = False) -> None:
        self._sync(self.release_async(instance_id, recycle=recycle))

    async def reap_async(self, run_id: str) -> int:
        """Force delete all environments tagged with this run_id and all warm pool actors."""
        with start_span(self.tracer, f"{CLIENT_SPAN_PREFIX}.reap", {"ate_env.run_id": run_id}):
            return await self._reap_internal(run_id)

    async def _reap_internal(self, run_id: str) -> int:
        with self._lock:
            warm_actors: list[str] = []
            for tid, actors in list(self._warm_paused_pool.items()):
                warm_actors.extend(actors)
            self._warm_paused_pool.clear()

            to_reap = [
                aid for aid, inst in self._owned_actors.items()
                if inst.run_id == run_id or aid in warm_actors or inst.run_id == "warm-pool"
            ]
            to_reap = list(dict.fromkeys(to_reap + warm_actors))

        sem = asyncio.Semaphore(16)
        failed_actors: list[str] = []
        reaped_count = 0
        reap_lock = asyncio.Lock()

        async def _reap_one(aid: str) -> None:
            nonlocal reaped_count
            async with sem:
                with self._lock:
                    inst = self._owned_actors.pop(aid, None)
                effective_image = (
                    (inst.rootfs_image or inst.template_id) if inst is not None else ""
                )
                token = _IN_TRACKED_LIFECYCLE.set(True)
                t_d0 = time.perf_counter()
                try:
                    await self.client.delete(aid, atespace=self.atespace)
                    if self.telemetry is not None and getattr(self.telemetry, "mode", "off") == "trace":
                        self.telemetry.record_op(
                            "delete",
                            (time.perf_counter() - t_d0) * 1000.0,
                            ok=True,
                            env_id=aid,
                            image=effective_image,
                            source="warm_pool" if aid in warm_actors else "direct",
                            trace_only=True,
                        )
                    async with reap_lock:
                        reaped_count += 1
                except Exception as e:
                    if self.telemetry is not None and getattr(self.telemetry, "mode", "off") == "trace":
                        self.telemetry.record_op(
                            "delete",
                            (time.perf_counter() - t_d0) * 1000.0,
                            ok=False,
                            env_id=aid,
                            image=effective_image,
                            source="warm_pool" if aid in warm_actors else "direct",
                            err=f"{type(e).__name__}: {e}",
                            trace_only=True,
                        )
                    logger.warning("Failed deleting actor %s during reap: %s", aid, e)
                    async with reap_lock:
                        failed_actors.append(aid)
                finally:
                    _IN_TRACKED_LIFECYCLE.reset(token)

        t_reap0 = time.perf_counter()
        await asyncio.gather(*[_reap_one(aid) for aid in to_reap], return_exceptions=True)
        reap_dur_ms = (time.perf_counter() - t_reap0) * 1000.0
        if (
            to_reap
            and self.telemetry is not None
            and getattr(self.telemetry, "mode", "off") != "off"
        ):
            self.telemetry.record_op(
                "reap",
                reap_dur_ms,
                ok=not failed_actors,
                env_id=run_id,
                source="warm_pool" if warm_actors else "direct",
                err=(
                    f"InfrastructureError: Failed to delete {len(failed_actors)} environments"
                    if failed_actors
                    else ""
                ),
            )
            if warm_actors:
                self.telemetry.record_op(
                    "retire",
                    reap_dur_ms,
                    ok=not failed_actors,
                    env_id=run_id,
                    source="warm_pool",
                )

        if failed_actors:
            raise InfrastructureError(
                f"Failed to delete {len(failed_actors)} environments during reap: {failed_actors}"
            )

        logger.info("Reaped %d Substrate environments for run %s", reaped_count, run_id)
        return reaped_count

    def reap(self, run_id: str) -> int:
        return self._sync(self.reap_async(run_id))

    def build_runtime(
        self, instance: RawSandboxInstance, data_plane: str = "ate_env"
    ) -> tuple[Any, Any | None]:
        from ..runtime.substrate_env_client import SubstrateEnvClientRuntime

        ep = instance.data_planes.get(data_plane) or instance.data_planes.get("ate_env")
        source = str(instance.metadata.get("source") or "direct")
        cache_state = str(instance.metadata.get("cache_state") or "unknown")
        acquired_at = instance.metadata.get("acquired_at_monotonic")
        return SubstrateEnvClientRuntime(
            env_id=instance.instance_id,
            endpoint=ep.address if ep else instance.endpoint,
            atespace=self.atespace,
            client=self.client,
            loop_runner=self._get_loop_runner(),
            tracer_config=self.tracer_config,
            telemetry=self.telemetry,
            source=source,
            cache_state=cache_state,
            acquired_at_monotonic=float(acquired_at) if acquired_at is not None else None,
        ), ep
