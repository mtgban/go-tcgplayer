# go-tcgplayer

A small, practical Go client for the **TCGplayer** API (catalog & pricing). It handles **OAuth2 client-credentials**, **automatic token refresh**, **rate limiting**, and resilient HTTP via **retryablehttp**.

> This repo also includes a tiny CLI to dump category groups and products.

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
  - `ListProductSKUs(ctx context.Context, productId int) ([]SKU, error)`

- **Category metadata** (decodes the ids referenced by SKUs)
  - `ListCategoryPrintings(ctx context.Context, category int) ([]Printing, error)`
  - `ListCategoryConditions(ctx context.Context, category int) ([]Condition, error)`
  - `ListCategoryLanguages(ctx context.Context, category int) ([]Language, error)`
  - `ListCategoryRarities(ctx context.Context, category int) ([]Rarity, error)`

### Product type filters

- `AllProductTypes` - everything (Cards + sealed)
- `ProductTypesSingles` - only `Cards`
- `ProductTypesSealed` - sealed products (boxes, packs, etc.)

---

## Pricing helpers

- `GetMarketPricesByProducts(ctx, productIds []int) ([]ProductPriceSet, error)`
- `GetMarketPricesBySKUs(ctx, skuIds []int) ([]SKUPriceSet, error)`

Each returns rows with the latest market pricing for the given IDs.

---

## Pagination & limits

- **Offset + limit** paging. Use `MaxItemsInResponse` (**100**) as the page size and iterate offsets: `0, 100, 200, ...`.
- **Batched IDs**. Endpoints accept up to `MaxIdsInRequest` (**250**) IDs at a time. The client checks this and errors early if you exceed it.

---

## Error handling

Low-level `Get()` returns a `BaseResponse` envelope. High-level helpers decode `BaseResponse.Results` into typed slices.

- On malformed JSON, you get a Go `error` (with the raw body snippet).
- On non-2xx responses, the call returns an error: composed from the envelope's API `errors` when present, otherwise from the HTTP status and raw body.

---

## CLI

A simple CLI is included to dump category metadata and all products for a given category. It demonstrates offsets, batching, and concurrency.

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

---

## License

MIT

