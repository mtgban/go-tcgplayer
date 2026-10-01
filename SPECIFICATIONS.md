# go-tcgplayer — Specification

What this package promises: the shape of every call it wraps, the tables it
carries, the format of the catalog dump it writes, and the invariants that
hold across all of them. `AGENTS.md` covers how to work on it.

Version pinned in the URLs: TCGplayer API **v1.39.0**.

---

## 1. What this is

Two things in one module:

- **A client** (`tcgplayer.go`) for the TCGplayer catalog and pricing
  endpoints, handling OAuth2 client-credentials, token refresh, rate
  limiting and retries.
- **A dump program** (`cmd/tcgdumper`) that walks one category exhaustively
  and writes it as a single JSON document, which every mtgban datastore is
  then built from.

The client is deliberately thin. It does not model the catalog; it returns
what the API returns, with the ids and names the API uses. The one place it
adds knowledge is the product type vocabulary (§5), because the API offers
no way to ask for it.

---

## 2. Authentication and transport

### 2.1 The token

`NewClient(publicKey, privateKey)` validates that both keys are non-empty
and makes **no request**. The first call that needs a token acquires one
from `TokenURL` with `grant_type=client_credentials`.

The response is the documented shape:

```json
{"access_token":"…","token_type":"bearer","expires_in":1209599,
 "userName":"…",".issued":"…",".expires":"…"}
```

Only `access_token` and `expires_in` are read. **`expires_in` is a count of
seconds** (1,209,599 ≈ 14 days) and is decoded as an `int64` and multiplied
by `time.Second` explicitly. Decoding it straight into a `time.Duration`
would be numerically right only by accident of the underlying type, and
reading it as nanoseconds expires the token instantly.

Expiry is measured from *receive* time, which slightly overestimates the
real window by the request's latency. The five-minute refresh buffer in
`RoundTrip` absorbs that.

### 2.2 The transport

Every request passes through `authTransport.RoundTrip`, which:

1. waits on a `rate.Limiter` of **80 requests/second, burst 20**;
2. reads the cached token under an `RWMutex`, and refreshes when it is
   empty or within **five minutes** of expiry;
3. clones the request and sets `Authorization: Bearer <token>` on the copy,
   because a `RoundTripper` must not modify the request it is handed;
4. on a **401**, replaces the token it sent, when that is still the token
   held, and sends the request once more, rebuilding a body through
   `GetBody`. A request whose body cannot be rebuilt gets the 401 back. A
   second 401 is returned as the answer.

Concurrent refreshes collapse onto a single fetch via
`golang.org/x/sync/singleflight`, keyed `oauth_token`. The fetch re-checks the
held token first, so requests rejected together, or one arriving just after
the replacement landed, cost one fetch between them. It runs on
`context.WithoutCancel` and each caller waits on `DoChan`, so a caller that
gives up returns at once without failing the fetch the others are waiting
on. The token request uses its own `retryablehttp.Client` so it is retried
like any other call.

### 2.3 Failure handling

| Concern | Behaviour |
| --- | --- |
| Per-attempt timeout | 1 minute for the token client, 2 minutes for the API client |
| Transient failures | `retryablehttp`'s default policy |
| Token failures | wrapped in `tokenError`; the API client's `CheckRetry` refuses to retry them, so bad credentials fail in one fetch rather than five |
| A token the server rejects (401) | replaced and the request sent once more (§2.2); a second 401 is an `*APIError` |
| Non-2xx with an error envelope | `*APIError{StatusCode, Messages}` |
| Non-2xx without one | `fmt.Errorf("http %d: %s", …)` |
| A body that is not an envelope | the http status, when the request failed; otherwise the decode error wrapped with `%w` |

The timeouts exist because a throttling server can hold a connection open
and silent. Without them a dump hangs with every worker blocked.

---

## 3. The response envelope

Every endpoint wraps its payload:

```go
type BaseResponse struct {
	TotalItems int             `json:"totalItems"`
	Success    bool            `json:"success"`
	Errors     []string        `json:"errors"`
	Results    json.RawMessage `json:"results"`
}
```

`Client.Get(ctx, link)` performs an authenticated GET and returns the
envelope with `Results` left raw for the caller to decode. It returns an
error on **any** non-2xx — an earlier version returned a failed response as
success whenever the envelope's `errors` array was empty, which callers read
as an empty result set.

### 3.1 Single-call listings

Four category listings and `ListProductSKUs` answer in one call. They send
no `totalItems` and ignore `limit` and `offset`, returning the whole list
whatever is asked: Yu-Gi-Oh's 39 rarities come back whole with `limit=2`,
with `offset=2&limit=2` and with `limit=500`, and the same holds across
Magic, Pokémon, One Piece, Flesh and Blood and Lorcana (checked
2026-10-01). With no count in the response there is nothing to check these
against, and 39 is the longest list seen.

`ListAllProducts` and `ListAllCategoryGroups` are different: they page, and
their `TotalItems` is the total across all pages.

---

## 4. Categories

A category is a game or product line: Magic is 1, Yu-Gi-Oh 2, Lorcana 71.
The constants are one `iota` block, so position *is* the id:

```go
const (
	CategoryMagic = iota + 1              // 1
	CategoryYuGiOh                        // 2
	…
	CategoryCyberpunk                     // 92
	categoryCount                         // 93, one past the last
)
```

**92 categories**, of which one slot (21, "My Little Pony") is blank: the
platform lists it but serves no groups under it.

`categoryCount` is unexported and exists so a test can walk every category
without a list that goes stale as categories are added.

### 4.1 The alignment invariant

Every constant must name the category the platform gives that id. This is
not self-evident and has been wrong: `CategoryCardfightVanguard` and
`CategoryChronoClashSystem` named each other's id, so a caller asking for
Vanguard's 272 groups received Chrono Clash's five. Verify against
`catalog/categories`, never by reading the list.

---

## 5. Product types

### 5.1 Why the table exists

`catalog/products` filters by `productTypes`, a comma-separated list of
names. The API publishes **no endpoint listing them**, and the names are
**per game, not per platform**. A caller must therefore know them, and a
caller that guesses loses products silently.

`AllProductTypes` holds all **36** names in use across the platform,
including some that differ only subtly — `Sealed Product` (six categories)
is not `Sealed Products` (46).

### 5.2 The per-category vocabulary

```go
var ProductTypesByCategory = map[int][]string{…}   // 72 entries
func ProductTypes(category int) []string
```

Each of the 72 entries was read off the platform and **accounts for its
category's entire product count**: the per-type totals sum exactly to the
count taken with no filter. The remaining 20 categories are supplies,
miniatures and Warhammer lines whose names the catalog and the search facets
both decline to expose, plus those serving no products at all.
`ProductTypes` falls back to all 36 names for them, which is loud rather
than safe — a caller counting its results finds the shortfall.

A test requires every category to be either in the map or listed in
`categoriesWithoutProductTypes` with a reason, so naming a category is a
decision rather than an omission.

### 5.3 Singles and sealed

```go
func SinglesProductTypes(category int) []string
func SealedProductTypes(category int) []string
```

A type is singles when it is `Cards` or ends in ` Singles`. **No category
uses more than one**, so the two functions partition what `ProductTypes`
returns. The suffix form is not cosmetic: Dragon Ball Super, UniVersus,
Final Fantasy and Star Wars Destiny name their singles after themselves, and
asking those categories for `Cards` returns zero.

A category with nothing of one kind gets an **empty, non-nil** list: 19
mapped categories sell no singles, and Epic sells nothing else. Empty and
nil mean different things to the product endpoints (§6.1), so the
difference is the contract, not a detail.

`ProductTypesSingles` and `ProductTypesSealed` are the older, category-less
pair, and are deprecated: they name Magic's types only. Use the functions.

---

## 6. The endpoints

Three access shapes, with different rules:

| Shape | Endpoints | Rule |
| --- | --- | --- |
| **Paged** | `ListAllProducts`, `ListAllCategoryGroups` | `offset` + `limit`, `limit` = `MaxItemsInResponse` (**100**). Pair with the matching `Total*` call to walk the whole set. |
| **Batched by id** | `GetProductsDetails`, `GetCategoriesDetails`, `GetMarketPricesByProducts`, `GetMarketPricesBySKUs` | At most `MaxIDsInRequest` (**250**) ids, rejected early. An empty list is also rejected: it would otherwise request the bare endpoint and return an opaque API error. |
| **Single call** | `ListCategoryPrintings`, `ListCategoryConditions`, `ListCategoryLanguages`, `ListCategoryRarities`, `ListProductSKUs` | No paging and no count: the API returns the whole list (§3.1). |

### 6.1 Counts

```go
TotalProducts(ctx, category, productTypes)   // nil = no filter; empty = error
TotalGroups(ctx, category)
TotalCategories(ctx)
```

All three issue a `limit=1` query and read `TotalItems`. **`TotalProducts`
with a nil filter is the only count that can see a product whose type is
unknown**, which is what makes the dump's completeness check possible.

`TotalProducts` and `ListAllProducts` refuse an empty, non-nil filter before
sending anything. The API reads a missing filter as every product, so a
caller asking for the singles of a category that has none would otherwise
be handed the whole category.

A 404 with an empty envelope is read as zero: the API reports an empty
result set as not-found, and for a count that is the answer.

### 6.2 Models

`Product` carries `ProductID`, `Name`, `CleanName`, `ImageURL`, `GroupID`,
`CategoryID`, `URL`, `ModifiedOn`, `ImageCount`, `PresaleInfo`, and
optionally `Skus` and `ExtendedData`. The API sends `presaleInfo` for every
product, presale or not: `IsPresale` marks one whose details may still
change before release, and `ReleasedOn` is also set for some products long
released (1,727 of Lorcana's 3,663 on 2026-10-01, 14 of them presale).
`ProductType` is **never returned by the API** — `tcgdumper` stamps the type
it fetched the product by. `Product.Extended(name)` reads one
`extendedData` entry, which is where the catalog files a card's collector
number (`Number`) and rarity (`Rarity`).

`SKU` is one sellable variant: `SKUID`, `ProductID`, `LanguageID`,
`PrintingID`, `ConditionID`. Every id in this package is a plain `int`,
so nothing stops a product id being passed where a sku id is meant: the
caller picks between `GetMarketPricesByProducts` and `GetMarketPricesBySKUs`.
The three trailing ids are decoded by the category metadata listings, which
is why the dump carries them.

`Group` is a set or expansion. `Group.ReleaseDate()` returns `PublishedOn`
without its time of day.

---

## 7. The catalog dump

```go
type CatalogDump struct {
	Category   Category    `json:"category"`
	Conditions []Condition `json:"conditions"`
	Languages  []Language  `json:"languages"`
	Printings  []Printing  `json:"printings"`
	Rarities   []Rarity    `json:"rarities"`
	Groups     []Group     `json:"groups"`
	Products   []Product   `json:"products"`
}
```

Written by `tcgdumper` as one JSON document, compact by default and indented
with `-p`/`-pretty`. Products carry their skus and are sorted by product id.

The metadata arrays make the dump **self-describing**: every `languageId`,
`printingId` and `conditionId` a sku references resolves inside the same
file, with no table maintained by the reader.

`CatalogDump.PrintingNames()` maps each product to the distinct printing
names its skus carry, ordered as the dump lists the category's printings.

### 7.1 Flags

| Flag | Meaning |
| --- | --- |
| `-category` | required, the category id to dump |
| `-pub` / `-pri` | keys; fall back to `TCGPLAYER_PUBLIC_KEY` / `TCGPLAYER_PRIVATE_KEY` |
| `-thread` | worker count, default 8 |
| `-p` / `-pretty` | indent the output; default is one line |

### 7.2 What a dump run does

1. Fetch the category's details; refuse an id the platform does not serve.
2. Fetch conditions, languages, printings and rarities.
3. Count groups, then page them.
4. For each of the category's product types, count the products of that
   type and enqueue one job per page.
5. **Count the category with no filter** and compare (§8).
6. Page every job across `-thread` workers, stamping each product with the
   type it answered to.
7. Sort by product id and check everything collected (§8); only then
   encode.

---

## 8. Invariants the dump enforces

A run **fails and writes nothing** when any of these does not hold. All of
them are checked before the first byte of JSON is written.

- **The known types account for the whole category.** `categoryTotal >
  totalProducts` means some type is missing from `ProductTypesByCategory`
  and its products would go undumped. This is the check that found
  Yu-Gi-Oh's missing 35.
- **Every page came back whole.** No page may error, each page must hold
  its share of its type's count, and the products and groups collected must
  match the counts taken up front. A failed or mismatched page is named by
  type and offset.
- **Every product is there once and belongs somewhere.** No product repeats
  within its type, no group repeats, every product's group is in the dump,
  and the distinct product ids equal the unfiltered count. Counts alone
  would let a duplicate stand in for the product it pushed out.
- **The category exists.** An id serving no category stops the run rather
  than panicking on an empty slice.
- **There is a worker.** A `-thread` below one is refused; no worker would
  take a page and the run would hang.

One condition **warns** rather than fails: `categoryTotal < totalProducts`
means a product carries more than one type. It is fetched once per type and
appears once per type in the dump, under the same id. None of the nightly
categories did so in the 14 runs to 2026-09-30.

Because the guards fail closed and the workflow uploads only on success, a
failed dump leaves the previous good file in the bucket.

---

## 9. Known upstream quirks

- **An empty result set is a 404**, not a zero count (§6.1).
- **Product ids are allocated before public release.** An id lower than a
  dump's highest does not mean the product existed when the dump ran; check
  `presaleInfo.releasedOn`, which the dump carries.
- **`Rarity.DBValue` can carry stray whitespace**, where `DisplayText` does
  not. Yu-Gi-Oh's rarity 515 answers
  `{"displayText":"Prismatic Collector's Rare","dbValue":"Prismatic Collector's Rare "}`,
  while the products carrying that rarity spell it without the space. A
  consumer joining products to rarities on `DBValue` and an exact string
  match misses every one of them; joining on `DisplayText` does not hit it.
  This package reproduces both faithfully rather than normalising, and the
  consumers trim: datastore-gen's Yu-Gi-Oh builder trims the product's
  rarity and the listing's `DisplayText`, because rarity is part of the
  identity a Yu-Gi-Oh card is resolved by and a padded one is a second
  identity for a card that already has one.
- **A category can be listed and serve nothing.** Id 21 is listed and has no
  groups; Palworld and Cyberpunk were listed and empty for weeks before
  carrying products.
- **`tcgcsv.com`**, the public mirror used for independent checks, requires
  a descriptive User-Agent for product files and carries no product type
  field.

---

## 10. The consumer's contract

Readers of the dump — `datastore-gen`, `go-mtgban`, `mtgban-website` — may
rely on:

- Every `groupId`, `conditionId`, `languageId` and `printingId` a product or
  sku references resolves within the same file.
- Products are sorted by id, and each id appears once, except a product
  filed under two types, which appears once per type (§8).
- Every product carries a non-empty `productType`, and
  `SinglesProductTypes`/`SealedProductTypes` classify it.
- The JSON field names are the API's own, unchanged.
- A published dump is complete, because an incomplete one is never uploaded.

Readers may **not** rely on:

- A product having `extendedData`, or any particular key within it. Sealed
  products routinely have none.
- `Number` being numeric. Over 8,000 Magic collector numbers are not.
- The set of product types being stable — a category may gain one.
- A product having `presaleInfo`. Dumps written before it was decoded carry
  none, so a missing one means unknown, not "not presale".

---

## 11. Glossary

| Term | Meaning |
| --- | --- |
| **Category** | A game or product line; the top-level split of the catalog |
| **Group** | A set or expansion within a category |
| **Product** | One catalog item: a card or a sealed item |
| **SKU** | One sellable variant of a product: language × printing × condition |
| **Product type** | The name a category files a product under; the `productTypes` filter |
| **Printing** | A finish, e.g. Normal or Foil |
| **Condition** | A grade, e.g. Near Mint |
| **Extended data** | Per-product key/value pairs where the catalog keeps collector number, rarity and game-specific fields |
| **Dump** | The single JSON document `tcgdumper` writes for one category |
