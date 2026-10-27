# Support

What this repository supports, and what that means.

## Supported

| Path | What it is |
|---|---|
| [`cmd/ate-env`](../cmd/ate-env) | The CLI. |
| [`cmd/ate-env-api`](../cmd/ate-env-api) | The API service. |
| [`cmd/ate-env-guest`](../cmd/ate-env-guest), [`guest/`](../guest) | The daemon inside each environment and its services. |
| [`clients/go`](../clients/go) | The Go client. |
| [`clients/python`](../clients/python/README.md) | The Python client (`ate_env`), including the fleet SDK (`SandboxFleet`, `AsyncSandboxFleet`). |
| [`integrations/`](../integrations) | Framework integrations: today [`integrations/nemo-gym`](../integrations/nemo-gym/README.md). |
| [`proto/ateenv/v1alpha`](../proto/ateenv/v1alpha) | The `ateenv.v1alpha` API. |

The packages under [`internal/`](../internal) that these binaries are built from are part of them:
`internal/apiservice`, `internal/ate`, `internal/mcp`, `internal/portproxy`, `internal/routertls` (the
router TLS client) and `internal/tool` (the MCP tool adapter). Go does not let other modules import
them, thus they are not an API of their own.

For these paths, supported means:

- Bug and security fixes land on `main`. There are no release branches.
- CI builds and tests them on each pull request.
- Dependabot proposes dependency updates for the Go module, the Python client and the GitHub
  Actions. The NeMo Gym integration pins nothing of its own; its only other dependency, `grpcio`,
  is also one of the Python client's.
- The code owners in [`.github/CODEOWNERS`](../.github/CODEOWNERS) review changes.

Supported is not a compatibility promise yet: the API and the clients are alpha and can change in
incompatible ways until v1.0.

## Maintained tooling, no compatibility promise

[`benchmarking/`](../benchmarking/swebench/README.md), [`cmd/benchmarking`](../cmd/benchmarking) and
[`internal/benchmarking`](../internal/benchmarking/DESIGN.md): the SWE-bench loader, its scripts and
the benchmark design. They are kept working with the rest of the repository and CI runs their
tests. Their flags can change at any time. A change to the fields of the events file or of the
report JSON is called out in the PR description; that is notice, not a compatibility promise.

## Build, test and documentation files

[`hack/`](../hack) (license and format checks), [`internal/internaltest`](../internal/internaltest)
(fakes of the Substrate control plane and router for tests), the [`Makefile`](../Makefile),
[`buf.gen.yaml`](../buf.gen.yaml), [`.ko.yaml`](../.ko.yaml), `go.mod`, [`.github/`](../.github)
(CI, Dependabot, code owners) and [`docs/`](.) are maintained with the code that they build, test or
describe. They are not an API: they change whenever that code needs it.

## Not supported: `examples/` and `internal/examples/`

[`examples/`](../examples) and [`internal/examples/`](../internal/examples) (a program that walks
through the `EnvironmentService` lifecycle) are provided as is. Examples get no support, no
compatibility promise and no security fixes, and they can break or be removed at any time. Do not
depend on them; copy what you need. The Go examples are packages of the root module, thus CI compiles and tests them with
the rest of the module; that is not a support commitment.
