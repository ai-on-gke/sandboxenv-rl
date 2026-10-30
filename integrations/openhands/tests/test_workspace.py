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
import logging
import os
import sys
import time
from types import SimpleNamespace

import grpc
import httpx
import pytest
from ate_env import EnvError, EnvironmentStatus, NotFoundError
from openhands.sdk.workspace import RemoteWorkspace

from ate_env_openhands import AteEnvWorkspace, port_capability, port_url

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


def test_cleanup_on_the_loop_thread_schedules_the_delete(stack, tmp_path):
    """__del__ can run on the loop thread (cyclic GC there); a blocking delete
    would wait for the loop that has to run it."""
    from ate_env_openhands import _runtime

    api_url, state = stack
    ws = make(api_url, state, tmp_path)
    env_id = ws.env_id

    async def on_loop():
        ws.cleanup()  # returns at once instead of waiting on itself
        return _runtime.on_loop_thread()

    assert _runtime.run(on_loop(), timeout=30) is True
    deadline = time.time() + 30
    while state.deletes != [env_id] and time.time() < deadline:
        time.sleep(0.05)
    assert state.deletes == [env_id]
    ws.cleanup()  # idempotent
    assert state.deletes == [env_id]


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


def test_port_capability_matches_the_proxy_vector():
    """The vector of ate-env-api's TestCapabilityVector, so the two stay in step."""
    assert port_capability("s3cret", "ate-env", "env1", 8000, 1700000000) == (
        "v1.1700000000.a2f01c6feececba10e18c15123f7f7124cd436d7ec370910c470f75325920f2f")


def _open_conversation(ws):
    pytest.importorskip("openhands.tools")
    from openhands.sdk import LLM, Conversation, RemoteConversation
    from openhands.tools.preset.default import get_default_agent
    from pydantic import SecretStr

    llm = LLM(usage_id="agent", model="openai/unused", api_key=SecretStr("x"),
              base_url="http://127.0.0.1:9")  # never called: the conversation is not run
    conversation = Conversation(agent=get_default_agent(llm=llm, cli_mode=True), workspace=ws)
    assert isinstance(conversation, RemoteConversation)
    return conversation


def _websocket_upgrades(state):
    return [p for p in state.proxied if p["headers"].get("Upgrade", "").lower() == "websocket"]


def test_with_the_api_token_the_event_websocket_carries_a_capability_only(stack, tmp_path,
                                                                         monkeypatch):
    """The SDK's event WebSocket sends no headers; the host's capability lets it through."""
    monkeypatch.setenv("OPENHANDS_REMOTE_WS_READY_TIMEOUT", "5")
    api_url, state = stack
    state.api_token = "api-secret"
    with make(api_url, state, tmp_path, api_token="api-secret") as ws:
        assert "ate-env-capability:v1." in ws.host and "api-secret" not in ws.host
        conversation = _open_conversation(ws)
        try:
            assert conversation.state.id
        finally:
            conversation.close()
    upgrades = _websocket_upgrades(state)
    assert upgrades, "the event stream went through the proxy as a WebSocket"
    for request in state.proxied:
        assert "api-secret" not in str(request["headers"])
        assert "ate-env-capability" not in str(request["headers"])
        assert not {"authorization", "x-env-authorization"} & {k.lower() for k in request["headers"]}


def test_an_expired_capability_does_not_open_the_websocket(stack, tmp_path, monkeypatch):
    """Without a valid capability the SDK cannot subscribe, and the conversation fails."""
    from openhands.sdk.conversation.exceptions import WebSocketConnectionError

    monkeypatch.setenv("OPENHANDS_REMOTE_WS_READY_TIMEOUT", "3")
    api_url, state = stack
    state.api_token = "api-secret"
    with make(api_url, state, tmp_path, api_token="api-secret", ws_capability_ttl=1) as ws:
        time.sleep(1.5)
        assert ws.execute_command("echo ok", timeout=30).stdout.strip() == "ok"  # the token
        with pytest.raises(WebSocketConnectionError):
            _open_conversation(ws)
    assert not _websocket_upgrades(state), "the proxy refused the expired capability"


def _capability_of(ws) -> str:
    return ws.host.split("ate-env-capability:", 1)[1].split("@", 1)[0]


def _is_upgrade(request) -> bool:
    return request["headers"].get("Upgrade", "").lower() == "websocket"


def test_rest_requests_never_carry_the_capability(stack, tmp_path, monkeypatch):
    """Only the event WebSocket sends the capability: REST requests, as the proxy
    receives them, carry no Authorization at all."""
    monkeypatch.setenv("OPENHANDS_REMOTE_WS_READY_TIMEOUT", "5")
    api_url, state = stack
    state.api_token = "api-secret"
    with make(api_url, state, tmp_path, api_token="api-secret") as ws:
        assert ws.execute_command("echo ok", timeout=30).stdout.strip() == "ok"
        assert ws.file_upload(b"x", str(tmp_path / "work" / "f")).success
        assert ws.alive
        conversation = _open_conversation(ws)
        try:
            assert conversation.state.id
        finally:
            conversation.close()
    rest = [r for r in state.received if not _is_upgrade(r)]
    upgrades = [r for r in state.received if _is_upgrade(r)]
    assert len(rest) >= 5 and upgrades
    for request in rest:
        assert "authorization" not in {k.lower() for k in request["headers"]}, request
        assert request["headers"].get("X-Env-Authorization") == "Bearer api-secret"
    for request in upgrades:
        assert request["headers"].get("Authorization", "").startswith("Basic ")


def test_repr_and_dumps_hide_the_token_and_the_capability(stack, tmp_path):
    api_url, state = stack
    state.api_token = "api-secret"
    with make(api_url, state, tmp_path, api_token="api-secret") as ws:
        capability = _capability_of(ws)
        for text in (repr(ws), str(ws), str(ws.model_dump()), ws.model_dump_json()):
            assert "api-secret" not in text
            assert capability not in text
        assert "ate-env-capability:***@" in repr(ws)
        assert "api_token" not in ws.model_dump()
        assert ws.model_dump()["host"].startswith("http://ate-env-capability:***@")


def test_errors_do_not_contain_the_capability(stack, tmp_path, no_finalizer):
    api_url, state = stack
    state.api_token = "api-secret"
    ws = make(api_url, state, tmp_path, api_token="api-secret")
    capability = _capability_of(ws)
    try:
        state.refuse = (500, "boom")
        with pytest.raises(httpx.HTTPStatusError) as status_error:
            ws._check_session_key()
        assert capability not in str(status_error.value)
        assert capability not in str(status_error.value.request.url)
        state.refuse = (400, "bad port")
        with pytest.raises(ValueError) as bad_request:
            ws._wait_for_health(timeout=5)
        assert capability not in str(bad_request.value)
        assert "ate-env-capability" not in str(bad_request.value)
    finally:
        state.refuse = None
        ws.cleanup()


def test_resume_renews_the_capability(stack, tmp_path, monkeypatch):
    """A pause longer than ws_capability_ttl: the resume renews the capability, thus
    a conversation reattached after it opens its event WebSocket."""
    monkeypatch.setenv("OPENHANDS_REMOTE_WS_READY_TIMEOUT", "5")
    api_url, state = stack
    state.api_token = "api-secret"
    with make(api_url, state, tmp_path, api_token="api-secret", ws_capability_ttl=1) as ws:
        before = _capability_of(ws)
        ws.pause()
        time.sleep(1.5)  # the first capability expires during the pause
        # The expiry is in whole seconds, thus a 1 s capability can expire before the
        # conversation opens its WebSocket. The renewed one has time for that.
        ws.ws_capability_ttl = 60
        ws.resume()
        assert _capability_of(ws) != before
        conversation = _open_conversation(ws)
        try:
            assert conversation.state.id
        finally:
            conversation.close()
    assert _websocket_upgrades(state)


def test_renewing_the_capability_keeps_a_live_conversation_working(stack, tmp_path,
                                                                  monkeypatch):
    """renew_ws_capability() changes only host's user info, which REST requests do not
    use, thus a live conversation keeps its HTTP client and can still send messages."""
    monkeypatch.setenv("OPENHANDS_REMOTE_WS_READY_TIMEOUT", "5")
    api_url, state = stack
    state.api_token = "api-secret"
    with make(api_url, state, tmp_path, api_token="api-secret") as ws:
        conversation = _open_conversation(ws)
        try:
            before = _capability_of(ws)
            time.sleep(1.1)  # a new expiry second gives a new capability
            ws.renew_ws_capability()
            assert _capability_of(ws) != before
            conversation.send_message("still connected")
        finally:
            conversation.close()


def test_capabilities_are_masked_in_logs(stack, tmp_path, caplog, monkeypatch):
    """httpx logs show no capability (REST URLs have none), and the websockets
    handshake log masks the Basic header."""
    monkeypatch.setenv("OPENHANDS_REMOTE_WS_READY_TIMEOUT", "5")
    api_url, state = stack
    state.api_token = "api-secret"
    caplog.set_level(logging.INFO, logger="httpx")
    caplog.set_level(logging.DEBUG, logger="websockets.client")
    with make(api_url, state, tmp_path, api_token="api-secret") as ws:
        capability = _capability_of(ws)
        ws.execute_command("echo ok", timeout=30)
        conversation = _open_conversation(ws)
        conversation.close()
    assert "HTTP Request" in caplog.text
    assert capability not in caplog.text
    assert "YXRlLWVudi1jYXBhYmlsaXR5" not in caplog.text  # base64 of the capability user


def test_the_log_filter_covers_child_loggers():
    """A filter on "httpcore" would miss "httpcore.http11"; each logger is named."""
    from ate_env_openhands import workspace as wsmod

    wsmod._redact_capabilities_in_logs()
    record = logging.LogRecord("httpcore.http11", logging.DEBUG, __file__, 1,
                               "send_request url=%s", ("http://ate-env-capability:v1.9.ab@h/x",), None)
    for name in ("httpcore.http11", "httpcore.connection", "websockets.client"):
        assert wsmod._redact_filter in logging.getLogger(name).filters
    wsmod._redact_filter.filter(record)
    assert "v1.9.ab" not in record.getMessage()
    header = logging.LogRecord("websockets.client", logging.DEBUG, __file__, 1, "> %s: %s",
                               ("Authorization", "Basic YXRlLWVudi1jYXBhYmlsaXR5OnYxLjEuYQ=="), None)
    wsmod._redact_filter.filter(header)
    assert header.getMessage() == "> Authorization: ***"


def test_capability_ttl_stays_below_the_proxy_maximum(stack, tmp_path):
    from ate_env_openhands.workspace import CAPABILITY_CLOCK_MARGIN, MAX_CAPABILITY_TTL

    api_url, state = stack
    state.api_token = "api-secret"
    with pytest.raises(ValueError):
        make(api_url, state, tmp_path, api_token="api-secret",
             ws_capability_ttl=MAX_CAPABILITY_TTL)
    longest = MAX_CAPABILITY_TTL - CAPABILITY_CLOCK_MARGIN
    with make(api_url, state, tmp_path, api_token="api-secret", ws_capability_ttl=longest) as ws:
        expiry = int(_capability_of(ws).split(".")[1])
        assert expiry <= time.time() + longest + 1


def test_get_secrets_urls_carry_no_capability(monkeypatch, no_finalizer):
    from openhands.sdk.secret import LookupSecret

    url = "http://ate-env-capability:v1.9.ab@127.0.0.1:7777/v1alpha/atespaces/a/envs/e/ports/8000"
    monkeypatch.setattr(RemoteWorkspace, "get_secrets", lambda self, names=None, **kw: {
        "GH": LookupSecret(url=f"{url}/api/settings/secrets/GH", headers={})})
    monkeypatch.setattr(AteEnvWorkspace, "model_post_init", lambda self, context: None)
    ws = AteEnvWorkspace.model_construct(host=url)
    secrets = ws.get_secrets()
    # Only the property: the URL has no user info. Its host can change (the proxy here,
    # the agent-server's loopback address with the in-process secret lookup).
    assert "@" not in secrets["GH"].url
    assert "ate-env-capability" not in secrets["GH"].url
    assert secrets["GH"].url.endswith("/api/settings/secrets/GH")


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


# ate-env-api's port proxy answers a crashed environment with 410 Gone and
# X-Env-State: CRASHED; waiting cannot fix it.
CRASHED = (410, "environment ate-env/x port 8000 has crashed (actor state CRASHED); "
                "delete it and create a new one", {"X-Env-State": "CRASHED"})


def test_health_stops_at_once_on_a_crashed_environment(no_finalizer, stack, tmp_path):
    api_url, state = stack
    state.refuse = CRASHED
    _fails_fast(RuntimeError, r"has crashed and will not recover \(HTTP 410.*delete it and create a new one",
                api_url=api_url, state=state, tmp_path=tmp_path)
    assert state.deletes == [state.creates[0]["id"]]


@pytest.mark.parametrize("answer", [
    (502, "connection refused", {"X-Env-State": "CRASHED"}),  # the header decides, any status
    (410, "gone", {}),  # 410 alone is final too
])
def test_health_stops_at_once_on_any_crashed_answer(no_finalizer, stack, tmp_path, answer):
    api_url, state = stack
    state.refuse = answer
    _fails_fast(RuntimeError, "has crashed", api_url=api_url, state=state, tmp_path=tmp_path)


def test_health_stops_at_once_on_a_deleting_environment(no_finalizer, stack, tmp_path):
    api_url, state = stack
    state.refuse = (404, "environment is being deleted (actor state DELETING)", {"X-Env-State": "DELETING"})
    _fails_fast(RuntimeError, "is being deleted", api_url=api_url, state=state, tmp_path=tmp_path)


def test_resume_stops_at_once_when_the_environment_crashed(stack, tmp_path):
    api_url, state = stack
    with make(api_url, state, tmp_path) as ws:
        state.refuse = CRASHED
        t0 = time.monotonic()
        with pytest.raises(RuntimeError, match="has crashed"):
            ws.resume()
        assert time.monotonic() - t0 < 5


# What an ate-env-api without the port-proxy rewrite passes through for a
# crashed environment: the Substrate router's 503, with no X-Env-State.
ROUTER_503 = (503, "actor ate-env/x unavailable: AssignWorker prerequisite not met for Actor: "
                   "ate-env/x (got: ACTOR_STATE_CRASHED, want ACTOR_STATE_SUSPENDED)")


@pytest.mark.parametrize("status,match", [
    (EnvironmentStatus.CRASHED, r"has crashed and will not recover \(state CRASHED.*delete it and create a new one"),
    (EnvironmentStatus.DELETING, r"is being deleted \(state DELETING"),
])
def test_health_reads_the_state_behind_a_proxy_without_the_rewrite(no_finalizer, stack, tmp_path,
                                                                   status, match):
    api_url, state = stack
    state.new_status = int(status)
    state.refuse = ROUTER_503
    _fails_fast(RuntimeError, match, api_url=api_url, state=state, tmp_path=tmp_path)
    assert state.refusals == 1, "the state check stopped the wait after the first 503"
    assert state.deletes == [state.creates[0]["id"]]


@pytest.mark.parametrize("wait,reads", [(4, 1), (7, 2)])
def test_health_state_checks_are_spaced(no_finalizer, stack, tmp_path, wait, reads):
    api_url, state = stack
    state.refuse = ROUTER_503  # the environment itself stays RUNNING
    with pytest.raises(RuntimeError, match="did not answer /health"):
        make(api_url, state, tmp_path, health_check_timeout=wait)
    assert state.refusals >= 3
    assert state.gets == reads, f"{state.gets} state reads in a {wait} s wait, want {reads} (one each 5 s)"


def test_a_stalled_existence_check_cannot_hold_the_wait(no_finalizer, stack, tmp_path):
    api_url, state = stack
    state.refuse = (404, "no such page")  # a 404 with no X-Env-State: the wait asks whether the env exists
    state.get_delay = 40.0  # a control plane that does not answer
    t0 = time.monotonic()
    with pytest.raises(RuntimeError, match="did not answer /health"):
        make(api_url, state, tmp_path, health_check_timeout=3)
    assert time.monotonic() - t0 < 10, "each read ends by the wait's deadline, not after 30 s"
    assert state.gets >= 1


def test_resume_reads_the_state_behind_a_proxy_without_the_rewrite(stack, tmp_path):
    api_url, state = stack
    with make(api_url, state, tmp_path) as ws:
        state.envs[ws.env_id]["status"] = int(EnvironmentStatus.CRASHED)
        state.refuse = ROUTER_503
        t0 = time.monotonic()
        with pytest.raises(RuntimeError, match="has crashed"):
            ws.resume()
        assert time.monotonic() - t0 < 5


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


def test_a_conversation_survives_pause_and_resume(stack, tmp_path):
    """A live RemoteConversation holds the workspace's HTTP client; pause() and
    resume() keep that client open, thus the conversation works afterwards."""
    pytest.importorskip("openhands.tools")
    from openhands.sdk import LLM, Conversation
    from openhands.tools.preset.default import get_default_agent
    from pydantic import SecretStr

    api_url, state = stack
    llm = LLM(usage_id="agent", model="openai/unused", api_key=SecretStr("x"),
              base_url="http://127.0.0.1:9")  # never called: the conversation is not run
    with make(api_url, state, tmp_path) as ws:
        conversation = Conversation(agent=get_default_agent(llm=llm, cli_mode=True), workspace=ws)
        try:
            client = ws.client
            ws.pause()
            ws.resume()
            assert ws.client is client and not client.is_closed
            before = len(state.proxied)
            conversation.send_message("still connected after resume")
            events = f"/api/conversations/{conversation.state.id}/events"
            sent = [p for p in state.proxied[before:]
                    if p["method"] == "POST" and p["path"].split("?")[0] == events]
            assert sent, "the message after the resume reached the agent-server"
        finally:
            conversation.close()
        target = tmp_path / "work" / "after-resume.txt"
        ws.execute_command(f"echo ok > {target}", timeout=30)
        assert ws.execute_command(f"cat {target}", timeout=30).stdout.strip() == "ok"
    assert client.is_closed, "cleanup() closes the client that pause and resume kept"


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
