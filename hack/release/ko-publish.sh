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

# Builds and pushes a multi-arch Go image with ko, retrying transient registry
# upload resets up to 3 times. Attaches :latest only on stable tags without a
# pre-release '-' suffix.
#
# Usage: ./hack/release/ko-publish.sh <tag> <repo> <pkg>

set -euo pipefail

if [[ $# -ne 3 ]]; then
  echo "usage: $0 <tag> <repo> <pkg>" >&2
  exit 1
fi

tag="$1"
repo="$2"
pkg="$3"

ko_tags="${tag}"
if [[ "${tag}" != *-* ]]; then
  ko_tags="${tag},latest"
fi

for attempt in 1 2 3; do
  if out=$(KO_DOCKER_REPO="${repo}" ko build --bare --tags "${ko_tags}" "${pkg}"); then
    if [[ "${out}" == *$'\n'* || ! "${out}" =~ ^"${repo}"@sha256:[0-9a-f]{64}$ ]]; then
      echo "unexpected ko output for ${pkg}: ${out}" >&2
      exit 1
    fi
    printf '%s\n' "${out}"
    exit 0
  fi
  echo "ko build ${pkg} failed (attempt ${attempt}/3); retrying in 5s..." >&2
  sleep 5
done

exit 1
