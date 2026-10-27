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

"""agent-sandbox-rl's configuration models, mapped onto ``ate_env.FleetConfig``.

The field names, defaults and validators are agent-sandbox-rl's
(kubernetes-sigs/agent-sandbox ``examples/agent-sandbox-rl``,
``agent_sandbox_rl/config.py``), so code written against it constructs the
same objects unchanged. ``FleetConfig.to_ate_env()`` gives the ``ate_env``
configuration the fleet actually runs with.

What maps, and what does not:

- A ``ClusterConfig`` is one ate-env-api endpoint. Its ``namespace`` is the
  atespace; the field default (``"default"``) means "the ambient atespace":
  ``SUBSTRATE_ENV_ATESPACE`` when set, else ate-env's default atespace. The
  endpoint and the caller token come from ``SUBSTRATE_ENV_API`` and
  ``SUBSTRATE_ENV_AUTH_TOKEN`` (the variables the ``ate-env`` CLI reads), or
  from the ate-env-only fields ``endpoint`` and ``auth_token``. One fleet
  talks to one endpoint, thus more than one cluster is an error.
- A ``TemplateSpec`` describes a pod. On ate-env the base ActorTemplate fixes
  the command, the resources and the placement when it is registered, thus
  the spec selects that template: the ate-env-only field ``name``, else
  ``SUBSTRATE_ENV_TEMPLATE``, else ate-env's default template. The pod fields
  (``resources``, ``keepalive_command``, ``runtime_class``,
  ``node_selector``, ``image_pull_*``, ``colocate_replicas``,
  ``extra_pod_spec``) have no ate-env meaning and are ignored. A session key
  in ``extra_pod_spec`` is the base template's key on ate-env.
- Fleet sizing maps: ``max_concurrent``, ``max_warmpool_size`` (to
  ``max_warmpool_replicas``), ``window_size``, ``ready_timeout`` (to the
  acquire and ready timeouts), ``run_id`` and ``observability``. The
  Kubernetes mechanics (placement, pool naming, create budgets, the circuit
  breaker, teardown hooks, disk-aware sizing, labels) are ignored, and one
  INFO log names each set field that is ignored. ``adopt_existing=True`` and
  ``run_isolation`` other than ``"none"`` change what a run creates; ate-env
  cannot honour them, thus they raise.
"""

from __future__ import annotations

import logging
import os
import re
from collections.abc import Callable
from typing import Any

from ate_env import DEFAULT_ATESPACE
from ate_env import FleetConfig as AteFleetConfig
from ate_env import ObservabilityConfig as AteObservabilityConfig
from ate_env.config import SecretStr
from pydantic import BaseModel, Field, field_validator, model_validator

logger = logging.getLogger("ate_env_openhands.agent_sandbox_rl")

ENV_API = "SUBSTRATE_ENV_API"
ENV_AUTH_TOKEN = "SUBSTRATE_ENV_AUTH_TOKEN"
ENV_TEMPLATE = "SUBSTRATE_ENV_TEMPLATE"
ENV_ATESPACE = "SUBSTRATE_ENV_ATESPACE"
DEFAULT_ENDPOINT = "127.0.0.1:7777"

# agent-sandbox-rl's constants that its defaults use.
KEEPALIVE_COMMAND = ["sleep", "infinity"]
DEFAULT_LABELS = {"app": "agent-sandbox-rl"}
RUN_ISOLATION_MODES = ("none", "names", "namespace")
PLACEMENTS = ("round-robin", "least-loaded", "capacity-weighted", "image-affinity")
_DNS1123_LABEL = re.compile(r"^[a-z0-9]([-a-z0-9]{0,61}[a-z0-9])?$")


class ResourceSpec(BaseModel):
    """Per-sandbox container resource requests. On ate-env the base template
    holds the limits; these values are ignored."""

    cpu: str = "250m"
    memory: str = "512Mi"


class TemplateSpec(BaseModel):
    """agent-sandbox-rl's pod template. On ate-env it selects the base
    ActorTemplate (``name``); the pod fields are ignored."""

    resources: ResourceSpec = Field(default_factory=ResourceSpec)
    keepalive_command: list[str] = Field(default_factory=lambda: list(KEEPALIVE_COMMAND))
    runtime_class: str | None = None
    node_selector: dict[str, str] | None = None
    image_pull_secret: str | None = None
    image_pull_policy: str = "IfNotPresent"
    colocate_replicas: bool = False
    extra_pod_spec: dict = Field(default_factory=dict)
    # ate-env only: the base ActorTemplate. None: SUBSTRATE_ENV_TEMPLATE, then
    # ate-env's default template.
    name: str | None = None

    @field_validator("image_pull_policy")
    @classmethod
    def _valid_pull_policy(cls, v: str) -> str:
        if v not in ("Always", "IfNotPresent", "Never"):
            raise ValueError("image_pull_policy must be Always|IfNotPresent|Never")
        return v

    def template_name(self) -> str | None:
        return self.name or os.environ.get(ENV_TEMPLATE) or None


class ObservabilityConfig(BaseModel):
    """agent-sandbox-rl's observability toggles, mapped onto ate-env's."""

    enable_metrics: bool = True
    enable_tracing: bool = False
    trace_service_name: str = "agent-sandbox-rl"


class ClusterConfig(BaseModel):
    """One target. On ate-env: one ate-env-api endpoint and one atespace."""

    name: str = "default"
    kubeconfig: str | None = None
    context: str | None = None
    in_cluster: bool = False
    namespace: str = "default"
    node_selector: dict[str, str] | None = None
    runtime_class: str | None = None
    image_pull_secret: str | None = None
    weight: float = 1.0
    max_replicas: int | None = None
    # ate-env only. None: SUBSTRATE_ENV_API / SUBSTRATE_ENV_AUTH_TOKEN.
    endpoint: str | None = None
    auth_token: str | None = Field(default=None, repr=False)

    @field_validator("weight")
    @classmethod
    def _weight_positive(cls, v: float) -> float:
        if v <= 0:
            raise ValueError("cluster weight must be > 0")
        return v

    @field_validator("name")
    @classmethod
    def _name_nonempty(cls, v: str) -> str:
        if not v:
            raise ValueError("cluster name must be non-empty")
        return v

    def ate_endpoint(self) -> str:
        return self.endpoint or os.environ.get(ENV_API) or DEFAULT_ENDPOINT

    def ate_atespace(self) -> str:
        if self.namespace != "default":
            return self.namespace
        return os.environ.get(ENV_ATESPACE) or DEFAULT_ATESPACE

    def ate_auth_token(self) -> str | None:
        return self.auth_token or os.environ.get(ENV_AUTH_TOKEN) or None


# Fleet fields that drive Kubernetes objects only: accepted, ignored on ate-env.
_IGNORED_FLEET_FIELDS = (
    "placement", "warm_per_task", "warm_create_budget", "overcommit_factor",
    "max_live_sandboxes", "breaker_poll_s", "breaker_trip_polls",
    "install_teardown_hooks", "template_name_prefix", "pool_name_format",
    "run_namespace_labels", "run_namespace_setup", "labels", "avg_image_gb",
    "node_ephemeral_gb", "disk_headroom", "cluster_nodes",
)
_IGNORED_TEMPLATE_FIELDS = (
    "resources", "keepalive_command", "runtime_class", "node_selector",
    "image_pull_secret", "image_pull_policy", "colocate_replicas", "extra_pod_spec",
)
_IGNORED_CLUSTER_FIELDS = (
    "kubeconfig", "context", "in_cluster", "node_selector", "runtime_class",
    "image_pull_secret", "weight", "max_replicas",
)


class FleetConfig(BaseModel):
    """agent-sandbox-rl's fleet configuration; ``to_ate_env()`` maps it."""

    clusters: list[ClusterConfig] = Field(default_factory=list)
    placement: str = "image-affinity"
    max_concurrent: int = 1
    max_warmpool_size: int = 8
    warm_per_task: bool = False
    window_size: int | None = None
    ready_timeout: int = 900
    warm_create_budget: int = 1000
    overcommit_factor: float = 1.5
    max_live_sandboxes: int | None = None
    breaker_poll_s: float = 5.0
    breaker_trip_polls: int = 3
    install_teardown_hooks: bool = True
    template: TemplateSpec = Field(default_factory=TemplateSpec)
    template_name_prefix: str = "r2e-img-"
    pool_name_format: str = "pool-{template}"
    adopt_existing: bool = False
    run_isolation: str = "none"
    run_id: str | None = None
    run_namespace_labels: dict[str, str] = Field(default_factory=dict)
    run_namespace_setup: Callable[..., Any] | None = None
    labels: dict[str, str] = Field(default_factory=lambda: dict(DEFAULT_LABELS))
    observability: ObservabilityConfig = Field(default_factory=ObservabilityConfig)
    avg_image_gb: float | None = None
    node_ephemeral_gb: float | None = None
    disk_headroom: float = 0.25
    cluster_nodes: int | None = None

    @field_validator("max_concurrent", "max_warmpool_size")
    @classmethod
    def _positive(cls, v: int) -> int:
        if v < 1:
            raise ValueError("must be >= 1")
        return v

    @field_validator("window_size", "cluster_nodes")
    @classmethod
    def _window_positive(cls, v: int | None) -> int | None:
        if v is not None and v < 1:
            raise ValueError("must be >= 1 or None")
        return v

    @field_validator("placement")
    @classmethod
    def _known_placement(cls, v: str) -> str:
        if v not in PLACEMENTS:
            raise ValueError(f"unknown placement '{v}'; choose from {sorted(PLACEMENTS)}")
        return v

    @field_validator("run_isolation")
    @classmethod
    def _known_isolation(cls, v: str) -> str:
        if v not in RUN_ISOLATION_MODES:
            raise ValueError(
                f"unknown run_isolation '{v}'; choose from {list(RUN_ISOLATION_MODES)}")
        return v

    @field_validator("run_id")
    @classmethod
    def _valid_run_id(cls, v: str | None) -> str | None:
        if v is not None and not _DNS1123_LABEL.match(v):
            raise ValueError(
                f"run_id {v!r} must be a DNS-1123 label (lowercase alphanumerics and "
                "'-', starting and ending with an alphanumeric, 63 chars max)")
        return v

    @model_validator(mode="after")
    def _ate_env_support(self) -> FleetConfig:
        if len(self.clusters) > 1:
            raise ValueError(
                "ate-env: one fleet talks to one ate-env-api endpoint; got "
                f"{len(self.clusters)} clusters. Run one fleet for each endpoint.")
        if self.adopt_existing:
            raise ValueError(
                "ate-env: adopt_existing=True is not supported; the fleet keeps its "
                "warm environments itself and has no pools to adopt")
        if self.run_isolation != "none":
            raise ValueError(
                f"ate-env: run_isolation={self.run_isolation!r} is not supported; "
                "separate runs with run_id or with an atespace for each run")
        return self

    def cluster(self) -> ClusterConfig:
        """The one target: the configured cluster, else the ambient one."""
        return self.clusters[0] if self.clusters else ClusterConfig()

    def ignored_fields(self) -> list[str]:
        """The set fields that have no ate-env meaning, for the INFO log."""
        defaults_fleet = FleetConfig.model_construct()
        defaults_template = TemplateSpec()
        defaults_cluster = ClusterConfig()
        out = [f for f in _IGNORED_FLEET_FIELDS
               if getattr(self, f) != getattr(defaults_fleet, f, None)]
        out += [f"template.{f}" for f in _IGNORED_TEMPLATE_FIELDS
                if getattr(self.template, f) != getattr(defaults_template, f)]
        cluster = self.cluster()
        out += [f"clusters[0].{f}" for f in _IGNORED_CLUSTER_FIELDS
                if getattr(cluster, f) != getattr(defaults_cluster, f)]
        return out

    def to_ate_env(self) -> AteFleetConfig:
        """The ``ate_env.FleetConfig`` this configuration means on ate-env."""
        cluster = self.cluster()
        token = cluster.ate_auth_token()
        observability: dict[str, Any] = {
            "enable_metrics": self.observability.enable_metrics,
            "enable_tracing": self.observability.enable_tracing,
        }
        if self.observability.trace_service_name != ObservabilityConfig().trace_service_name:
            observability["trace_service_name"] = self.observability.trace_service_name
        ignored = self.ignored_fields()
        if ignored:
            logger.info("ate-env ignores these agent-sandbox-rl settings: %s", ", ".join(ignored))
        return AteFleetConfig(
            backend="substrate",
            endpoint=cluster.ate_endpoint(),
            tenancy=cluster.ate_atespace(),
            template=self.template.template_name(),
            max_concurrent=self.max_concurrent,
            max_warmpool_replicas=self.max_warmpool_size,
            window_size=self.window_size,
            acquire_timeout_s=float(self.ready_timeout),
            ready_timeout_s=float(self.ready_timeout),
            run_id=self.run_id,
            auth_token=SecretStr(token) if token else None,
            observability=AteObservabilityConfig(**observability),
        )
