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

"""Exceptions raised by the ate-env client."""

from __future__ import annotations

import grpc

__all__ = [
    "EnvError",
    "NotFoundError",
    "InvalidArgumentError",
    "PermissionDeniedError",
    "FailedPreconditionError",
    "ProcessExitedError",
    "RpcError",
    "map_rpc_error",
    "InfrastructureError",
    "SandboxUnavailableError",
    "SandboxProtocolError",
    "CommandStartError",
    "SandboxStartError",
    "START_FAILURE_CRASHED",
    "START_FAILURE_KEY",
    "START_FAILURE_NO_CAPACITY",
    "CommandExecutionError",
    "CommandTimeoutError",
    "PreflightError",
]


class EnvError(Exception):
    """Base class for all ate-env client errors."""


class NotFoundError(EnvError):
    """Environment, process, file, or directory does not exist."""


class InvalidArgumentError(EnvError):
    """The server rejected the request as malformed."""


class PermissionDeniedError(EnvError):
    """Access denied — e.g. a file path outside the workspace sandbox."""


class FailedPreconditionError(EnvError):
    """The operation is not valid in the target's current state."""


class ProcessExitedError(FailedPreconditionError):
    """The process has already exited (signals and stdin need a running one)."""


class RpcError(EnvError):
    """Any other RPC failure; carries the gRPC status code."""

    def __init__(self, message: str, code: grpc.StatusCode | None = None):
        super().__init__(message)
        self.code = code


# Trailing metadata key ate-env-api sets on a guest call that failed because
# the environment cannot start (internal/apiservice.StartFailureKey). Its
# value is a START_FAILURE_* reason.
START_FAILURE_KEY = "ate-env-start-failure"
# The control plane moved the environment to CRASHED while it was waking.
START_FAILURE_CRASHED = "crashed"
# No worker had room to run the environment for ate-env-api's
# --wake-no-capacity-grace.
START_FAILURE_NO_CAPACITY = "no-capacity"


def _start_failure_reason(err: grpc.RpcError) -> str | None:
    """The START_FAILURE_KEY trailer of a failed call, if it carries one."""
    trailing = getattr(err, "trailing_metadata", None)
    if not callable(trailing):
        return None
    try:
        items = trailing() or ()
        for key, value in items:
            if key == START_FAILURE_KEY:
                return value.decode() if isinstance(value, bytes) else str(value)
    except Exception:
        return None
    return None


def map_rpc_error(err: BaseException) -> BaseException:
    """Convert a grpc.RpcError (including grpc.aio.AioRpcError) into an EnvError.

    A call that ate-env-api marked with START_FAILURE_KEY (the environment
    crashed while waking, or no worker had room for it) becomes a retryable
    SandboxStartError carrying the reason, whatever its status code.
    Non-RPC exceptions pass through unchanged.
    """
    if not isinstance(err, grpc.RpcError):
        return err
    code = err.code()
    details = err.details() or str(err)
    reason = _start_failure_reason(err)
    if reason:
        status = f"grpc {code.name}" if code is not None else None
        return SandboxStartError(details, status=status, reason=reason)
    if code == grpc.StatusCode.NOT_FOUND:
        return NotFoundError(details)
    if code == grpc.StatusCode.INVALID_ARGUMENT:
        return InvalidArgumentError(details)
    if code == grpc.StatusCode.PERMISSION_DENIED:
        return PermissionDeniedError(details)
    if code == grpc.StatusCode.FAILED_PRECONDITION:
        if "has exited" in details:
            return ProcessExitedError(details)
        return FailedPreconditionError(details)
    return RpcError(details, code)


# ---------------------------------------------------------------------------
# Infrastructure and Execution Errors
#
# Contract: ExecResult is returned only when the guest ran the command and it
# either exited or was killed at timeout_s (timed_out=True). Every other outcome
# raises an InfrastructureError. Transport and HTTP/gRPC statuses are never
# encoded as exit codes.
# ---------------------------------------------------------------------------


class InfrastructureError(EnvError):
    """The SDK could not obtain a verdict from the sandbox for this call.

    Never score these as agent failures (e.g. reward 0). Retry the rollout on a
    fresh sandbox when ``retryable`` is True, otherwise mask it.

    Attributes:
        sandbox_id: Sandbox the call targeted, when known.
        status: Transport status, e.g. "grpc UNAVAILABLE".
        retryable: Whether retrying on a fresh sandbox may succeed.
    """

    default_retryable: bool = True

    def __init__(
        self,
        message: str,
        *,
        sandbox_id: str | None = None,
        status: str | None = None,
        retryable: bool | None = None,
    ):
        super().__init__(message)
        self.sandbox_id = sandbox_id
        self.status = status
        self.retryable = self.default_retryable if retryable is None else retryable

    def __reduce__(self):
        return (
            _rebuild_infra_error,
            (type(self), str(self), self.sandbox_id, self.status, self.retryable),
        )


def _rebuild_infra_error(
    cls: type,
    message: str,
    sandbox_id: str | None,
    status: str | None,
    retryable: bool,
) -> InfrastructureError:
    return cls(message, sandbox_id=sandbox_id, status=status, retryable=retryable)


class SandboxUnavailableError(InfrastructureError):
    """Sandbox or data plane unreachable, gone, overloaded, or failed mid-call.

    Examples: connection refused/reset, gRPC UNAVAILABLE/UNKNOWN/INTERNAL/ABORTED,
    environment not found during lifecycle operations.
    """

    default_retryable = True


class SandboxProtocolError(InfrastructureError):
    """Request rejected or response malformed: a contract or configuration bug.

    Examples: gRPC INVALID_ARGUMENT, PERMISSION_DENIED.
    """

    default_retryable = False


class CommandStartError(InfrastructureError):
    """The guest could not start the command (missing executable or cwd inside guest)."""

    default_retryable = False


class SandboxStartError(InfrastructureError):
    """Raised when sandbox instantiation, template lookup, or boot fails.

    Attributes:
        reason: Why, when ate-env-api said so: START_FAILURE_CRASHED or
            START_FAILURE_NO_CAPACITY; None otherwise.
    """

    default_retryable = True

    def __init__(
        self,
        message: str,
        *,
        sandbox_id: str | None = None,
        status: str | None = None,
        retryable: bool | None = None,
        reason: str | None = None,
    ):
        super().__init__(message, sandbox_id=sandbox_id, status=status, retryable=retryable)
        self.reason = reason

    def __reduce__(self):
        return (
            _rebuild_start_error,
            (str(self), self.sandbox_id, self.status, self.retryable, self.reason),
        )


def _rebuild_start_error(
    message: str,
    sandbox_id: str | None,
    status: str | None,
    retryable: bool,
    reason: str | None,
) -> SandboxStartError:
    return SandboxStartError(
        message, sandbox_id=sandbox_id, status=status, retryable=retryable, reason=reason
    )


class PreflightError(InfrastructureError):
    """Raised when cluster or API connectivity verification fails."""

    default_retryable = False


class CommandExecutionError(EnvError):
    """Raised when a non-zero exit code occurs in strict mode."""

    def __init__(self, message: str, result: object = None):
        super().__init__(message)
        self.result = result


class CommandTimeoutError(CommandExecutionError, TimeoutError):
    """Raised in strict mode (check=True) when a command hit its deadline."""
