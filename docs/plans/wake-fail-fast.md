# Wake fail-fast: acceptance criteria

Status: met on `main` with #222 (fail fast on crash and no capacity) and the wake budgets of #202.
Each criterion is checked on every pull request by the property test `TestAwaitWakeProperties` in
[internal/apiservice/wakeretry_property_test.go](../../internal/apiservice/wakeretry_property_test.go).

## What it covers

A guest call to an environment that is suspended or still starting waits in `ensureAwake`
([internal/apiservice/wakeretry.go](../../internal/apiservice/wakeretry.go)). The loop probes the
guest through the router, reads the actor's state from the control plane when the probe answers
Unavailable, and backs off. Three budgets bound it: the wake budget (`--wake-budget`, default 5
min), the running budget (`--wake-running-budget`, default 20 s, counted from the first attempt that
sees the actor RUNNING), and the opt-in no-capacity grace (`--wake-no-capacity-grace`, default 0).
The loop body is `awaitWake`, which takes the probe and the state read as functions so the test can
script them.

## Criteria

| ID   | Criterion                                                                                                                                                                                                      |
| ---- | -------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| WF-1 | A probe the guest answers (anything but Unavailable) ends the wait at once, with no error.                                                                                                                     |
| WF-2 | An unreachable guest whose actor is CRASHED ends the wait at once with FailedPrecondition, naming the control plane's crash reason when there is one; DELETING ends it with NotFound. No later probe is sent.  |
| WF-3 | ResourceExhausted only with a positive no-capacity grace, after a run of "no worker has room" answers (actor not RUNNING) that lasted the grace, from the first probe's start to the last probe's answer.      |
| WF-4 | Once such a run has lasted the grace at a probe's answer, that probe is the last one, and the probe that ends the run starts no later than the grace (plus the 1 ms shortest backoff): the wait never runs on. |
| WF-5 | A timeout (Unavailable) only after the wake budget, or after the running budget counted from the first attempt of the current run of RUNNING answers.                                                          |
| WF-6 | The wait never runs past the wake budget by more than one probe.                                                                                                                                               |

## How the test checks them

`rapid` (`pgregory.net/rapid`) draws the budgets, an optional grace, a crash reason and a timeline
of up to 60 probes: each probe's latency (up to 3 s), the router's answer (the guest answers, "no
worker has room", or another Unavailable) and the state the control plane reports after it. Each
timeline runs once in its own `testing/synctest` bubble, so `time.Now`, `time.Sleep` and
`time.After` use a fake clock: a run that spans minutes takes microseconds and always takes the same
path. The test records every probe's start, answer and state, and checks WF-1 to WF-6 on the record
after the run. It tries 2,000 timelines per run (`-rapid.checks` overrides it) in about 0.2 s, and
shrinks a failing one to its shortest form.

The test was checked against seven deliberate breaks in `awaitWake`, three runs each; every run
caught every break:

| Break                                                       | Caught by |
| ----------------------------------------------------------- | --------- |
| No no-capacity grace check                                  | WF-4      |
| Grace measured from the probe's answer instead of its start | WF-4      |
| Backoff that sleeps past the grace                          | WF-4      |
| No fail-fast on CRASHED                                     | WF-2      |
| No fail-fast on DELETING                                    | WF-1/WF-2 |
| Running budget not restarted after a non-RUNNING answer     | WF-5      |
| Overall wake budget ignored while RUNNING                   | WF-6      |

## Limits

- The test drives `awaitWake` with scripted answers; it does not run the gRPC transport, the
  router's TLS check (`routertls.RejectedCertificate`, covered by
  `TestGuestCallFailsFastOnRejectedCertificate`) or the start-failure trailer, which needs a server
  transport (covered by `internal/apiservice/startfailure_test.go`).
- A control-plane read that fails ends the wait with that error; the timelines never fail the read.
- `go test -race` checks for data races in the same run; this test checks the wait's logic, which
  `-race` cannot: every access in `awaitWake` is in one goroutine.
