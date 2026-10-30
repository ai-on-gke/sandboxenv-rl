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

"""An agent-sandbox-rl fleet program in smoke mode, with only its imports
changed to ``ate_env_openhands.agent_sandbox_rl``.

It uses the same API surface as agent-sandbox's
``examples/agent-sandbox-rl/examples/run_openhands_fleet.py`` (pod template,
one cluster, warm-pool cap, ``strategy=`` and ``concurrency=``,
``handle.cluster_name``, ``make_handle_workspace(handle, api_key=...)``), so a
test can run it unchanged against the fakes. Not collected by pytest.
"""

import os

from ate_env_openhands.agent_sandbox_rl import (
    ClusterConfig,
    FleetConfig,
    ResourceSpec,
    SandboxFleet,
    Task,
    TemplateSpec,
)
from ate_env_openhands.agent_sandbox_rl.adapters.openhands import make_handle_workspace

SERVER_COMMAND = [
    "tini", "--", "/agent-server/.venv/bin/python", "-m",
    "openhands.agent_server", "--host", "0.0.0.0", "--port", "8000",
]


def _rollout(session_key):
    def fn(task, handle):
        workspace = make_handle_workspace(handle, api_key=session_key or None)
        try:
            result = workspace.execute_command("echo ready && uname -m")
            return {"id": task.id, "mode": "smoke", "cluster": handle.cluster_name,
                    "exit_code": result.exit_code, "output": result.stdout.strip()}
        finally:
            workspace.cleanup()
    return fn


def main():
    n_conversations = int(os.getenv("N_CONVERSATIONS", "2"))
    max_concurrent = int(os.getenv("MAX_CONCURRENT", "2"))
    image = os.environ["AGENT_SERVER_IMAGE"]
    session_key = os.getenv("SANDBOX_SESSION_KEY", "")
    template = TemplateSpec(
        keepalive_command=SERVER_COMMAND,
        runtime_class=None,
        node_selector=None,
        resources=ResourceSpec(cpu="500m", memory="1Gi"),
        extra_pod_spec={"containers": [{"ports": [{"containerPort": 8000}]}]},
    )
    fleet = SandboxFleet(FleetConfig(
        clusters=[ClusterConfig(name="default", namespace="default")],
        max_concurrent=max_concurrent, template=template,
        max_warmpool_size=max(8, max_concurrent), ready_timeout=600))
    fleet.load_tasks([Task(id=f"conv-{i}", image=image, metadata={})
                      for i in range(n_conversations)])
    return fleet.run(_rollout(session_key), strategy=os.getenv("WARMPOOL_STRATEGY", "naive"),
                     concurrency=max_concurrent)
