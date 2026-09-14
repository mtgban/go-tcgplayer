# Typed identifiers — scope, measured

**Status: in flight.** PR #9 (`codex/completeness-typed-ids`, "Strengthen
catalog completeness checks and introduce typed IDs") already introduces all
nine types:

```
CategoryID  ProductID  SKUID  GroupID  ConditionID
LanguageID  PrintingID  RarityID  ProductType
```

It is based on current `main` and mergeable. This note is not a proposal to
do the work; it is the measurement of what the work costs, so the **scope**
can be decided on evidence rather than symmetry.

## Why it is worth doing at all

Every id is a plain `int` today, so the compiler cannot tell one kind from
another:

```go
GetMarketPricesByProducts(ctx, []int)   // indistinguishable
GetMarketPricesBySKUs(ctx, []int)
```

Pass the wrong list to either and it compiles, hits the wrong endpoint, and
returns prices that are wrong rather than absent. The same shape sits in
`ListProductSKUs(productID int)` and `ListAllCategoryGroups(ctx, category,
offset int)`, where two adjacent bare ints swap silently.

## The measurement

A prototype of all nine types was built here and every consumer compiled
against it with a `replace` directive. The errors are the real migration
cost:

| Variant | go-mtgban | datastore-gen | riftbound-datastore | mtgban-website (own code) |
| --- | --- | --- | --- | --- |
| `CategoryID` only | 11 | **0** | 0 | 0 |
| + `ProductID`, `SKUID`, `ProductType` | 11 | 57 | 0 | 0 |
| + the five field-only ids | 11 | 88 | 0 | 0 |

Library side: **+169/−129 across three files**, nine internal errors,
resolved by making `ints2strings` generic (`[T ~int]`) plus one join helper.

**The wire format does not move** — verified by encoding a `CatalogDump` and
reading back `{"productId":12,"productType":"Cards"}`. A named type over
`int` or `string` marshals identically, so no dump regenerates and no
consumer parser changes.

`mtgban-website` needs nothing of its own; its errors are inside the
go-mtgban module it depends on and clear when that is fixed.

## The scope question for #9

**`CategoryID` is nearly free.** Eleven errors, all one map literal
(`SupportedGames map[string]int` in go-mtgban's `tcgplayer/game.go`), and
datastore-gen needs *nothing* — its `const yugiohCategory = 2` are untyped
constants, which convert implicitly.

**`ProductID` and `SKUID` carry the real safety**: they are what makes the
price-endpoint swap a compile error. 57 mechanical errors in datastore-gen,
mostly `map[int]` keys and `[]int` appends.

**The five field-only ids are the part worth questioning.** `GroupID`,
`ConditionID`, `LanguageID`, `PrintingID` and `RarityID` are the last 31
errors — `GroupID` alone drives 82 of the type mentions — and **none of them
appears as a function parameter anywhere in the API**. They exist only as
struct fields, so nothing can be swapped at a call site. They buy
consistency, not safety, and they are two thirds of datastore-gen's
migration.

Either scope is defensible. What should not happen is paying for the five
without noticing they were the expensive part.

## Notes for whoever lands it

- `TestBatchedIdBounds` shares one `func([]int)` across four id kinds and
  must be restructured — the type system correctly refuses to keep treating
  them as interchangeable. That cost is also evidence the change works.
- `ProductType` as a string type **cannot be closed** in Go; any string
  literal converts. `TestProductTypesByCategoryIsWellFormed` stays the real
  guard against a name the platform does not use.
- Land the library change, tag it, then one PR per consumer, each based on
  its own default branch. Do not stack them.
