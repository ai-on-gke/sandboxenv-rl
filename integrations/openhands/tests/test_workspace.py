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

"""AteEnvWorkspace against a fake ate-env-api and port proxy, and a real agent-server."""

from __future__ import annotations

import gc
import os
import sys
import time
from types import SimpleNamespace

import grpc
import httpx
import pytest
from ate_env import EnvError, NotFoundError
from openhands.sdk.workspace import RemoteWorkspace

from ate_env_openhands import AteEnvWorkspace, port_url

from .conftest import IMAGE, SESSION_KEY
from .fakes import DEFAULT_ATESPACE, FakeAteEnv, serve


def make(api_url, state, tmp_path, **kw) -> AteEnvWorkspace:
    args = dict(
        template="oh-agent-server",
        image=IMAGE,
        api_url=api_url,
        proxy_url=state.proxy_url,
        api_key=SESSION_KEY,
        working_dir=str(tmp_path / "work"),
        health_check_timeout=15,
    )
    args.update(kw)
    return AteEnvWorkspace(**args)


@pytest.fixture
def no_finalizer(monkeypatch):
    """Stub __del__, so a test proves the explicit cleanup and not the
    finalizer that CPython runs as soon as the failed object is released."""
    monkeypatch.setattr(AteEnvWorkspace, "__del__", lambda self: None)


# -- validation, before any call ---------------------------------------------


def test_requires_template_or_env_id():
    with pytest.raises(ValueError, match="template is required"):
        AteEnvWorkspace(api_url="127.0.0.1:1")


def test_rejects_an_image_tag():
    with pytest.raises(ValueError, match="pinned by digest"):
        AteEnvWorkspace(template="t", image="python:3.12", api_url="127.0.0.1:1")


def test_rejects_an_image_when_attaching():
    with pytest.raises(ValueError, match="only when creating"):
        AteEnvWorkspace(env_id="e1", image=IMAGE, api_url="127.0.0.1:1")


def test_rejects_a_host():
    with pytest.raises(ValueError, match="host is set by the workspace"):
        AteEnvWorkspace(template="t", host="http://elsewhere", api_url="127.0.0.1:1")


def test_proxy_headers_carry_the_api_token_only():
    proxy_headers = AteEnvWorkspace.proxy_headers
    assert proxy_headers(SimpleNamespace(api_token="tok")) == {"X-Env-Authorization": "Bearer tok"}
    assert proxy_headers(SimpleNamespace(api_token=None)) == {}


def test_is_a_remote_workspace():
    assert issubclass(AteEnvWorkspace, RemoteWorkspace)
    for name in ("template", "image", "atespace", "env_id", "server_port",
                 "health_check_timeout", "delete_on_exit", "working_dir", "api_key"):
        assert name in AteEnvWorkspace.model_fields


def test_port_url_always_names_the_atespace():
    assert port_url("ate-env-api.ate-env:7777", "ate-env", "e1", 8000) == (
        "http://ate-env-api.ate-env:7777/v1alpha/atespaces/ate-env/envs/e1/ports/8000")
    assert port_url("https://env.example.com/", "team-a", "e1", 9100) == (
        "https://env.example.com/v1alpha/atespaces/team-a/envs/e1/ports/9100")


# -- lifecycle -------------------------------------------------------------------


def test_creates_runs_and_deletes_on_exit(stack, tmp_path):
    api_url, state = stack
    with make(api_url, state, tmp_path, template_atespace="ate-env") as ws:
        env_id = ws.env_id
        assert state.creates == [{
            "id": env_id, "atespace": DEFAULT_ATESPACE, "template": "oh-agent-server",
            "template_atespace": "ate-env", "rootfs_image": IMAGE,
        }]
        assert ws.host == port_url(state.proxy_url, DEFAULT_ATESPACE, env_id, 8000)
        result = ws.execute_command("echo hello", timeout=30)
        assert (result.exit_code, result.stdout.strip()) == (0, "hello")
        assert os.path.isdir(tmp_path / "work"), "working_dir is created"
        assert ws.alive
    assert state.deletes == [env_id]
    assert env_id not in state.envs


def test_files_round_trip(stack, tmp_path):
    api_url, state = stack
    with make(api_url, state, tmp_path) as ws:
        remote = str(tmp_path / "work" / "data.bin")
        up = ws.file_upload(b"\x00\x01payload", remote)
        assert up.success, up.error
        down = ws.file_download(remote, tmp_path / "out.bin")
        assert down.success, down.error
        assert (tmp_path / "out.bin").read_bytes() == b"\x00\x01payload"


def test_health_waits_through_a_booting_environment(stack, tmp_path):
    api_url, state = stack
    state.unhealthy_polls = 3
    with make(api_url, state, tmp_path) as ws:
        assert ws.execute_command("true", timeout=30).exit_code == 0
    assert state.unhealthy_polls == 0, "the three 503s were waited out"


def test_a_health_attempt_can_wait_for_the_whole_remaining_budget(stack, tmp_path,
                                                                  monkeypatch):
    api_url, state = stack
    import ate_env_openhands.workspace as wsmod

    seen: list = []
    real_get = httpx.get

    def recording_get(url, **kw):
        seen.append(kw["timeout"])
        return real_get(url, **kw)

    monkeypatch.setattr(wsmod.httpx, "get", recording_get)
    with make(api_url, state, tmp_path, health_check_timeout=100):
        pass
    assert seen, "the health wait made a request"
    # Not capped at 30 s: the proxy holds a request while the actor wakes.
    assert seen[0].read > 90 and seen[0].connect <= 10


def test_health_check_timeout_defaults_to_a_first_use_budget():
    assert AteEnvWorkspace.model_fields["health_check_timeout"].default >= 300


def test_health_timeout_deletes_the_environment(no_finalizer, tmp_path):
    with serve(FakeAteEnv()) as (api_url, state):  # nothing listens on the port
        with pytest.raises(RuntimeError, match="did not answer /health.*502"):
            make(api_url, state, tmp_path, health_check_timeout=1.5)
        assert len(state.creates) == 1
        assert state.deletes == [state.creates[0]["id"]]
        assert state.envs == {}


def test_a_lost_create_answer_still_deletes_by_name(no_finalizer, stack, tmp_path):
    api_url, state = stack
    state.create_but_fail = True
    with pytest.raises(Exception, match="DEADLINE_EXCEEDED|lost the answer"):
        make(api_url, state, tmp_path)
    assert state.deletes == [state.creates[0]["id"]]
    assert state.envs == {}


def test_a_wrong_session_key_fails_and_cleans_up(no_finalizer, stack, tmp_path):
    api_url, state = stack
    with pytest.raises(PermissionError, match="rejected the session key"):
        make(api_url, state, tmp_path, api_key="wrong")
    assert state.deletes == [state.creates[0]["id"]]


def test_a_proxy_refusal_in_the_key_check_blames_the_token(no_finalizer, stack, tmp_path,
                                                          monkeypatch):
    api_url, state = stack
    state.api_token = "tok"
    wait = AteEnvWorkspace._wait_for_health

    def wait_then_rotate(self, *, timeout):
        wait(self, timeout=timeout)
        state.api_token = "rotated"  # the token changes after the health wait

    monkeypatch.setattr(AteEnvWorkspace, "_wait_for_health", wait_then_rotate)
    with pytest.raises(PermissionError, match="port proxy refused") as ei:
        make(api_url, state, tmp_path, api_token="tok")
    assert "session key" not in str(ei.value)
    assert "HTTP 401" in str(ei.value)
    assert len(state.deletes) == 1, "the failed constructor deletes what it created"


def test_attach_neither_creates_nor_deletes(stack, tmp_path):
    api_url, state = stack
    owner = make(api_url, state, tmp_path, delete_on_exit=False)
    owner.cleanup()
    env_id = owner.env_id
    assert env_id in state.envs and state.deletes == []
    with AteEnvWorkspace(env_id=env_id, api_url=api_url, proxy_url=state.proxy_url,
                         api_key=SESSION_KEY, working_dir=str(tmp_path)) as ws:
        assert ws.execute_command("echo attached", timeout=30).stdout.strip() == "attached"
    assert len(state.creates) == 1
    assert state.deletes == []
    assert env_id in state.envs


def test_attach_to_an_unknown_environment_raises(stack, tmp_path):
    api_url, state = stack
    with pytest.raises(NotFoundError):
        AteEnvWorkspace(env_id="no-such-env", api_url=api_url, proxy_url=state.proxy_url)
    assert state.creates == [] and state.deletes == []


def test_pause_suspends_and_resume_wakes(stack, tmp_path):
    api_url, state = stack
    with make(api_url, state, tmp_path) as ws:
        marker = tmp_path / "work" / "marker"
        ws.execute_command(f"echo kept > {marker}", timeout=30)
        ws.pause()
        assert state.suspends == [ws.env_id]
        assert state.envs[ws.env_id]["status"] != 0
        ws.resume()
        assert state.wakes == 1
        assert ws.execute_command(f"cat {marker}", timeout=30).stdout.strip() == "kept"


def test_exit_cleans_up_even_when_the_parent_exit_raises(stack, tmp_path, monkeypatch):
    api_url, state = stack
    ws = make(api_url, state, tmp_path)

    def boom(self, *args):
        raise RuntimeError("callback failed")

    monkeypatch.setattr(RemoteWorkspace, "__exit__", boom)
    with pytest.raises(RuntimeError, match="callback failed"):
        ws.__exit__(None, None, None)
    assert state.deletes == [ws.env_id]
    ws.cleanup()  # idempotent
    assert state.deletes == [ws.env_id]


def test_the_api_token_authenticates_at_the_proxy_and_never_reaches_the_sandbox(stack, tmp_path):
    api_url, state = stack
    state.api_token = "api-secret"
    with make(api_url, state, tmp_path, api_token="api-secret") as ws:
        assert ws.execute_command("echo ok", timeout=30).stdout.strip() == "ok"
        up = ws.file_upload(b"x", str(tmp_path / "work" / "f"))
        assert up.success, up.error
    assert state.proxied, "requests went through the proxy"
    for request in state.proxied:
        assert "api-secret" not in str(request["headers"])
        assert not {"authorization", "x-env-authorization"} & {k.lower() for k in request["headers"]}


# -- answers that waiting cannot change ------------------------------------------


def _fails_fast(exc_type, match, **kw):
    t0 = time.monotonic()
    with pytest.raises(exc_type, match=match):
        make(**kw, health_check_timeout=30)
    assert time.monotonic() - t0 < 5, "the health wait stopped at once"


def test_health_stops_at_once_when_the_proxy_wants_a_token(no_finalizer, stack, tmp_path):
    api_url, state = stack
    state.api_token = "api-secret"
    _fails_fast(PermissionError, "refused the request.*401",
                api_url=api_url, state=state, tmp_path=tmp_path)
    assert state.deletes == [state.creates[0]["id"]]


def test_health_stops_at_once_on_a_wrong_token(no_finalizer, stack, tmp_path):
    api_url, state = stack
    state.api_token = "api-secret"
    _fails_fast(PermissionError, "401", api_url=api_url, state=state, tmp_path=tmp_path,
                api_token="wrong")
    assert state.deletes == [state.creates[0]["id"]]


def test_health_stops_at_once_on_a_forbidden_request(no_finalizer, stack, tmp_path):
    api_url, state = stack
    state.refuse = (403, "cross-origin request refused")
    _fails_fast(PermissionError, "403", api_url=api_url, state=state, tmp_path=tmp_path)
    assert state.deletes == [state.creates[0]["id"]]


def test_health_stops_at_once_on_a_rejected_port(no_finalizer, stack, tmp_path):
    api_url, state = stack
    _fails_fast(ValueError, "port 80 is the ate-env guest",
                api_url=api_url, state=state, tmp_path=tmp_path, server_port=80)
    assert state.deletes == [state.creates[0]["id"]]


def test_resume_stops_at_once_when_the_environment_is_gone(stack, tmp_path):
    api_url, state = stack
    with make(api_url, state, tmp_path) as ws:
        state.envs.pop(ws.env_id)  # deleted by someone else
        t0 = time.monotonic()
        with pytest.raises(RuntimeError, match="no longer exists"):
            ws.resume()
        assert time.monotonic() - t0 < 5


# -- ownership -------------------------------------------------------------------


def test_a_validation_error_never_deletes_an_existing_environment(stack, tmp_path):
    api_url, state = stack
    owner = make(api_url, state, tmp_path, delete_on_exit=False)
    owner.cleanup()
    with pytest.raises(ValueError, match="only when creating"):
        AteEnvWorkspace(env_id=owner.env_id, image=IMAGE, delete_on_exit=True,
                        api_url=api_url, proxy_url=state.proxy_url)
    gc.collect()  # the failed object's __del__ runs here at the latest
    assert state.deletes == []
    assert owner.env_id in state.envs


def test_a_failed_attach_deletes_nothing(stack, tmp_path):
    api_url, state = stack
    with pytest.raises(NotFoundError):
        AteEnvWorkspace(env_id="no-such-env", delete_on_exit=True,
                        api_url=api_url, proxy_url=state.proxy_url)
    gc.collect()
    assert state.deletes == []


def test_a_copy_never_deletes_the_environment_or_closes_the_client(stack, tmp_path):
    api_url, state = stack
    with make(api_url, state, tmp_path) as ws:
        client = ws.client
        copy = ws.model_copy()
        assert copy.client is not client, "the copy opens its own HTTP client"
        copy.cleanup()
        del copy
        gc.collect()
        assert state.deletes == []
        assert ws.client is client and not client.is_closed
        assert ws.execute_command("echo still", timeout=30).stdout.strip() == "still"
    assert state.deletes == [ws.env_id]


def test_a_workspace_in_a_reference_cycle_still_deletes_on_collection(stack, tmp_path):
    api_url, state = stack
    ws = make(api_url, state, tmp_path)
    env_id = ws.env_id
    # The cycle makes the workspace cyclic garbage: the collector, not the
    # reference count, finalizes it, and it clears weakrefs before __del__.
    holder: list = [ws]
    holder.append(holder)
    del ws, holder
    gc.collect()
    assert state.deletes == [env_id]


def test_a_copy_in_a_reference_cycle_never_deletes(stack, tmp_path):
    api_url, state = stack
    with make(api_url, state, tmp_path) as ws:
        holder: list = [ws.model_copy()]
        holder.append(holder)
        del holder
        gc.collect()
        assert state.deletes == []
    assert state.deletes == [ws.env_id]


def test_del_skips_cleanup_at_interpreter_shutdown(stack, tmp_path, monkeypatch):
    api_url, state = stack
    ws = make(api_url, state, tmp_path)
    monkeypatch.setattr(sys, "is_finalizing", lambda: True)
    ws.__del__()
    assert state.deletes == []
    monkeypatch.undo()
    ws.cleanup()
    assert state.deletes == [ws.env_id]


# -- failed creates --------------------------------------------------------------


def test_a_create_that_lands_after_the_timeout_is_still_deleted(no_finalizer, stack, tmp_path):
    api_url, state = stack
    state.create_lands_after = 1.5
    with pytest.raises(TimeoutError):
        make(api_url, state, tmp_path, create_timeout=0.3)
    env_id = state.creates[0]["id"]
    assert len(state.deletes) >= 2 and set(state.deletes) == {env_id}, (
        "the first delete found nothing and a retry deleted the late environment")
    assert state.envs == {}


@pytest.mark.parametrize("code", [grpc.StatusCode.ALREADY_EXISTS,
                                  grpc.StatusCode.INVALID_ARGUMENT,
                                  grpc.StatusCode.FAILED_PRECONDITION])
def test_a_refused_create_deletes_nothing(no_finalizer, stack, tmp_path, code):
    api_url, state = stack
    state.create_code = code
    with pytest.raises(EnvError):
        make(api_url, state, tmp_path)
    # A refused create made nothing, and an environment of the same name
    # (ALREADY_EXISTS) is not this workspace's to delete.
    assert state.deletes == []


def test_port_url_reaches_another_port(stack, tmp_path):
    api_url, state = stack
    with make(api_url, state, tmp_path) as ws:
        assert ws.port_url(9100) == port_url(state.proxy_url, DEFAULT_ATESPACE, ws.env_id, 9100)
        assert httpx.get(f"{ws.port_url(9100)}/x").status_code == 502  # nothing listens


def test_a_conversation_runs_through_the_port_proxy(stack, tmp_path):
    """RemoteConversation: REST calls and the event WebSocket through the path form."""
    pytest.importorskip("openhands.tools")
    from openhands.sdk import LLM, Conversation, RemoteConversation
    from openhands.tools.preset.default import get_default_agent
    from pydantic import SecretStr

    api_url, state = stack
    llm = LLM(usage_id="agent", model="openai/unused", api_key=SecretStr("x"),
              base_url="http://127.0.0.1:9")  # never called: the conversation is not run
    with make(api_url, state, tmp_path) as ws:
        conversation = Conversation(agent=get_default_agent(llm=llm, cli_mode=True), workspace=ws)
        try:
            assert isinstance(conversation, RemoteConversation)
            assert conversation.state.id
        finally:
            conversation.close()
    assert any(p["headers"].get("Upgrade", "").lower() == "websocket" for p in state.proxied), (
        "the event stream went through the proxy as a WebSocket")


def test_a_closed_conversation_lets_the_environment_stay_parked(stack, tmp_path):
    """The park pattern: close the conversation, pause, resume, reattach by id."""
    pytest.importorskip("openhands.tools")
    from openhands.sdk import LLM, Conversation
    from openhands.tools.preset.default import get_default_agent
    from pydantic import SecretStr

    api_url, state = stack
    llm = LLM(usage_id="agent", model="openai/unused", api_key=SecretStr("x"),
              base_url="http://127.0.0.1:9")  # never called: the conversation is not run
    agent = get_default_agent(llm=llm, cli_mode=True)
    with make(api_url, state, tmp_path) as ws:
        conversation = Conversation(agent=agent, workspace=ws)
        conversation_id = conversation.state.id
        conversation.close()  # stops the event WebSocket; the agent-server keeps the conversation
        ws.pause()
        wakes = state.wakes
        time.sleep(2.5)  # longer than the SDK's WebSocket reconnect delay
        assert state.wakes == wakes, "nothing woke the parked environment"
        ws.resume()
        again = Conversation(agent=agent, workspace=ws, conversation_id=conversation_id)
        try:
            assert again.state.id == conversation_id
        finally:
            again.close()
