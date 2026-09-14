# Move the endpoint URLs onto the Client

The six endpoint URLs are package-level `var`s:

```go
var (
	TokenURL             = "https://api.tcgplayer.com/token"
	CatalogCategoriesURL = …
	…
)
```

They are variables rather than constants for one reason: tests point them at
an `httptest` server. That costs more than it looks.

- **Package-level mutable state.** Any consumer can rewrite them, for every
  other consumer in the process, at any time.
- **Tests cannot run in parallel.** `newTestClient` says so in its own
  comment — it mutates globals and restores them in `t.Cleanup`, so `t.Parallel()`
  is unavailable to every test in the package.
- **Two clients cannot differ.** Sandbox and production in one process is
  impossible.

## Proposed shape

A `baseURL` on the `Client`, defaulting to the public host, with the paths
derived from it:

```go
type Client struct {
	client  *retryablehttp.Client
	baseURL string
}

func NewClient(publicKey, privateKey string, opts ...Option) (*Client, error)
func WithBaseURL(u string) Option
```

Tests then construct a client pointed at their server and the globals go
back to being constants.

## Cost

Breaking: the exported URL vars are part of the API. A search across the
mtgban repos found **no consumer referencing them** — they are used only
inside this package and its tests — so the break is on paper only.

The endpoint wrappers each build their URL from a package var today; they
would take it from `tcg.baseURL` instead. `authTransport` needs the token
URL, so it gets the same value at construction.

Pairs naturally with functional options, which is also what
`todo/pagination-iterators.md` and a configurable rate limit would want. The
rate limit is currently hardcoded at `rate.NewLimiter(80, 20)` and a consumer
with a different budget cannot say so.
