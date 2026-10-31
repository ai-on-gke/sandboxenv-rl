# Releases and Published Deliverables

A release of `sandboxenv-rl` is anchored to a semantic version tag `vX.Y.Z` on `main`. The release process produces four core deliverables:

| Deliverable | Distribution Channel | Verification / Security |
| :--- | :--- | :--- |
| **Go CLI (`ate-env`)** | GitHub Release assets (`ate-env_<tag>_<os>_<arch>` and `.tar.gz`) for Linux and macOS (`amd64`, `arm64`) | `SHA256SUMS_<tag>.txt` & Cosign blob signatures (`.sig`, `.pem`) |
| **Container Images** (`ate-env-api`, `ate-env-guest`, `oh-agent-server-layer`) | GitHub Container Registry (`ghcr.io/ai-on-gke/sandboxenv-rl/*`) and optional Google Artifact Registry (`vars.GAR_IMAGE_PREFIX`) | Multi-arch (`linux/amd64`, `linux/arm64`), pinned by digest, and signed with Cosign keyless signatures bound to GitHub Actions OIDC |
| **Python Distributions** (`ate-env-client`, `nemo-gym-substrate`, `ate-env-openhands`, `uni-agent-substrate`) | PyPI | Built with `setuptools-scm>=8` and published via PyPA OIDC Trusted Publishing (`id-token: write`) |
| **gRPC Contracts & Protos** | Git repository tags & GitHub Release notes | Wire-compatible Protocol Buffers (`proto/ateenv/v1alpha`) |

## What a Release Binary Knows

The `ate-env` binaries attached to a release are built after container images have been pushed and signed, baking their SHA-256 digests into the CLI defaults:
- `ate-env manifest` defaults `--api-image` to the release's `ate-env-api` digest.
- `ate-env manifest template` defaults `--guest-image` to the release's `ate-env-guest` digest.
- `ate-env --version` prints the release tag.

A build from source (`go install ...@latest`, `go build`) has no baked-in defaults and requires both flags. Running `make build-cli` locally reproduces a release-style binary using `ATE_ENV_API_IMAGE` and `ATE_ENV_GUEST_IMAGE`.

> [!NOTE]
>
> The worker image (`--worker-image`) is not published from this repository: it is `ateworker-gvisor` from the [Substrate repository](https://github.com/agent-substrate/substrate) and must match the Substrate version deployed on the cluster. From a Substrate checkout at that version, run `go run ./cmd/ate-setup publish worker-images` to build and push it.

## The OpenHands Runtime Layer

The release publishes the runtime layer that [`integrations/openhands`](../integrations/openhands/README.md) mounts into task images at `/opt/oh`: the OpenHands agent server with its own isolated Python environment.

- Built from [`integrations/openhands/layer`](../integrations/openhands/layer) with `docker buildx` for `linux/amd64` and `linux/arm64`.
- Dependencies are hash-locked and base images are pinned by digest. The build verifies that the installed agent-server, SDK, and tools match the version specified in `layer/requirements.in`.
- Tagged `<agent-server version>-<release tag>` (e.g. `1.55.0-v0.1.0`) after signing.
- Use the digest from the release notes or `images_<tag>.txt` in the base template:

```bash
ate-env manifest template --template oh-agent-server \
  --layer oh=ghcr.io/ai-on-gke/sandboxenv-rl/oh-agent-server-layer@sha256:<digest>=/opt/oh ...
```

The client package `ate-env-openhands` pins `openhands-sdk` to the same version as the layer's agent-server; always use the layer and the client package from the same release.

## Image Verification & Signing

Images are multi-platform (`linux/amd64`, `linux/arm64`) and signed using Cosign keyless signing bound to this repository's GitHub Actions workflow identity:

```bash
cosign verify \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com \
  --certificate-identity-regexp '^https://github.com/ai-on-gke/sandboxenv-rl/\.github/workflows/release\.yaml@refs/tags/' \
  ghcr.io/ai-on-gke/sandboxenv-rl/ate-env-guest@sha256:<digest>
```

The same command verifies `ate-env-api` and `oh-agent-server-layer`; replace only the image reference. A valid signature proves the image was built by this repository's `release` workflow from a verified tag ref.

## Cutting a Release

The release cutting process is split into two stages:

1. Human: Run `create-release.py context`, which queries the GitHub `generate-notes` API, merged PR descriptions, and contributor list to generate `hack/release/release-context.md`.
2. Agent: Synthesizes `hack/release/RELEASE_NOTES_vX.Y.Z.md` from `release-context.md` and `hack/release/release-template.md`.
3. Human: Reviews and refines `RELEASE_NOTES_vX.Y.Z.md`.
4. Human: Run `create-release.py draft`, which pushes the `vX.Y.Z` Git tag and creates a Draft GitHub Release with the release notes.
5. CI: Automatically runs on the pushed tag, building and signing container images, compiling `ate-env` binaries, and attaching assets and image digests to the Draft Release.
6. Human: Reviews the Draft Release on GitHub and clicks **Publish release**.
7. CI: Automatically runs on release publication, building and publishing the Python packages to PyPI via OIDC Trusted Publishing.

### Detailed Commands

#### Gather Context & Draft Release Notes
```bash
python3 hack/release/create-release.py context --tag v0.1.0 --output hack/release/release-context.md
```
Review `hack/release/release-context.md` and use `hack/release/release-template.md` to draft `hack/release/RELEASE_NOTES_v0.1.0.md`. Categorize changes into Highlights (Core Runtime, Python SDK, Integrations), verify breaking changes, and preserve verbatim installation instructions.

#### Push Tag and Create Draft Release
Perform a dry-run check:
```bash
python3 hack/release/create-release.py draft --dry-run --tag v0.1.0 --notes-file hack/release/RELEASE_NOTES_v0.1.0.md
```
Create the draft release:
```bash
python3 hack/release/create-release.py draft --tag v0.1.0 --notes-file hack/release/RELEASE_NOTES_v0.1.0.md
```

#### Inspect Automated CI Build
The tag push triggers `.github/workflows/release.yaml`. It builds multi-arch container images, signs them with Cosign, cross-compiles CLI binaries, generates `SHA256SUMS_${TAG}.txt`, signs the checksums, and attaches all assets to the draft release.

Monitor progress:
```bash
gh run list --workflow release.yaml
```

#### Publish Release and Snowball to PyPI
Open `https://github.com/ai-on-gke/sandboxenv-rl/releases`, review the draft release notes, attached binaries, and the `## Images` table, then click **Publish release**.

Publishing the release fires the `release: types: [published]` event, which automatically builds and publishes the four Python distributions (`ate-env-client`, `nemo-gym-substrate`, `ate-env-openhands`, `uni-agent-substrate`) to PyPI using GitHub Actions OIDC Trusted Publishing.

## Repository Configuration & Permissions

### GitHub Container Registry (GHCR)
GitHub Container Registry packages are created on the first push by the workflow's `GITHUB_TOKEN`.
1. Ensure GitHub Actions has package creation rights under organization settings.
2. Confirm each package (`ate-env-api`, `ate-env-guest`, `oh-agent-server-layer`) is public and linked to this repository.

### Optional: Google Artifact Registry (GAR) Mirroring
To mirror container images to Google Artifact Registry, configure the following repository variables in GitHub Settings -> Secrets and variables -> Actions:
- `vars.GAR_IMAGE_PREFIX`: e.g. `us-docker.pkg.dev/my-project/my-repo`
- `vars.GCP_WORKLOAD_IDENTITY_PROVIDER`: Workload Identity Federation provider resource name
- `vars.GCP_SERVICE_ACCOUNT`: Google service account email with Artifact Registry Writer permissions

When `vars.GAR_IMAGE_PREFIX` is set, `release.yaml` authenticates via Workload Identity Federation, mirrors the images to GAR, signs them with Cosign, and includes their references in the release notes.

### PyPI Trusted Publishing
Each Python package is configured on [pypi.org](https://pypi.org) under Publishing -> "Add a GitHub publisher":
- Owner: `ai-on-gke`
- Repository: `sandboxenv-rl`
- Workflow name: `release.yaml`
- Environment: `pypi`
- Project names: `ate-env-client`, `nemo-gym-substrate`, `ate-env-openhands`, `uni-agent-substrate`

With OIDC Trusted Publishing, no long-lived PyPI API tokens or secrets are stored in GitHub.

## Reproducibility & Supply Chain Security Notes

- **Hermetic Nix Environment**: Release workflows execute CLI cross-compilation, container image builds, and Python packaging within the repository's Nix environment (`flake.lock` and `uv.lock`). Python distributions are built with `pyproject-build --no-isolation` using Nix-provided `build`, `setuptools`, `setuptools-scm`, and `wheel` rather than dynamic runtime `pip` installations.
- **Checksums**: Only a single versioned checksum file (`SHA256SUMS_${TAG}.txt`) is generated and uploaded.
- **Base images**: The guest base image is pinned by digest in [`.ko.yaml`](../.ko.yaml). The API image uses ko's default distroless base. The OpenHands layer pins base images by digest in its Dockerfile, its Python dependencies by hash in `layer/requirements.txt`, and both the QEMU binfmt image and the BuildKit image that builds and pushes the layer are pinned by digest in `release.yaml`.
- **Action pinning**: All GitHub Actions are pinned by full commit SHA with version comments to satisfy `zizmor` security checks.
- **Cache isolation**: Release builds run without Go module caching to avoid consuming poisoned caches from other branch runs.
