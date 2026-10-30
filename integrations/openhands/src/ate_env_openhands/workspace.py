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

"""An OpenHands remote workspace on an ate-env environment.

The environment runs an unmodified task image. A base template mounts the
OpenHands agent-server into it as a runtime layer, and the guest starts the
agent-server next to itself (see this package's README and ``layer/``).
ate-env-api's port proxy forwards ``/v1alpha/atespaces/<a>/envs/<id>/ports/<p>/...``
to that port in the environment, so the proxy URL is the workspace host: the
SDK appends its ``/api/...`` paths to it, and ``RemoteConversation`` builds
its event WebSocket from it. With ``api_token``, each request carries it in
``X-Env-Authorization``, which ate-env-api checks and removes before it
forwards the request. The SDK opens the event WebSocket without extra headers
and drops the host's query string, but keeps its user info, which the
WebSocket client sends as ``Authorization: Basic``. Thus with ``api_token`` the
host carries a port capability in its user info: an HMAC of this environment
port and an expiry, keyed with the token. ate-env-api accepts it on WebSocket
upgrades only and never forwards it; the token itself is never in a URL. Only
the event WebSocket uses the user info: the REST client gets the host without
it and never sends the capability, and ``repr``, dumps, errors and the known
loggers mask it.

Mirrors ``openhands.workspace.AgentSandboxWorkspace``: same lifecycle order
(create, health wait, parent init; clean up on any failure after create),
``pause()`` / ``resume()``, ``__exit__`` then ``cleanup()``, idempotent
``cleanup()``. The agent-sandbox warm pool maps to ``template``; the task
image, which agent-sandbox fixes in its SandboxTemplate, is a field here.
"""

from __future__ import annotations

import asyncio
import hashlib
import hmac
import logging
import re
import secrets
import shlex
import sys
import time
from collections.abc import Coroutine
from typing import Any

import grpc
import httpx
from ate_env import EnvError, EnvironmentStatus, NotFoundError, RpcError
from openhands.sdk.logger import get_logger
from openhands.sdk.workspace import RemoteWorkspace
from pydantic import Field, PrivateAttr, field_serializer

from . import _runtime

logger = get_logger(__name__)

DEFAULT_ATESPACE = "ate-env"

# The port proxy reads the caller's ate-env-api credential from this header
# and does not forward it, so the agent-server's own credentials stay apart.
ENV_AUTHORIZATION_HEADER = "X-Env-Authorization"

# The user of the port capability in the host's user info (see
# port_capability), and the latest expiry ate-env-api accepts.
CAPABILITY_USER = "ate-env-capability"
MAX_CAPABILITY_TTL = 7 * 24 * 3600.0
# ws_capability_ttl stays this far below the maximum, because the proxy checks
# the expiry against its own clock and a client clock can run ahead.
CAPABILITY_CLOCK_MARGIN = 300.0

# Deadline for get, suspend and delete, so that cleanup() cannot block forever.
CALL_TIMEOUT = 30.0

# After a create that failed on this side, how long a delete retries NotFound:
# the server can still be creating the environment.
ORPHAN_DELETE_WINDOW = 10.0

# Create failures after which the environment can exist on the server.
_AMBIGUOUS_CODES = {
    grpc.StatusCode.DEADLINE_EXCEEDED,
    grpc.StatusCode.UNAVAILABLE,
    grpc.StatusCode.UNKNOWN,
    grpc.StatusCode.INTERNAL,
    grpc.StatusCode.CANCELLED,
    grpc.StatusCode.ABORTED,
}


# ate-env-api's port proxy names the actor state of an environment that will
# not wake in this header: 410 Gone with CRASHED for a crashed one, 404 with
# DELETING for one that is being deleted.
ENV_STATE_HEADER = "X-Env-State"

# While the health wait retries, how often it also reads the environment's
# state over gRPC, so that a crashed environment stops the wait even when the
# port proxy passes the router's 503 unchanged.
STATE_CHECK_INTERVAL = 5.0
# Deadline of that read, or the time left in the wait if less (at least 1 s).
STATE_READ_TIMEOUT = 5.0


def _read_timeout(deadline: float) -> float:
    """Deadline of a state read in the health wait: STATE_READ_TIMEOUT, or the
    time left in the wait if less, at least 1 s, so a stalled control plane
    cannot hold the wait past its end."""
    return max(1.0, min(STATE_READ_TIMEOUT, deadline - time.monotonic()))


def _base_url(url: str) -> str:
    url = url.rstrip("/")
    return url if "://" in url else f"http://{url}"


def port_url(api_url: str, atespace: str, env_id: str, port: int) -> str:
    """URL of ``port`` in an environment, through ate-env-api's port proxy."""
    return f"{_base_url(api_url)}/v1alpha/atespaces/{atespace}/envs/{env_id}/ports/{port}"


def port_capability(token: str, atespace: str, env_id: str, port: int, expiry: int) -> str:
    """A port capability for ``port`` in an environment, signed with ``token``.

    ``v1.<expiry>.<mac>``, with ``mac`` the hex HMAC-SHA256, keyed with
    ate-env-api's bearer token, of the atespace, the environment id, the port
    and ``expiry`` (Unix seconds). ate-env-api's port proxy accepts it on
    WebSocket upgrades to that port until ``expiry``, at most seven days
    ahead.
    """
    msg = f"ate-env port capability v1\n{atespace}\n{env_id}\n{port}\n{expiry}"
    mac = hmac.new(token.encode(), msg.encode(), hashlib.sha256).hexdigest()
    return f"v1.{expiry}.{mac}"


def _with_capability(url: str, capability: str) -> str:
    scheme, rest = url.split("://", 1)
    return f"{scheme}://{CAPABILITY_USER}:{capability}@{rest}"


def _without_userinfo(url: str) -> str:
    """The URL without user info, thus without a port capability."""
    scheme, sep, rest = url.partition("://")
    if not sep:
        return url
    netloc, slash, path = rest.partition("/")
    return f"{scheme}://{netloc.rpartition('@')[2]}{slash}{path}"


_CAPABILITY_IN_URL = re.compile(re.escape(CAPABILITY_USER) + r":[^@/\s]+@")
# "Basic " and the base64 text of "ate-env-capability:..."; the user name is 18
# bytes, thus its first 24 base64 characters do not depend on the capability.
_CAPABILITY_IN_BASIC = re.compile(r"Basic\s+YXRlLWVudi1jYXBhYmlsaXR5[A-Za-z0-9+/=]*")


def _redact(text: str) -> str:
    text = _CAPABILITY_IN_URL.sub(CAPABILITY_USER + ":***@", text)
    return _CAPABILITY_IN_BASIC.sub("Basic ***", text)


def _needs_redact(text: str) -> bool:
    return CAPABILITY_USER in text or "YXRlLWVudi1jYXBhYmlsaXR5" in text


class _RedactCapability(logging.Filter):
    """Masks port capabilities in log records.

    httpx logs request URLs, and websockets logs each handshake header at DEBUG
    level, ``Authorization: Basic`` included, as a ``"> %s: %s"`` record.
    """

    def filter(self, record: logging.LogRecord) -> bool:
        if isinstance(record.msg, str) and _needs_redact(record.msg):
            record.msg = _redact(record.msg)
        if isinstance(record.args, tuple) and record.args:
            args = list(record.args)
            if len(args) == 2 and str(args[0]).lower() == "authorization":
                args[1] = "***"
            record.args = tuple(_redact(str(a)) if _needs_redact(str(a)) else a for a in args)
        return True


_redact_filter = _RedactCapability()

# A logger filter applies only to records of that exact logger, not to its
# children, thus name each logger that can log the host or the handshake.
_REDACTED_LOGGERS = (
    "httpx",
    "httpcore.connection",
    "httpcore.http11",
    "httpcore.http2",
    "httpcore.proxy",
    "websockets.client",
    "openhands.sdk.workspace.remote.base",
    "openhands.sdk.conversation.impl.remote_conversation",
)


def _redact_capabilities_in_logs() -> None:
    for name in _REDACTED_LOGGERS:
        log = logging.getLogger(name)
        if _redact_filter not in log.filters:
            log.addFilter(_redact_filter)


def _strip_userinfo_from_request(request: httpx.Request) -> None:
    """httpx request hook: a REST request never carries the host's user info."""
    if request.url.userinfo:
        request.url = request.url.copy_with(userinfo=b"")


def _new_env_id() -> str:
    return f"oh-{secrets.token_hex(6)}"


def _may_exist_after(exc: BaseException) -> bool:
    """Whether a failed create can still have made the environment."""
    if isinstance(exc, RpcError):
        return exc.code in _AMBIGUOUS_CODES
    # NotFound, InvalidArgument, PermissionDenied and FailedPrecondition are
    # definite refusals; a timeout, a cancel or a lost connection is not.
    return not isinstance(exc, EnvError)


def _from_port_proxy(resp: httpx.Response) -> bool:
    """Whether a 401 or 403 came from ate-env-api's port proxy, not the agent-server.

    The proxy answers a missing or wrong bearer token with
    ``WWW-Authenticate: Bearer`` and a body that names the bearer token.
    """
    if resp.headers.get("www-authenticate", "").lower().startswith("bearer"):
        return True
    return "bearer token" in resp.text


class AteEnvWorkspace(RemoteWorkspace):
    """Remote workspace backed by an ate-env environment.

    Creates an environment from ``template`` (a base template whose runtime
    layer carries the agent-server), with ``image`` as its root filesystem,
    waits until the agent-server answers ``/health`` through ate-env-api, and
    deletes the environment on exit. With ``env_id`` it attaches to an
    existing environment instead and leaves it in place on exit.

    Only the workspace that created or attached the environment deletes it.
    A ``model_copy()`` of it shares the environment but never deletes it, and
    gets its own HTTP client.

    Use either the context manager or ``cleanup()``. ``__del__`` cleans up as
    a last resort, but not during interpreter shutdown.

    Example:
        with AteEnvWorkspace(
            template="oh-agent-server",
            image="docker.io/library/python@sha256:...",
            api_key=SESSION_KEY,
        ) as workspace:
            result = workspace.execute_command("python3 --version")
    """

    working_dir: str = Field(
        default="/workspace",
        description="Working directory in the environment. Created when missing. "
        "Use /testbed for SWE-bench images.",
    )
    host: str = Field(
        default="",
        description="Agent-server URL through the port proxy. Set by the workspace "
        "from proxy_url (or api_url), atespace, env_id and server_port; do not pass it.",
    )

    # ate-env configuration
    template: str | None = Field(
        default=None,
        description="Base template with the agent-server layer and sidecar. Required "
        "unless env_id names an existing environment. Counterpart of agent-sandbox's "
        "warmpool.",
    )
    template_atespace: str | None = Field(
        default=None,
        description="Atespace of the template. Unset means the server default.",
    )
    image: str | None = Field(
        default=None,
        description="Task image (repo@sha256:...) to run unmodified. Unset means the "
        "template's own image.",
    )
    atespace: str = Field(
        default=DEFAULT_ATESPACE,
        description="Atespace of the environment. Counterpart of agent-sandbox's namespace.",
    )
    api_url: str = Field(
        default="127.0.0.1:7777",
        description="ate-env-api address, host:port or http(s) URL (gRPC and the port "
        "proxy share its listener).",
    )
    proxy_url: str | None = Field(
        default=None,
        description="Base URL of the port proxy when it differs from api_url.",
    )
    api_token: str | None = Field(
        default=None,
        repr=False,
        exclude=True,
        description="Bearer token for ate-env-api (its -auth-token-file). Sent on gRPC "
        "calls, and on port-proxy requests in X-Env-Authorization, which ate-env-api "
        "checks and removes before it forwards a request. The conversation event "
        "WebSocket carries a port capability derived from it instead (see "
        "ws_capability_ttl); the token is never in a URL.",
    )
    ws_capability_ttl: float = Field(
        default=86400.0,
        gt=0.0,
        le=MAX_CAPABILITY_TTL - CAPABILITY_CLOCK_MARGIN,
        description="Seconds the port capability in host stays valid, with api_token. "
        "A conversation's event WebSocket keeps the capability it opened with and "
        "reconnects with it, thus a conversation that lives longer needs a longer value "
        "(at most seven days less five minutes), or a reattach after "
        "renew_ws_capability() or resume().",
    )
    env_id: str | None = Field(
        default=None,
        description="Attach to this existing environment instead of creating one.",
    )
    server_port: int = Field(
        default=8000,
        ge=1,
        le=65535,
        description="Port the agent-server listens on in the environment.",
    )
    create_timeout: float = Field(
        default=600.0,
        gt=0.0,
        description="Seconds to wait for CreateEnvironment (a new image pulls and "
        "snapshots first).",
    )
    health_check_timeout: float = Field(
        default=600.0,
        gt=0.0,
        description="Seconds to wait for the agent-server's /health through the proxy. "
        "The first use of a task image on a cluster bakes its golden snapshot and then "
        "pulls the image onto the node, minutes for a large image; later uses take "
        "seconds. Answers that waiting cannot change (400, 401, 403, 404 for a "
        "deleted environment, and 410 or X-Env-State for a crashed or deleting one) "
        "stop the wait at once.",
    )
    delete_on_exit: bool | None = Field(
        default=None,
        description="Delete the environment in cleanup(). Unset means: delete it when "
        "this workspace created it, keep it when attached with env_id.",
    )

    # Ownership, set only after the create or the attach succeeds. A copy
    # carries _is_copy and never deletes. A plain flag, not a weakref: the
    # cyclic garbage collector clears weakrefs before it calls __del__.
    _is_copy: bool = PrivateAttr(default=False)
    _created: bool = PrivateAttr(default=False)
    _attached: bool = PrivateAttr(default=False)
    _deleted: bool = PrivateAttr(default=False)

    def model_post_init(self, context: Any) -> None:
        """Create or attach the environment, wait for the agent-server, initialize."""
        if self.host:
            raise ValueError("host is set by the workspace; pass proxy_url (or api_url) "
                             "and server_port instead")
        if self.env_id is None and not self.template:
            raise ValueError("template is required to create an environment; "
                             "pass env_id to attach to an existing one")
        if self.env_id is not None and self.image:
            raise ValueError("image applies only when creating; it cannot change "
                             "on an existing environment")
        if self.image and "@sha256:" not in self.image:
            raise ValueError(f"image {self.image!r} must be pinned by digest "
                             "(repo@sha256:...); resolve a tag with "
                             "`crane digest --platform linux/amd64 <ref>`")

        if self.env_id is None:
            self._create()
        else:
            self._call(self._ate().get(self.env_id, atespace=self.atespace))
            self._attached = True
            logger.info("Attached to environment %s/%s", self.atespace, self.env_id)

        # Everything past the create must clean up on failure: the caller gets
        # no object to close when the constructor raises.
        try:
            self._set_host()
            self._wait_for_health(timeout=self.health_check_timeout)
            super().model_post_init(context)
            self._check_session_key()
            self._ensure_working_dir()
            self._check_versions()
            logger.info("ate-env workspace is ready at %s", _redact(self.host))
        except Exception:
            self.cleanup()
            raise

    def model_copy(self, *, update: dict[str, Any] | None = None, deep: bool = False):  # type: ignore[override]
        """Copy the configuration. The copy never deletes the environment and
        opens its own HTTP client."""
        client, self._client = self._client, None
        try:
            copy = super().model_copy(update=update, deep=deep)
        finally:
            self._client = client
        copy._is_copy = True
        return copy

    def __repr_args__(self):  # type: ignore[no-untyped-def]
        for name, value in super().__repr_args__():
            if name == "host" and isinstance(value, str):
                value = _redact(value)
            yield name, value

    @field_serializer("host")
    def _serialize_host(self, host: str) -> str:
        return _redact(host)

    def _set_host(self) -> None:
        """Set host from the proxy URL, with a new port capability when api_token is set."""
        host = self.port_url(self.server_port)
        if self.api_token:
            expiry = int(time.time() + self.ws_capability_ttl)
            capability = port_capability(self.api_token, self.atespace, self.env_id,
                                         self.server_port, expiry)
            host = _with_capability(host, capability)
            _redact_capabilities_in_logs()
        object.__setattr__(self, "host", host)

    def renew_ws_capability(self) -> None:
        """Put a new port capability, valid ``ws_capability_ttl`` from now, in host.

        A ``RemoteConversation`` keeps the host it opened with, thus only a
        conversation opened or reattached after this call uses the new
        capability for its event WebSocket. The HTTP client stays open: REST
        requests do not use the capability, thus a live conversation can still
        send messages. No-op without ``api_token``.
        """
        if self.api_token:
            self._set_host()

    @property
    def _rest_host(self) -> str:
        """host without user info: the base of all REST requests."""
        return _without_userinfo(self.host)

    @property
    def client(self) -> httpx.Client:
        """The SDK's HTTP client, without the port capability.

        Only the event WebSocket uses host's user info. The client's base URL
        drops it; a no-op ``auth`` keeps httpx from sending it as Basic
        credentials; and a request hook removes it from the absolute URLs the
        SDK builds from host, thus it is in no request, log line or error.
        """
        client = super().client
        if not getattr(client, "_ate_env_rest", False):
            client.base_url = httpx.URL(self._rest_host)
            client.auth = httpx.Auth()
            hooks = client.event_hooks
            client.event_hooks = {
                "request": [*hooks.get("request", []), _strip_userinfo_from_request],
                "response": list(hooks.get("response", [])),
            }
            client._ate_env_rest = True  # type: ignore[attr-defined]
        return client

    @property
    def alive(self) -> bool:
        """Whether the agent-server answers /health, through the authenticated proxy."""
        try:
            resp = httpx.get(f"{self._rest_host}/health", headers=dict(self._headers),
                             timeout=5.0)
        except httpx.HTTPError:
            return False
        return 200 <= resp.status_code < 300

    def get_secrets(self, names=None):  # type: ignore[no-untyped-def,override]
        """The SDK's ``get_secrets()``, with lookup URLs that carry no port capability."""
        secrets = super().get_secrets(names)
        return {name: secret.model_copy(update={"url": _without_userinfo(secret.url)})
                for name, secret in secrets.items()}

    @property
    def _headers(self) -> dict[str, str]:
        headers = super()._headers
        headers.update(self.proxy_headers())
        return headers

    def proxy_headers(self) -> dict[str, str]:
        """Headers that authenticate a request to the port proxy itself.

        Add them to a request that goes to ``port_url(...)`` outside the SDK.
        Empty when ``api_token`` is unset.
        """
        if not self.api_token:
            return {}
        return {ENV_AUTHORIZATION_HEADER: f"Bearer {self.api_token}"}

    def _ate(self):
        return _runtime.client(self.api_url, self.api_token)

    def _call[T](self, coro: Coroutine[Any, Any, T], timeout: float = CALL_TIMEOUT) -> T:
        return _runtime.run(asyncio.wait_for(coro, timeout))

    def _owns(self) -> bool:
        return (self._created or self._attached) and not self._is_copy

    def _create(self) -> None:
        env_id = _new_env_id()
        object.__setattr__(self, "env_id", env_id)
        logger.info("Creating environment %s/%s from template %r%s",
                    self.atespace, env_id, self.template,
                    f" with image {self.image}" if self.image else "")
        try:
            self._call(
                self._ate().create(
                    env_id,
                    atespace=self.atespace,
                    template_name=self.template,
                    template_atespace=self.template_atespace,
                    rootfs_image=self.image,
                ),
                timeout=self.create_timeout,
            )
        except BaseException as exc:
            self._deleted = True  # this workspace never holds the environment
            if _may_exist_after(exc):
                self._delete_orphan(env_id)
            raise
        self._created = True

    def _delete_orphan(self, env_id: str) -> None:
        """Delete an environment whose create failed or timed out on this side.

        The server can finish the create after this side gave up, thus a
        NotFound is retried for a short time.
        """
        deadline = time.monotonic() + ORPHAN_DELETE_WINDOW
        delay = 0.5
        while True:
            try:
                self._call(self._ate().delete(env_id, atespace=self.atespace))
                logger.info("Deleted environment %s/%s after the failed create",
                            self.atespace, env_id)
                return
            except NotFoundError:
                if time.monotonic() + delay > deadline:
                    logger.warning(
                        "Environment %s/%s was not found after the failed create; it can "
                        "still appear on the server: delete it with `ate-env delete %s "
                        "--atespace %s`", self.atespace, env_id, env_id, self.atespace)
                    return
                time.sleep(delay)
                delay = min(delay * 2, 2.0)
            except Exception as exc:
                logger.warning("Error deleting environment %s/%s after the failed create: %s",
                               self.atespace, env_id, exc)
                return

    def port_url(self, port: int) -> str:
        """URL of another port in this environment, through the port proxy."""
        if self.env_id is None:
            raise RuntimeError("no environment")
        return port_url(self.proxy_url or self.api_url, self.atespace, self.env_id, port)

    def _wait_for_health(self, *, timeout: float) -> None:
        """Poll the agent-server's /health through the proxy until it answers.

        The first request also wakes a suspended environment. A 503 from the
        router (not running yet), a 502 and a refused connection are retried.
        Answers that waiting cannot change stop the wait at once: 401 and 403
        (the proxy refused the caller), 400 (the proxy refused the port or the
        id), 404 when the environment no longer exists, and 410 Gone or an
        ``X-Env-State`` of CRASHED or DELETING (ate-env-api's port proxy
        answers 410 with CRASHED for a crashed environment). While it retries,
        the wait also reads the environment's state over gRPC, at most once
        each STATE_CHECK_INTERVAL, and stops on CRASHED, DELETING or a deleted
        environment: a proxy without that answer passes the router's 503 for
        a crashed environment unchanged. Otherwise the last answer is in the
        error when the deadline passes.
        """
        deadline = time.monotonic() + timeout
        url = f"{self._rest_host}/health"
        headers = dict(self._headers)
        last = "no attempt"
        next_state_check = 0.0
        while True:
            remaining = deadline - time.monotonic()
            if remaining <= 0:
                break
            # One attempt can wait for the rest of the budget: the Substrate
            # router holds a request while the environment wakes, up to about
            # 10 s, then answers 504 (or 503 with X-Env-State through a port
            # proxy with the state check), which the loop retries. A shorter
            # client timeout would only cut that hold short.
            budget = max(remaining, 1.0)
            try:
                resp = httpx.get(url, headers=headers,
                                 timeout=httpx.Timeout(budget, connect=min(10.0, budget)))
            except httpx.HTTPError as exc:
                last = f"{type(exc).__name__}: {exc}"
            else:
                code = resp.status_code
                if 200 <= code < 300:
                    return
                text = resp.text.strip()[:200]
                last = f"HTTP {code}: {text}"
                self._raise_if_gone(resp, last)
                if code in (401, 403):
                    raise PermissionError(
                        f"the port proxy refused the request for {self.atespace}/{self.env_id} "
                        f"({last}); api_token must be ate-env-api's bearer token")
                if code == 400:
                    raise ValueError(f"the port proxy rejected {url} ({last})")
                if code == 404 and not self._exists(_read_timeout(deadline)):
                    raise RuntimeError(f"environment {self.atespace}/{self.env_id} no longer "
                                       f"exists ({last})")
            if time.monotonic() >= next_state_check:
                next_state_check = time.monotonic() + STATE_CHECK_INTERVAL
                self._raise_if_state_is_final(last, _read_timeout(deadline))
            time.sleep(min(1.0, max(deadline - time.monotonic(), 0)))
        raise RuntimeError(f"agent-server in {self.atespace}/{self.env_id} did not answer "
                           f"/health within {timeout}s; last: {last}")

    def _raise_if_gone(self, resp: httpx.Response, last: str) -> None:
        """Raise when an answer says that the environment will not wake.

        A crashed environment stays crashed: only a new one helps. Any answer
        with ``X-Env-State: CRASHED`` counts, whatever its status, and so does
        410 Gone without the header.
        """
        state = resp.headers.get(ENV_STATE_HEADER, "").strip().upper()
        where = f"environment {self.atespace}/{self.env_id}"
        if state == "CRASHED" or resp.status_code == 410:
            raise RuntimeError(
                f"{where} has crashed and will not recover ({last}); delete it and "
                "create a new one")
        if state == "DELETING":
            raise RuntimeError(f"{where} is being deleted ({last})")

    def _raise_if_state_is_final(self, last: str, timeout: float) -> None:
        """Raise when GetEnvironment says the environment will not wake.

        An error other than NotFound says nothing about the environment; the
        wait goes on.
        """
        where = f"environment {self.atespace}/{self.env_id}"
        try:
            info = self._call(self._ate().get(self.env_id, atespace=self.atespace), timeout=timeout)
        except NotFoundError:
            raise RuntimeError(f"{where} no longer exists ({last})") from None
        except Exception as exc:
            logger.debug("state check of %s skipped: %s", where, exc)
            return
        if info.status == EnvironmentStatus.CRASHED:
            raise RuntimeError(
                f"{where} has crashed and will not recover (state CRASHED; last: {last}); "
                "delete it and create a new one")
        if info.status == EnvironmentStatus.DELETING:
            raise RuntimeError(f"{where} is being deleted (state DELETING; last: {last})")

    def _exists(self, timeout: float = CALL_TIMEOUT) -> bool:
        """Whether the environment still exists; True when that is unknown."""
        try:
            self._call(self._ate().get(self.env_id, atespace=self.atespace), timeout=timeout)
        except NotFoundError:
            return False
        except Exception:
            return True
        return True

    def _check_session_key(self) -> None:
        """Fail at construction when the agent-server rejects api_key.

        /health answers without a key; the command API does not. The SDK turns
        a rejected command into a failed result, not an error, thus check once
        here. ``/api/bash/bash_events/search`` is not a public API: agent-server
        1.55.0, the version pinned in pyproject.toml and layer/requirements.in,
        serves it behind the session key. Check it again when the pin moves; a
        404 skips the check with a warning instead of failing.
        """
        resp = self.client.get(f"{self.api_prefix}/bash/bash_events/search", params={"limit": 1})
        if resp.status_code in (401, 403):
            detail = f"HTTP {resp.status_code}: {resp.text.strip()[:200]}"
            if _from_port_proxy(resp):
                # The token can change after the health wait, e.g. a rotation.
                raise PermissionError(
                    f"the port proxy refused the request for {self.atespace}/{self.env_id} "
                    f"({detail}); api_token must be ate-env-api's current bearer token")
            raise PermissionError(
                f"agent-server in {self.atespace}/{self.env_id} rejected the session key "
                f"({detail}); api_key must match the template's OH_SESSION_API_KEYS_0")
        if resp.status_code == 404:
            logger.warning("session key not checked: the agent-server has no "
                           "/api/bash/bash_events/search (version other than 1.55.0?)")
            return
        resp.raise_for_status()

    def _ensure_working_dir(self) -> None:
        result = self.execute_command(f"mkdir -p {shlex.quote(self.working_dir)}", timeout=30)
        if result.exit_code != 0:
            logger.warning("Could not create working_dir %s: %s", self.working_dir,
                           (result.stderr or result.stdout).strip()[:200])

    def _check_versions(self) -> None:
        """Warn when the local openhands-sdk and the agent-server's differ in version."""
        try:
            from importlib.metadata import version

            local = version("openhands-sdk")
            info = self.get_server_info()
            remote = str(info.get("sdk_version") or info.get("version") or "")
        except Exception as exc:  # informational only
            logger.debug("version check skipped: %s", exc)
            return
        if remote and remote != local:
            logger.warning("openhands-sdk %s talks to an agent-server with openhands-sdk %s; "
                           "keep both at one version (see the layer's requirements.in)",
                           local, remote)

    def pause(self) -> None:
        """Suspend the environment. Its filesystem is kept in the snapshot.

        Close any ``RemoteConversation`` on this workspace first. A live one
        keeps its event WebSocket: the suspend breaks it, the SDK reconnects
        after about a second, and the proxy wakes the environment for that
        request, thus it does not stay suspended and keeps a worker. To park
        between agent turns, close the conversation, call pause(), and after
        resume() reattach it with
        ``Conversation(agent=..., workspace=self, conversation_id=...)``; the
        agent-server keeps the conversation.
        """
        if self.env_id is None or self._deleted:
            raise RuntimeError("Cannot pause: no active environment")
        logger.info("Suspending environment %s/%s", self.atespace, self.env_id)
        # The HTTP client stays open: its pooled connections go to
        # ate-env-api, not to the actor, and the port proxy re-dials the
        # actor after the wake. A live RemoteConversation holds this client,
        # thus closing it would break the conversation.
        self._call(self._ate().suspend(self.env_id, atespace=self.atespace))

    def resume(self) -> None:
        """Wake the environment and wait for the agent-server.

        Any request wakes a suspended environment; this one makes the wait
        explicit. With ``api_token``, host gets a new port capability first
        (``renew_ws_capability()``), thus a conversation reattached after the
        resume can open its event WebSocket however long the pause was. The
        HTTP client does not change, thus a ``RemoteConversation`` created
        before pause() keeps working.
        """
        if self.env_id is None or self._deleted:
            raise RuntimeError("Cannot resume: no active environment")
        logger.info("Resuming environment %s/%s", self.atespace, self.env_id)
        if self.api_token:
            self._set_host()
        self._wait_for_health(timeout=self.health_check_timeout)

    def __enter__(self) -> AteEnvWorkspace:
        return self

    def __exit__(self, exc_type, exc_val, exc_tb) -> None:  # type: ignore[no-untyped-def]
        """Send completion callbacks, then clean up."""
        try:
            super().__exit__(exc_type, exc_val, exc_tb)
        finally:
            self.cleanup()

    def __del__(self) -> None:
        # During interpreter shutdown the loop thread can be gone, and a call
        # on it would hang; the atexit hook in _runtime has closed it.
        if sys.is_finalizing():
            return
        try:
            if getattr(self, "__pydantic_private__", None) is not None:
                self.cleanup()
        except Exception:
            pass  # never raise from __del__

    async def _delete_quietly(self) -> None:
        """Delete the environment on the loop; log, never raise."""
        try:
            await asyncio.wait_for(self._ate().delete(self.env_id, atespace=self.atespace), CALL_TIMEOUT)
        except NotFoundError:
            pass
        except Exception as exc:
            logger.warning("Error deleting environment %s/%s: %s", self.atespace, self.env_id, exc)

    def cleanup(self) -> None:
        """Drop HTTP state and delete the environment when owned (idempotent).

        Only the workspace that created or attached the environment deletes
        it: never a copy, and never a workspace whose constructor failed before
        the create or the attach succeeded.
        """
        try:
            self.reset_client()
        except Exception:
            pass
        if not self._owns() or self._deleted or self.env_id is None:
            return
        delete = self.delete_on_exit if self.delete_on_exit is not None else self._created
        if not delete:
            return
        self._deleted = True
        if _runtime.on_loop_thread():
            # cleanup() (through __del__, or a callback) on the loop thread:
            # waiting here would block the loop that runs the delete. Schedule
            # it and return; the loop finishes it.
            logger.warning("Deleting environment %s/%s from the loop thread: scheduled without waiting",
                           self.atespace, self.env_id)
            _runtime.submit(self._delete_quietly())
            return
        try:
            logger.info("Deleting environment %s/%s", self.atespace, self.env_id)
            self._call(self._ate().delete(self.env_id, atespace=self.atespace))
        except NotFoundError:
            pass
        except Exception as exc:
            logger.warning("Error deleting environment %s/%s: %s", self.atespace, self.env_id, exc)
