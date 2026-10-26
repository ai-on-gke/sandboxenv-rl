## Why

<!-- The problem or the motivation. Link the issue. -->

## What

<!-- What changes, and what stays the same. -->

## Tests

<!-- What you ran and the results: unit tests, CI, a live run on a cluster. -->

## Security impact

Does this change add or widen any of these?

- [ ] Network surface: a listener, a route, a proxy, a new caller of a port
- [ ] Privileges: of `ate-env-api`, of the guest, or in the access policy or the manifest
- [ ] Wire or API fields: a protobuf field, an RPC, a header, a CLI flag that changes defaults
- [ ] Dependencies: a new module, package, base image or GitHub Action
- [ ] None of the above

If you checked one of the first four boxes, link a short design note with the threat it adds and
its mitigation, and update
[`docs/security/threat-model.md`](https://github.com/ai-on-gke/sandboxenv-rl/blob/main/docs/security/threat-model.md)
in this pull request.
