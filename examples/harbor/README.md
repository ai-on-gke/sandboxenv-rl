# Terminal-Bench 2.0 on ate-env

Run a Harbor dataset with each trial in an ate-env environment, through the
provider in [`integrations/harbor`](../../integrations/harbor/README.md). The
steps below use Terminal-Bench 2.0; any task directory or registry dataset
works the same way.

## 1. Prerequisites

- A cluster with ate-env deployed ([deployment guide](../../README.md#deployment-guide))
  and `ate-env-api` port-forwarded:

  ```bash
  kubectl -n ate-env port-forward svc/ate-env-api 7777:7777 &
  export SUBSTRATE_ENV_API=127.0.0.1:7777
  export SUBSTRATE_ENV_AUTH_TOKEN=$(kubectl -n ate-env get secret ate-env-api-token -o jsonpath='{.data.token}' | base64 -d)
  ```

- A registry the worker nodes can pull from, for the task images. With Artifact
  Registry, the node pool's service account needs `roles/artifactregistry.reader`
  on the repository, and your machine a login (`gcloud auth configure-docker
  <region>-docker.pkg.dev`):

  ```bash
  export SUBSTRATE_ENV_HARBOR_REGISTRY=us-central1-docker.pkg.dev/<project>/harbor
  ```

- Python 3.12 or newer, docker with buildx, and the two packages from this checkout:

  ```bash
  pip install -e clients/python -e integrations/harbor
  ```

- Egress from the actors. Most tasks and every installed agent download packages
  while they run; ate-env sets no egress policy, what an actor reaches follows the
  Substrate defaults. Check once with a throwaway environment:

  ```bash
  ate-env create hb-probe --rootfs-image ubuntu@sha256:<digest>
  ate-env hb-probe shell 'apt-get update >/dev/null && echo egress ok'
  ate-env hb-probe delete
  ```

## 2. Get the tasks

```bash
harbor datasets download terminal-bench@2.0 -o ./tb2   # ./tb2/terminal-bench/<task>/...
```

## 3. Prebuild the images

ate-env runs images by digest. Build the dataset once and push it to your
registry; Harbor writes the image tags into the task files, and the provider
resolves them to digests at run time:

```bash
harbor admin upload-images -t ./tb2/terminal-bench -r "$SUBSTRATE_ENV_HARBOR_REGISTRY" \
  --tag tb2 --push --update-config
```

The alternative is to let the provider build each task's `environment/Dockerfile`
at its first trial (`HB_BUILD=local`, or `cloud-build` for a build in your GCP
project). Builds are skipped when the tag exists and happen once per task, but a
run at `-n 50` then starts with up to 50 builds on your machine. Prebuilding is
the better default.

## 4. Register the resource classes

Substrate requires CPU and memory limits on every template, so each (cpus,
memory_mb) pair a task asks for needs a base template named for it. The script
prints one `ate-env manifest template` command per class the dataset uses; the
flags after `--` go on each, at least the guest image and the snapshots bucket
from your deployment:

```bash
integrations/harbor/hack/register-templates.sh ./tb2/terminal-bench -- \
  --guest-image <registry>/ate-env-guest@sha256:<digest> \
  --snapshots-bucket <object-storage-url> | sh
```

Without a class template the provider falls back to `default-template` with a
warning (the `auto` policy); `harbor run --cpus limit` makes that an error.

## 5. Run

```bash
examples/harbor/run-tb2.sh -p ./tb2/terminal-bench -a oracle -n 10
```

`run-tb2.sh` is `harbor run` with the provider selected and its settings read
from the environment (`HB_BUILD`, `HB_TEMPLATE`, `HB_ATESPACE`,
`SUBSTRATE_ENV_HARBOR_REGISTRY`); every flag is passed through. The `oracle`
agent runs each task's reference solution and needs no model: its rewards are
the dataset's ground truth, which makes it the right first run and the right
comparison with Docker. With a model:

```bash
export ANTHROPIC_API_KEY=...   # or the provider your model needs
examples/harbor/run-tb2.sh -p ./tb2/terminal-bench -a terminus-2 -m anthropic/claude-sonnet-5-5 -n 50
```

Keep the token in `SUBSTRATE_ENV_AUTH_TOKEN`, not in `--ek api_token=`: Harbor
records the environment kwargs in each trial's `result.json`.

## 6. Read the results

```bash
harbor view jobs/<job>
cat jobs/<job>/<trial>/result.json | jq .verifier_result.rewards
cat jobs/<job>/<trial>/ate_env.json          # environment id, atespace, template, image digest
```

`--no-delete` keeps each environment suspended after its trial; the log names
the `ate-env <id> shell` command to inspect it. Delete them when done:
`ate-env <id> delete`.

## 7. Compare with Docker

The same command with `-e docker` (Harbor's default) runs the tasks in local
containers:

```bash
harbor run -p ./tb2/terminal-bench -a oracle -n 10 -e docker
```

For `oracle` the rewards must match task by task. Timing numbers for ate-env
(create, exec latency, total wall time at `-n 50`) next to Docker's are
recorded here once the plan's performance criterion has run.

| Measure | ate-env, plain create | ate-env, warm pool | Docker |
|---|---|---|---|
| Create or adopt, p50 / p95 | to be measured | to be measured | to be measured |
| `exec` latency, p50 / p95 | to be measured | to be measured | to be measured |
| Wall time, 50 `hello-world` trials with `nop` | to be measured | to be measured | to be measured |

## Troubleshooting

| Symptom | Cause and fix |
|---|---|
| `InvalidArgument: image ... is not pinned by digest` | A tag reached the API: `docker` is not on PATH, so the tag could not be resolved. Install docker or pin the digest in `task.toml`. |
| `FailedPrecondition: base actor template "harbor-c..." not found` | Register the class template (step 4). Under the `auto` policy this is a warning and the run goes on with `default-template`. |
| The create succeeds, then `did not answer within 600s` | The nodes cannot pull the image from the registry, the image has no `sh`, or port 80 is taken inside it. Check the derived template's state and the worker pod logs. |
| `apt-get` or `curl` fail inside the task | No egress from the actors (prerequisites). |
| The verifier fails at its first command with a permission error | The image runs as a non-root user and has no `sudo`; see the provider README's limits. |
| A guest call hangs, then fails | The router rejected `ate-env-api`'s certificate: its SPIFFE ID is missing from the router's allow-list. |
