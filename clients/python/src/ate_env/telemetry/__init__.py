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

"""Optional telemetry for the ate_env client: OpenTelemetry tracing and W11 benchmark telemetry.

Tracing needs the ``tracing`` extra. Nothing here imports OpenTelemetry at
import time; without the extra every call is a no-op.
"""

# pylint: disable=undefined-all-variable

from __future__ import annotations

from typing import TYPE_CHECKING, Any

from . import bench as _bench
from .bench import (
    _CURRENT_SLOT_WAIT_MS,
    _CURRENT_TASK_META,
    _IN_TRACKED_EXEC,
    _IN_TRACKED_LIFECYCLE,
    _IN_TRACKED_SHELL,
    _MAX_RESERVOIR_SAMPLES,
    SCHEMA_VERSION,
    BenchTelemetry,
    TraceEventRecord,
    _load_w1_schema,
    _wrap_env_handle,
    instrument_client,
    validate_event_dict,
    validate_trace_event_dict,
)
from .tracing import (
    CLIENT_SPAN_PREFIX,
    TraceContextInterceptor,
    TracerManager,
    create_tracer_manager,
    initialize_tracer,
    inject_metadata,
)

if TYPE_CHECKING:
    from .bench import (
        CANONICAL_EVENT_SOURCES,
        CANONICAL_TRACE_OPS,
        EVENT_ALLOWED_KEYS,
        EVENT_SCHEMA,
        TRACE_EVENT_ALLOWED_KEYS,
        TRACE_EVENT_SCHEMA,
    )

__all__ = [
    "BenchTelemetry",
    "CANONICAL_EVENT_SOURCES",
    "CANONICAL_TRACE_OPS",
    "CLIENT_SPAN_PREFIX",
    "EVENT_ALLOWED_KEYS",
    "EVENT_SCHEMA",
    "SCHEMA_VERSION",
    "TRACE_EVENT_ALLOWED_KEYS",
    "TRACE_EVENT_SCHEMA",
    "TraceContextInterceptor",
    "TraceEventRecord",
    "TracerManager",
    "_CURRENT_SLOT_WAIT_MS",
    "_CURRENT_TASK_META",
    "_IN_TRACKED_EXEC",
    "_IN_TRACKED_LIFECYCLE",
    "_IN_TRACKED_SHELL",
    "_MAX_RESERVOIR_SAMPLES",
    "_load_w1_schema",
    "_wrap_env_handle",
    "create_tracer_manager",
    "initialize_tracer",
    "inject_metadata",
    "instrument_client",
    "validate_event_dict",
    "validate_trace_event_dict",
]


def __getattr__(name: str) -> Any:
    """Lazily resolve W1 schema constants from ate_env.telemetry.bench (PEP 562)."""
    if name in _bench._LAZY_SCHEMA_ATTRS:  # pylint: disable=protected-access
        return getattr(_bench, name)
    raise AttributeError(f"module {__name__!r} has no attribute {name!r}")
