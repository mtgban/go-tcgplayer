# Record real API responses as test fixtures

The tests serve fixtures as raw JSON rather than encoding the structs under
test, which is what makes them able to catch a wrong `json:` tag:

```go
writeEnvelope(w, len(wantConditions), `[
	{"conditionId": 1, "name": "Near Mint", "abbreviation": "NM", "displayOrder": 1}
]`)
```

Every model is decoded from such a fixture with each field set to a
distinct value, and the dump test compares its output with a document built
from the stub's wire json. Renaming any tag but `BaseResponse.Success`,
which nothing reads, fails a test.

**The remaining gap:** those strings are hand-written from the API
documentation. They encode one reading of the docs, not what the platform
sends. A field transcribed wrongly would be agreed with by both the fixture
and the struct, and the suite would stay green.

A recording would also show what the structs leave out. tcgcsv.com's copy of
the product listing carries `categoryId`, `imageCount` and `presaleInfo`
(`isPresale`, `releasedOn`, `note`), and `Product` decodes none of them.

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
