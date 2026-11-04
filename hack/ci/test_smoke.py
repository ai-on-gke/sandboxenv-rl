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

"""Portable 9-subtest end-to-end smoke test for ate-env and ate-env-api.

What it does:
  1. Discovers kubectl context and resolves ate-env-api endpoint (or starts port-forward).
  2. Creates an isolated environment actor using `ate-env create`.
  3. Tests remote shell execution (`uname -a`) and stdin stream piping (`tr a-z A-Z`).
  4. Tests file write and read round-trip (`/tmp/smoke.txt`).
  5. Validates built-in MCP server: invokes tools/list and calls `read_file` via HTTP JSON-RPC.
  6. Tests suspend and transparent wake-on-request resume.
  7. Validates caller token authentication and port proxy atespace allowlist.
  8. Scrapes Prometheus metrics exposition at /metrics.
  9. Deletes environment during teardown.

When it runs:
  - CI: Executed as final verification stage of `e2e-kind` job in .github/workflows/ci.yaml.
  - Can also run standalone against any running GKE or Kind cluster with ate-env deployed.
  - Wrapped by `make smoke` and flake app `nix run .#smoke`.

How to run:
  - `make smoke` (preferred; auto-selects Nix or Docker)
  - `nix run .#smoke` / `nix develop -c smoke`
  - Direct: `pytest hack/ci/test_smoke.py -v`
  Examples:
    make smoke
    LOCAL_PORT=18888 make smoke
    SUBSTRATE_ENV_API=127.0.0.1:7777 pytest hack/ci/test_smoke.py -v

Prerequisites:
  - Running Kubernetes cluster with Agent Substrate and ate-env-api deployed.
  - `ate-env` CLI binary (built or found on PATH; auto-built if missing) and `kubectl`.

Environment variables:
  SUBSTRATE_ENV_API         127.0.0.1:<LOCAL_PORT>  Host and port address of target ate-env-api service
  ATE_ENV_API_TARGET        (fallback)              Alternative host and port target address for API
  SUBSTRATE_ENV_AUTH_TOKEN  (unset)                 Bearer token required by ate-env-api
  ATE_ENV_EXPECT_METRICS    (unset)                 Set to 1 to require /metrics exposition check
  ATE_ENV_EXPECT_PORT_PROXY_ALLOWLIST (unset)       Set to 1 to require port-proxy atespace allowlist check
  LOCAL_PORT                17777                   Local port on host for port-forwarding ate-env-api
  NAMESPACE                 ate-env                 Kubernetes namespace where ate-env-api is deployed
  KUBECTL_CONTEXT           (auto-detected)         Target kubectl context for cluster operations
  PROJECT_ID                (unset)                 GKE project ID for context derivation (with loc/name)
  CLUSTER_LOCATION          (unset)                 GKE cluster location for context derivation
  CLUSTER_NAME              (unset)                 GKE cluster name for context derivation
  KIND_CLUSTER_NAME         (unset)                 Kind cluster name for context derivation (kind-<name>)
  ATE_ENV_BIN               (auto: bin/ate-env)     Path to ate-env CLI binary (auto-built if not found)

Outputs / exit codes:
  - Pytest logs and subtest results printed to stdout.
  - Side effects: creates, suspends, and deletes an ephemeral test environment actor.
  - Exit code 0 if all executed subtests pass (up to 9; auth and /metrics subtests run when configured).

Troubleshooting:
  - See docs/chores/troubleshooting.md for port conflict resolution and router SPIFFE
    authorization details.

See also:
  hack/ci/e2e.py, hack/ci/lib.py, Makefile, cmd/ate-env, nix/ci.nix.
"""

from __future__ import annotations

import json
import os
import re
import shutil
import sys
import time
import urllib.error
import urllib.parse
import urllib.request
from pathlib import Path
from typing import Any

import pytest

sys.path.insert(0, str(Path(__file__).resolve().parent))
import lib


def _detect_kubectl_context() -> str:
    if ctx := os.environ.get("KUBECTL_CONTEXT"):
        return ctx
    p, loc, name = os.environ.get("PROJECT_ID"), os.environ.get("CLUSTER_LOCATION"), os.environ.get("CLUSTER_NAME")
    if p and loc and name:
        return f"gke_{p}_{loc}_{name}"
    if kind := os.environ.get("KIND_CLUSTER_NAME"):
        return f"kind-{kind}"
    res = lib.run("kubectl", "config", "current-context", check=False, capture=True)
    return res.stdout.strip() if res.returncode == 0 and res.stdout.strip() else "kind-rl-substrate"


@pytest.fixture
def api_addr():
    addr = os.environ.get("SUBSTRATE_ENV_API") or os.environ.get("ATE_ENV_API_TARGET") or f"127.0.0.1:{os.environ.get('LOCAL_PORT', '17777')}"
    host, _, port_str = addr.rpartition(":")
    port = int(port_str) if port_str else 17777
    if lib.port_open(host or "127.0.0.1", port):
        yield addr
    else:
        with lib.port_forward(_detect_kubectl_context(), os.environ.get("NAMESPACE", "ate-env"), "svc/ate-env-api", port) as fwd:
            yield fwd


@pytest.fixture
def ate_env():
    root = Path(__file__).resolve().parent.parent.parent
    if (b := os.environ.get("ATE_ENV_BIN")) and os.access(b, os.X_OK):
        return b
    if (cand := root / "bin/ate-env").is_file() and os.access(cand, os.X_OK):
        return str(cand)
    if which := shutil.which("ate-env"):
        return which
    cand.parent.mkdir(parents=True, exist_ok=True)
    lib.run("go", "-C", str(root), "build", "-o", str(cand), "./cmd/ate-env", capture=False)
    return str(cand)


@pytest.fixture
def smoke_env(ate_env: str, api_addr: str):
    smoke_id = f"smoke-env-{int(time.time())}"
    ns = os.environ.get("NAMESPACE", "ate-env")
    lib.run(ate_env, "create", smoke_id, "--api", api_addr, "--atespace", ns)
    try:
        yield smoke_id
    finally:
        lib.run(ate_env, "delete", smoke_id, "--api", api_addr, "--atespace", ns, check=False)


def _mcp_request(url: str, payload: dict[str, Any]) -> dict[str, Any]:
    headers = {"Content-Type": "application/json", "Accept": "application/json, text/event-stream"}
    if token := os.environ.get("SUBSTRATE_ENV_AUTH_TOKEN"):
        headers["Authorization"] = f"Bearer {token}"
    req = urllib.request.Request(url, data=json.dumps(payload).encode(), headers=headers, method="POST")
    with urllib.request.urlopen(req, timeout=30) as resp:
        body = resp.read().decode()
    data = [line[5:].strip() for line in body.splitlines() if line.strip().startswith("data:")]
    return json.loads("\n".join(data) if data else body)


def _metric_sum(bodies: list[str], pattern: str) -> float:
    total = 0.0
    for body in bodies:
        for m in re.finditer(pattern, body, re.MULTILINE):
            total += float(m.group(1))
    return total


def test_smoke(smoke_env: str, ate_env: str, api_addr: str, subtests: Any):
    ns = os.environ.get("NAMESPACE", "ate-env")
    ctx = _detect_kubectl_context()
    payload = f"smoke-{int(time.time())}"
    mcp_url = f"http://{api_addr}/v1alpha/envs/{smoke_env}/mcp?{urllib.parse.urlencode({'atespace': ns})}"

    with subtests.test("shell"):
        assert "smoke-shell-ok" in lib.run(ate_env, smoke_env, "--api", api_addr, "--atespace", ns, "shell", "uname -a && echo smoke-shell-ok").stdout

    with subtests.test("stdin"):
        assert lib.run(ate_env, smoke_env, "--api", api_addr, "--atespace", ns, "shell", "--stdin", "--timeout", "30s", "tr a-z A-Z", input="smoke-stdin").stdout.strip() == "SMOKE-STDIN"

    with subtests.test("file round-trip"):
        lib.run(ate_env, smoke_env, "--api", api_addr, "--atespace", ns, "write", "/tmp/smoke.txt", input=payload)
        assert lib.run(ate_env, smoke_env, "--api", api_addr, "--atespace", ns, "read", "/tmp/smoke.txt").stdout == payload

    with subtests.test("mcp tools/list"):
        tools = _mcp_request(mcp_url, {"jsonrpc": "2.0", "id": 1, "method": "tools/list"}).get("result", {}).get("tools", [])
        assert {"shell", "read_file"}.issubset({t.get("name") for t in tools if isinstance(t, dict)})

    with subtests.test("mcp tools/call read_file"):
        res = _mcp_request(mcp_url, {"jsonrpc": "2.0", "id": 2, "method": "tools/call", "params": {"name": "read_file", "arguments": {"path": "/tmp/smoke.txt"}}}).get("result", {})
        assert not res.get("isError")
        assert any(c.get("text") == payload for c in res.get("content", []) if isinstance(c, dict) and c.get("type") == "text")

    with subtests.test("suspend + wake-on-request"):
        lib.run(ate_env, "suspend", smoke_env, "--api", api_addr, "--atespace", ns)
        assert lib.run(ate_env, smoke_env, "--api", api_addr, "--atespace", ns, "read", "/tmp/smoke.txt").stdout == payload

    if token := os.environ.get("SUBSTRATE_ENV_AUTH_TOKEN"):
        with subtests.test("caller auth + port-proxy atespace allowlist"):
            unauth_env = dict(os.environ, SUBSTRATE_ENV_AUTH_TOKEN="")
            res = lib.run(
                ate_env, smoke_env, "--api", api_addr, "--atespace", ns, "shell", "echo unauth",
                env=unauth_env, check=False,
            )
            assert res.returncode != 0
            assert "Unauthenticated" in (res.stderr or "")

            bad_req = urllib.request.Request(
                f"http://{api_addr}/v1alpha/atespaces/{ns}/envs/{smoke_env}/ports/8080/",
                headers={"Authorization": "Bearer invalid-smoke-token"},
            )
            with pytest.raises(urllib.error.HTTPError) as bad_exc:
                urllib.request.urlopen(bad_req, timeout=15)
            assert bad_exc.value.code == 401

            expect_allowlist = os.environ.get("ATE_ENV_EXPECT_PORT_PROXY_ALLOWLIST") == "1"
            if not expect_allowlist:
                args_res = lib.run(
                    "kubectl", f"--context={ctx}", "-n", ns, "get", "deploy/ate-env-api",
                    "-o", "jsonpath={.spec.template.spec.containers[0].args[*]}",
                    check=False, capture=True,
                )
                if (
                    args_res.returncode == 0
                    and "-port-proxy-atespaces=*" not in args_res.stdout.split()
                ):
                    expect_allowlist = True

            if expect_allowlist:
                foreign_ns = "foreign-ns" if ns != "foreign-ns" else "other-ns"
                req = urllib.request.Request(
                    f"http://{api_addr}/v1alpha/atespaces/{foreign_ns}/envs/{smoke_env}/ports/8080/",
                    headers={"Authorization": f"Bearer {token}"},
                )
                with pytest.raises(urllib.error.HTTPError) as exc_info:
                    urllib.request.urlopen(req, timeout=15)
                assert exc_info.value.code == 403

    expect_metrics = os.environ.get("ATE_ENV_EXPECT_METRICS") == "1"
    if not expect_metrics:
        svc_chk = lib.run(
            "kubectl", f"--context={ctx}", "-n", ns, "get", "svc/ate-env-api",
            "-o", "jsonpath={.spec.ports[*].name}", check=False, capture=True,
        )
        expect_metrics = svc_chk.returncode == 0 and "metrics" in svc_chk.stdout.split()

    if expect_metrics:
        with subtests.test("/metrics"):
            pods_res = lib.run(
                "kubectl", f"--context={ctx}", "-n", ns, "get", "pods",
                "-l", "app=ate-env-api", "-o", "json",
                check=True, capture=True,
            )
            pods = json.loads(pods_res.stdout).get("items", [])
            ready_pods = [
                pod for pod in pods
                if not pod.get("metadata", {}).get("deletionTimestamp")
                and any(
                    c.get("type") == "Ready" and c.get("status") == "True"
                    for c in pod.get("status", {}).get("conditions", [])
                )
            ]
            assert ready_pods, f"no ready app=ate-env-api pods found in namespace {ns}"

            bodies: list[str] = []
            for pod in ready_pods:
                pod_name = pod["metadata"]["name"]
                metrics_port = next(
                    (
                        p["containerPort"]
                        for c in pod.get("spec", {}).get("containers", [])
                        for p in c.get("ports", [])
                        if p.get("name") == "metrics"
                    ),
                    9090,
                )
                res = lib.run(
                    "kubectl", f"--context={ctx}", "get", "--raw",
                    f"/api/v1/namespaces/{ns}/pods/{pod_name}:{metrics_port}/proxy/metrics",
                    check=True, capture=True,
                )
                body = res.stdout
                assert "ate_env_api_build_info{" in body
                assert "ate_env_api_environments{" in body
                bodies.append(body)

            assert _metric_sum(
                bodies,
                r'^ate_env_api_requests_total\{(?=[^}]*code="OK")'
                r'(?=[^}]*method="/ateenv\.v1alpha\.EnvironmentService/CreateEnvironment")'
                r'[^}]*\}\s+([0-9.eE+-]+)$',
            ) >= 1.0
            assert _metric_sum(
                bodies,
                rf'^ate_env_api_create_to_ready_seconds_count\{{'
                rf'(?=[^}}]*atespace="{re.escape(ns)}")(?=[^}}]*status="ok")[^}}]*\}}\s+([0-9.eE+-]+)$',
            ) >= 1.0

    with subtests.test("delete"):
        lib.run(ate_env, "delete", smoke_env, "--api", api_addr, "--atespace", ns)
