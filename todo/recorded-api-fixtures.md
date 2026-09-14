# Record real API responses as test fixtures

The tests serve fixtures as raw JSON rather than encoding the structs under
test, which is what makes them able to catch a wrong `json:` tag:

```go
writeEnvelope(w, len(wantConditions), `[
	{"conditionId": 1, "name": "Near Mint", "abbreviation": "NM", "displayOrder": 1}
]`)
```

That was a real fix. When fixtures were encoded from `[]Condition`, breaking
`json:"abbreviation"` to `json:"WRONG"` failed nothing — the value round
tripped through the same wrong tag it was written with. Nine mutations are
caught now, including the `expires_in` unit bug.

**The remaining gap:** those strings are hand-written from the API
documentation. They encode one reading of the docs, not what the platform
sends. A field transcribed wrongly would be agreed with by both the fixture
and the struct, and the suite would stay green.

## Proposal

Capture one real response per endpoint into `testdata/`, with credentials, and
serve those:

```
testdata/
  token.json
  catalog_categories_1.json
  catalog_categories_1_conditions.json
  catalog_categories_1_languages.json
  catalog_categories_1_printings.json
  catalog_categories_1_rarities.json
  catalog_products_page.json
  catalog_groups_page.json
  pricing_product.json
  pricing_sku.json
```

Recorded with a small `-record` helper behind a build tag, so a refresh is
one command and the capture path is reviewable.

## Care

- **Scrub the token.** `token.json` must carry a placeholder, never a real
  bearer. Check it before it is committed, not after.
- **Trim to a few entries.** A Magic product page is large; two or three
  products carry the same shape.
- **Keep one hand-written fixture per edge case.** The 404-with-empty-envelope
  and short-listing cases are constructed situations a recording will not
  contain, and they guard real behaviour.
- **A recording pins a moment.** If TCGplayer changes a payload the suite
  keeps passing against the old one; the live dump is what notices. This
  raises the floor, it does not replace running against the API.
