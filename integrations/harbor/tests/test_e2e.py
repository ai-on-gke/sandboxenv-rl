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

"""End-to-end test against a live ate-env deployment.

Skipped unless SUBSTRATE_E2E_API_URL points at a reachable ate-env-api (e.g.
`kubectl port-forward -n ate-env svc/ate-env-api 7777:7777` then
`SUBSTRATE_E2E_API_URL=127.0.0.1:7777 pytest tests/test_e2e.py`).
SUBSTRATE_ENV_AUTH_TOKEN is the caller token when the API requires one.
HB_E2E_TEMPLATE names the fallback base template (default `default-template`);
the class template `harbor-c1-m512` is used when it is registered.
HB_E2E_ATESPACE names the target atespace when set.

Runs Harbor's trial runner on the echo task with the oracle agent: the
cluster pulls the pinned alpine image, the verifier gives reward 1, and the
environment is deleted at the end.
"""

from __future__ import annotations

import asyncio
import json
import os
import shutil
import subprocess
import sys
from pathlib import Path

import pytest

from .conftest import ECHO_TASK

API_URL = os.environ.get("SUBSTRATE_E2E_API_URL")
TEMPLATE = os.environ.get("HB_E2E_TEMPLATE", "default-template")
ATESPACE = os.environ.get("HB_E2E_ATESPACE")

pytestmark = pytest.mark.skipif(
    not API_URL, reason="SUBSTRATE_E2E_API_URL not set; e2e needs a live ate-env-api"
)


async def test_oracle_trial_live(tmp_path: Path) -> None:
    jobs = tmp_path / "jobs"
    jobs.mkdir()
    adjacent_harbor = Path(sys.executable).parent / "harbor"
    harbor_bin = (
        str(adjacent_harbor)
        if adjacent_harbor.is_file()
        else (shutil.which("harbor") or str(adjacent_harbor))
    )
    cmd = [
        harbor_bin, "run",
        "-p", str(ECHO_TASK), "-a", "oracle",
        "-e", "ate_env_harbor:AteEnvEnvironment",
        "--ek", f"api_url={API_URL}", "--ek", f"template={TEMPLATE}", "--ek", "build=none",
        *(["--ek", f"atespace={ATESPACE}"] if ATESPACE else []),
        "-n", "1", "--jobs-dir", str(jobs), "-q", "-y",
    ]
    env = {**os.environ, "HARBOR_TELEMETRY": "off"}
    proc = await asyncio.create_subprocess_exec(
        *cmd, env=env, cwd=jobs, stdout=subprocess.PIPE, stderr=subprocess.STDOUT
    )
    out, _ = await asyncio.wait_for(proc.communicate(), 1200)
    text = out.decode(errors="replace")
    results = list(jobs.glob("*/*/result.json"))
    assert len(results) == 1, text
    result = json.loads(results[0].read_text())
    assert result.get("exception_info") is None, text
    assert set((result["verifier_result"]["rewards"] or {}).values()) == {1}, text
    record = json.loads((results[0].parent / "ate_env.json").read_text())
    assert record["id"].startswith("hb-")
