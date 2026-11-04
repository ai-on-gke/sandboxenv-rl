#!/usr/bin/env bash

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

# Builds sdist and wheel distributions for clients/python (ate-env-client),
# checks metadata with twine, and optionally verifies the built version matches
# a release Git tag's PEP 440 version.
#
# Usage: ./hack/release/build-dists.sh <outdir> [<expected-tag>]

set -euo pipefail

if [[ $# -lt 1 || $# -gt 2 ]]; then
  echo "usage: $0 <outdir> [<expected-tag>]" >&2
  exit 1
fi

script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
outdir="$1"
expected_tag="${2:-}"

mkdir -p "${outdir}"
echo "--> Building clients/python distribution (sdist & wheel) into ${outdir}..."
pyproject-build --no-isolation --outdir "${outdir}" clients/python

echo "--> Checking Python distribution metadata with twine..."
twine check "${outdir}"/*

if [[ -n "${expected_tag}" ]]; then
  echo "--> Verifying built version matches tag ${expected_tag}..."
  expected_version="$(python3 "${script_dir}/create-release.py" pep440 --tag "${expected_tag}")"
  python3 - "${outdir}" "${expected_tag}" "${expected_version}" <<'PY'
import email
import pathlib
import sys
import zipfile

outdir = pathlib.Path(sys.argv[1])
tag = sys.argv[2]
expected = sys.argv[3]

wheels = sorted(outdir.glob("*.whl"))
sdists = sorted(outdir.glob("*.tar.gz"))
if len(wheels) != 1 or len(sdists) != 1:
    sys.exit(f"error: expected 1 wheel and 1 sdist in {outdir}, found wheels={wheels} sdists={sdists}")

with zipfile.ZipFile(wheels[0]) as zf:
    meta_names = [n for n in zf.namelist() if n.endswith(".dist-info/METADATA")]
    if len(meta_names) != 1:
        sys.exit(f"error: expected 1 METADATA file in {wheels[0].name}, found {meta_names}")
    msg = email.message_from_bytes(zf.read(meta_names[0]))
    built_version = msg.get("Version", "")

if built_version != expected:
    sys.exit(
        f"error: wheel {wheels[0].name} has Version={built_version!r}, expected {expected!r} for tag {tag!r}"
    )

expected_sdist = f"ate_env_client-{expected}.tar.gz"
if sdists[0].name != expected_sdist:
    sys.exit(f"error: sdist name {sdists[0].name!r} does not match expected {expected_sdist!r}")

print(f"Verified ate-env-client {built_version} matches tag {tag}")
PY
fi

ls -l "${outdir}"
