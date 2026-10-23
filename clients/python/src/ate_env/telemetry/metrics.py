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

"""Prometheus histograms for the ate_env client, behind the ``metrics`` extra.

``prometheus_client`` is imported and the ``ate_env_client_*`` collectors are
registered lazily: nothing touches the default registry at import. Client
timings are recorded once :func:`enable_client_metrics` has been called (a
metrics-enabled ``ate_env.observability.Observer`` calls it); each histogram
registers on its first observation.
"""

from __future__ import annotations

import asyncio
import contextlib
import logging
import threading
import time
from collections.abc import Iterator
from typing import Any

logger = logging.getLogger("ate_env.telemetry.metrics")

# One bucket set, in seconds, for every ate_env histogram.
SEC_BUCKETS = (0.05, 0.1, 0.25, 0.5, 1, 2, 5, 10, 30, 60, 120, 300, 600)

_MISSING_EXTRA_MSG = (
    "prometheus_client not installed; metrics disabled. "
    "Install the metrics extra: pip install 'ate-env-client[metrics]'"
)

_LOCK = threading.Lock()
_COLLECTOR_LOCK = threading.RLock()
_PROM: Any = None  # the prometheus_client module once imported, or False
_MISSING_LOGGED = False


def _load_prom() -> Any:
    """Import prometheus_client on first use; returns the module or None."""
    global _PROM
    if _PROM is None:
        with _LOCK:
            if _PROM is None:
                try:
                    import prometheus_client
                except ImportError:
                    _PROM = False
                else:
                    _PROM = prometheus_client
    return _PROM or None


def prometheus_available() -> bool:
    return _load_prom() is not None


def log_missing_extra() -> None:
    """Log the missing-extra warning once per process."""
    global _MISSING_LOGGED
    with _LOCK:
        if _MISSING_LOGGED:
            return
        _MISSING_LOGGED = True
    logger.warning(_MISSING_EXTRA_MSG)


def status_of(exc: BaseException) -> str:
    """The ``status`` label for a block that raised ``exc``.

    ``cancelled`` for a cancellation (``asyncio.CancelledError``, for example a
    trainer that cancels stragglers) or an interrupt (``KeyboardInterrupt``),
    so these do not count as errors; ``error`` otherwise.
    """
    if isinstance(exc, (asyncio.CancelledError, KeyboardInterrupt)):
        return "cancelled"
    return "error"


# (id(registry) or 0 for the global one, name) -> (collector, type, label names)
_COLLECTORS: dict[tuple[int, str], tuple[Any, type, tuple[str, ...]]] = {}


def _metric(cls: Any, name: str, documentation: str, labelnames: Any = (), *,
            registry: Any = None, **kwargs: Any) -> Any:
    """Create a collector, or reuse the one this module created under ``name``.

    Idempotent across a second caller. A reuse must ask for the same type and
    label names, else ValueError. ``registry`` is a
    ``prometheus_client.CollectorRegistry`` (default: the global registry).
    After a module reload the cache is empty; the collector the earlier
    module registered is then looked up in the registry and checked the same
    way.
    """
    labels = tuple(labelnames)
    key = (id(registry) if registry is not None else 0, name)
    with _COLLECTOR_LOCK:
        hit = _COLLECTORS.get(key)
        if hit is None:
            reg_kw = {} if registry is None else {"registry": registry}
            try:
                collector = cls(name, documentation, labels, **reg_kw, **kwargs)
            except ValueError:
                collector = _registered(name, registry)
                if collector is None:
                    raise
            hit = _COLLECTORS[key] = (collector, type(collector),
                                      tuple(getattr(collector, "_labelnames", labels)))
    collector, kind, have = hit
    if not issubclass(kind, cls) or have != labels:
        raise ValueError(
            f"metric {name!r} already exists as {kind.__name__}{list(have)}, "
            f"not {cls.__name__}{list(labels)}"
        )
    return collector


def _registered(name: str, registry: Any) -> Any:
    """The collector already registered under ``name`` (module reload), or None."""
    prom = _load_prom()
    reg = registry if registry is not None else (prom.REGISTRY if prom else None)
    # _names_to_collectors is a private prometheus_client attribute (there is no
    # public lookup by name). It is read only on this reload path.
    names = getattr(reg, "_names_to_collectors", {})
    for cand in (name, name + "_total", name + "_created"):
        if cand in names:
            return names[cand]
    return None


# op -> (family, help)
CLIENT_FAMILIES: dict[str, tuple[str, str]] = {
    "acquire": ("ate_env_client_acquire_seconds",
                "Time for the client to acquire an environment (claim or create)"),
    "exec": ("ate_env_client_exec_seconds", "Time to run a command in an environment"),
    "suspend": ("ate_env_client_suspend_seconds", "Time to suspend an environment"),
    "warm_claim": ("ate_env_client_warm_claim_seconds", "Time to validate a warm claim"),
    "release": ("ate_env_client_release_seconds",
                "Time to release (delete or recycle) an environment"),
}

_ENABLED = False
_HISTOGRAMS: dict[str, Any] = {}


def enable_client_metrics() -> bool:
    """Start recording ``ate_env_client_*`` timings in this process.

    The switch is process-global and one-way: there is no way to turn it off,
    so a later fleet with ``enable_metrics=False`` still records them.
    Returns False (after one warning) when prometheus_client is not installed.
    """
    global _ENABLED
    if _load_prom() is None:
        log_missing_extra()
        return False
    _ENABLED = True
    return True


def client_metrics_enabled() -> bool:
    return _ENABLED


def _histogram(op: str) -> Any:
    hist = _HISTOGRAMS.get(op)
    if hist is None:
        with _LOCK:
            hist = _HISTOGRAMS.get(op)
            if hist is None:
                name, doc = CLIENT_FAMILIES[op]
                hist = _metric(_load_prom().Histogram, name, doc, ["status"], buckets=SEC_BUCKETS)
                _HISTOGRAMS[op] = hist
    return hist


def observe(op: str, seconds: float, status: str = "ok") -> None:
    """Record one client timing; a no-op until client metrics are enabled."""
    if not _ENABLED:
        return
    _histogram(op).labels(status=status).observe(seconds)


@contextlib.contextmanager
def timed(op: str) -> Iterator[None]:
    """Time the block into ``ate_env_client_<op>_seconds`` (status ok, error or cancelled)."""
    if not _ENABLED:
        yield
        return
    start = time.monotonic()
    status = "ok"
    try:
        yield
    except BaseException as e:
        status = status_of(e)
        raise
    finally:
        observe(op, time.monotonic() - start, status)
