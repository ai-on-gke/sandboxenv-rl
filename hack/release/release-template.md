# Agent Substrate Environments ($TAG)

We are pleased to announce the release of **Agent Substrate Environments (`sandboxenv-rl`) $TAG**!

<!--
Optional: Uncomment if this is an alpha, beta, or release candidate:
### ⚠️ Pre-Release Notice
This is a pre-release version of `sandboxenv-rl`. It is intended for testing and validation.
-->

<!--
Optional: Uncomment if there are breaking changes or manual migrations required:
### ⚠️ Breaking Changes / Action Required
- **[Component]**: Description of breaking change and migration steps (#PR).
-->

### Highlights

#### Core Runtime, CLI & Substrate
- Highlight 1 (#PR)

#### Python SDK & Fleet
- Highlight 1 (#PR)

#### Framework Integrations (NeMo Gym, OpenHands, uni-agent)
- Highlight 1 (#PR)

### Full Changelog
$CHANGELOG

### Installation

#### Go CLI (`ate-env`)
Download pre-built binaries from the GitHub Release assets:
```bash
# Example for Linux amd64
curl -sSLO "https://github.com/ai-on-gke/sandboxenv-rl/releases/download/$TAG/ate-env_${TAG#v}_linux_amd64.tar.gz"
tar -xzf "ate-env_${TAG#v}_linux_amd64.tar.gz"
sudo mv ate-env /usr/local/bin/
```

#### Container Images
Container images are published to GitHub Container Registry (GHCR):
```bash
# Agent Substrate Environment API server
docker pull ghcr.io/ai-on-gke/sandboxenv-rl/ate-env-api:$TAG

# Guest agent init/exec daemon
docker pull ghcr.io/ai-on-gke/sandboxenv-rl/ate-env-guest:$TAG

# OpenHands agent server layer
docker pull ghcr.io/ai-on-gke/sandboxenv-rl/oh-agent-server-layer:$TAG
```

Verify image signatures with Cosign:
```bash
cosign verify \
  --certificate-identity-regexp "https://github.com/ai-on-gke/sandboxenv-rl/.github/workflows/release.yaml" \
  --certificate-oidc-issuer "https://token.actions.githubusercontent.com" \
  ghcr.io/ai-on-gke/sandboxenv-rl/ate-env-api:$TAG
```

#### Python Packages (PyPI)
Install the client SDK and integration adapters from PyPI:
```bash
# Core async Python client
pip install "ate-env-client==$VERSION"

# NeMo Gym integration
pip install "nemo-gym-substrate==$VERSION"

# OpenHands integration
pip install "ate-env-openhands==$VERSION"

# uni-agent integration
pip install "uni-agent-substrate==$VERSION"
```

### Contributors
$CONTRIBUTORS
