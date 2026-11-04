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

"""Verify the Dafny models and compare the results with what their README lists.

The models in docs/plans/telemetry/verify/ fail on purpose: each "before" lemma
models code with a known bug, and each open gap is a lemma that does not hold
yet. So "dafny verify exits 0" is the wrong check. This script runs
`dafny verify` on every .dfy file there and fails unless, for each file:

  1. the README's Run block lists it, as `dafny verify <file>  # N verified, M errors`;
  2. Dafny reports exactly N verified and M errors;
  3. the lemmas the errors fall in are exactly the README's "Lemmas that fail
     by design" rows for that file.

A fix that makes a "fails by design" lemma verify, or a code-model change that
breaks a verified one, fails the check until the README says so too.

Usage: hack/verify/dafny.py [--dafny PATH] [--dir DIR]
"""

from __future__ import annotations

import argparse
import re
import subprocess
import sys
from pathlib import Path

REPO_ROOT = Path(__file__).resolve().parents[2]
DEFAULT_DIR = REPO_ROOT / "docs" / "plans" / "telemetry" / "verify"

RUN_LINE = re.compile(r"^dafny verify (\S+\.dfy)\s+# (\d+) verified, (\d+) errors?\b")
FAIL_ROW = re.compile(r"^\|\s*`([^`]+\.dfy)`\s*\|\s*`(\w+)`\s*\|")
SUMMARY = re.compile(r"finished with (\d+) verified, (\d+) errors?")
ERROR = re.compile(r"^(\S+\.dfy)\((\d+),\d+\): Error")
DECL = re.compile(r"^\s*(?:ghost\s+)?(?:lemma|function|predicate|method)\s+(\w+)")


def read_expectations(
    readme: Path,
) -> tuple[dict[str, tuple[int, int]], dict[str, set[str]]]:
    """Counts from the Run block and failing lemmas from the table, per file."""
    counts: dict[str, tuple[int, int]] = {}
    failing: dict[str, set[str]] = {}
    for line in readme.read_text().splitlines():
        if m := RUN_LINE.match(line):
            counts[m.group(1)] = (int(m.group(2)), int(m.group(3)))
        elif m := FAIL_ROW.match(line):
            failing.setdefault(m.group(1), set()).add(m.group(2))
    return counts, failing


def enclosing_decl(source: list[str], line_no: int) -> str:
    """Name of the lemma (or function) that contains 1-based line `line_no`."""
    for line in reversed(source[:line_no]):
        if m := DECL.match(line):
            return m.group(1)
    return "<top level>"


def verify(dafny: str, path: Path) -> tuple[int, int, set[str], str]:
    proc = subprocess.run(
        [dafny, "verify", path.name],
        cwd=path.parent,
        capture_output=True,
        text=True,
        check=False,
    )
    out = proc.stdout + proc.stderr
    m = SUMMARY.search(out)
    if m is None:
        raise RuntimeError(f"{path.name}: no verifier summary in dafny output:\n{out}")
    source = path.read_text().splitlines()
    lemmas = {
        enclosing_decl(source, int(e.group(2)))
        for e in (ERROR.match(line) for line in out.splitlines())
        if e is not None and e.group(1) == path.name
    }
    return int(m.group(1)), int(m.group(2)), lemmas, out


def _shown(path: Path) -> str:
    try:
        return str(path.resolve().relative_to(REPO_ROOT))
    except ValueError:
        return str(path)


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__.splitlines()[0])
    parser.add_argument(
        "--dafny", default="dafny", help="dafny binary (default: dafny on PATH)"
    )
    parser.add_argument(
        "--dir", type=Path, default=DEFAULT_DIR, help="directory of .dfy models"
    )
    args = parser.parse_args()

    readme = args.dir / "README.md"
    counts, failing = read_expectations(readme)
    models = sorted(args.dir.glob("*.dfy"))
    problems: list[str] = []

    for name in sorted(set(counts) | set(failing)):
        if not (args.dir / name).exists():
            problems.append(f"{readme.name} lists {name}, which does not exist")

    for path in models:
        name = path.name
        if name not in counts:
            problems.append(f"{name}: not in the Run block of {readme.name}")
            continue
        verified, errors, lemmas, out = verify(args.dafny, path)
        want_verified, want_errors = counts[name]
        want_lemmas = failing.get(name, set())
        print(
            f"{name}: {verified} verified, {errors} errors; failing: {sorted(lemmas) or '-'}"
        )
        if (verified, errors) != (want_verified, want_errors):
            problems.append(
                f"{name}: {verified} verified, {errors} errors; "
                f"{readme.name} says {want_verified} verified, {want_errors} errors"
            )
        if lemmas != want_lemmas:
            if extra := sorted(lemmas - want_lemmas):
                problems.append(
                    f"{name}: fails in {extra}, not listed as failing by design"
                )
            if fixed := sorted(want_lemmas - lemmas):
                problems.append(
                    f"{name}: {fixed} listed as failing by design but verify"
                )
        if problems and problems[-1].startswith(name):
            print(out, file=sys.stderr)

    if problems:
        print(f"\nDafny models do not match {_shown(readme)}:", file=sys.stderr)
        for p in problems:
            print(f"  - {p}", file=sys.stderr)
        return 1
    print(f"All {len(models)} Dafny models match {_shown(readme)}.")
    return 0


if __name__ == "__main__":
    sys.exit(main())
