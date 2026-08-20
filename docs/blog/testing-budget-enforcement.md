# Three bugs I found writing tests for my own budget enforcement

I run Tollgate, a small proxy that sits between AI agents and the LLM APIs they call, and its whole job is to say how much an agent has spent and stop it before it spends too much. I was about to start the next feature when it occurred to me that I'd never checked whether the spend numbers it produces are correct. Not "does it compile" correct. Correct after a restart, and correct when two hundred requests land on it at once.

So instead of writing the feature, I went through GitHub issues in LiteLLM's tracker, a much larger project doing a similar job, looking for cases where someone's bill or budget enforcement had actually turned out wrong in production: #27704, #27639, #31441, #35766, #35906, #36083. I wrote a test for the failure mode each one described, against my own code, on the theory that a test failing for the first time is worth more than one that's been green since it was written.

## Bug 1: cancelled streams weren't metered at all

This came from testing what happens when a client disconnects mid-stream: an agent gets a partial answer, then its process dies or its context gets cancelled before the response finishes arriving. I expected an edge case in how much of the response gets billed. Instead I found a case where nothing got billed.

The proxy is built on Go's `httputil.ReverseProxy`, which streams the upstream response back as it arrives. The code that reads that response, pulls the token counts out, and hands them off to be recorded ran right after the handler returned:

```go
func (p *Proxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
    ...
    p.rp.ServeHTTP(w, r)   // streams the response back to the client
    // record usage, log the request
}
```

That looks like it should work, right up until the client hangs up before the response is done. `ReverseProxy` doesn't return normally then; it panics, with a specific value, `http.ErrAbortHandler`, that Go's own HTTP server recognizes and treats as a closed connection rather than a crash. The panic unwinds straight past the recording code, so it never runs.

A client could receive most of an expensive response, and the moment its connection closed before the last byte, that request left no trace anywhere: no database row, nothing counted against its budget. I doubt anyone was disconnecting on purpose to dodge a spend limit, but the hole was shaped exactly like that, and I only found it because I went looking.

The fix is a `defer`. Deferred calls in Go still run while a panic is unwinding, before it reaches the standard library's own per-request recovery, the part that quietly closes the connection instead of logging a crash. Moving the recording call into a deferred function means it runs whether the handler returned normally or blew up:

```go
func (p *Proxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
    ...
    defer p.finish(r, state, start)   // runs even if this panics
    p.rp.ServeHTTP(w, r)
}
```

I left the panic itself unrecovered. `net/http` already knows what to do with `ErrAbortHandler`; there wasn't a good reason to reimplement that.

(Test: [`cmd/tollgate/metering_correctness_test.go`](https://github.com/opslync/tollgate/blob/main/cmd/tollgate/metering_correctness_test.go).)

## Bug 2: about 29% of spend records silently dropped under load

This turned up while testing something else: whether a hard kill of the process, an out-of-memory kill rather than a graceful shutdown, could lose already-accepted spend. I fired two hundred concurrent requests at a fresh SQLite database and counted how many rows landed. Roughly 29% hadn't.

SQLite allows exactly one writer at a time; every other connection waits, and past `busy_timeout` the write fails outright with `SQLITE_BUSY`. Go's `database/sql` opens a pool of connections by default, the right call for almost any database and the wrong one for SQLite, since a pool of writers just means more connections fighting over the one lock that exists. Whichever inserts lost that fight came back with an error the calling code could only log. Nothing retried them, so that spend was gone with no record it had been attempted.

The fix was one line: `db.SetMaxOpenConns(1)`. A single connection means writes queue instead of colliding, so nothing hits `SQLITE_BUSY` at all. It also made those two hundred concurrent inserts run about 30x faster, since a pool all blocked on the same lock was doing a lot of expensive waiting that a queue skips.

This isn't specific to what I was building. Any Go service using SQLite under real concurrency has this failure mode by default, and it fails quietly: the error says the write failed, not that data is being lost.

(Tests: [`cmd/tollgate/durability_correctness_test.go`](https://github.com/opslync/tollgate/blob/main/cmd/tollgate/durability_correctness_test.go), [`internal/store/durability_correctness_test.go`](https://github.com/opslync/tollgate/blob/main/internal/store/durability_correctness_test.go).)

## Bug 3: a flagged $0 looked exactly like a real $0

Smaller, this one. A stored request could show a cost of $0 for three different reasons: genuinely free, a response that didn't parse the way the code expected so the real cost was unknown, or a model not yet in the pricing table. All three looked identical on disk, and a dashboard reading that data couldn't tell "nothing happened here" from "something happened and I don't know what it cost," which is worse than being wrong because it doesn't look like a problem at all.

The fix was a column, `usage_status`, recording which of the three applied, plus a count of flagged rows shown next to the total so a suspicious $0 doesn't read as a clean one.

(Test: [`pricing/pricing_correctness_test.go`](https://github.com/opslync/tollgate/blob/main/pricing/pricing_correctness_test.go).)

## The one I didn't fix

Budget enforcement checks whether a request is allowed before it goes upstream, and records the actual cost only once the response comes back, since the cost isn't knowable any earlier. An entire round trip to the LLM provider sits between those two points, and any request that checks during it sees the spend total from before the gap, not after, so enough concurrent requests arriving together all get waved through before any of them are recorded.

I measured the worst case: a one-dollar limit, a hundred concurrent requests at the boundary, every single one let through. $2.99 recorded against that dollar. At least the bound is real: overshoot can't exceed however many requests were in flight when the limit was crossed, and it's a single burst rather than a leak, since the next request after those finish sees the corrected total and gets blocked normally.

Fixing it properly means reserving an estimated cost the moment a request is allowed through, truing it up once the real cost is known, and releasing the reservation on every path that doesn't end in one, a disconnect, an upstream error, the process dying mid-request. That's a bigger, riskier change than anything else here, so I wrote the bound down instead and opened [issue #17](https://github.com/opslync/tollgate/issues/17) with a sketch of the real fix.

(Test: [`internal/budget/concurrency_correctness_test.go`](https://github.com/opslync/tollgate/blob/main/internal/budget/concurrency_correctness_test.go).)

## The mutation test

Worth mentioning, since it's a check that's easy to skip. I'd written a test asserting that a forged or expired credential gets rejected outright: no request forwarded, nothing recorded. Before trusting it, I went into the authentication code and deliberately broke the part meant to reject a failed check, so it let the request through instead. Five subtests failed immediately, and the output showed exactly what you'd want to see if this had happened for real: an unattributed, billed row in the database, from a request that should never have gone anywhere. I put the code back afterward.

If the test had still passed with the code broken, it wouldn't have been telling me anything.

## What's there now

The suite that came out of this covers five groups of invariants, durability, concurrency, metering accuracy, attribution, and identity, seventeen tests in total, running in their own CI job on every push. The full table, including the two bounds published above instead of quietly fixed, is at [correctness.md](../correctness.md) if you'd rather check this against the actual tests than take my word for it.

Tollgate is what all of this sits under: a proxy in front of an agent's LLM calls that attributes spend to whichever agent made it and enforces a budget on it in real time, which is the reason getting these numbers right mattered enough to spend a week on before building the next feature.
