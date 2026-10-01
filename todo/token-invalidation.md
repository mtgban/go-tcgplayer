# Recover from a token the server stops accepting

A token lives about 14 days (`expires_in` is 1,209,599 seconds), and
`RoundTrip` replaces it only when it is missing or within five minutes of
expiry. Two gaps follow. Neither has been observed; both come from reading
the code.

## A revoked token wedges the client

If TCGplayer stops accepting a token before its expiry, say after a key
rotation, every request answers 401. `retryablehttp`'s default policy does
not retry a 401, and nothing clears the cached token, so the client fails
every call until the token's own expiry, up to two weeks. The nightly
dumper starts fresh each run and recovers the next night; a long-lived
process holding a `Client` does not recover until restarted.

Proposal: on a 401, clear the cached token if it is still the one the
request carried (so concurrent failures do not each trigger a refresh), and
retry the request once with a fresh token. A second 401 is the answer.

## One cancelled caller fails every waiter

Concurrent refreshes collapse onto one `singleflight` call, and that call
runs on the context of whichever request arrived first. If that caller
cancels, everyone waiting on the flight gets its cancellation, wrapped in
`tokenError`, which the outer client is told not to retry.

Proposal: fetch the token on `context.WithoutCancel(ctx)` bounded by the
token client's own timeout, so the fetch belongs to no single caller.

## Care

Test both with the `httptest` server: a token endpoint handing out two
tokens and an API rejecting the first. Then break the fix and watch the test
go red, as every guard here must.
