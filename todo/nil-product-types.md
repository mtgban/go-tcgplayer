# A category with no singles asks for every product

`ListAllProducts` and `TotalProducts` treat a `nil` product type list as no
filter at all, which is what makes `TotalProducts(ctx, category, nil)` the
count that sees every product. The helpers that pick a category's types
return `nil` for "none":

- `SinglesProductTypes` is `nil` for 19 mapped categories, the ones selling
  no singles: supplies, storage, Funko, gift cards, KeyForge and the like.
- `SealedProductTypes` is `nil` for Epic, which sells only singles.

So asking for a category's singles, where it has none, returns its whole
catalog, and asking for Epic's sealed products returns its cards. Nothing
fails; the answer is simply the wrong set.

## Who it reaches

go-mtgban's `NewScraperGeneric` defaults to
`tcgplayer.SinglesProductTypes(category)` when no types are given and passes
the result straight to `ListAllProducts` and `TotalProducts`. Nothing
constructs it today (go-mtgban checked on 2026-10-01, mtgban-website and
bantool on 2026-09-30), so this is latent. The README warns about it, which is a
caveat, not a guard.

## Proposal

Make "none" and "no filter" different values:

- `SinglesProductTypes` and `SealedProductTypes` return `[]string{}` rather
  than `nil` when the category has none.
- `ListAllProducts` and `TotalProducts` reject a non-nil empty list with an
  error, since a filter naming no types can only mean a caller with nothing
  to ask for.

`nil` keeps meaning no filter, so the unfiltered count is unchanged.
go-mtgban compares its types with `slices.Equal`, which treats `nil` and an
empty slice as equal, so its log tag keeps working.

## Care

Nobody has checked what the API does with an empty `productTypes=`
parameter. Reject it before the request rather than finding out.
