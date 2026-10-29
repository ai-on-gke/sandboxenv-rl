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

"""ImageResolver against stub docker and gcloud executables.

The stubs record every invocation and emulate the three commands the
resolver runs: ``buildx imagetools inspect`` (a digest for images that
exist), ``buildx build --push`` (writes the metadata file and marks the tag
as existing) and ``gcloud builds submit``.
"""

from __future__ import annotations

import asyncio
from pathlib import Path

import pytest

from ate_env_harbor.images import (
    ImageError,
    ImageResolver,
    repository_name,
    repository_of,
)

from .conftest import ALPINE, _digest

REGISTRY = "reg.example/harbor"

@pytest.fixture
def dockerfile_dir(tmp_path) -> Path:
    env_dir = tmp_path / "task" / "environment"
    env_dir.mkdir(parents=True)
    (env_dir / "Dockerfile").write_text("FROM alpine:3.20\nWORKDIR /app\n")
    return env_dir


def resolver(stubs, **kw) -> ImageResolver:
    options = {"registry": REGISTRY, "build": "local", "docker": stubs.docker, "gcloud": stubs.gcloud}
    options.update(kw)
    return ImageResolver(**options)


async def resolve(r: ImageResolver, env_dir: Path, *, docker_image=None, force=False, name="echo", eid="abc123"):
    return await r.resolve(
        docker_image=docker_image,
        environment_dir=env_dir,
        environment_name=name,
        environment_id=eid,
        force_build=force,
        build_timeout_sec=60,
    )


def test_repository_helpers():
    assert repository_of("ubuntu:24.04") == "ubuntu"
    assert repository_of("reg:5000/a/b:tag") == "reg:5000/a/b"
    assert repository_of("a/b@sha256:x") == "a/b"
    assert repository_of("a/b:t@sha256:x") == "a/b"
    assert repository_name("hello-world") == "hello-world"
    assert repository_name("Hello World!/v2") == "hello-world/v2"
    assert repository_name("a//b-") == "a/b"
    assert repository_name("...") == "task"


async def test_digest_reference_is_used_as_is(stubs, tmp_path):
    r = resolver(stubs, build="none", registry=None)
    assert await resolve(r, tmp_path, docker_image=ALPINE) == ALPINE
    assert stubs.calls() == []


async def test_tag_is_resolved_once(stubs, tmp_path):
    r = resolver(stubs, build="none", registry=None)
    ref = await resolve(r, tmp_path, docker_image="ubuntu:24.04")
    assert ref == f"ubuntu@{_digest('ubuntu:24.04')}"
    assert await resolve(r, tmp_path, docker_image="ubuntu:24.04") == ref
    assert len(stubs.calls()) == 1


async def test_tag_without_docker_names_the_fix(tmp_path):
    r = ImageResolver(registry=None, build="none", docker="no-such-docker-binary")
    with pytest.raises(ImageError, match="pin it as repo@sha256"):
        await resolve(r, tmp_path, docker_image="ubuntu:24.04")


async def test_dockerfile_is_built_pushed_and_cached(stubs, dockerfile_dir):
    r = resolver(stubs)
    tag = f"{REGISTRY}/echo:abc123"
    ref = await resolve(r, dockerfile_dir)
    assert ref == f"{REGISTRY}/echo@{_digest(tag)}"
    build = stubs.builds()
    assert len(build) == 1
    assert "--push" in build[0] and "--provenance=false" in build[0]
    assert build[0][build[0].index("--platform") + 1] == "linux/amd64"
    assert build[0][-1] == str(dockerfile_dir)
    # Cached inside the process, and found in the registry by a new resolver.
    assert await resolve(r, dockerfile_dir) == ref
    assert await resolve(resolver(stubs), dockerfile_dir) == ref
    assert len(stubs.builds()) == 1


async def test_force_build_rebuilds(stubs, dockerfile_dir):
    r = resolver(stubs)
    await resolve(r, dockerfile_dir)
    await resolve(r, dockerfile_dir, force=True)
    assert len(stubs.builds()) == 2


async def test_concurrent_resolves_build_once(stubs, dockerfile_dir, monkeypatch):
    monkeypatch.setenv("STUB_BUILD_SLEEP", "0.3")
    r = resolver(stubs)
    refs = await asyncio.gather(*(resolve(r, dockerfile_dir) for _ in range(20)))
    assert len(set(refs)) == 1
    assert len(stubs.builds()) == 1


async def test_prebuilt_image_wins_unless_forced(stubs, dockerfile_dir):
    r = resolver(stubs)
    assert await resolve(r, dockerfile_dir, docker_image=ALPINE) == ALPINE
    assert stubs.builds() == []
    assert await resolve(r, dockerfile_dir, docker_image=ALPINE, force=True) != ALPINE
    assert len(stubs.builds()) == 1


async def test_build_none_refuses_a_dockerfile(stubs, dockerfile_dir):
    with pytest.raises(ImageError, match="harbor admin upload-images"):
        await resolve(resolver(stubs, build="none"), dockerfile_dir)


async def test_registry_is_required_to_build(stubs, dockerfile_dir):
    with pytest.raises(ImageError, match="--ek registry="):
        await resolve(resolver(stubs, registry=None), dockerfile_dir)


async def test_build_failure_shows_the_output(stubs, dockerfile_dir, monkeypatch):
    monkeypatch.setenv("STUB_BUILD_FAIL", "1")
    with pytest.raises(ImageError, match="the Dockerfile is broken"):
        await resolve(resolver(stubs), dockerfile_dir)


async def test_cloud_build(stubs, dockerfile_dir):
    r = resolver(stubs, build="cloud-build")
    tag = f"{REGISTRY}/echo:abc123"
    assert await resolve(r, dockerfile_dir) == f"{REGISTRY}/echo@{_digest(tag)}"
    submit = [c for c in stubs.calls() if c[:2] == ["builds", "submit"]]
    assert len(submit) == 1 and "--tag" in submit[0] and "--timeout" in submit[0]


def test_build_mode_is_validated():
    with pytest.raises(ValueError, match="build must be one of"):
        ImageResolver(registry=None, build="remote")


async def test_two_resolvers_share_one_build(stubs, dockerfile_dir, monkeypatch):
    # Harbor makes one environment, thus one resolver, per trial: the cache
    # and the locks are per process.
    monkeypatch.setenv("STUB_BUILD_SLEEP", "0.3")
    refs = await asyncio.gather(*(resolve(resolver(stubs), dockerfile_dir) for _ in range(5)))
    assert len(set(refs)) == 1
    assert len(stubs.builds()) == 1
