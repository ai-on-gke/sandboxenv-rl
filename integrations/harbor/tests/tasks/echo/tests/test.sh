#!/bin/sh

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

# Reward 1 when the agent wrote the file and the environment/ upload landed
# in the workdir; 0 otherwise. Needs only sh (no network).
mkdir -p /logs/verifier
if [ -f /app/hello.txt ] && [ "$(cat /app/out.txt 2>/dev/null)" = "hello" ]; then
  echo 1 > /logs/verifier/reward.txt
else
  echo 0 > /logs/verifier/reward.txt
fi
