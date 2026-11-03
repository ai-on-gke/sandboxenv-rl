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

"""Offline unit and dry-run tests for make-r2e-dataset.py, r2e-gold.yaml, and run-r2e-swebench-200.sh."""

from __future__ import annotations

import importlib.util
import json
import os
import pathlib
import shutil
import subprocess
import sys
import types
from typing import Any

import pytest

_DIR = pathlib.Path(__file__).resolve().parent
_SPEC = importlib.util.spec_from_file_location("make_r2e_dataset", _DIR / "make-r2e-dataset.py")
assert _SPEC is not None and _SPEC.loader is not None
r2e = importlib.util.module_from_spec(_SPEC)
_SPEC.loader.exec_module(r2e)


def _sample_parsed_commit(
    path: str = "pkg/calc.py",
    *,
    old_line: str = "    return a - b",
    new_line: str = "    return a + b",
    include_test: bool = True,
) -> dict[str, Any]:
    diffs: list[dict[str, Any]] = [
        {
            "header": {"file": {"path": path}, "misc_line": "old mode 100644"},
            "index_line": {
                "old_commit_hash": "1111111",
                "new_commit_hash": "2222222",
                "mode": "100644",
            },
            "minus_file": {"path": f"a/{path}"},
            "plus_file": {"path": f"b/{path}"},
            "hunks": [
                {
                    "descriptor": {
                        "old_range": {"start": 1, "length": 2},
                        "new_range": {"start": 1, "length": 2},
                        "section": "def add(a, b):",
                    },
                    "line_group": {
                        "all_lines": [
                            {"type": "context", "content": "def add(a, b):"},
                            {"type": "deleted", "content": old_line},
                            {"type": "added", "content": new_line},
                            {"type": "note", "content": "No newline at end of file"},
                            {"type": "ignored", "content": "unused"},
                        ]
                    },
                },
                {
                    "descriptor": {
                        "old_range": {"start": 10},
                        "new_range": {"start": 10},
                    },
                    "line_group": {"all_lines": [{"type": "context", "content": ""}]},
                },
            ],
        }
    ]
    if include_test:
        diffs.append(
            {
                "header": {"file": {"path": "tests/test_calc.py"}},
                "minus_file": {"path": "a/tests/test_calc.py"},
                "plus_file": {"path": "b/tests/test_calc.py"},
                "hunks": [],
            }
        )
        diffs.append(
            {
                "header": {"file": {"path": "README.md"}},
                "minus_file": {"path": "a/README.md"},
                "plus_file": {"path": "b/README.md"},
                "hunks": [],
            }
        )
    return {"file_diffs": diffs}


def test_extract_gold_patch_filters_tests_and_formats_hunks():
    assert r2e.extract_gold_patch("") == ""
    assert r2e.extract_gold_patch({}) == ""

    commit = _sample_parsed_commit()
    patch_src = r2e.extract_gold_patch(json.dumps(commit), source_only=True)
    assert "diff --git a/pkg/calc.py b/pkg/calc.py\n" in patch_src
    assert "old mode 100644\n" in patch_src
    assert "index 1111111..2222222 100644\n" in patch_src
    assert "@@ -1,2 +1,2 @@ def add(a, b):\n" in patch_src
    assert "@@ -10 +10 @@\n" in patch_src
    assert "\\ No newline at end of file\n" in patch_src
    assert "tests/test_calc.py" not in patch_src
    assert "README.md" not in patch_src

    patch_all = r2e.extract_gold_patch(commit, source_only=False)
    assert "tests/test_calc.py" in patch_all
    assert "README.md" in patch_all


def test_build_r2e_commands_executes_setup_grade_reset_and_second_step(tmp_path):
    with pytest.raises(ValueError, match="expected_output_json"):
        r2e.build_r2e_commands("diff", {})

    testbed = tmp_path / "testbed"
    hidden = tmp_path / "r2e_tests"
    archive = tmp_path / "work" / "r2e_tests.tar.gz"
    venv_bin = testbed / ".venv" / "bin"
    local_bin = tmp_path / "local_bin"
    work_dir = tmp_path / "work"
    work_dir.mkdir(parents=True)
    (testbed / "pkg" / "__pycache__").mkdir(parents=True)
    (testbed / "pkg" / "__pycache__" / "calc.pyc").write_bytes(b"pyc")
    (testbed / "pkg" / "calc.py").write_text("def add(a, b):\n    return a - b\n", encoding="utf-8")

    venv_bin.mkdir(parents=True)
    helper = venv_bin / "helper-tool"
    helper.write_text("#!/bin/sh\necho ok\n", encoding="utf-8")
    helper.chmod(0o755)
    # Non-executable trailing entry ensures the symlink loop never trips set -e.
    (venv_bin / "zz_notes.txt").write_text("note\n", encoding="utf-8")

    hidden.mkdir(parents=True)
    (hidden / "check.py").write_text(
        "import sys; sys.path.insert(0, '.')\n"
        "from pkg.calc import add\n"
        "assert add(2, 3) == 5\n",
        encoding="utf-8",
    )

    run_tests = testbed / "run_tests.sh"
    run_tests.write_text(
        "#!/usr/bin/env bash\n"
        "if python3 r2e_tests/check.py; then\n"
        "  printf '=== short test summary info ===\\n'\n"
        "  printf '\\x1b[32mPASSED\\x1b[0m r2e_tests/test_calc.py::TestCalc::test_add - ok\\n'\n"
        "  printf 'PASSED r2e_tests/test_calc.py::test_smoke\\n'\n"
        "else\n"
        "  printf '=== short test summary info ===\\n'\n"
        "  printf 'FAILED r2e_tests/test_calc.py::TestCalc::test_add - AssertionError\\n'\n"
        "  printf 'PASSED r2e_tests/test_calc.py::test_smoke\\n'\n"
        "fi\n",
        encoding="utf-8",
    )
    run_tests.chmod(0o755)

    subprocess.run(["git", "init", "-q"], cwd=testbed, check=True)
    subprocess.run(
        ["git", "-c", "user.email=t@localhost", "-c", "user.name=t", "add", "-A"],
        cwd=testbed,
        check=True,
    )
    subprocess.run(
        ["git", "-c", "user.email=t@localhost", "-c", "user.name=t", "commit", "-q", "-m", "init"],
        cwd=testbed,
        check=True,
    )

    patch = (
        "diff --git a/pkg/calc.py b/pkg/calc.py\n"
        "--- a/pkg/calc.py\n"
        "+++ b/pkg/calc.py\n"
        "@@ -1,2 +1,2 @@\n"
        " def add(a, b):\n"
        "-    return a - b\n"
        "+    return a + b\n"
    )
    expected = {"TestCalc.test_add": "PASSED", "test_smoke": "PASSED"}
    setup_cmd, reset_cmd, grade_cmd = r2e.build_r2e_commands(
        patch,
        json.dumps(expected),
        testbed=str(testbed),
        hidden_tests=str(hidden),
        archive=str(archive),
        venv_bin=str(venv_bin),
        local_bin=str(local_bin),
        work_dir=str(work_dir),
    )

    subprocess.run(["sh", "-c", setup_cmd], check=True)
    assert not hidden.exists()
    assert archive.is_file()
    assert not (testbed / "pkg" / "__pycache__").exists()
    assert (local_bin / "helper-tool").is_symlink()

    step1 = subprocess.run(["sh", "-c", grade_cmd], check=True, capture_output=True, text=True)
    verdict1 = json.loads(step1.stdout.strip())
    assert verdict1 == {"valid": True, "reward": 1, "detail": "matched 2/2 tests"}

    subprocess.run(["sh", "-c", reset_cmd], check=True)
    assert not (testbed / "r2e_tests").exists()
    assert "return a - b" in (testbed / "pkg" / "calc.py").read_text(encoding="utf-8")

    # Step 2 proves archive reuse across steps mode rollouts.
    step2 = subprocess.run(["sh", "-c", grade_cmd], check=True, capture_output=True, text=True)
    assert json.loads(step2.stdout.strip())["reward"] == 1

    # Wrong expected map yields reward 0 with exit code 0.
    subprocess.run(["sh", "-c", reset_cmd], check=True)
    _, _, wrong_grade_cmd = r2e.build_r2e_commands(
        patch,
        {"TestCalc.test_add": "FAILED", "test_smoke": "PASSED"},
        testbed=str(testbed),
        hidden_tests=str(hidden),
        archive=str(archive),
        venv_bin=str(venv_bin),
        local_bin=str(local_bin),
        work_dir=str(work_dir),
    )
    wrong = subprocess.run(
        ["sh", "-c", wrong_grade_cmd], check=True, capture_output=True, text=True
    )
    wrong_verdict = json.loads(wrong.stdout.strip())
    assert wrong_verdict["valid"] is True
    assert wrong_verdict["reward"] == 0


def _digest(n: int) -> str:
    return f"sha256:{n:064x}"


def _easy_line(iid: str, image: str, *, empty_patch: bool = False) -> str:
    parsed = {} if empty_patch else _sample_parsed_commit(include_test=False)
    inst = {
        "instance_id": iid,
        "repo_name": "demo/repo",
        "commit_hash": iid,
        "docker_image": image,
        "expected_output_json": json.dumps({"TestCalc.test_add": "PASSED"}),
        "parsed_commit_content": json.dumps(parsed),
    }
    return json.dumps({"responses_create_params": {"metadata": {"instance_dict": json.dumps(inst)}}})


def test_cli_local_jsonl_images_out_and_digests(tmp_path):
    src = tmp_path / "easy.jsonl"
    src.write_text(
        "\n".join(
            [
                _easy_line("inst-b", "r2e/img-b:1"),
                _easy_line("inst-a", "r2e/img-a:1"),
                _easy_line("inst-empty", "r2e/img-empty:1", empty_patch=True),
                _easy_line("inst-dup", "r2e/img-dup:1"),
                _easy_line("inst-excl", "r2e/img-excl:1"),
                _easy_line("inst-missing", "r2e/img-missing:1"),
            ]
        )
        + "\n",
        encoding="utf-8",
    )
    excl = tmp_path / "exclude.txt"
    excl.write_text("# comment\ninst-excl  offline socket test\n\n", encoding="utf-8")

    imgs_only = tmp_path / "images-only.txt"
    rc = r2e.main(
        [
            "--dataset",
            str(src),
            "--exclude-file",
            str(excl),
            "--limit",
            "2",
            "--images-out",
            str(imgs_only),
        ]
    )
    assert rc == 0
    assert imgs_only.read_text(encoding="utf-8").splitlines() == ["r2e/img-a:1", "r2e/img-b:1"]

    digests_file = tmp_path / "images.json"
    digests_file.write_text(
        json.dumps(
            {
                "r2e/img-a:1": f"us-docker.pkg.dev/p/r/img-a@{_digest(1)}",
                "docker.io/r2e/img-b:1": f"us-docker.pkg.dev/p/r/img-b@{_digest(2)}",
                # Duplicate digest of img-a: must be skipped.
                "r2e/img-dup:1": f"us-docker.pkg.dev/p/r/img-dup@{_digest(1)}",
            }
        ),
        encoding="utf-8",
    )
    out = tmp_path / "out.jsonl"
    imgs_written = tmp_path / "images-written.txt"
    rc_missing = r2e.main(
        [
            "--dataset",
            str(src),
            "--exclude-file",
            str(excl),
            "--image-digests",
            str(digests_file),
            "--images-out",
            str(imgs_written),
            "--out",
            str(out),
        ]
    )
    assert rc_missing == 1  # inst-missing had no digest entry

    rows = [json.loads(line) for line in out.read_text(encoding="utf-8").splitlines()]
    assert [r["instance_id"] for r in rows] == ["inst-a", "inst-b"]
    assert all(r["kind"] == "command" for r in rows)
    meta = json.loads((tmp_path / "out.jsonl.meta.json").read_text(encoding="utf-8"))
    assert meta["instances"] == 2
    assert meta["missing_images"] == 1
    assert meta["skipped_empty_patch"] == 1
    assert meta["resolved_revision"].startswith("sha256:")
    assert imgs_written.read_text(encoding="utf-8").splitlines() == ["r2e/img-a:1", "r2e/img-b:1"]


def test_cli_json_registry_pinned_and_error_paths(tmp_path, monkeypatch):
    with pytest.raises(SystemExit):
        r2e.main([])

    # Missing file triggers load error -> exit 2.
    assert r2e.main(["--dataset", str(tmp_path / "nope.jsonl"), "--out", str(tmp_path / "x.jsonl")]) == 2

    flat_json = tmp_path / "flat.json"
    flat_json.write_text(
        json.dumps(
            [
                {
                    "instance_id": "",
                    "repo_name": "org_repo",
                    "commit_hash": "abc123",
                    "docker_image": f"us-docker.pkg.dev/p/r/already@{_digest(10)}",
                    "expected_output_json": {"t": "PASSED"},
                    "parsed_commit_content": _sample_parsed_commit(include_test=False),
                },
                {
                    "instance_id": "reg-item",
                    "repo_name": "org_repo",
                    "commit_hash": "def456",
                    "docker_image": "docker.io/r2e/reg-item:v1",
                    "expected_output_json": {"t": "PASSED"},
                    "parsed_commit_content": _sample_parsed_commit(include_test=False),
                },
            ]
        ),
        encoding="utf-8",
    )

    monkeypatch.setattr(
        r2e,
        "pin_ref",
        lambda ref: ref.split(":v1")[0] + "@" + _digest(11) if ":v1" in ref else ref,
    )
    out = tmp_path / "flat-out.jsonl"
    assert (
        r2e.main(
            [
                "--source",
                "r2e_gym",
                "--dataset",
                str(flat_json),
                "--registry",
                "us-docker.pkg.dev/p/mirror",
                "--limit",
                "2",
                "--out",
                str(out),
            ]
        )
        == 0
    )
    rows = [json.loads(line) for line in out.read_text(encoding="utf-8").splitlines()]
    assert [r["instance_id"] for r in rows] == ["org_repo-abc123", "reg-item"]
    assert rows[1]["image"] == f"us-docker.pkg.dev/p/mirror/r2e/reg-item@{_digest(11)}"


def test_load_r2e_rows_hf_sources_and_missing_hub(tmp_path, monkeypatch):
    monkeypatch.setattr(r2e, "resolve_revision", lambda dataset, rev: None)
    with pytest.raises(RuntimeError, match="huggingface_hub is missing"):
        r2e.load_r2e_rows("r2e_gym_easy", "org/ds", "train", None)

    easy_file = tmp_path / "hub_easy.jsonl"
    easy_file.write_text(_easy_line("hub-1", "r2e/hub:1") + "\n", encoding="utf-8")
    monkeypatch.setattr(r2e, "resolve_revision", lambda dataset, rev: "deadbeef")

    fake_hf = types.ModuleType("huggingface_hub")
    fake_hf.hf_hub_download = lambda ds, fn, repo_type, revision: str(easy_file)  # type: ignore[attr-defined]
    monkeypatch.setitem(sys.modules, "huggingface_hub", fake_hf)
    rows_easy, rev_easy = r2e.load_r2e_rows("r2e_gym_easy", r2e.R2E_EASY_DATASET, "train", None)
    assert rev_easy == "deadbeef"
    assert len(rows_easy) == 1 and rows_easy[0]["instance_id"] == "hub-1"

    fake_ds = types.ModuleType("datasets")
    fake_ds.load_dataset = lambda ds, split, revision: [{"instance_id": "sub-1"}]  # type: ignore[attr-defined]
    monkeypatch.setitem(sys.modules, "datasets", fake_ds)
    rows_sub, rev_sub = r2e.load_r2e_rows("r2e_gym", r2e.R2E_SUBSET_DATASET, "train", "deadbeef")
    assert rev_sub == "deadbeef"
    assert rows_sub == [{"instance_id": "sub-1"}]


@pytest.mark.skipif(shutil.which("go") is None, reason="go toolchain required for dry-run")
def test_run_r2e_swebench_200_dry_run_graded_and_io(tmp_path):
    r2e_src = tmp_path / "r2e-src.jsonl"
    r2e_src.write_text(
        "\n".join(
            _easy_line(f"r2e-inst-{i:02d}", f"us-docker.pkg.dev/p/r2e/img-{i:02d}@{_digest(100 + i)}")
            for i in range(20)
        )
        + "\n",
        encoding="utf-8",
    )
    r2e_manifest = tmp_path / "r2e-20.jsonl"
    assert r2e.main(["--dataset", str(r2e_src), "--out", str(r2e_manifest)]) == 0

    swe_manifest = tmp_path / "swe-20.jsonl"
    swe_rows = [
        json.dumps(
            {
                "instance_id": f"django__django-{10000 + i}",
                "repo": "django/django",
                "image": f"us-docker.pkg.dev/p/swe/img-{i:02d}@{_digest(200 + i)}",
                "patch": "diff --git a/a.py b/a.py\n",
                "test_patch": "diff --git a/t.py b/t.py\n",
                "eval_script": "#!/bin/bash\npytest\n",
                "fail_to_pass": ["test_x"],
                "pass_to_pass": [],
                "log_parser": "parse_log_pytest",
                "eval_type": "pass_and_fail",
            }
        )
        for i in range(20)
    ]
    swe_manifest.write_text("\n".join(swe_rows) + "\n", encoding="utf-8")

    script = _DIR / "run-r2e-swebench-200.sh"

    # 1. Default 20 + 20 images at max_concurrency=200 (graded, steps mode -> 5 rollouts/image = 100 + 100).
    env_20 = {
        **os.environ,
        "R2E_DATASET": str(r2e_manifest),
        "SWE_DATASET": str(swe_manifest),
        "ARTIFACT_DIR": str(tmp_path / "art-20"),
    }
    proc_20 = subprocess.run(
        [str(script), "dry-run"], env=env_20, capture_output=True, text=True, check=True
    )
    assert '"run":"r2eswe20-swe"' in proc_20.stderr
    assert '"run":"r2eswe20-r2e"' in proc_20.stderr
    assert proc_20.stderr.count('"environments":100,"width":100') == 2

    # 2. 10 + 10 images at max_concurrency=200 (auto fan-out = 10 rollouts/image -> 100 + 100 = 200).
    env_10 = {
        **env_20,
        "R2E_IMAGES": "10",
        "SWE_IMAGES": "10",
        "ARTIFACT_DIR": str(tmp_path / "art-10"),
    }
    proc_10 = subprocess.run(
        [str(script), "dry-run"], env=env_10, capture_output=True, text=True, check=True
    )
    assert proc_10.stderr.count('"images":10') == 2
    assert proc_10.stderr.count('"environments":100,"width":100') == 2

    # 3. I/O image-delivery mode (eval mode, workload-file-read).
    env_io = {
        **env_20,
        "WORKLOAD": "io",
        "MODE": "eval",
        "ARTIFACT_DIR": str(tmp_path / "art-io"),
    }
    proc_io = subprocess.run(
        [str(script), "dry-run"], env=env_io, capture_output=True, text=True, check=True
    )
    assert proc_io.stderr.count('"trajectory":"workload-file-read"') == 2
