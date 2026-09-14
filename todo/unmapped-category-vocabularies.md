# Find the product type names for the 20 unmapped categories

`ProductTypesByCategory` covers 72 of 92 categories. The other 20 are listed
in `categoriesWithoutProductTypes` in the test, in two groups.

**Eleven serve products under names nobody has been able to read:**

| id | category | products |
| --- | --- | --- |
| 5 | Boardgames | 3,689 |
| 11 | Star Wars Miniatures | 957 |
| 41 | Warhammer Box Sets | 907 |
| 4 | Axis & Allies | 532 |
| 39 | Warhammer Books | 466 |
| 43 | Citadel Paints | 453 |
| 45 | Warhammer Game Accessories | 394 |
| 42 | Warhammer Clampacks | 131 |
| 40 | Warhammer Big Box Games | 61 |
| 44 | Citadel Tools | 42 |
| 15 | Organizers & Stores | 2 |

Asking each with all 36 known names accounts for **zero** of their products,
so every one uses a vocabulary of its own.

**Nine serve no products at all** and need nothing until they do:
Monsterpocalypse, Redakai, World of Warcraft Miniatures, Supplies, My Little
Pony (21), Architect, Marvel Comics, DC Comics, Neopets Battledome.

## Why the usual method failed

The 36 known names were read from the search facet aggregation that
tcgplayer.com's own search uses:

```
POST https://mp-search-api.tcgplayer.com/v1/search/request
{"aggregations":["productTypeName"],
 "filters":{"term":{"productLineName":["yugioh"]}}}
```

That enumerated 69 product lines and reproduced Yu-Gi-Oh's facet counts
exactly. These eleven are not among them — they are supplies and miniatures
lines the storefront search does not index the same way.

## Approaches

1. **Find the line name.** The aggregation keys off `productLineName`; the
   eleven may be present under a name that does not match the category name.
   Enumerate `productLineName` with no filter and compare against the
   category list by product count.
2. **Probe candidates against the catalog API.** `TotalProducts(ctx, id,
   []string{candidate})` is a cheap yes/no. Plausible names are visible on
   the storefront's own facet UI for those categories.
3. **Ask TCGplayer.** The absence of a documented product-types endpoint is
   the root cause; it is worth an API support request.

## Done when

Each of the eleven either has an entry whose per-type totals sum to its
unfiltered count, or a recorded reason why not. Until then `ProductTypes`
falls back to all 36 names, which is loud rather than safe: a dump of one of
these categories fails the count check instead of silently shipping short.
