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

# The devShell's Python: pkgs.python313 plus every dependency (base and all
# extras) of the pyproject.toml files below whose PEP 508 marker matches this
# interpreter, taken from nixpkgs by name, except the ones in `skipped` below:
# our own projects, and extras listed in `skippedExtras`.
#
# Also provides a minimal env for clients/python with base + dev extra only
# (no tracing/metrics) to test importorskip guards and fallbacks.
#
# Versions are not checked here, and a name nixpkgs lacks, removed or marks
# broken is left out with a warning instead of failing evaluation, so
# `make update-pins` can always run. hack/ci/check-pins.py compares the built
# env with the pyproject.toml files and says what to fix.
# See docs/chores/check-pins.md.
let
  inherit (pkgs) lib;
  inherit (pyproject-nix.lib)
    pep508
    pep621
    project
    pypa
    ;
  python = pkgs.python313;
  environ = pep508.mkEnviron python;

  clientProject = project.loadPyproject { projectRoot = ../clients/python; };
  nemoProject = project.loadPyproject { projectRoot = ../integrations/nemo-gym; };
  projects = [
    clientProject
    nemoProject
  ];

  # Dependencies that deliberately don't come from nixpkgs, with the reason
  # check-pins reports for them (it reads this through pureEnvSetup).
  #
  # Our own projects (e.g. nemo-gym-substrate depends on ate-env-client) are
  # imported from this checkout through PYTHONPATH, not packaged.
  localProjects = lib.listToAttrs (
    map (p: {
      name = pypa.normalizePackageName p.pyproject.project.name;
      value = "a project in this repository, imported from the checkout through PYTHONPATH";
    }) projects
  );
  # Extras whose packages nixpkgs doesn't have and the tests don't need, by
  # project name. Every other extra is included.
  skippedExtras = {
    # nemo-gym itself is not in nixpkgs; the tests use the local fallback types.
    nemo-gym-substrate = [ "gym" ];
  };
  extraSkips = lib.concatMapAttrs (
    projectName: extras:
    let
      p = lib.findFirst (
        p: p.pyproject.project.name == projectName
      ) (throw "nix/python.nix: skippedExtras names unknown project ${projectName}") projects;
      deps = pep621.filterDependenciesByEnviron environ [ ] p.dependencies;
    in
    lib.listToAttrs (
      lib.concatMap (
        extra:
        map (d: {
          name = d.name;
          value = "only in the optional ${extra} extra of ${projectName}, which nix/python.nix leaves out";
        }) (deps.extras.${extra} or [ ])
      ) extras
    )
  ) skippedExtras;
  skipped = localProjects // extraSkips;
  notFromNix = builtins.attrNames skipped;

  # tryEval turns removed (throwing) aliases and broken/insecure packages into
  # false; isDerivation rejects non-package attrs of the set (e.g. `pkgs`).
  usable =
    ps: name:
    let
      r = builtins.tryEval (lib.isDerivation (ps.${name} or null) && ps.${name}.drvPath != "");
    in
    r.success && r.value
    || lib.warn "nix/python.nix: ${name} left out of the Python env (not in nixpkgs, removed, or broken); run `make check-pins`" false;

  buildEnv =
    wanted:
    python.withPackages (
      ps: map (name: ps.${name}) (builtins.filter (usable ps) (lib.unique wanted)) ++ [ ps.packaging ]
    );

  wantedAll = lib.subtractLists notFromNix (
    lib.concatMap (
      p:
      let
        deps = pep621.filterDependenciesByEnviron environ [ ] p.dependencies;
      in
      map (d: d.name) (deps.dependencies ++ lib.concatLists (builtins.attrValues deps.extras))
    ) projects
  );

  clientDeps = pep621.filterDependenciesByEnviron environ [ ] clientProject.dependencies;
  wantedBase = lib.subtractLists notFromNix (
    map (d: d.name) (clientDeps.dependencies ++ (clientDeps.extras.dev or [ ]))
  );
in
{
  # The interpreter both envs are built on. nix/ci.nix reuses it for the
  # OpenHands venv, so bumping Python is this one line.
  inherit python skipped;
  # Everything: all projects, every extra. This is the devShell's Python.
  pythonEnvFull = buildEnv wantedAll;
  # clients/python base dependencies plus only its dev extra (pytest), no
  # tracing/metrics: tests the client as a user without optional extras sees it.
  pythonEnvBase = buildEnv wantedBase;
}
