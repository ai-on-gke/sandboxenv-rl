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
"""R2E-Gym reward parser and gold-patch reconstruction."""

from __future__ import annotations

import json

from uni_agent_substrate.tasks.r2e_gym.reward import (
    calculate_reward,
    extract_gold_patch,
    parse_pytest_summary,
)

LOG = """
============================= test session starts ==============================
collected 3 items
r2e_tests/test_x.py ..F
=========================== short test summary info ============================
PASSED r2e_tests/test_x.py::TestA::test_one
PASSED r2e_tests/test_x.py::test_two
FAILED r2e_tests/test_x.py::test_three - AssertionError: boom
ERROR r2e_tests/test_y.py::test_err - ImportError
========================= 1 failed, 2 passed in 0.10s ==========================
"""


def test_parse_summary():
    assert parse_pytest_summary(LOG) == {
        "TestA.test_one": "PASSED",
        "test_two": "PASSED",
        "test_three": "FAILED",
        "test_err": "ERROR",
    }
    assert parse_pytest_summary("no summary here") == {}
    assert parse_pytest_summary(None) == {}


def test_reward_exact_match_only():
    expected = {"TestA.test_one": "PASSED", "test_two": "PASSED", "test_three": "FAILED", "test_err": "ERROR"}
    assert calculate_reward(LOG, json.dumps(expected)) == 1.0
    expected["test_three"] = "PASSED"
    assert calculate_reward(LOG, json.dumps(expected)) == 0.0
    assert calculate_reward(LOG, json.dumps({"test_two": "PASSED"})) == 0.0  # extra tests observed
    assert calculate_reward("", json.dumps({"test_two": "PASSED"})) == 0.0
    assert calculate_reward(LOG, "{}") == 0.0


def test_gold_patch_skips_tests_and_rebuilds_hunks():
    parsed = {
        "file_diffs": [
            {
                "header": {"file": {"path": "pkg/mod.py"}},
                "index_line": {"old_commit_hash": "aaa", "new_commit_hash": "bbb", "mode": "100644"},
                "minus_file": {"path": "a/pkg/mod.py"},
                "plus_file": {"path": "b/pkg/mod.py"},
                "hunks": [
                    {
                        "descriptor": {"old_range": {"start": 1, "length": 2}, "new_range": {"start": 1, "length": 3}, "section": "def f():"},
                        "line_group": {
                            "all_lines": [
                                {"type": "context", "content": "x = 1"},
                                {"type": "added", "content": "y = 2"},
                                {"type": "deleted", "content": "z = 3"},
                                {"type": "note", "content": "No newline at end of file"},
                            ]
                        },
                    }
                ],
            },
            {"header": {"file": {"path": "tests/test_mod.py"}}, "hunks": []},
            {"header": {"file": {"path": "README.md"}}, "hunks": []},
        ]
    }
    patch = extract_gold_patch(json.dumps(parsed))
    assert patch.startswith("diff --git a/pkg/mod.py b/pkg/mod.py\nindex aaa..bbb 100644\n--- a/pkg/mod.py\n+++ b/pkg/mod.py\n")
    assert "@@ -1,2 +1,3 @@ def f():\n x = 1\n+y = 2\n-z = 3\n\\ No newline at end of file\n" in patch
    assert "test_mod" not in patch and "README" not in patch
    assert extract_gold_patch("") == ""
