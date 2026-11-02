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

"""Parse and format Go and Python test coverage reports and diff coverage.

What it does:
  1. Merges Go covdata profiles (unit tests + guest daemon integration) if present.
  2. Parses Go coverprofile lines to compute per-top-level-package and total statement coverage,
     excluding generated proto stubs.
  3. Formats Python coverage metrics by package and total from coverage data.
  4. Generates Mermaid horizontal bar charts (`xychart-beta horizontal`) visualizing per-package coverage.
  5. Runs diff-cover against the PR base branch (origin/main or GITHUB_BASE_REF) over Cobertura XMLs,
     emitting GitHub Actions workflow annotations (`::notice`) for uncovered changed lines.
  6. Emits a clean Markdown summary report with Mermaid charts to stdout, saves to <covdir>/summary.md, and
     appends to $GITHUB_STEP_SUMMARY when running under GitHub Actions.

When it runs:
  - CI: invoked after Go and Python test steps in .github/workflows/ci.yaml.
  - Local CI: invoked by `ci-go`, `ci-python`, and `make coverage`.

How to run:
  - Direct: `./hack/ci/coverage-summary.py [--covdir <dir>] [--go] [--python] [--merge-covdata] [--diff-cover] [--print-paths]`
  Examples:
    ./hack/ci/coverage-summary.py --covdir=coverage --go --diff-cover
    ./hack/ci/coverage-summary.py --covdir=coverage --python --diff-cover
    ./hack/ci/coverage-summary.py --covdir=coverage --merge-covdata --go --python --diff-cover --print-paths

Prerequisites:
  - Python 3.10+
  - go tool cover, gocover-cobertura, and diff-cover on PATH (provided by Nix devShell)

Environment variables:
  GITHUB_ACTIONS       (unset)  Set to "true" when running inside GitHub Actions.
  GITHUB_STEP_SUMMARY  (unset)  Path to GitHub Actions step summary file.
  GITHUB_BASE_REF      (unset)  Target PR base branch in GitHub Actions.

Outputs / exit codes:
  - Writes markdown report to <covdir>/summary.md (or summary-go.md / summary-python.md for single-language runs) and appends to $GITHUB_STEP_SUMMARY if set.
  - Exits with 0 on successful reporting. Never fails the build on coverage thresholds.

See also:
  docs/chores/coverage.md, Makefile, nix/ci.nix, .coveragerc.
"""

from __future__ import annotations

import argparse
import collections
import json
import os
import pathlib
import shutil
import subprocess
import sys


def run_cmd(args: list[str], check: bool = True) -> subprocess.CompletedProcess[str]:
    """Helper to run a subprocess and capture output."""
    try:
        return subprocess.run(
            args,
            check=check,
            capture_output=True,
            text=True,
        )
    except subprocess.CalledProcessError as e:
        if check:
            print(f"Command {' '.join(args)} failed: {e.stderr}", file=sys.stderr)
            raise
        return subprocess.CompletedProcess(args, e.returncode, e.stdout, e.stderr)


def merge_go_covdata(covdir: pathlib.Path) -> None:
    """Merge and format unit and guest-daemon covdata directories."""
    unit_dir = covdir / "go-unit"
    daemon_dir = covdir / "go-daemon"
    merged_dir = covdir / "go-merged"

    has_unit = unit_dir.is_dir() and any(unit_dir.glob("covcounters.*"))
    has_daemon = daemon_dir.is_dir() and any(daemon_dir.glob("covcounters.*"))

    if has_unit and has_daemon:
        if merged_dir.exists():
            shutil.rmtree(merged_dir, ignore_errors=True)
        merged_dir.mkdir(parents=True, exist_ok=True)
        in_flag = f"-i={unit_dir},{daemon_dir}"
        out_flag = f"-o={merged_dir}"
        run_cmd(["go", "tool", "covdata", "merge", in_flag, out_flag])
        run_cmd(["go", "tool", "covdata", "textfmt", f"-i={merged_dir}", f"-o={covdir / 'go.out'}"])
    elif has_unit:
        run_cmd(["go", "tool", "covdata", "textfmt", f"-i={unit_dir}", f"-o={covdir / 'go.out'}"])
    elif has_daemon:
        run_cmd(["go", "tool", "covdata", "textfmt", f"-i={daemon_dir}", f"-o={covdir / 'go.out'}"])
    else:
        return

    # Update filtered output, html, func, and xml
    filter_go_profile(covdir)


def filter_go_profile(covdir: pathlib.Path) -> None:
    """Filter generated proto files from go.out to produce go.filtered.out and reports."""
    raw_profile = covdir / "go.out"
    if not raw_profile.is_file():
        return

    filtered_profile = covdir / "go.filtered.out"
    with open(raw_profile, "r", encoding="utf-8") as f_in, open(filtered_profile, "w", encoding="utf-8") as f_out:
        mode_written = False
        for line in f_in:
            if line.startswith("mode:"):
                if not mode_written:
                    f_out.write(line)
                    mode_written = True
                continue
            # Drop generated protobuf stubs
            if ".pb.go:" in line or "/proto/ateenv/" in line or "/proto/" in line:
                continue
            f_out.write(line)

    # Re-generate go-func.txt, go.html, and go.xml
    func_txt = covdir / "go-func.txt"
    res = run_cmd(["go", "tool", "cover", f"-func={filtered_profile}"], check=False)
    if res.returncode == 0:
        func_txt.write_text(res.stdout, encoding="utf-8")

    run_cmd(["go", "tool", "cover", f"-html={filtered_profile}", f"-o={covdir / 'go.html'}"], check=False)

    # Convert to Cobertura XML via gocover-cobertura
    with open(filtered_profile, "rb") as f_in, open(covdir / "go.xml", "wb") as f_out:
        subprocess.run(["gocover-cobertura"], stdin=f_in, stdout=f_out, check=False)


def parse_go_coverage(covdir: pathlib.Path) -> tuple[float, int, int, list[tuple[str, int, int, float]]]:
    """Parse go.filtered.out and compute package and total statement counts."""
    filtered_profile = covdir / "go.filtered.out"
    if not filtered_profile.is_file():
        return 0.0, 0, 0, []

    pkg_stmts: dict[str, int] = collections.defaultdict(int)
    pkg_covered: dict[str, int] = collections.defaultdict(int)

    with open(filtered_profile, "r", encoding="utf-8") as f:
        for line in f:
            line = line.strip()
            if not line or line.startswith("mode:"):
                continue
            parts = line.split()
            if len(parts) != 3:
                continue
            target, stmts_str, count_str = parts[0], parts[1], parts[2]
            try:
                stmts = int(stmts_str)
                count = int(count_str)
            except ValueError:
                continue

            # Strip module prefix: github.com/agent-substrate/env/
            path = target.split("github.com/agent-substrate/env/")[-1].split(":")[0]
            pkg = "/".join(path.split("/")[:-1])
            if not pkg:
                pkg = "."
            pkg_stmts[pkg] += stmts
            if count > 0:
                pkg_covered[pkg] += stmts

    total_stmts = sum(pkg_stmts.values())
    total_covered = sum(pkg_covered.values())
    total_pct = (total_covered / total_stmts * 100.0) if total_stmts > 0 else 0.0

    packages = []
    for pkg in sorted(pkg_stmts.keys()):
        s = pkg_stmts[pkg]
        c = pkg_covered[pkg]
        pct = (c / s * 100.0) if s > 0 else 0.0
        packages.append((pkg, s, c, pct))

    return total_pct, total_stmts, total_covered, packages


def parse_python_coverage(
    covdir: pathlib.Path,
) -> tuple[float, int, int, int, int, int, list[tuple[str, int, int, int, int, float]]]:
    """Parse python coverage via `coverage json` and compute per-package metrics."""
    py_cov_file = covdir / "py" / ".coverage"
    if not py_cov_file.is_file():
        # Fallback to .coverage at root
        py_cov_file = pathlib.Path(".coverage")
        if not py_cov_file.is_file():
            return 0.0, 0, 0, 0, 0, 0, []

    py_json_path = covdir / "py.json"
    env = os.environ.copy()
    env["COVERAGE_FILE"] = str(py_cov_file)
    subprocess.run(
        ["coverage", "json", "-o", str(py_json_path), "--rcfile=.coveragerc"],
        env=env,
        check=False,
        capture_output=True,
    )

    if not py_json_path.is_file():
        return 0.0, 0, 0, 0, 0, []

    try:
        data = json.loads(py_json_path.read_text(encoding="utf-8"))
    except (json.JSONDecodeError, OSError):
        return 0.0, 0, 0, 0, 0, []

    totals = data.get("totals", {})
    total_stmts = totals.get("num_statements", 0)
    total_covered = totals.get("covered_lines", 0)
    total_branches = totals.get("num_branches", 0)
    total_covered_branches = totals.get("covered_branches", 0)
    total_brpart = totals.get("num_partial_branches", 0)
    total_pct = totals.get("percent_covered", 0.0)

    # Group files into known repository packages
    def categorize(filepath: str) -> str:
        norm = filepath.replace("\\", "/")
        if "clients/python" in norm:
            return "clients/python (ate_env)"
        if "integrations/nemo-gym" in norm:
            return "integrations/nemo-gym"
        if "integrations/uni-agent" in norm:
            return "integrations/uni-agent"
        if "internal/benchmarking/swebench" in norm:
            return "internal/benchmarking/swebench"
        if "benchmarking/swebench" in norm:
            return "benchmarking/swebench"
        if "benchmarking/telemetry_report" in norm:
            return "benchmarking/telemetry_report"
        if "examples/Qwen3-1.7B-verl" in norm:
            return "examples/Qwen3-1.7B-verl"
        return norm.split("/")[0]

    groups: dict[str, dict[str, int]] = collections.defaultdict(
        lambda: {"stmts": 0, "covered": 0, "branches": 0, "cov_branches": 0, "brpart": 0}
    )

    for filepath, fileinfo in data.get("files", {}).items():
        cat = categorize(filepath)
        summary = fileinfo.get("summary", {})
        s = summary.get("num_statements", 0)
        c = summary.get("covered_lines", 0)
        b = summary.get("num_branches", 0)
        cb = summary.get("covered_branches", 0)
        bp = summary.get("num_partial_branches", 0)
        groups[cat]["stmts"] += s
        groups[cat]["covered"] += c
        groups[cat]["branches"] += b
        groups[cat]["cov_branches"] += cb
        groups[cat]["brpart"] += bp

    package_list = []
    for pkg in sorted(groups.keys()):
        g = groups[pkg]
        s = g["stmts"]
        c = g["covered"]
        b = g["branches"]
        cb = g["cov_branches"]
        bp = g["brpart"]
        total_items = s + b
        covered_items = c + cb
        pct = (covered_items / total_items * 100.0) if total_items > 0 else 0.0
        package_list.append((pkg, s, c, b, bp, pct))

    return total_pct, total_stmts, total_covered, total_branches, total_covered_branches, total_brpart, package_list


def resolve_base_ref() -> str | None:
    """Find the best git commit or branch to compare against for diff-cover."""
    candidates = []
    base_env = os.environ.get("GITHUB_BASE_REF")
    if base_env:
        candidates.append(f"origin/{base_env}")
        candidates.append(base_env)
    candidates.extend(["origin/main", "main", "HEAD~1"])

    head_res = subprocess.run(["git", "rev-parse", "HEAD"], capture_output=True, text=True, check=False)
    head_sha = head_res.stdout.strip() if head_res.returncode == 0 else None

    for ref in candidates:
        res = subprocess.run(["git", "rev-parse", "--verify", ref], capture_output=True, text=True, check=False)
        if res.returncode == 0:
            ref_sha = res.stdout.strip()
            # If candidate resolves to HEAD (e.g. push to main where origin/main == HEAD),
            # diff-cover would compare HEAD..HEAD (0 lines). Skip candidates that resolve to HEAD,
            # unless it's HEAD~1.
            if head_sha and ref_sha == head_sha and ref != "HEAD~1":
                continue
            return ref
    return None


def generate_mermaid_chart(items: list[tuple[str, int, int, float]]) -> str:
    """Generate a Mermaid horizontal bar chart for package coverage.

    items is a list of (package_name, covered_statements, total_statements, coverage_pct).
    Packages are sorted by coverage ascending.
    """
    if not items:
        return ""
    sorted_items = sorted(items, key=lambda x: (x[3], x[0]))
    labels = []
    bars = []
    for name, _covered, _total, pct in sorted_items:
        clean_name = name.replace('"', "'")
        value = f"{round(pct, 1):g}"
        # The value goes in the label: mermaid sizes bar data labels to fit
        # inside the bar, which makes them unreadable with many packages.
        labels.append(f'"{clean_name} ({value}%)"')
        bars.append(value)

    # Height per bar, so long package lists don't get squeezed.
    height = max(300, 24 * len(sorted_items))
    lines = [
        "```mermaid",
        f'%%{{init: {{"xyChart": {{"width": 900, "height": {height}}}}}}}%%',
        "xychart-beta horizontal",
        f"    x-axis [{', '.join(labels)}]",
        '    y-axis "Coverage (%)" 0 --> 100',
        f"    bar [{', '.join(bars)}]",
        "```",
    ]
    return "\n".join(lines)


def resolve_repo_relative_path(path_str: str) -> str:
    """Normalize file path from coverage tools to be relative to the repository root."""
    repo_root = pathlib.Path.cwd()
    p = pathlib.Path(path_str)
    if (repo_root / p).is_file():
        return p.as_posix()

    # Strip module prefix if present (e.g. github.com/agent-substrate/env/...)
    if "github.com/agent-substrate/env/" in path_str:
        sub = path_str.split("github.com/agent-substrate/env/")[-1]
        if (repo_root / sub).is_file():
            return pathlib.Path(sub).as_posix()

    # Search tracked git files for matching basename or suffix
    name = p.name
    matches = list(repo_root.glob(f"**/{name}"))
    if len(matches) == 1:
        return matches[0].relative_to(repo_root).as_posix()
    for m in matches:
        if m.as_posix().endswith(p.as_posix()):
            return m.relative_to(repo_root).as_posix()
    if matches:
        return matches[0].relative_to(repo_root).as_posix()
    return p.as_posix()


def merge_line_ranges(lines: list[int]) -> list[tuple[int, int]]:
    """Merge sorted/unsorted line numbers into consecutive (start, end) ranges."""
    if not lines:
        return []
    sorted_lines = sorted(set(lines))
    ranges = []
    start = sorted_lines[0]
    end = start
    for line in sorted_lines[1:]:
        if line == end + 1:
            end = line
        else:
            ranges.append((start, end))
            start = line
            end = line
    ranges.append((start, end))
    return ranges


def run_diff_cover(
    covdir: pathlib.Path, go: bool, python: bool, diff_report_path: pathlib.Path | None = None
) -> str | None:
    """Run diff-cover across available XML reports against the PR base branch."""
    base_ref = resolve_base_ref()
    if not base_ref:
        return None

    xml_files = []
    go_xml = covdir / "go.xml"
    py_xml = covdir / "py.xml"

    if go and go_xml.is_file():
        xml_files.append(str(go_xml))
    if python and py_xml.is_file():
        xml_files.append(str(py_xml))

    if not xml_files:
        return None

    if diff_report_path is None:
        diff_report_path = covdir / "diff-cover.md"
    diff_report_path.parent.mkdir(parents=True, exist_ok=True)
    diff_json_path = diff_report_path.with_suffix(".json")

    cmd = [
        "diff-cover",
        *xml_files,
        f"--compare-branch={base_ref}",
        f"--format=markdown:{diff_report_path},json:{diff_json_path}",
    ]
    proc = subprocess.run(cmd, capture_output=True, text=True, check=False)
    if proc.returncode != 0:
        print(f"Warning: diff-cover exited {proc.returncode}; no diff coverage section:", file=sys.stderr)
        print((proc.stderr or proc.stdout).strip(), file=sys.stderr)

    uncovered_ranges: list[tuple[int, str, int, int]] = []
    if diff_json_path.is_file():
        try:
            diff_data = json.loads(diff_json_path.read_text(encoding="utf-8"))
            for src_path, stats in diff_data.get("src_stats", {}).items():
                violation_lines = stats.get("violation_lines", [])
                if not violation_lines:
                    continue
                rel_path = resolve_repo_relative_path(src_path)
                for start, end in merge_line_ranges(violation_lines):
                    count = end - start + 1
                    uncovered_ranges.append((count, rel_path, start, end))
        except (json.JSONDecodeError, OSError, KeyError, TypeError) as e:
            print(f"Warning: could not parse diff-cover json report: {e}", file=sys.stderr)

    # Sort ranges by line count descending, then file path and start line
    uncovered_ranges.sort(key=lambda r: (-r[0], r[1], r[2]))

    # If running in GitHub Actions, emit workflow notice annotations for up to 10 ranges
    if os.environ.get("GITHUB_ACTIONS") == "true":
        for count, file_path, start, end in uncovered_ranges[:10]:
            print(
                f"::notice file={file_path},line={start},endLine={end},"
                f"title=Not covered by tests::{count} changed line(s) not covered"
            )

    remaining_note = ""
    if len(uncovered_ranges) > 10:
        remaining = len(uncovered_ranges) - 10
        remaining_note = f"\n\n*{remaining} more uncovered range{'s' if remaining != 1 else ''}; see the diff coverage table below.*"

    if diff_report_path.is_file():
        content = diff_report_path.read_text(encoding="utf-8").strip()
        if content:
            if remaining_note:
                lines = content.split("\n")
                inserted = False
                for idx, line in enumerate(lines):
                    if line.startswith("## Diff:"):
                        lines.insert(idx + 1, remaining_note.strip())
                        inserted = True
                        break
                if inserted:
                    content = "\n".join(lines)
                else:
                    content += remaining_note
            return content
    return None


def main() -> None:
    parser = argparse.ArgumentParser(description="Generate CI test coverage markdown summaries.")
    parser.add_argument("--covdir", default="coverage", help="Coverage output directory")
    parser.add_argument("--go", action="store_true", help="Include Go coverage metrics")
    parser.add_argument("--python", action="store_true", help="Include Python coverage metrics")
    parser.add_argument("--merge-covdata", action="store_true", help="Merge unit and guest-daemon covdata")
    parser.add_argument("--diff-cover", action="store_true", help="Run diff-cover over changed lines")
    parser.add_argument("--print-paths", action="store_true", help="Print report output paths")
    parser.add_argument("--summary-file", help="Custom summary output file path")
    parser.add_argument("--diff-file", help="Custom diff-cover markdown output file path")
    args = parser.parse_args()

    covdir = pathlib.Path(args.covdir)
    covdir.mkdir(parents=True, exist_ok=True)

    if args.merge_covdata or (args.go and (covdir / "go-unit").is_dir()):
        merge_go_covdata(covdir)

    sections = []
    sections.append("# Test Coverage Summary\n")

    if args.go:
        filter_go_profile(covdir)
        total_pct, total_stmts, total_covered, pkgs = parse_go_coverage(covdir)
        sections.append(f"### Go Coverage: **{total_pct:.1f}%** ({total_covered:,} / {total_stmts:,} statements)\n")
        if pkgs:
            chart = generate_mermaid_chart([(pkg, c, s, pct) for pkg, s, c, pct in pkgs])
            if chart:
                sections.append(chart + "\n")
        else:
            sections.append("*No Go coverage data available.*\n")

    if args.python:
        total_pct, total_stmts, total_covered, total_b, total_cb, _, py_pkgs = parse_python_coverage(covdir)
        if total_b > 0:
            sections.append(
                f"### Python Coverage: **{total_pct:.1f}%** "
                f"(lines+branches; {total_covered:,} / {total_stmts:,} statements, {total_cb:,} / {total_b:,} branches)\n"
            )
        else:
            sections.append(
                f"### Python Coverage: **{total_pct:.1f}%** ({total_covered:,} / {total_stmts:,} statements)\n"
            )
        if py_pkgs:
            chart = generate_mermaid_chart([(pkg, c, s, pct) for pkg, s, c, b, bp, pct in py_pkgs])
            if chart:
                sections.append(chart + "\n")
        else:
            sections.append("*No Python coverage data available.*\n")

    # Determine diff report target
    if args.diff_file:
        diff_report_path = pathlib.Path(args.diff_file)
    elif args.go and args.python:
        diff_report_path = covdir / "diff-cover.md"
    elif args.go:
        diff_report_path = covdir / "diff-cover-go.md"
    elif args.python:
        diff_report_path = covdir / "diff-cover-python.md"
    else:
        diff_report_path = covdir / "diff-cover.md"

    if args.diff_cover:
        diff_md = run_diff_cover(covdir, go=args.go, python=args.python, diff_report_path=diff_report_path)
        if diff_md:
            sections.append(f"\n{diff_md}\n")

    summary_text = "\n".join(sections).strip() + "\n"

    # Determine summary target path
    if args.summary_file:
        summary_file = pathlib.Path(args.summary_file)
    elif args.go and args.python:
        summary_file = covdir / "summary.md"
    elif args.go:
        summary_file = covdir / "summary-go.md"
    elif args.python:
        summary_file = covdir / "summary-python.md"
    else:
        summary_file = covdir / "summary.md"

    summary_file.parent.mkdir(parents=True, exist_ok=True)
    summary_file.write_text(summary_text, encoding="utf-8")

    # Output to stdout
    print(summary_text)

    # Append to GITHUB_STEP_SUMMARY if configured in CI
    step_summary_env = os.environ.get("GITHUB_STEP_SUMMARY")
    if step_summary_env:
        try:
            with open(step_summary_env, "a", encoding="utf-8") as f:
                f.write("\n" + summary_text + "\n")
        except OSError as e:
            print(f"Warning: could not write to GITHUB_STEP_SUMMARY: {e}", file=sys.stderr)

    if args.print_paths:
        print("\nGenerated Coverage Reports:")
        if (covdir / "go.html").is_file():
            print(f"  Go HTML:      {covdir / 'go.html'}")
        if (covdir / "py-html" / "index.html").is_file():
            print(f"  Python HTML:  {covdir / 'py-html' / 'index.html'}")
        if summary_file.is_file():
            print(f"  Summary MD:   {summary_file}")


if __name__ == "__main__":
    main()
