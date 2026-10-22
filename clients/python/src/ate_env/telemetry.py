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

Supports two active modes configured explicitly or via environment variables:
- ``measure``: In-memory Prometheus counters and histograms with zero extra
  RPCs and zero per-call disk writes on the hot path (Guardrail 4), plus
  optional coarse step-boundary flush to ``ENV_BENCH_EVENTS_PATH``.
- ``trace``: Per-call JSONL trace emission matching the W1 ``TraceEvent``
  schema (Guardrail 1) for offline conversion by W12.
"""

# pylint: disable=too-many-arguments,too-many-positional-arguments,too-many-locals,too-many-instance-attributes

from __future__ import annotations

import contextlib
import dataclasses
import functools
import json
import math
import os
import time
from collections.abc import AsyncIterator, Callable, Mapping, Sequence
from datetime import datetime, timezone
from pathlib import Path
from typing import Any

__all__ = [
    "SCHEMA_VERSION",
    "TRACE_EVENT_ALLOWED_KEYS",
    "EVENT_ALLOWED_KEYS",
    "TraceEventRecord",
    "BenchTelemetry",
    "validate_trace_event_dict",
    "validate_event_dict",
    "instrument_client",
]

SCHEMA_VERSION = "v1"

# Exact W1 schema field sets (Guardrail 1: no unknown top-level fields).
TRACE_EVENT_ALLOWED_KEYS: frozenset[str] = frozenset(
    {
        "schema_version",
        "ts",
        "job",
        "env",
        "step",
        "op",
        "command",
        "path",
        "bytes",
        "dur_ms",
        "exit",
        "ok",
        "err",
    }
)

EVENT_ALLOWED_KEYS: frozenset[str] = frozenset(
    {
        "ts",
        "run",
        "event",
        "name",
        "env",
        "instance",
        "task",
        "rollout",
        "step",
        "dur_ms",
        "ok",
        "exit",
        "reward",
        "attempts",
        "err",
        "tail",
        "job",
        "snapshot",
        "node",
        "bytes",
        "cache_hit",
    }
)

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


def validate_trace_event_dict(payload: Mapping[str, Any]) -> None:
    """Validate a dict against the W1 TraceEvent JSON schema (Guardrail 1)."""
    unknown = set(payload.keys()) - TRACE_EVENT_ALLOWED_KEYS
    if unknown:
        raise ValueError(f"trace event contains unknown W1 fields: {sorted(unknown)}")
    if payload.get("schema_version") != SCHEMA_VERSION:
        raise ValueError(
            f"trace event schema_version must be {SCHEMA_VERSION!r}, "
            f"got {payload.get('schema_version')!r}"
        )
    if not payload.get("op"):
        raise ValueError("trace event op is required")
    if not payload.get("ts"):
        raise ValueError("trace event ts is required")
    dur_ms = payload.get("dur_ms", 0.0)
    if not isinstance(dur_ms, (int, float)) or dur_ms < 0:
        raise ValueError(f"trace event dur_ms must be >= 0, got {dur_ms!r}")


def validate_event_dict(payload: Mapping[str, Any]) -> None:
    """Validate a dict against the W1 Event JSON schema (Guardrail 1)."""
    unknown = set(payload.keys()) - EVENT_ALLOWED_KEYS
    if unknown:
        raise ValueError(f"event contains unknown W1 fields: {sorted(unknown)}")
    if not payload.get("run"):
        raise ValueError("event run is required")
    if not payload.get("event"):
        raise ValueError("event kind is required")
    dur_ms = payload.get("dur_ms", 0.0)
    if not isinstance(dur_ms, (int, float)) or dur_ms < 0:
        raise ValueError(f"event dur_ms must be >= 0, got {dur_ms!r}")


@dataclasses.dataclass(frozen=True)
class TraceEventRecord:
    """A single W1-compliant SDK trace event record."""

    op: str
    dur_ms: float
    ok: bool = True
    ts: str = ""
    job: str = ""
    env: str = ""
    step: int = 0
    command: Sequence[str] = ()
    path: str = ""
    bytes_count: int = 0
    exit_code: int | None = None
    err: str = ""

    def to_dict(self) -> dict[str, Any]:
        """Serialize to a W1-validated dictionary."""
        timestamp = self.ts or datetime.now(timezone.utc).isoformat().replace("+00:00", "Z")
        out: dict[str, Any] = {
            "schema_version": SCHEMA_VERSION,
            "ts": timestamp,
            "op": self.op,
            "dur_ms": round(float(self.dur_ms), 4),
            "ok": bool(self.ok),
        }
        if self.job:
            out["job"] = self.job
        if self.env:
            out["env"] = self.env
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
    """In-memory histogram accumulator for a single label set."""

    buckets: tuple[float, ...] = _DEFAULT_BUCKETS_MS
    counts: list[int] = dataclasses.field(default_factory=list)
    total_count: int = 0
    total_sum: float = 0.0
    samples: list[float] = dataclasses.field(default_factory=list)

    def __post_init__(self) -> None:
        if not self.counts:
            self.counts = [0] * len(self.buckets)

    def observe(self, value_ms: float) -> None:
        """Record one latency observation in milliseconds."""
        self.total_count += 1
        self.total_sum += value_ms
        self.samples.append(value_ms)
        for idx, bound in enumerate(self.buckets):
            if value_ms <= bound:
                self.counts[idx] += 1


def _percentile(sorted_values: Sequence[float], q: float) -> float:
    """Compute the nearest-rank percentile of a sorted sequence."""
    n = len(sorted_values)
    if n == 0:
        return 0.0
    if q >= 1.0:
        return float(sorted_values[-1])
    idx = max(0, min(n - 1, int(math.ceil(q * n)) - 1))
    return float(sorted_values[idx])


class BenchTelemetry:
    """Low-overhead SDK telemetry collector and W1 trace emitter (W11).

    In ``measure`` mode, all observations stay in memory (zero per-call I/O or
    extra RPCs) until ``render_prometheus()`` or ``flush_step_events()`` is
    called at a coarse step boundary.
    In ``trace`` mode, each completed operation appends one W1-validated JSONL
    line to ``trace_path``.
    """

    def __init__(
        self,
        mode: str | None = None,
        *,
        run_id: str | None = None,
        job: str | None = None,
        trace_path: str | Path | None = None,
        events_path: str | Path | None = None,
    ) -> None:
        resolved_mode = (mode or os.environ.get("ENV_BENCH_MODE") or "measure").strip().lower()
        if resolved_mode not in ("off", "measure", "trace"):
            raise ValueError(
                f"invalid telemetry mode {resolved_mode!r}; expected 'off', 'measure', or 'trace'"
            )
        self.mode: str = resolved_mode
        self.run_id: str = run_id or os.environ.get("ENV_BENCH_RUN_ID") or "sdk-bench"
        self.job: str = job or os.environ.get("ENV_BENCH_JOB") or "default"
        raw_trace = trace_path or os.environ.get("ENV_BENCH_TRACE_PATH")
        self.trace_path: Path | None = Path(raw_trace) if raw_trace else None
        raw_events = events_path or os.environ.get("ENV_BENCH_EVENTS_PATH")
        self.events_path: Path | None = Path(raw_events) if raw_events else None
        self.current_step: int = 0

        # In-memory Prometheus state keyed by (op, source, cache_state, ok_str).
        self._histograms: dict[tuple[str, str, str, str], _HistogramState] = {}
        self._slot_wait_histograms: dict[str, _HistogramState] = {}
        self._counters: dict[tuple[str, str], int] = {}
        self._pending_step_ops: list[TraceEventRecord] = []

    def set_step(self, step: int) -> None:
        """Update the current RL training step index for subsequent operations."""
        if step < 0:
            raise ValueError(f"step must be >= 0, got {step}")
        self.current_step = step

    def record_op(
        self,
        op: str,
        dur_ms: float,
        *,
        ok: bool = True,
        env_id: str = "",
        step: int | None = None,
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
    ) -> None:
        """Record a completed SDK operation according to the active mode."""
        if self.mode == "off":
            return

        effective_step = self.current_step if step is None else step
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
            self._counters[(op, "retry")] = self._counters.get((op, "retry"), 0) + retried
        if timed_out:
            self._counters[(op, "timeout")] = self._counters.get((op, "timeout"), 0) + 1

        record = TraceEventRecord(
            op=op,
            dur_ms=dur_ms,
            ok=ok,
            job=self.job,
            env=env_id,
            step=effective_step,
            command=command,
            path=path,
            bytes_count=bytes_count,
            exit_code=exit_code,
            err=err,
        )
        self._pending_step_ops.append(record)

        # Guardrail 4: Per-call JSONL file logging is strictly restricted to trace mode.
        if self.mode == "trace" and self.trace_path is not None:
            payload = record.to_dict()
            self.trace_path.parent.mkdir(parents=True, exist_ok=True)
            with self.trace_path.open("a", encoding="utf-8") as fh:
                fh.write(json.dumps(payload, separators=(",", ":")) + "\n")

    @contextlib.asynccontextmanager
    async def track(
        self,
        op: str,
        *,
        env_id: str = "",
        step: int | None = None,
        command: Sequence[str] = (),
        path: str = "",
        bytes_count: int = 0,
        source: str = "direct",
        cache_state: str = "unknown",
    ) -> AsyncIterator[dict[str, Any]]:
        """Async context manager that times an SDK call and records telemetry."""
        ctx_meta: dict[str, Any] = {
            "bytes": bytes_count,
            "exit_code": None,
            "source": source,
            "cache_state": cache_state,
            "slot_wait_ms": 0.0,
            "retried": 0,
            "timed_out": False,
        }
        start = time.perf_counter()
        ok = True
        err_msg = ""
        try:
            yield ctx_meta
        except BaseException as exc:
            ok = False
            err_msg = str(exc)
            raise
        finally:
            dur_ms = (time.perf_counter() - start) * 1000.0
            self.record_op(
                op,
                dur_ms,
                ok=ok,
                env_id=env_id,
                step=step,
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
            )

    def render_prometheus(self) -> str:
        """Render in-memory histograms and counters in Prometheus exposition format."""
        lines: list[str] = [
            "# HELP ate_env_sdk_op_duration_ms SDK operation latency in milliseconds.",
            "# TYPE ate_env_sdk_op_duration_ms histogram",
        ]
        for (op, source, cache_state, ok_str), hist in sorted(self._histograms.items()):
            base_labels = (
                f'job="{self.job}",op="{op}",source="{source}",'
                f'cache_state="{cache_state}",ok="{ok_str}"'
            )
            for bound, count in zip(hist.buckets, hist.counts):
                lines.append(
                    f'ate_env_sdk_op_duration_ms_bucket{{{base_labels},le="{bound}"}} {count}'
                )
            lines.append(
                f'ate_env_sdk_op_duration_ms_bucket{{{base_labels},le="+Inf"}} {hist.total_count}'
            )
            lines.append(f"ate_env_sdk_op_duration_ms_sum{{{base_labels}}} {hist.total_sum:.4f}")
            lines.append(f"ate_env_sdk_op_duration_ms_count{{{base_labels}}} {hist.total_count}")

        if self._slot_wait_histograms:
            lines.append(
                "# HELP ate_env_sdk_slot_wait_ms Client-side slot wait latency in milliseconds."
            )
            lines.append("# TYPE ate_env_sdk_slot_wait_ms histogram")
            for op, hist in sorted(self._slot_wait_histograms.items()):
                base_labels = f'job="{self.job}",op="{op}"'
                for bound, count in zip(hist.buckets, hist.counts):
                    lines.append(
                        f'ate_env_sdk_slot_wait_ms_bucket{{{base_labels},le="{bound}"}} {count}'
                    )
                lines.append(
                    f'ate_env_sdk_slot_wait_ms_bucket{{{base_labels},le="+Inf"}} {hist.total_count}'
                )
                lines.append(f"ate_env_sdk_slot_wait_ms_sum{{{base_labels}}} {hist.total_sum:.4f}")
                lines.append(f"ate_env_sdk_slot_wait_ms_count{{{base_labels}}} {hist.total_count}")

        lines.append("# HELP ate_env_sdk_events_total SDK error, retry, and timeout counters.")
        lines.append("# TYPE ate_env_sdk_events_total counter")
        for (op, kind), count in sorted(self._counters.items()):
            lines.append(
                f'ate_env_sdk_events_total{{job="{self.job}",op="{op}",kind="{kind}"}} {count}'
            )

        return "\n".join(lines) + "\n"

    def flush_step_events(
        self,
        step: int | None = None,
        *,
        path: str | Path | None = None,
    ) -> list[dict[str, Any]]:
        """Flush buffered operations as W1 Event JSONL lines at a step boundary."""
        target_path = Path(path) if path is not None else self.events_path
        effective_step = self.current_step if step is None else step
        now_iso = datetime.now(timezone.utc).isoformat().replace("+00:00", "Z")

        emitted: list[dict[str, Any]] = []
        op_to_event = {
            "create": "create",
            "suspend": "suspend",
            "delete": "delete",
            "shell": "step",
            "start_process": "step",
            "read_file": "step",
            "write_file": "step",
        }
        for rec in self._pending_step_ops:
            ev_kind = op_to_event.get(rec.op, "step")
            ev: dict[str, Any] = {
                "ts": rec.ts or now_iso,
                "run": self.run_id,
                "event": ev_kind,
                "dur_ms": round(float(rec.dur_ms), 4),
                "ok": rec.ok,
            }
            if ev_kind == "step":
                ev["name"] = rec.op
            if rec.job:
                ev["job"] = rec.job
            if rec.env:
                ev["env"] = rec.env
            st = rec.step if rec.step > 0 else effective_step
            if st > 0:
                ev["step"] = st
            if rec.bytes_count > 0:
                ev["bytes"] = rec.bytes_count
            if rec.exit_code is not None:
                ev["exit"] = rec.exit_code
            if rec.err:
                ev["err"] = rec.err
            validate_event_dict(ev)
            emitted.append(ev)

        self._pending_step_ops.clear()

        if target_path is not None and emitted:
            target_path.parent.mkdir(parents=True, exist_ok=True)
            with target_path.open("a", encoding="utf-8") as fh:
                for ev in emitted:
                    fh.write(json.dumps(ev, separators=(",", ":")) + "\n")

        return emitted

    def summary_stats(self) -> dict[str, dict[str, float]]:
        """Return p50, p95, max, and count per operation for quick inspection."""
        out: dict[str, dict[str, float]] = {}
        by_op: dict[str, list[float]] = {}
        for (op, _, _, _), hist in self._histograms.items():
            by_op.setdefault(op, []).extend(hist.samples)
        for op, samples in by_op.items():
            ordered = sorted(samples)
            out[op] = {
                "count": float(len(ordered)),
                "p50_ms": _percentile(ordered, 0.50),
                "p95_ms": _percentile(ordered, 0.95),
                "max_ms": _percentile(ordered, 1.00),
            }
        return out


def _wrap_env_handle(env_handle: Any, telemetry: BenchTelemetry) -> Any:
    """Wrap an Env instance's methods so calls record into telemetry."""
    if getattr(env_handle, "_bench_telemetry_wrapped", False):
        return env_handle

    orig_shell = env_handle.shell
    orig_start_process = env_handle.start_process
    orig_write_file = env_handle.write_file
    orig_read_file_bytes = env_handle.read_file_bytes
    orig_suspend = env_handle.suspend
    orig_delete = env_handle.delete

    @functools.wraps(orig_shell)
    async def tracked_shell(command_line: str, *args: Any, **kwargs: Any) -> Any:
        async with telemetry.track(
            "shell",
            env_id=env_handle.id,
            command=("sh", "-c", command_line),
        ) as meta:
            res = await orig_shell(command_line, *args, **kwargs)
            meta["exit_code"] = res.exit_code
            return res

    @functools.wraps(orig_start_process)
    async def tracked_start_process(command: Sequence[str], *args: Any, **kwargs: Any) -> Any:
        async with telemetry.track(
            "start_process",
            env_id=env_handle.id,
            command=tuple(command),
        ):
            return await orig_start_process(command, *args, **kwargs)

    @functools.wraps(orig_write_file)
    async def tracked_write_file(path: str, data: Any, *args: Any, **kwargs: Any) -> int:
        async with telemetry.track("write_file", env_id=env_handle.id, path=path) as meta:
            written = await orig_write_file(path, data, *args, **kwargs)
            meta["bytes"] = int(written)
            return written

    @functools.wraps(orig_read_file_bytes)
    async def tracked_read_file_bytes(path: str, *args: Any, **kwargs: Any) -> bytes:
        async with telemetry.track("read_file", env_id=env_handle.id, path=path) as meta:
            content = await orig_read_file_bytes(path, *args, **kwargs)
            meta["bytes"] = len(content)
            return content

    @functools.wraps(orig_suspend)
    async def tracked_suspend(*args: Any, **kwargs: Any) -> None:
        async with telemetry.track("suspend", env_id=env_handle.id):
            await orig_suspend(*args, **kwargs)

    @functools.wraps(orig_delete)
    async def tracked_delete(*args: Any, **kwargs: Any) -> None:
        async with telemetry.track("delete", env_id=env_handle.id):
            await orig_delete(*args, **kwargs)

    env_handle.shell = tracked_shell
    env_handle.start_process = tracked_start_process
    env_handle.write_file = tracked_write_file
    env_handle.read_file_bytes = tracked_read_file_bytes
    env_handle.suspend = tracked_suspend
    env_handle.delete = tracked_delete
    setattr(env_handle, "_bench_telemetry_wrapped", True)
    return env_handle


def instrument_client(client: Any, telemetry: BenchTelemetry) -> Any:
    """Attach BenchTelemetry instrumentation to an ate_env.Client in place."""
    if getattr(client, "_bench_telemetry_wrapped", False):
        return client

    orig_create: Callable[..., Any] = client.create
    orig_suspend: Callable[..., Any] = client.suspend
    orig_delete: Callable[..., Any] = client.delete
    orig_env: Callable[..., Any] = client.env

    @functools.wraps(orig_create)
    async def tracked_create(env_id: str, *args: Any, **kwargs: Any) -> Any:
        async with telemetry.track("create", env_id=env_id):
            handle = await orig_create(env_id, *args, **kwargs)
        return _wrap_env_handle(handle, telemetry)

    @functools.wraps(orig_suspend)
    async def tracked_suspend(env_id: str, *args: Any, **kwargs: Any) -> None:
        async with telemetry.track("suspend", env_id=env_id):
            await orig_suspend(env_id, *args, **kwargs)

    @functools.wraps(orig_delete)
    async def tracked_delete(env_id: str, *args: Any, **kwargs: Any) -> None:
        async with telemetry.track("delete", env_id=env_id):
            await orig_delete(env_id, *args, **kwargs)

    @functools.wraps(orig_env)
    def tracked_env(env_id: str, *args: Any, **kwargs: Any) -> Any:
        handle = orig_env(env_id, *args, **kwargs)
        return _wrap_env_handle(handle, telemetry)

    client.create = tracked_create
    client.suspend = tracked_suspend
    client.delete = tracked_delete
    client.env = tracked_env
    setattr(client, "_bench_telemetry_wrapped", True)
    return client
