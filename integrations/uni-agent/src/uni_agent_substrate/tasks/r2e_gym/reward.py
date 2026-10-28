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
"""R2E-Gym grading: the per-test pytest outcome map must equal the dataset's expected map.

R2E-Gym rows carry ``expected_output_json``: a JSON object mapping a test id (the
pytest node id without its file prefix, ``::`` joined by ``.``) to ``PASSED``,
``FAILED`` or ``ERROR``. ``run_tests.sh`` in the image prints pytest's
"short test summary info" section; it is parsed the same way and compared key by key.
"""

from __future__ import annotations

import json
import re
from typing import Any

SUMMARY_MARKER = "short test summary info"
_ANSI = re.compile(r"\x1b\[[0-9;]*m|\[\d+m")
_STATUSES = ("PASSED", "FAILED", "ERROR")


def _test_id(line: str) -> str:
    """``r2e_tests/test_a.py::TestX::test_y - msg`` -> ``TestX.test_y``."""
    parts = line.split("::")
    name = ".".join(parts[1:]) if len(parts) > 1 else line
    return name.split(" - ", 1)[0].strip()


def parse_pytest_summary(log: str | None) -> dict[str, str]:
    """Parse the status of every test listed after pytest's summary marker."""
    if not log or SUMMARY_MARKER not in log:
        return {}
    section = log.split(SUMMARY_MARKER, 1)[1]
    statuses: dict[str, str] = {}
    for raw in section.splitlines():
        line = _ANSI.sub("", raw).strip()
        for status in _STATUSES:   # pytest's short-summary lines start with the status word
            if line.startswith(status + " "):
                statuses[_test_id(line)] = status
                break
    return statuses


def _normalize(mapping: dict[str, Any]) -> dict[str, str]:
    return {_ANSI.sub("", str(k)).split(" - ", 1)[0].strip(): str(v) for k, v in mapping.items()}


def calculate_reward(test_output: str, expected_output_json: str | dict[str, Any]) -> float:
    """1.0 iff the parsed outcome map equals the expected map (same tests, same statuses)."""
    expected_raw = json.loads(expected_output_json) if isinstance(expected_output_json, str) else expected_output_json
    expected = _normalize(expected_raw or {})
    observed = _normalize(parse_pytest_summary(test_output))
    observed.pop("", None)
    expected.pop("", None)
    if not expected or observed.keys() != expected.keys():
        return 0.0
    return 1.0 if all(observed[k] == expected[k] for k in expected) else 0.0


def extract_gold_patch(parsed_commit_content: str | dict[str, Any], *, source_only: bool = True) -> str:
    """Rebuild a unified diff of the gold commit from R2E-Gym's ``parsed_commit_content``.

    Used only by the oracle check: with ``source_only`` test files are skipped, so the
    patch changes the implementation and the hidden tests decide the outcome.
    """
    if not parsed_commit_content:
        return ""
    data = json.loads(parsed_commit_content) if isinstance(parsed_commit_content, str) else parsed_commit_content
    chunks: list[str] = []
    for file_diff in data.get("file_diffs", []):
        header = file_diff.get("header") or {}
        path = ((header.get("file") or {}).get("path")) or ""
        if not path or (source_only and (not path.endswith(".py") or _looks_like_test(path))):
            continue
        chunks.append(f"diff --git a/{path} b/{path}\n")
        if header.get("misc_line"):
            chunks.append(header["misc_line"] + "\n")
        index_line = file_diff.get("index_line")
        if index_line:
            mode = index_line.get("mode") or ""
            chunks.append(
                f"index {index_line.get('old_commit_hash', '')}..{index_line.get('new_commit_hash', '')}"
                f"{' ' + mode if mode else ''}\n"
            )
        minus, plus = file_diff.get("minus_file"), file_diff.get("plus_file")
        if minus and plus:
            chunks.append(f"--- {minus['path']}\n+++ {plus['path']}\n")
        for hunk in file_diff.get("hunks", []):
            desc = hunk.get("descriptor") or {}
            chunks.append(f"@@ -{_range(desc.get('old_range'))} +{_range(desc.get('new_range'))} @@")
            chunks.append(f" {desc['section']}\n" if desc.get("section") else "\n")
            for line in (hunk.get("line_group") or {}).get("all_lines", []):
                prefix = {"context": " ", "added": "+", "deleted": "-", "note": "\\ "}.get(line.get("type", ""))
                if prefix is not None:
                    chunks.append(f"{prefix}{line.get('content', '')}\n")
    return "".join(chunks)


def _looks_like_test(path: str) -> bool:
    parts = path.split("/")
    return (
        path.endswith("_test.py")
        or parts[-1].startswith("test_")
        or any(p in ("test", "tests", "Test", "Tests") for p in parts[:-1])
    )


def _range(rng: dict[str, Any] | None) -> str:
    rng = rng or {}
    start = rng.get("start", 0)
    return f"{start},{rng['length']}" if rng.get("length") is not None else str(start)
