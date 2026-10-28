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
"""Register the Substrate provider and the r2e_gym task with uni-agent's lazy registries.

uni-agent looks providers and tasks up by name in module maps and imports the module on
first use. Adding entries here (instead of patching uni-agent) keeps the upstream package
untouched. Every Python process that resolves tasks must import this module: the recipe
installs a ``uni_agent_substrate.pth`` line into site-packages so Ray task workers,
the driver and the reward process all pick it up at interpreter start.
"""

from __future__ import annotations

from . import PROVIDER_NAME, TASK_NAME


def register() -> None:
    try:
        from uni_agent.sandbox.registry import SANDBOX_MODULES
        from uni_agent.tasks.registry import TASK_MODULES
    except ImportError:  # uni-agent not installed in this interpreter: nothing to register
        return
    SANDBOX_MODULES.setdefault(PROVIDER_NAME, "uni_agent_substrate.provider")
    TASK_MODULES.setdefault(TASK_NAME, "uni_agent_substrate.tasks.r2e_gym.task")


register()
