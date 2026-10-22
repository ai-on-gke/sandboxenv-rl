# Copyright 2026 Google LLC
#
# Licensed under the Apache License, Version 2.0 (the "License");
#
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

from __future__ import annotations

import json
import time
from pathlib import Path
from typing import Any

import pytest

from ate_env.telemetry import (
    BenchTelemetry,
    TraceEventRecord,
    instrument_client,
    validate_event_dict,
    validate_trace_event_dict,
)
from ate_env.types import ShellResult


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

    async def create(self, env_id: str, **_kwargs: Any) -> _FakeEnv:
        """Simulate creating an environment."""
        self.rpc_counter["create"] = self.rpc_counter.get("create", 0) + 1
        return _FakeEnv(env_id, self.rpc_counter)

    async def suspend(self, env_id: str, **_kwargs: Any) -> None:
        """Simulate suspending an environment by ID."""
        _ = env_id
        self.rpc_counter["suspend"] = self.rpc_counter.get("suspend", 0) + 1

    async def delete(self, env_id: str, **_kwargs: Any) -> None:
        """Simulate deleting an environment by ID."""
        _ = env_id
        self.rpc_counter["delete"] = self.rpc_counter.get("delete", 0) + 1

    def env(self, env_id: str, **_kwargs: Any) -> _FakeEnv:
        """Return an environment handle without extra RPCs."""
        return _FakeEnv(env_id, self.rpc_counter)


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

    # Guardrail 4: No extra inspect/status RPCs and no per-call trace file created.
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
    assert 'ate_env_sdk_op_duration_ms_count{job="grpo-trainer",op="create"' in prom_text
    assert 'ate_env_sdk_op_duration_ms_count{job="grpo-trainer",op="shell"' in prom_text
    assert 'ate_env_sdk_op_duration_ms_count{job="grpo-trainer",op="suspend"' in prom_text

    # Coarse step-boundary flush writes valid W1 Event JSONL lines.
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


@pytest.mark.asyncio
async def test_trace_mode_emits_w1_validated_trace_jsonl(tmp_path: Path) -> None:
    """Verify Guardrail 1: trace mode writes W1-compliant TraceEvent JSONL records."""
    trace_file = tmp_path / "trace.jsonl"
    telemetry = BenchTelemetry(
        mode="trace",
        run_id="run-t1",
        job="swe-sim",
        trace_path=trace_file,
    )
    telemetry.set_step(2)

    client = instrument_client(_FakeClient(), telemetry)
    env = await client.create("sbx-trace-0")
    await env.write_file("/tmp/a.txt", b"12345")
    await env.shell("echo ok")
    await env.suspend()
    await env.delete()

    assert trace_file.exists()
    records = [
        json.loads(line)
        for line in trace_file.read_text(encoding="utf-8").splitlines()
        if line.strip()
    ]
    assert [r["op"] for r in records] == ["create", "write_file", "shell", "suspend", "delete"]
    for rec in records:
        validate_trace_event_dict(rec)
        assert rec["schema_version"] == "v1"
        assert rec["job"] == "swe-sim"
        assert rec["env"] == "sbx-trace-0"
        assert rec["step"] == 2

    assert records[1]["path"] == "/tmp/a.txt"
    assert records[1]["bytes"] == 5
    assert records[2]["command"] == ["sh", "-c", "echo ok"]
    assert records[2]["exit"] == 0


def test_strict_schema_validation_rejects_unknown_fields() -> None:
    """Verify Guardrail 1: unknown top-level keys are rejected for both TraceEvent and Event."""
    good_trace = TraceEventRecord(op="shell", dur_ms=12.5, job="j1", env="e1").to_dict()
    validate_trace_event_dict(good_trace)

    bad_trace = dict(good_trace)
    bad_trace["unexpected_field"] = 42
    with pytest.raises(ValueError, match="unknown W1 fields"):
        validate_trace_event_dict(bad_trace)

    bad_event = {
        "ts": "2026-10-22T00:00:00Z",
        "run": "r1",
        "event": "step",
        "dur_ms": 5.0,
        "ok": True,
        "bogus_key": "nope",
    }
    with pytest.raises(ValueError, match="unknown W1 fields"):
        validate_event_dict(bad_event)


def test_measure_mode_overhead_microbenchmark() -> None:
    """Verify W11 acceptance: measure-mode per-op recording overhead stays well under bound."""
    telemetry = BenchTelemetry(mode="measure", job="bench")
    iterations = 2000
    start = time.perf_counter()
    for i in range(iterations):
        telemetry.record_op(
            "shell",
            10.0 + (i % 5),
            ok=True,
            env_id="env-0",
            step=1,
            source="warm_pool",
            cache_state="hit",
            slot_wait_ms=0.5,
        )
    elapsed_ms = (time.perf_counter() - start) * 1000.0
    per_op_us = (elapsed_ms * 1000.0) / iterations
    # 2000 in-memory observations should complete well under 100 us/op (typically ~2-5 us/op).
    assert per_op_us < 100.0
    stats = telemetry.summary_stats()
    assert stats["shell"]["count"] == float(iterations)
    assert stats["shell"]["p50_ms"] >= 10.0
