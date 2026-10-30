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
  description = "Reproducible development environment and local CI runner for sandboxenv-rl";

  inputs = {
    nixpkgs.url = "github:nixos/nixpkgs?ref=nixos-unstable";
    flake-parts.url = "github:hercules-ci/flake-parts";
    pyproject-nix = {
      url = "github:pyproject-nix/pyproject.nix";
      inputs.nixpkgs.follows = "nixpkgs";
    };
  };

  outputs =
    {
      flake-parts,
      pyproject-nix,
      ...
    }@inputs:
    flake-parts.lib.mkFlake { inherit inputs; } (
      { ... }:
      {
        systems = [
          "x86_64-linux"
          "aarch64-linux"
          "aarch64-darwin"
        ];

        perSystem =
          { pkgs, ... }:
          let
            ci = import ./nix/ci.nix { inherit pkgs pyproject-nix; };
            scripts = {
              default = ci.ci;
              inherit (ci)
                ci
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
                ;
            };
          in
          {
            packages = scripts;

            devShells = {
              default = pkgs.mkShell {
                packages =
                  ci.ciPackages ++ ci.devTools ++ builtins.attrValues (builtins.removeAttrs scripts [ "default" ]);

                shellHook = ''
                  ${ci.pureEnvSetup}
                  export PATH="$PWD/bin:$PATH"
                  export KO_DOCKER_REPO="''${KO_DOCKER_REPO:-localhost:5001}"
                  export SUBSTRATE_ENV_API="''${SUBSTRATE_ENV_API:-127.0.0.1:17777}"

                  # Put go.mod's `tool` entries (buf, protoc-gen-go, ...) on PATH as
                  # shims that exec `go tool <name>`, so they always run the version
                  # go.mod pins and compile lazily on first use. Shims work from
                  # anywhere inside the repo, where `go tool` can find go.mod.
                  go_tool_bin="''${XDG_CACHE_HOME:-$HOME/.cache}/sandboxenv-rl/go-tool-bin"
                  mkdir -p "$go_tool_bin"
                  while read -r pkg; do
                    printf '#!/bin/sh\nexec go tool %s "$@"\n' "''${pkg##*/}" > "$go_tool_bin/''${pkg##*/}"
                    chmod +x "$go_tool_bin/''${pkg##*/}"
                  done < <(go list tool 2>/dev/null || true)
                  export PATH="$go_tool_bin:$PATH"
                  unset go_tool_bin pkg
                '';
              };

              openhands = ci.openhandsShell;
              uni-agent = ci.uniAgentShell;
            };
          };
      }
    );
}
