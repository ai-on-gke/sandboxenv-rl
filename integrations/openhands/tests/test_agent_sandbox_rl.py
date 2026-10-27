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

"""ate_env_openhands.agent_sandbox_rl: agent-sandbox-rl's names on ate-env.

The mapping onto ate_env.FleetConfig, the run() signature, the handles'
cluster_name, and an agent-sandbox-rl program that runs against the fakes with
only its imports changed. With AGENT_SANDBOX_RL_EXAMPLE set to agent-sandbox's
run_openhands_fleet.py, the upstream file itself runs the same way.
"""

from __future__ import annotations

import difflib
import functools
import io
import json
import logging
import runpy
from contextlib import redirect_stdout
from pathlib import Path

import ate_env
import pytest
from ate_env import DEFAULT_ATESPACE
from ate_env.backend.mock import MockBackendDriver

import ate_env_openhands.agent_sandbox_rl.adapters.openhands as compat_openhands
from ate_env_openhands import fleet as oh_fleet
from ate_env_openhands.agent_sandbox_rl import (
    ClusterConfig,
    FleetConfig,
    ObservabilityConfig,
    ResourceSpec,
    SandboxFleet,
    SandboxHandle,
    Task,
    TemplateSpec,
)

from .conftest import IMAGE, SESSION_KEY

ENV_VARS = ("SUBSTRATE_ENV_API", "SUBSTRATE_ENV_AUTH_TOKEN", "SUBSTRATE_ENV_TEMPLATE",
            "SUBSTRATE_ENV_ATESPACE")
PROGRAM = Path(__file__).with_name("agent_sandbox_rl_program.py")
TOKEN = "caller-token"


@pytest.fixture(autouse=True)
def _no_ambient_env(monkeypatch):
    for name in ENV_VARS:
        monkeypatch.delenv(name, raising=False)


def _mock_fleet(config: FleetConfig | None = None) -> SandboxFleet:
    return SandboxFleet(config, driver=MockBackendDriver())


# --- configuration mapping ---------------------------------------------------


def test_fleet_config_maps_onto_ate_env():
    config = FleetConfig(
        clusters=[ClusterConfig(name="c1", namespace="team-a", endpoint="api.example:7777",
                                auth_token=TOKEN)],
        max_concurrent=4, max_warmpool_size=6, window_size=3, ready_timeout=120, run_id="run-1",
        template=TemplateSpec(name="oh-agent-server"),
        observability=ObservabilityConfig(enable_metrics=False, enable_tracing=True,
                                          trace_service_name="rollouts"),
    )
    ate = config.to_ate_env()
    assert isinstance(ate, ate_env.FleetConfig)
    assert (ate.backend, ate.endpoint, ate.tenancy, ate.template) == (
        "substrate", "api.example:7777", "team-a", "oh-agent-server")
    assert (ate.max_concurrent, ate.max_warmpool_replicas, ate.window_size) == (4, 6, 3)
    assert (ate.acquire_timeout_s, ate.ready_timeout_s, ate.run_id) == (120.0, 120.0, "run-1")
    assert ate.auth_token is not None and ate.auth_token.get_secret_value() == TOKEN
    obs = ate.observability
    assert (obs.enable_metrics, obs.enable_tracing, obs.trace_service_name) == (
        False, True, "rollouts")


def test_ate_env_settings_default_from_the_environment(monkeypatch):
    ate = FleetConfig().to_ate_env()
    assert (ate.endpoint, ate.tenancy, ate.template, ate.auth_token) == (
        "127.0.0.1:7777", DEFAULT_ATESPACE, None, None)
    monkeypatch.setenv("SUBSTRATE_ENV_API", "ate-env-api.ate-env:7777")
    monkeypatch.setenv("SUBSTRATE_ENV_AUTH_TOKEN", TOKEN)
    monkeypatch.setenv("SUBSTRATE_ENV_TEMPLATE", "oh-agent-server")
    monkeypatch.setenv("SUBSTRATE_ENV_ATESPACE", "team-b")
    ate = FleetConfig(clusters=[ClusterConfig(namespace="default")]).to_ate_env()
    assert (ate.endpoint, ate.tenancy, ate.template) == (
        "ate-env-api.ate-env:7777", "team-b", "oh-agent-server")
    assert ate.auth_token.get_secret_value() == TOKEN
    # A namespace other than the default, and the ate-env-only fields, win.
    ate = FleetConfig(
        clusters=[ClusterConfig(namespace="team-a", endpoint="other:1", auth_token="t2")],
        template=TemplateSpec(name="base"),
    ).to_ate_env()
    assert (ate.endpoint, ate.tenancy, ate.template) == ("other:1", "team-a", "base")
    assert ate.auth_token.get_secret_value() == "t2"


@pytest.mark.parametrize(("kwargs", "match"), [
    ({"clusters": [ClusterConfig(name="a"), ClusterConfig(name="b")]}, "one ate-env-api endpoint"),
    ({"adopt_existing": True}, "adopt_existing"),
    ({"run_isolation": "names"}, "run_isolation"),
])
def test_settings_ate_env_cannot_honour_raise(kwargs, match):
    with pytest.raises(ValueError, match=match):
        FleetConfig(**kwargs)


@pytest.mark.parametrize("build", [
    lambda: FleetConfig(max_concurrent=0),
    lambda: FleetConfig(placement="random"),
    lambda: FleetConfig(run_id="Not_A_Label"),
    lambda: TemplateSpec(image_pull_policy="Sometimes"),
    lambda: ClusterConfig(weight=0),
])
def test_agent_sandbox_rl_validators_still_apply(build):
    with pytest.raises(ValueError):
        build()


def test_ignored_settings_are_named_in_one_info_log(caplog):
    config = FleetConfig(
        clusters=[ClusterConfig(name="c1", context="gke_x", namespace="team-a")],
        placement="round-robin",
        template=TemplateSpec(resources=ResourceSpec(cpu="2"), extra_pod_spec={"containers": [{}]}),
    )
    assert config.ignored_fields() == [
        "placement", "template.resources", "template.extra_pod_spec", "clusters[0].context"]
    with caplog.at_level(logging.INFO, logger="ate_env_openhands.agent_sandbox_rl"):
        config.to_ate_env()
    [record] = caplog.records
    assert "template.resources" in record.getMessage()
    assert FleetConfig().ignored_fields() == []


def test_the_token_stays_out_of_repr():
    assert TOKEN not in repr(ClusterConfig(auth_token=TOKEN))
    assert TOKEN not in repr(FleetConfig(clusters=[ClusterConfig(auth_token=TOKEN)]).to_ate_env())


# --- fleet -------------------------------------------------------------------


def test_run_takes_strategy_as_its_second_argument():
    fleet = _mock_fleet(FleetConfig(max_concurrent=2))
    fleet.load_tasks([Task(id="t1", image=IMAGE), Task(id="t2", image=IMAGE)])
    assert fleet.run(lambda task, handle: task.id, "none", 2) == ["t1", "t2"]
    assert fleet.report.strategy == "none"
    fleet.run(lambda task, handle: task.id, concurrency=2)
    assert fleet.report.strategy == "naive"  # agent-sandbox-rl's default


def test_epochs_give_one_list_for_each_pass():
    fleet = _mock_fleet()
    fleet.load_tasks([Task(id="t1", image=IMAGE)])
    assert fleet.run(lambda task, handle: task.id, epochs=2) == [["t1"], ["t1"]]
    with pytest.raises(ValueError, match="epochs"):
        fleet.run(lambda task, handle: None, epochs=0)


@pytest.mark.parametrize("kwargs", [{"keep_warm": True}, {"recycle": True}, {"reset": "cd /"}])
def test_session_reuse_options_raise(kwargs):
    fleet = _mock_fleet()
    fleet.load_tasks([Task(id="t1", image=IMAGE)])
    with pytest.raises(NotImplementedError, match=next(iter(kwargs))):
        fleet.run(lambda task, handle: None, **kwargs)


def test_handles_carry_the_cluster_name():
    fleet = _mock_fleet(FleetConfig(clusters=[ClusterConfig(name="c1")]))
    handle = fleet.acquire(Task(id="t1", image=IMAGE))
    assert isinstance(handle, SandboxHandle) and isinstance(handle, ate_env.SandboxHandle)
    assert handle.cluster_name == "c1" and handle.sandbox_id
    [a, b] = fleet.acquire([Task(id="t2", image=IMAGE), Task(id="t3", image=IMAGE)])
    assert a.cluster_name == b.cluster_name == "c1"
    fleet.release([handle, a, b])
    seen = []
    fleet.load_tasks([Task(id="t4", image=IMAGE)])
    fleet.run(lambda task, handle: seen.append(handle.cluster_name))
    assert seen == ["c1"]


def test_the_adapter_names_are_the_integration_ones():
    assert compat_openhands.make_handle_workspace is oh_fleet.make_handle_workspace
    assert compat_openhands.make_fleet_workspace is oh_fleet.make_fleet_workspace


# --- a program with only its imports changed, against the fakes ----------------


@pytest.fixture
def ate_env_from_environment(stack, monkeypatch, tmp_path):
    """The fakes with the caller token required, reached the way an
    agent-sandbox-rl program reaches ate-env: through the environment only.

    The fake splits gRPC and the port proxy over two ports, and its
    agent-server runs on this host; a real ate-env-api serves both on one
    listener, and the layer's working directory exists. Thus the workspace
    adapter gets those two values here, and nothing else: the token comes
    from the fleet.
    """
    api_url, state = stack
    state.api_token = TOKEN
    monkeypatch.setenv("SUBSTRATE_ENV_API", api_url)
    monkeypatch.setenv("SUBSTRATE_ENV_AUTH_TOKEN", TOKEN)
    monkeypatch.setenv("SUBSTRATE_ENV_TEMPLATE", "oh-agent-server")
    monkeypatch.setenv("AGENT_SERVER_IMAGE", IMAGE)
    monkeypatch.setenv("SANDBOX_SESSION_KEY", SESSION_KEY)
    monkeypatch.setenv("N_CONVERSATIONS", "4")
    monkeypatch.setenv("MAX_CONCURRENT", "2")
    monkeypatch.delenv("LLM_API_KEY", raising=False)

    def fake_topology(handle, **kwargs):
        assert "api_token" not in kwargs
        return oh_fleet.make_handle_workspace(
            handle, proxy_url=state.proxy_url, working_dir=str(tmp_path / handle.task.id),
            health_check_timeout=15, **kwargs)

    monkeypatch.setattr(compat_openhands, "make_handle_workspace",
                        functools.wraps(oh_fleet.make_handle_workspace)(fake_topology))
    return state


def _check_smoke_run(results, state):
    assert sorted(r["id"] for r in results) == [f"conv-{i}" for i in range(4)]
    for r in results:
        assert (r["mode"], r["cluster"], r["exit_code"]) == ("smoke", "default", 0)
        assert r["output"].startswith("ready")
    assert state.creates and {c["template"] for c in state.creates} == {"oh-agent-server"}
    assert {c["rootfs_image"] for c in state.creates} == {IMAGE}
    assert {c["atespace"] for c in state.creates} == {DEFAULT_ATESPACE}
    assert state.envs == {}  # each handle released, the warm pool torn down


def test_an_agent_sandbox_rl_program_runs_with_only_its_imports_changed(ate_env_from_environment):
    results = runpy.run_path(str(PROGRAM))["main"]()
    _check_smoke_run(results, ate_env_from_environment)


def test_the_upstream_example_runs_with_only_its_imports_changed(ate_env_from_environment,
                                                                  monkeypatch, tmp_path):
    """Runs agent-sandbox's run_openhands_fleet.py, import-swapped. Set
    AGENT_SANDBOX_RL_EXAMPLE to its path; skipped otherwise."""
    import os

    path = os.environ.get("AGENT_SANDBOX_RL_EXAMPLE")
    if not path:
        pytest.skip("set AGENT_SANDBOX_RL_EXAMPLE to agent-sandbox's run_openhands_fleet.py")
    upstream = Path(path).read_text()
    ported = upstream.replace("from agent_sandbox_rl", "from ate_env_openhands.agent_sandbox_rl")
    changed = [line for line in difflib.ndiff(upstream.splitlines(), ported.splitlines())
               if line.startswith(("- ", "+ "))]
    assert changed and all("import" in line for line in changed), changed
    script = tmp_path / "run_openhands_fleet.py"
    script.write_text(ported)
    module = runpy.run_path(str(script))
    out = io.StringIO()
    with redirect_stdout(out):
        module["main"]()
    report = json.loads(out.getvalue())
    assert report["conversations"] == 4
    _check_smoke_run(report["results"], ate_env_from_environment)
