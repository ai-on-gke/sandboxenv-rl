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

"""Serve NeMo Skills' local sandbox server (POST /execute, GET /health) from
the layer: ``python3 -m nemo_runtime.sandbox_server --host 0.0.0.0 --port 6000``.

The upstream module is fetched into third_party/nemo-skills when the layer is
built (see the Dockerfile: pinned by commit and sha256) and imported unchanged.
This wrapper chooses the bind address (Flask's default is 127.0.0.1, which the
router cannot reach), the port, and how IPython session workers start."""

from __future__ import annotations

import argparse
import logging
import multiprocessing
import os
import sys
from pathlib import Path

_THIRD_PARTY = Path(__file__).resolve().parent / "third_party" / "nemo-skills"


def load_app():
    """Import the upstream Flask application under its own module name.

    Its directory goes first on sys.path, so ``local_sandbox_server`` is
    importable by name: a session worker started with the spawn method
    imports it again in a fresh interpreter (see main)."""
    if str(_THIRD_PARTY) not in sys.path:
        sys.path.insert(0, str(_THIRD_PARTY))
    import local_sandbox_server  # noqa: PLC0415 - importable only from here on

    return local_sandbox_server.app


def main(argv: list[str] | None = None) -> int:
    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument("--host", default="0.0.0.0", help="bind address; 0.0.0.0 so the router reaches it")
    ap.add_argument("--port", type=int, default=6000, help="TCP port (NeMo Skills' default is 6000)")
    args = ap.parse_args(argv)
    os.environ.setdefault("WORKER_NUM", "0")
    # `language: python` runs `python -c ...` through PATH. A task image's
    # own python wins when it has one; the layer's interpreter is the
    # fallback, so an image without python still executes code.
    layer_bin = str(Path(sys.executable).resolve().parent)
    os.environ["PATH"] = os.environ.get("PATH", "/usr/local/bin:/usr/bin:/bin") + os.pathsep + layer_bin
    logging.getLogger("werkzeug").setLevel(logging.WARNING)
    app = load_app()
    # Upstream serves this app under uWSGI, several single-threaded worker
    # processes, so a new IPython session forks from a process with no other
    # thread in it. Here one process serves many threads, because a long
    # /execute must not block /health, which the guest probes; a fork from a
    # threaded process inherits a lock another thread held at that moment,
    # and the child can hang on it (Python 3.12 warns about exactly this). So
    # session workers start with spawn: a fresh interpreter imports
    # local_sandbox_server by name (sys.path travels to the child) and runs
    # shell_worker. The session's process then lives as long as before; the
    # cost is one interpreter start per new session.
    multiprocessing.set_start_method("spawn")
    app.run(host=args.host, port=args.port, threaded=True, use_reloader=False)
    return 0


if __name__ == "__main__":
    sys.exit(main())
