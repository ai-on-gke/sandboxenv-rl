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

"""Warm-pool sizing.

A warm pool's depth is its image's share of the concurrency budget, not its
task count: with ``max_concurrent`` claims in flight at most, an image that
holds a tenth of the tasks needs about a tenth of ``max_concurrent`` warm
replicas, however many tasks it has::

    replicas = clamp(round(max_concurrent * tasks_image / tasks_total),
                     1, min(tasks_image, max_pool))

``per_task`` sizes every pool to ``min(tasks_image, max_pool)`` instead, one
warm replica per task, for RL rollouts where every one of G rollouts of a
problem should claim at once rather than wait for a replica to come back.
"""

from __future__ import annotations

from collections.abc import Iterable, Mapping


def compute_replicas(
    tasks_image: int,
    tasks_total: int,
    max_concurrent: int,
    max_pool: int,
    *,
    per_task: bool = False,
) -> int:
    """Warm replicas for one image.

    Returns 0 when the image has no tasks or ``max_pool`` is 0 (no warm
    replicas: every claim creates an environment from the template).
    """
    if tasks_image <= 0 or max_pool <= 0:
        return 0
    cap = min(tasks_image, max_pool)
    if per_task:
        return cap
    total = max(tasks_total, tasks_image)
    share = round(max_concurrent * tasks_image / total)
    return max(1, min(share, cap))


def plan_replicas(
    counts: Mapping[str, int],
    max_concurrent: int,
    max_pool: int,
    *,
    per_task: bool = False,
    images: Iterable[str] | None = None,
) -> dict[str, int]:
    """Warm replicas per image for the images in ``images`` (all by default).

    The concurrency budget is shared by the selected images only, so a
    window of a sliding strategy is sized for the claims it will serve.
    """
    selected = list(counts) if images is None else list(images)
    total = sum(counts.get(img, 0) for img in selected)
    return {
        img: compute_replicas(
            counts.get(img, 0), total, max_concurrent, max_pool, per_task=per_task
        )
        for img in selected
    }
