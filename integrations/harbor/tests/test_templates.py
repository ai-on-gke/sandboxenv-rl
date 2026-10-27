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

import subprocess
from pathlib import Path

import pytest

from ate_env_harbor.templates import (
    class_template_name,
    cpu_quantity,
    manifest_template_args,
    register_command,
)

SCRIPT = Path(__file__).parent.parent / "hack" / "register-templates.sh"


@pytest.mark.parametrize(
    ("cpus", "memory_mb", "expected"),
    [
        (1, 2048, "harbor-c1-m2048"),
        (2.0, 4096, "harbor-c2-m4096"),
        (0.5, 2048, "harbor-c500m-m2048"),
        (1.25, None, "harbor-c1250m"),
        (None, 512, "harbor-m512"),
        (None, None, None),
    ],
)
def test_class_template_name(cpus, memory_mb, expected):
    assert class_template_name("harbor-", cpus, memory_mb) == expected


def test_class_template_name_rejects_bad_prefix():
    with pytest.raises(ValueError, match="template_prefix"):
        class_template_name("Harbor_", 1, 512)


def test_quantities_and_args():
    assert cpu_quantity(2) == "2" and cpu_quantity(0.5) == "500m"
    assert manifest_template_args(0.5, 2048) == ["--cpu", "500m", "--memory", "2048Mi"]
    assert manifest_template_args(None, None) == []
    with pytest.raises(ValueError):
        cpu_quantity(0)


def test_register_command():
    assert register_command("harbor-", 1, 512, extra=("--snapshots-bucket", "gs://b")) == (
        "ate-env manifest template --template harbor-c1-m512 --cpu 1 --memory 512Mi "
        "--snapshots-bucket gs://b | kubectl-ate create actor-template -f -"
    )
    assert register_command("harbor-", None, None) is None


def _task(root: Path, name: str, body: str) -> None:
    (root / name).mkdir(parents=True)
    (root / name / "task.toml").write_text(f'[task]\nname = "{name}"\n\n[environment]\n{body}\n')


def test_script_prints_one_command_per_class(tmp_path):
    tasks = tmp_path / "tasks"
    _task(tasks, "a", "cpus = 1\nmemory_mb = 512")
    _task(tasks, "b", "cpus = 0.5\nmemory_mb = 2048")
    _task(tasks, "c", "cpus = 1\nmemory_mb = 512")  # the same class as a
    _task(tasks, "d", "")  # no resources: the default template serves it
    out = subprocess.run(
        [str(SCRIPT), str(tasks), "--", "--snapshots-bucket", "gs://b"],
        check=True, capture_output=True, text=True,
    ).stdout.splitlines()
    assert out == [
        register_command("harbor-", 1, 512, extra=("--snapshots-bucket", "gs://b")),
        register_command("harbor-", 0.5, 2048, extra=("--snapshots-bucket", "gs://b")),
    ]
    names = [line.split("--template ")[1].split()[0] for line in out]
    assert names == [class_template_name("harbor-", 1, 512), class_template_name("harbor-", 0.5, 2048)]


def test_script_prefix_option(tmp_path):
    tasks = tmp_path / "tasks"
    _task(tasks, "a", "cpus = 2\nmemory_mb = 1024")
    out = subprocess.run(
        [str(SCRIPT), "--prefix", "tb-", str(tasks)], check=True, capture_output=True, text=True
    ).stdout
    assert "--template tb-c2-m1024 --cpu 2 --memory 1024Mi |" in out
