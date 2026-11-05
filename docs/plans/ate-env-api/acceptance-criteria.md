# ate-env-api: acceptance criteria for derived image templates

Status: AE-1 and AE-2 met by #244, which fixed the two bugs that property tests found against them;
AE-3 to AE-5 met on `main` before it. Each criterion names the tests that check it on every pull
request (the Go job's `go test -race ./...`).

## Scope

`CreateEnvironment` with a `rootfs_image` pinned by digest runs the environment on a template
derived from a base template: the same template, with the image as its container's root filesystem
and the guest mounted from the base's image at `/ate`
([internal/apiservice/imagetemplate.go](../../../internal/apiservice/imagetemplate.go),
`getOrCreateImageTemplate` in
[internal/apiservice/server.go](../../../internal/apiservice/server.go)). The derived template is
named `<base>-<first 12 hex digits of the digest>` and is created on first use; a template whose
golden snapshot failed is recreated when no actor a recreate would harm uses it.
[docs/architecture.md](../../architecture.md) describes the flow, and the threat model lists the
image check as the mitigation for "a caller runs an image of its choice".

## Criteria

| ID   | Criterion                                                                                                                                                                                                                                            | Status        | Tests                                                                                                                             |
| ---- | ---------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- | ------------- | --------------------------------------------------------------------------------------------------------------------------------- |
| AE-1 | A `CreateEnvironment` that succeeds runs the image it asked for. A derived template that runs another image is refused with `FailedPrecondition`, whether it existed before the call or a concurrent create made it.                                 | met by #244   | `TestPropConcurrentCreatesUseRequestedImage`, `TestConcurrentCreateOnCollidingDigestFailsPrecondition`, `TestPropSameImageDigest` |
| AE-2 | A derived template's guest command resolves under `/ate` for every base command `rerootCommand` accepts, so the guest is found at boot.                                                                                                              | met by #244   | `TestPropRerootStaysUnderMount`, `TestRerootKeepsDotDotUnderMount`                                                                |
| AE-3 | The derived name is `<base>-<first 12 hex digits>`: the same for any repository path or tag of one digest, always a valid Kubernetes name of at most 63 characters, and never a panic or an invalid name for any input.                              | met on `main` | `TestPropImageTemplateNameIdentity`, `TestPropImageTemplateNameNeverInvalid`                                                      |
| AE-4 | Deriving from a derived template equals deriving from its base: `Derive(Derive(b, n1, i1), n2, i2) == Derive(b, n2, i2)`; the base is never changed and volume names stay unique.                                                                    | met on `main` | `TestPropDeriveComposes`                                                                                                          |
| AE-5 | Concurrent creates on a derived template whose golden snapshot failed all succeed; the template is deleted and recreated at most once, only when no actor a recreate harms uses it, no actor is created while it is absent, and exactly one remains. | met on `main` | `TestPropConcurrentRecreate`                                                                                                      |

## What the property tests found

- **AE-1.** `getOrCreateImageTemplate` checked the image of a template that already existed, but
  when its own create lost a race (`AlreadyExists`) it returned the winner's template unchecked. Two
  digests that share their first 12 hex digits map to one derived name, so the losing caller's
  environment ran the winner's image. Found by `TestPropConcurrentCreatesUseRequestedImage`, which
  draws digests that share a prefix together with RPC delays; it needs that input and that ordering
  at once, which no example test had.
- **AE-2.** `rerootCommand` used `path.Join("/ate", cmd)`, which resolves a leading `..`:
  `/../ko-app/ate-env-guest` became `/ko-app/ate-env-guest`, outside the guest mount. Found by
  `TestPropRerootStaysUnderMount`, which draws paths with `..`, `.` and empty segments; the example
  tests used clean paths only.

Neither is a timing bug, so `testing/synctest` alone would not have found them: it makes scheduling
deterministic but generates no inputs and checks nothing.
