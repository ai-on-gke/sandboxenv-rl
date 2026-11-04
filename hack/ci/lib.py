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

"""Shared Python test and automation helpers for sandboxenv-rl CI scripts.

What it does:
  1. Provides `run()`: executes commands with text output, stream capture, and error details.
  2. Provides `pipe()`: pipes output of process 1 into process 2 with exit code validation.
  3. Provides `port_open()`: tests TCP socket accessibility with configurable timeout.
  4. Provides `wait_until()`: polls predicate callable until condition is met or timeout.
  5. Provides `port_forward()`: context manager running `kubectl port-forward` in the background.
  6. Provides `step()`: prints formatted stage announcement banner.

When it runs:
  Imported as a library module by hack/ci/e2e.py and hack/ci/test_smoke.py.

How to run:
  Not directly executable. Imported in Python scripts:
    import lib
    lib.run("kubectl", "get", "pods")
    with lib.port_forward(ctx, ns, svc, local_port): ...

Prerequisites:
  - Python 3.10+.
  - kubectl on PATH (for port_forward helper).

Environment variables:
  None read directly by lib.py; callers may pass custom env dictionaries to `run()` or `pipe()`.

Outputs / exit codes:
  - Helper functions return CompletedProcess or yield address string.
  - Raises RuntimeError on command/pipe failures or TimeoutError on polling timeouts.

Troubleshooting:
  - See docs/chores/troubleshooting.md#port-forward-startup-failures for port-forward
    diagnostics and debugging.

See also:
  hack/ci/e2e.py, hack/ci/test_smoke.py.
"""

from __future__ import annotations

import os
import socket
import subprocess
import tempfile
import time
from collections.abc import Callable
from contextlib import contextmanager
from typing import Any


def step(msg: str) -> None:
    print(f"==> {msg}", flush=True)


def run(*argv: Any, input: str | None = None, cwd: str | None = None, env: dict[str, str] | None = None, capture: bool = True, check: bool = True) -> subprocess.CompletedProcess[str]:
    cmd = [str(a) for a in argv]
    res = subprocess.run(cmd, input=input, text=True, cwd=cwd, env=env, stdout=subprocess.PIPE if capture else None, stderr=subprocess.PIPE if capture else None, check=False)
    if check and res.returncode != 0:
        err = f"Command failed ({res.returncode}): {' '.join(cmd)}"
        if capture and res.stderr:
            err += f"\n{res.stderr.strip()}"
        raise RuntimeError(err)
    return res


def pipe(p_argv: list[Any], c_argv: list[Any], cwd: str | None = None, env: dict[str, str] | None = None, capture: bool = False, check: bool = True) -> subprocess.CompletedProcess[str]:
    p1 = subprocess.Popen([str(a) for a in p_argv], stdout=subprocess.PIPE, text=True, cwd=cwd, env=env)
    p2 = subprocess.Popen([str(a) for a in c_argv], stdin=p1.stdout, stdout=subprocess.PIPE if capture else None, stderr=subprocess.PIPE if capture else None, text=True, cwd=cwd, env=env)
    if p1.stdout:
        p1.stdout.close()
    out, err = p2.communicate()
    p1_code = p1.wait()
    if check and (p1_code != 0 or p2.returncode != 0):
        raise RuntimeError(f"Pipe failed: {' '.join(str(x) for x in p_argv)} | {' '.join(str(x) for x in c_argv)}\n{err or ''}")
    return subprocess.CompletedProcess([str(a) for a in c_argv], p2.returncode, stdout=out, stderr=err)


def port_open(host: str, port: int, timeout: float = 0.5) -> bool:
    try:
        with socket.create_connection((host, int(port)), timeout=timeout):
            return True
    except OSError:
        return False


def wait_until(pred: Callable[[], Any], timeout: float = 30.0, interval: float = 0.5, what: str = "condition") -> Any:
    deadline = time.monotonic() + timeout
    while time.monotonic() < deadline:
        if res := pred():
            return res
        time.sleep(interval)
    raise TimeoutError(f"Timed out waiting for {what} after {timeout}s")


@contextmanager
def port_forward(
    context: str,
    namespace: str,
    svc: str,
    local_port: int,
    target_port: int = 7777,
):
    """Starts `kubectl port-forward` in the background as a context manager.

    Refuses to start if local_port is already in use. Callers that wish to reuse
    an existing listener should check port_open beforehand.
    """
    host = "127.0.0.1"
    if port_open(host, local_port):
        raise RuntimeError(
            f"Port {host}:{local_port} is already in use. "
            f"Cannot start port-forward to {svc} in namespace '{namespace}' (context '{context}'). "
            "Stop the process using the port, or pick a free port."
        )

    cmd = ["kubectl", f"--context={context}", "-n", namespace, "port-forward", svc, f"{local_port}:{target_port}"]
    with tempfile.NamedTemporaryFile(mode="w+", delete=False, prefix="pf-", suffix=".log") as f:
        log_path = f.name
        proc = subprocess.Popen(cmd, stdout=f, stderr=subprocess.STDOUT, text=True)
    try:
        def check():
            if proc.poll() is not None:
                with open(log_path) as lf:
                    raise RuntimeError(f"port-forward exited early ({proc.returncode}):\n{lf.read()}")
            return port_open(host, local_port)
        wait_until(check, timeout=30.0, interval=0.5, what=f"port-forward {local_port}")
        yield f"{host}:{local_port}"
    finally:
        if proc.poll() is None:
            proc.terminate()
            proc.wait()
        try:
            os.unlink(log_path)
        except OSError:
            pass
