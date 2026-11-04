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

# A runtime layer with the tools Harbor's terminus agents install at the
# start of every trial: tmux and asciinema. Mounted read-only into any task
# image (ate-env manifest template --layer tools=<this image@sha256:...>=/opt/harbor-tools),
# it appears as /opt/harbor-tools/bin/{tmux,asciinema}. Both binaries are
# static, thus they run on glibc and musl images alike; the layer is
# linux/amd64, the worker nodes' architecture.
#
# Build and push:  docker buildx build --platform linux/amd64 --push -t <repo>:tmux-3.8 layer
# Pin by digest:   docker buildx imagetools inspect --format '{{json .Manifest.Digest}}' <repo>:tmux-3.8

FROM alpine:3.20@sha256:d9e853e87e55526f6b2917df91a2115c36dd7c696a35be12163d44e6e2a4b6bc AS build

RUN apk add --no-cache build-base bison pkgconf \
      ncurses-dev ncurses-static libevent-dev libevent-static

ARG TMUX_VERSION=3.8
ARG TMUX_SHA256=e79c699c7e949dccd0a4a125e17b8d1261e311b16979dbc8eb34542f3966d82e
ADD https://github.com/tmux/tmux/releases/download/${TMUX_VERSION}/tmux-${TMUX_VERSION}.tar.gz /src/tmux.tar.gz
RUN echo "${TMUX_SHA256}  /src/tmux.tar.gz" | sha256sum -c - \
 && tar -xzf /src/tmux.tar.gz -C /src \
 && cd /src/tmux-${TMUX_VERSION} \
 && ./configure --enable-static \
 && make -j"$(nproc)" \
 && strip tmux \
 && mkdir -p /out/bin && mv tmux /out/bin/tmux

# asciinema 3 is a Rust program; its musl release binary is static.
ARG ASCIINEMA_VERSION=v3.2.1
ARG ASCIINEMA_SHA256=bec9781bc8f297a9d3d74ff60205599507f2abba1183578b8b2f22be4c999214
ADD https://github.com/asciinema/asciinema/releases/download/${ASCIINEMA_VERSION}/asciinema-x86_64-unknown-linux-musl /out/bin/asciinema
RUN echo "${ASCIINEMA_SHA256}  /out/bin/asciinema" | sha256sum -c - && chmod 0755 /out/bin/asciinema

# Only the binaries: the image is a mount, not a container that runs.
FROM scratch
COPY --from=build /out/bin/tmux /bin/tmux
COPY --from=build /out/bin/asciinema /bin/asciinema
