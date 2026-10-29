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
#
"""uni-agent integration for Agent Substrate.

- :mod:`uni_agent_substrate.provider`: sandbox provider ``ate_env`` (environments created through ate-env-api).
- :mod:`uni_agent_substrate.tasks.r2e_gym`: uni-agent task ``r2e_gym`` (hidden-test grading in the same environment).
- :mod:`uni_agent_substrate._register`: adds both to uni-agent's lazy registries; imported from a ``.pth`` hook.
"""

__all__ = ["PROVIDER_NAME", "TASK_NAME"]

PROVIDER_NAME = "ate_env"
TASK_NAME = "r2e_gym"
