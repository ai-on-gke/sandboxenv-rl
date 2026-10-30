#!/usr/bin/env python3
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

"""End-to-end integration test pipeline for sandboxenv-rl.

What it does:
  1. Validates Docker daemon access and locates/clones agent-substrate/substrate.
  2. Builds kubectl-ate and ate-env CLI binaries and places them in bin/.
  3. Provisions target cluster (Kind by default with local registry, or reuses existing).
  4. Deploys Agent Substrate control plane components (if provisioning fresh cluster).
  5. Compiles container images (ateworker-gvisor, ate-env-api, ate-env-guest) using ko to target registry.
  6. Applies Kubernetes manifests (atespace, access policy, WorkerPool, and ate-env-api).
  7. Registers ActorTemplate with target snapshot bucket and waits for golden snapshot.
  8. Forwards a free local port and runs portable smoke test and Python SDK full-stack tests.

When it runs:
  - CI: .github/workflows/ci.yaml in job `e2e-kind` on PR and push to main.
  - Wrapped by `make ci-e2e`.
  - Nix app: `nix run .#ci-e2e` (or `nix develop -c ci-e2e`).

How to run:
  - `make ci-e2e` (preferred; auto-selects Nix or Docker)
  - `nix run .#ci-e2e` / `nix develop -c ci-e2e`
  - Direct: `python3 hack/ci/e2e.py`
  Examples:
    make ci-e2e
    KEEP_CLUSTER=1 make ci-e2e
    REUSE_CLUSTER=sbx-e2e-abc123 make ci-e2e
    LOCAL_PORT=18888 REUSE_CLUSTER=sbx-e2e-abc123 make ci-e2e

Prerequisites:
  - Docker daemon running and accessible (socket mounted if inside container).
  - Tools on PATH: go, ko, kind, kubectl, pytest (all provided in nix develop or Dockerfile.ci).

Environment variables:
  REUSE_CLUSTER                   (unset)             Existing cluster name to run against; never created or deleted
  KEEP_CLUSTER                    0                   Set to 1 to preserve cluster after run completes
  NAMESPACE                       ate-env             Kubernetes namespace for ate-env deployment
  LOCAL_PORT                      (free port)         Local port on host for ate-env-api port-forward (free port by default)
  TEMPLATE_NAME                   default-template    Name of ActorTemplate to register and test
  ATE_ENV_ROOTFS_IMAGE            (target default)    Rootfs image for test_create_from_image; empty disables
  ATE_INGRESS_ALLOWED_SPIFFE_IDS  (auto)              Allowed SPIFFE IDs for router TLS ingress
  PYTEST_BIN                      (auto)              Path to pytest binary or python test runner

Outputs / exit codes:
  - Provisions test cluster and Docker containers; publishes images to target registry.
  - Builds binaries into bin/ and generates golden snapshot.
  - Exits with 0 on pipeline success; non-zero if any provisioning, build, or test step fails.
  - Creates a Kind cluster named sbx-e2e-<8 hex> (random, not configurable; KIND_CLUSTER_NAME is
    ignored) and deletes it on completion or error unless KEEP_CLUSTER=1.

Troubleshooting:
  - See docs/chores/troubleshooting.md for common failure modes (inotify limits,
    GoldenActorCrashed, busy ports, and Substrate module resolution).

See also:
  hack/ci/targets.py, hack/ci/test_smoke.py, hack/ci/ensure-substrate.sh,
  clients/python/tests/e2e/test_full_stack.py, Makefile, nix/ci.nix.
"""

from __future__ import annotations

import argparse
import json
import os
import shutil
import socket
import sys
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent))
import lib
from targets import KindTarget


def locate_substrate(root: Path) -> Path:
    cmd = ["ensure-substrate"] if shutil.which("ensure-substrate") else [str(root / "hack/ci/ensure-substrate.sh")]
    return Path(lib.run(*cmd).stdout.strip()).resolve()


def build_cli_tools(root: Path, sub_dir: Path) -> tuple[Path, Path]:
    lib.step("Building CLI tools (kubectl-ate, ate-env)...")
    ate_ctl = root / "bin/kubectl-ate"
    ate_env = root / "bin/ate-env"
    for d, out, pkg in [(sub_dir, ate_ctl, "./cmd/kubectl-ate"), (root, ate_env, "./cmd/ate-env")]:
        out.parent.mkdir(parents=True, exist_ok=True)
        lib.run("go", "-C", str(d), "build", "-o", str(out), pkg, capture=False)

    os.environ["PATH"] = f"{root / 'bin'}:{sub_dir / 'bin'}:{os.environ.get('PATH', '')}"
    return ate_ctl, ate_env


def build_images(root: Path, sub_dir: Path, target: KindTarget) -> dict[str, str]:
    lib.step("Building container images using ko...")
    imgs: dict[str, str] = {}
    for key, c_dir, pkg in [
        ("worker", sub_dir, "./cmd/ateworker-gvisor"),
        ("api", root, "./cmd/ate-env-api"),
        ("guest", root, "./cmd/ate-env-guest"),
    ]:
        ko_env = dict(
            os.environ,
            KO_DOCKER_REPO=f"{target.registry}/{pkg.split('/')[-1]}",
            KO_DEFAULTPLATFORMS=target.platform,
        )
        out = lib.run("ko", "build", "--bare", pkg, cwd=str(c_dir), env=ko_env).stdout.strip().splitlines()
        imgs[key] = out[-1].strip()
        lib.step(f"  {key}-image: {imgs[key]}")
    return imgs


def deploy_manifests(
    target: KindTarget,
    ate_ctl: Path,
    ate_env: Path,
    ns: str,
    tmpl: str,
    imgs: dict[str, str],
) -> None:
    ctl_str, env_str = str(ate_ctl), str(ate_env)
    lib.step("Ensuring atespace and access-policy in Substrate...")
    lib.run(ctl_str, f"--context={target.context}", "create", "atespace", ns, check=False)
    pipe_res = lib.pipe(
        [env_str, "manifest", "access-policy", "--namespace", ns],
        [ctl_str, f"--context={target.context}", "apply", "access-policy", "-a", ns, "-f", "-"],
        check=False,
        capture=True,
    )
    if pipe_res.returncode != 0:
        res = lib.pipe(
            [env_str, "manifest", "access-policy", "--namespace", ns],
            [ctl_str, f"--context={target.context}", "create", "access-policy", "-a", ns, "-f", "-"],
            check=False,
            capture=True,
        )
        if res.returncode != 0 and "AlreadyExists" not in (res.stderr or ""):
            raise RuntimeError(f"Failed to create access-policy: {res.stderr}")

    lib.step(f"Applying Kubernetes manifests (namespace/{ns}, WorkerPool, Deployment/ate-env-api)...")
    lib.pipe(
        [
            env_str,
            "manifest",
            "--namespace",
            ns,
            "--template",
            tmpl,
            "--api-image",
            imgs["api"],
            "--worker-image",
            imgs["worker"],
            "--replicas",
            "2",
            # `ate-env manifest` requires a caller token by default (#104). The
            # Kind cluster is private to this run, so deploy without one rather
            # than create and plumb a token Secret; exercising caller auth in
            # E2E is a follow-up.
            "--insecure-no-auth",
        ],
        ["kubectl", f"--context={target.context}", "apply", "-f", "-"],
        check=True,
    )
    for target_deploy in ["deploy/ate-env-api", f"deploy/{tmpl}-workerpool"]:
        lib.run(
            "kubectl",
            f"--context={target.context}",
            "-n",
            ns,
            "rollout",
            "status",
            target_deploy,
            "--timeout=180s",
            capture=False,
        )

    lib.step(f"Registering {tmpl} ActorTemplate in Substrate...")
    lib.run(ctl_str, f"--context={target.context}", "delete", "actor-template", "-a", ns, tmpl, check=False)
    lib.pipe(
        [
            env_str,
            "manifest",
            "template",
            "--atespace",
            ns,
            "--template",
            tmpl,
            "--guest-image",
            imgs["guest"],
            "--snapshots-bucket",
            target.bucket_url,
        ],
        [ctl_str, f"--context={target.context}", "create", "actor-template", "-f", "-"],
        check=True,
    )

    lib.step(f"Waiting for {tmpl} golden snapshot...")

    def check_golden():
        r = lib.run(
            ctl_str,
            f"--context={target.context}",
            "get",
            "actor-template",
            "-a",
            ns,
            tmpl,
            "-o",
            "json",
            check=False,
        )
        if r.returncode == 0 and r.stdout.strip():
            st = json.loads(r.stdout).get("status", {}).get("goldenSnapshotStatus", {})
            if err := st.get("errorMessage"):
                raise RuntimeError(f"Golden snapshot failed: {err}")
            return st.get("goldenTag", {}).get("name")

    tag = lib.wait_until(check_golden, timeout=180.0, interval=2.0, what=f"{tmpl} golden snapshot")
    lib.step(f"Golden snapshot ready: {tag}")


def pick_port() -> int:
    if raw := os.environ.get("LOCAL_PORT"):
        port = int(raw)
        if lib.port_open("127.0.0.1", port):
            raise RuntimeError(
                f"LOCAL_PORT={port} is already in use on 127.0.0.1. "
                "Stop the existing listener or unset LOCAL_PORT to let e2e.py pick a free port."
            )
        return port
    with socket.socket(socket.AF_INET, socket.SOCK_STREAM) as s:
        s.bind(("127.0.0.1", 0))
        return int(s.getsockname()[1])


def run_tests(
    root: Path,
    target: KindTarget,
    ate_env: Path,
    ns: str,
    tmpl: str,
    port: int,
) -> None:
    lib.step(f"Running test suites under port-forward on 127.0.0.1:{port}...")
    with lib.port_forward(target.context, ns, "svc/ate-env-api", port):
        test_env = dict(
            os.environ,
            KUBECTL_CONTEXT=target.context,
            NAMESPACE=ns,
            LOCAL_PORT=str(port),
            ATE_ENV_API_TARGET=f"127.0.0.1:{port}",
            SUBSTRATE_ENV_API=f"127.0.0.1:{port}",
            ATE_ENV_TEMPLATE=tmpl,
            ATE_ENV_BIN=str(ate_env),
            ATE_ENV_ROOTFS_IMAGE=target.rootfs_image or "",
            PYTHONPATH=f"{root}/clients/python/src:{root}/integrations/nemo-gym/src:{os.environ.get('PYTHONPATH', '')}",
        )
        # The Kind API runs without caller auth (--insecure-no-auth above); never
        # send a token from the shell, which may belong to another cluster.
        test_env.pop("SUBSTRATE_ENV_AUTH_TOKEN", None)
        py = [
            os.environ.get("PYTEST_BIN") or shutil.which("pytest") or sys.executable,
            *([] if os.environ.get("PYTEST_BIN") or shutil.which("pytest") else ["-m", "pytest"]),
        ]
        lib.step("Running portable smoke test (7 subtests)...")
        lib.run(*py, str(root / "hack/ci/test_smoke.py"), "-v", env=test_env, capture=False)
        lib.step("Running live Python SDK integration tests...")
        lib.run(*py, str(root / "clients/python/tests/e2e/test_full_stack.py"), "-v", env=test_env, capture=False)


def main() -> None:
    parser = argparse.ArgumentParser(description="End-to-end integration test pipeline for sandboxenv-rl on Kind.")
    parser.parse_args()

    if lib.run("docker", "info", check=False).returncode != 0:
        sys.exit("ERROR: Docker daemon is not running or accessible.")

    root = Path(__file__).resolve().parent.parent.parent
    sub_dir = locate_substrate(root)

    ns = os.environ.get("NAMESPACE", "ate-env")
    port = pick_port()
    tmpl = os.environ.get("TEMPLATE_NAME", "default-template")

    target = KindTarget(sub_dir=sub_dir, namespace=ns)

    try:
        target.provision()
        ate_ctl, ate_env = build_cli_tools(root, sub_dir)
        imgs = build_images(root, sub_dir, target)
        deploy_manifests(target, ate_ctl, ate_env, ns, tmpl, imgs)
        run_tests(root, target, ate_env, ns, tmpl, port)
        lib.step("PASS: E2E Kind pipeline succeeded!")
    finally:
        target.teardown()


if __name__ == "__main__":
    main()
