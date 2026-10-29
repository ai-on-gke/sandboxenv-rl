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

"""An agent-sandbox-rl style rollout on ate-env: the fleet acquires sandboxes,
each rollout binds an OpenHands workspace to its handle, the fleet releases.

The agent-sandbox-rl rollout shape, with the imports changed:

    fleet = ate_env.SandboxFleet(FleetConfig(backend="substrate", ...))
    def rollout(task, handle):
        workspace = ate_env_openhands.make_handle_workspace(handle, api_key=KEY)
        ...
    fleet.run(rollout)

Environment: SUBSTRATE_ENV_API (ate-env-api host:port, through a port-forward),
OH_TEMPLATE (the base template with the agent-server layer, see demo.sh),
OH_SESSION_KEY (the template's session key), OH_IMAGE (digest-pinned task
image; repeat with commas for several tasks), OH_PROXY_URL (default: http://
plus SUBSTRATE_ENV_API), OH_API_TOKEN (ate-env-api bearer, when it runs with
-auth-token-file), ATESPACE (default ate-env), OH_WARM (idle sandboxes to keep
per image, default 0). Each rollout runs one command through the agent-server
and reports it; replace the body with a Conversation or a tool loop.
"""

from __future__ import annotations

import os
import sys
import time

from ate_env import FleetConfig, SandboxFleet, Task
from ate_env.config import SecretStr
from ate_env_openhands import make_handle_workspace


def main() -> int:
    api = os.environ.get("SUBSTRATE_ENV_API", "127.0.0.1:7777")
    template = os.environ["OH_TEMPLATE"]
    key = os.environ["OH_SESSION_KEY"]
    images = [i for i in os.environ["OH_IMAGE"].split(",") if i]
    proxy = os.environ.get("OH_PROXY_URL") or (api if "://" in api else f"http://{api}")
    token = os.environ.get("OH_API_TOKEN")
    atespace = os.environ.get("ATESPACE", "ate-env")
    warm = int(os.environ.get("OH_WARM", "0"))

    config = FleetConfig(backend="substrate", endpoint=api, tenancy=atespace, template=template,
                         auth_token=SecretStr(token) if token else None,
                         max_concurrent=min(4, len(images)))
    tasks = [Task(id=f"task-{i}", image=image) for i, image in enumerate(images)]

    def rollout(task: Task, handle) -> dict:
        t0 = time.monotonic()
        workspace = make_handle_workspace(handle, api_url=api, proxy_url=proxy, api_key=key,
                                          api_token=token, working_dir="/workspace")
        try:
            ready = time.monotonic() - t0
            result = workspace.execute_command("echo hello from $(hostname); python3 --version", timeout=60)
            return {"task": task.id, "env": handle.sandbox_id, "ready_s": round(ready, 2),
                    "exit": result.exit_code, "out": result.stdout.strip()}
        finally:
            workspace.cleanup()  # HTTP state only; fleet.run releases the sandbox

    with SandboxFleet(config) as fleet:
        fleet.load_tasks(tasks)
        if warm:
            for image in dict.fromkeys(images):
                fleet.set_pool_replicas(image, warm)
        results = fleet.run(rollout)
    for r in results:
        print(r)
    return 0 if all(r.get("exit") == 0 for r in results) else 1


if __name__ == "__main__":
    sys.exit(main())
