---
name: create-release
description:
  Draft and cut releases for sandboxenv-rl (Agent Substrate Environments), including release notes
  generation, human-in-the-loop review, git tag push, draft GitHub Release creation, and automated
  PyPI / container image publishing.
---

# Cutting Releases for Agent Substrate Environments (`sandboxenv-rl`)

Use this skill when preparing, drafting, and publishing a release of `sandboxenv-rl`.

## Release Architecture & Deliverables

Every `sandboxenv-rl` release produces four core deliverables:

| Deliverable                                                                                                   | Distribution Channel                                                                                                            | Verification / Security                                              |
| :------------------------------------------------------------------------------------------------------------ | :------------------------------------------------------------------------------------------------------------------------------ | :------------------------------------------------------------------- |
| **Go CLI (`ate-env`)**                                                                                        | GitHub Release Assets (`.tar.gz`)                                                                                               | Cosign keyless signatures & `SHA256SUMS_${TAG}.txt`                  |
| **Container Images** (`ate-env-api`, `ate-env-guest`, `oh-agent-server-layer`)                                | GitHub Container Registry (`ghcr.io/ai-on-gke/sandboxenv-rl/*`) and optional Google Artifact Registry (`vars.GAR_IMAGE_PREFIX`) | Cosign OIDC signatures via GitHub Actions                            |
| **Python Distributions** (`ate-env-client`, `nemo-gym-substrate`, `ate-env-openhands`, `uni-agent-substrate`) | PyPI                                                                                                                            | PyPA OIDC Trusted Publishing (`id-token: write`, environment `pypi`) |
| **gRPC Contracts & Protos**                                                                                   | Git repository tags & GitHub Release notes                                                                                      | Protocol buffer wire compatibility                                   |

## Release Procedure Overview

The release cutting process is split into two stages:

1. Human: Run `create-release.py context`, which queries the GitHub `generate-notes` API, merged PR
   descriptions, and contributor list to generate `hack/release/release-context.md`.
2. Agent: Synthesizes `hack/release/RELEASE_NOTES_vX.Y.Z.md` from `release-context.md` and
   `hack/release/release-template.md`.
3. Human: Reviews and refines `RELEASE_NOTES_vX.Y.Z.md`.
4. Human: Run `create-release.py draft`, which pushes the `vX.Y.Z` Git tag and creates a Draft
   GitHub Release with the release notes.
5. CI: Automatically runs on the pushed tag, building and signing container images, compiling
   `ate-env` binaries, and attaching assets and image digests to the Draft Release.
6. Human: Reviews the Draft Release on GitHub and clicks **Publish release**.
7. CI: Automatically runs on release publication, building and publishing the Python packages to
   PyPI via OIDC Trusted Publishing.

## Step-by-Step Instructions

### Gather Release Context

Run the release context command for your target semantic version:

```bash
python3 hack/release/create-release.py context --tag v0.1.0 --output hack/release/release-context.md
```

Options:

- `--tag <vX.Y.Z>`: The semver tag being prepared (e.g. `v0.1.0` or `v0.1.0-rc1`). Required.
- `--previous-tag <vA.B.C>`: Previous release tag to diff from. Optional (GitHub auto-detects if
  omitted).
- `--target-commitish <sha|branch>`: Commit or branch to tag (defaults to `HEAD`).

This command:

1. Calls the GitHub `releases/generate-notes` API to retrieve the PR list and commit ranges.
2. Queries PR details (labels, author, title) and extracts explicit `## Release note` blocks from PR
   descriptions.
3. Compiles a deduplicated list of non-bot contributors.
4. Outputs `hack/release/release-context.md`.

### Synthesize Release Notes (Human-in-the-Loop)

1. Open `hack/release/release-context.md` and `hack/release/release-template.md`.
2. Draft `hack/release/RELEASE_NOTES_<tag>.md` following the template:
   - **Title & Announcement**: Mention the release tag and short summary.
   - **Pre-Release Notice**: Include if the version is an alpha, beta, or release candidate.
   - **Breaking Changes / Action Required**: Highlight any breaking protocol, API, or configuration
     changes.
   - **Highlights**: Group merged pull requests by area:
     - _Core Runtime, CLI & Substrate_
     - _Python SDK & Fleet_
     - _Framework Integrations_ (NeMo Gym, OpenHands, uni-agent)
     - _Observability & Benchmarking_ Each bullet point should link the PR:
       `* Description of feature or fix (#<pr_number>)`.
   - **Full Changelog**: Include the categorized list or link from the generated notes.
   - **Installation**: Keep the verbatim installation blocks for CLI binaries, container images with
     `cosign verify` commands, and Python packages from PyPI.
   - **Contributors**: List all contributor `@handles`.
3. Present the drafted notes to the user for review and confirm approval.

### Create the Draft Release

Once the notes are approved:

1. Perform a dry-run check:

   ```bash
   python3 hack/release/create-release.py draft --dry-run --tag v0.1.0 --notes-file hack/release/RELEASE_NOTES_v0.1.0.md
   ```

2. Execute the draft release:
   ```bash
   python3 hack/release/create-release.py draft --tag v0.1.0 --notes-file hack/release/RELEASE_NOTES_v0.1.0.md
   ```

This command:

- Creates the local annotated tag `vX.Y.Z` if not already present.
- Pushes the tag `refs/tags/vX.Y.Z` to `origin`.
- Creates or updates a **draft** release on GitHub with `--draft --verify-tag`.

### Automated CI Build & Signing

The tag push automatically triggers the `.github/workflows/release.yaml` workflow in GitHub Actions:

- **Container Build & Sign**: Builds multi-arch images (`linux/amd64`, `linux/arm64`) for
  `ate-env-api`, `ate-env-guest`, and `oh-agent-server-layer`. Pushes images to GHCR (and optional
  GAR if `vars.GAR_IMAGE_PREFIX` is set), signs them with Cosign, and records digests.
- **CLI Compilation**: Compiles `ate-env` for Linux (`amd64`, `arm64`) and macOS (`amd64`, `arm64`).
- **Checksums & Attestations**: Computes `SHA256SUMS_${TAG}.txt` and generates Cosign signatures.
- **Asset Attachment**: Uploads binary archives, checksums, and signatures to the draft GitHub
  Release, and appends an `## Images` verification table to the release notes.

Monitor the workflow run in GitHub Actions:

```bash
gh run list --workflow release.yaml
```

### Officiate the Release and Publish to PyPI

1. Navigate to the draft release in your browser:
   `https://github.com/ai-on-gke/sandboxenv-rl/releases`
2. Inspect the attached assets (`ate-env_*.tar.gz`, `SHA256SUMS_*.txt`, `.sig` files) and the
   verified image table.
3. Click **Edit** -> **Publish release**.
4. Officiating the release fires the `release: types: [published]` event, which triggers the
   `publish-pypi` job in `.github/workflows/release.yaml`. It builds source distributions and wheels
   for `clients/python`, `integrations/nemo-gym`, `integrations/openhands`, and
   `integrations/uni-agent` via `pyproject-build --no-isolation` and publishes them to PyPI via PyPA
   OIDC Trusted Publishing.

## Troubleshooting & Notes

- **Overwriting a published release**: The script refuses to overwrite an existing published
  release. If a patch is needed, bump the patch version (e.g. `v0.1.1`).
- **Dynamic Python Versioning**: Python packages use `setuptools-scm>=8` with `root = "../.."`. The
  tag `vX.Y.Z` determines the distribution version on PyPI automatically.
- **Optional GAR Mirroring**: If `vars.GAR_IMAGE_PREFIX` is set in repository variables alongside
  Workload Identity Federation credentials (`vars.GCP_WORKLOAD_IDENTITY_PROVIDER` and
  `vars.GCP_SERVICE_ACCOUNT`), images are automatically mirrored to Google Artifact Registry and
  signed with Cosign.
