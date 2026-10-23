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

"""Unit tests and microbenchmarks for W11 SDK telemetry and trace emission."""

# pylint: disable=redefined-builtin,redefined-outer-name,protected-access,no-member
# pylint: disable=too-many-locals,too-many-statements,use-implicit-booleaness-not-comparison

from __future__ import annotations

import asyncio
import importlib.resources
import json
import os
import shutil
import subprocess
import sys
import time
import zipfile
from concurrent.futures import ThreadPoolExecutor
from datetime import datetime, timezone
from pathlib import Path
from typing import Any

import grpc.aio
import pytest

from ate_env import AsyncSandboxFleet, Client, FleetConfig, SandboxFleet, Task
from ate_env._gen.ateenv.v1alpha import env_pb2_grpc, guest_pb2_grpc
from ate_env.telemetry import (
    _MAX_RESERVOIR_SAMPLES,
    CANONICAL_TRACE_OPS,
    BenchTelemetry,
    TraceEventRecord,
    _load_w1_schema,
    instrument_client,
    instrument_fleet,
    validate_event_dict,
    validate_trace_event_dict,
)
from ate_env.telemetry import (
    bench as _bench_mod,
)
from ate_env.types import ShellResult

from .fakes import (
    FakeEnvironmentService,
    FakeFileSystemService,
    FakeProc,
    FakeProcessService,
)


class _FakeEnv:
    """Minimal in-memory Env stand-in that tracks RPC call counts."""

    def __init__(self, env_id: str, rpc_counter: dict[str, int]) -> None:
        self.id = env_id
        self.atespace = "ate-env"
        self._rpc_counter = rpc_counter

    async def shell(self, command_line: str, **_kwargs: Any) -> ShellResult:
        """Simulate a shell execution."""
        self._rpc_counter["shell"] = self._rpc_counter.get("shell", 0) + 1
        return ShellResult(stdout=f"ran: {command_line}\n", stderr="", exit_code=0)

    async def start_process(self, command: list[str], **_kwargs: Any) -> dict[str, Any]:
        """Simulate starting a background process."""
        self._rpc_counter["start_process"] = self._rpc_counter.get("start_process", 0) + 1
        return {"command": command}

    async def write_file(self, path: str, data: bytes, **_kwargs: Any) -> int:
        """Simulate writing a file."""
        self._rpc_counter["write_file"] = self._rpc_counter.get("write_file", 0) + 1
        _ = path
        return len(data)

    async def read_file_bytes(self, path: str, **_kwargs: Any) -> bytes:
        """Simulate reading a file."""
        self._rpc_counter["read_file_bytes"] = self._rpc_counter.get("read_file_bytes", 0) + 1
        _ = path
        return b"hello-world"

    async def suspend(self) -> None:
        """Simulate suspending the environment."""
        self._rpc_counter["suspend"] = self._rpc_counter.get("suspend", 0) + 1

    async def delete(self) -> None:
        """Simulate deleting the environment."""
        self._rpc_counter["delete"] = self._rpc_counter.get("delete", 0) + 1


class _FakeClient:
    """Minimal in-memory Client stand-in that tracks RPC call counts."""

    def __init__(self) -> None:
        self.rpc_counter: dict[str, int] = {}

    async def create(self, id: str, **_kwargs: Any) -> _FakeEnv:
        """Simulate creating an environment."""
        self.rpc_counter["create"] = self.rpc_counter.get("create", 0) + 1
        return _FakeEnv(id, self.rpc_counter)

    async def get(self, id: str, **_kwargs: Any) -> dict[str, Any]:
        """Simulate fetching environment info."""
        self.rpc_counter["get"] = self.rpc_counter.get("get", 0) + 1
        return {"id": id}

    async def suspend(self, id: str, **_kwargs: Any) -> None:
        """Simulate suspending an environment by ID."""
        _ = id
        self.rpc_counter["suspend"] = self.rpc_counter.get("suspend", 0) + 1

    async def delete(self, id: str, **_kwargs: Any) -> None:
        """Simulate deleting an environment by ID."""
        _ = id
        self.rpc_counter["delete"] = self.rpc_counter.get("delete", 0) + 1

    def env(self, id: str, **_kwargs: Any) -> _FakeEnv:
        """Return an environment handle without extra RPCs."""
        return _FakeEnv(id, self.rpc_counter)


@pytest.fixture
async def local_grpc_stack(tmp_path: Path):
    """In-process grpc.aio server using Unix domain socket for sandboxed test execution."""
    server = grpc.aio.server()
    environments = FakeEnvironmentService()
    processes = FakeProcessService()
    filesystem = FakeFileSystemService()
    env_pb2_grpc.add_EnvironmentServiceServicer_to_server(environments, server)
    guest_pb2_grpc.add_ProcessServiceServicer_to_server(processes, server)
    guest_pb2_grpc.add_FileSystemServiceServicer_to_server(filesystem, server)
    sock_path = tmp_path / "ate_env_test.sock"
    endpoint = f"unix:{sock_path}"
    server.add_insecure_port(endpoint)
    await server.start()
    client = Client(endpoint)
    try:
        yield client, environments, processes, filesystem
    finally:
        await client.close()
        await server.stop(None)


def test_bundled_w1_schemas_match_canonical_if_present() -> None:
    """Verify bundled schemas/v1/*.schema.json match internal/benchmarking/rlbench/schema/v1/."""
    py_root = Path(__file__).resolve().parents[1]
    bundled_dir = py_root / "src" / "ate_env" / "schemas" / "v1"
    repo_root = py_root.parents[1]
    canonical_dir = repo_root / "internal" / "benchmarking" / "rlbench" / "schema" / "v1"

    for name in ("trace_event.schema.json", "event.schema.json"):
        bundled_file = bundled_dir / name
        assert bundled_file.is_file(), f"missing bundled schema {bundled_file}"
        parsed = json.loads(bundled_file.read_text(encoding="utf-8"))
        assert parsed["additionalProperties"] is False
        canonical_file = canonical_dir / name
        if canonical_file.is_file():
            assert bundled_file.read_bytes() == canonical_file.read_bytes(), (
                f"{bundled_file} drifted from canonical {canonical_file}"
            )


@pytest.mark.asyncio
async def test_default_mode_is_off_and_instrument_client_is_noop(
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    """Verify default mode is 'off' and instrument_client returns the unwrapped client."""
    monkeypatch.delenv("ENV_BENCH_MODE", raising=False)
    telemetry = BenchTelemetry()
    assert telemetry.mode == "off"

    fake_client = _FakeClient()
    wrapped = instrument_client(fake_client, telemetry)
    assert wrapped is fake_client

    async with telemetry.track("shell", env_id="sbx-off") as meta:
        meta["exit_code"] = 0
    assert telemetry.summary_stats() == {}


@pytest.mark.asyncio
async def test_real_client_and_env_no_double_counting(
    local_grpc_stack: Any, tmp_path: Path
) -> None:
    """Verify real Client + Env instrumentation avoids double-counting shell/suspend/delete."""
    client, _envs, processes, _fs = local_grpc_stack
    trace_file = tmp_path / "real_trace.jsonl"
    telemetry = BenchTelemetry(
        mode="trace",
        run_id="run-real",
        job="swe-real",
        trace_path=trace_file,
    )
    telemetry.set_step(3)
    instrument_client(client, telemetry)

    # Keyword id= argument on create and get
    env = await client.create(id="real-sbx-1")
    info = await client.get(id="real-sbx-1")
    assert info.id == "real-sbx-1"

    await env.write_file("/workspace/hello.txt", b"hello-bytes")
    content = await env.read_file_bytes("/workspace/hello.txt")
    assert content == b"hello-bytes"

    processes.next_procs.append(FakeProc(chunks=[("stdout", b"ok\n")], exit_code=0))
    shell_res = await env.shell("echo ok")
    assert shell_res.exit_code == 0

    processes.next_procs.append(FakeProc(chunks=[("stdout", b"bg\n")], exit_code=0))
    proc = await env.start_process(["echo", "bg"])
    await proc.wait()

    await env.suspend()
    await env.delete()
    telemetry.close()

    records = [
        json.loads(line)
        for line in trace_file.read_text(encoding="utf-8").splitlines()
        if line.strip()
    ]
    ops = [r["op"] for r in records]
    # Exact 1-to-1 mapping: shell must NOT emit an inner start_process,
    # and env.suspend()/env.delete() must NOT double-count via client.suspend()/client.delete().
    assert ops == [
        "create",
        "get",
        "write_file",
        "read_file",
        "shell",
        "start_process",
        "suspend",
        "delete",
    ]
    for rec in records:
        validate_trace_event_dict(rec)
        assert "source" not in rec
        assert rec["env"] == "real-sbx-1"
        assert rec["step"] == 3

    # Verify direct client.get() was recorded with source="direct" and event="get" (not "wake")
    flushed = telemetry.flush_step_events(step=3)
    get_events = [ev for ev in flushed if ev["tags"].get("op") == "get"]
    assert len(get_events) == 1
    assert get_events[0]["event"] == "get"
    assert get_events[0]["tags"]["source"] == "direct"
    assert "source" not in get_events[0]


@pytest.mark.asyncio
async def test_measure_mode_zero_extra_rpcs_and_zero_per_call_files(tmp_path: Path) -> None:
    """Verify Guardrail 4: measure mode emits Prometheus metrics with 0 extra RPCs or trace I/O."""
    trace_file = tmp_path / "should_not_exist.jsonl"
    events_file = tmp_path / "step_events.jsonl"
    telemetry = BenchTelemetry(
        mode="measure",
        run_id="run-m1",
        job="grpo-trainer",
        trace_path=trace_file,
        events_path=events_file,
    )
    telemetry.set_step(1)

    client = instrument_client(_FakeClient(), telemetry)
    env = await client.create("sbx-0")
    await env.write_file("/workspace/patch.diff", b"diff --git a/x b/x\n")
    data = await env.read_file_bytes("/workspace/patch.diff")
    assert data == b"hello-world"
    res = await env.shell("pytest -q")
    assert res.exit_code == 0
    await env.suspend()
    await env.delete()

    assert client.rpc_counter == {
        "create": 1,
        "write_file": 1,
        "read_file_bytes": 1,
        "shell": 1,
        "suspend": 1,
        "delete": 1,
    }
    assert not trace_file.exists()
    assert not events_file.exists()

    prom_text = telemetry.render_prometheus()
    assert 'ate_env_sdk_op_duration_seconds_count{job="grpo-trainer",op="create"' in prom_text
    assert 'ate_env_sdk_op_duration_ms_count{job="grpo-trainer",op="shell"' in prom_text
    assert 'ate_env_sdk_op_duration_ms_count{job="grpo-trainer",op="suspend"' in prom_text

    flushed = telemetry.flush_step_events(step=1)
    assert len(flushed) == 6
    assert events_file.exists()
    lines = [
        json.loads(line)
        for line in events_file.read_text(encoding="utf-8").splitlines()
        if line.strip()
    ]
    assert len(lines) == 6
    for ev in lines:
        validate_event_dict(ev)
        assert ev["run"] == "run-m1"
        assert ev["job"] == "grpo-trainer"
        assert ev["step"] == 1
        assert "dur_ms_mean" in ev["metrics"]
        assert "dur_ms_sum" in ev["metrics"]
        assert "dur_ms_max" in ev["metrics"]


def test_operation_completion_timestamps_preserved_across_delayed_flush(tmp_path: Path) -> None:
    """Verify Event ts reflects actual operation completion time rather than flush call time."""
    events_file = tmp_path / "events.jsonl"
    telemetry = BenchTelemetry(
        mode="measure",
        run_id="run-ts",
        job="ts-job",
        events_path=events_file,
    )
    telemetry.record_op("acquire", 5.0, ok=True, env_id="e1", step=1)
    time.sleep(0.06)
    telemetry.record_op("exec", 15.0, ok=True, env_id="e1", step=1, step_name="run_tests")
    time.sleep(0.06)
    flush_called_at = datetime.now(timezone.utc)
    flushed = telemetry.flush_step_events(step=1)

    assert len(flushed) == 2
    ts_acquire = datetime.fromisoformat(flushed[0]["ts"].replace("Z", "+00:00"))
    ts_exec = datetime.fromisoformat(flushed[1]["ts"].replace("Z", "+00:00"))
    assert (ts_exec - ts_acquire).total_seconds() >= 0.04
    assert (flush_called_at - ts_exec).total_seconds() >= 0.04
    assert flushed[1]["name"] == "run_tests"


def test_strict_schema_validation_enforces_enums_types_and_formats(tmp_path: Path) -> None:
    """Verify W1 schema validator enforces enums, RFC3339 ts, non-negative numbers, and keys."""
    good_trace = TraceEventRecord(
        op="shell",
        dur_ms=12.5,
        job="j1",
        env="e1",
    ).to_dict()
    validate_trace_event_dict(good_trace)

    # TraceEvent does NOT allow a 'source' field under W1 trace_event.schema.json
    with pytest.raises(ValueError, match="unknown W1 fields"):
        validate_trace_event_dict({**good_trace, "source": "warm_pool"})

    # Unknown top-level field
    with pytest.raises(ValueError, match="unknown W1 fields"):
        validate_trace_event_dict({**good_trace, "unexpected_field": 42})

    # Invalid op enum (including non-canonical trace ops like prewarm/retire/reap)
    with pytest.raises(ValueError, match="op must be one of"):
        validate_trace_event_dict({**good_trace, "op": "prewarm"})

    # Invalid RFC3339 timestamp
    with pytest.raises(ValueError, match="valid RFC3339 timestamp"):
        validate_trace_event_dict({**good_trace, "ts": "yesterday"})

    # Empty env string rejected in validator and TraceEventRecord.to_dict(),
    # while record_op logs a warning and skips writing to trace.jsonl on empty env_id
    with pytest.raises(ValueError, match="env is required"):
        validate_trace_event_dict({**good_trace, "env": ""})
    with pytest.raises(ValueError, match="env is required"):
        TraceEventRecord(op="shell", dur_ms=1.0, job="j1", env="").to_dict()

    trace_tel = BenchTelemetry(mode="trace", job="j1", trace_path=tmp_path / "t.jsonl")
    try:
        trace_tel.record_op("shell", 1.0, ok=True, env_id="")
        assert (tmp_path / "t.jsonl").read_text(encoding="utf-8") == ""
    finally:
        trace_tel.close()

    # Negative dur_ms
    with pytest.raises(ValueError, match="dur_ms must be >= 0"):
        validate_trace_event_dict({**good_trace, "dur_ms": -1.0})

    good_event = {
        "ts": "2026-10-22T00:00:00Z",
        "run": "r1",
        "event": "step",
        "name": "run_tests",
        "source": "warm_pool",
        "dur_ms": 5.0,
        "ok": True,
        "count": 3,
        "tags": {"source": "warm_pool"},
        "metrics": {"p95_ms": 6.0},
    }
    validate_event_dict(good_event)

    with pytest.raises(ValueError, match="unknown W1 fields"):
        validate_event_dict({**good_event, "bogus_key": "nope"})

    with pytest.raises(ValueError, match="source must be one of"):
        validate_event_dict({**good_event, "source": "direct"})


@pytest.mark.asyncio
async def test_cancelled_error_vs_timeout_distinguished() -> None:
    """Verify CancelledError sets timed_out=False whereas TimeoutError sets timed_out=True."""
    telemetry = BenchTelemetry(mode="measure", job="cancel-vs-timeout")

    with pytest.raises(asyncio.CancelledError):
        async with telemetry.track("exec", env_id="e1", step=1):
            raise asyncio.CancelledError()

    with pytest.raises(asyncio.TimeoutError):
        async with telemetry.track("acquire", env_id="e1", step=1):
            raise asyncio.TimeoutError("deadline exceeded")

    stats = telemetry.summary_stats()
    assert stats["exec"]["timeouts"] == 0.0
    assert stats["exec"]["errors"] == 1.0
    assert stats["acquire"]["timeouts"] == 1.0
    assert stats["acquire"]["errors"] == 1.0

    flushed = {ev["event"]: ev for ev in telemetry.flush_step_events(step=1)}
    assert flushed["step"]["err"] == "CancelledError"
    assert "timeout" not in flushed["step"]
    assert flushed["step"]["metrics"].get("timeouts", 0.0) == 0.0
    assert flushed["acquire"]["timeout"] is True
    assert flushed["acquire"]["metrics"]["timeouts"] == 1.0


def test_bounded_memory_retention_for_histograms_and_step_aggregates() -> None:
    """Verify reservoir sampling caps histogram samples and measure mode bounds step aggregates."""
    telemetry = BenchTelemetry(mode="measure", job="mem-bound")
    for i in range(3000):
        telemetry.record_op(
            "exec",
            float(i % 100),
            ok=True,
            env_id=f"env-{i}",
            step=1,
            step_name="run_tests",
            source="warm_pool",
        )

    assert len(telemetry._histograms) == 1
    hist = next(iter(telemetry._histograms.values()))
    assert not hasattr(hist, "samples")
    assert len(hist.reservoir) == _MAX_RESERVOIR_SAMPLES
    assert hist.total_count == 3000
    assert hist.min_val == 0.0
    assert hist.max_val == 99.0

    # All 3000 ops share a single (op, name, source, step) aggregate entry in measure mode.
    assert len(telemetry._step_aggregates) == 1
    flushed = telemetry.flush_step_events(step=1)
    assert len(flushed) == 1
    assert flushed[0]["count"] == 3000
    assert flushed[0]["dur_ms"] == pytest.approx(148500.0, abs=0.01)
    assert flushed[0]["metrics"]["dur_ms_sum"] == pytest.approx(148500.0, abs=0.01)
    assert flushed[0]["metrics"]["dur_ms_mean"] == pytest.approx(49.5, abs=0.01)
    assert flushed[0]["metrics"]["dur_ms_max"] == 99.0


def test_thread_safety_under_concurrent_workers() -> None:
    """Verify BenchTelemetry is thread-safe across concurrent worker threads."""
    telemetry = BenchTelemetry(mode="measure", job="threaded-bench")
    num_threads = 8
    ops_per_thread = 250

    def _worker(tid: int) -> None:
        for i in range(ops_per_thread):
            telemetry.record_op(
                "shell",
                1.0 + (i % 10),
                ok=(i % 17 != 0),
                env_id=f"env-{tid}",
                step=1,
                source="warm_pool",
            )
            if i % 50 == 0:
                _ = telemetry.summary_stats()
                _ = telemetry.render_prometheus()

    with ThreadPoolExecutor(max_workers=num_threads) as pool:
        futures = [pool.submit(_worker, tid) for tid in range(num_threads)]
        for fut in futures:
            fut.result()

    stats = telemetry.summary_stats()
    assert stats["shell"]["count"] == float(num_threads * ops_per_thread)


def test_trace_mode_requires_path_and_supports_artifact_dir_and_worker_shards(
    monkeypatch: pytest.MonkeyPatch, tmp_path: Path
) -> None:
    """Verify trace mode validates path configuration and writes per-worker event shards."""
    monkeypatch.delenv("ENV_BENCH_TRACE_PATH", raising=False)
    monkeypatch.delenv("ENV_BENCH_EVENTS_PATH", raising=False)
    monkeypatch.delenv("ENV_BENCH_METRICS_PATH", raising=False)
    monkeypatch.delenv("ARTIFACT_DIR", raising=False)

    with pytest.raises(ValueError, match="trace mode requires trace_path"):
        BenchTelemetry(mode="trace", job="missing-path")

    monkeypatch.setenv("ARTIFACT_DIR", str(tmp_path))
    telemetry = BenchTelemetry(mode="trace", job="art-job", worker_id="worker-7")
    telemetry.record_op("create", 4.2, ok=True, env_id="sbx-7", step=1)
    # Empty env_id in trace mode logs a warning and skips writing to trace.jsonl
    # rather than raising inside track()'s finally block.
    telemetry.record_op("exec", 1.5, ok=True, env_id="", step=1)
    flushed = telemetry.flush_step_events(step=1)
    assert len(flushed) == 2

    prom_out = telemetry.dump_prometheus()
    assert prom_out is not None and prom_out.exists()
    assert (tmp_path / "trace.jsonl").exists()
    trace_lines = (tmp_path / "trace.jsonl").read_text(encoding="utf-8").splitlines()
    assert len(trace_lines) == 1
    assert (tmp_path / "events" / "worker-7.jsonl").exists()
    telemetry.close()


def test_sandbox_fleet_default_telemetry_reads_env_bench_mode(
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    """Verify instrument_fleet honors ENV_BENCH_MODE, explicit telemetry, and one-way dependency."""
    monkeypatch.delenv("ARTIFACT_DIR", raising=False)
    monkeypatch.delenv("ENV_BENCH_TRACE_PATH", raising=False)
    monkeypatch.setenv("ENV_BENCH_MODE", "measure")

    cfg = FleetConfig(backend="mock")
    fleet = instrument_fleet(SandboxFleet(cfg))
    assert fleet.telemetry.mode == "measure"
    assert fleet.telemetry.run_id == fleet.run_id
    fleet.close()

    explicit_tel = BenchTelemetry(mode="measure", run_id="explicit-run", job="kwarg-job")
    fleet_kw = instrument_fleet(SandboxFleet(FleetConfig(backend="mock")), explicit_tel)
    assert fleet_kw.telemetry is explicit_tel
    fleet_kw.close()

    async_fleet_kw = instrument_fleet(AsyncSandboxFleet(FleetConfig(backend="mock")), explicit_tel)
    assert async_fleet_kw.telemetry is explicit_tel
    async_fleet_kw.close()

    # Verify strict one-way dependency: no core SDK module imports bench.py.
    src_root = Path(__file__).resolve().parents[1] / "src" / "ate_env"
    for rel_path in (
        "__init__.py",
        "config.py",
        "client.py",
        "fleet.py",
        "async_fleet.py",
        "handle.py",
        "strategies.py",
        "backend/substrate.py",
        "runtime/substrate_env_client.py",
        "telemetry/tracing.py",
    ):
        text = (src_root / rel_path).read_text(encoding="utf-8")
        assert "BenchTelemetry" not in text, f"{rel_path} references BenchTelemetry"
        assert "telemetry.bench" not in text, f"{rel_path} imports telemetry.bench"


@pytest.mark.asyncio
async def test_prometheus_label_escaping_and_error_truncation() -> None:
    """Verify Prometheus label escaping and measure-mode error truncation."""
    telemetry = BenchTelemetry(mode="measure", job='job"with\\quotes\nand_newline')
    with pytest.raises(RuntimeError):
        async with telemetry.track("exec", env_id="e1", step_name='step"1\n'):
            raise RuntimeError("first line of error\nsecond line of traceback\nthird line")

    prom = telemetry.render_prometheus()
    assert 'job="job\\"with\\\\quotes\\nand_newline"' in prom
    assert "ate_env_sdk_op_duration_seconds_bucket" in prom

    flushed = telemetry.flush_step_events()
    assert len(flushed) == 1
    assert flushed[0]["err"] == "RuntimeError: first line of error"
    assert "\n" not in flushed[0]["err"]


@pytest.mark.asyncio
async def test_substrate_driver_and_runtime_sdk_telemetry_integration(
    monkeypatch: pytest.MonkeyPatch, local_grpc_stack: Any, tmp_path: Path
) -> None:
    """Verify Fleet, Driver, Runtime, and SandboxHandle emit W1 telemetry ops and ttfi/wake."""
    monkeypatch.delenv("ARTIFACT_DIR", raising=False)
    monkeypatch.delenv("ENV_BENCH_TRACE_PATH", raising=False)
    monkeypatch.delenv("ENV_BENCH_EVENTS_PATH", raising=False)
    monkeypatch.delenv("ENV_BENCH_METRICS_PATH", raising=False)
    client, _envs, processes, _fs = local_grpc_stack
    trace_file = tmp_path / "sdk_trace.jsonl"
    telemetry = BenchTelemetry(
        mode="trace",
        run_id="run-sdk",
        job="sdk-int",
        trace_path=trace_file,
    )
    telemetry.set_step(5)

    cfg = FleetConfig(
        backend="substrate",
        endpoint=client.endpoint,
        template="tmpl-test",
        max_warmpool_replicas=1,
        max_concurrent=1,
    )
    fleet = instrument_fleet(AsyncSandboxFleet(cfg), telemetry)
    img = "docker.io/library/python@sha256:" + "d" * 64
    task1 = Task(id="t-warm", image=img)
    task2 = Task(id="t-cold", image=img)
    fleet.load_tasks([task1, task2])

    # 1. Prewarm 1 sandbox -> records 'prewarm' event and canonical 'create'+'suspend' trace lines
    await fleet.setup()

    # 2. Acquire from warm pool -> records 'acquire' event and canonical 'wake' trace line
    handle_warm = await fleet.acquire(task1)
    await asyncio.sleep(0.03)
    processes.next_procs.append(FakeProc(chunks=[("stdout", b"warm-ready\n")], exit_code=0))
    res1 = await handle_warm.exec_async("echo warm", name="apply_patch")
    assert res1.ok
    await handle_warm.recycle_async()

    # 3. Drain warm pool so next acquire is cold_create
    await fleet.teardown()
    handle_cold = await fleet.acquire(task2)
    await asyncio.sleep(0.03)
    processes.next_procs.append(FakeProc(chunks=[("stdout", b"cold-ready\n")], exit_code=0))
    res2 = await handle_cold.exec_async("pytest -q", name="run_tests")
    assert res2.ok
    await handle_cold.release_async()
    fleet.close()

    stats = telemetry.summary_stats()
    for expected_op in (
        "prewarm",
        "acquire",
        "wake",
        "ttfi",
        "exec",
        "task_exec",
        "suspend",
        "delete",
        "retire",
    ):
        assert expected_op in stats, f"missing expected SDK op {expected_op!r} in {list(stats)}"

    # Verify ttfi and wake include the Ready-to-first-command idle gap (~30ms)
    # while exec records pure command execution duration
    assert stats["wake"]["max_ms"] >= 25.0
    assert stats["ttfi"]["max_ms"] >= 25.0
    assert stats["exec"]["count"] == 2.0
    assert stats["ttfi"]["max_ms"] > stats["exec"]["max_ms"]

    # Verify every line in trace.jsonl is strictly a canonical W1 TraceEvent
    # and the Fleet lifecycle emits the full per-environment op sequence with image & instance
    trace_records = [
        json.loads(line)
        for line in trace_file.read_text(encoding="utf-8").splitlines()
        if line.strip()
    ]
    assert [r["op"] for r in trace_records] == [
        "create",
        "suspend",
        "wake",
        "exec",
        "suspend",
        "delete",
        "create",
        "exec",
        "delete",
    ]
    for rec in trace_records:
        validate_trace_event_dict(rec)
        assert rec["op"] in CANONICAL_TRACE_OPS
    assert trace_records[2]["env"] == handle_warm.sandbox_id
    assert trace_records[2]["image"] == img
    assert trace_records[2]["instance"] == "t-warm"
    assert trace_records[6]["env"] == handle_cold.sandbox_id
    assert trace_records[6]["image"] == img
    assert trace_records[6]["instance"] == "t-cold"

    flushed = telemetry.flush_step_events(step=5)
    for ev in flushed:
        validate_event_dict(ev)
    by_event_source = {
        (e["event"], e.get("source", "direct"), e.get("name", "")): e for e in flushed
    }
    assert ("acquire", "warm_pool", "") in by_event_source
    assert ("acquire", "cold_create", "") in by_event_source
    assert ("wake", "warm_pool", "") in by_event_source
    assert ("ttfi", "cold_create", "") in by_event_source
    assert ("step", "warm_pool", "apply_patch") in by_event_source
    assert ("step", "cold_create", "run_tests") in by_event_source


@pytest.mark.asyncio
async def test_concurrent_async_overhead_benchmark() -> None:
    """Verify measure mode and off mode add negligible relative overhead under concurrent load."""
    concurrency = 16
    ops_per_worker = 50
    total_ops = concurrency * ops_per_worker

    async def _run_workload(client_obj: Any) -> float:
        env = await client_obj.create("bench-env")

        async def _worker() -> None:
            for _ in range(ops_per_worker):
                await env.shell("echo benchmark")

        start = time.perf_counter()
        await asyncio.gather(*(_worker() for _ in range(concurrency)))
        return (time.perf_counter() - start) * 1000.0

    # Warm up event loop
    await _run_workload(_FakeClient())

    # 1. Unwrapped baseline
    base_ms = await _run_workload(_FakeClient())
    per_op_base_us = (base_ms * 1000.0) / total_ops

    # 2. Mode="off"
    off_telemetry = BenchTelemetry(mode="off", job="bench-off")
    off_client = instrument_client(_FakeClient(), off_telemetry)
    assert off_client is not None
    off_ms = await _run_workload(off_client)
    per_op_off_us = (off_ms * 1000.0) / total_ops

    # 3. Mode="measure"
    measure_telemetry = BenchTelemetry(mode="measure", job="bench-measure")
    measure_client = instrument_client(_FakeClient(), measure_telemetry)
    measure_ms = await _run_workload(measure_client)
    per_op_measure_us = (measure_ms * 1000.0) / total_ops

    # Relative overhead over unwrapped baseline must stay well below 60us/op
    assert (per_op_off_us - per_op_base_us) < 25.0
    assert (per_op_measure_us - per_op_base_us) < 60.0

    # Verify bounded memory state after concurrent benchmark
    stats = measure_telemetry.summary_stats()
    assert stats["shell"]["count"] == float(total_ops)
    for hist in measure_telemetry._histograms.values():
        assert len(hist.reservoir) <= _MAX_RESERVOIR_SAMPLES
    assert len(measure_telemetry._step_aggregates) <= 2


def test_package_data_includes_w1_schemas(
    tmp_path: Path, monkeypatch: pytest.MonkeyPatch
) -> None:
    """Verify wheel packages W1 schemas, lazy loading in off mode, and fail-fast missing schema."""
    monkeypatch.delenv("ENV_BENCH_MODE", raising=False)
    pkg_root = Path(__file__).resolve().parents[1]
    pyproject_text = (pkg_root / "pyproject.toml").read_text(encoding="utf-8")
    assert '"schemas/v1/*.json"' in pyproject_text
    assert (
        importlib.resources.files("ate_env")
        .joinpath("schemas", "v1", "trace_event.schema.json")
        .is_file()
    )
    assert (
        importlib.resources.files("ate_env")
        .joinpath("schemas", "v1", "event.schema.json")
        .is_file()
    )

    # 1. Build an actual wheel in an isolated tmp copy and verify both W1 JSON Schema files
    staged_pkg = tmp_path / "pkg"
    dist_dir = tmp_path / "dist"
    dist_dir.mkdir(parents=True, exist_ok=True)
    (staged_pkg / "src").mkdir(parents=True, exist_ok=True)
    shutil.copy2(pkg_root / "pyproject.toml", staged_pkg / "pyproject.toml")
    shutil.copy2(pkg_root / "README.md", staged_pkg / "README.md")
    shutil.copytree(pkg_root / "src" / "ate_env", staged_pkg / "src" / "ate_env")
    pip_res = subprocess.run(
        [
            sys.executable,
            "-m",
            "pip",
            "wheel",
            "--no-deps",
            "--no-build-isolation",
            str(staged_pkg),
            "-w",
            str(dist_dir),
        ],
        capture_output=True,
        text=True,
        check=False,
    )
    if pip_res.returncode != 0:
        subprocess.run(
            [
                sys.executable,
                "-c",
                "import setuptools.build_meta as bm, sys; bm.build_wheel(sys.argv[1])",
                str(dist_dir),
            ],
            cwd=str(staged_pkg),
            check=True,
        )
    wheels = list(dist_dir.glob("ate_env_client-*.whl"))
    assert len(wheels) == 1
    with zipfile.ZipFile(wheels[0], "r") as zf:
        names = set(zf.namelist())
        assert "ate_env/schemas/v1/trace_event.schema.json" in names
        assert "ate_env/schemas/v1/event.schema.json" in names

    # 2. Verify no hand-written _FALLBACK_SCHEMAS exists and missing schema raises RuntimeError
    assert not hasattr(_bench_mod, "_FALLBACK_SCHEMAS")
    with pytest.raises(RuntimeError, match="W1 JSON Schema"):
        _load_w1_schema("nonexistent.schema.json")

    # 3. Verify lazy schema loading: import ate_env and BenchTelemetry(mode="off") read 0 schemas
    _load_w1_schema.cache_clear()
    assert _load_w1_schema.cache_info().currsize == 0
    off_tel = BenchTelemetry(mode="off")
    assert off_tel.mode == "off"
    assert _load_w1_schema.cache_info().currsize == 0

    env_copy = dict(os.environ)
    env_copy.pop("ENV_BENCH_MODE", None)
    env_copy["PYTHONPATH"] = str(pkg_root / "src")
    subprocess.run(
        [
            sys.executable,
            "-c",
            (
                "import ate_env; from ate_env.telemetry import bench; "
                "assert bench._load_w1_schema.cache_info().currsize == 0"
            ),
        ],
        env=env_copy,
        check=True,
    )

    measure_tel = BenchTelemetry(mode="measure")
    assert measure_tel.mode == "measure"
    assert _load_w1_schema.cache_info().currsize == 2
