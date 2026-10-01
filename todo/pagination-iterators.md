# Offer iterators over the paged endpoints

Walking a category means pairing a count with a paged call and getting the
arithmetic right:

```go
total, err := c.TotalProducts(ctx, category, types)
for i := 0; i < total; i += tcgplayer.MaxItemsInResponse {
	page, err := c.ListAllProducts(ctx, category, types, true, i)
	…
}
```

Every consumer writes that loop, and `tcgdumper` writes a more careful
version of it with per-type jobs and a worker pool. The offset arithmetic,
the "did I get everything" check and the error handling are re-derived each
time, which is exactly where a silent shortfall hides.

## Proposal

Go 1.23 range-over-func, which this module can use (`go 1.26.0`):

```go
func (tcg *Client) Products(ctx context.Context, category int, types []string, includeSkus bool) iter.Seq2[Product, error]
func (tcg *Client) Groups(ctx context.Context, category int) iter.Seq2[Group, error]
```

```go
for product, err := range client.Products(ctx, category, types, true) {
	if err != nil {
		return err
	}
	…
}
```

The iterator owns the count, the offsets and the short-page check, so a
consumer cannot get them wrong. Existing paged calls stay: a caller wanting
a specific page still has one.

## Care

- **Do not hide the completeness check.** The iterator must fail when the
  pages do not add up to the count it opened with, the same way `tcgdumper`
  does — an iterator that quietly stops early would reintroduce the exact
  bug this repo guards against.
- **Concurrency stays the caller's.** `tcgdumper` fans out across product
  types with a worker pool; a sequential iterator is the wrong shape there.
  Either expose a page iterator it can drive, or leave the dumper as it is.
  Do not make the iterator concurrent internally — order becomes undefined
  and the error path stops being obvious.
- Measure before migrating the dumper. It is the one consumer whose paging
  is already correct and well tested.
