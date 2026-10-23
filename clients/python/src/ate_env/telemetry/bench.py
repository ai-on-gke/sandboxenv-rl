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

"""SDK benchmark telemetry and trace instrumentation (W11).

Supports three modes configured explicitly or via ``ENV_BENCH_MODE``:
- ``off`` (default when unset): Zero-overhead no-op; ``instrument_client``
  returns the client unwrapped and ``track()`` yields immediately without
  timing or allocations.
- ``measure``: Thread-safe, bounded-memory Prometheus counters and histograms
  with zero extra RPCs and zero per-call disk writes on the hot path
  (Guardrail 4), plus optional coarse step-boundary flush to per-worker event
  shards ``<events_dir>/<worker_id>.jsonl`` and Prometheus textfile export.
- ``trace``: Per-call JSONL trace emission written directly via atomic
  ``os.write`` to an ``O_APPEND`` descriptor, validated against the W1
  ``trace_event.schema.json`` schema (Guardrail 1).
"""

# pylint: disable=too-many-arguments,too-many-positional-arguments,too-many-locals
# pylint: disable=too-many-instance-attributes,too-many-branches,too-many-statements
# pylint: disable=broad-exception-caught,too-many-lines,too-many-return-statements
# pylint: disable=undefined-all-variable

from __future__ import annotations

import asyncio
import contextlib
import contextvars
import dataclasses
import functools
import importlib.resources
import json
import logging
import math
import os
import random
import socket
import threading
import time
from collections.abc import AsyncIterator, Callable, Iterator, Mapping, Sequence
from datetime import datetime, timezone
from pathlib import Path
from typing import TYPE_CHECKING, Any

from .tracing import CLIENT_SPAN_PREFIX, combine_tracers

if TYPE_CHECKING:
    TRACE_EVENT_SCHEMA: dict[str, Any]
    EVENT_SCHEMA: dict[str, Any]
    TRACE_EVENT_ALLOWED_KEYS: frozenset[str]
    EVENT_ALLOWED_KEYS: frozenset[str]
    CANONICAL_TRACE_OPS: frozenset[str]
    CANONICAL_EVENT_SOURCES: frozenset[str]

logger = logging.getLogger("ate_env.telemetry")

__all__ = [
    "SCHEMA_VERSION",
    "TRACE_EVENT_SCHEMA",
    "EVENT_SCHEMA",
    "TRACE_EVENT_ALLOWED_KEYS",
    "EVENT_ALLOWED_KEYS",
    "CANONICAL_TRACE_OPS",
    "CANONICAL_EVENT_SOURCES",
    "TraceEventRecord",
    "BenchTelemetry",
    "validate_trace_event_dict",
    "validate_event_dict",
    "instrument_client",
    "instrument_fleet",
]

SCHEMA_VERSION = "v1"

# Bounded reservoir size per histogram key for O(1) memory retention.
_MAX_RESERVOIR_SAMPLES = 256
_MEASURE_ERR_MAX_CHARS = 160
_TRACE_ERR_MAX_CHARS = 512

# Task-local re-entrancy guards so delegating calls (Env.shell -> Env.start_process,
# Env.suspend -> Client.suspend) never double-count in instrument_client.
_IN_TRACKED_SHELL: contextvars.ContextVar[bool] = contextvars.ContextVar(
    "_ate_env_in_tracked_shell", default=False
)
_IN_TRACKED_LIFECYCLE: contextvars.ContextVar[bool] = contextvars.ContextVar(
    "_ate_env_in_tracked_lifecycle", default=False
)


@functools.lru_cache(maxsize=4)
def _load_w1_schema(filename: str) -> dict[str, Any]:
    """Load a W1 JSON Schema file from the repo or bundled package schemas."""
    here = Path(__file__).resolve()
    candidates: list[Path] = []
    try:
        candidates.append(
            here.parents[5] / "internal" / "benchmarking" / "rlbench" / "schema" / "v1" / filename
        )
    except IndexError:
        pass
    try:
        candidates.append(here.parents[1] / "schemas" / "v1" / filename)
    except IndexError:
        pass
    for candidate in candidates:
        if candidate.is_file():
            with candidate.open("r", encoding="utf-8") as fh:
                loaded = json.load(fh)
                if isinstance(loaded, dict):
                    return loaded
    with contextlib.suppress(Exception):
        pkg_res = importlib.resources.files("ate_env").joinpath("schemas", "v1", filename)
        if pkg_res.is_file():
            loaded = json.loads(pkg_res.read_text(encoding="utf-8"))
            if isinstance(loaded, dict):
                return loaded
    raise RuntimeError(f"ate_env: W1 JSON Schema {filename!r} not found")


def _get_trace_event_schema() -> dict[str, Any]:
    """Lazily load and return the W1 TraceEvent JSON Schema."""
    return _load_w1_schema("trace_event.schema.json")


def _get_event_schema() -> dict[str, Any]:
    """Lazily load and return the W1 Event JSON Schema."""
    return _load_w1_schema("event.schema.json")


@functools.lru_cache(maxsize=1)
def _get_trace_event_allowed_keys() -> frozenset[str]:
    """Lazily derive allowed TraceEvent keys from the W1 JSON Schema (Guardrail 1)."""
    return frozenset(_get_trace_event_schema().get("properties", {}).keys())


@functools.lru_cache(maxsize=1)
def _get_event_allowed_keys() -> frozenset[str]:
    """Lazily derive allowed Event keys from the W1 JSON Schema (Guardrail 1)."""
    return frozenset(_get_event_schema().get("properties", {}).keys())


@functools.lru_cache(maxsize=1)
def _get_canonical_trace_ops() -> frozenset[str]:
    """Lazily derive canonical TraceEvent op enum values from the W1 JSON Schema."""
    return frozenset(_get_trace_event_schema()["properties"]["op"]["enum"])


@functools.lru_cache(maxsize=1)
def _get_canonical_event_sources() -> frozenset[str]:
    """Lazily derive canonical Event source enum values from the W1 JSON Schema."""
    return frozenset(_get_event_schema()["properties"]["source"]["enum"])


_LAZY_SCHEMA_ATTRS: dict[str, Callable[[], Any]] = {
    "TRACE_EVENT_SCHEMA": _get_trace_event_schema,
    "EVENT_SCHEMA": _get_event_schema,
    "TRACE_EVENT_ALLOWED_KEYS": _get_trace_event_allowed_keys,
    "EVENT_ALLOWED_KEYS": _get_event_allowed_keys,
    "CANONICAL_TRACE_OPS": _get_canonical_trace_ops,
    "CANONICAL_EVENT_SOURCES": _get_canonical_event_sources,
}


def __getattr__(name: str) -> Any:
    """Lazily resolve W1 schema constants on first attribute access (PEP 562)."""
    getter = _LAZY_SCHEMA_ATTRS.get(name)
    if getter is not None:
        return getter()
    raise AttributeError(f"module {__name__!r} has no attribute {name!r}")

_DEFAULT_BUCKETS_MS: tuple[float, ...] = (
    1.0,
    5.0,
    10.0,
    25.0,
    50.0,
    100.0,
    250.0,
    500.0,
    1000.0,
    2500.0,
    5000.0,
    10000.0,
    30000.0,
    60000.0,
)

_OP_TO_EVENT_KIND: dict[str, str] = {
    "acquire": "acquire",
    "create": "create",
    "get": "get",
    "wake": "wake",
    "ttfi": "ttfi",
    "suspend": "suspend",
    "fork": "fork",
    "delete": "delete",
    "prewarm": "prewarm",
    "retire": "retire",
    "reap": "reap",
    "task_exec": "task",
    "exec": "step",
    "shell": "step",
    "start_process": "step",
    "read_file": "step",
    "write_file": "step",
}


def _utc_now_iso() -> str:
    """Return the current UTC completion timestamp formatted as RFC3339 with Z."""
    return datetime.now(timezone.utc).isoformat().replace("+00:00", "Z")


def _validate_rfc3339(ts_val: Any, label: str) -> None:
    """Validate that ts_val is a non-empty RFC3339 timestamp string with timezone."""
    if not isinstance(ts_val, str) or not ts_val.strip() or "T" not in ts_val:
        raise ValueError(f"{label} ts must be a valid RFC3339 timestamp, got {ts_val!r}")
    normalized = ts_val[:-1] + "+00:00" if ts_val.endswith(("Z", "z")) else ts_val
    try:
        parsed = datetime.fromisoformat(normalized)
    except ValueError as exc:
        raise ValueError(
            f"{label} ts must be a valid RFC3339 timestamp, got {ts_val!r}"
        ) from exc
    if parsed.tzinfo is None:
        raise ValueError(f"{label} ts must include a UTC offset or 'Z', got {ts_val!r}")


def _matches_json_type(val: Any, expected_type: str) -> bool:
    """Check whether a Python value matches a JSON Schema primitive type."""
    if expected_type == "string":
        return isinstance(val, str)
    if expected_type == "boolean":
        return isinstance(val, bool)
    if expected_type == "integer":
        return isinstance(val, int) and not isinstance(val, bool)
    if expected_type == "number":
        return isinstance(val, (int, float)) and not isinstance(val, bool)
    if expected_type == "array":
        return isinstance(val, (list, tuple))
    if expected_type == "object":
        return isinstance(val, Mapping)
    return True


def _validate_against_schema(
    payload: Mapping[str, Any],
    schema: Mapping[str, Any],
    label: str,
) -> None:
    """Deterministic W1 JSON Schema validator backed directly by the loaded schema."""
    properties: Mapping[str, Any] = schema.get("properties", {})
    allowed_keys = frozenset(properties.keys())
    if schema.get("additionalProperties") is False:
        unknown = set(payload.keys()) - allowed_keys
        if unknown:
            raise ValueError(f"{label} contains unknown W1 fields: {sorted(unknown)}")

    for req_key in schema.get("required", ()):
        if req_key not in payload or payload[req_key] is None:
            raise ValueError(f"{label} {req_key} is required")
        req_val = payload[req_key]
        if isinstance(req_val, str) and not req_val.strip():
            raise ValueError(f"{label} {req_key} is required")

    for key, val in payload.items():
        prop_spec = properties.get(key)
        if not isinstance(prop_spec, Mapping):
            continue
        expected_type = prop_spec.get("type")
        if isinstance(expected_type, str):
            if not _matches_json_type(val, expected_type):
                raise ValueError(
                    f"{label} {key} must be of type {expected_type}, got {val!r}"
                )
        elif isinstance(expected_type, list):
            if not any(_matches_json_type(val, t) for t in expected_type):
                raise ValueError(
                    f"{label} {key} must match one of types {expected_type}, got {val!r}"
                )

        if "const" in prop_spec and val != prop_spec["const"]:
            raise ValueError(
                f"{label} {key} must be {prop_spec['const']!r}, got {val!r}"
            )
        if "enum" in prop_spec and val not in prop_spec["enum"]:
            raise ValueError(
                f"{label} {key} must be one of {prop_spec['enum']}, got {val!r}"
            )
        if (
            "minLength" in prop_spec
            and isinstance(val, str)
            and len(val.strip()) < int(prop_spec["minLength"])
        ):
            raise ValueError(f"{label} {key} must be non-empty")
        if (
            "minimum" in prop_spec
            and isinstance(val, (int, float))
            and not isinstance(val, bool)
            and val < prop_spec["minimum"]
        ):
            raise ValueError(
                f"{label} {key} must be >= {prop_spec['minimum']}, got {val!r}"
            )
        if key == "ts" or prop_spec.get("format") == "date-time":
            _validate_rfc3339(val, label)


def validate_trace_event_dict(payload: Mapping[str, Any]) -> None:
    """Validate a dict against the W1 TraceEvent JSON schema (Guardrail 1)."""
    if "schema_version" in payload and payload.get("schema_version") != SCHEMA_VERSION:
        raise ValueError(
            f"trace event schema_version must be {SCHEMA_VERSION!r}, "
            f"got {payload.get('schema_version')!r}"
        )
    _validate_against_schema(payload, _get_trace_event_schema(), "trace event")


def validate_event_dict(payload: Mapping[str, Any]) -> None:
    """Validate a dict against the W1 Event JSON schema (Guardrail 1)."""
    _validate_against_schema(payload, _get_event_schema(), "event")


def _escape_prom_label(value: str) -> str:
    """Escape a Prometheus label value per the exposition format specification."""
    return value.replace("\\", "\\\\").replace('"', '\\"').replace("\n", "\\n")


def _format_error(exc_or_msg: BaseException | str, mode: str) -> str:
    """Format an error string, stripping multiline stdout/stderr in measure mode."""
    if isinstance(exc_or_msg, BaseException):
        exc_type = type(exc_or_msg).__name__
        raw = str(exc_or_msg).strip()
        if not raw:
            return exc_type
        first_line = raw.splitlines()[0].strip()
        if mode == "measure":
            formatted = (
                first_line
                if first_line.startswith(exc_type)
                else f"{exc_type}: {first_line}"
            )
            return formatted[:_MEASURE_ERR_MAX_CHARS]
        formatted = f"{exc_type}: {raw}" if not raw.startswith(exc_type) else raw
        return formatted[:_TRACE_ERR_MAX_CHARS]

    raw_str = str(exc_or_msg).strip()
    if not raw_str:
        return ""
    if mode == "measure":
        return raw_str.splitlines()[0].strip()[:_MEASURE_ERR_MAX_CHARS]
    return raw_str[:_TRACE_ERR_MAX_CHARS]


@dataclasses.dataclass(frozen=True)
class TraceEventRecord:
    """A single W1-compliant SDK trace event record.

    ``ts`` is the operation completion UTC timestamp (RFC3339 with ``Z``),
    matching ``swebench.Recorder`` (end timestamp + ``dur_ms``).
    """

    op: str
    dur_ms: float
    ok: bool = True
    ts: str = ""
    run: str = ""
    job: str = ""
    env: str = ""
    instance: str = ""
    image: str = ""
    step: int = 0
    command: Sequence[str] = ()
    path: str = ""
    bytes_count: int = 0
    exit_code: int | None = None
    err: str = ""

    def __post_init__(self) -> None:
        if not self.ts:
            object.__setattr__(self, "ts", _utc_now_iso())

    def to_dict(self) -> dict[str, Any]:
        """Serialize to a W1-validated TraceEvent dictionary."""
        timestamp = self.ts or _utc_now_iso()
        out: dict[str, Any] = {
            "schema_version": SCHEMA_VERSION,
            "ts": timestamp,
            "env": self.env,
            "op": self.op,
            "dur_ms": round(float(self.dur_ms), 4),
            "ok": bool(self.ok),
        }
        if self.run:
            out["run"] = self.run
        if self.job:
            out["job"] = self.job
        if self.instance:
            out["instance"] = self.instance
        if self.image:
            out["image"] = self.image
        if self.step > 0:
            out["step"] = int(self.step)
        if self.command:
            out["command"] = list(self.command)
        if self.path:
            out["path"] = self.path
        if self.bytes_count > 0:
            out["bytes"] = int(self.bytes_count)
        if self.exit_code is not None:
            out["exit"] = int(self.exit_code)
        if self.err:
            out["err"] = self.err
        validate_trace_event_dict(out)
        return out


@dataclasses.dataclass
class _HistogramState:
    """Bounded-memory histogram accumulator for a single label set."""

    buckets: tuple[float, ...] = _DEFAULT_BUCKETS_MS
    counts: list[int] = dataclasses.field(default_factory=list)
    total_count: int = 0
    total_sum: float = 0.0
    min_val: float = math.inf
    max_val: float = 0.0
    reservoir: list[float] = dataclasses.field(default_factory=list)

    def __post_init__(self) -> None:
        if not self.counts:
            self.counts = [0] * len(self.buckets)

    def observe(self, value_ms: float) -> None:
        """Record one latency observation in milliseconds using Algorithm R reservoir sampling."""
        self.total_count += 1
        self.total_sum += value_ms
        self.min_val = min(self.min_val, value_ms)
        self.max_val = max(self.max_val, value_ms)
        if len(self.reservoir) < _MAX_RESERVOIR_SAMPLES:
            self.reservoir.append(value_ms)
        else:
            replace_idx = random.randint(0, self.total_count - 1)
            if replace_idx < _MAX_RESERVOIR_SAMPLES:
                self.reservoir[replace_idx] = value_ms
        for idx, bound in enumerate(self.buckets):
            if value_ms <= bound:
                self.counts[idx] += 1


@dataclasses.dataclass
class _StepAggregate:
    """Bounded per-(op, name, source, step) summary state for measure mode."""

    op: str
    name: str
    source: str
    step: int
    count: int = 0
    ok_count: int = 0
    err_count: int = 0
    dur_ms_sum: float = 0.0
    dur_ms_max: float = 0.0
    slot_wait_ms_sum: float = 0.0
    retries_sum: int = 0
    timeouts_count: int = 0
    bytes_sum: int = 0
    first_ts: str = ""
    last_ts: str = ""
    last_env: str = ""
    last_exit_code: int | None = None
    last_err: str = ""
    cache_state: str = "unknown"

    def observe(
        self,
        *,
        ts: str,
        dur_ms: float,
        ok: bool,
        env_id: str,
        bytes_count: int,
        exit_code: int | None,
        err: str,
        cache_state: str,
        slot_wait_ms: float,
        retried: int,
        timed_out: bool,
    ) -> None:
        """Accumulate one operation into this step aggregate."""
        self.count += 1
        if ok:
            self.ok_count += 1
        else:
            self.err_count += 1
        self.dur_ms_sum += dur_ms
        self.dur_ms_max = max(self.dur_ms_max, dur_ms)
        self.slot_wait_ms_sum += max(0.0, slot_wait_ms)
        self.retries_sum += max(0, retried)
        if timed_out:
            self.timeouts_count += 1
        self.bytes_sum += max(0, bytes_count)
        if not self.first_ts:
            self.first_ts = ts
        self.last_ts = ts
        if env_id:
            self.last_env = env_id
        if exit_code is not None:
            self.last_exit_code = exit_code
        if err:
            self.last_err = err
        if cache_state and cache_state != "unknown":
            self.cache_state = cache_state


def _percentile(sorted_values: Sequence[float], q: float) -> float:
    """Compute the nearest-rank percentile of a sorted sequence."""
    n = len(sorted_values)
    if n == 0:
        return 0.0
    if q >= 1.0:
        return float(sorted_values[-1])
    idx = max(0, min(n - 1, int(math.ceil(q * n)) - 1))
    return float(sorted_values[idx])


def _resolve_events_shard_path(raw_path: str | Path, worker_id: str) -> Path:
    """Resolve events_path to <dir>/<worker_id>.jsonl unless a .jsonl file is given."""
    path_obj = Path(raw_path)
    if path_obj.suffix == ".jsonl" and not path_obj.is_dir():
        return path_obj
    safe_worker = "".join(
        ch if (ch.isalnum() or ch in ("-", "_", ".")) else "-" for ch in worker_id
    ).strip("-") or "worker"
    return path_obj / f"{safe_worker}.jsonl"


class BenchTelemetry:
    """Low-overhead, thread-safe SDK telemetry collector and W1 trace emitter (W11).

    - ``off`` (default when ``ENV_BENCH_MODE`` is unset): Zero-overhead no-op.
    - ``measure``: Bounded in-memory histograms, counters, and per-step
      aggregates (zero per-call I/O or extra RPCs).
    - ``trace``: Per-call W1-validated JSONL lines appended atomically via
      ``os.write`` to an ``O_APPEND`` file descriptor opened once at init.
    """

    def __init__(
        self,
        mode: str | None = None,
        *,
        run_id: str | None = None,
        job: str | None = None,
        worker_id: str | None = None,
        trace_path: str | Path | None = None,
        events_path: str | Path | None = None,
        metrics_path: str | Path | None = None,
    ) -> None:
        resolved_mode = (mode or os.environ.get("ENV_BENCH_MODE") or "off").strip().lower()
        if resolved_mode not in ("off", "measure", "trace"):
            raise ValueError(
                f"invalid telemetry mode {resolved_mode!r}; expected 'off', 'measure', or 'trace'"
            )
        self.mode: str = resolved_mode
        self.run_id: str = run_id or os.environ.get("ENV_BENCH_RUN_ID") or "sdk-bench"
        self.job: str = job or os.environ.get("ENV_BENCH_JOB") or "default"
        default_worker = f"{socket.gethostname()}-{os.getpid()}"
        self.worker_id: str = (
            worker_id or os.environ.get("ENV_BENCH_WORKER_ID") or default_worker
        ).strip()

        artifact_dir = os.environ.get("ARTIFACT_DIR", "").strip()
        raw_trace = trace_path or os.environ.get("ENV_BENCH_TRACE_PATH")
        if not raw_trace and artifact_dir and self.mode == "trace":
            raw_trace = Path(artifact_dir) / "trace.jsonl"
        if self.mode == "trace" and not raw_trace:
            raise ValueError(
                "trace mode requires trace_path, ENV_BENCH_TRACE_PATH, or ARTIFACT_DIR"
            )
        self.trace_path: Path | None = Path(raw_trace) if raw_trace else None

        raw_events = events_path or os.environ.get("ENV_BENCH_EVENTS_PATH")
        if not raw_events and artifact_dir and self.mode in ("measure", "trace"):
            raw_events = Path(artifact_dir) / "events"
        self.events_path: Path | None = (
            _resolve_events_shard_path(raw_events, self.worker_id) if raw_events else None
        )

        raw_metrics = metrics_path or os.environ.get("ENV_BENCH_METRICS_PATH")
        if not raw_metrics and artifact_dir and self.mode in ("measure", "trace"):
            raw_metrics = Path(artifact_dir) / "client_metrics.prom"
        self.metrics_path: Path | None = Path(raw_metrics) if raw_metrics else None

        self.current_step: int = 0
        self._lock = threading.Lock()

        # In-memory Prometheus state keyed by (op, source, cache_state, ok_str).
        self._histograms: dict[tuple[str, str, str, str], _HistogramState] = {}
        self._slot_wait_histograms: dict[str, _HistogramState] = {}
        self._counters: dict[tuple[str, str], int] = {}
        # Bounded per-(op, name, source, step) step aggregates for measure mode.
        self._step_aggregates: dict[tuple[str, str, str, int], _StepAggregate] = {}

        self._trace_fd: int | None = None
        self._events_fd: int | None = None
        self._events_fd_path: Path | None = None

        if self.mode != "off":
            _get_trace_event_schema()
            _get_event_schema()

        if self.mode == "trace" and self.trace_path is not None:
            self.trace_path.parent.mkdir(parents=True, exist_ok=True)
            self._trace_fd = os.open(
                str(self.trace_path), os.O_APPEND | os.O_CREAT | os.O_WRONLY, 0o644
            )

    def close(self) -> None:
        """Close open file descriptors and dump Prometheus metrics if configured."""
        if self.mode == "measure" and self.metrics_path is not None:
            with contextlib.suppress(Exception):
                self.dump_prometheus()
        with self._lock:
            if self._trace_fd is not None:
                with contextlib.suppress(OSError):
                    os.close(self._trace_fd)
                self._trace_fd = None
            if self._events_fd is not None:
                with contextlib.suppress(OSError):
                    os.close(self._events_fd)
                self._events_fd = None
                self._events_fd_path = None

    def __del__(self) -> None:
        with contextlib.suppress(Exception):
            with self._lock:
                if self._trace_fd is not None:
                    os.close(self._trace_fd)
                    self._trace_fd = None
                if self._events_fd is not None:
                    os.close(self._events_fd)
                    self._events_fd = None

    def set_step(self, step: int) -> None:
        """Update the current RL training step index for subsequent operations."""
        if step < 0:
            raise ValueError(f"step must be >= 0, got {step}")
        with self._lock:
            self.current_step = step

    def record_op(
        self,
        op: str,
        dur_ms: float,
        *,
        ok: bool = True,
        env_id: str = "",
        image: str = "",
        instance: str = "",
        step: int | None = None,
        step_name: str = "",
        command: Sequence[str] = (),
        path: str = "",
        bytes_count: int = 0,
        exit_code: int | None = None,
        err: str = "",
        source: str = "direct",
        cache_state: str = "unknown",
        slot_wait_ms: float = 0.0,
        retried: int = 0,
        timed_out: bool = False,
        ts: str = "",
        emit_trace: bool = True,
        trace_op: str = "",
        trace_only: bool = False,
    ) -> None:
        """Record a completed SDK operation.

        ``ts`` is captured at operation completion (UTC RFC3339 with ``Z``).
        In ``trace`` mode, only canonical W1 ``TraceEvent`` ops
        (``CANONICAL_TRACE_OPS``) with ``emit_trace=True`` are written to
        ``trace.jsonl``; higher-level fleet/sub-phase aggregates (``acquire``,
        ``ttfi``, ``prewarm``, ``retire``, ``reap``, ``task_exec``) are routed
        to step-boundary ``Event`` records and Prometheus histograms unless
        ``trace_op`` maps them to a canonical ``TraceEvent`` op (e.g.,
        ``acquire`` -> ``create`` or ``wake``).
        """
        if self.mode == "off" or (trace_only and self.mode != "trace"):
            return

        completion_ts = ts or _utc_now_iso()
        formatted_err = _format_error(err, self.mode) if err else ""
        if timed_out and not formatted_err:
            formatted_err = "TimeoutError"

        with self._lock:
            effective_step = self.current_step if step is None else step
            if not trace_only:
                key = (op, source, cache_state, "true" if ok else "false")
                hist = self._histograms.get(key)
                if hist is None:
                    hist = _HistogramState()
                    self._histograms[key] = hist
                hist.observe(dur_ms)

                if slot_wait_ms > 0.0:
                    sw_hist = self._slot_wait_histograms.get(op)
                    if sw_hist is None:
                        sw_hist = _HistogramState()
                        self._slot_wait_histograms[op] = sw_hist
                    sw_hist.observe(slot_wait_ms)

                if not ok:
                    self._counters[(op, "error")] = self._counters.get((op, "error"), 0) + 1
                if retried > 0:
                    self._counters[(op, "retry")] = (
                        self._counters.get((op, "retry"), 0) + retried
                    )
                if timed_out:
                    self._counters[(op, "timeout")] = (
                        self._counters.get((op, "timeout"), 0) + 1
                    )

                effective_name = step_name or op
                agg_key = (op, effective_name, source, effective_step)
                agg = self._step_aggregates.get(agg_key)
                if agg is None:
                    agg = _StepAggregate(
                        op=op,
                        name=effective_name,
                        source=source,
                        step=effective_step,
                    )
                    self._step_aggregates[agg_key] = agg
                agg.observe(
                    ts=completion_ts,
                    dur_ms=dur_ms,
                    ok=ok,
                    env_id=env_id,
                    bytes_count=bytes_count,
                    exit_code=exit_code,
                    err=formatted_err,
                    cache_state=cache_state,
                    slot_wait_ms=slot_wait_ms,
                    retried=retried,
                    timed_out=timed_out,
                )

            # Guardrail 4: Per-call JSONL file logging is strictly restricted to trace mode
            # and canonical W1 TraceEvent operations.
            canonical_ops = _get_canonical_trace_ops()
            effective_trace_op = trace_op or (op if op in canonical_ops else "")
            if (
                self.mode == "trace"
                and emit_trace
                and effective_trace_op in canonical_ops
            ):
                if not env_id or not env_id.strip():
                    logger.warning(
                        "Skipping W1 TraceEvent %r in trace mode because env_id is empty",
                        effective_trace_op,
                    )
                else:
                    record = TraceEventRecord(
                        op=effective_trace_op,
                        dur_ms=dur_ms,
                        ok=ok,
                        ts=completion_ts,
                        run=self.run_id,
                        job=self.job,
                        env=env_id,
                        instance=instance,
                        image=image,
                        step=effective_step,
                        command=command,
                        path=path,
                        bytes_count=bytes_count,
                        exit_code=exit_code,
                        err=formatted_err,
                    )
                    payload = record.to_dict()
                    line_bytes = (json.dumps(payload, separators=(",", ":")) + "\n").encode(
                        "utf-8"
                    )
                    if self._trace_fd is None and self.trace_path is not None:
                        self.trace_path.parent.mkdir(parents=True, exist_ok=True)
                        self._trace_fd = os.open(
                            str(self.trace_path),
                            os.O_APPEND | os.O_CREAT | os.O_WRONLY,
                            0o644,
                        )
                    if self._trace_fd is not None:
                        os.write(self._trace_fd, line_bytes)

    @contextlib.asynccontextmanager
    async def track(
        self,
        op: str,
        *,
        env_id: str = "",
        image: str = "",
        instance: str = "",
        step: int | None = None,
        step_name: str = "",
        command: Sequence[str] = (),
        path: str = "",
        bytes_count: int = 0,
        source: str = "direct",
        cache_state: str = "unknown",
        trace_op: str = "",
    ) -> AsyncIterator[dict[str, Any]]:
        """Async context manager that times an SDK call and records telemetry."""
        if self.mode == "off":
            yield {}
            return

        ctx_meta: dict[str, Any] = {
            "env_id": env_id,
            "image": image,
            "instance": instance,
            "step_name": step_name,
            "bytes": bytes_count,
            "exit_code": None,
            "source": source,
            "cache_state": cache_state,
            "slot_wait_ms": 0.0,
            "retried": 0,
            "timed_out": False,
            "trace_op": trace_op,
        }
        start = time.perf_counter()
        ok = True
        err_msg = ""
        try:
            yield ctx_meta
        except asyncio.CancelledError as exc:
            ok = False
            ctx_meta["timed_out"] = False
            err_msg = _format_error(exc, self.mode) or "CancelledError"
            raise
        except BaseException as exc:
            ok = False
            err_msg = _format_error(exc, self.mode)
            if isinstance(exc, (TimeoutError, asyncio.TimeoutError)) or (
                type(exc).__name__ in ("CommandTimeoutError", "SandboxStartError")
                and "timed out" in str(exc).lower()
            ):
                ctx_meta["timed_out"] = True
            raise
        finally:
            dur_ms = (time.perf_counter() - start) * 1000.0
            self.record_op(
                op,
                dur_ms,
                ok=ok,
                env_id=str(ctx_meta.get("env_id") or env_id),
                image=str(ctx_meta.get("image") or image),
                instance=str(ctx_meta.get("instance") or instance),
                step=step,
                step_name=str(ctx_meta.get("step_name") or step_name),
                command=command,
                path=path,
                bytes_count=int(ctx_meta.get("bytes") or 0),
                exit_code=ctx_meta.get("exit_code"),
                err=err_msg,
                source=str(ctx_meta.get("source") or source),
                cache_state=str(ctx_meta.get("cache_state") or cache_state),
                slot_wait_ms=float(ctx_meta.get("slot_wait_ms") or 0.0),
                retried=int(ctx_meta.get("retried") or 0),
                timed_out=bool(ctx_meta.get("timed_out")),
                trace_op=str(ctx_meta.get("trace_op") or trace_op),
            )

    def render_prometheus(self) -> str:
        """Render in-memory histograms and counters in Prometheus exposition format."""
        with self._lock:
            hist_snapshot = [
                (k, list(h.buckets), list(h.counts), h.total_count, h.total_sum)
                for k, h in sorted(self._histograms.items())
            ]
            sw_snapshot = [
                (op, list(h.buckets), list(h.counts), h.total_count, h.total_sum)
                for op, h in sorted(self._slot_wait_histograms.items())
            ]
            counter_snapshot = list(sorted(self._counters.items()))

        esc_job = _escape_prom_label(self.job)
        lines: list[str] = [
            "# HELP ate_env_sdk_op_duration_seconds SDK operation latency in seconds.",
            "# TYPE ate_env_sdk_op_duration_seconds histogram",
        ]
        for (
            (op, source, cache_state, ok_str),
            buckets,
            counts,
            total_count,
            total_sum,
        ) in hist_snapshot:
            base_labels = (
                f'job="{esc_job}",op="{_escape_prom_label(op)}",'
                f'source="{_escape_prom_label(source)}",'
                f'cache_state="{_escape_prom_label(cache_state)}",'
                f'ok="{_escape_prom_label(ok_str)}"'
            )
            for bound_ms, count in zip(buckets, counts):
                bound_s = bound_ms / 1000.0
                lines.append(
                    f"ate_env_sdk_op_duration_seconds_bucket"
                    f'{{{base_labels},le="{bound_s:g}"}} {count}'
                )
            lines.append(
                f'ate_env_sdk_op_duration_seconds_bucket{{{base_labels},le="+Inf"}} {total_count}'
            )
            lines.append(
                f"ate_env_sdk_op_duration_seconds_sum{{{base_labels}}} {total_sum / 1000.0:.6f}"
            )
            lines.append(f"ate_env_sdk_op_duration_seconds_count{{{base_labels}}} {total_count}")

        lines.extend(
            [
                "# HELP ate_env_sdk_op_duration_ms SDK operation latency in milliseconds.",
                "# TYPE ate_env_sdk_op_duration_ms histogram",
            ]
        )
        for (
            (op, source, cache_state, ok_str),
            buckets,
            counts,
            total_count,
            total_sum,
        ) in hist_snapshot:
            base_labels = (
                f'job="{esc_job}",op="{_escape_prom_label(op)}",'
                f'source="{_escape_prom_label(source)}",'
                f'cache_state="{_escape_prom_label(cache_state)}",'
                f'ok="{_escape_prom_label(ok_str)}"'
            )
            for bound_ms, count in zip(buckets, counts):
                lines.append(
                    f'ate_env_sdk_op_duration_ms_bucket{{{base_labels},le="{bound_ms}"}} {count}'
                )
            lines.append(
                f'ate_env_sdk_op_duration_ms_bucket{{{base_labels},le="+Inf"}} {total_count}'
            )
            lines.append(f"ate_env_sdk_op_duration_ms_sum{{{base_labels}}} {total_sum:.4f}")
            lines.append(f"ate_env_sdk_op_duration_ms_count{{{base_labels}}} {total_count}")

        if sw_snapshot:
            lines.append(
                "# HELP ate_env_sdk_slot_wait_seconds Client-side slot wait latency in seconds."
            )
            lines.append("# TYPE ate_env_sdk_slot_wait_seconds histogram")
            for op, buckets, counts, total_count, total_sum in sw_snapshot:
                base_labels = f'job="{esc_job}",op="{_escape_prom_label(op)}"'
                for bound_ms, count in zip(buckets, counts):
                    bound_s = bound_ms / 1000.0
                    lines.append(
                        f"ate_env_sdk_slot_wait_seconds_bucket"
                        f'{{{base_labels},le="{bound_s:g}"}} {count}'
                    )
                lines.append(
                    f'ate_env_sdk_slot_wait_seconds_bucket{{{base_labels},le="+Inf"}} {total_count}'
                )
                lines.append(
                    f"ate_env_sdk_slot_wait_seconds_sum{{{base_labels}}} {total_sum / 1000.0:.6f}"
                )
                lines.append(f"ate_env_sdk_slot_wait_seconds_count{{{base_labels}}} {total_count}")

            lines.append(
                "# HELP ate_env_sdk_slot_wait_ms Client-side slot wait latency in milliseconds."
            )
            lines.append("# TYPE ate_env_sdk_slot_wait_ms histogram")
            for op, buckets, counts, total_count, total_sum in sw_snapshot:
                base_labels = f'job="{esc_job}",op="{_escape_prom_label(op)}"'
                for bound_ms, count in zip(buckets, counts):
                    lines.append(
                        f'ate_env_sdk_slot_wait_ms_bucket{{{base_labels},le="{bound_ms}"}} {count}'
                    )
                lines.append(
                    f'ate_env_sdk_slot_wait_ms_bucket{{{base_labels},le="+Inf"}} {total_count}'
                )
                lines.append(f"ate_env_sdk_slot_wait_ms_sum{{{base_labels}}} {total_sum:.4f}")
                lines.append(f"ate_env_sdk_slot_wait_ms_count{{{base_labels}}} {total_count}")

        lines.append("# HELP ate_env_sdk_events_total SDK error, retry, and timeout counters.")
        lines.append("# TYPE ate_env_sdk_events_total counter")
        for (op, kind), count in counter_snapshot:
            lines.append(
                f'ate_env_sdk_events_total{{job="{esc_job}",'
                f'op="{_escape_prom_label(op)}",kind="{_escape_prom_label(kind)}"}} {count}'
            )

        return "\n".join(lines) + "\n"

    def dump_prometheus(self, path: str | Path | None = None) -> Path | None:
        """Atomically write Prometheus exposition text to path or self.metrics_path."""
        target = Path(path) if path is not None else self.metrics_path
        if target is None:
            return None
        content = self.render_prometheus().encode("utf-8")
        target.parent.mkdir(parents=True, exist_ok=True)
        tmp_target = target.parent / f".{target.name}.{os.getpid()}.{threading.get_ident()}.tmp"
        fd = os.open(str(tmp_target), os.O_WRONLY | os.O_CREAT | os.O_TRUNC, 0o644)
        try:
            os.write(fd, content)
        finally:
            os.close(fd)
        os.replace(tmp_target, target)
        return target

    def flush_step_events(
        self,
        step: int | None = None,
        *,
        path: str | Path | None = None,
    ) -> list[dict[str, Any]]:
        """Flush bounded per-step operation aggregates as W1 Event JSONL lines.

        Each emitted ``Event`` aggregates ``count`` operations for a given
        ``(op, name, source, step)`` key: ``dur_ms`` is the total duration
        across those ``count`` operations, while ``metrics["dur_ms_mean"]``
        and ``metrics["dur_ms_max"]`` report the per-operation mean and max.

        Note on ``ttfi`` and ``wake``:
        - SDK ``ttfi`` (in ``events/*.jsonl``) measures the Ready-to-first-command-end
          sub-phase on a cold create (``acquire`` measures create-start to Ready, so
          total create-start to first-command completion is ``acquire + ttfi``).
        - On warm-pool or snapshot acquire, ``trace.jsonl`` ``op="wake"`` records the
          wire claim-to-Ready duration, whereas ``events/*.jsonl`` ``event="wake"``
          (emitted on the first ``exec`` after warm/snapshot acquire) records the
          Ready-to-first-command-end sub-phase (matching ``event="ttfi"`` on cold start).
        """
        with self._lock:
            effective_step = self.current_step if step is None else step
            drained = list(self._step_aggregates.values())
            self._step_aggregates.clear()

        target_path = (
            _resolve_events_shard_path(path, self.worker_id)
            if path is not None
            else self.events_path
        )
        fallback_iso = _utc_now_iso()
        emitted: list[dict[str, Any]] = []

        for agg in drained:
            ev_kind = _OP_TO_EVENT_KIND.get(agg.op, "step")
            dur_sum = round(float(agg.dur_ms_sum), 4)
            dur_mean = round(float(agg.dur_ms_sum) / max(1, agg.count), 4)
            ev: dict[str, Any] = {
                "schema_version": SCHEMA_VERSION,
                "ts": agg.last_ts or fallback_iso,
                "run": self.run_id,
                "worker": self.worker_id,
                "event": ev_kind,
                "dur_ms": dur_sum,
                "ok": agg.err_count == 0,
                "count": agg.count,
            }
            if agg.name and (ev_kind in ("step", "task") or agg.name != agg.op):
                ev["name"] = agg.name
            if self.job:
                ev["job"] = self.job
            if agg.last_env:
                ev["env"] = agg.last_env
            st = agg.step if agg.step > 0 else effective_step
            if st > 0:
                ev["step"] = st
            if agg.retries_sum > 0:
                ev["attempts"] = agg.count + agg.retries_sum
            if agg.bytes_sum > 0:
                ev["bytes"] = agg.bytes_sum
            if agg.last_exit_code is not None:
                ev["exit"] = agg.last_exit_code
            if agg.last_err:
                ev["err"] = agg.last_err
            if agg.cache_state in ("hit", "miss"):
                ev["cache_hit"] = agg.cache_state == "hit"
            if agg.slot_wait_ms_sum > 0.0:
                ev["slot_wait_ms"] = round(float(agg.slot_wait_ms_sum), 4)
            if agg.timeouts_count > 0:
                ev["timeout"] = True

            tags: dict[str, str] = {"worker": self.worker_id, "op": agg.op}
            if agg.source:
                tags["source"] = agg.source
            if agg.source in _get_canonical_event_sources():
                ev["source"] = agg.source
            ev["tags"] = tags

            metrics: dict[str, float] = {
                "dur_ms_sum": dur_sum,
                "dur_ms_mean": dur_mean,
                "dur_ms_max": round(float(agg.dur_ms_max), 4),
                "ok_count": float(agg.ok_count),
                "err_count": float(agg.err_count),
            }
            if agg.slot_wait_ms_sum > 0.0:
                metrics["slot_wait_ms"] = round(float(agg.slot_wait_ms_sum), 4)
            if agg.timeouts_count > 0:
                metrics["timeouts"] = float(agg.timeouts_count)
            ev["metrics"] = metrics

            validate_event_dict(ev)
            emitted.append(ev)

        if target_path is not None and emitted:
            payload_bytes = b"".join(
                (json.dumps(ev, separators=(",", ":")) + "\n").encode("utf-8")
                for ev in emitted
            )
            with self._lock:
                if path is None and target_path == self.events_path:
                    if self._events_fd is None or self._events_fd_path != target_path:
                        target_path.parent.mkdir(parents=True, exist_ok=True)
                        self._events_fd = os.open(
                            str(target_path),
                            os.O_APPEND | os.O_CREAT | os.O_WRONLY,
                            0o644,
                        )
                        self._events_fd_path = target_path
                    os.write(self._events_fd, payload_bytes)
                else:
                    target_path.parent.mkdir(parents=True, exist_ok=True)
                    fd = os.open(
                        str(target_path),
                        os.O_APPEND | os.O_CREAT | os.O_WRONLY,
                        0o644,
                    )
                    try:
                        os.write(fd, payload_bytes)
                    finally:
                        os.close(fd)

        if self.mode == "measure" and self.metrics_path is not None:
            with contextlib.suppress(Exception):
                self.dump_prometheus()

        return emitted

    def summary_stats(self) -> dict[str, dict[str, float]]:
        """Return p50, p95, max, count, errors, retries, and timeouts per operation."""
        out: dict[str, dict[str, float]] = {}
        by_op_samples: dict[str, list[float]] = {}
        by_op_count: dict[str, int] = {}
        by_op_max: dict[str, float] = {}
        with self._lock:
            for (op, _, _, _), hist in self._histograms.items():
                by_op_samples.setdefault(op, []).extend(hist.reservoir)
                by_op_count[op] = by_op_count.get(op, 0) + hist.total_count
                if hist.total_count > 0:
                    by_op_max[op] = max(by_op_max.get(op, 0.0), hist.max_val)
            counters_snapshot = dict(self._counters)
        for op, samples in by_op_samples.items():
            ordered = sorted(samples)
            out[op] = {
                "count": float(by_op_count.get(op, len(ordered))),
                "p50_ms": _percentile(ordered, 0.50),
                "p95_ms": _percentile(ordered, 0.95),
                "max_ms": float(by_op_max.get(op, _percentile(ordered, 1.00))),
                "errors": float(counters_snapshot.get((op, "error"), 0)),
                "retries": float(counters_snapshot.get((op, "retry"), 0)),
                "timeouts": float(counters_snapshot.get((op, "timeout"), 0)),
            }
        return out


def _extract_env_id(args: tuple[Any, ...], kwargs: Mapping[str, Any]) -> str:
    """Extract environment ID from positional or keyword arguments ('id' or 'env_id')."""
    if args:
        return str(args[0])
    val = kwargs.get("id")
    if val is None:
        val = kwargs.get("env_id")
    return str(val) if val is not None else ""


def _wrap_env_handle(env_handle: Any, telemetry: BenchTelemetry) -> Any:
    """Wrap an Env instance's methods so calls record into telemetry without double-counting."""
    if telemetry.mode == "off" or getattr(env_handle, "_bench_telemetry_wrapped", False):
        return env_handle

    orig_shell = env_handle.shell
    orig_start_process = env_handle.start_process
    orig_write_file = env_handle.write_file
    orig_read_file_bytes = env_handle.read_file_bytes
    orig_suspend = env_handle.suspend
    orig_delete = env_handle.delete

    @functools.wraps(orig_shell)
    async def tracked_shell(command_line: str, *args: Any, **kwargs: Any) -> Any:
        step_name = str(kwargs.pop("_bench_step_name", "") or "")
        token = _IN_TRACKED_SHELL.set(True)
        try:
            async with telemetry.track(
                "shell",
                env_id=env_handle.id,
                step_name=step_name,
                command=("sh", "-c", command_line),
            ) as meta:
                res = await orig_shell(command_line, *args, **kwargs)
                if meta:
                    meta["exit_code"] = res.exit_code
                return res
        finally:
            _IN_TRACKED_SHELL.reset(token)

    @functools.wraps(orig_start_process)
    async def tracked_start_process(command: Sequence[str], *args: Any, **kwargs: Any) -> Any:
        if _IN_TRACKED_SHELL.get():
            return await orig_start_process(command, *args, **kwargs)
        step_name = str(kwargs.pop("_bench_step_name", "") or "")
        async with telemetry.track(
            "start_process",
            env_id=env_handle.id,
            step_name=step_name,
            command=tuple(command),
        ):
            return await orig_start_process(command, *args, **kwargs)

    @functools.wraps(orig_write_file)
    async def tracked_write_file(path: str, data: Any, *args: Any, **kwargs: Any) -> int:
        async with telemetry.track("write_file", env_id=env_handle.id, path=path) as meta:
            written = await orig_write_file(path, data, *args, **kwargs)
            if meta:
                meta["bytes"] = int(written)
            return written

    @functools.wraps(orig_read_file_bytes)
    async def tracked_read_file_bytes(path: str, *args: Any, **kwargs: Any) -> bytes:
        async with telemetry.track("read_file", env_id=env_handle.id, path=path) as meta:
            content = await orig_read_file_bytes(path, *args, **kwargs)
            if meta:
                meta["bytes"] = len(content)
            return content

    @functools.wraps(orig_suspend)
    async def tracked_suspend(*args: Any, **kwargs: Any) -> None:
        if _IN_TRACKED_LIFECYCLE.get():
            await orig_suspend(*args, **kwargs)
            return
        token = _IN_TRACKED_LIFECYCLE.set(True)
        try:
            async with telemetry.track("suspend", env_id=env_handle.id):
                await orig_suspend(*args, **kwargs)
        finally:
            _IN_TRACKED_LIFECYCLE.reset(token)

    @functools.wraps(orig_delete)
    async def tracked_delete(*args: Any, **kwargs: Any) -> None:
        if _IN_TRACKED_LIFECYCLE.get():
            await orig_delete(*args, **kwargs)
            return
        token = _IN_TRACKED_LIFECYCLE.set(True)
        try:
            async with telemetry.track("delete", env_id=env_handle.id):
                await orig_delete(*args, **kwargs)
        finally:
            _IN_TRACKED_LIFECYCLE.reset(token)

    env_handle.shell = tracked_shell
    env_handle.start_process = tracked_start_process
    env_handle.write_file = tracked_write_file
    env_handle.read_file_bytes = tracked_read_file_bytes
    env_handle.suspend = tracked_suspend
    env_handle.delete = tracked_delete
    setattr(env_handle, "_bench_telemetry_wrapped", True)
    return env_handle


def instrument_client(client: Any, telemetry: BenchTelemetry) -> Any:
    """Attach BenchTelemetry instrumentation to an ate_env.Client in place.

    When ``telemetry.mode == 'off'``, returns ``client`` completely unwrapped.
    """
    if telemetry.mode == "off" or getattr(client, "_bench_telemetry_wrapped", False):
        return client

    orig_create: Callable[..., Any] = client.create
    orig_get: Callable[..., Any] | None = getattr(client, "get", None)
    orig_suspend: Callable[..., Any] = client.suspend
    orig_delete: Callable[..., Any] = client.delete
    orig_env: Callable[..., Any] = client.env

    @functools.wraps(orig_create)
    async def tracked_create(*args: Any, **kwargs: Any) -> Any:
        if _IN_TRACKED_LIFECYCLE.get():
            handle = await orig_create(*args, **kwargs)
            return _wrap_env_handle(handle, telemetry)
        env_id = _extract_env_id(args, kwargs)
        token = _IN_TRACKED_LIFECYCLE.set(True)
        try:
            async with telemetry.track("create", env_id=env_id, source="cold_create") as meta:
                handle = await orig_create(*args, **kwargs)
                if meta and not env_id and getattr(handle, "id", ""):
                    meta["env_id"] = handle.id
        finally:
            _IN_TRACKED_LIFECYCLE.reset(token)
        return _wrap_env_handle(handle, telemetry)

    if orig_get is not None:

        @functools.wraps(orig_get)
        async def tracked_get(*args: Any, **kwargs: Any) -> Any:
            if _IN_TRACKED_LIFECYCLE.get():
                return await orig_get(*args, **kwargs)
            env_id = _extract_env_id(args, kwargs)
            token = _IN_TRACKED_LIFECYCLE.set(True)
            try:
                async with telemetry.track("get", env_id=env_id, source="direct"):
                    return await orig_get(*args, **kwargs)
            finally:
                _IN_TRACKED_LIFECYCLE.reset(token)

        client.get = tracked_get

    @functools.wraps(orig_suspend)
    async def tracked_suspend(*args: Any, **kwargs: Any) -> None:
        if _IN_TRACKED_LIFECYCLE.get():
            await orig_suspend(*args, **kwargs)
            return
        env_id = _extract_env_id(args, kwargs)
        token = _IN_TRACKED_LIFECYCLE.set(True)
        try:
            async with telemetry.track("suspend", env_id=env_id):
                await orig_suspend(*args, **kwargs)
        finally:
            _IN_TRACKED_LIFECYCLE.reset(token)

    @functools.wraps(orig_delete)
    async def tracked_delete(*args: Any, **kwargs: Any) -> None:
        if _IN_TRACKED_LIFECYCLE.get():
            await orig_delete(*args, **kwargs)
            return
        env_id = _extract_env_id(args, kwargs)
        token = _IN_TRACKED_LIFECYCLE.set(True)
        try:
            async with telemetry.track("delete", env_id=env_id):
                await orig_delete(*args, **kwargs)
        finally:
            _IN_TRACKED_LIFECYCLE.reset(token)

    @functools.wraps(orig_env)
    def tracked_env(*args: Any, **kwargs: Any) -> Any:
        handle = orig_env(*args, **kwargs)
        return _wrap_env_handle(handle, telemetry)

    client.create = tracked_create
    client.suspend = tracked_suspend
    client.delete = tracked_delete
    client.env = tracked_env
    setattr(client, "_bench_telemetry_wrapped", True)
    return client


class _BenchSpan:
    """In-process span consumer that captures SDK span attributes and timing."""

    def __init__(self, name: str, parent: _BenchSpan | None = None) -> None:
        self.name = name
        self.parent = parent
        self.attributes: dict[str, Any] = {}
        self.error_description: str = ""
        self.start_perf: float = time.perf_counter()

    def set_attribute(self, key: str, value: Any) -> None:
        """Record a span attribute."""
        self.attributes[key] = value

    def set_status(self, status: Any, description: str | None = None) -> None:
        """Record span status description when provided."""
        _ = status
        if description:
            self.error_description = str(description)

    def record_error(self, description: str) -> None:
        """Record an error description emitted via mark_error()."""
        if description:
            self.error_description = str(description)


_ACTIVE_BENCH_SPANS: contextvars.ContextVar[tuple[_BenchSpan, ...]] = contextvars.ContextVar(
    "_ate_env_active_bench_spans", default=()
)


class _BenchTracer:
    """Tracer implementation that consumes ate_env_client.* spans into BenchTelemetry."""

    def __init__(self, telemetry: BenchTelemetry) -> None:
        self.telemetry = telemetry
        self._lock = threading.Lock()
        self._env_meta: dict[str, dict[str, Any]] = {}

    def start_span(self, name: str, **kwargs: Any) -> _BenchSpan:
        """Create a standalone benchmark span."""
        _ = kwargs
        stack = _ACTIVE_BENCH_SPANS.get()
        return _BenchSpan(name=name, parent=stack[-1] if stack else None)

    @contextlib.contextmanager
    def start_as_current_span(self, name: str, **kwargs: Any) -> Iterator[_BenchSpan | None]:
        """Start an in-process benchmark span and record W1 telemetry on exit."""
        _ = kwargs
        if self.telemetry.mode == "off":
            yield None
            return
        stack = _ACTIVE_BENCH_SPANS.get()
        parent = stack[-1] if stack else None
        span = _BenchSpan(name=name, parent=parent)
        token = _ACTIVE_BENCH_SPANS.set((*stack, span))
        exc_caught: BaseException | None = None
        try:
            yield span
        except BaseException as exc:
            exc_caught = exc
            raise
        finally:
            dur_ms = (time.perf_counter() - span.start_perf) * 1000.0
            _ACTIVE_BENCH_SPANS.reset(token)
            self._on_span_end(span, dur_ms, exc_caught)

    def _flush_task_exec(self, env_id: str) -> None:
        """Flush accumulated per-task exec duration for env_id on release."""
        if not env_id:
            return
        with self._lock:
            meta = self._env_meta.get(env_id)
            if not meta or int(meta.get("task_exec_count") or 0) <= 0:
                return
            total_ms = float(meta.get("task_exec_ms") or 0.0)
            source = str(meta.get("source") or "direct")
            image = str(meta.get("image") or "")
            instance = str(meta.get("instance") or "")
            meta["task_exec_ms"] = 0.0
            meta["task_exec_count"] = 0
        cache_state = (
            "hit"
            if source in ("warm_pool", "snapshot")
            else ("miss" if source == "cold_create" else "unknown")
        )
        self.telemetry.record_op(
            "task_exec",
            total_ms,
            ok=True,
            env_id=env_id,
            image=image,
            instance=instance,
            source=source,
            cache_state=cache_state,
            emit_trace=False,
        )

    def _on_span_end(
        self,
        span: _BenchSpan,
        dur_ms: float,
        exc_val: BaseException | None,
    ) -> None:
        if self.telemetry.mode == "off":
            return
        attrs = span.attributes
        parent_name = span.parent.name if span.parent is not None else ""
        parent_attrs = span.parent.attributes if span.parent is not None else {}

        ok = True
        timed_out = False
        err_msg = ""
        if isinstance(exc_val, asyncio.CancelledError):
            ok = False
            timed_out = False
            err_msg = _format_error(exc_val, self.telemetry.mode) or "CancelledError"
        elif exc_val is not None:
            ok = False
            err_msg = _format_error(exc_val, self.telemetry.mode)
            if isinstance(exc_val, (TimeoutError, asyncio.TimeoutError)) or (
                type(exc_val).__name__ in ("CommandTimeoutError", "SandboxStartError")
                and "timed out" in str(exc_val).lower()
            ):
                timed_out = True
        elif span.error_description:
            ok = False
            err_msg = _format_error(span.error_description, self.telemetry.mode)
            if "timed out" in span.error_description.lower():
                timed_out = True

        prefix = f"{CLIENT_SPAN_PREFIX}."
        if not span.name.startswith(prefix):
            return
        short_name = span.name[len(prefix) :]

        if short_name == "acquire":
            env_id = str(attrs.get("ate.actor.name") or "")
            source = str(attrs.get("ate_env.acquire.source") or "cold_create")
            cache_state = "hit" if source in ("warm_pool", "snapshot") else "miss"
            image = str(
                attrs.get("ate_env.rootfs_image") or attrs.get("ate.template.name") or ""
            )
            instance = str(attrs.get("ate_env.task_id") or "")
            retries = int(attrs.get("ate_env.retries") or 0)
            slot_wait_ms = float(attrs.get("ate_env.slot_wait_ms") or 0.0)
            trace_op = "wake" if source in ("warm_pool", "snapshot") else "create"
            if ok and env_id:
                with self._lock:
                    self._env_meta[env_id] = {
                        "image": image,
                        "instance": instance,
                        "source": source,
                        "acquired_at": time.monotonic(),
                        "task_exec_ms": 0.0,
                        "task_exec_count": 0,
                    }
            self.telemetry.record_op(
                "acquire",
                dur_ms,
                ok=ok,
                env_id=env_id,
                image=image,
                instance=instance,
                source=source,
                cache_state=cache_state,
                slot_wait_ms=slot_wait_ms,
                retried=retries,
                timed_out=timed_out,
                err=err_msg,
                trace_op=trace_op,
                emit_trace=bool(env_id),
            )
            return

        if short_name == "warm_pool":
            warmed = int(attrs.get("ate_env.warmed_count") or 0)
            if warmed > 0:
                template_id = str(attrs.get("ate.template.name") or "")
                image = str(attrs.get("ate_env.rootfs_image") or template_id)
                self.telemetry.record_op(
                    "prewarm",
                    dur_ms,
                    ok=ok,
                    env_id=template_id,
                    image=image,
                    source="warm_pool",
                    cache_state="miss",
                    timed_out=timed_out,
                    err=err_msg,
                    emit_trace=False,
                )
            return

        if short_name == "unwarm_pool":
            deleted_count = int(attrs.get("ate_env.replicas") or 0)
            if deleted_count > 0:
                template_id = str(attrs.get("ate.template.name") or "")
                image = str(attrs.get("ate_env.rootfs_image") or template_id)
                self.telemetry.record_op(
                    "retire",
                    dur_ms,
                    ok=ok,
                    env_id=template_id,
                    image=image,
                    source="warm_pool",
                    cache_state="hit",
                    timed_out=timed_out,
                    err=err_msg,
                    emit_trace=False,
                )
            return

        if short_name == "reap":
            reaped_total = int(attrs.get("ate_env.reaped_count") or 0)
            warm_reaped = int(attrs.get("ate_env.warm_reaped_count") or 0)
            run_id = str(attrs.get("ate_env.run_id") or "")
            if reaped_total > 0:
                reap_source = "warm_pool" if warm_reaped > 0 else "direct"
                reap_cache = "hit" if warm_reaped > 0 else "unknown"
                self.telemetry.record_op(
                    "reap",
                    dur_ms,
                    ok=ok,
                    env_id=run_id,
                    source=reap_source,
                    cache_state=reap_cache,
                    timed_out=timed_out,
                    err=err_msg,
                    emit_trace=False,
                )
                if warm_reaped > 0:
                    self.telemetry.record_op(
                        "retire",
                        dur_ms,
                        ok=ok,
                        env_id=run_id,
                        source="warm_pool",
                        cache_state="hit",
                        timed_out=timed_out,
                        err=err_msg,
                        emit_trace=False,
                    )
            return

        if short_name == "release":
            env_id = str(attrs.get("ate.actor.name") or "")
            self._flush_task_exec(env_id)
            return

        if short_name in (
            "create_environment",
            "get_environment",
            "suspend_environment",
            "delete_environment",
        ):
            env_id = str(attrs.get("ate.actor.name") or "")
            if not env_id or env_id.startswith("preflight-probe-"):
                return
            if parent_name == f"{CLIENT_SPAN_PREFIX}.acquire":
                return
            if parent_name == f"{CLIENT_SPAN_PREFIX}.warm_pool":
                image = str(
                    attrs.get("ate_env.rootfs_image")
                    or parent_attrs.get("ate_env.rootfs_image")
                    or attrs.get("ate.template.name")
                    or parent_attrs.get("ate.template.name")
                    or ""
                )
                if short_name == "create_environment":
                    with self._lock:
                        self._env_meta[env_id] = {
                            "image": image,
                            "instance": "",
                            "source": "warm_pool",
                            "acquired_at": None,
                            "task_exec_ms": 0.0,
                            "task_exec_count": 0,
                        }
                    self.telemetry.record_op(
                        "create",
                        dur_ms,
                        ok=ok,
                        env_id=env_id,
                        image=image,
                        source="warm_pool",
                        cache_state="miss",
                        timed_out=timed_out,
                        err=err_msg,
                        trace_only=True,
                    )
                elif short_name == "suspend_environment":
                    with self._lock:
                        meta = self._env_meta.get(env_id, {})
                        eff_image = str(meta.get("image") or image)
                    self.telemetry.record_op(
                        "suspend",
                        dur_ms,
                        ok=ok,
                        env_id=env_id,
                        image=eff_image,
                        source="warm_pool",
                        cache_state="miss",
                        timed_out=timed_out,
                        err=err_msg,
                        trace_only=True,
                    )
                return
            if (
                parent_name
                in (
                    f"{CLIENT_SPAN_PREFIX}.unwarm_pool",
                    f"{CLIENT_SPAN_PREFIX}.reap",
                )
                and short_name == "delete_environment"
            ):
                with self._lock:
                    meta = self._env_meta.pop(env_id, {})
                image = str(
                    meta.get("image")
                    or parent_attrs.get("ate_env.rootfs_image")
                    or parent_attrs.get("ate.template.name")
                    or ""
                )
                source = str(meta.get("source") or "warm_pool")
                cache_state = "hit" if source in ("warm_pool", "snapshot") else "unknown"
                self.telemetry.record_op(
                    "delete",
                    dur_ms,
                    ok=ok,
                    env_id=env_id,
                    image=image,
                    instance=str(meta.get("instance") or ""),
                    source=source,
                    cache_state=cache_state,
                    timed_out=timed_out,
                    err=err_msg,
                    trace_only=True,
                )
                return
            if parent_name == f"{CLIENT_SPAN_PREFIX}.release" and short_name in (
                "suspend_environment",
                "delete_environment",
            ):
                self._flush_task_exec(env_id)
                with self._lock:
                    if short_name == "delete_environment":
                        meta = self._env_meta.pop(env_id, {})
                    else:
                        meta = dict(self._env_meta.get(env_id, {}))
                source = str(
                    meta.get("source")
                    or parent_attrs.get("ate_env.acquire.source")
                    or "direct"
                )
                cache_state = (
                    "hit"
                    if source in ("warm_pool", "snapshot")
                    else ("miss" if source == "cold_create" else "unknown")
                )
                op_name = "suspend" if short_name == "suspend_environment" else "delete"
                self.telemetry.record_op(
                    op_name,
                    dur_ms,
                    ok=ok,
                    env_id=env_id,
                    image=str(meta.get("image") or ""),
                    instance=str(meta.get("instance") or ""),
                    source=source,
                    cache_state=cache_state,
                    timed_out=timed_out,
                    err=err_msg,
                )
                return
            return

        if short_name == "exec":
            env_id = str(attrs.get("ate.actor.name") or "")
            with self._lock:
                meta = self._env_meta.setdefault(
                    env_id,
                    {
                        "image": str(attrs.get("ate_env.rootfs_image") or ""),
                        "instance": str(attrs.get("ate_env.task_id") or ""),
                        "source": "direct",
                        "acquired_at": None,
                        "task_exec_ms": 0.0,
                        "task_exec_count": 0,
                    },
                )
                source = str(meta.get("source") or "direct")
                image = str(
                    meta.get("image") or attrs.get("ate_env.rootfs_image") or ""
                )
                instance = str(
                    meta.get("instance") or attrs.get("ate_env.task_id") or ""
                )
                acquired_at = meta.get("acquired_at")
                if attrs.get("ate_env.first_command") is True:
                    meta["acquired_at"] = None
                exec_dur_ms = (
                    float(attrs.get("ate_env.duration_s") or (dur_ms / 1000.0))
                    * 1000.0
                )
                meta["task_exec_ms"] = float(meta.get("task_exec_ms") or 0.0) + max(
                    0.0, exec_dur_ms
                )
                meta["task_exec_count"] = int(meta.get("task_exec_count") or 0) + 1

            cache_state = (
                "hit"
                if source in ("warm_pool", "snapshot")
                else ("miss" if source == "cold_create" else "unknown")
            )
            timed_out_exec = bool(attrs.get("ate_env.timed_out")) or timed_out

            if attrs.get("ate_env.first_command") is True:
                first_cmd_window_ms = dur_ms
                if acquired_at is not None:
                    first_cmd_window_ms = max(
                        dur_ms, (time.monotonic() - float(acquired_at)) * 1000.0
                    )
                if source in ("warm_pool", "snapshot"):
                    self.telemetry.record_op(
                        "wake",
                        first_cmd_window_ms,
                        ok=ok,
                        env_id=env_id,
                        image=image,
                        instance=instance,
                        source=source,
                        cache_state="hit",
                        timed_out=timed_out_exec,
                        err=err_msg,
                        emit_trace=False,
                    )
                elif source == "cold_create":
                    self.telemetry.record_op(
                        "ttfi",
                        first_cmd_window_ms,
                        ok=ok,
                        env_id=env_id,
                        image=image,
                        instance=instance,
                        source="cold_create",
                        cache_state="miss",
                        timed_out=timed_out_exec,
                        err=err_msg,
                        emit_trace=False,
                    )

            step_name = str(attrs.get("ate_env.step_name") or "exec")
            exe = str(attrs.get("ate_env.command.executable") or "")
            cmd_seq: tuple[str, ...] = (exe,) if exe else ()
            exit_code = attrs.get("ate_env.exit_code")
            self.telemetry.record_op(
                "exec",
                dur_ms,
                ok=ok,
                env_id=env_id,
                image=image,
                instance=instance,
                step_name=step_name,
                command=cmd_seq,
                exit_code=int(exit_code) if exit_code is not None else None,
                err=err_msg,
                source=source,
                cache_state=cache_state,
                timed_out=timed_out_exec,
            )
            return

        if short_name in ("write_file", "read_file"):
            if parent_name == span.name:
                return
            env_id = str(attrs.get("ate.actor.name") or "")
            with self._lock:
                meta = dict(self._env_meta.get(env_id, {}))
            source = str(meta.get("source") or "direct")
            cache_state = (
                "hit"
                if source in ("warm_pool", "snapshot")
                else ("miss" if source == "cold_create" else "unknown")
            )
            image = str(meta.get("image") or attrs.get("ate_env.rootfs_image") or "")
            instance = str(meta.get("instance") or attrs.get("ate_env.task_id") or "")
            self.telemetry.record_op(
                short_name,
                dur_ms,
                ok=ok,
                env_id=env_id,
                image=image,
                instance=instance,
                path=str(attrs.get("ate_env.file.path") or ""),
                bytes_count=int(attrs.get("ate_env.file.size") or 0),
                err=err_msg,
                source=source,
                cache_state=cache_state,
                timed_out=timed_out,
            )


def instrument_fleet(
    fleet: Any,
    telemetry: BenchTelemetry | None = None,
) -> Any:
    """Attach BenchTelemetry instrumentation to a SandboxFleet or AsyncSandboxFleet.

    Consumes the SDK's generic ``ate_env_client.*`` spans from the outside via
    ``CompositeTracer`` without requiring any core SDK module to import
    ``ate_env.telemetry.bench``. Sets ``fleet.telemetry`` and returns ``fleet``.
    """
    sync_fleet = getattr(fleet, "_fleet", fleet)
    run_id = getattr(sync_fleet, "run_id", None)
    resolved = telemetry if telemetry is not None else BenchTelemetry(run_id=run_id)
    setattr(fleet, "telemetry", resolved)
    if sync_fleet is not fleet:
        setattr(sync_fleet, "telemetry", resolved)

    if not getattr(fleet, "_bench_close_wrapped", False):
        orig_close = fleet.close

        @functools.wraps(orig_close)
        def tracked_close(*args: Any, **kwargs: Any) -> Any:
            try:
                return orig_close(*args, **kwargs)
            finally:
                with contextlib.suppress(Exception):
                    resolved.close()

        fleet.close = tracked_close
        setattr(fleet, "_bench_close_wrapped", True)

    if resolved.mode == "off" or getattr(sync_fleet, "_bench_telemetry_wrapped", False):
        return fleet

    bench_tracer = _BenchTracer(resolved)
    setattr(
        sync_fleet,
        "_tracer",
        combine_tracers(getattr(sync_fleet, "_tracer", None), bench_tracer),
    )
    backend = getattr(sync_fleet, "backend", None)
    if backend is not None:
        backend.tracer = combine_tracers(
            getattr(backend, "tracer", None), bench_tracer
        )
        raw_client = getattr(backend, "_client", None)
        if raw_client is not None:
            raw_client.tracer = combine_tracers(
                getattr(raw_client, "tracer", None), bench_tracer
            )
    setattr(sync_fleet, "_bench_telemetry_wrapped", True)
    return fleet
