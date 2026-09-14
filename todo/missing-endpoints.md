# Wrap the endpoints the client still omits

The client covers the catalog and market pricing. Two gaps are worth closing.

## Buylist pricing

`/pricing/buy/product/{ids}` and `/pricing/buy/sku/{ids}` are not wrapped,
though market pricing is. For a repo whose consumers run arbitrage between
what stores pay and what they charge, the buy side is an odd omission.

The shape mirrors `GetMarketPricesByProducts` exactly — batch by id, cap at
`MaxIDsInRequest`, reject an empty list, decode `Results` into a slice — so
this is the smallest useful change in this folder.

## SKU-level metadata already half-present

`ListProductSKUs` returns skus for one product. There is no batched form, so
a caller wanting skus for many products either pages the whole category with
`includeSkus` or issues one request per product. If TCGplayer offers a
batched sku endpoint, wrapping it would let a consumer refresh skus without
re-walking a catalog.

## Worth checking first

`SPECIFICATIONS.md` §6 lists what is wrapped. Before adding anything, fetch
one raw response and print every key at every level, then diff that against
the struct that will decode it — the fields a struct omits are not the fields
the vendor omits. `Category` carries `SeoCategoryName`, `SealedLabel`,
`NonSealedLabel`, `ConditionGuideURL`, `IsScannable` and `Popularity`
precisely because someone looked.

## Not worth wrapping

Store and listing endpoints (inventory, orders, catalog search) are a
different product with a different auth story. This package is the catalog
and its prices; keep it that way.
