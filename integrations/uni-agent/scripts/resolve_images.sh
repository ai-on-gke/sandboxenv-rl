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
#
# Resolve task-image tags to digest-pinned references the cluster can pull.
#
#   resolve_images.sh IMAGES_TXT REGISTRY_REPO [OUT_JSON] [PARALLEL]
#
# IMAGES_TXT     one Docker Hub reference per line (from `python -m uni_agent_substrate.datasets --images-out`)
# REGISTRY_REPO  either an Artifact Registry *remote* repository that proxies Docker Hub
#                (e.g. REGION-docker.pkg.dev/PROJECT/<dockerhub-remote-repo>): no copy, the digest is
#                resolved through the proxy and the image is fetched lazily on first pull;
#                or, with MODE=mirror, a standard repository to copy images into by digest
#                (same approach as benchmarking/swebench/mirror-images.sh).
# OUT_JSON       mapping {tag ref: digest ref} consumed by `datasets --image-digests` and
#                by the ate_env provider (default images.json)
#
# Needs crane (go install github.com/google/go-containerregistry/cmd/crane@latest) and
# registry credentials (gcloud auth configure-docker REGION-docker.pkg.dev).
set -euo pipefail
IMAGES="${1:?images.txt}"; REPO="${2:?registry repo}"; OUT="${3:-images.json}"; PAR="${4:-8}"
MODE="${MODE:-proxy}"
command -v crane >/dev/null || { echo "crane not found on PATH" >&2; exit 1; }
tmp=$(mktemp -d); trap 'rm -rf "$tmp"' EXIT

resolve_one() {  # prints "<tag ref> <digest ref>"
  local ref="$1" name tag src dst digest
  name="${ref%%:*}"; tag="${ref##*:}"; [ "$name" = "$ref" ] && tag=latest
  if [ "$MODE" = "mirror" ]; then
    src="$ref"; dst="$REPO/${name#docker.io/}"
    digest=$(crane digest "$src") || return 1
    crane copy "$src@$digest" "$dst:$tag" >/dev/null 2>&1 || crane copy "$src" "$dst:$tag" >/dev/null || return 1
    echo "$ref $dst@$digest"
  else
    dst="$REPO/${name#docker.io/}"
    digest=$(crane digest "$dst:$tag") || return 1   # resolving through the proxy also caches the image
    echo "$ref $dst@$digest"
  fi
}
export -f resolve_one; export REPO MODE
: > "$tmp/failed.txt"; export FAILED_LOG="$tmp/failed.txt"
grep -vE '^\s*(#|$)' "$IMAGES" | xargs -P "$PAR" -I{} bash -c 'resolve_one "$1" || { echo "FAILED $1" >&2; echo "$1" >> "$FAILED_LOG"; }' _ {} > "$tmp/map.txt"
python3 - "$tmp/map.txt" "$OUT" <<'PY'
import json, sys
pairs = dict(line.split(None, 1) for line in open(sys.argv[1]) if line.strip())
existing = {}
try:
    existing = json.load(open(sys.argv[2]))
except FileNotFoundError:
    pass
existing.update({k: v.strip() for k, v in pairs.items()})
json.dump(existing, open(sys.argv[2], "w"), indent=1, sort_keys=True)
print(f"resolved {len(pairs)} images -> {sys.argv[2]} ({len(existing)} total)")
PY
if [ -s "$tmp/failed.txt" ]; then
  echo "$(wc -l < "$tmp/failed.txt") images did not resolve (listed above); the map is partial" >&2
  exit 1
fi
