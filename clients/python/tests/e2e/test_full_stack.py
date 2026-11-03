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

"""Full-stack integration tests against a real ate-env-api (cluster required).

Port-forward the API service first:

    kubectl -n ate-env port-forward svc/ate-env-api 17777:7777

then run:

    ATE_ENV_API_TARGET=127.0.0.1:17777 pytest tests/e2e/test_full_stack.py

Set SUBSTRATE_ENV_AUTH_TOKEN when the API requires caller authentication.

Unlike test_guest_daemon.py, this exercises the whole path: environment
lifecycle against the Substrate control plane, and guest operations proxied
by ate-env-api through the atenet router into the environment's actor.

Set ATE_ENV_ROOTFS_IMAGE to a digest-pinned image that has python3 (for example
docker.io/library/python:3.13-slim@sha256:...) to also run the create-from-image
test, which checks derived template creation, port proxy HTTP/WebSocket routing,
token/capability authentication, transparent wake, and template reuse.
"""

from __future__ import annotations

import asyncio
import base64
import hashlib
import hmac
import os
import socket
import time
import urllib.error
import urllib.request
import uuid

import pytest

from ate_env import (
    Client,
    EnvError,
    EnvironmentStatus,
    FailedPreconditionError,
    InvalidArgumentError,
    NotFoundError,
    ProcessState,
    Signal,
)

TARGET = os.environ.get("ATE_ENV_API_TARGET")
READY_TIMEOUT = float(os.environ.get("ATE_ENV_READY_TIMEOUT", "180"))
# Optional ActorTemplate override; the server default is "default-template".
TEMPLATE = os.environ.get("ATE_ENV_TEMPLATE")
# Optional digest-pinned rootfs image for the create-from-image test.
ROOTFS_IMAGE = os.environ.get("ATE_ENV_ROOTFS_IMAGE")
AUTH_TOKEN = os.environ.get("SUBSTRATE_ENV_AUTH_TOKEN")
EXPECTED_MARKER = os.environ.get("ATE_ENV_EXPECTED_MARKER")

pytestmark = pytest.mark.skipif(
    not TARGET,
    reason="set ATE_ENV_API_TARGET=host:port of a reachable ate-env-api",
)


@pytest.fixture
async def client():
    c = Client(TARGET, auth_token=AUTH_TOKEN)
    try:
        yield c
    finally:
        await c.close()


async def _wait_until_serving(env) -> None:
    """Retry a trivial shell until the environment's guest is reachable.

    Substrate resumes idle actors on demand, so traffic itself is the
    readiness gate; early requests can fail while the actor wakes up.
    """
    deadline = time.monotonic() + READY_TIMEOUT
    last_err: Exception | None = None
    while time.monotonic() < deadline:
        try:
            result = await env.shell("echo ready")
            if result.exit_code == 0 and result.stdout == "ready\n":
                return
        except EnvError as e:
            last_err = e
        await asyncio.sleep(2)
    raise AssertionError(f"environment {env.id} never became reachable: {last_err}")


async def _wait_for_status(client, env, want: EnvironmentStatus, timeout: float = 120) -> None:
    deadline = time.monotonic() + timeout
    status = None
    while time.monotonic() < deadline:
        status = (await env.info()).status
        if status == want:
            return
        await asyncio.sleep(2)
    raise AssertionError(f"environment {env.id} stuck in {status!r}, wanted {want!r}")


async def _wait_until_deleted(env, timeout: float = 60) -> None:
    """Poll env.info() until NotFoundError is returned."""
    deadline = time.monotonic() + timeout
    while time.monotonic() < deadline:
        try:
            await env.info()
        except NotFoundError:
            return
        await asyncio.sleep(2)
    pytest.fail(f"environment {env.id} still exists after delete")


def _mint_capability(
    token: str, atespace: str, env_id: str, port: int, expiry: int
) -> str:
    msg = f"ate-env port capability v1\n{atespace}\n{env_id}\n{port}\n{expiry}".encode()
    mac = hmac.new(token.encode(), msg, hashlib.sha256).hexdigest()
    return f"v1.{expiry}.{mac}"


def _capability_auth_header(cap: str) -> str:
    raw = f"ate-env-capability:{cap}".encode()
    return f"Basic {base64.b64encode(raw).decode()}"


def _http_get(
    target: str,
    path: str,
    auth_token: str | None = None,
    auth_header: str | None = None,
    timeout: float = 15.0,
) -> tuple[int, bytes]:
    url = f"http://{target}{path}"
    headers = {}
    if auth_header:
        headers["Authorization"] = auth_header
    elif auth_token:
        headers["Authorization"] = f"Bearer {auth_token}"
    req = urllib.request.Request(url, headers=headers)
    try:
        with urllib.request.urlopen(req, timeout=timeout) as resp:
            return resp.status, resp.read()
    except urllib.error.HTTPError as e:
        return e.code, e.read()


def _ws_connect(
    target: str, path: str, auth_header: str | None = None
) -> tuple[int, socket.socket]:
    host, _, port_str = target.rpartition(":")
    host = host.strip("[]") or "127.0.0.1"
    port = int(port_str) if port_str else 80
    s = socket.create_connection((host, port), timeout=10)
    try:
        key = base64.b64encode(os.urandom(16)).decode()
        headers = [
            f"GET {path} HTTP/1.1",
            f"Host: {target}",
            "Upgrade: websocket",
            "Connection: Upgrade",
            f"Sec-WebSocket-Key: {key}",
            "Sec-WebSocket-Version: 13",
        ]
        if auth_header:
            headers.append(f"Authorization: {auth_header}")
        headers.append("\r\n")
        s.sendall("\r\n".join(headers).encode())

        buf = bytearray()
        while b"\r\n\r\n" not in buf:
            chunk = s.recv(4096)
            if not chunk:
                break
            buf.extend(chunk)
        header_part = bytes(buf).partition(b"\r\n\r\n")[0]
        first_line = header_part.split(b"\r\n")[0].decode("latin1")
        parts = first_line.split(" ", 2)
        status_code = int(parts[1]) if len(parts) > 1 else 0
        return status_code, s
    except BaseException:
        s.close()
        raise


def _recv_exact(sock: socket.socket, n: int) -> bytes:
    sock.settimeout(10)
    buf = bytearray()
    while len(buf) < n:
        chunk = sock.recv(n - len(buf))
        if not chunk:
            break
        buf.extend(chunk)
    return bytes(buf)


SERVER_CODE = """
import socket, threading, base64, hashlib

def handle(conn):
    try:
        buf = bytearray()
        while b"\\r\\n\\r\\n" not in buf:
            chunk = conn.recv(1024)
            if not chunk:
                return
            buf.extend(chunk)
        headers_raw, _, leftover = bytes(buf).partition(b"\\r\\n\\r\\n")
        lines = headers_raw.decode("latin1").split("\\r\\n")
        req_line = lines[0]
        headers = {}
        for line in lines[1:]:
            if not line:
                break
            k, _, v = line.partition(":")
            headers[k.strip().lower()] = v.strip()

        if "authorization" in headers or "x-env-authorization" in headers:
            conn.sendall(b"HTTP/1.1 500 Credential Leaked\\r\\nConnection: close\\r\\n\\r\\n")
            return

        if headers.get("upgrade", "").lower() == "websocket":
            key = headers.get("sec-websocket-key", "")
            magic = "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"
            accept = base64.b64encode(hashlib.sha1((key + magic).encode()).digest()).decode()
            resp = (
                f"HTTP/1.1 101 Switching Protocols\\r\\n"
                f"Upgrade: websocket\\r\\n"
                f"Connection: Upgrade\\r\\n"
                f"Sec-WebSocket-Accept: {accept}\\r\\n"
                f"\\r\\n"
            ).encode("latin1")
            conn.sendall(resp)
            if leftover:
                conn.sendall(leftover)
            while True:
                data = conn.recv(1024)
                if not data:
                    break
                conn.sendall(data)
            return

        if req_line.startswith("GET /hello"):
            conn.sendall(
                b"HTTP/1.1 200 OK\\r\\n"
                b"Content-Type: text/plain\\r\\n"
                b"Content-Length: 13\\r\\n"
                b"Connection: close\\r\\n"
                b"\\r\\n"
                b"port-proxy-ok"
            )
            return

        conn.sendall(b"HTTP/1.1 404 Not Found\\r\\nConnection: close\\r\\n\\r\\n")
    finally:
        try:
            conn.close()
        except Exception:
            pass

s = socket.socket(socket.AF_INET, socket.SOCK_STREAM)
s.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
s.bind(("0.0.0.0", 8888))
s.listen(128)
while True:
    conn, _ = s.accept()
    threading.Thread(target=handle, args=(conn,), daemon=True).start()
"""


async def test_get_missing_environment(client):
    with pytest.raises(NotFoundError):
        await client.get(f"pye2e-missing-{uuid.uuid4().hex[:8]}")


async def test_full_lifecycle(client):
    env_id = f"pye2e-{uuid.uuid4().hex[:8]}"
    env = await client.create(env_id, template_name=TEMPLATE)
    assert env.id == env_id
    assert env.atespace == "ate-env"

    try:
        info = await env.info()
        assert info.id == env_id
        assert isinstance(info.status, EnvironmentStatus)

        await _wait_until_serving(env)

        if EXPECTED_MARKER:
            assert (
                await env.shell("echo $E2E_TEMPLATE_MARKER")
            ).stdout == f"{EXPECTED_MARKER}\n"

        # Shell: output, stderr, exit codes.
        result = await env.shell("echo out; echo err >&2; exit 7")
        assert result.stdout == "out\n"
        assert result.stderr == "err\n"
        assert result.exit_code == 7

        # Files: binary roundtrip and NotFound mapping through the proxy.
        content = os.urandom(128 * 1024 + 3)
        path = f"/tmp/pye2e-{uuid.uuid4().hex[:8]}.bin"
        assert await env.write_file(path, content) == len(content)
        assert await env.read_file_bytes(path) == content
        with pytest.raises(NotFoundError):
            await env.read_file_bytes("/tmp/pye2e-does-not-exist")

        # Processes: follow-stream with exit, stdin, signals.
        proc = await env.start_process(["sh", "-c", "echo one; echo two >&2"])
        stdout = bytearray()
        stderr = bytearray()
        exit_info = None
        async for out in proc.output(follow=True):
            if out.stdout is not None:
                stdout.extend(out.stdout)
            elif out.stderr is not None:
                stderr.extend(out.stderr)
            else:
                exit_info = out.exit
        assert stdout == b"one\n"
        assert stderr == b"two\n"
        assert exit_info is not None and exit_info.state == ProcessState.EXITED
        assert exit_info.exit_code == 0

        cat = await env.start_process(["cat"], stdin=True)
        await cat.write_input(b"through the proxy\n", close=True)
        assert (await cat.wait()).exit_code == 0
        assert b"".join([o.stdout async for o in cat.output() if o.stdout]) == b"through the proxy\n"

        sleeper = await env.start_process(["sleep", "300"])
        await sleeper.signal(Signal.TERM)
        assert (await sleeper.wait()).exit_code == 143  # 128 + SIGTERM

        # Lifecycle: suspend checkpoints the environment.
        await env.suspend()
        await _wait_for_status(client, env, EnvironmentStatus.SUSPENDED)
    finally:
        try:
            await env.delete()
        except EnvError as e:
            pytest.fail(f"cleanup delete of {env_id} failed: {e}")

    # Deletion is reflected in the control plane.
    await _wait_until_deleted(env)


async def test_create_from_image_rejects_unpinned_image(client):
    # Validated before anything touches the control plane.
    with pytest.raises(InvalidArgumentError):
        await client.create(f"pye2e-img-{uuid.uuid4().hex[:8]}", rootfs_image="python:3.12-slim")


async def test_create_from_image_needs_a_base_template(client):
    missing = f"pye2e-nobase-{uuid.uuid4().hex[:8]}"
    with pytest.raises(FailedPreconditionError) as excinfo:
        await client.create(
            f"pye2e-img-{uuid.uuid4().hex[:8]}",
            template_name=missing,
            rootfs_image="docker.io/library/busybox@sha256:" + "0" * 64,
        )
    assert missing in str(excinfo.value)


@pytest.mark.skipif(
    not ROOTFS_IMAGE,
    reason="set ATE_ENV_ROOTFS_IMAGE=repo@sha256:... (an image with sh and python3) for the create-from-image test",
)
async def test_create_from_image(client):
    base = TEMPLATE or "default-template"
    want_template = f"{base}-{ROOTFS_IMAGE.split('@sha256:', 1)[1][:12]}"
    envs = []
    try:
        env = await client.create(
            f"pye2e-img-{uuid.uuid4().hex[:8]}", template_name=TEMPLATE, rootfs_image=ROOTFS_IMAGE
        )
        envs.append(env)

        # The derived template is reported at once, before the actor serves.
        info = await env.info()
        assert info.template is not None
        assert info.template.name == want_template
        assert info.template.atespace == env.atespace

        await _wait_until_serving(env)

        if EXPECTED_MARKER:
            assert (
                await env.shell("echo $E2E_TEMPLATE_MARKER")
            ).stdout == f"{EXPECTED_MARKER}\n"

        # The process runs in the rootfs image's rootfs, with the guest coming
        # from the image volume rather than from the image itself.
        result = await env.shell(
            "test -x /ate/ko-app/ate-env-guest && echo guest:volume; "
            "test -e /ko-app/ate-env-guest && echo guest:baked; "
            "test -r /etc/os-release && echo rootfs:ok"
        )
        assert result.exit_code == 0, result
        lines = result.stdout.splitlines()
        assert "guest:volume" in lines, result
        assert "guest:baked" not in lines, "the rootfs image should not carry the guest"
        assert "rootfs:ok" in lines, result

        # The workspace is writable and the guest file path works unchanged.
        content = os.urandom(64 * 1024 + 1)
        path = f"/tmp/pye2e-img-{uuid.uuid4().hex[:8]}.bin"
        assert await env.write_file(path, content) == len(content)
        assert await env.read_file_bytes(path) == content

        # Port proxy: verify port 80 is rejected.
        code, _ = await asyncio.to_thread(
            _http_get,
            TARGET,
            f"/v1alpha/atespaces/{env.atespace}/envs/{env.id}/ports/80/",
            auth_token=AUTH_TOKEN,
        )
        assert code == 400

        # Start in-sandbox TCP server on port 8888.
        server_proc = await env.start_process(["python3", "-u", "-c", SERVER_CODE])

        # Poll until the port proxy forwards to the server.
        deadline = time.monotonic() + 20
        code, body = 0, b""
        while time.monotonic() < deadline:
            try:
                code, body = await asyncio.to_thread(
                    _http_get,
                    TARGET,
                    f"/v1alpha/atespaces/{env.atespace}/envs/{env.id}/ports/8888/hello",
                    auth_token=AUTH_TOKEN,
                    timeout=5.0,
                )
            except (urllib.error.URLError, TimeoutError, ConnectionError):
                code, body = 0, b""
            if code == 200:
                break
            await asyncio.sleep(0.5)
        assert code == 200
        assert body == b"port-proxy-ok"

        # WebSocket capability authentication and echo over hijacked stream.
        if AUTH_TOKEN:
            expiry = int(time.time()) + 3600
            cap = _mint_capability(AUTH_TOKEN, env.atespace, env.id, 8888, expiry)
            plain_code, _ = await asyncio.to_thread(
                _http_get,
                TARGET,
                f"/v1alpha/atespaces/{env.atespace}/envs/{env.id}/ports/8888/hello",
                auth_header=_capability_auth_header(cap),
            )
            assert plain_code == 401

            ws_code, ws = await asyncio.to_thread(
                _ws_connect,
                TARGET,
                f"/v1alpha/atespaces/{env.atespace}/envs/{env.id}/ports/8888/",
                auth_header=_capability_auth_header(cap),
            )
            try:
                assert ws_code == 101
                await asyncio.to_thread(ws.sendall, b"ping-ws\n")
                echo = await asyncio.to_thread(_recv_exact, ws, len(b"ping-ws\n"))
                assert echo == b"ping-ws\n"
            finally:
                ws.close()

            bad_cap = cap[:-1] + ("0" if cap[-1] != "0" else "1")
            bad_code, bad_ws = await asyncio.to_thread(
                _ws_connect,
                TARGET,
                f"/v1alpha/atespaces/{env.atespace}/envs/{env.id}/ports/8888/",
                auth_header=_capability_auth_header(bad_cap),
            )
            try:
                assert bad_code == 401
            finally:
                bad_ws.close()

        # Suspend and transparent wake-on-request through the port proxy.
        await env.suspend()
        await _wait_for_status(client, env, EnvironmentStatus.SUSPENDED)

        wake_code, wake_body = await asyncio.to_thread(
            _http_get,
            TARGET,
            f"/v1alpha/atespaces/{env.atespace}/envs/{env.id}/ports/8888/hello",
            auth_token=AUTH_TOKEN,
            timeout=READY_TIMEOUT,
        )
        assert wake_code == 200
        assert wake_body == b"port-proxy-ok"
        assert (await env.info()).status == EnvironmentStatus.RUNNING

        await server_proc.signal(Signal.TERM)
        await server_proc.wait()

        # A second environment on the same image reuses the derived template
        # instead of minting another one.
        env2 = await client.create(
            f"pye2e-img-{uuid.uuid4().hex[:8]}", template_name=TEMPLATE, rootfs_image=ROOTFS_IMAGE
        )
        envs.append(env2)
        assert (await env2.info()).template.name == want_template
        await _wait_until_serving(env2)
        assert (await env2.shell("echo second")).stdout == "second\n"
    finally:
        cleanup_errors = []
        for e in envs:
            try:
                await e.delete()
            except EnvError as exc:
                cleanup_errors.append(f"{e.id}: {exc}")
        if cleanup_errors:
            pytest.fail(f"cleanup delete failed: {', '.join(cleanup_errors)}")

    for e in envs:
        await _wait_until_deleted(e)
