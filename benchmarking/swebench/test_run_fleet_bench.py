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

"""Unit and cross-language integration tests for run_fleet_bench.py."""

from __future__ import annotations

import json
import pathlib
import shutil
import subprocess
import sys
import threading
from typing import Any

import pytest

_DIR = pathlib.Path(__file__).resolve().parent
_REPO_ROOT = _DIR.parents[1]
if str(_DIR) not in sys.path:
    sys.path.insert(0, str(_DIR))

import run_fleet_bench as rfb

_SDK, _INFRA_ERR, _ = rfb.load_sdk()
_PKG_NAME = _SDK.__name__
_mock_backend_mod = __import__(f"{_PKG_NAME}.backend.mock", fromlist=["MockBackendDriver"])
_mock_runtime_mod = __import__(f"{_PKG_NAME}.runtime.mock", fromlist=["MockRuntimeHook"])
_types_mod = __import__(f"{_PKG_NAME}.types", fromlist=["ExecResult", "RawSandboxInstance"])
_errors_mod = __import__(f"{_PKG_NAME}.errors", fromlist=["SandboxUnavailableError"])

MockBackendDriver = _mock_backend_mod.MockBackendDriver
MockRuntimeHook = _mock_runtime_mod.MockRuntimeHook
ExecResult = _types_mod.ExecResult
SandboxUnavailableError = _errors_mod.SandboxUnavailableError


def _digest(n: int) -> str:
    return f"sha256:{n:064x}"


class _GradedMockRuntime(MockRuntimeHook):
    """Mock guest runtime that emits deterministic SWE-bench and R2E verdicts."""

    def __init__(self, *, fail_first_exec: bool = False) -> None:
        super().__init__()
        self._fail_first_exec = fail_first_exec
        self._lock = threading.Lock()
        self._calls = 0

    def exec(
        self,
        command: str | list[str],
        cwd: str = "",
        env: dict[str, str] | None = None,
        timeout_s: float = 120.0,
    ) -> Any:
        cmd_str = command if isinstance(command, str) else " ".join(command)
        with self._lock:
            self._calls += 1
            call_num = self._calls
        if self._fail_first_exec and call_num == 1:
            raise SandboxUnavailableError("transient wake unavailable")
        if "/tmp/ate-grade.py" in cmd_str or "swebench-grade.py" in cmd_str:
            verdict = json.dumps({"valid": True, "resolved": True, "f2p": "1/1", "p2p": "2/2"})
            return ExecResult(exit_code=0, stdout=f"pytest ok\n{verdict}\n", stderr="")
        if "R2E_GRADE_MARKER" in cmd_str:
            verdict = json.dumps({"valid": True, "reward": 1, "detail": "matched 1/1 tests"})
            return ExecResult(exit_code=0, stdout=f"{verdict}\n", stderr="")
        return super().exec(command, cwd=cwd, env=env, timeout_s=timeout_s)


class _GradedMockDriver(MockBackendDriver):
    """MockBackendDriver returning _GradedMockRuntime instances."""

    def __init__(self, *, fail_first_exec: bool = False) -> None:
        super().__init__()
        self._fail_first_exec = fail_first_exec

    def build_runtime(self, instance: Any, data_plane: str = "mock") -> tuple[Any, Any | None]:
        return _GradedMockRuntime(fail_first_exec=self._fail_first_exec), None


def _write_r2e_manifest(path: pathlib.Path, count: int = 6) -> pathlib.Path:
    rows = [
        json.dumps({
            "instance_id": f"r2e-inst-{i:02d}",
            "repo": "demo/repo",
            "image": f"us-docker.pkg.dev/p/r2e/img-{i:02d}@{_digest(10 + i)}",
            "kind": "command",
            "material": {
                "setup_command": "echo setup",
                "reset_command": "echo reset",
                "command": "echo R2E_GRADE_MARKER",
                "expected_exit_code": 0,
            },
        })
        for i in range(count)
    ]
    path.write_text("\n".join(rows) + "\n", encoding="utf-8")
    return path


def _write_swe_manifest(path: pathlib.Path, count: int = 4) -> pathlib.Path:
    rows = [
        json.dumps({
            "instance_id": f"django__django-{10000 + i}",
            "repo": "django/django",
            "image": f"us-docker.pkg.dev/p/swe/img-{i:02d}@{_digest(100 + i)}",
            "patch": "diff --git a/a.py b/a.py\n",
            "test_patch": "diff --git a/t.py b/t.py\n",
            "eval_script": "#!/bin/bash\npytest\n",
            "fail_to_pass": ["test_x"],
            "pass_to_pass": ["test_y"],
            "log_parser": "parse_log_pytest",
            "eval_type": "pass_and_fail",
        })
        for i in range(count)
    ]
    path.write_text("\n".join(rows) + "\n", encoding="utf-8")
    return path


def test_unit_helpers_duration_graders_and_labels(tmp_path, monkeypatch):
    assert rfb.parse_duration_s("500ms") == pytest.approx(0.5)
    assert rfb.parse_duration_s("3m0s") == pytest.approx(180.0)
    assert rfb.parse_duration_s("1h2m3s") == pytest.approx(3723.0)
    assert rfb.parse_duration_s("2.5") == pytest.approx(2.5)
    with pytest.raises(ValueError):
        rfb.parse_duration_s("")
    with pytest.raises(ValueError):
        rfb.parse_duration_s("-5")
    with pytest.raises(ValueError):
        rfb.parse_duration_s("5days")

    assert rfb.format_go_duration(0) == "0s"
    assert rfb.format_go_duration(0.25) == "250ms"
    assert rfb.format_go_duration(180.0) == "3m0s"
    assert rfb.default_window_size(1) == 1
    assert rfb.default_window_size(6) == 3
    assert rfb.default_window_size(20) == 8

    assert rfb.dataset_label("https://storage.googleapis.com/bucket/swe-30.jsonl?sig=secret") == "swe-30.jsonl"
    assert rfb.shard_event_path("/tmp/art", "swe") == pathlib.Path("/tmp/art/events/swe.jsonl")
    assert rfb.shard_event_path("/tmp/art/events", "r2e.jsonl") == pathlib.Path("/tmp/art/events/r2e.jsonl")

    rew, detail = rfb.grade_swebench_output(
        'logs\n{"valid": true, "resolved": true, "f2p": "1/1", "p2p": "3/3"}\n', 0
    )
    assert (rew, detail) == (1, "f2p 1/1 p2p 3/3")
    assert rfb.grade_swebench_output('{"valid": false}\n', 0) == (0, "invalid run")

    assert rfb.grade_command_output('{"valid": true, "reward": 1, "detail": "ok"}\n', "", 0, 0, "") == (1, "ok")
    assert rfb.grade_command_output("wrong\n", "", 0, 0, "right") == (
        0,
        "stdout mismatch: got 'wrong', want 'right'",
    )
    assert rfb.grade_command_output("", "", 2, 0, "") == (0, "exit 2, want 0")

    monkeypatch.setenv(rfb.AUTH_TOKEN_ENV, "  env-secret  ")
    assert rfb.resolve_auth_token("") == "env-secret"
    empty_tok = tmp_path / "empty.tok"
    empty_tok.write_text("   \n", encoding="utf-8")
    with pytest.raises(ValueError, match="is empty"):
        rfb.resolve_auth_token(str(empty_tok))


def test_all_four_strategies_and_fleet_report_outputs(tmp_path):
    r2e_manifest = _write_r2e_manifest(tmp_path / "r2e-6.jsonl", count=6)

    for idx, strategy in enumerate(rfb.VALID_STRATEGIES, start=1):
        out_jsonl = tmp_path / f"events-{strategy}.jsonl"
        report_prefix = tmp_path / f"fleet-{strategy}"
        argv = [
            "--backend=mock",
            f"--dataset={r2e_manifest}",
            "--trajectory=command-default",
            f"--run-id=py-strat-{idx}",
            "--mode=steps",
            f"--strategy={strategy}",
            "--rollouts=2",
            "--steps=1",
            "--width=4",
            f"--out={out_jsonl}",
            f"--fleet-report-out={report_prefix}",
            "--label=client=python",
            f"--label=arm=python-{strategy}",
        ]
        if strategy in ("sliding", "pipelined"):
            argv.append("--window-size=2")

        rc = rfb.run_benchmark(argv, custom_driver=_GradedMockDriver())
        assert rc == 0
        assert (tmp_path / f"fleet-{strategy}.json").is_file()
        assert (tmp_path / f"fleet-{strategy}.txt").is_file()

        events = [json.loads(line) for line in out_jsonl.read_text(encoding="utf-8").splitlines()]
        run_ev = [e for e in events if e["event"] == "run"][-1]
        task_evs = [e for e in events if e["event"] == "task"]
        barrier_evs = [e for e in events if e["event"] == "step_barrier"]

        assert len(task_evs) == 12
        assert all(e["ok"] and e["reward"] == 1 for e in task_evs)
        assert len(barrier_evs) == 1
        assert run_ev["ok"] is True
        assert run_ev["count"] == 12
        assert run_ev["labels"]["strategy"] == strategy
        assert run_ev["labels"]["client"] == "python"
        assert run_ev["labels"]["arm"] == f"python-{strategy}"

        if strategy == "none":
            assert run_ev["working_set_hits"] == 0
            assert run_ev["working_set_misses"] == 12
            assert run_ev["working_set_hit_rate"] == pytest.approx(0.0)
            assert run_ev["standing_pool_size"] == 0
            assert sum(1 for e in events if e["event"] == "ttfi") == 12
            assert sum(1 for e in events if e["event"] == "create") == 12
        elif strategy == "naive":
            assert run_ev["working_set_hits"] == 12
            assert run_ev["working_set_hit_rate"] == pytest.approx(1.0)
            assert run_ev["standing_pool_size"] == 12
            assert sum(1 for e in events if e["event"] == "wake") == 12
            assert any(e["event"] == "prewarm" for e in events)
        elif strategy == "sliding":
            assert run_ev["working_set_hits"] == 12
            assert run_ev["working_set_hit_rate"] == pytest.approx(1.0)
            assert run_ev["standing_pool_size"] == 4
            assert run_ev["labels"]["window_size"] == "2"
            assert sum(1 for e in events if e["event"] == "wake") == 12
        elif strategy == "pipelined":
            assert run_ev["working_set_hits"] == 12
            assert run_ev["working_set_hit_rate"] == pytest.approx(1.0)
            assert 4 <= run_ev["standing_pool_size"] <= 8
            assert run_ev["labels"]["window_size"] == "2"
            assert sum(1 for e in events if e["event"] == "wake") == 12


def test_swebench_gold_smoke_wake_retry_and_validation(tmp_path):
    swe_manifest = _write_swe_manifest(tmp_path / "swe-4.jsonl", count=2)

    # 1. Standalone swebench-gold + transient first-step SandboxUnavailableError retry.
    gold_events_dir = tmp_path / "gold-run"
    rc = rfb.run_benchmark(
        [
            "--backend=mock",
            f"--dataset={swe_manifest}",
            "--trajectory=swebench-gold",
            "--run-id=py-gold",
            "--mode=eval",
            "--strategy=naive",
            "--tasks-per-image=1",
            "--width=2",
            "--wake-budget=5s",
            "--wake-backoff=5ms",
            f"--events-dir={gold_events_dir}",
            "--worker=swe",
        ],
        custom_driver=_GradedMockDriver(fail_first_exec=True),
    )
    assert rc == 0
    swe_jsonl = gold_events_dir / "events" / "swe.jsonl"
    assert swe_jsonl.is_file()
    events = [json.loads(line) for line in swe_jsonl.read_text(encoding="utf-8").splitlines()]
    task_evs = [e for e in events if e["event"] == "task"]
    assert len(task_evs) == 2
    assert all(e["ok"] and e["reward"] == 1 for e in task_evs)

    # 2. Standalone swebench-smoke with --set SMOKE_CMD and first-step exec retry -> attempts == 2.
    smoke_out = tmp_path / "smoke.jsonl"
    rc_smoke = rfb.run_benchmark(
        [
            "--backend=mock",
            f"--dataset={swe_manifest}",
            "--trajectory=swebench-smoke",
            "--set=SMOKE_CMD=echo smoke-ok",
            "--run-id=py-smoke",
            "--mode=eval",
            "--strategy=naive",
            "--width=1",
            "--wake-budget=5s",
            "--wake-backoff=5ms",
            f"--out={smoke_out}",
        ],
        custom_driver=_GradedMockDriver(fail_first_exec=True),
    )
    assert rc_smoke == 0
    smoke_events = [json.loads(line) for line in smoke_out.read_text(encoding="utf-8").splitlines()]
    wake_evs = [e for e in smoke_events if e["event"] == "wake"]
    assert len(wake_evs) == 2
    assert all(e["attempts"] == 2 and e["ok"] is True for e in wake_evs)

    # 3. Fail-loud CLI validation checks.
    with pytest.raises(ValueError, match="--window-size applies to"):
        rfb.run_benchmark(["--dataset", str(swe_manifest), "--strategy=naive", "--window-size=2", "--dry-run"])
    with pytest.raises(ValueError, match="single-pass"):
        rfb.run_benchmark(["--dataset", str(swe_manifest), "--mode=steps", "--steps=2", "--dry-run"])
    with pytest.raises(ValueError, match="loader records that key itself"):
        rfb.run_benchmark(["--dataset", str(swe_manifest), "--label=width=10", "--dry-run"])
    with pytest.raises(ValueError, match="does not read it"):
        rfb.run_benchmark(["--dataset", str(swe_manifest), "--trajectory=swebench-gold", "--set=UNUSED=1", "--dry-run"])

    # 4. Timed-out ExecResult (exit_code=None, timed_out=True) returns clean TimeoutError without TypeError.
    class _TimeoutHandle:
        def exec(self, command, timeout_s=10.0, check=False):
            return ExecResult(exit_code=None, stdout="partial", stderr="", timed_out=True)

    code_t, out_t, _err_t, att_t, exc_t = rfb._execute_step_with_retry(
        handle=_TimeoutHandle(),
        step_spec={"name": "slow", "run": "sleep 99", "timeout_s": 0.1},
        default_step_timeout_s=1.0,
        first=True,
        wake_budget_s=1.0,
        wake_backoff_s=0.01,
        infra_error_cls=_INFRA_ERR,
    )
    assert code_t == -1
    assert out_t == "partial"
    assert att_t == 1
    assert isinstance(exc_t, TimeoutError)

    # 5. Post-acquire runtime construction failure still emits a failed task event.
    class _BrokenRuntimeDriver(_GradedMockDriver):
        def build_runtime(self, instance: Any, data_plane: str = "mock") -> tuple[Any, Any | None]:
            raise RuntimeError("runtime attach boom")

    broken_out = tmp_path / "broken-rt.jsonl"
    rc_broken = rfb.run_benchmark(
        [
            "--backend=mock",
            f"--dataset={swe_manifest}",
            "--trajectory=swebench-smoke",
            "--run-id=py-broken",
            "--mode=eval",
            "--strategy=naive",
            "--width=2",
            f"--out={broken_out}",
        ],
        custom_driver=_BrokenRuntimeDriver(),
    )
    assert rc_broken == 1
    broken_evs = [json.loads(line) for line in broken_out.read_text(encoding="utf-8").splitlines()]
    broken_tasks = [e for e in broken_evs if e["event"] == "task"]
    assert len(broken_tasks) == 2
    assert all(e["ok"] is False and "runtime attach boom" in e.get("err", "") for e in broken_tasks)
    broken_run = next(e for e in broken_evs if e["event"] == "run")
    assert broken_run["labels"]["set.SMOKE_CMD"] == "git status --short && python3 --version || python --version"


@pytest.mark.skipif(shutil.which("go") is None, reason="go toolchain required for cross-language report check")
def test_cross_language_render_plan_and_go_report_compatibility(tmp_path):
    r2e_manifest = _write_r2e_manifest(tmp_path / "r2e-6.jsonl", count=6)
    plan_json = tmp_path / "r2e-plan.json"

    subprocess.run(
        [
            "go",
            "run",
            "./cmd/benchmarking/swebench-loader",
            "render-plan",
            f"-dataset={r2e_manifest}",
            "-images=6",
            "-trajectory=command-default",
            "-fanout=2",
            f"-out={plan_json}",
        ],
        cwd=_REPO_ROOT,
        check=True,
    )
    assert plan_json.is_file()

    arm_reports: list[str] = []
    for idx, strategy in enumerate(rfb.VALID_STRATEGIES, start=1):
        out_jsonl = tmp_path / f"py-{strategy}.jsonl"
        argv = [
            "--backend=mock",
            f"--rendered-plan={plan_json}",
            f"--run-id=py-a{idx}",
            "--mode=steps",
            f"--strategy={strategy}",
            "--rollouts=2",
            "--steps=1",
            "--width=4",
            f"--out={out_jsonl}",
            "--label=client=python",
            f"--label=arm=python-{strategy}",
        ]
        if strategy in ("sliding", "pipelined"):
            argv.append("--window-size=2")
        assert rfb.run_benchmark(argv, custom_driver=_GradedMockDriver()) == 0

        summary_json = tmp_path / f"py-{strategy}-summary.json"
        subprocess.run(
            [
                "go",
                "run",
                "./cmd/benchmarking/swebench-loader",
                "report",
                str(out_jsonl),
                "-json",
                str(summary_json),
            ],
            cwd=_REPO_ROOT,
            check=True,
            capture_output=True,
            text=True,
        )
        summary = json.loads(summary_json.read_text(encoding="utf-8"))
        assert summary["tasks"] == 12
        assert summary["rewarded"] == 12
        assert summary["reward_rate"] == pytest.approx(1.0)
        if strategy == "none":
            assert summary["labels"]["park"] == "delete"
            assert summary.get("paused_sandboxes", 0) == 0
        else:
            assert summary["labels"]["park"] == "suspend"
            assert summary.get("paused_sandboxes", 0) > 0
        summary["run"] = f"python-{strategy}"
        summary_json.write_text(json.dumps(summary, indent=2) + "\n", encoding="utf-8")
        arm_reports.append(str(summary_json))

    comp_json = tmp_path / "comparison.json"
    comp_proc = subprocess.run(
        [
            "go",
            "run",
            "./cmd/benchmarking/swebench-loader",
            "report",
            *arm_reports,
            "-json",
            str(comp_json),
        ],
        cwd=_REPO_ROOT,
        check=True,
        capture_output=True,
        text=True,
    )
    for col in ("python-none", "python-naive", "python-sliding", "python-pipelined"):
        assert col in comp_proc.stdout
    comp_data = json.loads(comp_json.read_text(encoding="utf-8"))
    assert len(comp_data) == 4
