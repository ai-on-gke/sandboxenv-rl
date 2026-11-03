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

"""Tests for trace.jsonl recording (ENV_BENCH_MODE=trace) and measure mode."""

from __future__ import annotations

# pylint: disable=redefined-outer-name,unused-argument
import json
import os
import subprocess
import sys
from pathlib import Path
from typing import Any

import grpc.aio
import pytest

from ate_env import AsyncSandboxFleet, Client, FleetConfig, SandboxFleet, Task
from ate_env._gen.ateenv.v1alpha import env_pb2_grpc, guest_pb2_grpc
from ate_env.loop_runner import LoopRunner
from ate_env.telemetry import (
    CANONICAL_TRACE_OPS,
    TraceFileSpanProcessor,
    build_trace_event,
    clear_bench_context,
    flush_step_events,
    get_recorded_summary,
    record_trace_event,
    render_prometheus,
    reset_recorded_metrics,
    set_bench_context,
)
from ate_env.telemetry.trace_recorder import resolve_trace_path

from .fakes import FakeEnvironmentService, FakeFileSystemService, FakeProcessService

for _proxy_var in (
    "http_proxy",
    "https_proxy",
    "HTTP_PROXY",
    "HTTPS_PROXY",
    "all_proxy",
    "ALL_PROXY",
):
    os.environ.pop(_proxy_var, None)

REPO_ROOT = Path(__file__).resolve().parents[3]
SCHEMA_PATH = REPO_ROOT / "internal" / "benchmarking" / "schema" / "v1" / "trace_event.schema.json"
VALIDATOR_SCRIPT = REPO_ROOT / "internal" / "benchmarking" / "schema" / "validate_schema.py"


@pytest.fixture(autouse=True)
def _clean_telemetry_state():
    clear_bench_context()
    reset_recorded_metrics()
    yield
    clear_bench_context()
    reset_recorded_metrics()


async def _start_fake_grpc_server() -> tuple[grpc.aio.Server, int]:
    server = grpc.aio.server()
    env_pb2_grpc.add_EnvironmentServiceServicer_to_server(FakeEnvironmentService(), server)
    guest_pb2_grpc.add_ProcessServiceServicer_to_server(FakeProcessService(), server)
    guest_pb2_grpc.add_FileSystemServiceServicer_to_server(FakeFileSystemService(), server)
    port = server.add_insecure_port("127.0.0.1:0")
    await server.start()
    return server, port


@pytest.fixture
def threaded_grpc_endpoint():
    runner = LoopRunner(name="trace-test-fake-server")
    server, port = runner.run(_start_fake_grpc_server())
    try:
        yield f"127.0.0.1:{port}"
    finally:
        runner.run(server.stop(None))
        runner.stop()


def _validate_jsonl_against_schema(trace_file: Path) -> list[dict[str, Any]]:
    assert trace_file.exists(), f"Expected trace file at {trace_file}"
    out = subprocess.run(
        [sys.executable, str(VALIDATOR_SCRIPT), str(SCHEMA_PATH), str(trace_file)],
        capture_output=True,
        text=True,
        check=False,
    )
    assert (
        out.returncode == 0
    ), f"Schema validation failed:\nSTDOUT: {out.stdout}\nSTDERR: {out.stderr}"
    lines = [
        ln.strip() for ln in trace_file.read_text(encoding="utf-8").splitlines() if ln.strip()
    ]
    assert lines, "Expected at least one JSONL trace record"
    return [json.loads(ln) for ln in lines]


def test_build_and_record_trace_event_validation(
    tmp_path: Path, monkeypatch: pytest.MonkeyPatch
):
    trace_file = tmp_path / "manual_trace.jsonl"
    monkeypatch.setenv("ENV_BENCH_MODE", "trace")
    monkeypatch.setenv("ENV_BENCH_TRACE_PATH", str(trace_file))
    set_bench_context(run_id="run-01", job="job-a", step=2)

    for op in CANONICAL_TRACE_OPS:
        ev = record_trace_event(
            op,
            env="sb-unit-1",
            dur_ms=12.3456,
            ok=True,
            instance="django__django-11099",
            image="sweb.eval.x86_64.django__django-11099:latest",
            command=["pytest", "-q"] if op in ("exec", "shell", "start_process") else None,
            path="/workspace/fix.py" if op in ("write_file", "read_file") else None,
            bytes=128 if op in ("write_file", "read_file") else None,
            exit=0 if op in ("exec", "shell", "start_process") else None,
        )
        assert ev is not None
        assert ev["op"] == op
        assert ev["run"] == "run-01"
        assert ev["job"] == "job-a"
        assert ev["step"] == 2

    records = _validate_jsonl_against_schema(trace_file)
    assert [r["op"] for r in records] == list(CANONICAL_TRACE_OPS)

    with pytest.raises(ValueError, match="trace_event.op"):
        build_trace_event("invalid_op", env="sb-1")
    with pytest.raises(ValueError, match="trace_event.env"):
        build_trace_event("create", env="")


def test_resolve_trace_path_artifact_dir_fallback(
    tmp_path: Path, monkeypatch: pytest.MonkeyPatch
):
    monkeypatch.delenv("ENV_BENCH_TRACE_PATH", raising=False)
    monkeypatch.setenv("ARTIFACT_DIR", str(tmp_path / "artifacts"))
    assert resolve_trace_path() == tmp_path / "artifacts" / "trace.jsonl"


async def test_low_level_client_and_env_emit_valid_trace_jsonl(
    tmp_path: Path, monkeypatch: pytest.MonkeyPatch
):
    trace_file = tmp_path / "client_trace.jsonl"
    monkeypatch.setenv("ENV_BENCH_MODE", "trace")
    monkeypatch.setenv("ENV_BENCH_TRACE_PATH", str(trace_file))
    monkeypatch.setenv("ENV_BENCH_RUN_ID", "run-lowlevel")
    monkeypatch.setenv("ENV_BENCH_JOB", "job-0")
    monkeypatch.setenv("ENV_BENCH_STEP", "1")
    monkeypatch.setenv("ENV_BENCH_INSTANCE", "astropy__astropy-12907")

    server, port = await _start_fake_grpc_server()
    try:
        async with Client(f"127.0.0.1:{port}") as client:
            env = await client.create(
                "env-ll-1",
                template_name="swe-template",
                rootfs_image="sweb.eval.x86_64.astropy__astropy-12907@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
            )
            fetched = await client.get("env-ll-1")
            assert fetched.id == "env-ll-1"

            await env.write_file("/workspace/patch.diff", b"diff --git a/x b/x\n")
            data = await env.read_file_bytes("/workspace/patch.diff")
            assert data == b"diff --git a/x b/x\n"

            res = await env.shell("pytest -q")
            assert res.exit_code == 0

            proc = await env.start_process(["git", "status"])
            proc_res = await proc.wait()
            assert proc_res.exit_code == 0

            await env.suspend()
            await env.delete()
    finally:
        await server.stop(None)

    records = _validate_jsonl_against_schema(trace_file)
    ops = [r["op"] for r in records]
    # Note: env.shell must NOT double-emit a nested start_process record.
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

    by_op = {r["op"]: r for r in records}
    assert by_op["create"]["image"] == "sweb.eval.x86_64.astropy__astropy-12907@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
    assert by_op["create"]["instance"] == "astropy__astropy-12907"
    assert by_op["create"]["run"] == "run-lowlevel"
    assert by_op["create"]["job"] == "job-0"
    assert by_op["create"]["step"] == 1
    assert by_op["write_file"]["path"] == "/workspace/patch.diff"
    assert by_op["write_file"]["bytes"] == 19
    assert by_op["read_file"]["path"] == "/workspace/patch.diff"
    assert by_op["read_file"]["bytes"] == 19
    assert by_op["shell"]["command"] == ["sh", "-c", "pytest -q"]
    assert by_op["shell"]["exit"] == 0
    assert by_op["start_process"]["command"] == ["git", "status"]


def test_sync_substrate_fleet_emits_valid_trace_jsonl(
    tmp_path: Path, monkeypatch: pytest.MonkeyPatch, threaded_grpc_endpoint: str
):
    trace_file = tmp_path / "fleet_substrate_trace.jsonl"
    monkeypatch.setenv("ENV_BENCH_MODE", "trace")
    monkeypatch.setenv("ENV_BENCH_TRACE_PATH", str(trace_file))
    set_bench_context(job="job-sync", step=0)

    cfg = FleetConfig(
        backend="substrate",
        endpoint=threaded_grpc_endpoint,
        strategy="naive",
        max_warmpool_replicas=1,
        max_concurrent=2,
    )
    tasks = [
        Task(id="sympy__sympy-1", image="sweb.eval.x86_64.sympy__sympy-1@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"),
        Task(id="sympy__sympy-2", image="sweb.eval.x86_64.sympy__sympy-1@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"),
    ]

    def worker(task: Task, handle: Any) -> int:
        handle.write_file("/workspace/hello.txt", "world")
        content = handle.read_file_bytes("/workspace/hello.txt")
        assert content == b"world"
        return handle.exec("pytest -k test_sympy").exit_code

    with SandboxFleet(cfg) as fleet:
        fleet.load_tasks(tasks)
        results = fleet.run(worker, concurrency=2)
    assert results == [0, 0]

    records = _validate_jsonl_against_schema(trace_file)
    ops = [r["op"] for r in records]
    assert "create" in ops
    assert "suspend" in ops
    assert "wake" in ops
    assert "write_file" in ops
    assert "read_file" in ops
    assert "exec" in ops
    assert "delete" in ops
    # SandboxHandle.exec must emit 'exec' and suppress nested Env.shell / Env.start_process.
    assert "shell" not in ops
    assert "start_process" not in ops

    exec_records = [r for r in records if r["op"] == "exec"]
    assert len(exec_records) == 2
    assert {r["instance"] for r in exec_records} == {"sympy__sympy-1", "sympy__sympy-2"}
    for rec in exec_records:
        assert rec["command"] == ["sh", "-c", "pytest -k test_sympy"]
        assert rec["exit"] == 0
        assert rec["job"] == "job-sync"
        assert rec["step"] == 0


async def test_async_mock_fleet_emits_valid_trace_jsonl(
    tmp_path: Path, monkeypatch: pytest.MonkeyPatch
):
    trace_file = tmp_path / "fleet_mock_trace.jsonl"
    monkeypatch.setenv("ENV_BENCH_MODE", "trace")
    monkeypatch.setenv("ENV_BENCH_TRACE_PATH", str(trace_file))
    set_bench_context(job="job-async", step=3)

    cfg = FleetConfig(backend="mock", strategy="naive", max_concurrent=2)
    fleet = AsyncSandboxFleet(cfg)
    fleet.load_tasks([
        Task(id="django__django-1", image="sweb.eval.x86_64.django__django-1:latest"),
        Task(id="django__django-2", image="sweb.eval.x86_64.django__django-2:latest"),
    ])

    async def worker(task: Task, handle: Any) -> int:
        await handle.write_file_async("/workspace/in.txt", "abc")
        await handle.read_file_bytes_async("/workspace/in.txt")
        res = await handle.exec_async(["pytest", "-q"])
        return res.exit_code

    results = await fleet.run(worker, concurrency=2)
    assert results == [0, 0]

    records = _validate_jsonl_against_schema(trace_file)
    ops = [r["op"] for r in records]
    assert ops.count("create") == 2
    assert ops.count("write_file") == 2
    assert ops.count("read_file") == 2
    assert ops.count("exec") == 2
    assert ops.count("delete") == 2


def test_measure_mode_writes_zero_trace_lines_and_records_subphases(
    tmp_path: Path, monkeypatch: pytest.MonkeyPatch, threaded_grpc_endpoint: str
):
    trace_file = tmp_path / "must_not_exist.jsonl"
    events_file = tmp_path / "events.jsonl"
    monkeypatch.setenv("ENV_BENCH_MODE", "measure")
    monkeypatch.setenv("ENV_BENCH_TRACE_PATH", str(trace_file))
    monkeypatch.setenv("ENV_BENCH_EVENTS_PATH", str(events_file))
    set_bench_context(run_id="measure-run-1", job="job-m", step=4)

    cfg = FleetConfig(
        backend="substrate",
        endpoint=threaded_grpc_endpoint,
        strategy="naive",
        max_warmpool_replicas=1,
        max_concurrent=2,
    )
    tasks = [
        Task(id="inst-1", image="sweb.eval.x86_64.flask__flask-1@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"),
        Task(id="inst-2", image="sweb.eval.x86_64.flask__flask-1@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"),
    ]

    with SandboxFleet(cfg) as fleet:
        fleet.load_tasks(tasks)
        results = fleet.run(
            lambda task, handle: handle.exec("echo measure").exit_code, concurrency=2
        )
    assert results == [0, 0]

    # Measure mode MUST write zero per-call trace lines.
    assert not trace_file.exists()

    summary = get_recorded_summary()
    assert summary["ops"]["acquire"]["count"] == 2
    assert summary["ops"]["exec"]["count"] == 2
    assert summary["ops"]["suspend"]["count"] == 1

    # Subphase timings: slot_wait, wire_claim, and ready_to_first_command
    subkeys = set(summary["subphases"])
    assert any(k.startswith("slot_wait:") for k in subkeys), subkeys
    assert "wire_claim:warm_pool" in subkeys, subkeys
    assert "wire_claim:cold_create" in subkeys, subkeys
    assert any(k.startswith("ready_to_first_command:") for k in subkeys), subkeys

    prom_text = render_prometheus()
    assert "ate_env_client_acquire_seconds" in prom_text
    assert "ate_env_client_exec_seconds" in prom_text
    assert "ate_env_client_slot_wait_seconds" in prom_text
    assert "ate_env_client_wire_claim_seconds" in prom_text
    assert "ate_env_client_ready_to_first_command_seconds" in prom_text

    # Step-boundary events flushed to ENV_BENCH_EVENTS_PATH
    assert events_file.exists()
    event_lines = [
        json.loads(ln)
        for ln in events_file.read_text(encoding="utf-8").splitlines()
        if ln.strip()
    ]
    event_names = {ev["event"] for ev in event_lines}
    assert {"acquire", "exec", "slot_wait", "wire_claim", "ready_to_first_command"} <= event_names
    for ev in event_lines:
        assert ev["run"] == "measure-run-1"
        assert ev["job"] == "job-m"
        assert ev["step"] == 4


def test_span_processor_handles_explicit_path_and_errors(tmp_path: Path):
    trace_file = tmp_path / "processor.jsonl"
    proc = TraceFileSpanProcessor(trace_file, run_id="r-proc", job="j-proc", step=7)

    class DummySpan:
        name = "ate_env_client.exec"
        span_key = 999001
        start_time = 1_700_000_000_000_000_000
        end_time = 1_700_000_000_025_000_000
        attributes = {
            "ate.actor.name": "sb-err-1",
            "ate_env.command.executable": "pytest",
            "ate_env.exit_code": 2,
        }
        trace_annotations = {
            "command": ["pytest", "tests/"],
            "ok": False,
            "err": "command exited with code 2",
            "instance": "pytest__pytest-1",
        }
        status = None

    proc.on_start(DummySpan())
    proc.on_end(DummySpan())
    # Duplicate on_end with same span_key is ignored
    proc.on_end(DummySpan())
    assert proc.force_flush() is True
    proc.shutdown()

    records = _validate_jsonl_against_schema(trace_file)
    assert len(records) == 1
    rec = records[0]
    assert rec["op"] == "exec"
    assert rec["env"] == "sb-err-1"
    assert rec["ok"] is False
    assert rec["exit"] == 2
    assert rec["err"] == "command exited with code 2"
    assert rec["dur_ms"] == 25.0
    assert rec["run"] == "r-proc"
    assert rec["job"] == "j-proc"
    assert rec["step"] == 7

    # Also test manual flush_step_events API
    manual_events = tmp_path / "manual_events.jsonl"
    assert flush_step_events(step=1, events_path=manual_events) == []
