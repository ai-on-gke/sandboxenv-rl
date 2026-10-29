# Releases and published images

A release of this repo is a git tag `vX.Y.Z` on `main`. Pushing the tag runs
[`.github/workflows/release.yaml`](../.github/workflows/release.yaml), which
publishes everything a user needs from this repo:

| artifact | where | how to reference it |
|---|---|---|
| `ate-env-api` image | `ghcr.io/ai-on-gke/sandboxenv-rl/ate-env-api` | by digest, from the release notes or `images_<tag>.txt` |
| `ate-env-guest` image | `ghcr.io/ai-on-gke/sandboxenv-rl/ate-env-guest` | by digest, same places |
| OpenHands runtime layer | `ghcr.io/ai-on-gke/sandboxenv-rl/oh-agent-server-layer` | by digest, same places; see [The OpenHands layer](#the-openhands-layer) |
| `ate-env` CLI | release assets `ate-env_<tag>_<os>_<arch>` for linux and darwin, amd64 and arm64 | download, `chmod +x` |
| checksums | `SHA256SUMS_<tag>.txt` | `sha256sum -c` |

Images are multi-platform (linux/amd64, linux/arm64) and signed with cosign
keyless signing bound to this repository's GitHub Actions identity. The two
ate-env images are tagged with the release tag and `latest`; the OpenHands layer
is tagged `<agent-server version>-<release tag>`, for example `1.55.0-v0.2.0`,
after it is signed, and has no `latest`.

## What a release binary knows

The `ate-env` binaries attached to a release are built after the images, with
the two digests baked in. `ate-env manifest` defaults `--api-image` to the
release's `ate-env-api`, and `ate-env manifest template` defaults
`--guest-image` to the release's `ate-env-guest`. `ate-env --version` prints the
tag. A build from source (`go install ...@latest`, `go build`) has no defaults
and asks for both flags. `make build-cli` reproduces a release-style binary
locally from `ATE_ENV_API_IMAGE` and `ATE_ENV_GUEST_IMAGE`.

The worker image (`--worker-image`) is not published here: it is
`ateworker-gvisor` from the [Substrate repo](https://github.com/agent-substrate/substrate)
and must match the Substrate version deployed on the cluster. From a Substrate
checkout at that version, `go run ./cmd/ate-setup publish worker-images` builds
and pushes it and prints the digest-pinned ref. Use the digest: a registry's
`:latest` is whatever was built last, and a stale worker image makes workers
register with no capacity, so no actor finds room.

## The OpenHands layer

The release also publishes the runtime layer that
[`integrations/openhands`](../integrations/openhands/README.md) mounts into task
images at `/opt/oh`: the OpenHands agent-server with its own Python. It is built
from [`integrations/openhands/layer`](../integrations/openhands/layer) with
`docker buildx` for linux/amd64 and linux/arm64; QEMU runs the arm64 stages on
the amd64 runner. Its dependencies are hash-locked and its base images are pinned
by digest, and the build fails when the installed agent-server, sdk and tools
are not the version in `layer/requirements.in`. That version is the first part
of the tag. The image index carries no provenance attestation, like the ko
images; the cosign signature records where it comes from.

Use the digest from the release notes or `images_<tag>.txt` in the base
template:

```bash
ate-env manifest template --template oh-agent-server \
  --layer oh=ghcr.io/ai-on-gke/sandboxenv-rl/oh-agent-server-layer@sha256:<digest>=/opt/oh ...
```

The client package `ate-env-openhands` pins `openhands-sdk` to the same version
as the layer's agent-server; use the layer and the package from the same release.

## Pin by digest

Always deploy images by digest, never by tag. Substrate requires it for
ActorTemplates because a changed image invalidates snapshots, and the `latest`
tag moves with every release. The release notes and `images_<tag>.txt` carry the
digests; the CLI defaults are digests.

## Verify a signature

```bash
cosign verify \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com \
  --certificate-identity-regexp '^https://github.com/ai-on-gke/sandboxenv-rl/\.github/workflows/release\.yaml@refs/tags/' \
  ghcr.io/ai-on-gke/sandboxenv-rl/ate-env-guest@sha256:<digest>
```

The same command verifies `ate-env-api` and `oh-agent-server-layer`; change
only the image reference.

A valid signature proves the image was built by this repository's `release`
workflow from a tag: the identity is the workflow file at a `refs/tags/` ref,
so a signature from another workflow or from a branch run does not verify. The
workflow refuses to run from any ref other than the tag it publishes, thus a
republished image verifies with the same command.

The workflow signs with Cosign 3 (the version `sigstore/cosign-installer` v4
installs by default), which writes the standardized bundle format and stores
it as an OCI 1.1 referring artifact next to the image. Verify with Cosign 3 or
newer; a Cosign 2 client does not find these signatures.

## Cutting a release

```bash
git tag -a v0.1.0 -m "v0.1.0"
git push origin v0.1.0
```

The workflow runs in this order:

1. It builds the OpenHands layer for both platforms and pushes it by digest,
   with no tag. This is the slow step.
2. It builds and pushes the two ate-env images with the release tag and `latest`.
3. It signs all three images in one step, directly after the push.
4. It tags the layer `<agent-server version>-<release tag>`. The tag points at
   the signed digest.
5. It builds the CLI for four platforms, creates the GitHub release with
   generated notes, uploads the assets, and appends an `## Images` section with
   the digests and the verify command to the notes.

If a step fails, the job stops there. A failed layer build leaves no tag and no
release: the registry has at most an untagged layer index. After step 2, a tag
points at an unsigned image only until step 3 completes. If a later step fails,
the images are signed, but the layer tag, the assets or the notes can be
missing until a republish.

To republish an existing tag after a failed run, start the workflow by hand
from the Actions tab: select the tag in "Use workflow from" and enter the same
tag as the input. The workflow refuses another ref, because the signature
identity names the ref and the verify command above accepts only `refs/tags/`.
The run uses the workflow file at that tag, thus a fix to the workflow on `main`
needs a new tag. A republish replaces the assets and the images section rather than
duplicating them, and produces new digests, since the images are rebuilt.

## First-time setup for the organization

GitHub Container Registry packages are created on the first push by the
workflow's `GITHUB_TOKEN`, under the repository. Once:

1. Allow GitHub Actions to create packages for the organization (organization
   settings, Packages).
2. After the first release, open each package (`ate-env-api`, `ate-env-guest`,
   `oh-agent-server-layer`) and confirm it is public and linked to this
   repository, so anonymous pulls and the repository's permissions apply.

## Reproducibility notes

- The guest's base image is pinned by digest in [`.ko.yaml`](../.ko.yaml). It
  is a `bash` image so that templates whose container is the guest itself have
  a shell for `shell()`; for rootfs-image templates only the static binary is
  used. Bump the pin deliberately and mention it in the release notes.
- The API image uses ko's default distroless base.
- The OpenHands layer pins its base images by digest in its Dockerfile and its
  Python dependencies by hash in `layer/requirements.txt`. The QEMU binfmt
  image and the BuildKit image that builds and pushes the layer are pinned by
  digest in `release.yaml`. Dependabot proposes updates for the Dockerfile's
  base images, not for these two pins. To bump one, resolve the new tag's
  digest (`crane digest <image>:<tag>`) and update the tag and the digest
  together.
- The actions both workflows use are pinned by commit SHA, with the version in
  a trailing comment; the organization's workflow security scan (zizmor)
  requires it. To bump one, resolve the new tag's commit (`git ls-remote --tags
  https://github.com/<owner>/<action> v<x.y.z>`) and update the SHA and the
  comment together. The release job also runs with Go module caching off so a
  release build cannot consume a cache seeded by another run.
- `make images` builds the same two images for a registry of your choice
  (`ATE_ENV_IMAGE_REPO`), for development against a private cluster; the
  released images are the ones to point customers at.
