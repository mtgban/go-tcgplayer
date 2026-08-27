// Package tcgplayer is a client for the TCGplayer catalog and pricing
// API, and defines the format of the catalog dumps cmd/tcgdumper writes.
package tcgplayer

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/hashicorp/go-retryablehttp"
	"golang.org/x/sync/singleflight"
	"golang.org/x/time/rate"
)

const (
	// MaxItemsInResponse is the largest page a listing endpoint returns
	MaxItemsInResponse = 100
	// MaxIDsInRequest is the most ids a batched endpoint accepts
	MaxIDsInRequest = 250
)

const tcgAPIVersion = "v1.39.0"

// Endpoint URLs, overridable for testing
var (
	TokenURL = "https://api.tcgplayer.com/token"

	CatalogCategoriesURL = "https://api.tcgplayer.com/" + tcgAPIVersion + "/catalog/categories"
	CatalogProductsURL   = "https://api.tcgplayer.com/" + tcgAPIVersion + "/catalog/products"
	CatalogGroupsURL     = "https://api.tcgplayer.com/" + tcgAPIVersion + "/catalog/groups"

	PricingProductURL = "https://api.tcgplayer.com/" + tcgAPIVersion + "/pricing/product"
	PricingSKUURL     = "https://api.tcgplayer.com/" + tcgAPIVersion + "/pricing/sku"
)

// All active categories on the platform
const (
	CategoryMagic = iota + 1
	CategoryYuGiOh
	CategoryPokemon
	CategoryAxisAllies
	CategoryBoardgames
	CategoryDDMiniatures
	CategoryEpic
	CategoryHeroclix
	CategoryMonsterpocalypse
	CategoryRedakai
	CategoryStarWarsMiniatures
	CategoryWorldOfWarcraftMiniatures
	CategoryWoW
	CategorySupplies
	CategoryOrganizersStores
	CategoryCardfightVanguard
	CategoryForceOfWill
	CategoryDiceMasters
	CategoryFutureCardBuddyFight
	CategoryWeissSchwarz
	_ // 21, My Little Pony: listed by the platform, serves no groups
	CategoryTCGplayer
	CategoryDragonBallZ
	CategoryFinalFantasy
	CategoryUniVersus
	CategoryStarWarsDestiny
	CategoryDragonBallSuper
	CategoryDragoborne
	CategoryFunko
	CategoryMetaX
	CategoryCardSleeves
	CategoryDeckBoxes
	CategoryCardStorageTins
	CategoryLifeCounters
	CategoryPlaymats
	CategoryZombieWorldOrder
	CategoryTheCasterChronicles
	CategoryMyLittlePony
	CategoryWarhammerBooks
	CategoryWarhammerBigBoxGames
	CategoryWarhammerBoxSets
	CategoryWarhammerClampacks
	CategoryCitadelPaints
	CategoryCitadelTools
	CategoryWarhammerGameAccessories
	CategoryBooks
	CategoryExodus
	CategoryLightseekers
	CategoryProtectivePages
	CategoryStorageAlbums
	CategoryCollectibleStorage
	CategorySupplyBundles
	CategoryMunchkin
	CategoryWarhammerAgeOfSigmarChampions
	CategoryArchitect
	CategoryBulkLots
	CategoryTransformers
	CategoryBakugan
	CategoryKeyForge
	CategoryChronoClashSystem
	CategoryArgentSaga
	CategoryFleshAndBlood
	CategoryDigimon
	CategoryAlternateSouls
	CategoryGateRuler
	CategoryMetaZoo
	CategoryWIXOSS
	CategoryOnePiece
	CategoryMarvelComics
	CategoryDCComics
	CategoryLorcana
	CategoryBattleSpiritsSaga
	CategoryShadowverseEvolve
	CategoryGrandArchive
	CategoryAkora
	CategoryKryptik
	CategorySorceryContestedRealm
	CategoryAlphaClash
	CategoryStarWarsUnlimited
	CategoryDragonBallSuperFusionWorld
	CategoryUnionArena
	CategoryTCGplayerSupplies
	CategoryElestrals
	CategoryNeopetsBattledome
	CategoryPokemonJapan
	CategoryGundam
	CategoryHololive
	CategoryGodzilla
	CategoryRiftbound
	CategoryCookieRunBraverse
	CategoryPalworld
	CategoryCyberpunk
)

// AllProductTypes lists every product type the catalog files products under
var AllProductTypes = []string{
	"Cards",
	"Booster Box",
	"Booster Pack",
	"Sealed Products",
	"Intro Pack",
	"Fat Pack",
	"Box Sets",
	"Precon/Event Decks",
	"Magic Deck Pack",
	"Magic Booster Box Case",
	"All 5 Intro Packs",
	"Intro Pack Display",
	"3x Magic Booster Packs",
	"Booster Battle Pack",
}

// ProductTypesSingles lists the product types holding single cards
var ProductTypesSingles = []string{AllProductTypes[0]}

// ProductTypesSealed lists the product types holding sealed products
var ProductTypesSealed = AllProductTypes[1:]

// Client talks to the TCGplayer API, holding the credentials every call
// needs. It acquires and refreshes bearer tokens on demand, holds requests
// to a rate limit, and retries the ones that fail transiently. A Client is
// safe for concurrent use.
type Client struct {
	client *retryablehttp.Client
}

// NewClient returns a Client authenticating with the given key pair. Both
// keys are required. No request is made here: the first token is acquired
// by the first call that needs one.
func NewClient(publicKey, privateKey string) (*Client, error) {
	if publicKey == "" || privateKey == "" {
		return nil, fmt.Errorf("missing public or private key")
	}

	tokenClient := retryablehttp.NewClient()
	tokenClient.Logger = nil
	tokenClient.HTTPClient.Timeout = time.Minute

	tcg := Client{}
	tcg.client = retryablehttp.NewClient()
	tcg.client.Logger = nil
	// Bound each attempt so a stalled connection surfaces as a
	// retryable error instead of hanging the caller forever
	tcg.client.HTTPClient.Timeout = 2 * time.Minute
	// Do not retry requests that failed acquiring a token: the token
	// client has its own retries, going through them again would only
	// multiply attempts and backoff on credentials that cannot work
	tcg.client.CheckRetry = func(ctx context.Context, resp *http.Response, err error) (bool, error) {
		var terr *tokenError
		if errors.As(err, &terr) {
			return false, err
		}
		return retryablehttp.DefaultRetryPolicy(ctx, resp, err)
	}
	tcg.client.HTTPClient.Transport = &authTransport{
		parent:      tcg.client.HTTPClient.Transport,
		tokenClient: tokenClient,
		publicKey:   publicKey,
		privateKey:  privateKey,

		// Set a relatively high rate to prevent unexpected limits later
		limiter: rate.NewLimiter(80, 20),
	}
	return &tcg, nil
}

type authTransport struct {
	sf          singleflight.Group
	parent      http.RoundTripper
	tokenClient *retryablehttp.Client
	publicKey   string
	privateKey  string
	token       string
	expires     time.Time
	limiter     *rate.Limiter
	mtx         sync.RWMutex
}

// tokenError marks a token acquisition failure that has already been
// through the token client retries and must not be retried again
type tokenError struct {
	err error
}

func (e *tokenError) Error() string { return e.err.Error() }
func (e *tokenError) Unwrap() error { return e.err }

func (t *authTransport) requestToken(ctx context.Context) (string, time.Time, error) {
	params := url.Values{}
	params.Set("grant_type", "client_credentials")
	params.Set("client_id", t.publicKey)
	params.Set("client_secret", t.privateKey)
	payload := strings.NewReader(params.Encode())

	req, err := retryablehttp.NewRequestWithContext(ctx, http.MethodPost, TokenURL, payload)
	if err != nil {
		return "", time.Time{}, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := t.tokenClient.Do(req)
	if err != nil {
		return "", time.Time{}, err
	}
	defer resp.Body.Close()

	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", time.Time{}, err
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", time.Time{}, fmt.Errorf("token http %d: %s", resp.StatusCode, string(data))
	}

	var response struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   int64  `json:"expires_in"` // seconds
	}
	err = json.Unmarshal(data, &response)
	if err != nil {
		return "", time.Time{}, err
	}

	// Measured from receive time, this slightly overestimates the real
	// validity window; the refresh buffer in RoundTrip absorbs the skew
	expires := time.Now().Add(time.Duration(response.ExpiresIn) * time.Second)
	return response.AccessToken, expires, nil
}

func (t *authTransport) refreshToken(ctx context.Context) (string, error) {
	// Run this only once for concurrent requests
	v, err, _ := t.sf.Do("oauth_token", func() (any, error) {
		tok, exp, err := t.requestToken(ctx)
		if err != nil {
			return nil, err
		}
		// Update internal state under lock
		t.mtx.Lock()
		t.token, t.expires = tok, exp
		t.mtx.Unlock()
		return tok, nil
	})
	if err != nil {
		return "", err
	}
	return v.(string), nil
}

// RoundTrip waits for the rate limiter, then attaches a valid bearer token
// to a copy of req, acquiring or refreshing one when needed
func (t *authTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	err := t.limiter.Wait(req.Context())
	if err != nil {
		return nil, err
	}

	// Load exisiting data if present
	t.mtx.RLock()
	token, expires := t.token, t.expires
	t.mtx.RUnlock()

	// Check their validity
	if token == "" || time.Now().After(expires.Add(-5*time.Minute)) {
		var err error
		token, err = t.refreshToken(req.Context())
		if err != nil {
			return nil, &tokenError{err}
		}
	}

	// RoundTrippers must not modify the original request
	req = req.Clone(req.Context())
	req.Header.Set("Authorization", fmt.Sprintf("Bearer %s", token))

	// Not strictly needed, but shield for an unset parent
	rt := t.parent
	if rt == nil {
		rt = http.DefaultTransport
	}
	return rt.RoundTrip(req)
}

// BaseResponse is the envelope every endpoint wraps its payload in. Results
// is left raw for the caller to decode into the type the endpoint returns.
type BaseResponse struct {
	TotalItems int             `json:"totalItems"`
	Success    bool            `json:"success"`
	Errors     []string        `json:"errors"`
	Results    json.RawMessage `json:"results"`
}

// APIError is a request the API answered with an error envelope, keeping
// the http status the message arrived under
type APIError struct {
	StatusCode int
	Messages   []string
}

// Error joins the messages the API reported
func (e *APIError) Error() string {
	return strings.Join(e.Messages, " ")
}

// Get performs an authenticated GET request against link and parses the
// response envelope, leaving its payload for the caller to decode
func (tcg *Client) Get(ctx context.Context, link string) (*BaseResponse, error) {
	req, err := retryablehttp.NewRequestWithContext(ctx, http.MethodGet, link, nil)
	if err != nil {
		return nil, err
	}

	resp, err := tcg.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	var response BaseResponse
	err = json.Unmarshal(data, &response)
	if err != nil {
		// Not an envelope (e.g. an error page from a proxy), report
		// the http status when the request failed
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			return nil, fmt.Errorf("http %d: %s", resp.StatusCode, string(data))
		}
		return nil, fmt.Errorf("%w: %s", err, string(data))
	}
	// Prefer the error messages reported by the API when present,
	// otherwise fall back to the raw status and body
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		if len(response.Errors) > 0 {
			return nil, &APIError{StatusCode: resp.StatusCode, Messages: response.Errors}
		}
		return nil, fmt.Errorf("http %d: %s", resp.StatusCode, string(data))
	}

	return &response, nil
}

// TotalProducts reports how many products a category holds, optionally
// narrowed to the given product types
func (tcg *Client) TotalProducts(ctx context.Context, category int, productTypes []string) (int, error) {
	return tcg.queryTotal(ctx, CatalogProductsURL, category, productTypes)
}

// TotalGroups reports how many groups a category holds
func (tcg *Client) TotalGroups(ctx context.Context, category int) (int, error) {
	return tcg.queryTotal(ctx, CatalogGroupsURL, category, nil)
}

// TotalCategories reports how many categories the platform holds
func (tcg *Client) TotalCategories(ctx context.Context) (int, error) {
	return tcg.queryTotal(ctx, CatalogCategoriesURL, 0, nil)
}

// queryTotal reports how many items a full listing would return
func (tcg *Client) queryTotal(ctx context.Context, link string, category int, productTypes []string) (int, error) {
	u, err := url.Parse(link)
	if err != nil {
		return 0, err
	}
	v := url.Values{}
	if category > 0 {
		v.Set("categoryId", fmt.Sprint(category))
	}
	if productTypes != nil {
		v.Set("productTypes", strings.Join(productTypes, ","))
	}
	v.Set("limit", fmt.Sprint(1))
	u.RawQuery = v.Encode()

	response, err := tcg.Get(ctx, u.String())
	// The API reports an empty result set as a not-found error rather
	// than a zero count, so for a total that is the answer, not a failure
	var apiErr *APIError
	if errors.As(err, &apiErr) && apiErr.StatusCode == http.StatusNotFound {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	return response.TotalItems, nil
}

// checkComplete reports a listing that answered with fewer items than it
// counted. The endpoints below do not paginate, so a short answer drops
// entries with nothing to page for the rest; endpoints that leave totalItems
// unset count as zero here and cannot trip it.
func checkComplete(resp *BaseResponse, got int) error {
	if resp.TotalItems > got {
		return fmt.Errorf("listing returned %d of %d items", got, resp.TotalItems)
	}
	return nil
}

// Printing is a finish a category's cards are printed in, such as Foil
type Printing struct {
	PrintingID   int    `json:"printingId"`
	Name         string `json:"name"`
	DisplayOrder int    `json:"displayOrder"`
	ModifiedOn   string `json:"modifiedOn"`
}

// ListCategoryPrintings returns the printings a category's skus reference
func (tcg *Client) ListCategoryPrintings(ctx context.Context, category int) ([]Printing, error) {
	resp, err := tcg.Get(ctx, fmt.Sprintf("%s/%d/printings", CatalogCategoriesURL, category))
	if err != nil {
		return nil, err
	}

	var out []Printing
	err = json.Unmarshal(resp.Results, &out)
	if err != nil {
		return nil, err
	}
	if err := checkComplete(resp, len(out)); err != nil {
		return nil, err
	}

	return out, nil
}

// Condition is a grade a category's products are sold in, such as Near Mint
type Condition struct {
	ConditionID  int    `json:"conditionId"`
	Name         string `json:"name"`
	Abbreviation string `json:"abbreviation"`
	DisplayOrder int    `json:"displayOrder"`
}

// ListCategoryConditions returns the conditions a category's skus reference
func (tcg *Client) ListCategoryConditions(ctx context.Context, category int) ([]Condition, error) {
	resp, err := tcg.Get(ctx, fmt.Sprintf("%s/%d/conditions", CatalogCategoriesURL, category))
	if err != nil {
		return nil, err
	}

	var out []Condition
	err = json.Unmarshal(resp.Results, &out)
	if err != nil {
		return nil, err
	}
	if err := checkComplete(resp, len(out)); err != nil {
		return nil, err
	}

	return out, nil
}

// Language is a language a category's products are printed in
type Language struct {
	LanguageID   int    `json:"languageId"`
	Name         string `json:"name"`
	Abbreviation string `json:"abbr"`
}

// ListCategoryLanguages returns the languages a category's skus reference
func (tcg *Client) ListCategoryLanguages(ctx context.Context, category int) ([]Language, error) {
	resp, err := tcg.Get(ctx, fmt.Sprintf("%s/%d/languages", CatalogCategoriesURL, category))
	if err != nil {
		return nil, err
	}

	var out []Language
	err = json.Unmarshal(resp.Results, &out)
	if err != nil {
		return nil, err
	}
	if err := checkComplete(resp, len(out)); err != nil {
		return nil, err
	}

	return out, nil
}

// Rarity is a rarity a category's cards are printed at
type Rarity struct {
	RarityID    int    `json:"rarityId"`
	DisplayText string `json:"displayText"`
	DBValue     string `json:"dbValue"`
}

// ListCategoryRarities returns the rarities a category's products carry
func (tcg *Client) ListCategoryRarities(ctx context.Context, category int) ([]Rarity, error) {
	resp, err := tcg.Get(ctx, fmt.Sprintf("%s/%d/rarities", CatalogCategoriesURL, category))
	if err != nil {
		return nil, err
	}

	var out []Rarity
	err = json.Unmarshal(resp.Results, &out)
	if err != nil {
		return nil, err
	}
	if err := checkComplete(resp, len(out)); err != nil {
		return nil, err
	}

	return out, nil
}

// Product is a single item in the catalog, a card or a sealed product
type Product struct {
	ProductID  int    `json:"productId"`
	Name       string `json:"name"`
	CleanName  string `json:"cleanName"`
	ImageURL   string `json:"imageUrl"`
	GroupID    int    `json:"groupId"`
	URL        string `json:"url"`
	ModifiedOn string `json:"modifiedOn"`

	// Never returned by the API, which does not report the product type a
	// product is filed under; tcgdumper stamps the type it fetched the
	// product by, so it is present in catalog dumps only
	ProductType string `json:"productType,omitempty"`

	// Only available for catalog API calls
	Skus []SKU `json:"skus,omitempty"`
	// Only available for catalog API calls
	ExtendedData []struct {
		Name        string `json:"name"`
		DisplayName string `json:"displayName"`
		Value       string `json:"value"`
	} `json:"extendedData,omitempty"`
}

// GetProductsDetails returns the details of the given products, at most
// MaxIDsInRequest of them per call. Pass includeSkus to have each product
// carry the skus it is sold as.
func (tcg *Client) GetProductsDetails(ctx context.Context, productIDs []int, includeSkus bool) ([]Product, error) {
	if len(productIDs) == 0 {
		return nil, errors.New("no ids in request")
	}
	if len(productIDs) > MaxIDsInRequest {
		return nil, errors.New("too many ids in request")
	}

	ids := ints2strings(productIDs)
	link := CatalogProductsURL + "/" + strings.Join(ids, ",")

	u, err := url.Parse(link)
	if err != nil {
		return nil, err
	}

	v := url.Values{}
	v.Set("getExtendedFields", "true")
	if includeSkus {
		v.Set("includeSkus", "true")
	}

	u.RawQuery = v.Encode()

	resp, err := tcg.Get(ctx, u.String())
	if err != nil {
		return nil, err
	}

	var out []Product
	err = json.Unmarshal(resp.Results, &out)
	if err != nil {
		return nil, err
	}

	return out, nil
}

// ListAllProducts returns one page of a category's products, starting at
// offset and holding at most MaxItemsInResponse of them. Pair it with
// TotalProducts to walk a whole category.
func (tcg *Client) ListAllProducts(ctx context.Context, category int, productTypes []string, includeSkus bool, offset int) ([]Product, error) {
	u, err := url.Parse(CatalogProductsURL)
	if err != nil {
		return nil, err
	}

	v := url.Values{}
	v.Set("getExtendedFields", "true")
	v.Set("categoryId", fmt.Sprint(category))
	if productTypes != nil {
		v.Set("productTypes", strings.Join(productTypes, ","))
	}
	if includeSkus {
		v.Set("includeSkus", "true")
	}
	v.Set("offset", fmt.Sprint(offset))
	v.Set("limit", fmt.Sprint(MaxItemsInResponse))
	u.RawQuery = v.Encode()

	resp, err := tcg.Get(ctx, u.String())
	if err != nil {
		return nil, err
	}

	var out []Product
	err = json.Unmarshal(resp.Results, &out)
	if err != nil {
		return nil, err
	}

	return out, nil
}

// SKU is a sellable variant of a product: one combination of language,
// printing and condition
type SKU struct {
	SKUID       int `json:"skuId"`
	ProductID   int `json:"productId"`
	LanguageID  int `json:"languageId"`
	PrintingID  int `json:"printingId"`
	ConditionID int `json:"conditionId"`
}

// ListProductSKUs returns the skus a product is sold as
func (tcg *Client) ListProductSKUs(ctx context.Context, productID int) ([]SKU, error) {
	link := fmt.Sprintf("%s/%d/skus", CatalogProductsURL, productID)
	resp, err := tcg.Get(ctx, link)
	if err != nil {
		return nil, err
	}

	var out []SKU
	err = json.Unmarshal(resp.Results, &out)
	if err != nil {
		return nil, err
	}
	if err := checkComplete(resp, len(out)); err != nil {
		return nil, err
	}

	return out, nil
}

// Group is a set, expansion or other collection a category files its
// products under
type Group struct {
	GroupID      int    `json:"groupId"`
	Name         string `json:"name"`
	Abbreviation string `json:"abbreviation"`
	Supplemental bool   `json:"supplemental"`
	PublishedOn  string `json:"publishedOn"`
	ModifiedOn   string `json:"modifiedOn"`
	CategoryID   int    `json:"categoryId"`
}

// ListAllCategoryGroups returns one page of a category's groups, starting
// at offset and holding at most MaxItemsInResponse of them. Pair it with
// TotalGroups to walk a whole category.
func (tcg *Client) ListAllCategoryGroups(ctx context.Context, category, offset int) ([]Group, error) {
	u, err := url.Parse(CatalogGroupsURL)
	if err != nil {
		return nil, err
	}
	v := url.Values{}
	v.Set("categoryId", fmt.Sprint(category))
	v.Set("offset", fmt.Sprint(offset))
	v.Set("limit", fmt.Sprint(MaxItemsInResponse))
	u.RawQuery = v.Encode()

	resp, err := tcg.Get(ctx, u.String())
	if err != nil {
		return nil, err
	}

	var out []Group
	err = json.Unmarshal(resp.Results, &out)
	if err != nil {
		return nil, err
	}

	return out, nil
}

// Category is a game or product line the catalog is split into, such as
// Magic
type Category struct {
	CategoryID        int    `json:"categoryId"`
	Name              string `json:"name"`
	ModifiedOn        string `json:"modifiedOn"`
	DisplayName       string `json:"displayName"`
	SeoCategoryName   string `json:"seoCategoryName"`
	SealedLabel       string `json:"sealedLabel"`
	NonSealedLabel    string `json:"nonSealedLabel"`
	ConditionGuideURL string `json:"conditionGuideUrl"`
	IsScannable       bool   `json:"isScannable"`
	Popularity        int    `json:"popularity"`
}

// GetCategoriesDetails returns the details of the given categories, at most
// MaxIDsInRequest of them per call
func (tcg *Client) GetCategoriesDetails(ctx context.Context, categoryIDs []int) ([]Category, error) {
	if len(categoryIDs) == 0 {
		return nil, errors.New("no ids in request")
	}
	if len(categoryIDs) > MaxIDsInRequest {
		return nil, errors.New("too many ids in request")
	}

	ids := ints2strings(categoryIDs)
	link := CatalogCategoriesURL + "/" + strings.Join(ids, ",")

	resp, err := tcg.Get(ctx, link)
	if err != nil {
		return nil, err
	}

	var out []Category
	err = json.Unmarshal(resp.Results, &out)
	if err != nil {
		return nil, err
	}

	return out, nil
}

func ints2strings(ids []int) []string {
	out := make([]string, 0, len(ids))
	for i := range ids {
		out = append(out, strconv.Itoa(ids[i]))
	}
	return out
}

// ProductPriceSet is the current pricing of one product, for one sub type
type ProductPriceSet struct {
	ProductID      int     `json:"productId"`
	LowPrice       float64 `json:"lowPrice"`
	MarketPrice    float64 `json:"marketPrice"`
	MidPrice       float64 `json:"midPrice"`
	DirectLowPrice float64 `json:"directLowPrice"`
	SubTypeName    string  `json:"subTypeName"`
}

// GetMarketPricesByProducts returns the current pricing of the given
// products, at most MaxIDsInRequest of them per call
func (tcg *Client) GetMarketPricesByProducts(ctx context.Context, productIDs []int) ([]ProductPriceSet, error) {
	if len(productIDs) == 0 {
		return nil, errors.New("no ids in request")
	}
	if len(productIDs) > MaxIDsInRequest {
		return nil, errors.New("too many ids in request")
	}

	ids := ints2strings(productIDs)
	link := PricingProductURL + "/" + strings.Join(ids, ",")

	resp, err := tcg.Get(ctx, link)
	if err != nil {
		return nil, err
	}

	var out []ProductPriceSet
	err = json.Unmarshal(resp.Results, &out)
	if err != nil {
		return nil, err
	}

	return out, nil
}

// SKUPriceSet is the current pricing of one sku
type SKUPriceSet struct {
	SKUID              int     `json:"skuId"`
	LowPrice           float64 `json:"lowPrice"`
	LowestShipping     float64 `json:"lowestShipping"`
	LowestListingPrice float64 `json:"lowestListingPrice"`
	MarketPrice        float64 `json:"marketPrice"`
	DirectLowPrice     float64 `json:"directLowPrice"`
}

// GetMarketPricesBySKUs returns the current pricing of the given skus, at
// most MaxIDsInRequest of them per call
func (tcg *Client) GetMarketPricesBySKUs(ctx context.Context, skuIDs []int) ([]SKUPriceSet, error) {
	if len(skuIDs) == 0 {
		return nil, errors.New("no ids in request")
	}
	if len(skuIDs) > MaxIDsInRequest {
		return nil, errors.New("too many ids in request")
	}

	ids := ints2strings(skuIDs)
	link := PricingSKUURL + "/" + strings.Join(ids, ",")

	resp, err := tcg.Get(ctx, link)
	if err != nil {
		return nil, err
	}

	var out []SKUPriceSet
	err = json.Unmarshal(resp.Results, &out)
	if err != nil {
		return nil, err
	}

	return out, nil
}

// CatalogDump is the envelope cmd/tcgdumper writes for a category. Naming it
// here keeps the program that writes a dump and the programs that read one on
// a single definition of the format, rather than each carrying its own.
type CatalogDump struct {
	Category   Category    `json:"category"`
	Conditions []Condition `json:"conditions"`
	Languages  []Language  `json:"languages"`
	Printings  []Printing  `json:"printings"`
	Rarities   []Rarity    `json:"rarities"`
	Groups     []Group     `json:"groups"`
	Products   []Product   `json:"products"`
}

// Extended reads the extendedData entry a product carries under name, or ""
// when it carries none. The catalog files a card's collector number, rarity
// and the rest there rather than as fields of their own.
func (p Product) Extended(name string) string {
	for _, e := range p.ExtendedData {
		if e.Name == name {
			return e.Value
		}
	}
	return ""
}

// ReleaseDate is the group's publish date without the time of day.
// ReleaseDate is the day the group was published, without the time of day
func (g Group) ReleaseDate() string {
	return strings.SplitN(g.PublishedOn, "T", 2)[0]
}

// PrintingNames maps each product to the distinct printing names its skus
// carry, ordered as the dump lists the category's printings. A printing the
// dump does not list for a product is one that product is not sold in.
func (d *CatalogDump) PrintingNames() map[int][]string {
	name := map[int]string{}
	rank := map[string]int{}
	for i, printing := range d.Printings {
		name[printing.PrintingID] = printing.Name
		rank[printing.Name] = i
	}
	out := map[int][]string{}
	for _, product := range d.Products {
		var names []string
		for _, sku := range product.Skus {
			n := name[sku.PrintingID]
			if n == "" || slices.Contains(names, n) {
				continue
			}
			names = append(names, n)
		}
		slices.SortFunc(names, func(a, b string) int { return rank[a] - rank[b] })
		out[product.ProductID] = names
	}
	return out
}
