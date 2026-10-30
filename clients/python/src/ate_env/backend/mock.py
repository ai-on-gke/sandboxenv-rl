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

from __future__ import annotations

import collections
import time
import uuid
from typing import Any

from ..errors import SandboxStartError
from ..telemetry import metrics
from ..telemetry.trace_recorder import current_slot_wait_s, is_measure_mode
from ..types import EnvironmentSpec, RawSandboxInstance
from .base import BackendDriver


class MockBackendDriver(BackendDriver):
    """In-memory mock backend driver for hermetic unit testing and PoC simulation."""

    backend_name = "mock"

    def __init__(self, fail_preflight: bool = False, fail_acquire: bool = False):
        self.fail_preflight = fail_preflight
        self.fail_acquire = fail_acquire
        self.templates: dict[str, EnvironmentSpec] = {}
        self.warm_pools: dict[str | tuple[str, str | None], int] = collections.defaultdict(int)
        self.instances: dict[str, RawSandboxInstance] = {}
        self.reaped_runs: set[str] = set()

    def preflight(self) -> None:
        if self.fail_preflight:
            raise RuntimeError("Mock preflight failed: cluster unreachable")

    def ensure_template(self, env: EnvironmentSpec) -> str:
        tid = env.template_key()
        self.templates[tid] = env
        return tid

    def warm_pool(
        self,
        template_id: str,
        replicas: int,
        wait: bool = True,
        run_id: str | None = None,
        rootfs_image: str | None = None,
    ) -> None:
        self._set_pool(template_id, rootfs_image, replicas)

    def resize_pool(
        self,
        template_id: str,
        replicas: int,
        wait: bool = True,
        rootfs_image: str | None = None,
    ) -> None:
        self._set_pool(template_id, rootfs_image, replicas)

    def _set_pool(self, template_id: str, rootfs_image: str | None, replicas: int) -> None:
        """Set the pool's size and move the observer's warm count by the
        delta, up or down, so the gauge and the run report follow a shrink."""
        key = template_id if rootfs_image is None else (template_id, rootfs_image)
        delta = replicas - self.warm_pools.get(key, 0)
        if delta > 0:
            self._observe("warm_add", delta)
        elif delta < 0:
            self._observe("warm_remove", -delta)
        self.warm_pools[template_id] = replicas
        if rootfs_image is not None:
            self.warm_pools[(template_id, rootfs_image)] = replicas

    def unwarm_pool(self, template_id: str, rootfs_image: str | None = None) -> None:
        key = template_id if rootfs_image is None else (template_id, rootfs_image)
        self._observe("warm_remove", self.warm_pools.get(key, 0))
        self.warm_pools[template_id] = 0
        if rootfs_image is not None:
            self.warm_pools.pop((template_id, rootfs_image), None)

    def pool_size(self, template_id: str, rootfs_image: str | None = None) -> int | None:
        key = template_id if rootfs_image is None else (template_id, rootfs_image)
        return self.warm_pools.get(key, 0)

    def acquire(
        self,
        template_id: str,
        run_id: str,
        timeout_s: float = 180.0,
        rootfs_image: str | None = None,
    ) -> RawSandboxInstance:
        start = time.monotonic()
        slot_wait_s = current_slot_wait_s.get() or 0.0
        if self.fail_acquire:
            self._observe("claim", "error")
            metrics.observe_subphase("slot_wait", slot_wait_s, status="error", source="mock")
            metrics.observe_subphase(
                "wire_claim", time.monotonic() - start, status="error", source="mock"
            )
            if is_measure_mode():
                metrics.observe("acquire", time.monotonic() - start, status="error")
            raise SandboxStartError("Mock acquire failed: capacity exhausted")

        warm_count = self.warm_pools.get(
            (template_id, rootfs_image) if rootfs_image is not None else template_id,
            self.warm_pools.get(template_id, 0),
        )
        claim_source = "warm_pool" if warm_count > 0 else "mock"
        inst_id = f"mock-sb-{uuid.uuid4().hex[:8]}"
        metadata: dict[str, Any] = {
            "created_at": time.time(),
            "template": template_id,
            "claim_source": claim_source,
        }
        if rootfs_image is not None:
            metadata["rootfs_image"] = rootfs_image
        inst = RawSandboxInstance(
            instance_id=inst_id,
            endpoint=f"http://127.0.0.1:8000/{inst_id}",
            template_id=template_id,
            run_id=run_id,
            metadata=metadata,
            rootfs_image=rootfs_image,
        )
        self.instances[inst_id] = inst
        self._observe("claim", "ok")
        dur = time.monotonic() - start
        metrics.observe_subphase("slot_wait", slot_wait_s, status="ok", source=claim_source)
        metrics.observe_subphase("wire_claim", dur, status="ok", source=claim_source)
        if is_measure_mode():
            metrics.observe("acquire", dur, status="ok")
        return inst

    def release(self, instance_id: str, recycle: bool = False) -> None:
        start = time.monotonic()
        if instance_id in self.instances:
            if not recycle:
                del self.instances[instance_id]
        if is_measure_mode():
            metrics.observe("release", time.monotonic() - start, status="ok")

    def reap(self, run_id: str) -> int:
        self.reaped_runs.add(run_id)
        to_del = [iid for iid, inst in self.instances.items() if inst.run_id == run_id]
        for iid in to_del:
            del self.instances[iid]
        return len(to_del)

    def build_runtime(
        self, instance: RawSandboxInstance, data_plane: str = "mock"
    ) -> tuple[Any, Any | None]:
        from ..runtime.mock import MockRuntimeHook

        return MockRuntimeHook(), None
