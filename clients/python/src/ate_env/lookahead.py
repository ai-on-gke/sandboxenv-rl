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

"""`LookaheadWarmer`: warm pools that follow an RL trainer's batch stream.

A trainer owns its step loop. It knows the next batch or two before it
dispatches them, and with off-policy staleness it may still be claiming for a
batch it dispatched ``max_staleness`` steps ago. The warmer keeps each image's
pool sized for exactly the batches in flight::

    warmer = LookaheadWarmer(fleet, keep_finished=max_staleness)
    warmer.add(0, {img: n_prompts * G for img, n_prompts in batch0}, wait=True)
    warmer.add(1, counts_of(batch1))      # warms while step 0 runs
    ...                                   # dispatch, fleet.acquire(task) per episode
    warmer.add(2, counts_of(batch2))
    warmer.finish(0)                      # kept for keep_finished more finishes
    warmer.close()

Requested replicas per image are the sum of its counts over live steps and the
last ``keep_finished`` finished steps, capped at ``cap`` (default
``max_warmpool_replicas``). Each ``add`` / ``finish`` applies the change
since the previous request.

Substrate has no controller that refills a pool after a claim: every claim
takes a paused actor out of the pool for good. So a change of ``d`` requested
replicas is applied to the pool's *current* idle size, ``max(0, idle + d)``,
rather than as an absolute target, which would re-warm replicas the current
batch already claimed. The target never exceeds the new request.
"""

from __future__ import annotations

import asyncio
import collections
import contextvars
import logging
import threading
from collections.abc import Mapping
from concurrent.futures import ThreadPoolExecutor
from dataclasses import dataclass, field
from typing import Any

logger = logging.getLogger("ate_env.lookahead")


@dataclass
class WarmReport:
    """What one ``add`` / ``finish`` / ``close`` changed.

    Attributes:
        changes: image -> (previous request, new request), for the images
            whose request was applied.
        errors: image -> the exception that kept its change from applying.
            The image keeps its previous request, so the next call retries it.
    """

    changes: dict[str, tuple[int, int]] = field(default_factory=dict)
    errors: dict[str, BaseException] = field(default_factory=dict)

    @property
    def ok(self) -> bool:
        return not self.errors


class LookaheadWarmer:
    """Sizes a fleet's warm pools for the batches a trainer has in flight.

    Args:
        fleet: a ``SandboxFleet`` or ``AsyncSandboxFleet``.
        keep_finished: how many finished steps keep their pools (the
            trainer's ``max_staleness``: claims for them may still arrive).
        cap: per-image replica cap; None uses ``max_warmpool_replicas``.
        fail_fast: raise the first warm error after a call instead of only
            reporting it in ``WarmReport.errors``.

    Calls are serialized; ``add_async`` / ``finish_async`` / ``close_async``
    run them off the event loop.
    """

    def __init__(self, fleet: Any, *, keep_finished: int = 0, cap: int | None = None,
                 fail_fast: bool = False):
        if not isinstance(keep_finished, int) or keep_finished < 0:
            raise ValueError(f"keep_finished must be an int >= 0, got {keep_finished!r}")
        if cap is not None and (not isinstance(cap, int) or cap < 0):
            raise ValueError(f"cap must be an int >= 0 or None, got {cap!r}")
        # AsyncSandboxFleet wraps a SandboxFleet; the warmer drives the sync one.
        self._fleet = getattr(fleet, "_fleet", fleet)
        self.keep_finished = keep_finished
        self.cap = self._fleet.config.max_warmpool_replicas if cap is None else cap
        self.fail_fast = fail_fast
        self._live: dict[int, dict[str, int]] = {}
        self._finished: collections.deque[tuple[int, dict[str, int]]] = collections.deque()
        self._requested: dict[str, int] = {}
        self._lock = threading.Lock()
        self._closed = False

    @property
    def requested(self) -> dict[str, int]:
        """Replicas currently requested per image (images at 0 omitted)."""
        with self._lock:
            return dict(self._requested)

    def add(self, step: int, counts: Mapping[str, int], *, wait: bool = False) -> WarmReport:
        """Register step ``step``'s demand, ``{image: claims}``, and apply it.

        ``wait=True`` waits for new warm replicas to be suspended (the
        start-up barrier before the first step); either way, the call returns
        once the environments are created.
        """
        demand: dict[str, int] = {}
        for img, n in counts.items():
            if not isinstance(n, int) or isinstance(n, bool) or n < 0:
                raise ValueError(f"count for {img!r} must be an int >= 0, got {n!r}")
            if n:
                demand[img] = n
        with self._lock:
            self._check_open()
            if step in self._live or any(s == step for s, _ in self._finished):
                raise ValueError(f"step {step} was already added")
            self._live[step] = demand
            return self._apply_locked(wait)

    def finish(self, step: int) -> WarmReport:
        """Mark step ``step`` done; its pools stay for ``keep_finished`` more finishes."""
        with self._lock:
            self._check_open()
            if step not in self._live:
                raise ValueError(f"step {step} is not live")
            demand = self._live.pop(step)
            if self.keep_finished:
                self._finished.append((step, demand))
                while len(self._finished) > self.keep_finished:
                    self._finished.popleft()
            return self._apply_locked(wait=False)

    def close(self) -> WarmReport:
        """Drop every request and drain the pools this warmer asked for."""
        with self._lock:
            if self._closed:
                return WarmReport()
            self._live.clear()
            self._finished.clear()
            report = self._apply_locked(wait=False, raise_errors=False)
            self._closed = True
            return report

    async def add_async(self, step: int, counts: Mapping[str, int], *,
                        wait: bool = False) -> WarmReport:
        return await asyncio.to_thread(self.add, step, counts, wait=wait)

    async def finish_async(self, step: int) -> WarmReport:
        return await asyncio.to_thread(self.finish, step)

    async def close_async(self) -> WarmReport:
        return await asyncio.to_thread(self.close)

    def _check_open(self) -> None:
        if self._closed:
            raise RuntimeError("LookaheadWarmer is closed")

    def _desired_locked(self) -> dict[str, int]:
        total: collections.Counter[str] = collections.Counter()
        for demand in self._live.values():
            total.update(demand)
        for _step, demand in self._finished:
            total.update(demand)
        return {img: min(n, self.cap) for img, n in total.items() if n > 0}

    def _apply_locked(self, wait: bool, raise_errors: bool = True) -> WarmReport:
        desired = self._desired_locked()
        todo = {
            img: (self._requested.get(img, 0), desired.get(img, 0))
            for img in set(desired) | set(self._requested)
            if self._requested.get(img, 0) != desired.get(img, 0)
        }
        report = WarmReport()
        if not todo:
            return report

        def _apply_one(img: str, old: int, new: int) -> None:
            if new == 0:
                self._fleet.unwarm_image(img)
                return
            idle = self._fleet.pool_size(img)
            if idle is None:  # the driver cannot tell: assume what we asked for
                idle = old
            target = min(max(0, idle + (new - old)), new)
            self._fleet.set_pool_replicas(img, target, wait=wait)

        workers = min(self._fleet.config.warm_parallelism, len(todo))
        with ThreadPoolExecutor(max_workers=workers, thread_name_prefix="ate-env-lookahead") as ex:
            futures = {
                img: ex.submit(contextvars.copy_context().run, _apply_one, img, old, new)
                for img, (old, new) in todo.items()
            }
            for img, fut in futures.items():
                err = fut.exception()
                if err is None:
                    report.changes[img] = todo[img]
                    if todo[img][1]:
                        self._requested[img] = todo[img][1]
                    else:
                        self._requested.pop(img, None)
                else:
                    report.errors[img] = err
                    logger.warning("Lookahead warm of %s (%d -> %d) failed: %s",
                                   img, todo[img][0], todo[img][1], err)
        if report.errors and self.fail_fast and raise_errors:
            raise next(iter(report.errors.values()))
        return report
