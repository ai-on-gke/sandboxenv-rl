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

{
  pkgs,
  pyproject-nix,
  uv2nix,
  pyproject-build-systems,
}:

# The Python envs of the devShells and the CI scripts, built from uv.lock with
# uv2nix: every package is the exact version and wheel the lock names, so
# `uv sync` outside Nix gives the same versions. The projects of the uv
# workspace (pyproject.toml at the root) are installed editable, resolved
# from $REPO_ROOT at run time (pureEnvSetup in nix/ci.nix exports it), so
# editing their code needs no rebuild.
#
# To add or bump a dependency, edit the pyproject.toml and run `uv lock` in
# the devShell. See docs/chores/check-pins.md.
let
  inherit (pkgs) lib;

  workspace = uv2nix.lib.workspace.loadWorkspace { workspaceRoot = ../.; };

  # Wheels only, as uv picks them; the few packages without a usable wheel
  # are built from source with the overrides below.
  lockOverlay = workspace.mkPyprojectOverlay { sourcePreference = "wheel"; };

  # uv.lock doesn't record build systems (astral-sh/uv#5190), so each package
  # built from source names its own. Only the ones the envs below need.
  buildSystemOverrides =
    final: prev:
    let
      withBuildSystem =
        name: spec:
        prev.${name}.overrideAttrs (old: {
          nativeBuildInputs = old.nativeBuildInputs ++ final.resolveBuildSystem spec;
        });
    in
    {
      # git source (integrations/uni-agent's uni-agent dependency group)
      uni-agent = withBuildSystem "uni-agent" {
        setuptools = [ ];
        wheel = [ ];
      };
      # sdist only, from openhands-tools
      func-timeout = withBuildSystem "func-timeout" { setuptools = [ ]; };
    };

  # Workspace projects, editable. Their sources are cut down to what the
  # editable build reads (metadata and the top-level package), so only a
  # metadata change rebuilds them. Dockerfile.ci copies the same files into
  # its Nix layer.
  editableOverlay = workspace.mkEditablePyprojectOverlay { root = "$REPO_ROOT"; };
  # Each workspace project and its top-level package.
  members = {
    ate-env-client = "ate_env";
    nemo-gym-substrate = "nemo_gym_substrate";
    ate-env-openhands = "ate_env_openhands";
    uni-agent-substrate = "uni_agent_substrate";
    ate-env-harbor = "ate_env_harbor";
  };
  editableSources =
    _final: prev:
    lib.mapAttrs (
      name: package:
      prev.${name}.overrideAttrs (old: {
        src = lib.fileset.toSource {
          root = old.src;
          fileset = lib.fileset.unions [
            (old.src + "/pyproject.toml")
            (old.src + "/README.md")
            (old.src + "/src/${package}/__init__.py")
          ];
        };
      })
    ) members;

  mkPythonSet =
    python:
    (pkgs.callPackage pyproject-nix.build.packages { inherit python; }).overrideScope (
      lib.composeManyExtensions [
        pyproject-build-systems.overlays.wheel
        lockOverlay
        buildSystemOverrides
        editableOverlay
        editableSources
      ]
    );

  # The default interpreter. uni-agent upstream declares requires-python
  # <3.13, so its env stays on 3.12.
  python = pkgs.python313;
  pythonSet = mkPythonSet python;
  python312Set = mkPythonSet pkgs.python312;

  # Every extra of a workspace project, as listed in its pyproject.toml.
  allExtras = name: workspace.deps.optionals.${name};
in
{
  inherit python;

  # Everything for the client and nemo-gym-substrate: every extra, except
  # nemo-gym's `gym` (the real nemo-gym; its tests use local fallback types,
  # and it pulls in ray). This is the devShell's Python.
  pythonEnvFull = pythonSet.mkVirtualEnv "sandboxenv-rl-python" {
    ate-env-client = allExtras "ate-env-client";
    nemo-gym-substrate = lib.remove "gym" (allExtras "nemo-gym-substrate");
  };

  # clients/python base dependencies plus only its dev extra (pytest), no
  # tracing/metrics: tests the client as a user without optional extras sees it.
  # Flake package python-env-base; ci-python builds it on first use, so the
  # default devShell doesn't carry it.
  pythonEnvBase = pythonSet.mkVirtualEnv "sandboxenv-rl-python-base" {
    ate-env-client = [ "dev" ];
  };

  # integrations/openhands with its test extra (the agent-server the tests
  # start); the .#openhands devShell and `make ci-openhands`.
  openhandsEnv = pythonSet.mkVirtualEnv "sandboxenv-rl-python-openhands" {
    ate-env-client = [ "dev" ];
    ate-env-openhands = [ "test" ];
  };

  # integrations/harbor with its test extra (Harbor's own trial runner on this
  # provider); the .#harbor devShell and `make ci-harbor`.
  harborEnv = pythonSet.mkVirtualEnv "sandboxenv-rl-python-harbor" {
    ate-env-client = [ "dev" ];
    ate-env-harbor = [ "test" ];
  };

  # integrations/uni-agent with its test extra and the uni-agent dependency
  # group (uni-agent itself, from git), on Python 3.12; the .#uni-agent
  # devShell, and ci-python's uni-agent suite (flake package
  # python-env-uni-agent, built on first use like python-env-base).
  uniAgentEnv = python312Set.mkVirtualEnv "sandboxenv-rl-python-uni-agent" {
    ate-env-client = [ "dev" ];
    uni-agent-substrate = [
      "test"
      "uni-agent"
    ];
  };
}
