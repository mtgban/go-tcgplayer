# Publish a manifest beside each dump

The dump's guards catch a run that *failed*. Nothing catches a run that
*succeeded* against a catalog that was itself wrong — TCGplayer serving a
truncated category with 200s, counts and all, would pass every check and
overwrite a good file with a smaller one.

## Proposal

Write a small `manifest.json` next to `tcgplayer-catalog.json.xz`:

```json
{
  "category": {"id": 2, "name": "YuGiOh"},
  "dumpedAt": "2026-09-14T05:04:11Z",
  "products": 47433,
  "groups": 658,
  "skus": 301875,
  "productTypes": {"Cards": 46242, "Tin": 22, "YGO Start Decks": 13},
  "tool": "tcgdumper <commit>"
}
```

It costs nothing to produce — every number is already counted during the run —
and makes two things possible:

1. **A shrink check in the workflow.** Download the previous manifest, and
   fail the upload when the product count drops by more than a threshold
   (say 5%) without the dump having failed. A category legitimately shrinks
   rarely and slightly; a 30% drop is the platform having a bad night.
2. **Answering "when was this built, and from what" without decompressing a
   gigabyte.** Consumers currently infer freshness from the B2 object's
   timestamp.

## Care

- The threshold must be a warning-with-override, not an absolute block: a
  category really can lose products when TCGplayer retires a line.
- Store the manifest uncompressed so a workflow can read it with `curl`.
- `productTypes` in the manifest doubles as a cheap drift check against
  `ProductTypesByCategory` — a type appearing there that the table does not
  name is a category that needs an entry.
