# AGENTS.md

Guidance for AI coding agents working on **go-tcgplayer**, the Go client for
the TCGplayer catalog and pricing API and the home of `tcgdumper`, the
program that writes the nightly catalog dump every mtgban datastore is built
from. Read `SPECIFICATIONS.md` for the API surface, the dump format and the
invariants; read `README.md` for the human-facing overview.

## The one rule that matters

**Nothing the catalog counted may go missing from a dump, and a dump that
cannot prove it is whole must not be written.** Every other rule here is a
consequence.

The API does not hand you a category's products. It hands you pages of a
*filtered* view, and the filter is a list of product type names the caller
supplies. Ask for the wrong names and the API answers cheerfully with fewer
products and no error. That is the failure this repo exists to prevent, and
it has happened:

- The library shipped one product type list, Magic's. Yu-Gi-Oh files 22
  products under `Tin` and 13 under `YGO Start Decks`, names Magic does not
  use, so **every Yu-Gi-Oh dump was short by 35 products and reported
  success**.
- Worse and quieter: Dragon Ball Super, UniVersus, Final Fantasy and Star
  Wars Destiny name their singles after themselves (`Dragon Ball Super
  Singles`). Asking those categories for `Cards` returns **zero**. A dump
  would have been empty, and nothing would have said so.

Both were found by counting, not by reading. So:

- **Count before you fetch, and check after.** `tcgdumper` asks the API how
  many products the category holds *with no product type filter at all* —
  the only count that can see a product whose type nobody named — and
  refuses to write when the per-type totals do not add up to it. It also
  checks the groups and products it collected against the counts it opened
  with, because a page that answers short without erroring is a silent loss
  the failed-page tally never sees, and by identity, because a duplicate can
  stand in for the product it pushed out.
- **A guard that cannot fail is decoration.** Every guard in this repo has
  been mutation-tested: break the thing it protects and watch it go red. Do
  the same for any guard you add, and say in the PR what you broke.

## Layout

```
tcgplayer.go            the whole client: auth transport, the envelope, the
                        endpoint wrappers, the category and product type
                        tables, and the dump format types
tcgplayer_test.go       the client's tests: endpoints against httptest,
                        and the dump helpers
cmd/tcgdumper/          the dump program and its end-to-end tests
.revive.toml            the lint rules, shared with go-mtgban
.github/workflows/ci.yml            gofmt, vet, revive, staticcheck, build, test
.github/workflows/catalog-dump.yml  the nightly dump and its B2 upload
.github/dependabot.yml              weekly action and Go module updates
```

One file holds the library on purpose. It is ~1,100 lines, most of it two
tables (92 categories, 72 product type vocabularies) and the endpoint
wrappers, which are near-identical by design — each one counts its ids,
builds a URL, calls `Get`, and decodes `Results` into its own slice. Resist
collapsing them into a generic helper: the repetition is what lets a reader
see one endpoint's whole behaviour without following a call chain.

## Build, test, format, lint

```sh
gofmt -l .                                      # must print nothing
go vet ./...
go run github.com/mgechev/revive@v1.13.0 -set_exit_status -config .revive.toml ./...
go run honnef.co/go/tools/cmd/staticcheck@2026.2.1 ./...
go build ./...
go test -race ./...
```

That is exactly what `ci.yml` runs on every push and pull request. Run all
of it before committing; the lint rules are the set go-mtgban holds itself
to, so a habit picked up in one repo reads the same in the other.

## How the client works

Three things happen to every request, in `authTransport.RoundTrip`:

1. **The rate limiter.** 80 requests a second, burst 20. Set high
   deliberately — the ceiling is never reached in practice, because the
   dump's concurrency (8 workers) and per-page latency hold the real rate to
   single digits.
2. **The token.** Acquired on the first request that needs one and refreshed
   five minutes before expiry. Concurrent callers collapse onto one fetch
   through a `singleflight`; the token endpoint has its own retryable client
   so that the one request everything depends on is not the only one without
   retries. The fetch runs on `context.WithoutCancel`, so the caller that
   happened to start it cannot fail the others by giving up, and each caller
   still stops waiting when its own context ends.
3. **One retry on a rejected token.** A token lives about 14 days, and the
   server can stop accepting it sooner, as after a key rotation: the live API
   then answers 401 with "Missing or invalid bearer token." The transport
   drops that token, only if it is still the one held, fetches another and
   sends the request once more. A second 401 is the answer. Without this,
   every call fails until the old token would have expired.
4. **A clone of the request.** `RoundTripper` implementations must not
   modify the request they are given, so the `Authorization` header goes on
   a copy.

A token failure is wrapped in `tokenError`, and the outer client's
`CheckRetry` refuses to retry it. Without that, bad credentials cost five
token fetches and fifteen seconds of backoff before the caller hears about
it, because the outer client reads the failure as a generic transport error.

Both clients carry a per-attempt timeout (one minute for tokens, two for
everything else). They exist because a throttling server can leave a
connection open and silent, and before the timeouts a dump could hang
forever with every worker blocked and nothing in the log.

## Verifying a change

**Against a mock.** `newTestClient` points every endpoint URL at an
`httptest` server. Fixtures are written **as the json the API sends**, never
encoded from the types under test — encoding our own structs round-trips
through the same tags being tested, so a wrong `json:` tag would decode back
to the value it was written from and the test would pass. This is not
hypothetical: before the fixtures were wire-format, breaking
`json:"abbreviation"` to `json:"WRONG"` failed nothing.

**Against the live API.** Credentials come from `TCGPLAYER_PUBLIC_KEY` and
`TCGPLAYER_PRIVATE_KEY`, or `-pub`/`-pri`. A live run is the only way to
settle a question about what the platform actually holds, and several
questions can only be settled that way — no offline test knows which product
type names a category uses.

```sh
go build ./cmd/tcgdumper
./tcgdumper -category 2 > yugioh.json      # exits non-zero if short
```

**Never measure on a stale catalog.** The catalog changes nightly, and a
count taken last week is confidently wrong. Two categories were left out of
the product type table because they served nothing the day it was read;
weeks later they held 264 and 422 products.

## Conventions

- **Doc comments on every exported name**, opening with the name. `revive`'s
  `exported` and `package-comments` rules enforce it.
- **Initialisms are initialisms**: `ID`, `URL`, `API`, `SKU`, `DB`. Not
  `Id`, `Url`, `Sku`.
- **No package-name stutter.** The import path already says `tcgplayer`, so
  the endpoint is `tcgplayer.CatalogProductsURL`, never
  `tcgplayer.TCGAPICatalogProductsURL`.
- **Comments carry their evidence.** A rule without the number it was
  measured against gets deleted by the next reader who cannot see what it
  protects. "8k+ Magic numbers are not numeric" is why the collector number
  is a string.
- **Tests name the call and print got before want**, and compare whole
  structures rather than picking fields, so a failure says what it should
  have been.

## Gotchas

- **Product types are named per game.** There is no platform-wide
  vocabulary. `ProductTypes(category)` answers for a category; the tables
  behind it were read off the platform and each one accounts for its
  category's whole product count. 20 categories have no entry, and
  `ProductTypes` falls back to all 36 known names for them — which is not
  safe, merely loud: the dumper's count check reports the shortfall rather
  than passing off a partial dump.
- **A category constant is not self-evidently right.** `CategoryCardfightVanguard`
  and `CategoryChronoClashSystem` named each other's id for a long time; id
  16 holds 272 Vanguard sets and id 60 holds five Chrono Clash games. Verify
  a constant against the platform, not by reading the list.
- **Product ids are allocated before release.** An id below the highest one
  a dump contains is *not* proof the product existed when the dump ran — 99
  Magic products looked like misses on that reasoning and every one turned
  out to carry a `releasedOn` after the dump. Check `presaleInfo.releasedOn`
  and `modifiedOn`, not id order. `presaleInfo` is on the API's product
  record and on tcgcsv.com; `Product` does not decode it, so the dump does
  not carry it.
- **An empty result set arrives as a 404.** `queryTotal` reads a not-found
  with an empty envelope as zero, because for a count that is the answer and
  not a failure.
- **`tcgcsv.com` needs a descriptive User-Agent.** Its group listings are
  open but the product files answer 401 to a default Python or curl agent.
  It is the only independent check on the catalog available without
  credentials, and it carries no product type field, which is why the type
  vocabulary had to be read from the search facets instead.
- **staticcheck can only read the Go releases it was built to read.** Run
  under a newer Go than its release supports, it fails on every
  standard-library import with "export data version N is greater than
  maximum supported version". `2026.2.1` reads 1.26 and 1.27. CI follows
  `go.mod` — `setup-go` reads `go-version-file` and runs its `toolchain`
  line, go1.26.8 — so when a local run fails that way and nothing else does,
  the pin needs a newer release; the code is fine.
- **`gh run list --commit <short-sha>` silently returns nothing.** Use the
  full sha or query the workflow. A poll built on the short form waits
  forever for a run that already finished.

## Git

Branch off `main`, never off another branch with an open PR. Run the whole
gate before committing. Open a pull request against `main`; **do not push to
`main` without being asked for that push.** Commit messages say what changed
and why it was wrong before, in prose, without bullet lists.

## The dump, and who reads it

`catalog-dump.yml` runs at 05:00 UTC and writes one file per category to
`b2://mtgban-datastore/<name>/tcgplayer-catalog.json.xz`. Categories run one
at a time (`max-parallel: 1`) to keep the request rate down, and
`fail-fast: false` so one category's failure does not cancel the rest. A
failed dump uploads nothing, which leaves the previous good file in place.

`datastore-gen` builds every game datastore from these files, `go-mtgban`
prices against the same catalog, and `mtgban-website` reads the dump for
TCGplayer's own product names. A change to `CatalogDump` is a change to all
three — see `SPECIFICATIONS.md` for the contract.
