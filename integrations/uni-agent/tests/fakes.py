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
"""Re-export the fake Substrate environment/guest gRPC services from clients/python/tests."""

from __future__ import annotations

import importlib.util
import sys
from pathlib import Path

_SRC = Path(__file__).resolve().parents[3] / "clients" / "python" / "tests" / "fakes.py"
_spec = importlib.util.spec_from_file_location("ate_env_test_fakes", _SRC)
_mod = importlib.util.module_from_spec(_spec)
sys.modules[_spec.name] = _mod
_spec.loader.exec_module(_mod)  # type: ignore[union-attr]

FakeEnvironmentService = _mod.FakeEnvironmentService
FakeProcessService = _mod.FakeProcessService
FakeFileSystemService = _mod.FakeFileSystemService
FakeProc = _mod.FakeProc
