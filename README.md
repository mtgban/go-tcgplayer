# go-tcgplayer

A small, practical Go client for the **TCGplayer** API (catalog & pricing). It handles **OAuth2 client-credentials**, **automatic token refresh**, **rate limiting**, and resilient HTTP via **retryablehttp**.

> This repo also holds `tcgdumper`, the program that writes the nightly catalog dump every mtgban datastore is built from.

---

## Features

- **Easy auth** - fetches `access_token` using client-credentials and refreshes it before expiry.
- **Rate limiting** - token-bucket guard around every request (defaults suitable for TCGplayer).
- **Resilient HTTP** - built on [`hashicorp/go-retryablehttp`](https://github.com/hashicorp/go-retryablehttp).
- **Thread-safe** - safe to use from multiple goroutines.
- **Tiny surface area** - helpers for common catalog & pricing flows, plus a low-level `Get` for anything not wrapped yet.

---

## Install

```bash
go get github.com/mtgban/go-tcgplayer
```

---

## Quick start

```go
package main

import (
    "context"
    "fmt"

    "github.com/mtgban/go-tcgplayer"
)

func main() {
    c, err := tcgplayer.NewClient("<PUBLIC_KEY>", "<PRIVATE_KEY>")
    if err != nil {
        // <handle err>
    }

    rows, err := c.GetMarketPricesByProducts(context.TODO(), []int{12345, 67890})
    if err != nil {
        // <handle err>
    }
    fmt.Println("retrieved rows:", len(rows))
}
```

---

## Authentication

`NewClient(publicKey, privateKey)` wires a custom `http.RoundTripper` that:

1. waits on a rate limiter,
2. ensures a valid bearer token (refreshing if missing/near expiry),
3. sets `Authorization: Bearer <token>` on each request.

Tokens are fetched from TCGplayer's `/token` endpoint using `grant_type=client_credentials`. Token acquisition is synchronized so bursts don't stampede the token endpoint.

---

## Catalog helpers

- **Categories**
  - `GetCategoriesDetails(ctx context.Context, ids []int) ([]Category, error)`
  - `TotalCategories(ctx context.Context) (int, error)`

- **Groups**
  - `ListAllCategoryGroups(ctx context.Context, category, offset int) ([]Group, error)`
  - `TotalGroups(ctx context.Context, category int) (int, error)`

- **Products**
  - `GetProductsDetails(ctx context.Context, ids []int, includeSkus bool) ([]Product, error)`
  - `ListAllProducts(ctx context.Context, category int, productTypes []string, includeSkus bool, offset int) ([]Product, error)`
  - `ListProductSKUs(ctx context.Context, productID int) ([]SKU, error)`
  - `TotalProducts(ctx context.Context, category int, productTypes []string) (int, error)`

- **Category metadata** (decodes the ids referenced by SKUs)
  - `ListCategoryPrintings(ctx context.Context, category int) ([]Printing, error)`
  - `ListCategoryConditions(ctx context.Context, category int) ([]Condition, error)`
  - `ListCategoryLanguages(ctx context.Context, category int) ([]Language, error)`
  - `ListCategoryRarities(ctx context.Context, category int) ([]Rarity, error)`

### Product types

`ListAllProducts` and `TotalProducts` filter by product type name, and the names belong to each game rather than to the platform. Yu-Gi-Oh files products under `Tin` and `YGO Start Decks`, which Magic never uses. Dragon Ball Super, UniVersus, Final Fantasy and Star Wars Destiny call their singles `<Game> Singles`, so asking them for `Cards` finds nothing. The API publishes no list of names and does not reject a wrong one: it answers with fewer products.

Ask the package for a category's names instead of writing them out:

- `ProductTypes(category)` - every type the category files products under
- `SinglesProductTypes(category)` - the type holding its single cards
- `SealedProductTypes(category)` - all the others

A `nil` list means no filter at all, which returns every product in the category. `SinglesProductTypes` is `nil` for a category that sells no singles, such as supplies and storage, and `SealedProductTypes` is `nil` for one that sells only singles, such as Epic. Check before passing either on.

To know a walk found everything, compare it with `TotalProducts(ctx, category, nil)`. With no filter it counts products whose type no list names, which a count of the same names cannot see.

`AllProductTypes` is every name in use across the platform, which makes it the wrong filter for any one category. `ProductTypesSingles` and `ProductTypesSealed` are deprecated: they name Magic's types only.

---

## Pricing helpers

- `GetMarketPricesByProducts(ctx, productIds []int) ([]ProductPriceSet, error)`
- `GetMarketPricesBySKUs(ctx, skuIds []int) ([]SKUPriceSet, error)`

Each returns rows with the latest market pricing for the given IDs.

---

## Pagination & limits

- **Offset + limit** paging. Pair each paged call with its count, `TotalProducts` or `TotalGroups`, and walk offsets `0, 100, 200, ...` in steps of `MaxItemsInResponse` (**100**). A page can answer short without an error, so check that what you collected adds up to the count.
- **Batched IDs**. Endpoints accept up to `MaxIDsInRequest` (**250**) IDs at a time. The client rejects more than that, and an empty list, before sending anything.

---

## Error handling

Low-level `Get()` returns a `BaseResponse` envelope. High-level helpers decode `BaseResponse.Results` into typed slices.

- On malformed JSON, you get a Go `error` carrying the raw body.
- On non-2xx responses, the call returns an error: an `*APIError` holding the status and the envelope's `errors` when present, otherwise one built from the HTTP status and raw body.
- A count of an empty result set is `0`, not an error. The API answers one with a 404, and the `Total*` calls read that as zero.

---

## tcgdumper

`tcgdumper` writes one category's whole catalog as a single JSON document, a `CatalogDump`: the category, its conditions, languages, printings and rarities, its groups, and every product with its skus. `catalog-dump.yml` runs it nightly and uploads each dump to B2, where datastore-gen, go-mtgban and mtgban-website read it.

It exits non-zero rather than pass off a short dump. Before fetching it counts the category with no product type filter, and it fails when the category's product types do not account for that count, when a page fails or comes back the wrong size, when the products and groups it collected do not match the counts it opened with, or when a product or group repeats or a product names a group the dump does not hold. All of it is checked before anything is written, so a failed run leaves no output. The workflow uploads only on success, so a failed night leaves the previous dump in place.

```bash
# Build
go build ./cmd/tcgdumper

# Run
TCGPLAYER_PUBLIC_KEY=... TCGPLAYER_PRIVATE_KEY=... ./tcgdumper -category 3 -thread 8 > pokemon.json
```

Flags:
- `-category` (int, required) - Category ID to dump
- `-thread` (int, default 8) - worker concurrency for paging products
- `-pub` / `-pri` (string) - TCGplayer public/private keys; fall back to the `TCGPLAYER_PUBLIC_KEY` / `TCGPLAYER_PRIVATE_KEY` environment variables
- `-p` / `-pretty` - indent the JSON output (default is a single line)

### Reading a dump

Decode it into `tcgplayer.CatalogDump`. Every product carries `productType`, the type it was fetched by, which `SinglesProductTypes` and `SealedProductTypes` classify. `Product.Extended(name)` reads an extended data entry such as `Number` or `Rarity`, `Group.ReleaseDate()` gives the publish date without the time of day, and `CatalogDump.PrintingNames()` maps each product to the printings its skus are sold in. [SPECIFICATIONS.md](SPECIFICATIONS.md) sets out the format and what a reader may rely on.

---

## License

MIT

