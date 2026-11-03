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

"""OpenTelemetry SpanProcessor and JSONL recorder for v1 TraceEvent streams.

When ``ENV_BENCH_MODE=trace`` (or when ``TraceFileSpanProcessor`` is attached
with a trace path), ``ate_env_client.*`` spans are serialized into
``v1/trace_event.schema.json``-compliant JSONL lines at ``ENV_BENCH_TRACE_PATH``
(or ``$ARTIFACT_DIR/trace.jsonl``).

When ``ENV_BENCH_MODE=measure``, zero per-call JSONL lines are written.
"""

from __future__ import annotations

# pylint: disable=redefined-builtin,global-statement
import collections
import contextlib
import contextvars
import json
import logging
import os
import threading
import time
from collections.abc import Iterator, Sequence
from datetime import datetime, timezone
from pathlib import Path
from typing import Any

logger = logging.getLogger("ate_env.telemetry.trace_recorder")

SCHEMA_VERSION = "v1"

CANONICAL_TRACE_OPS: tuple[str, ...] = (
    "create",
    "get",
    "suspend",
    "delete",
    "wake",
    "exec",
    "shell",
    "start_process",
    "write_file",
    "read_file",
)

_CANONICAL_OPS_SET = frozenset(CANONICAL_TRACE_OPS)

_SPAN_NAME_TO_OP: dict[str, str] = {
    "ate_env_client.create_environment": "create",
    "ate_env_client.get_environment": "get",
    "ate_env_client.suspend_environment": "suspend",
    "ate_env_client.delete_environment": "delete",
    "ate_env_client.exec": "exec",
    "ate_env_client.shell": "shell",
    "ate_env_client.start_process": "start_process",
    "ate_env_client.write_file": "write_file",
    "ate_env_client.read_file": "read_file",
    "ate_env_client.wake": "wake",
}

# Context variables to suppress duplicate nested spans (e.g. Env.shell inside
# SandboxHandle.exec, or Env.start_process inside Env.shell) and propagate
# task/step context across async/threaded boundaries.
in_handle_span: contextvars.ContextVar[bool] = contextvars.ContextVar(
    "ate_env_in_handle_span", default=False
)
in_env_span: contextvars.ContextVar[bool] = contextvars.ContextVar(
    "ate_env_in_env_span", default=False
)
in_driver_span: contextvars.ContextVar[bool] = contextvars.ContextVar(
    "ate_env_in_driver_span", default=False
)
current_warm_claim: contextvars.ContextVar[bool] = contextvars.ContextVar(
    "ate_env_current_warm_claim", default=False
)
current_slot_wait_s: contextvars.ContextVar[float | None] = contextvars.ContextVar(
    "ate_env_current_slot_wait_s", default=None
)
current_task_context: contextvars.ContextVar[dict[str, Any] | None] = (
    contextvars.ContextVar("ate_env_current_task_context", default=None)
)

_WRITE_LOCK = threading.Lock()
_META_LOCK = threading.Lock()
_SPAN_ANNOTATIONS: dict[int, dict[str, Any]] = {}
_ENV_META: dict[str, dict[str, Any]] = {}
_PROCESS_CONTEXT: dict[str, Any] = {}
_DEFAULT_PROCESSORS: dict[str, TraceFileSpanProcessor] = {}


def bench_mode() -> str:
    """Return the normalized ENV_BENCH_MODE ('trace', 'measure', or 'off')."""
    raw = os.environ.get("ENV_BENCH_MODE", "").strip().lower()
    if raw in ("trace", "measure"):
        return raw
    return "off"


def is_trace_mode() -> bool:
    """Return True when ENV_BENCH_MODE is 'trace'."""
    return bench_mode() == "trace"


def is_measure_mode() -> bool:
    """Return True when ENV_BENCH_MODE is 'measure'."""
    return bench_mode() == "measure"


def resolve_trace_path(explicit_path: str | Path | None = None) -> Path | None:
    """Resolve the target trace.jsonl path from explicit argument or env vars."""
    if explicit_path is not None:
        raw = str(explicit_path).strip()
        return Path(raw) if raw else None
    env_path = os.environ.get("ENV_BENCH_TRACE_PATH", "").strip()
    if env_path:
        return Path(env_path)
    artifact_dir = os.environ.get("ARTIFACT_DIR", "").strip()
    if artifact_dir:
        return Path(artifact_dir) / "trace.jsonl"
    return None


def should_auto_enable_tracing(cfg: Any = None) -> bool:
    """Return True when trace recording should auto-activate on a client/fleet."""
    mode = bench_mode()
    if mode == "measure":
        return False
    if mode == "trace":
        return True
    if cfg is not None and bool(getattr(cfg, "enable_tracing", False)):
        return resolve_trace_path() is not None
    return False


def set_bench_context(
    *,
    run_id: str | None = None,
    job: str | None = None,
    step: int | None = None,
    instance: str | None = None,
    image: str | None = None,
) -> None:
    """Set process-level benchmark metadata defaults for emitted trace records."""
    with _META_LOCK:
        if run_id is not None:
            _PROCESS_CONTEXT["run"] = run_id
        if job is not None:
            _PROCESS_CONTEXT["job"] = job
        if step is not None:
            _PROCESS_CONTEXT["step"] = step
        if instance is not None:
            _PROCESS_CONTEXT["instance"] = instance
        if image is not None:
            _PROCESS_CONTEXT["image"] = image


def clear_bench_context() -> None:
    """Clear process-level benchmark metadata and cached environment metadata."""
    with _META_LOCK:
        _PROCESS_CONTEXT.clear()
        _ENV_META.clear()
        _SPAN_ANNOTATIONS.clear()


def get_bench_context() -> dict[str, Any]:
    """Return merged benchmark metadata from env vars, process context, and task contextvar."""
    out: dict[str, Any] = {}
    env_run = os.environ.get("ENV_BENCH_RUN_ID", "").strip()
    env_job = os.environ.get("ENV_BENCH_JOB", "").strip()
    env_step = os.environ.get("ENV_BENCH_STEP", "").strip()
    env_inst = os.environ.get("ENV_BENCH_INSTANCE", "").strip()
    env_img = os.environ.get("ENV_BENCH_IMAGE", "").strip()
    if env_run:
        out["run"] = env_run
    if env_job:
        out["job"] = env_job
    if env_step:
        with contextlib.suppress(ValueError):
            out["step"] = int(env_step)
    if env_inst:
        out["instance"] = env_inst
    if env_img:
        out["image"] = env_img
    with _META_LOCK:
        out.update({k: v for k, v in _PROCESS_CONTEXT.items() if v is not None})
    task_ctx = current_task_context.get()
    if task_ctx:
        out.update({k: v for k, v in task_ctx.items() if v is not None})
    return out


@contextlib.contextmanager
def task_context(
    *,
    run_id: str | None = None,
    job: str | None = None,
    step: int | None = None,
    instance: str | None = None,
    image: str | None = None,
) -> Iterator[None]:
    """Bind task-local metadata for trace events emitted inside the block."""
    prev = current_task_context.get() or {}
    merged = dict(prev)
    if run_id is not None:
        merged["run"] = run_id
    if job is not None:
        merged["job"] = job
    if step is not None:
        merged["step"] = step
    if instance is not None:
        merged["instance"] = instance
    if image is not None:
        merged["image"] = image
    token = current_task_context.set(merged)
    try:
        yield
    finally:
        current_task_context.reset(token)


def register_env_meta(
    env_id: str,
    *,
    instance: str | None = None,
    image: str | None = None,
    run_id: str | None = None,
    job: str | None = None,
) -> None:
    """Associate instance/image/run metadata with an environment ID for trace attribution."""
    if not env_id:
        return
    with _META_LOCK:
        entry = _ENV_META.setdefault(env_id, {})
        if instance:
            entry["instance"] = instance
        if image:
            entry["image"] = image
        if run_id:
            entry["run"] = run_id
        if job:
            entry["job"] = job
        if len(_ENV_META) > 16384:
            oldest = next(iter(_ENV_META))
            _ENV_META.pop(oldest, None)


def get_env_meta(env_id: str) -> dict[str, Any]:
    """Return registered metadata for env_id, if any."""
    if not env_id:
        return {}
    with _META_LOCK:
        return dict(_ENV_META.get(env_id, {}))


def _span_key(span: Any) -> int | None:
    """Return a stable lookup key for a span object or its underlying SpanContext."""
    if span is None:
        return None
    if hasattr(span, "span_key"):
        return int(span.span_key)
    with contextlib.suppress(Exception):
        ctx = span.get_span_context() if hasattr(span, "get_span_context") else getattr(span, "context", None)
        span_id = getattr(ctx, "span_id", 0)
        if span_id:
            return int(span_id)
    return id(span)


def annotate_span(span: Any, **fields: Any) -> None:
    """Attach trace-only metadata to a span without polluting OTel span attributes."""
    if span is None or is_measure_mode():
        return
    if hasattr(span, "annotate_trace"):
        span.annotate_trace(**fields)
        return
    key = _span_key(span)
    if key is None:
        return
    with _META_LOCK:
        entry = _SPAN_ANNOTATIONS.setdefault(key, {})
        for k, v in fields.items():
            if v is not None:
                entry[k] = v
        if len(_SPAN_ANNOTATIONS) > 16384:
            oldest = next(iter(_SPAN_ANNOTATIONS))
            _SPAN_ANNOTATIONS.pop(oldest, None)


def pop_span_annotation(span: Any) -> dict[str, Any]:
    """Retrieve and remove trace-only annotations for a finished span."""
    out: dict[str, Any] = {}
    if hasattr(span, "trace_annotations"):
        out.update(span.trace_annotations)
    key = _span_key(span)
    if key is not None:
        with _META_LOCK:
            stored = _SPAN_ANNOTATIONS.pop(key, None)
            if stored:
                out.update(stored)
    return out


def normalize_command_list(
    command: Sequence[str] | str | None, *, is_shell: bool = False
) -> list[str] | None:
    """Normalize a command string or sequence into a list of strings for TraceEvent.command."""
    if command is None:
        return None
    if isinstance(command, str):
        if not command:
            return None
        if is_shell:
            return ["sh", "-c", command]
        return ["sh", "-c", command]
    items = [str(part) for part in command]
    return items if items else None


def _utc_rfc3339(ts_ns: int | None = None) -> str:
    """Format a UTC timestamp as RFC3339 with Z suffix."""
    if ts_ns is not None and ts_ns > 0:
        dt = datetime.fromtimestamp(ts_ns / 1e9, tz=timezone.utc)
    else:
        dt = datetime.now(timezone.utc)
    return dt.isoformat().replace("+00:00", "Z")


def build_trace_event(
    op: str,
    *,
    env: str | None = None,
    env_id: str | None = None,
    dur_ms: float | None = None,
    duration_ms: float | None = None,
    ok: bool = True,
    ts: str | None = None,
    run: str | None = None,
    run_id: str | None = None,
    job: str | None = None,
    step: int | None = None,
    instance: str | None = None,
    image: str | None = None,
    command: Sequence[str] | str | None = None,
    cmd: Sequence[str] | str | None = None,
    path: str | None = None,
    bytes: int | None = None,  # noqa: A002
    exit: int | None = None,  # noqa: A002
    exit_code: int | None = None,
    err: str | None = None,
) -> dict[str, Any]:
    """Construct and validate a v1 TraceEvent dictionary matching trace_event.schema.json."""
    if op not in _CANONICAL_OPS_SET:
        raise ValueError(f"trace_event.op must be one of {CANONICAL_TRACE_OPS}, got {op!r}")

    resolved_env = (env if env is not None else env_id) or ""
    resolved_env = str(resolved_env).strip()
    if not resolved_env:
        raise ValueError("trace_event.env is required and must be non-empty")

    raw_dur = dur_ms if dur_ms is not None else (duration_ms if duration_ms is not None else 0.0)
    resolved_dur = round(max(0.0, float(raw_dur)), 3)

    ctx = get_bench_context()
    env_meta = get_env_meta(resolved_env)

    resolved_run = run if run is not None else run_id
    if not resolved_run:
        resolved_run = env_meta.get("run") or ctx.get("run")

    resolved_job = job if job is not None else (env_meta.get("job") or ctx.get("job"))
    resolved_step = step if step is not None else ctx.get("step")
    resolved_instance = instance if instance is not None else (
        env_meta.get("instance") or ctx.get("instance")
    )
    resolved_image = image if image is not None else (
        env_meta.get("image") or ctx.get("image")
    )
    raw_cmd = command if command is not None else cmd
    resolved_cmd = normalize_command_list(raw_cmd, is_shell=op in ("shell", "exec"))
    resolved_exit = exit if exit is not None else exit_code

    record: dict[str, Any] = {
        "schema_version": SCHEMA_VERSION,
        "ts": ts or _utc_rfc3339(),
        "env": resolved_env,
        "op": op,
        "dur_ms": resolved_dur,
        "ok": bool(ok),
    }
    if resolved_run:
        record["run"] = str(resolved_run)
    if resolved_job:
        record["job"] = str(resolved_job)
    if resolved_instance:
        record["instance"] = str(resolved_instance)
    if resolved_image:
        record["image"] = str(resolved_image)
    if resolved_step is not None and int(resolved_step) >= 0:
        record["step"] = int(resolved_step)
    if resolved_cmd:
        record["command"] = resolved_cmd
    if path:
        record["path"] = str(path)
    if bytes is not None and int(bytes) >= 0:
        record["bytes"] = int(bytes)
    if resolved_exit is not None:
        record["exit"] = int(resolved_exit)
    if err:
        record["err"] = str(err)
    return record


def record_trace_event(
    op: str,
    *,
    env: str | None = None,
    env_id: str | None = None,
    dur_ms: float | None = None,
    duration_ms: float | None = None,
    ok: bool = True,
    ts: str | None = None,
    run: str | None = None,
    run_id: str | None = None,
    job: str | None = None,
    step: int | None = None,
    instance: str | None = None,
    image: str | None = None,
    command: Sequence[str] | str | None = None,
    cmd: Sequence[str] | str | None = None,
    path: str | None = None,
    bytes: int | None = None,  # noqa: A002
    exit: int | None = None,  # noqa: A002
    exit_code: int | None = None,
    err: str | None = None,
    trace_path: str | Path | None = None,
    force: bool = False,
) -> dict[str, Any] | None:
    """Write a single v1 TraceEvent JSONL record to trace_path when trace mode is active."""
    if is_measure_mode() and not force:
        return None
    if not force and not is_trace_mode() and trace_path is None:
        return None
    target = resolve_trace_path(trace_path)
    if target is None:
        return None

    record = build_trace_event(
        op,
        env=env,
        env_id=env_id,
        dur_ms=dur_ms,
        duration_ms=duration_ms,
        ok=ok,
        ts=ts,
        run=run,
        run_id=run_id,
        job=job,
        step=step,
        instance=instance,
        image=image,
        command=command,
        cmd=cmd,
        path=path,
        bytes=bytes,
        exit=exit,
        exit_code=exit_code,
        err=err,
    )
    line = json.dumps(record, separators=(",", ":")) + "\n"
    with _WRITE_LOCK:
        target.parent.mkdir(parents=True, exist_ok=True)
        with target.open("a", encoding="utf-8") as f:
            f.write(line)
    return record


def _extract_span_error(span: Any, ann: dict[str, Any]) -> tuple[bool, str | None]:
    """Determine (ok, err) from span annotations and OpenTelemetry Status."""
    if "ok" in ann:
        ok = bool(ann["ok"])
        err = ann.get("err")
        return ok, str(err) if err else None

    status = getattr(span, "status", None)
    if status is not None:
        status_code = getattr(status, "status_code", None)
        code_name = getattr(status_code, "name", str(status_code or ""))
        if code_name == "ERROR" or getattr(status, "is_ok", True) is False:
            desc = getattr(status, "description", None) or ann.get("err") or "error"
            return False, str(desc)

    if ann.get("err"):
        return False, str(ann["err"])
    return True, None


class TraceFileSpanProcessor:
    """OpenTelemetry-compatible SpanProcessor that writes v1 trace.jsonl records."""

    def __init__(
        self,
        trace_path: str | Path | None = None,
        *,
        run_id: str | None = None,
        job: str | None = None,
        step: int | None = None,
        always_record: bool = False,
    ) -> None:
        self._explicit_path = Path(trace_path) if trace_path is not None else None
        self.run_id = run_id
        self.job = job
        self.step = step
        self.always_record = always_record or (trace_path is not None)
        self._seen_keys: collections.deque[int] = collections.deque(maxlen=4096)
        self._seen_lock = threading.Lock()

    @property
    def trace_path(self) -> Path | None:
        """Return the resolved path where JSONL trace lines are written."""
        return resolve_trace_path(self._explicit_path)

    def on_start(self, span: Any, parent_context: Any = None) -> None:
        """Called when a span starts (no-op for file output)."""
        _ = (span, parent_context)

    def on_end(self, span: Any) -> None:
        """Convert a finished ate_env span into a v1 TraceEvent JSONL line."""
        if is_measure_mode():
            pop_span_annotation(span)
            return
        if not self.always_record and not is_trace_mode():
            pop_span_annotation(span)
            return

        ann = pop_span_annotation(span)
        if ann.get("suppress"):
            return

        name = getattr(span, "name", "") or ""
        op = ann.get("op") or _SPAN_NAME_TO_OP.get(name)
        if not op or op not in _CANONICAL_OPS_SET:
            return

        key = _span_key(span)
        if key is not None:
            with self._seen_lock:
                if key in self._seen_keys:
                    return
                self._seen_keys.append(key)

        attrs = dict(getattr(span, "attributes", None) or {})
        env_id = (
            ann.get("env")
            or attrs.get("ate.actor.name")
            or attrs.get("ate_env.env_id")
            or ""
        )
        if not env_id:
            return

        image = ann.get("image") or attrs.get("ate_env.rootfs_image")
        instance = ann.get("instance")
        run_val = ann.get("run_id") or ann.get("run") or attrs.get("ate_env.run_id") or self.run_id
        if op == "create":
            register_env_meta(
                str(env_id),
                instance=instance,
                image=image,
                run_id=run_val,
                job=self.job,
            )

        start_ns = getattr(span, "start_time", None)
        end_ns = getattr(span, "end_time", None)
        if start_ns is not None and end_ns is not None and end_ns >= start_ns:
            dur_ms = (end_ns - start_ns) / 1e6
        else:
            dur_ms = float(ann.get("dur_ms", getattr(span, "dur_ms", 0.0)))

        ok, err = _extract_span_error(span, ann)
        cmd = ann.get("command")
        if cmd is None and op in ("exec", "shell", "start_process"):
            exe = attrs.get("ate_env.command.executable")
            if exe:
                cmd = [str(exe)]

        path = ann.get("path") or attrs.get("ate_env.file.path")
        size = ann.get("bytes")
        if size is None and "ate_env.file.size" in attrs:
            size = int(attrs["ate_env.file.size"])

        exit_code = ann.get("exit")
        if exit_code is None and "ate_env.exit_code" in attrs:
            exit_code = int(attrs["ate_env.exit_code"])

        ts_str = _utc_rfc3339(end_ns if isinstance(end_ns, int) else None)
        target = self.trace_path
        if target is None:
            return

        with contextlib.suppress(ValueError):
            record_trace_event(
                op,
                env=str(env_id),
                dur_ms=dur_ms,
                ok=ok,
                ts=ts_str,
                run=run_val,
                job=ann.get("job") or self.job,
                step=ann.get("step") if ann.get("step") is not None else self.step,
                instance=instance,
                image=image,
                command=cmd,
                path=path,
                bytes=size,
                exit=exit_code,
                err=err,
                trace_path=target,
                force=True,
            )

    def shutdown(self) -> None:
        """Shut down the processor."""

    def force_flush(self, timeout_millis: int = 30000) -> bool:
        """Flush buffered spans (writes are synchronous per span)."""
        _ = timeout_millis
        return True


def get_default_trace_processor() -> TraceFileSpanProcessor:
    """Return a shared TraceFileSpanProcessor for the current trace path."""
    path = resolve_trace_path()
    key = str(path) if path is not None else "<env>"
    with _META_LOCK:
        proc = _DEFAULT_PROCESSORS.get(key)
        if proc is None:
            proc = TraceFileSpanProcessor(path)
            _DEFAULT_PROCESSORS[key] = proc
        return proc


_SPAN_SEQ = 0
_SPAN_SEQ_LOCK = threading.Lock()


def _next_span_key() -> int:
    global _SPAN_SEQ  # noqa: PLW0603
    with _SPAN_SEQ_LOCK:
        _SPAN_SEQ += 1
        return _SPAN_SEQ


class _TapSpan:
    """Lightweight span wrapper that forwards to an optional OTel span and TraceFileSpanProcessor."""

    def __init__(self, name: str, otel_span: Any = None) -> None:
        self.name = name
        self._otel_span = otel_span
        self.span_key = _next_span_key()
        self.attributes: dict[str, Any] = {}
        self.trace_annotations: dict[str, Any] = {}
        self.start_time = time.time_ns()
        self.end_time: int | None = None
        self.dur_ms: float = 0.0
        self._start_mono = time.perf_counter()
        self.status: Any = None

    def set_attribute(self, key: str, value: Any) -> None:
        """Record an OTel attribute on both the local view and any wrapped OTel span."""
        self.attributes[key] = value
        if self._otel_span is not None:
            with contextlib.suppress(Exception):
                self._otel_span.set_attribute(key, value)

    def annotate_trace(self, **fields: Any) -> None:
        """Record trace-only fields without setting them on the wrapped OTel span."""
        for k, v in fields.items():
            if v is not None:
                self.trace_annotations[k] = v

    def set_status(self, status: Any) -> None:
        """Forward status to the wrapped OTel span and store locally."""
        self.status = status
        if self._otel_span is not None and hasattr(self._otel_span, "set_status"):
            with contextlib.suppress(Exception):
                self._otel_span.set_status(status)

    def record_error(self, description: str) -> None:
        """Mark the span as failed with description."""
        self.trace_annotations["ok"] = False
        self.trace_annotations["err"] = description

    def get_span_context(self) -> Any:
        """Delegate get_span_context to the wrapped OTel span when present."""
        if self._otel_span is not None and hasattr(self._otel_span, "get_span_context"):
            return self._otel_span.get_span_context()
        return None

    def finish(self) -> None:
        """Record end timestamps before passing to TraceFileSpanProcessor.on_end."""
        self.end_time = time.time_ns()
        self.dur_ms = (time.perf_counter() - self._start_mono) * 1000.0
        if self._otel_span is not None:
            otel_attrs = getattr(self._otel_span, "attributes", None)
            if otel_attrs:
                for k, v in dict(otel_attrs).items():
                    self.attributes.setdefault(k, v)
            if self.status is None:
                self.status = getattr(self._otel_span, "status", None)


class TraceTapTracer:
    """Wraps an optional OTel tracer and feeds finished spans to a TraceFileSpanProcessor."""

    def __init__(
        self,
        otel_tracer: Any = None,
        processor: TraceFileSpanProcessor | None = None,
    ) -> None:
        self._otel_tracer = otel_tracer
        self._processor = processor or get_default_trace_processor()

    @contextlib.contextmanager
    def start_as_current_span(self, name: str, **kwargs: Any) -> Iterator[_TapSpan]:
        """Start a span on the underlying OTel tracer (if any) and notify processor on exit."""
        if self._otel_tracer is not None:
            with self._otel_tracer.start_as_current_span(name, **kwargs) as otel_span:
                tap = _TapSpan(name, otel_span)
                self._processor.on_start(tap)
                try:
                    yield tap
                except BaseException as exc:
                    tap.record_error(f"{type(exc).__name__}: {exc}")
                    raise
                finally:
                    tap.finish()
                    self._processor.on_end(tap)
        else:
            tap = _TapSpan(name, None)
            self._processor.on_start(tap)
            try:
                yield tap
            except BaseException as exc:
                tap.record_error(f"{type(exc).__name__}: {exc}")
                raise
            finally:
                tap.finish()
                self._processor.on_end(tap)
