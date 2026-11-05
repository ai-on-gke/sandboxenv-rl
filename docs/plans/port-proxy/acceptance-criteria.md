# Port proxy: acceptance criteria for forwarding

Status: PP-1 met by #245, which fixed the bug a property test found against it. The criterion names
the tests that check it on every pull request (the Go job's `go test -race ./...`).

## Scope

ate-env-api's port proxy
([internal/portproxy/portproxy.go](../../../internal/portproxy/portproxy.go)) forwards an HTTP or
WebSocket request to a port a daemon listens on inside an environment. A caller addresses the
environment and port in one of two forms:

- **Header form:** `X-Env-Id`, `X-Env-Port` (and `X-Env-Atespace`) headers; the request's own path
  is forwarded.
- **Path form:** `/v1alpha/envs/<id>/ports/<port>/<rest>` (or
  `/v1alpha/atespaces/<atespace>/envs/<id>/ports/<port>/<rest>`), for browsers and WebSocket clients
  that cannot set headers; `/<rest>` is forwarded.

`Dispatch` matches the path form itself rather than on a `ServeMux` "so the daemon gets the path
exactly as sent": a mux would redirect a missing trailing slash, `//` and `..`, and WebSocket
clients do not follow redirects.

## Criteria

| ID   | Criterion                                                                                                                                                                                                                                 | Status      | Tests                                                                                                       |
| ---- | ----------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- | ----------- | ----------------------------------------------------------------------------------------------------------- |
| PP-1 | The daemon gets the forwarded path exactly as the caller sent it, byte for byte, in both forms: the path form's `/<rest>` after the routing prefix, and the header form's path. No character is escaped, unescaped or cleaned on the way. | met by #245 | `TestPathFormDeliversPathAsSentProperty`, `TestPathFormKeepsSubDelimsAsSent`, `TestPathFormKeepsPathAsSent` |

## What the property test found

- **PP-1.** When the path form's rest held no percent-escape, `parsePath` dropped the raw path (it
  equalled the decoded one), and Go's `url.EscapedPath` re-escaped the characters it escapes in a
  path though they are valid as sent: `/!` reached the daemon as `/%21` and `/files/report(1).txt`
  as `/files/report%281%29.txt`. The header form delivered them unchanged. Found by
  `TestPathFormDeliversPathAsSentProperty`, which generates paths from unreserved characters,
  sub-delims, `:` `@` and percent-escapes and sends each through both forms; the example tests used
  unreserved characters and escapes only, and the bug needs a sub-delim with no escape anywhere in
  the rest.

It is not a timing bug, so `testing/synctest` alone would not have found it: synctest makes
scheduling deterministic but generates no inputs and checks nothing.
