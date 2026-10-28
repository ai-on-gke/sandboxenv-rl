# NeMo Skills local sandbox server

`local_sandbox_server.py` is not in this repository. The layer's Dockerfile
fetches it when the image is built, from https://github.com/NVIDIA/NeMo-Skills,
path `nemo_skills/code_execution/local_sandbox/local_sandbox_server.py`, at
commit `bcf059af55c20a89f797724598f9908d126153e6` (2026-09-11), and checks its
sha256 (`c79197dc02e33b63b58b30783e3cfe0076e8edf6ceb409835b5d0762756b621d`),
so the file in the image is byte-identical to upstream; `NEMO_SKILLS_COMMIT`
and `NEMO_SKILLS_SERVER_SHA256` in the Dockerfile are the pins. Licensed by
NVIDIA under Apache-2.0 (`LICENSE`). It is the Flask application behind NeMo
Skills' `LocalSandbox` client: `POST /execute` with `generated_code`,
`language` (`ipython`, `python`, `shell`, `lean4`), `timeout`, optional
`std_input` and `session_id`; `GET /health`; and the IPython session routes.
`nemo_runtime.sandbox_server` imports it from this directory and serves it on
the host and port the sidecar command names. To update, set the two pins in
the Dockerfile to the new commit and the new file's sha256, and re-lock
`requirements.in` against NeMo Skills' `requirements/sandbox.lock`.
