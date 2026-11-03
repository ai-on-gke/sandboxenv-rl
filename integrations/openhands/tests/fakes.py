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

"""Fakes for the workspace tests: ate-env-api's environment service over gRPC,
and its port proxy over HTTP in front of a real agent-server on localhost.

The fake proxy keeps the router's behaviour that the workspace relies on: an
unknown environment is a 404, a suspended one is woken by the request, the
routing prefix is stripped, and HTTP upgrades are relayed both ways. It also
keeps ate-env-api's own answers: 400 for port 80 (the guest) and a port out
of range, and, with ``api_token``, 401 for a request without the bearer in
X-Env-Authorization or Authorization, which it removes before it forwards. A
WebSocket upgrade can instead carry a port capability as ``Authorization:
Basic`` with the user ``ate-env-capability``; the proxy checks it for the
environment port and never forwards it.
"""

from __future__ import annotations

import asyncio
import base64
import contextlib
import hashlib
import hmac
import http.client
import re
import socket
import threading
import time
from collections.abc import Iterator
from dataclasses import dataclass, field
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

import grpc
import grpc.aio
from ate_env import EnvironmentStatus
from ate_env._gen.ateenv.v1alpha import env_pb2, env_pb2_grpc
from ate_env.loop_runner import LoopRunner

DEFAULT_ATESPACE = "ate-env"
DEFAULT_TEMPLATE = "default-template"


@dataclass
class FakeAteEnv:
    """State shared by the fake services, plus the knobs tests script."""

    envs: dict[str, dict] = field(default_factory=dict)
    creates: list[dict] = field(default_factory=list)
    deletes: list[str] = field(default_factory=list)
    suspends: list[str] = field(default_factory=list)
    create_code: grpc.StatusCode | None = None
    create_but_fail: bool = False  # record the env, then fail the call
    upstreams: dict[int, str] = field(default_factory=dict)  # port -> host:port
    unhealthy_polls: int = 0  # answer this many /health requests with 503 first
    new_status: int | None = None  # status of a created environment; RUNNING when unset
    gets: int = 0  # GetEnvironment calls
    get_delay: float = 0.0  # GetEnvironment answers this many seconds late
    api_token: str | None = None  # the proxy requires this bearer when set
    # answer every proxy request with this: (status, body) or (status, body, headers)
    refuse: tuple[int, str] | tuple[int, str, dict[str, str]] | None = None
    refusals: int = 0  # proxy requests answered with refuse
    create_lands_after: float | None = None  # create succeeds this late, after the client gave up
    wakes: int = 0
    proxied: list[dict] = field(default_factory=list)
    received: list[dict] = field(default_factory=list)  # each request as the proxy got it
    lock: threading.Lock = field(default_factory=threading.Lock)

    def env_pb(self, env_id: str) -> env_pb2.Environment:
        e = self.envs[env_id]
        return env_pb2.Environment(
            id=env_id,
            atespace=e["atespace"],
            template=env_pb2.Template(name=e["template"], atespace=e["template_atespace"]),
            status=e["status"],
        )


class FakeEnvironmentService(env_pb2_grpc.EnvironmentServiceServicer):
    def __init__(self, state: FakeAteEnv) -> None:
        self.state = state

    async def CreateEnvironment(self, request, context):
        s = self.state
        s.creates.append({
            "id": request.id,
            "atespace": request.atespace,
            "template": request.template.name,
            "template_atespace": request.template.atespace,
            "rootfs_image": request.rootfs_image,
        })
        if s.create_code is not None:
            await context.abort(s.create_code, "create refused")
        if s.create_lands_after is not None:
            # The server goes on with the create after the caller's deadline.
            asyncio.get_running_loop().call_later(
                s.create_lands_after, self._register, request)
            await asyncio.sleep(s.create_lands_after + 30)
        if request.id in s.envs:
            await context.abort(grpc.StatusCode.ALREADY_EXISTS, "exists")
        self._register(request)
        if s.create_but_fail:
            await context.abort(grpc.StatusCode.DEADLINE_EXCEEDED, "lost the answer")
        return env_pb2.CreateEnvironmentResponse(environment=s.env_pb(request.id))

    def _register(self, request) -> None:
        self.state.envs[request.id] = {
            "atespace": request.atespace or DEFAULT_ATESPACE,
            "template": request.template.name or DEFAULT_TEMPLATE,
            "template_atespace": request.template.atespace or DEFAULT_ATESPACE,
            "status": self.state.new_status if self.state.new_status is not None
            else int(EnvironmentStatus.RUNNING),
        }

    async def GetEnvironment(self, request, context):
        self.state.gets += 1
        if self.state.get_delay:
            await asyncio.sleep(self.state.get_delay)
        if request.id not in self.state.envs:
            await context.abort(grpc.StatusCode.NOT_FOUND, "no such env")
        return env_pb2.GetEnvironmentResponse(environment=self.state.env_pb(request.id))

    async def SuspendEnvironment(self, request, context):
        if request.id not in self.state.envs:
            await context.abort(grpc.StatusCode.NOT_FOUND, "no such env")
        self.state.suspends.append(request.id)
        self.state.envs[request.id]["status"] = int(EnvironmentStatus.SUSPENDED)
        return env_pb2.SuspendEnvironmentResponse()

    async def DeleteEnvironment(self, request, context):
        self.state.deletes.append(request.id)
        if self.state.envs.pop(request.id, None) is None:
            await context.abort(grpc.StatusCode.NOT_FOUND, "no such env")
        return env_pb2.DeleteEnvironmentResponse()


_PATH = re.compile(r"^/v1alpha/(?:atespaces/(?P<a>[^/]+)/)?envs/(?P<id>[^/]+)/ports/(?P<port>[0-9]+)(?P<rest>/.*)?$")
_HOP = {"connection", "keep-alive", "proxy-connection", "transfer-encoding", "upgrade", "host", "te", "trailer"}


def _relay(a: socket.socket, b: socket.socket) -> None:
    try:
        while data := a.recv(65536):
            b.sendall(data)
    except OSError:
        pass
    finally:
        with contextlib.suppress(OSError):
            b.shutdown(socket.SHUT_WR)


def _capability(authorization: str | None) -> str | None:
    """The port capability in an ``Authorization: Basic`` value, if any."""
    scheme, _, payload = (authorization or "").strip().partition(" ")
    if scheme.lower() != "basic":
        return None
    try:
        user, _, capability = base64.b64decode(payload.strip()).decode().partition(":")
    except (ValueError, UnicodeDecodeError):
        return None
    return capability if user == "ate-env-capability" else None


def _capability_ok(token: str, capability: str, atespace: str, env_id: str, port: int) -> bool:
    """Checks a port capability the way ate-env-api's port proxy does."""
    version, _, rest = capability.partition(".")
    expiry, _, mac = rest.partition(".")
    if version != "v1" or not expiry.isdigit():
        return False
    left = int(expiry) - time.time()
    if left <= 0 or left > 7 * 24 * 3600:
        return False
    msg = f"ate-env port capability v1\n{atespace}\n{env_id}\n{port}\n{expiry}".encode()
    want = hmac.new(token.encode(), msg, hashlib.sha256).hexdigest()
    return hmac.compare_digest(mac, want)


def _handler(state: FakeAteEnv):
    class Handler(BaseHTTPRequestHandler):
        protocol_version = "HTTP/1.1"

        def log_message(self, *args) -> None:
            pass

        def _route(self):
            """(upstream, rest, headers to drop, record) for a request, or None once answered."""
            with state.lock:
                state.received.append({"method": self.command, "path": self.path,
                                       "headers": dict(self.headers.items())})
            if state.refuse is not None:
                state.refusals += 1
                self._reply(*state.refuse)
                return None
            path, _, query = self.path.partition("?")
            m = _PATH.match(path)
            if not m:
                self._reply(404, "not a port-proxy path")
                return None
            port = int(m["port"])
            if port == 80:
                self._reply(400, "port 80 is the ate-env guest, which ate-env-api serves itself")
                return None
            if port > 65535:
                self._reply(400, f"port {m['port']!r} is not a TCP port (1-65535)")
                return None
            drop = {"x-env-authorization"}
            env_id, atespace = m["id"], m["a"] or DEFAULT_ATESPACE
            capability = _capability(self.headers.get("Authorization"))
            if capability is not None:
                drop.add("authorization")  # a capability is never the daemon's
            if state.api_token is not None:
                want = f"Bearer {state.api_token}"
                given = self.headers.get("X-Env-Authorization")
                if given is not None:
                    ok = given == want
                elif capability is not None:
                    ok = (self.headers.get("Upgrade", "").lower() == "websocket"
                          and _capability_ok(state.api_token, capability, atespace, env_id, port))
                else:
                    drop.add("authorization")
                    ok = self.headers.get("Authorization") == want
                if not ok:
                    self._reply(401, "missing or invalid bearer token; send it in "
                                     "X-Env-Authorization or Authorization")
                    return None
            with state.lock:
                env = state.envs.get(env_id)
                if env is None or env["atespace"] != atespace:
                    self._reply(404, f"no actor {env_id}")
                    return None
                if env["status"] == int(EnvironmentStatus.SUSPENDED):
                    env["status"] = int(EnvironmentStatus.RUNNING)
                    state.wakes += 1
                rest = (m["rest"] or "/") + (f"?{query}" if query else "")
                if rest.startswith("/health") and state.unhealthy_polls > 0:
                    state.unhealthy_polls -= 1
                    self._reply(503, f"actor {env_id} is not running")
                    return None
                upstream = state.upstreams.get(port)
                # What the daemon sees: the headers after the proxy's removals.
                forwarded = {k: v for k, v in self.headers.items() if k.lower() not in drop}
                record = {"method": self.command, "path": rest, "headers": forwarded, "body": b""}
                state.proxied.append(record)
            if upstream is None:
                self._reply(502, "connection refused")
                return None
            return upstream, rest, drop, record

        def _reply(self, code: int, text: str, headers: dict[str, str] | None = None) -> None:
            body = text.encode()
            self.send_response(code)
            for k, v in (headers or {}).items():
                self.send_header(k, v)
            self.send_header("Content-Length", str(len(body)))
            self.end_headers()
            self.wfile.write(body)

        def _forward(self) -> None:
            routed = self._route()
            if routed is None:
                return
            upstream, rest, drop, record = routed
            if self.headers.get("Upgrade"):
                self._upgrade(upstream, rest, drop)
                return
            length = int(self.headers.get("Content-Length") or 0)
            body = self.rfile.read(length) if length else None
            record["body"] = body or b""
            headers = {k: v for k, v in self.headers.items() if k.lower() not in _HOP | drop}
            conn = http.client.HTTPConnection(upstream, timeout=120)
            try:
                conn.request(self.command, rest, body=body, headers=headers)
                resp = conn.getresponse()
                data = resp.read()
            finally:
                conn.close()
            self.send_response(resp.status)
            for k, v in resp.getheaders():
                if k.lower() not in _HOP and k.lower() != "content-length":
                    self.send_header(k, v)
            self.send_header("Content-Length", str(len(data)))
            self.end_headers()
            self.wfile.write(data)

        def _upgrade(self, upstream: str, rest: str, drop: set[str]) -> None:
            host, port = upstream.rsplit(":", 1)
            up = socket.create_connection((host, int(port)), timeout=30)
            lines = [f"{self.command} {rest} HTTP/1.1", f"Host: {upstream}"]
            lines += [f"{k}: {v}" for k, v in self.headers.items() if k.lower() not in {"host"} | drop]
            up.sendall(("\r\n".join(lines) + "\r\n\r\n").encode())
            up.settimeout(None)
            self.connection.settimeout(None)
            t = threading.Thread(target=_relay, args=(up, self.connection), daemon=True)
            t.start()
            _relay(self.connection, up)
            t.join(timeout=5)
            up.close()
            self.close_connection = True

        do_GET = do_POST = do_PUT = do_DELETE = do_PATCH = do_HEAD = _forward

    return Handler


@contextlib.contextmanager
def serve(state: FakeAteEnv | None = None) -> Iterator[tuple[str, FakeAteEnv]]:
    """Run the fake API (gRPC) and port proxy (HTTP) on one loopback port each.

    Yields ``(api_url, state)``; the proxy URL is ``state.proxy_url``.
    """
    state = state or FakeAteEnv()
    runner = LoopRunner(name="fake-ate-env")

    async def start():
        server = grpc.aio.server()
        env_pb2_grpc.add_EnvironmentServiceServicer_to_server(FakeEnvironmentService(state), server)
        port = server.add_insecure_port("127.0.0.1:0")
        await server.start()
        return server, port

    server, grpc_port = runner.run(start())
    proxy = ThreadingHTTPServer(("127.0.0.1", 0), _handler(state))
    proxy.daemon_threads = True
    threading.Thread(target=proxy.serve_forever, daemon=True).start()
    state.proxy_url = f"http://127.0.0.1:{proxy.server_address[1]}"  # type: ignore[attr-defined]
    try:
        yield f"127.0.0.1:{grpc_port}", state
    finally:
        proxy.shutdown()
        proxy.server_close()
        runner.run(server.stop(None))
        runner.stop()
