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

"""Verify consistency between Nix toolchain/package pins and repository definitions.

What it does:
  1. Reads the nixpkgs revision and date from flake.lock (shown in every mismatch message).
  2. Compares Go toolchain directives in go.mod against Nix go package version.
  3. Checks Python interpreter requirements (pyproject.toml) against Nix python version.
  4. Checks every requirement in the pyproject.toml files whose packages come from Nix
     (PYPROJECTS: clients/python, integrations/nemo-gym) against the installed Nix Python env
     (importlib.metadata). OpenHands and uni-agent are not checked: they run in pip venvs
     (ci-venv / .#openhands / .#uni-agent) whose PyPI dependencies float by design, and pip
     itself enforces their requires-python against the venv interpreter.
  5. Verifies Dockerfile.ci base image is pinned to an immutable @sha256: digest.
  6. In update mode, runs `nix flake update`, re-pins Dockerfile.ci via crane, and re-checks.

When it runs:
  - CI: .github/workflows/ci-devenv.yaml on PRs touching toolchain files, push to main, and weekly cron.
  - Wrapped by `make check-pins` and `make update-pins`.
  - Nix apps: `nix run .#check-pins` and `nix run .#update-pins`.

How to run:
  - `make check-pins` (preferred check; auto-selects Nix or Docker)
  - `make update-pins` (preferred update; updates flake inputs and Dockerfile.ci)
  - `nix run .#check-pins` / `nix run .#update-pins`
  - Direct: `python3 hack/ci/check-pins.py [check|update|versions]`
  Examples:
    make check-pins
    make update-pins
    python3 hack/ci/check-pins.py update nixpkgs
    python3 hack/ci/check-pins.py versions --json

Prerequisites:
  - Python 3.11+ with `packaging` (the Nix devShell python) for check mode.
  - For update mode: Nix with flakes enabled and internet access (crane used via nixpkgs).

Environment variables:
  GITHUB_ACTIONS       (unset)      When set to "true", outputs GitHub Actions workflow annotations
  GITHUB_STEP_SUMMARY  (unset)      Path to GitHub Actions step summary markdown file for table reports

Outputs / exit codes:
  - Prints the pin check results (or, in update mode, a before/after version table) to stdout.
  - Emits workflow annotations and appends markdown report to $GITHUB_STEP_SUMMARY when in CI.
  - Mutates flake.lock and Dockerfile.ci in update mode.
  - Exit code 0 if all checks pass; exit code 1 on pin mismatch or update failure.

Troubleshooting:
  - Base image resolution: Resolves the multi-arch digest for `nixos/nix:latest` via crane
    from Docker Hub and re-pins Dockerfile.ci.
  - Python dependency mismatch: If pyproject.toml requires a version unavailable in Nix, either
    update flake inputs via `make update-pins` or relax the pin constraint in pyproject.toml.
  - Stale nixpkgs: freshness is not checked here. `make check-pins` runs DeterminateSystems'
    flake-checker afterwards, which reports inputs older than 30 days; refresh with
    `make update-pins`.
  - Lock evaluation recovery: In update mode, before-versions are recorded via `nix develop`.
    If the old flake.lock or devShell fails evaluation (e.g. when go.mod specifies a Go minor
    version nixpkgs lacks), update continues with unknown before-versions and prints a note rather
    than aborting, allowing recovery via `make update-pins ARGS=nixpkgs`.

See also:
  .github/workflows/ci-devenv.yaml, flake.lock, Dockerfile.ci, nix/ci.nix, nix/python.nix, Makefile.
"""

import argparse
import dataclasses
import datetime
import json
import os
import pathlib
import re
import shutil
import subprocess
import sys


def run(
    argv: list[str], *, check: bool = True, **kwargs
) -> subprocess.CompletedProcess[str]:
    """Run a subprocess with text=True and clear error reporting."""
    kwargs.setdefault("text", True)
    try:
        return subprocess.run(argv, check=check, **kwargs)
    except subprocess.CalledProcessError as e:
        stderr, stdout = (e.stderr or "").strip(), (e.stdout or "").strip()
        details = f": {stderr}" if stderr else (f": {stdout}" if stdout else "")
        raise RuntimeError(
            f"Command failed (exit {e.returncode}): {' '.join(argv)}{details}"
        ) from e
    except OSError as e:
        raise RuntimeError(f"Failed to execute {' '.join(argv)}: {e}") from e


TOOL_SPECS: list[tuple[str, list[str], str]] = [
    ("go", ["go", "env", "GOVERSION"], r"(?:go)?([0-9.]+)"),
    ("python", ["python3", "--version"], r"([0-9.]+)"),
    ("ruff", ["ruff", "--version"], r"(\S+)$"),
    ("ko", ["ko", "version"], r"(\S+)$"),
    ("kind", ["kind", "version"], r"(v[0-9.]+)"),
    ("kubectl", ["kubectl", "version", "--client=true"], r"(v[0-9.]+)"),
]


def probe_tool_version(name: str, cmd: list[str], pat: str) -> str:
    """Run tool command and extract version via regex."""
    try:
        proc = run(cmd, capture_output=True)
        m = re.search(pat, proc.stdout)
        return m.group(1) if m else proc.stdout.strip().split()[-1]
    except (RuntimeError, subprocess.SubprocessError, OSError):
        return "unknown"


def collect_tool_versions() -> dict[str, str]:
    """Collect current versions of standard CI tools."""
    return {name: probe_tool_version(name, cmd, pat) for name, cmd, pat in TOOL_SPECS}


@dataclasses.dataclass
class CheckResult:
    category: str
    target_file: str
    line_number: int
    item: str
    requirement: str
    nix_version: str
    status: str  # "PASS", "FAIL", "WARN", "SKIP"
    message: str
    fix_advice: str | None = None


def make_fix_advice(step2: str) -> str:
    return (
        "How to fix:\n"
        "  1. Run `make update-pins` (or `nix flake update nixpkgs`) then `make check-pins`.\n"
        f"  2. {step2}\n"
        "  3. Run `make check-pins` to verify."
    )


def add_result(
    results: list[CheckResult],
    category: str,
    target_file: str,
    line_number: int,
    item: str,
    requirement: str,
    nix_version: str,
    passed: bool,
    pass_msg: str,
    fail_msg: str,
    fix_advice: str | None = None,
) -> None:
    results.append(
        CheckResult(
            category=category,
            target_file=target_file,
            line_number=line_number,
            item=item,
            requirement=requirement,
            nix_version=nix_version,
            status="PASS" if passed else "FAIL",
            message=pass_msg if passed else fail_msg,
            fix_advice=None if passed else fix_advice,
        )
    )


def find_line_number(file_path: pathlib.Path, regex_pattern: str) -> int:
    """Return the 1-based line number of the first line matching regex_pattern."""
    try:
        compiled = re.compile(regex_pattern)
        for idx, line in enumerate(file_path.read_text(encoding="utf-8").splitlines(), start=1):
            if compiled.search(line):
                return idx
    except (OSError, re.error):
        pass
    return 1


def get_flake_nixpkgs_info(repo_root: pathlib.Path) -> tuple[str, str, str]:
    """Return (rev_short, date_str, nix_info_str) from flake.lock."""
    flake_lock_path = repo_root / "flake.lock"
    rev_short, date_str = "unknown", "unknown"
    if flake_lock_path.exists():
        try:
            with open(flake_lock_path, "r", encoding="utf-8") as f:
                locked = json.load(f).get("nodes", {}).get("nixpkgs", {}).get("locked", {})
            rev = locked.get("rev", "unknown")
            rev_short = rev[:7] if len(rev) >= 7 else rev
            last_mod = locked.get("lastModified", 0)
            if last_mod:
                date_str = datetime.datetime.fromtimestamp(last_mod, datetime.UTC).strftime("%Y-%m-%d")
        except (OSError, json.JSONDecodeError, KeyError) as e:
            rev_short = f"error({e})"
    return rev_short, date_str, f"flake.lock nixpkgs {rev_short} {date_str}"


def parse_go_version(v: str) -> tuple[int, ...]:
    clean = re.sub(r"^go", "", v).strip()
    clean = re.sub(r"[^0-9.].*$", "", clean)
    return tuple(int(x) for x in clean.split(".") if x)


def check_go(
    repo_root: pathlib.Path, nix_info: str, results: list[CheckResult]
) -> None:
    """Check Go version against go.mod directives."""
    go_mod_path = repo_root / "go.mod"
    if not go_mod_path.exists():
        return

    go_directive, toolchain_directive = None, None
    go_line, toolchain_line = 1, 1
    with open(go_mod_path, "r", encoding="utf-8") as f:
        for idx, line in enumerate(f, start=1):
            if m := re.match(r"^go\s+([0-9.]+)", line.strip()):
                go_directive, go_line = m.group(1), idx
            elif m := re.match(r"^toolchain\s+go?([0-9.]+)", line.strip()):
                toolchain_directive, toolchain_line = m.group(1), idx

    try:
        proc = run(["go", "env", "GOVERSION"], capture_output=True)
        nix_go_raw = proc.stdout.strip()
    except (RuntimeError, subprocess.SubprocessError, OSError) as e:
        results.append(
            CheckResult("Go", "go.mod", go_line, "go toolchain", str(go_directive), f"error: {e}", "FAIL", f"Failed to execute `go env GOVERSION`: {e}", "Ensure Go is installed and available in the PATH.")
        )
        return

    nix_go_clean = nix_go_raw.removeprefix("go")
    nix_ver = parse_go_version(nix_go_clean)

    if go_directive:
        req_ver = parse_go_version(go_directive)
        req_m, nix_m = req_ver[:2], nix_ver[:2]
        same_mm, ge = nix_m == req_m, nix_ver >= req_ver
        reasons = ([f"version {nix_go_clean} < {go_directive}"] if not ge else []) + (
            [f"major.minor {nix_m[0]}.{nix_m[1]} != {req_m[0]}.{req_m[1]}"] if not same_mm else []
        )
        reason_str = ", ".join(reasons)
        add_result(
            results,
            category="Go",
            target_file="go.mod",
            line_number=go_line,
            item="go",
            requirement=f">= {go_directive}, same major.minor" if (same_mm and ge) else f"go {go_directive} ({reason_str})",
            nix_version=nix_go_raw,
            passed=bool(same_mm and ge),
            pass_msg=f"Nix provides {nix_go_raw}, satisfies go.mod (go {go_directive})",
            fail_msg=f"pin mismatch: Go — go.mod requires {go_directive} ({reason_str}), Nix ({nix_info}) provides {nix_go_raw}",
            fix_advice=make_fix_advice(f"nix/ci.nix picks pkgs.go_{req_m[0]}_{req_m[1]} from go.mod; if its patch release is older than go.mod asks, run `make update-pins` or lower the patch in go.mod's go directive."),
        )

    if toolchain_directive:
        tc_req_ver = parse_go_version(toolchain_directive)
        ge = nix_ver >= tc_req_ver
        add_result(
            results,
            category="Go",
            target_file="go.mod",
            line_number=toolchain_line,
            item="toolchain",
            requirement=f">= {toolchain_directive}" if ge else f"toolchain {toolchain_directive}",
            nix_version=nix_go_raw,
            passed=ge,
            pass_msg=f"Nix provides {nix_go_raw}, satisfies go.mod (toolchain {toolchain_directive})",
            fail_msg=f"pin mismatch: Go toolchain — go.mod requires {toolchain_directive}, Nix ({nix_info}) provides {nix_go_raw}",
            fix_advice=make_fix_advice(f"If nixpkgs Go is below toolchain {toolchain_directive}, update nix/ci.nix or relax toolchain in go.mod."),
        )


PYPROJECTS = (
    "clients/python/pyproject.toml",
    "integrations/nemo-gym/pyproject.toml",
)


def check_python(repo_root: pathlib.Path, nix_info: str, results: list[CheckResult]) -> None:
    """Check each pyproject.toml against the Nix Python environment running this script.

    Every requirement line is checked on its own against what is installed, so
    duplicates across base/extras are all enforced, and a dependency nix/python.nix
    left out (absent, removed or broken in nixpkgs) shows up as not installed.
    """
    import platform
    import tomllib
    from importlib import metadata

    from packaging.requirements import Requirement
    from packaging.specifiers import SpecifierSet
    from packaging.utils import canonicalize_name

    # nix/python.nix decides which dependencies don't come from nixpkgs (our own
    # projects, and extras it leaves out) and exports them with a reason.
    raw_skip = os.environ.get("SANDBOXENV_NIX_PYTHON_SKIPPED")
    if raw_skip is None:
        raise SystemExit("check-pins: SANDBOXENV_NIX_PYTHON_SKIPPED is unset; run inside the devShell "
                         "(make check-pins, or nix develop -c python3 hack/ci/check-pins.py)")
    skip = {canonicalize_name(k): v for k, v in json.loads(raw_skip).items()}

    py = platform.python_version()
    for rel in PYPROJECTS:
        path = repo_root / rel
        data = tomllib.loads(path.read_text(encoding="utf-8"))
        project = data.get("project", {})

        if rp := project.get("requires-python"):
            add_result(
                results,
                category="Python Interpreter",
                target_file=rel,
                line_number=find_line_number(path, r"requires-python\s*="),
                item=f"{rel}: requires-python",
                requirement=rp,
                nix_version=py,
                passed=SpecifierSet(rp).contains(py, prereleases=True),
                pass_msg=f"Nix provides {py}, satisfies {rel} ({rp})",
                fail_msg=f"pin mismatch: Python interpreter — {rel} requires {rp}, Nix ({nix_info}) provides {py}",
                fix_advice=make_fix_advice(f"Update python in nix/python.nix or adjust requires-python in {rel}."),
            )

        lines = list(project.get("dependencies", []))
        for extra in project.get("optional-dependencies", {}).values():
            lines.extend(extra)
        for line in lines:
            req = Requirement(line)
            if req.marker is not None and not req.marker.evaluate():
                continue
            name, spec = canonicalize_name(req.name), str(req.specifier)
            # The quoted requirement as written: exact even for duplicates and
            # for names that prefix others (grpcio vs grpcio-tools).
            line_no = find_line_number(path, rf"[\"']{re.escape(line)}[\"']")
            if name in skip:
                results.append(CheckResult("Python Package", rel, line_no, name, "not from Nix", f"skipped: {skip[name]}",
                                           "SKIP", f"Python package {name} skipped: {skip[name]} ({rel})", None))
                continue
            try:
                version = metadata.version(name)
            except metadata.PackageNotFoundError:
                version = None
            add_result(
                results, "Python Package", rel, line_no, name, spec or "any", version or "not in Nix pythonEnv",
                passed=version is not None and req.specifier.contains(version, prereleases=True),
                pass_msg=f"Nix provides {name} {version}, satisfies {rel} ({spec or 'any'})",
                fail_msg=f"pin mismatch: {name} — {rel} requires {spec}, Nix ({nix_info}) provides {version}" if version else
                f"{name} is not in the Nix Python env: nixpkgs lacks it, removed it, or marks it broken",
                fix_advice=make_fix_advice(f"Or relax the pin in {rel}." if version else
                                           f"Package/override {name} in nix/python.nix; if it's only in an optional extra the "
                                           f"tests don't need, add that extra to skippedExtras there."),
            )


def check_dockerfile_base_image(
    repo_root: pathlib.Path, results: list[CheckResult]
) -> None:
    """Verify Dockerfile.ci base image is pinned with @sha256: digest."""
    dockerfile_path = repo_root / "Dockerfile.ci"
    if not dockerfile_path.exists():
        return

    from_line_no = 1
    base_image = None
    for idx, line in enumerate(dockerfile_path.read_text(encoding="utf-8").splitlines(), start=1):
        if line.strip().startswith("FROM ") and not line.strip().startswith("FROM --"):
            from_line_no = idx
            parts = line.strip().split()
            base_image = parts[1] if len(parts) >= 2 else None
            break

    if not base_image:
        results.append(
            CheckResult("Dockerfile.ci", "Dockerfile.ci", 1, "FROM base image", "@sha256:<digest>", "not found", "FAIL", "No FROM instruction found in Dockerfile.ci")
        )
        return

    has_digest = bool(re.search(r"@sha256:[a-f0-9]{64}", base_image))
    add_result(
        results,
        category="Dockerfile.ci",
        target_file="Dockerfile.ci",
        line_number=from_line_no,
        item="base image digest",
        requirement="@sha256:<digest>",
        nix_version=base_image,
        passed=has_digest,
        pass_msg=f"Dockerfile.ci base image is pinned: {base_image}",
        fail_msg=f"pin mismatch: Dockerfile.ci — base image '{base_image}' is not pinned by @sha256: digest",
        fix_advice=make_fix_advice(
            f"Or resolve manually: DIGEST=$(nix run nixpkgs#crane -- digest {base_image.split('@', 1)[0]}) and update Dockerfile.ci."
        ),
    )



def emit_github_actions_annotations(results: list[CheckResult]) -> None:
    """Emit GitHub Actions workflow annotations for FAIL and WARN."""
    if os.environ.get("GITHUB_ACTIONS") != "true":
        return
    for res in results:
        if res.status in ("FAIL", "WARN"):
            level = "error" if res.status == "FAIL" else "warning"
            title = "pin mismatch" if res.status == "FAIL" else "pin warning"
            print(f"::{level} file={res.target_file},line={res.line_number},title={title}::{res.message}")


def write_github_step_summary(results: list[CheckResult], nix_info: str) -> None:
    """Write markdown summary table to $GITHUB_STEP_SUMMARY if available."""
    summary_path = os.environ.get("GITHUB_STEP_SUMMARY")
    if not summary_path:
        return

    icons = {"PASS": "✅ PASS", "FAIL": "❌ FAIL", "WARN": "⚠️ WARN", "SKIP": "ℹ️ SKIP"}
    lines = [
        "## Dependency & Nix Toolchain Pin Verification",
        "",
        f"**Nixpkgs Reference:** `{nix_info}`",
        "",
        "| Category | Target | Item | Requirement | Nix / Env Version | Result |",
        "|---|---|---|---|---|---|",
    ]
    for r in results:
        lines.append(
            f"| {r.category} | `{r.target_file}:{r.line_number}` | `{r.item}` | "
            f"`{r.requirement.replace('|', '\\|')}` | `{r.nix_version.replace('|', '\\|')}` | {icons.get(r.status, r.status)} |"
        )

    failures = [r for r in results if r.status == "FAIL"]
    if failures:
        lines.extend(["", "### Failure Details & Fix Instructions"])
        for f in failures:
            lines.extend([f"#### ❌ {f.item} ({f.target_file}:{f.line_number})", f"> **Message:** {f.message}"])
            if f.fix_advice:
                lines.extend(["", "```text", f.fix_advice, "```"])

    try:
        with open(summary_path, "a", encoding="utf-8") as f:
            f.write("\n".join(lines) + "\n")
    except OSError as e:
        print(f"Warning: failed to write to GITHUB_STEP_SUMMARY: {e}", file=sys.stderr)


def run_check(repo_root: pathlib.Path) -> int:
    _, _, nix_info = get_flake_nixpkgs_info(repo_root)

    print("=================================================================")
    print("Checking Toolchain & Dependency Pins against Nix Flake")
    print(f"Nix Environment: {nix_info}")
    print("=================================================================")

    results: list[CheckResult] = []
    check_go(repo_root, nix_info, results)
    check_python(repo_root, nix_info, results)
    check_dockerfile_base_image(repo_root, results)

    print("\nResults:")
    for res in results:
        sym = {"PASS": "✓", "WARN": "!", "SKIP": "- [SKIP]", "FAIL": "✗"}.get(res.status, res.status)
        tag = f"[{res.category}] " if res.status != "FAIL" else ""
        print(f"  {sym} {tag}{res.message}")

    failures = [r for r in results if r.status == "FAIL"]
    if failures:
        print("\n" + "=" * 65)
        print("ACTION REQUIRED: Fix Pin Mismatches")
        print("=" * 65)
        for f in failures:
            print(f"\n[FAIL] {f.target_file}:{f.line_number} — {f.item}")
            print(f"       Requirement: {f.requirement}")
            print(f"       Nix version: {f.nix_version}")
            if f.fix_advice:
                print("\n" + f.fix_advice)
        print("=" * 65)

    emit_github_actions_annotations(results)
    write_github_step_summary(results, nix_info)

    print()
    if failures:
        print(f"FAILED: {len(failures)} pin check(s) failed.")
        return 1

    print("SUCCESS: All required pin checks passed.")
    return 0


def resolve_crane_digest(target: str) -> str:
    """Resolve digest with crane from PATH, else nix run nixpkgs#crane --."""
    crane_bin = shutil.which("crane")
    cmd = [crane_bin, "digest", target] if crane_bin else ["nix", "run", "nixpkgs#crane", "--", "digest", target]
    return run(cmd, capture_output=True).stdout.strip()


def resolve_base_image_digest(image_tagged: str) -> str:
    """Resolve base image digest via crane."""
    try:
        return resolve_crane_digest(image_tagged)
    except (RuntimeError, subprocess.SubprocessError, OSError):
        return ""


def update_dockerfile_base_image(repo_root: pathlib.Path) -> None:
    """Re-pin Dockerfile.ci base image FROM line to current resolved digest."""
    dockerfile_path = repo_root / "Dockerfile.ci"
    if not dockerfile_path.exists():
        return

    content = dockerfile_path.read_text(encoding="utf-8")
    # Only the image reference changes; anything after it on the line (e.g.
    # Dockerfile.ci's `AS base`, which later stages build on) is kept.
    if not (m := re.search(r"(?m)^FROM\s+(\S+)(.*)$", content)):
        return

    img, rest = m.group(1).split("@", 1)[0], m.group(2)
    image_tagged = img if ":" in img else f"{img}:latest"
    print(f"==> Resolving current digest for Dockerfile.ci base image ({image_tagged})...")
    digest = resolve_base_image_digest(image_tagged)

    if re.match(r"^sha256:[0-9a-f]{64}$", digest):
        new_from = f"FROM {image_tagged}@{digest}{rest}"
        print(f"==> Pinning {dockerfile_path.name} to: {new_from}")
        dockerfile_path.write_text(content[: m.start()] + new_from + content[m.end() :], encoding="utf-8")
    else:
        print(f"WARNING: Could not resolve digest for {image_tagged}", file=sys.stderr)


def record_tool_versions_via_nix(repo_root: pathlib.Path) -> dict[str, str]:
    """Record toolchain versions inside Nix develop devShell via versions --json."""
    proc = run(
        ["nix", "develop", "-c", "python3", "hack/ci/check-pins.py", "versions", "--json"],
        capture_output=True,
        cwd=repo_root,
    )
    for line in reversed(proc.stdout.strip().splitlines()):
        if line.strip().startswith("{") and line.strip().endswith("}"):
            return json.loads(line.strip())
    return json.loads(proc.stdout.strip())


def cmd_versions(as_json: bool) -> int:
    """Display toolchain versions directly in the current environment."""
    versions = collect_tool_versions()
    if as_json:
        print(json.dumps(versions))
    else:
        for name, _, _ in TOOL_SPECS:
            print(f"{name:<12} {versions.get(name, 'unknown')}")
    return 0


def cmd_update(repo_root: pathlib.Path, flake_inputs: list[str]) -> int:
    """Update Nix flake inputs, display before/after versions, re-pin Dockerfile.ci, and check."""
    if not shutil.which("nix"):
        print("ERROR: Nix is required to update pins ('nix' not found on PATH).", file=sys.stderr)
        return 1

    print("==> Recording toolchain versions before update...")
    try:
        before_versions = record_tool_versions_via_nix(repo_root)
    except RuntimeError:
        print("NOTE: 'Before' toolchain versions are unknown because the old lock failed to evaluate.")
        before_versions = {}

    cmd = ["nix", "flake", "update"]
    if flake_inputs:
        print(f"==> Updating Nix flake inputs: {' '.join(flake_inputs)}...")
        cmd.extend(flake_inputs)
    else:
        print("==> Updating all Nix flake inputs...")

    try:
        run(cmd, cwd=repo_root)
    except (RuntimeError, subprocess.SubprocessError, OSError) as e:
        print(f"ERROR: Nix flake update failed: {e}", file=sys.stderr)
        return 1

    print("==> Recording toolchain versions after update...")
    try:
        after_versions = record_tool_versions_via_nix(repo_root)
    except RuntimeError as e:
        # The updated lock still can't evaluate the devShell, e.g. go.mod asks
        # for a Go minor the new nixpkgs doesn't have either. Say so plainly
        # instead of ending on the raw Nix error; flake.lock keeps the update.
        print(f"ERROR: the devShell still doesn't evaluate after the update:\n{e}", file=sys.stderr)
        print("Lower the `go` directive in go.mod (or whatever the error names), then run `make check-pins`.", file=sys.stderr)
        return 1

    print(
        "\n========================================================================\n"
        "Toolchain & Dependency Version Changes\n"
        "========================================================================\n"
        f"{'Tool':<12} {'Before':<20} {'After':<20} {'Status':<12}\n"
        "------------------------------------------------------------------------"
    )
    for name, _, _ in TOOL_SPECS:
        b_val, a_val = before_versions.get(name, "unknown"), after_versions.get(name, "unknown")
        status = "(unchanged)" if b_val == a_val else "UPDATED"
        print(f"{name:<12} {b_val:<20} {a_val:<20} {status:<12}")
    print("========================================================================\n")

    update_dockerfile_base_image(repo_root)

    print("\n==> Running check-pins to verify consistency...")
    return run(["nix", "develop", "-c", "python3", "hack/ci/check-pins.py", "check"], check=False, cwd=repo_root).returncode


def main() -> int:
    repo_root = pathlib.Path(__file__).resolve().parents[2]
    parser = argparse.ArgumentParser(
        description="Verify and update Nix toolchain and repository dependency pins."
    )
    sub = parser.add_subparsers(dest="command")
    sub.add_parser("check", help="Verify toolchain and dependency pins (default)")
    up = sub.add_parser("update", help="Update Nix flake inputs and re-pin dependencies")
    up.add_argument("inputs", nargs="*", metavar="INPUT", help="Specific Nix flake inputs to update (default: all)")
    v = sub.add_parser("versions", help="Display toolchain versions")
    v.add_argument("--json", action="store_true", help="Output versions as JSON")

    args = parser.parse_args()
    dispatch = {
        "check": lambda: run_check(repo_root),
        "update": lambda: cmd_update(repo_root, args.inputs),
        "versions": lambda: cmd_versions(args.json),
    }
    fn = dispatch.get(args.command or "check")
    if fn:
        return fn()
    parser.print_help()
    return 1


if __name__ == "__main__":
    sys.exit(main())
