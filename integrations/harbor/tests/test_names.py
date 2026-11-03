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

from __future__ import annotations

import re

import pytest

from ate_env_harbor._names import MAX_NAME_LEN, env_id

NAME = re.compile(r"^[a-z0-9]([-a-z0-9]*[a-z0-9])?$")


def test_env_id_keeps_the_session_and_adds_hex():
    first = env_id("hello-world__bZZeEkw__env")
    second = env_id("hello-world__bZZeEkw__env")
    assert first.startswith("hb-hello-world-bzzeekw-env-")
    assert first != second
    assert NAME.match(first) and len(first) <= MAX_NAME_LEN


def test_env_id_fits_long_sessions():
    name = env_id("a" * 200 + "__verifier__grade")
    assert len(name) == MAX_NAME_LEN
    assert NAME.match(name)


@pytest.mark.parametrize(
    ("session", "expected"),
    [
        ("A__B", "hb-a-b-abc123"),
        ("///", "hb-env-abc123"),
        ("harbor/hello-world__x__env", "hb-harbor-hello-world-x-env-abc123"),
    ],
)
def test_env_id_sanitizes(session, expected):
    assert env_id(session, suffix="abc123") == expected
