# Spend accounting: what we guarantee, and what we don't

Tollgate's whole job is to tell you what your agents cost. That number is only
worth having if it survives restarts, concurrency, cancelled requests, and
upstreams that answer with something unexpected.

This page lists every invariant we test, whether it holds, and the exact bound
where it doesn't. Each row links to the test that proves it, and every one of
those tests runs in CI on every push (the `correctness` job:
`go test -race -run Correctness ./...`).

Where an invariant does not hold, the limitation is stated here rather than
softened in the test. A bound you can plan around is more useful than a
guarantee that quietly isn't one.

## Invariants

### A — durability

| ID | Invariant | Holds | Bound if not | Test |
|---|---|---|---|---|
| A1 | Spend recorded before a restart is still counted after it. | Yes | — | `TestCorrectness_RestartPreservesSpend` (`internal/budget/durability_correctness_test.go`) |
| A2 | A killed agent stays killed across a restart, from the first request — not the first re-sync. | Yes | — | `TestCorrectness_RestartPreservesKill` (`internal/budget/durability_correctness_test.go`) |
| A3 | `spend_events_accepted == spend_events_persisted`. | Almost | One record per in-flight request may be lost to a hard kill (SIGKILL/OOM/node loss). See [The hard-kill window](#the-hard-kill-window). | `TestCorrectness_HardKillPersistsAcceptedSpend` (`cmd/tollgate/durability_correctness_test.go`), `TestCorrectness_ConcurrentInsertsAllPersist` (`internal/store/durability_correctness_test.go`) |
| A4 | A rolling window's elapsed portion is preserved across a restart, and still ages out on schedule. | Yes | — | `TestCorrectness_MidWindowRestartPreservesWindow` (`internal/budget/durability_correctness_test.go`) |

### B — concurrency

| ID | Invariant | Holds | Bound if not | Test |
|---|---|---|---|---|
| B1 | The sum of concurrently recorded spend equals the sequential sum. | Yes | — | `TestCorrectness_ConcurrentRecordLosesNoSpend` (`internal/budget/concurrency_correctness_test.go`) |
| B2 | No more than one request is allowed past a hard limit. | **No** | Overshoot is bounded by the number of requests in flight when the limit is crossed. See [Budget overshoot](#budget-overshoot). | `TestCorrectness_ConcurrentCheckAtBoundary` (`internal/budget/concurrency_correctness_test.go`) |
| B3 | The periodic SQLite re-sync is idempotent against live increments. | Yes, with one transient exception | A re-sync landing between a request's SQLite insert and its in-memory increment counts that request twice, for at most one refresh interval (5s). Self-correcting, and fail-closed. | `TestCorrectness_ResyncDoesNotDoubleCount` (`internal/budget/concurrency_correctness_test.go`) |

### C — metering accuracy

| ID | Invariant | Holds | Bound if not | Test |
|---|---|---|---|---|
| C1 | A client disconnect mid-stream still records the usage seen so far — never a partial-but-wrong total. | Yes | Output tokens generated after the client hangs up are billed by the provider and not seen by Tollgate. See [Cancelled streams](#cancelled-streams). | `TestCorrectness_CancelledStreamAttributesTokensSeen` (`cmd/tollgate/metering_correctness_test.go`), `TestCorrectness_TruncatedStreamReportsTokensSeen` (`internal/meter/metering_correctness_test.go`) |
| C2 | Cached input is priced at the cache rate, not the input rate. | Yes | — | `TestCorrectness_CachedTokensPriceAtCacheRate` (`internal/meter/metering_correctness_test.go`) |
| C3 | An unparseable usage block is recorded as an error, not as $0. | Yes | — | `TestCorrectness_UnpricedRequestIsFlaggedNotZeroed`, `TestCorrectness_UnparseableUsageIsLoggedLoudly` (`cmd/tollgate/metering_correctness_test.go`), `TestCorrectness_UnparseableUsageIsNotZero` (`internal/meter/metering_correctness_test.go`) |
| C4 | A model missing from `pricing.yaml` is flagged, not zero-costed. | Yes | Its token counts are still recorded and trustworthy; only the dollar figure is unknown. | `TestCorrectness_UnknownModelIsNotPricedAtZero` (`pricing/pricing_correctness_test.go`), `TestCorrectness_UnpricedRequestIsFlaggedNotZeroed` (`cmd/tollgate/metering_correctness_test.go`) |

### D — attribution integrity

| ID | Invariant | Holds | Bound if not | Test |
|---|---|---|---|---|
| D1 | Agent identity comes from the resolved key alone; no client-supplied field can alter attribution. | Yes | — | `TestCorrectness_ClientCannotOverrideAttribution` (`cmd/tollgate/attribution_correctness_test.go`) |
| D2 | Cost is computed server-side from parsed response usage and the embedded pricing table. | Yes | — | `TestCorrectness_ClientCannotLowerRecordedCost` (`cmd/tollgate/attribution_correctness_test.go`) |
| D3 | Concurrent requests from different agents never bleed into each other's totals. | Yes | — | `TestCorrectness_ConcurrentAgentsAttributeIndependently` (`cmd/tollgate/attribution_correctness_test.go`) |

### E — identity integrity (Kubernetes ServiceAccount authentication)

Group D covers static keys: a key is a string, and a string can be copied.
Tollgate's actual claim is stronger — in Kubernetes mode, an agent's identity is
the ServiceAccount token the kubelet projected into its pod, so attribution is
bound to the workload rather than to a secret anyone holding it can present.
That claim is only worth making if a pod cannot wear another pod's identity, and
it was the one property the first pass of this suite did not test.

Where the trust sits: Tollgate validates no signatures itself. It asks the API
server via TokenReview, and treats `authenticated: false` as final. Pod name and
UID come only from the `authentication.kubernetes.io/pod-*` extras the API
server sets for a genuinely bound token — nothing client-supplied reaches an
identity. These tests exercise that whole chain (`Authenticator` → `PodCache` →
`Resolver` → `auth` middleware → proxy → SQLite) against a fake API server, not
a stub in place of the resolver.

| ID | Invariant | Holds | Bound if not | Test |
|---|---|---|---|---|
| E1 | A token that does not authenticate — forged, expired, or an authenticated non-ServiceAccount principal — never produces an identity, never reaches the upstream, and is never recorded or billed. | Yes | — | `TestCorrectness_UnauthenticatedTokenFailsClosed` (`cmd/tollgate/identity_correctness_test.go`) |
| E2 | A pod authenticated by its own ServiceAccount token is never attributed as a different pod, ServiceAccount, or workload — including under concurrent, interleaved use of several pods' tokens. | Yes | — | `TestCorrectness_TokenSubstitutionCannotCrossAttribute` (`cmd/tollgate/identity_correctness_test.go`) |
| E3 | No client-supplied header can inject or override pod, ServiceAccount, namespace, or workload attribution when identity comes from a ServiceAccount token. | Yes | — | `TestCorrectness_HeadersCannotInjectWorkloadIdentity` (`cmd/tollgate/identity_correctness_test.go`) |

All three hold. One deliberate behaviour is worth knowing about even though it
is not a hole in them: see [Identity without a pod](#identity-without-a-pod).

## The limitations, in detail

### Budget overshoot

**A hard limit can be exceeded by the requests already in flight when it is
crossed.**

Tollgate checks a budget before forwarding a request and records its cost after
the response comes back, because a request's cost is not knowable until it
finishes. Between those two moments sits an entire upstream round trip. Every
request that checks during that gap sees the pre-spend counter and is allowed.

With 100 requests in flight at the boundary and a $1.00 limit, the test measures
a worst case of $2.99 recorded — all 100 allowed. That is the bound: **overshoot
never exceeds the number of concurrent in-flight requests, and it is a one-shot
burst, not a leak.** Once those requests are recorded, the next one is blocked.
A runaway agent looping serially is stopped on its very next request, which is
the case the kill switch and budget enforcement exist for.

Closing this properly means reserve-then-commit: debit an estimated cost at
check time, true it up at record time, and release the reservation on every
failure path (client disconnect, upstream 5xx, process death mid-flight). That
is a proxy-wide semantic change with its own new failure modes, so for now the
bound is published rather than papered over.

If you need a hard ceiling rather than a soft one, cap concurrency at the agent
and size the budget to absorb one burst. Closing the gap properly — rather than
mitigating around it — is tracked in
[opslync/tollgate#17](https://github.com/opslync/tollgate/issues/17), with a
reserve-then-commit design sketch.

### The hard-kill window

**A SIGKILL can lose the spend of requests that were in flight.**

There is no queue or background flusher between metering and storage: the
recorder calls SQLite synchronously on the request goroutine, so there is
nothing to drain and nothing to lose in a batch. But the recorder runs *after*
the response has finished streaming to the client. Between "the agent holds a
complete response" and "the row is in SQLite" there is a real, narrow window,
and a process killed inside it loses those records permanently.

The bound is one record per request in flight, and never a record for a request
the client did not receive. A graceful shutdown (SIGTERM, which is what
Kubernetes sends on a normal rollout) does not hit this window.

### Cancelled streams

**Tollgate bills what the upstream declared, not what it might have generated
afterwards.**

An Anthropic stream declares input and cache tokens up front in `message_start`
and output tokens at the end in `message_delta`. When a client hangs up in
between, Tollgate records a row with the real input tokens and zero output
tokens. If the provider kept generating after the disconnect, it will bill for
output tokens that Tollgate never saw.

This is a deliberate choice: the alternative is either discarding the request
entirely (which undercounts strictly more, and lets an agent evade its budget by
hanging up mid-stream) or guessing at the output tokens (which invents money you
cannot reconcile against a provider invoice).

### Identity without a pod

**An authenticated ServiceAccount token with no pod binding is attributed at
ServiceAccount level, not rejected.**

Two cases produce an identity with no workload attached: a token that is not a
bound projected token (so the API server reports no pod extras), and a bound
token whose pod has not yet appeared in the pod cache — the cache is a periodic
list, so a pod can send its first request before the next poll. In both, the
request is attributed to `<namespace>/<serviceaccount>` with the pod, workload
kind and workload name left empty.

This is enrichment failing, not authentication failing, and the two are kept
separate on purpose: the caller is still a principal the API server vouched for,
so refusing it would drop real spend on the floor rather than record it slightly
coarser. The security property is unaffected — the namespace and ServiceAccount
still come from TokenReview, and the empty fields stay empty rather than being
filled in from anything the client sent. What it costs is resolution: spend from
a cold-cache pod lands on the ServiceAccount's line rather than its Deployment's.

### Telling a flagged $0 apart from a real $0

Every stored request carries a `usage_status`:

| Value | Meaning | Is `cost_usd` trustworthy? |
|---|---|---|
| `ok` | Usage parsed, model priced. | Yes |
| `usage_unparsed` | A 2xx response that should have carried usage didn't — truncated JSON, a stream that ended early, a missing `usage` field. | No — cost is unknown, not zero |
| `model_unpriced` | Usage parsed, but the model has no entry in `pricing.yaml`. | No — token counts are fine, the price isn't |
| `not_metered` | No usage was ever expected: a non-2xx response, or a content type that carries none. | Yes — $0 is correct |

`GET /usage` reports `unpriced_requests` per group. **When that count is
non-zero, the `cost_usd` next to it is a floor, not a total.** Both flagged
cases also emit a warning log naming the agent.

## Why these tests

Each one corresponds to a publicly documented failure in a competing system —
LiteLLM issues #27704, #27639, #31441, #35766, #35906, #36083, and the reviewer
warnings on PRs #35854/#35886/#35887. These are failure modes that have cost
someone real money in production, not hypothetical edge cases.

Group E is the exception, and came from a review of this page rather than from
someone else's outage: everything above tests the numbers, and nothing tested
the identity those numbers hang off. A spend figure attributed to the wrong
workload is wrong no matter how carefully it was metered.

Three bugs were found and fixed while writing this suite:

1. **Spend lost to SQLite write contention.** Under 200 concurrent requests,
   ~29% of usage records failed to insert with `SQLITE_BUSY` — the connection
   pool opened multiple writers that fought over SQLite's single write lock
   until `busy_timeout` expired. The recorder could only log the failure, so
   that spend was gone. Fixed by serialising on one connection
   (`internal/store`), which also made 200 concurrent inserts ~30x faster.
2. **Cancelled streams recorded nothing at all.** `httputil.ReverseProxy` panics
   with `http.ErrAbortHandler` when a client disconnects mid-response, which
   unwound straight past the metering code. Every cancelled stream was
   unattributed and uncounted — a budget bypass for any agent willing to hang
   up. Fixed by metering in a `defer` (`internal/proxy`).
3. **Unpriced requests were indistinguishable from free ones.** A $0 row could
   mean "this request was free", "we couldn't read the usage", or "we don't know
   this model's price", with nothing on disk to tell them apart. Fixed with the
   `usage_status` column above.

## Running them yourself

```sh
go test -race -run Correctness ./...
```
