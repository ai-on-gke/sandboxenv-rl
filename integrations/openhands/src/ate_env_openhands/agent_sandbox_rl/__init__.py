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

"""agent-sandbox-rl's API on ate-env: port a fleet program by its imports.

Replace the package prefix in the imports and nothing else::

    from ate_env_openhands.agent_sandbox_rl import (
        ClusterConfig, FleetConfig, ResourceSpec, SandboxFleet, Task, TemplateSpec,
    )
    from ate_env_openhands.agent_sandbox_rl.adapters.openhands import make_handle_workspace

Configure the ate-env side with environment variables:
``SUBSTRATE_ENV_API`` (the ate-env-api address) and ``SUBSTRATE_ENV_AUTH_TOKEN``
(the caller token), which the ``ate-env`` CLI also reads, plus
``SUBSTRATE_ENV_TEMPLATE`` (the base ActorTemplate, for OpenHands the template
with the agent-server layer) and ``SUBSTRATE_ENV_ATESPACE`` (when the cluster
namespace is left at its default). See ``config`` for what maps and what is
ignored.
"""

from ate_env import Task

from .config import (
    ClusterConfig,
    FleetConfig,
    ObservabilityConfig,
    ResourceSpec,
    TemplateSpec,
)
from .fleet import SandboxFleet, SandboxHandle

__all__ = [
    "ClusterConfig",
    "FleetConfig",
    "ObservabilityConfig",
    "ResourceSpec",
    "SandboxFleet",
    "SandboxHandle",
    "Task",
    "TemplateSpec",
]
