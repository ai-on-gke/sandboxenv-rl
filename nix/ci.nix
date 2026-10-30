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

{ pkgs, pyproject-nix }:

let
  # Go comes from go.mod: its `go` directive picks the nixpkgs package with
  # the same major.minor (go 1.27.5 -> pkgs.go_1_27). The patch level is
  # whatever nixpkgs ships; check-pins fails if it is below go.mod's.
  goModVersion =
    let
      lines = pkgs.lib.splitString "\n" (builtins.readFile ../go.mod);
      matches = builtins.filter (m: m != null) (
        map (builtins.match "go ([0-9]+)\\.([0-9]+)(\\.[0-9]+)?[[:space:]]*") lines
      );
    in
    if matches == [ ] then throw "nix/ci.nix: go.mod has no `go` directive" else builtins.head matches;
  goAttr = "go_${builtins.elemAt goModVersion 0}_${builtins.elemAt goModVersion 1}";
  goToolchain =
    pkgs.${goAttr} or (throw ''
      nix/ci.nix: go.mod asks for Go ${builtins.elemAt goModVersion 0}.${builtins.elemAt goModVersion 1}, but the locked nixpkgs has no pkgs.${goAttr}.
      Lower the `go` directive in go.mod to a Go version the locked nixpkgs packages, or wait until nixpkgs packages Go ${builtins.elemAt goModVersion 0}.${builtins.elemAt goModVersion 1}.
      If a newer nixpkgs already has it, run `make update-pins ARGS=nixpkgs` (or `nix flake update nixpkgs`) to move the lock forward.
    '');

  pythonEnvs = import ./python.nix { inherit pkgs pyproject-nix; };
  inherit (pythonEnvs) pythonEnvFull pythonEnvBase;
  # Interpreters for the pip venvs (ci-venv and the per-project devShells).
  # The default one is the same interpreter nix/python.nix builds on.
  python313Bare = pythonEnvs.python;
  # uni-agent upstream declares requires-python <3.13, so its venv stays on 3.12.
  python312Bare = pkgs.python312;

  # uni-agent is not on PyPI: install it from this pinned commit, with --no-deps,
  # plus the dependencies upstream CI installs next to it. Shared by the
  # .#uni-agent devShell and ci-python's uni-agent suite.
  uniAgentRev = "174b040e541f70234a22930a3f2ff8d1e2be5399";
  uniAgentPreInstalls = [
    "--no-deps git+https://github.com/verl-project/uni-agent@${uniAgentRev}"
    "pydantic pyyaml httpx jsonschema openai"
  ];

  # PyPI wheels with native code expect FHS libraries that Nix's Python can't
  # find. Only what our venvs have needed so far; see nix-ld's list for
  # others to add:
  # https://wiki.nixos.org/wiki/Python (the nix-ld / buildFHSEnv sections)
  ciVenvLibPath = pkgs.lib.makeLibraryPath [
    pkgs.stdenv.cc.cc.lib
    pkgs.zlib
  ];

  ciPackages = with pkgs; [
    # Go toolchain selected from go.mod's `go` directive (see goToolchain)
    goToolchain
    golangci-lint

    # Container, image, signing, and Kubernetes tooling
    ko
    crane
    kind
    kubectl
    docker-client

    # Python build, testing, and language tooling (ruff provided by pythonEnvFull dev extra)
    pythonEnvFull

    # Prettier for Markdown documentation formatting
    prettier

    # Utilities, build tools, and CI scripting
    stdenv.cc
    cacert
    coreutils
    gnused
    gawk
    findutils
    gnugrep
    curl
    jq
    flake-checker
    shellcheck
    actionlint
    zizmor
    git
    gnumake
  ];

  devTools = with pkgs; [
    gopls
  ];

  pureEnvSetup = ''
    if [[ -z "''${HOME:-}" || "''${HOME}" == "/homeless-shelter" || ! -w "''${HOME}" ]]; then
      UID_VAL="$(id -u)"
      HOME="''${TMPDIR:-/tmp}/sandboxenv-rl-home-''${UID_VAL}"
      export HOME
      mkdir -p "''${HOME}"
    fi
    XDG_CACHE_HOME="''${XDG_CACHE_HOME:-''${HOME}/.cache}"
    export XDG_CACHE_HOME
    mkdir -p "''${XDG_CACHE_HOME}"
    if [[ -z "''${USER:-}" ]]; then
      USER="$(id -un 2>/dev/null || echo runner)"
      export USER
    fi
    export SSL_CERT_FILE="''${SSL_CERT_FILE:-${pkgs.cacert}/etc/ssl/certs/ca-bundle.crt}"
    export NIX_SSL_CERT_FILE="''${NIX_SSL_CERT_FILE:-''${SSL_CERT_FILE}}"
    export GIT_SSL_CAINFO="''${GIT_SSL_CAINFO:-''${SSL_CERT_FILE}}"
    export GOTOOLCHAIN="local"
    export PYTHONPATH="$PWD/clients/python/src:$PWD/integrations/nemo-gym/src:''${PYTHONPATH:-}"
    export PYTHONNOUSERSITE=1
    export PYTHONUNBUFFERED=1
    # Which pyproject dependencies nix/python.nix deliberately doesn't take from
    # nixpkgs, and why; hack/ci/check-pins.py reports these as skipped.
    export SANDBOXENV_NIX_PYTHON_SKIPPED=${pkgs.lib.escapeShellArg (builtins.toJSON pythonEnvs.skipped)}
  '';

  mkHackScript =
    name: script:
    pkgs.writeShellApplication {
      inherit name;
      runtimeInputs = ciPackages;
      text = ''
        cd "$(git rev-parse --show-toplevel 2>/dev/null || pwd)"
        ${pureEnvSetup}
        exec ${script} "$@"
      '';
    };

  ci-go = pkgs.writeShellApplication {
    name = "ci-go";
    runtimeInputs = ciPackages;
    text = ''
      set -euo pipefail
      cd "$(git rev-parse --show-toplevel 2>/dev/null || pwd)"
      ${pureEnvSetup}

      echo "=== [sandboxenv-rl] Go Build, Vet, Test (-race), Format & Protos ==="
      echo "--> Go build..."
      go build ./...
      echo "--> Go vet..."
      go vet ./...
      echo "--> Go test (-race)..."
      go test -race ./...
      echo "--> Format & boilerplate..."
      make verify-gofmt
      make verify-boilerplate
      make PRETTIER=prettier verify-docs
      echo "--> Protobuf stub verification..."
      make verify-protos
      echo "--> docs/metrics.md is generated from internal/telemetry..."
      metrics_md="$(mktemp)"
      go run ./hack/metrics-docs >"$metrics_md"
      diff -u docs/metrics.md "$metrics_md"
      rm -f "$metrics_md"
      echo "=== [sandboxenv-rl] Go CI checks passed ==="
    '';
  };

  mkVenvSetupCode = ''
    repo_root="$(git rev-parse --show-toplevel 2>/dev/null || pwd)"
    if [[ "$project_dir" = /* ]]; then
      project_path="$project_dir"
    else
      project_path="$repo_root/$project_dir"
    fi
    project_name="$(basename "$project_dir")"
    project_pyproject="$project_path/pyproject.toml"
    client_pyproject="$repo_root/clients/python/pyproject.toml"
    python_real="$(realpath "$python_bin")"

    hash_input() {
      printf '%s\n' "$python_real"
      if [[ -f "$project_pyproject" ]]; then
        cat "$project_pyproject"
      fi
      if [[ -f "$client_pyproject" ]]; then
        cat "$client_pyproject"
      fi
      printf '%s\n' "$extras"
      for pi in "''${pre_installs[@]}"; do
        printf '%s\n' "$pi"
      done
    }

    # The devShell's PYTHONPATH points at this repo's source trees; pip would
    # treat those as installed. The venv must only see what pip installs.
    unset PYTHONPATH

    hash_val="$(hash_input | sha256sum | cut -c1-12)"
    venv_dir="''${XDG_CACHE_HOME}/sandboxenv-rl/venvs/''${project_name}-''${hash_val}"

    if [[ -f "$venv_dir/.installed" ]]; then
      echo "ci-venv: reusing cached virtual environment at $venv_dir"
    else
      echo "ci-venv: creating virtual environment at $venv_dir"
      rm -rf "$venv_dir"
      mkdir -p "$(dirname "$venv_dir")"
      "$python_bin" -m venv "$venv_dir"
      for pi in "''${pre_installs[@]}"; do
        read -r -a pi_args <<< "$pi"
        "$venv_dir/bin/pip" install "''${pi_args[@]}"
      done
      if [[ -n "$extras" ]]; then
        "$venv_dir/bin/pip" install -e "$repo_root/clients/python[dev]" -e "''${project_path}[''${extras}]"
      else
        "$venv_dir/bin/pip" install -e "$repo_root/clients/python[dev]" -e "''${project_path}"
      fi
      touch "$venv_dir/.installed"
    fi

    # Dev tools come from nixpkgs, ahead of the venv: the flake pins their
    # versions, and PyPI's ruff is a standalone binary that needs an FHS
    # dynamic loader, which the Docker image doesn't have.
    PATH="${pkgs.ruff}/bin:''${venv_dir}/bin:''${PATH}"
    export PATH
    VIRTUAL_ENV="''${venv_dir}"
    export VIRTUAL_ENV
    LD_LIBRARY_PATH="${ciVenvLibPath}''${LD_LIBRARY_PATH:+:''${LD_LIBRARY_PATH}}"
    export LD_LIBRARY_PATH
  '';

  mkVenvShell =
    {
      projectDir,
      python ? python313Bare,
      extras ? "",
      preInstalls ? [ ],
    }:
    let
      preInstallsBash = pkgs.lib.concatMapStringsSep "\n" (
        pi: "pre_installs+=(${pkgs.lib.escapeShellArg pi})"
      ) preInstalls;
    in
    pkgs.mkShell {
      packages = [
        python
        pkgs.git
        pkgs.coreutils
        pkgs.findutils
        pkgs.gnugrep
        pkgs.gnused
        pkgs.stdenv.cc
        pkgs.cacert
      ];

      shellHook = ''
        ${pureEnvSetup}
        python_bin="${python}/bin/python3"
        project_dir="${projectDir}"
        extras="${extras}"
        pre_installs=()
        ${preInstallsBash}
        ${mkVenvSetupCode}
      '';
    };

  openhandsShell = mkVenvShell {
    projectDir = "integrations/openhands";
    python = python313Bare;
    extras = "test";
  };

  uniAgentShell = mkVenvShell {
    projectDir = "integrations/uni-agent";
    python = python312Bare;
    extras = "test";
    preInstalls = uniAgentPreInstalls;
  };

  ci-venv = pkgs.writeShellApplication {
    name = "ci-venv";
    runtimeInputs = ciPackages ++ [
      python312Bare
      python313Bare
    ];
    text = ''
      # Manage and execute commands within isolated Python virtual environments.
      #
      # What it does:
      #   1. Resolves repository root and parses project directory, extras, and pre-install options.
      #   2. Computes a cache hash over the Python interpreter path, pyproject.toml files, extras, and pre-installs.
      #   3. Creates an isolated venv using the flake's pinned Python interpreter (pkgs.python313 by default,
      #      or pkgs.python312 via --python 3.12) under $XDG_CACHE_HOME/sandboxenv-rl/venvs/<project>-<hash>,
      #      reusing it if present.
      #   4. Installs clients/python[dev] and the target project (with extras) in editable mode,
      #      plus any pre-install requirements.
      #   5. Runs the requested command inside the venv with LD_LIBRARY_PATH scoped for native wheels.
      #
      # When it runs:
      #   - CI: OpenHands and uni-agent integration workflows.
      #   - Makefile: `make ci-openhands`.
      #   - Flake app: `nix run .#ci-venv -- ...` or `ci-venv ...` in devShell.
      #
      # How to run:
      #   - `ci-venv <project-dir> [--python 3.12|3.13] [--extras a,b] [--pre-install <pip args>]... -- <command...>`
      #   Examples:
      #     ci-venv integrations/openhands --extras test -- sh -c 'python -m pytest integrations/openhands/tests -q && ruff check integrations/openhands examples/openhands'
      #     ci-venv integrations/uni-agent --python 3.12 --extras test \
      #       --pre-install "--no-deps git+https://github.com/verl-project/uni-agent@<rev>" \
      #       --pre-install "pydantic pyyaml httpx jsonschema openai" \
      #       -- sh -c 'python -m pytest integrations/uni-agent/tests -q && ruff check integrations/uni-agent'
      #
      # Prerequisites:
      #   - Network access to PyPI and GitHub (if pre-install requires git clone) during initial venv creation.
      #
      # Environment variables:
      #   XDG_CACHE_HOME   ''${HOME}/.cache  Root cache directory for persistent venvs
      #   LD_LIBRARY_PATH  (appended)        Scoped FHS libraries for native PyPI wheels
      #
      # Outputs / exit codes:
      #   - Passes through exit code from the executed command.
      #   - Exits non-zero if venv creation, pip install, or target command fails.

      set -euo pipefail
      ${pureEnvSetup}

      if [[ $# -lt 1 ]]; then
        echo "Usage: ci-venv <project-dir> [--python 3.12|3.13] [--extras a,b] [--pre-install <pip args>]... -- <command...>" >&2
        exit 1
      fi

      project_dir=""
      python_ver="3.13"
      extras=""
      pre_installs=()

      while [[ $# -gt 0 ]]; do
        case "$1" in
          --python)
            python_ver="$2"
            shift 2
            ;;
          --extras)
            extras="$2"
            shift 2
            ;;
          --pre-install)
            pre_installs+=("$2")
            shift 2
            ;;
          --)
            shift
            break
            ;;
          -*)
            echo "ci-venv: unrecognized argument: $1" >&2
            exit 1
            ;;
          *)
            if [[ -z "$project_dir" ]]; then
              project_dir="$1"
              shift
            else
              echo "ci-venv: unexpected positional argument: $1" >&2
              exit 1
            fi
            ;;
        esac
      done

      if [[ -z "$project_dir" ]]; then
        echo "ci-venv: missing required <project-dir>" >&2
        exit 1
      fi

      if [[ $# -eq 0 ]]; then
        echo "ci-venv: no command specified after '--'" >&2
        exit 1
      fi

      case "$python_ver" in
        3.12)
          python_bin="${python312Bare}/bin/python3"
          ;;
        3.13)
          python_bin="${python313Bare}/bin/python3"
          ;;
        *)
          echo "ci-venv: unsupported Python version '$python_ver' (expected 3.12 or 3.13)" >&2
          exit 1
          ;;
      esac

      ${mkVenvSetupCode}

      exec "$@"
    '';
  };

  ci-python = pkgs.writeShellApplication {
    name = "ci-python";
    runtimeInputs = ciPackages ++ [ ci-venv ];
    text = ''
      set -euo pipefail
      cd "$(git rev-parse --show-toplevel 2>/dev/null || pwd)"
      ${pureEnvSetup}

      echo "=== [sandboxenv-rl] Python Lint & Test Suites ==="
      echo "--> Python ruff check..."
      ruff check clients/python examples/verl_swebench
      ruff check --config clients/python/pyproject.toml examples/Qwen3-1.7B-verl
      ruff check benchmarking/telemetry_report
      # The base-env suites only mean something if the base env really lacks
      # the optional extras. A Python package added to the devShell can leak
      # every Python input onto PYTHONPATH (nixpkgs' Python setup hook), which
      # would quietly turn them into full-env runs, so check first.
      echo "--> Checking the base env can't import the tracing/metrics extras..."
      if ! ${pythonEnvBase}/bin/python -c '
      import importlib.util, sys
      leaked = [m for m in ("opentelemetry", "prometheus_client") if importlib.util.find_spec(m)]
      if leaked:
          print("pythonEnvBase can import " + ", ".join(leaked) + "; PYTHONPATH=" + repr(__import__("os").environ.get("PYTHONPATH", "")))
          sys.exit(1)
      '; then
        echo "ERROR: the base env sees packages it must not have, so the base-env suites would test nothing extra." >&2
        echo "Something in the devShell probably put Python site-packages on PYTHONPATH; check \`nix develop -c sh -c 'echo \$PYTHONPATH'\`." >&2
        exit 1
      fi
      echo "--> Python pytest suites (in parallel)..."
      # Each suite writes its own log; logs print in a fixed order afterwards so
      # the output doesn't interleave. The base-env client run shares a rootdir
      # with the full one, so it skips the pytest cache to avoid racing on it.
      logs="$(mktemp -d)"
      trap 'rm -rf "$logs"' EXIT
      names=()
      pids=()
      suite() {
        local name="$1" dir="$2"
        shift 2
        (cd "$dir" && "$@") >"$logs/''${#pids[@]}.log" 2>&1 &
        names+=("$name")
        pids+=("$!")
      }
      suite "clients/python (full env)" clients/python pytest -q
      suite "clients/python (base env, no tracing/metrics)" clients/python \
        ${pythonEnvBase}/bin/python -m pytest -q -p no:cacheprovider
      suite "examples/Qwen3-1.7B-verl" . pytest examples/Qwen3-1.7B-verl/tests -q
      suite "integrations/nemo-gym" integrations/nemo-gym pytest -q
      suite "integrations/uni-agent" . ci-venv integrations/uni-agent \
        --python 3.12 \
        --extras test \
        ${
          pkgs.lib.concatMapStringsSep " " (
            pi: "--pre-install ${pkgs.lib.escapeShellArg pi}"
          ) uniAgentPreInstalls
        } \
        -- sh -c 'python -m pytest integrations/uni-agent/tests -q && ruff check integrations/uni-agent'
      suite "swebench" . pytest internal/benchmarking/swebench/trajectories benchmarking/swebench -q
      # The report tool runs on both envs, like the client: its end-to-end test
      # traces a fleet and needs the telemetry extras.
      suite "benchmarking/telemetry_report (full env)" . pytest benchmarking/telemetry_report -q -p no:cacheprovider
      suite "benchmarking/telemetry_report (base env)" . \
        ${pythonEnvBase}/bin/python -m pytest benchmarking/telemetry_report -q -p no:cacheprovider

      failed=0
      for i in "''${!pids[@]}"; do
        if wait "''${pids[$i]}"; then status=ok; else status=FAILED; failed=1; fi
        echo "--> ''${names[$i]}: $status"
        cat "$logs/$i.log"
      done
      rm -rf "$logs"
      if [[ "$failed" -ne 0 ]]; then
        echo "=== [sandboxenv-rl] Python CI checks FAILED ===" >&2
        exit 1
      fi
      echo "=== [sandboxenv-rl] Python CI checks passed ==="
    '';
  };

  ci = pkgs.writeShellApplication {
    name = "ci";
    runtimeInputs = [
      ci-go
      ci-python
    ];
    text = ''
      set -euo pipefail
      echo "=============================================="
      echo "Running sandboxenv-rl local CI verification"
      echo "=============================================="
      ci-go
      ci-python
      echo "=============================================="
      echo "All sandboxenv-rl local CI checks passed!"
      echo "=============================================="
    '';
  };

  ci-all = ci;

  ensure-substrate = mkHackScript "ensure-substrate" "./hack/ci/ensure-substrate.sh";
  ci-e2e = mkHackScript "ci-e2e" "./hack/ci/e2e.py";
  smoke = mkHackScript "smoke" "pytest hack/ci/test_smoke.py -v";
  check-substrate-head = mkHackScript "check-substrate-head" "./hack/ci/check-substrate-head.sh";
  # Pin consistency (hack/ci/check-pins.py), then DeterminateSystems'
  # flake-checker for nixpkgs freshness (< 30 days), upstream owner and
  # supported branch. flake-checker only reports; it does not fail the run.
  check-pins = pkgs.writeShellApplication {
    name = "check-pins";
    runtimeInputs = ciPackages;
    text = ''
      cd "$(git rev-parse --show-toplevel 2>/dev/null || pwd)"
      ${pureEnvSetup}
      ./hack/ci/check-pins.py "$@"
      flake-checker --no-telemetry
    '';
  };
  # update-pins has its own minimal runtimeInputs independent of goToolchain and
  # Python environments so that it evaluates and runs even when the locked
  # nixpkgs cannot evaluate the devShell (for example, when go.mod requests a Go
  # minor version not yet packaged in nixpkgs, which is precisely when
  # `make update-pins ARGS=nixpkgs` is needed to recover).
  update-pins = pkgs.writeShellApplication {
    name = "update-pins";
    runtimeInputs = [
      pkgs.python3
      pkgs.crane
      pkgs.git
      pkgs.coreutils
    ];
    text = ''
      cd "$(git rev-parse --show-toplevel 2>/dev/null || pwd)"
      ${pureEnvSetup}
      exec ./hack/ci/check-pins.py update "$@"
    '';
  };
  lint-actions-locally = mkHackScript "lint-actions-locally" "./hack/ci/lint-actions-locally.sh";
in
{
  inherit
    ciPackages
    devTools
    pureEnvSetup
    ci
    ci-all
    ci-go
    ci-python
    ci-venv
    ci-e2e
    smoke
    check-substrate-head
    ensure-substrate
    check-pins
    update-pins
    lint-actions-locally
    openhandsShell
    uniAgentShell
    ;
}
