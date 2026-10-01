# Decode the product fields the struct drops

tcgcsv.com's copy of the product listing carries keys that `Product` does
not decode. One record from a Lorcana group, fetched 2026-09-30:

```
categoryId  cleanName  extendedData  groupId  imageCount  imageUrl
modifiedOn  name  presaleInfo.isPresale  presaleInfo.note
presaleInfo.releasedOn  productId  url
```

`categoryId`, `imageCount` and `presaleInfo` are missing from the struct,
so they never reach the dump. All nine products in that group were presale,
each noting that "card details, including rarity and card name, may change
up until release date".

## Why it matters

- AGENTS.md tells a reader to check `presaleInfo.releasedOn` rather than id
  order when asking whether a product existed when a dump ran. A reader of
  the dump cannot.
- datastore-gen builds card identities from a dump's names and rarities. A
  presale product's may still change, and nothing in the dump marks it.

## Proposal

Add the three to `Product`:

```go
CategoryID  int  `json:"categoryId"`
ImageCount  int  `json:"imageCount"`
PresaleInfo *struct {
	IsPresale  bool   `json:"isPresale"`
	ReleasedOn string `json:"releasedOn"`
	Note       string `json:"note"`
} `json:"presaleInfo,omitempty"`
```

Purely additive: a reader on an older version ignores the new keys.

## First

tcgcsv.com is a mirror. Before adding anything, fetch one raw record from
the API itself and print every key at every level, then diff that against
the struct. Extend the full-field fixtures in `tcgplayer_test.go` and the
dumper's wire fixtures to set the new fields, so a wrong tag fails.
