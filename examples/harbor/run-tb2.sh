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

# Runs a Harbor dataset or tasks directory on ate-env: `harbor run` with the
# ate-env provider selected and its settings taken from the environment.
#
#   run-tb2.sh [harbor run flags...]
#
# Without flags: `-d terminal-bench@2.0 -a oracle -n 10`. Everything given is
# passed to `harbor run` after the provider flags, thus `-p ./tb2 -a terminus-2
# -m <model> -n 50` works as it does with any other --env.
#
# Environment:
#   SUBSTRATE_ENV_API              the port-forwarded ate-env-api (required)
#   SUBSTRATE_ENV_AUTH_TOKEN       the caller token, when the API requires one
#   SUBSTRATE_ENV_HARBOR_REGISTRY  where images built from a Dockerfile go
#   HB_BUILD                       local | cloud-build | none (default: none,
#                                  the dataset is prebuilt; see README.md)
#   HB_TEMPLATE                    the default base template (default-template)
#   HB_ATESPACE                    the atespace (ate-env)
#   HB_PLATFORM                    the build platform (linux/amd64)
set -o errexit -o nounset -o pipefail

: "${SUBSTRATE_ENV_API:?set SUBSTRATE_ENV_API to the port-forwarded ate-env-api, e.g. 127.0.0.1:7777}"

provider=(-e ate_env_harbor:AteEnvEnvironment --ek "build=${HB_BUILD:-none}")
if [[ -n "${SUBSTRATE_ENV_HARBOR_REGISTRY:-}" ]]; then
  provider+=(--ek "registry=${SUBSTRATE_ENV_HARBOR_REGISTRY}")
fi
if [[ -n "${HB_TEMPLATE:-}" ]]; then
  provider+=(--ek "template=${HB_TEMPLATE}")
fi
if [[ -n "${HB_ATESPACE:-}" ]]; then
  provider+=(--ek "atespace=${HB_ATESPACE}")
fi
if [[ -n "${HB_PLATFORM:-}" ]]; then
  provider+=(--ek "platform=${HB_PLATFORM}")
fi

if [[ $# -eq 0 ]]; then
  set -- -d terminal-bench@2.0 -a oracle -n 10
fi

exec harbor run "${provider[@]}" "$@"
