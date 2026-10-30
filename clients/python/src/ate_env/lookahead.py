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

Each step's demand is ``{image: claims}``. An image's target is the sum of the
*remaining* demand of live steps and of the last ``keep_finished`` finished
steps, capped at ``cap`` (default ``max_warmpool_replicas``), and is applied
to the pool as an absolute idle count.

Substrate has no controller that refills a pool after a claim: every claim
takes a paused actor out of the pool for good. So before each change the
warmer reads how many sandboxes the fleet acquired per image since its last
call (``fleet.acquired_count``), warm or cold, and takes them off the steps'
remaining demand, oldest step first. A rollout that was served, from the pool
or by a cold create, is then neither warmed for again nor kept for by a
finished step; and finishing a step drops only what its rollouts did not
acquire, never the replicas of steps that are still live.
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
        changes: image -> (previous target, new target), for the images
            whose target was applied.
        errors: image -> the exception that kept its change from applying.
            The image keeps its previous target, so the next call retries it.
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
        if not isinstance(keep_finished, int) or isinstance(keep_finished, bool) or keep_finished < 0:
            raise ValueError(f"keep_finished must be an int >= 0, got {keep_finished!r}")
        if cap is not None and (not isinstance(cap, int) or isinstance(cap, bool) or cap < 0):
            raise ValueError(f"cap must be an int >= 0 or None, got {cap!r}")
        # AsyncSandboxFleet wraps a SandboxFleet; the warmer drives the sync one.
        self._fleet = getattr(fleet, "_fleet", fleet)
        self.keep_finished = keep_finished
        self.cap = self._fleet.config.max_warmpool_replicas if cap is None else cap
        self.fail_fast = fail_fast
        # Remaining (not yet claimed) demand per step, {image: replicas}.
        self._live: dict[int, dict[str, int]] = {}
        self._finished: collections.deque[tuple[int, dict[str, int]]] = collections.deque()
        # Idle replicas per image this warmer last asked for, and the idle
        # count it saw right after (None: the driver cannot tell).
        self._target: dict[str, int] = {}
        self._expected_idle: dict[str, int | None] = {}
        # fleet.acquired_count(image) at the last observation, per image in demand.
        self._acquired_seen: dict[str, int] = {}
        self._lock = threading.Lock()
        self._closed = False

    @property
    def requested(self) -> dict[str, int]:
        """Idle replicas currently targeted per image (images at 0 omitted)."""
        with self._lock:
            return dict(self._target)

    def remaining(self) -> dict[int, dict[str, int]]:
        """Unclaimed demand per live or kept step, as last observed."""
        with self._lock:
            out = {step: dict(d) for step, d in self._finished}
            out.update({step: dict(d) for step, d in self._live.items()})
            return out

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
            self._observe_claims_locked()
            for img in demand:
                # Acquires made before an image's first step are not its claims.
                self._acquired_seen.setdefault(img, self._fleet.acquired_count(img))
            self._live[step] = demand
            return self._apply_locked(wait)

    def finish(self, step: int) -> WarmReport:
        """Mark step ``step`` done; its pools stay for ``keep_finished`` more finishes."""
        with self._lock:
            self._check_open()
            if step not in self._live:
                raise ValueError(f"step {step} is not live")
            self._observe_claims_locked()
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

    def _steps_oldest_first_locked(self) -> list[tuple[int, dict[str, int]]]:
        steps = list(self._finished) + list(self._live.items())
        return sorted(steps, key=lambda item: item[0])

    def _observe_claims_locked(self) -> None:
        """Take sandboxes acquired since the last call off the steps' demand.

        Claims are counted by the fleet (``acquired_count``), warm or cold, so
        rollouts served by cold creates (a pool smaller than the demand, or
        warming that fell behind) are counted too. They are attributed to the
        oldest steps first, which is how a trainer consumes them: earlier
        steps' rollouts are the ones in flight. The idle count is re-read
        only to trim a pool that grew beyond its target (recycled actors).
        """
        for img, seen in list(self._acquired_seen.items()):
            now = self._fleet.acquired_count(img)
            self._acquired_seen[img] = now
            claimed = now - seen
            for _step, demand in self._steps_oldest_first_locked():
                if claimed <= 0:
                    break
                take = min(claimed, demand.get(img, 0))
                if take:
                    demand[img] -= take
                    if not demand[img]:
                        del demand[img]
                    claimed -= take
        for img in list(self._expected_idle):
            idle = self._fleet.pool_size(img)
            if idle is not None:
                self._expected_idle[img] = idle

    def _desired_locked(self) -> dict[str, int]:
        total: collections.Counter[str] = collections.Counter()
        for _step, demand in self._steps_oldest_first_locked():
            total.update(demand)
        return {img: min(n, self.cap) for img, n in total.items() if n > 0}

    def _apply_locked(self, wait: bool, raise_errors: bool = True) -> WarmReport:
        desired = self._desired_locked()
        todo: dict[str, tuple[int, int]] = {}
        for img in set(desired) | set(self._target):
            old, new = self._target.get(img, 0), desired.get(img, 0)
            idle = self._expected_idle.get(img)
            # A changed target, or idle replicas beyond it (recycled actors).
            if old != new or (idle is not None and idle > new):
                todo[img] = (old, new)
        report = WarmReport()
        if not todo:
            return report

        def _apply_one(img: str, new: int) -> None:
            if new == 0:
                self._fleet.unwarm_image(img)
            else:
                self._fleet.set_pool_replicas(img, new, wait=wait)

        workers = min(self._fleet.config.warm_parallelism, len(todo))
        with ThreadPoolExecutor(max_workers=workers, thread_name_prefix="ate-env-lookahead") as ex:
            futures = {
                img: ex.submit(contextvars.copy_context().run, _apply_one, img, new)
                for img, (_old, new) in todo.items()
            }
            for img, fut in futures.items():
                err = fut.exception()
                old, new = todo[img]
                # Whatever happened, the next call measures claims against
                # the idle count as it is now, not as it was meant to be.
                self._expected_idle[img] = self._fleet.pool_size(img)
                if err is None:
                    if old != new:
                        report.changes[img] = (old, new)
                    if new:
                        self._target[img] = new
                    else:
                        self._target.pop(img, None)
                        self._expected_idle.pop(img, None)
                else:
                    report.errors[img] = err
                    logger.warning("Lookahead warm of %s (%d -> %d) failed: %s",
                                   img, old, new, err)
        if report.errors and self.fail_fast and raise_errors:
            raise next(iter(report.errors.values()))
        return report
