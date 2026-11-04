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

"""Offline unit and dry-run tests for make-r2e-dataset.py, r2e-gold.yaml, and run-r2e-swebench.sh."""

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
def test_run_r2e_swebench_dry_run_flags_and_validation(tmp_path):
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

    script = _DIR / "run-r2e-swebench.sh"

    help_proc = subprocess.run([str(script), "--help"], capture_output=True, text=True, check=True)
    assert "--concurrency" in help_proc.stdout
    assert "--images" in help_proc.stdout
    assert "--rollouts-per-image" in help_proc.stdout
    assert "--strategy" in help_proc.stdout
    assert "--client" in help_proc.stdout

    # 1. Default 20 + 20 images at max_concurrency=200 (graded, steps mode -> 5 rollouts/image = 100 + 100).
    proc_default = subprocess.run(
        [
            str(script),
            "dry-run",
            "--r2e-dataset",
            str(r2e_manifest),
            "--swe-dataset",
            str(swe_manifest),
            "--artifact-dir",
            str(tmp_path / "art-default"),
        ],
        capture_output=True,
        text=True,
        check=True,
    )
    assert '"run":"r2eswe-swe"' in proc_default.stderr
    assert '"run":"r2eswe-r2e"' in proc_default.stderr
    assert proc_default.stderr.count('"environments":100,"width":100') == 2

    # 2. CLI --concurrency and --images (auto fan-out = ceil(60 / 10) = 6 rollouts/image).
    proc_ci = subprocess.run(
        [
            str(script),
            "dry-run",
            "--r2e-dataset",
            str(r2e_manifest),
            "--swe-dataset",
            str(swe_manifest),
            "--concurrency",
            "120",
            "--images",
            "10",
            "--artifact-dir",
            str(tmp_path / "art-ci"),
        ],
        capture_output=True,
        text=True,
        check=True,
    )
    assert "concurrency=120 images=20 (r2e=10x6@width=60, swe=10x6@width=60)" in proc_ci.stderr
    assert proc_ci.stderr.count('"images":10') == 2
    assert proc_ci.stderr.count('"environments":60,"width":60') == 2

    # 3. CLI --images and --rollouts-per-image without --concurrency -> derives concurrency = 2 * 8 * 3 = 48.
    proc_ir = subprocess.run(
        [
            str(script),
            "dry-run",
            "--r2e-dataset",
            str(r2e_manifest),
            "--swe-dataset",
            str(swe_manifest),
            "--images",
            "8",
            "--rollouts-per-image",
            "3",
            "--artifact-dir",
            str(tmp_path / "art-ir"),
        ],
        capture_output=True,
        text=True,
        check=True,
    )
    assert "concurrency=48 images=16 (r2e=8x3@width=24, swe=8x3@width=24)" in proc_ir.stderr
    assert proc_ir.stderr.count('"environments":24,"width":24') == 2

    # 4. Oversubscribed fleet (--concurrency 80 --images 20 --rollouts-per-image 6 -> 120 envs @ width 40 with -park-each)
    # and asymmetric slice (--concurrency 200 --r2e-images 15 --swe-images 5 --rollouts-per-image 10 -> 150 + 50).
    proc_over = subprocess.run(
        [
            str(script),
            "dry-run",
            "--r2e-dataset",
            str(r2e_manifest),
            "--swe-dataset",
            str(swe_manifest),
            "--concurrency",
            "80",
            "--images",
            "20",
            "--rollouts-per-image",
            "6",
            "--artifact-dir",
            str(tmp_path / "art-over"),
            "--",
            "-label=arm=oversub",
        ],
        capture_output=True,
        text=True,
        check=True,
    )
    assert "concurrency=80 images=40 (r2e=20x6@width=40, swe=20x6@width=40)" in proc_over.stderr
    assert proc_over.stderr.count('"environments":120,"width":40') == 2

    proc_asym = subprocess.run(
        [
            str(script),
            "dry-run",
            "--r2e-dataset",
            str(r2e_manifest),
            "--swe-dataset",
            str(swe_manifest),
            "--concurrency=200",
            "--r2e-images=15",
            "--swe-images=5",
            "--rollouts-per-image=10",
            "--workload=io",
            "--mode=eval",
            "--artifact-dir",
            str(tmp_path / "art-asym"),
        ],
        capture_output=True,
        text=True,
        check=True,
    )
    assert "concurrency=200 images=20 (r2e=15x10@width=150, swe=5x10@width=50)" in proc_asym.stderr
    assert proc_asym.stderr.count('"trajectory":"workload-file-read"') == 2

    # 4b. Environment variable configuration fallback (MAX_CONCURRENCY, IMAGES, ROLLOUTS_PER_IMAGE).
    proc_env = subprocess.run(
        [str(script), "dry-run"],
        env={
            **os.environ,
            "R2E_DATASET": str(r2e_manifest),
            "SWE_DATASET": str(swe_manifest),
            "MAX_CONCURRENCY": "60",
            "IMAGES": "10",
            "ROLLOUTS_PER_IMAGE": "3",
            "ARTIFACT_DIR": str(tmp_path / "art-env"),
        },
        capture_output=True,
        text=True,
        check=True,
    )
    assert "concurrency=60 images=20 (r2e=10x3@width=30, swe=10x3@width=30)" in proc_env.stderr
    assert proc_env.stderr.count('"environments":30,"width":30') == 2

    # 5. Fail-loud validation checks on invalid flags and incompatible oversubscribed step-pause.
    bad_unknown = subprocess.run(
        [str(script), "dry-run", "--bogus"],
        check=False,
        capture_output=True,
        text=True,
    )
    assert bad_unknown.returncode == 2
    assert "unknown argument: --bogus" in bad_unknown.stderr

    bad_conc = subprocess.run(
        [
            str(script),
            "dry-run",
            "--r2e-dataset",
            str(r2e_manifest),
            "--swe-dataset",
            str(swe_manifest),
            "--concurrency",
            "1",
        ],
        check=False,
        capture_output=True,
        text=True,
    )
    assert bad_conc.returncode == 2
    assert "must be >= 2" in bad_conc.stderr

    bad_pause = subprocess.run(
        [
            str(script),
            "dry-run",
            "--r2e-dataset",
            str(r2e_manifest),
            "--swe-dataset",
            str(swe_manifest),
            "--concurrency",
            "80",
            "--images",
            "20",
            "--rollouts-per-image",
            "6",
            "--step-pause",
            "5s",
        ],
        check=False,
        capture_output=True,
        text=True,
    )
    assert bad_pause.returncode == 2
    assert "STEP_PAUSE" in bad_pause.stderr

    # 6. Strategy sweep (--strategy all) across Go API -> 4 arms.
    proc_strat_all = subprocess.run(
        [
            str(script),
            "dry-run",
            "--r2e-dataset",
            str(r2e_manifest),
            "--swe-dataset",
            str(swe_manifest),
            "--concurrency",
            "40",
            "--images",
            "4",
            "--rollouts-per-image",
            "2",
            "--strategy",
            "all",
            "--artifact-dir",
            str(tmp_path / "art-strat-all"),
        ],
        capture_output=True,
        text=True,
        check=True,
    )
    assert "steps=1" in proc_strat_all.stderr
    assert "arms=4 (go-none go-naive go-sliding go-pipelined)" in proc_strat_all.stderr
    for s_name in ("none", "naive", "sliding", "pipelined"):
        assert f'"strategy":"{s_name}"' in proc_strat_all.stderr

    # 7. Python client chain (--client python --strategy pipelined --window-size 4) renders plan JSONs.
    art_py = tmp_path / "art-py"
    proc_py = subprocess.run(
        [
            str(script),
            "dry-run",
            "--r2e-dataset",
            str(r2e_manifest),
            "--swe-dataset",
            str(swe_manifest),
            "--concurrency",
            "40",
            "--images",
            "6",
            "--rollouts-per-image",
            "2",
            "--client",
            "python",
            "--strategy",
            "pipelined",
            "--window-size",
            "4",
            "--artifact-dir",
            str(art_py),
        ],
        capture_output=True,
        text=True,
        check=True,
    )
    assert "arms=1 (python-pipelined)" in proc_py.stderr
    assert proc_py.stderr.count('"client": "python"') == 2
    assert proc_py.stderr.count('"window_size": 4') == 2
    assert (art_py / "plans" / "swe-plan.json").is_file()
    assert (art_py / "plans" / "r2e-plan.json").is_file()

    # 8. Direct Go Substrate chain (--client go-direct) and full matrix (--client all --strategy all).
    proc_direct = subprocess.run(
        [
            str(script),
            "dry-run",
            "--r2e-dataset",
            str(r2e_manifest),
            "--swe-dataset",
            str(swe_manifest),
            "--concurrency",
            "20",
            "--images",
            "6",
            "--rollouts-per-image",
            "2",
            "--client",
            "go-direct",
            "--control-addr",
            "127.0.0.1:7443",
            "--router-addr",
            "127.0.0.1:8443",
            "--strategy",
            "sliding",
            "--window-size",
            "5",
            "--artifact-dir",
            str(tmp_path / "art-direct"),
        ],
        capture_output=True,
        text=True,
        check=True,
    )
    assert "arms=1 (go-direct-sliding)" in proc_direct.stderr
    assert '"window_size":5' in proc_direct.stderr

    proc_all_no_ctrl = subprocess.run(
        [
            str(script),
            "dry-run",
            "--r2e-dataset",
            str(r2e_manifest),
            "--swe-dataset",
            str(swe_manifest),
            "--concurrency",
            "20",
            "--images",
            "2",
            "--rollouts-per-image",
            "1",
            "--client",
            "all",
            "--strategy",
            "all",
            "--artifact-dir",
            str(tmp_path / "art-all-8"),
        ],
        capture_output=True,
        text=True,
        check=True,
    )
    assert (
        "arms=8 (go-none go-naive go-sliding go-pipelined "
        "python-none python-naive python-sliding python-pipelined)"
    ) in proc_all_no_ctrl.stderr

    proc_all_with_ctrl = subprocess.run(
        [
            str(script),
            "dry-run",
            "--r2e-dataset",
            str(r2e_manifest),
            "--swe-dataset",
            str(swe_manifest),
            "--concurrency",
            "20",
            "--images",
            "2",
            "--rollouts-per-image",
            "1",
            "--client",
            "all",
            "--control-addr",
            "127.0.0.1:7443",
            "--strategy",
            "all",
            "--artifact-dir",
            str(tmp_path / "art-all-12"),
        ],
        capture_output=True,
        text=True,
        check=True,
    )
    assert "arms=12 (" in proc_all_with_ctrl.stderr
    assert "go-direct-pipelined" in proc_all_with_ctrl.stderr

    proc_client_all_default = subprocess.run(
        [
            str(script),
            "dry-run",
            "--r2e-dataset",
            str(r2e_manifest),
            "--swe-dataset",
            str(swe_manifest),
            "--concurrency",
            "20",
            "--images",
            "2",
            "--rollouts-per-image",
            "1",
            "--client",
            "all",
            "--artifact-dir",
            str(tmp_path / "art-client-all"),
        ],
        capture_output=True,
        text=True,
        check=True,
    )
    assert "steps=1" in proc_client_all_default.stderr
    assert "arms=2 (go-pipelined python-pipelined)" in proc_client_all_default.stderr

    # 9. Fail-loud checks for strategy/client flags.
    for bad_flags, want_msg in [
        (["dry-run", "--strategy", "bogus"], "STRATEGY must be"),
        (["dry-run", "--client", "bogus"], "CLIENT must be"),
        (["dry-run", "--strategy", "naive", "--window-size", "4"], "--window-size"),
        (["dry-run", "--mode", "open-loop", "--strategy", "all"], "open-loop"),
        (["dry-run", "--mode", "open-loop", "--client", "python"], "open-loop"),
        (["dry-run", "--steps", "2", "--strategy", "sliding"], "--steps"),
        (["dry-run", "--steps", "2", "--client", "python"], "--steps"),
        (["dry-run", "--execution", "in-cluster", "--auth-token-file", str(r2e_manifest)], "AUTH_TOKEN_SECRET"),
        (["dry-run", "--ateapi-ca-file", str(tmp_path / "missing-ca.pem")], "ATEAPI_CA_FILE not found"),
        (["run", "--client", "go-direct"], "--control-addr"),
    ]:
        bad_res = subprocess.run(
            [
                str(script),
                *bad_flags,
                "--r2e-dataset",
                str(r2e_manifest),
                "--swe-dataset",
                str(swe_manifest),
            ],
            check=False,
            capture_output=True,
            text=True,
        )
        assert bad_res.returncode == 2, f"expected rc=2 for {bad_flags}, got {bad_res.returncode}"
        assert want_msg in bad_res.stderr
