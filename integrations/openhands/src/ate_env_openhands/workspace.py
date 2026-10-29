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
forwards the request.

Mirrors ``openhands.workspace.AgentSandboxWorkspace``: same lifecycle order
(create, health wait, parent init; clean up on any failure after create),
``pause()`` / ``resume()``, ``__exit__`` then ``cleanup()``, idempotent
``cleanup()``. The agent-sandbox warm pool maps to ``template``; the task
image, which agent-sandbox fixes in its SandboxTemplate, is a field here.
"""

from __future__ import annotations

import asyncio
import re
import secrets
import shlex
import sys
import time
from collections.abc import Coroutine
from typing import TYPE_CHECKING, Any

import grpc
import httpx
from ate_env import EnvError, EnvironmentStatus, NotFoundError, RpcError
from openhands.sdk.logger import get_logger
from openhands.sdk.workspace import RemoteWorkspace
from pydantic import Field, PrivateAttr

from . import _runtime

if TYPE_CHECKING:
    from openhands.sdk.secret import LookupSecret

logger = get_logger(__name__)

DEFAULT_ATESPACE = "ate-env"

# The port proxy reads the caller's ate-env-api credential from this header
# and does not forward it, so the agent-server's own credentials stay apart.
ENV_AUTHORIZATION_HEADER = "X-Env-Authorization"

# Deadline for get, suspend and delete, so that cleanup() cannot block forever.
CALL_TIMEOUT = 30.0

# After a create that failed on this side, how long a delete retries NotFound:
# the server can still be creating the environment.
ORPHAN_DELETE_WINDOW = 10.0

# The agent-server's rule for secret names (openhands-agent-server 1.55.0).
_SECRET_NAME = re.compile(r"[a-zA-Z][a-zA-Z0-9_]{0,63}")

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
        description="Bearer token for ate-env-api (its -auth-token-file). Sent on gRPC "
        "calls, and on port-proxy requests in X-Env-Authorization, which ate-env-api "
        "checks and removes before it forwards a request. The conversation event "
        "WebSocket does not carry it (see the README's Limits).",
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
            object.__setattr__(self, "host", self.port_url(self.server_port))
            self._wait_for_health(timeout=self.health_check_timeout)
            super().model_post_init(context)
            self._check_session_key()
            self._ensure_working_dir()
            self._check_versions()
            logger.info("ate-env workspace is ready at %s", self.host)
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

    @property
    def _headers(self) -> dict[str, str]:
        headers = super()._headers
        headers.update(self.proxy_headers())
        return headers

    def get_secrets(self, names: list[str] | None = None, *,
                    agent_profile_id: str | None = None) -> dict[str, LookupSecret]:
        """The SDK's ``get_secrets()``, with lookups that the agent-server answers itself.

        The SDK points each ``LookupSecret`` at ``host`` and copies the request headers.
        Here ``host`` is the port proxy as this process reaches it: the agent-server in
        the sandbox cannot reach that address, and the headers carry ``api_token``. Thus
        each lookup names ``http://127.0.0.1:<server_port>/api/settings/secrets/<name>``
        and sends the session key only. The caller token never enters the sandbox.

        The agent-server answers these URLs in process when it was started with
        ``python -m openhands.agent_server``, as the layer starts it: that start sets
        ``OH_INTERNAL_SERVER_URL`` (``--host 0.0.0.0`` becomes ``127.0.0.1``). Started
        another way, it falls back to an HTTP request to its own loopback address,
        authenticated by the session key, which can stall its own event loop.
        """
        from openhands.sdk.secret import LookupSecret

        found = super().get_secrets(names, agent_profile_id=agent_profile_id)
        session_headers = super()._headers  # the SDK's headers: the session key, no api_token
        return {
            name: LookupSecret(url=self.local_secret_url(name), headers=dict(session_headers),
                               description=secret.description)
            for name, secret in found.items()
        }

    def local_secret_url(self, name: str) -> str:
        """The URL of secret ``name`` on the agent-server's own loopback address.

        ``name`` must follow the agent-server's rule for secret names. It is not
        URL-encoded: the agent-server's in-process resolver does not decode the path.
        """
        if not _SECRET_NAME.fullmatch(name):
            raise ValueError(
                f"secret name {name!r} is not valid: it must match {_SECRET_NAME.pattern}"
            )
        return f"http://127.0.0.1:{self.server_port}/api/settings/secrets/{name}"

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
        url = f"{self.host}/health"
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
        # Pooled connections tunnel to the actor that is going away.
        self.reset_client()
        self._call(self._ate().suspend(self.env_id, atespace=self.atespace))

    def resume(self) -> None:
        """Wake the environment and wait for the agent-server.

        Any request wakes a suspended environment; this one makes the wait
        explicit. The host does not change.
        """
        if self.env_id is None or self._deleted:
            raise RuntimeError("Cannot resume: no active environment")
        logger.info("Resuming environment %s/%s", self.atespace, self.env_id)
        self._wait_for_health(timeout=self.health_check_timeout)
        self.reset_client()

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
