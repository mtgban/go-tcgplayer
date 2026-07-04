package tcgplayer

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/hashicorp/go-cleanhttp"
	"github.com/hashicorp/go-retryablehttp"
	"golang.org/x/sync/singleflight"
	"golang.org/x/time/rate"
)

const (
	MaxItemsInResponse = 100
	MaxIdsInRequest    = 250
)

const (
	tcgApiVersion = "v1.39.0"

	TcgApiTokenURL = "https://api.tcgplayer.com/token"

	TcgApiCatalogCategoriesURL = "https://api.tcgplayer.com/" + tcgApiVersion + "/catalog/categories"
	TcgApiCatalogProductsURL   = "https://api.tcgplayer.com/" + tcgApiVersion + "/catalog/products"
	TcgApiCatalogGroupsURL     = "https://api.tcgplayer.com/" + tcgApiVersion + "/catalog/groups"

	TcgApiPricingProductURL = "https://api.tcgplayer.com/" + tcgApiVersion + "/pricing/product"
	TcgApiPricingSkuURL     = "https://api.tcgplayer.com/" + tcgApiVersion + "/pricing/sku"
)

// All active categories on the platform
const (
	CategoryMagic = iota + 1
	CategoryYuGiOh
	CategoryPokemon
	CategoryAxisAllies
	_
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
	CategoryChronoClashSystem
	CategoryForceOfWill
	CategoryDiceMasters
	CategoryFutureCardBuddyFight
	CategoryWeissSchwarz
	_
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
	CategoryCardfightVanguard
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
	_
	_
)

// List of all possible product types
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

// List of all product types containing Singles
var ProductTypesSingles = []string{AllProductTypes[0]}

// List of all product types containing Sealed Products
var ProductTypesSealed = AllProductTypes[1:len(AllProductTypes)]

type Client struct {
	client *retryablehttp.Client
}

func NewClient(publicKey, privateKey string) (*Client, error) {
	if publicKey == "" || privateKey == "" {
		return nil, fmt.Errorf("missing public or private key")
	}

	tcg := Client{}
	tcg.client = retryablehttp.NewClient()
	tcg.client.Logger = nil
	tcg.client.HTTPClient.Transport = &authTransport{
		parent:     tcg.client.HTTPClient.Transport,
		publicKey:  publicKey,
		privateKey: privateKey,

		// Set a relatively high rate to prevent unexpected limits later
		limiter: rate.NewLimiter(80, 20),
	}
	return &tcg, nil
}

type authTransport struct {
	sf         singleflight.Group
	parent     http.RoundTripper
	publicKey  string
	privateKey string
	token      string
	expires    time.Time
	limiter    *rate.Limiter
	mtx        sync.RWMutex
}

func (t *authTransport) requestToken(ctx context.Context) (string, time.Time, error) {
	params := url.Values{}
	params.Set("grant_type", "client_credentials")
	params.Set("client_id", t.publicKey)
	params.Set("client_secret", t.privateKey)
	payload := strings.NewReader(params.Encode())

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, TcgApiTokenURL, payload)
	if err != nil {
		return "", time.Time{}, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := cleanhttp.DefaultClient().Do(req)
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
			return nil, err
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

type BaseResponse struct {
	TotalItems int             `json:"totalItems"`
	Success    bool            `json:"success"`
	Errors     []string        `json:"errors"`
	Results    json.RawMessage `json:"results"`
}

// Perform an authenticated GET request and partially parse the response
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
			return nil, errors.New(strings.Join(response.Errors, " "))
		}
		return nil, fmt.Errorf("http %d: %s", resp.StatusCode, string(data))
	}

	return &response, nil
}

func (tcg *Client) TotalProducts(ctx context.Context, category int, productTypes []string) (int, error) {
	return tcg.queryTotal(ctx, TcgApiCatalogProductsURL, category, productTypes)
}

func (tcg *Client) TotalGroups(ctx context.Context, category int) (int, error) {
	return tcg.queryTotal(ctx, TcgApiCatalogGroupsURL, category, nil)
}

func (tcg *Client) TotalCategories(ctx context.Context, category int) (int, error) {
	return tcg.queryTotal(ctx, TcgApiCatalogCategoriesURL, category, nil)
}

// Retrieve how many items a full call will be
func (tcg *Client) queryTotal(ctx context.Context, link string, category int, productTypes []string) (int, error) {
	u, err := url.Parse(link)
	if err != nil {
		return 0, err
	}
	v := url.Values{}
	v.Set("categoryId", fmt.Sprint(category))
	if productTypes != nil {
		v.Set("productTypes", strings.Join(productTypes, ","))
	}
	v.Set("limit", fmt.Sprint(1))
	u.RawQuery = v.Encode()

	response, err := tcg.Get(ctx, u.String())
	if err != nil {
		return 0, err
	}
	return response.TotalItems, nil
}

type Printing struct {
	PrintingId   int    `json:"printingId"`
	Name         string `json:"name"`
	DisplayOrder int    `json:"displayOrder"`
	ModifiedOn   string `json:"modifiedOn"`
}

func (tcg *Client) ListCategoryPrintings(ctx context.Context, category int) ([]Printing, error) {
	resp, err := tcg.Get(ctx, fmt.Sprintf("%s/%d/printings", TcgApiCatalogCategoriesURL, category))
	if err != nil {
		return nil, err
	}

	var out []Printing
	err = json.Unmarshal(resp.Results, &out)
	if err != nil {
		return nil, err
	}

	return out, nil
}

type Product struct {
	ProductId  int    `json:"productId"`
	Name       string `json:"name"`
	CleanName  string `json:"cleanName"`
	ImageUrl   string `json:"imageUrl"`
	GroupId    int    `json:"groupId"`
	URL        string `json:"url"`
	ModifiedOn string `json:"modifiedOn"`

	// Only available for catalog API calls
	Skus []SKU `json:"skus,omitempty"`
	// Only available for catalog API calls
	ExtendedData []struct {
		Name        string `json:"name"`
		DisplayName string `json:"displayName"`
		Value       string `json:"value"`
	} `json:"extendedData,omitempty"`
}

func (tcg *Client) GetProductsDetails(ctx context.Context, productIds []int, includeSkus bool) ([]Product, error) {
	if len(productIds) > MaxIdsInRequest {
		return nil, errors.New("too many ids in request")
	}

	ids := ints2strings(productIds)
	link := TcgApiCatalogProductsURL + "/" + strings.Join(ids, ",")

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

func (tcg *Client) ListAllProducts(ctx context.Context, category int, productTypes []string, includeSkus bool, offset int) ([]Product, error) {
	u, err := url.Parse(TcgApiCatalogProductsURL)
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

type SKU struct {
	SkuId       int `json:"skuId"`
	ProductId   int `json:"productId"`
	LanguageId  int `json:"languageId"`
	PrintingId  int `json:"printingId"`
	ConditionId int `json:"conditionId"`
}

func (tcg *Client) ListProductSKUs(ctx context.Context, productId int) ([]SKU, error) {
	link := fmt.Sprintf("%s/%d/skus", TcgApiCatalogProductsURL, productId)
	resp, err := tcg.Get(ctx, link)
	if err != nil {
		return nil, err
	}

	var out []SKU
	err = json.Unmarshal(resp.Results, &out)
	if err != nil {
		return nil, err
	}

	return out, nil
}

type Group struct {
	GroupID      int    `json:"groupId"`
	Name         string `json:"name"`
	Abbreviation string `json:"abbreviation"`
	Supplemental bool   `json:"supplemental"`
	PublishedOn  string `json:"publishedOn"`
	ModifiedOn   string `json:"modifiedOn"`
	CategoryID   int    `json:"categoryId"`
}

func (tcg *Client) ListAllCategoryGroups(ctx context.Context, category, offset int) ([]Group, error) {
	u, err := url.Parse(TcgApiCatalogGroupsURL)
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

func (tcg *Client) GetCategoriesDetails(ctx context.Context, categoryIds []int) ([]Category, error) {
	if len(categoryIds) > MaxIdsInRequest {
		return nil, errors.New("too many ids in request")
	}

	ids := ints2strings(categoryIds)
	link := TcgApiCatalogCategoriesURL + "/" + strings.Join(ids, ",")

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

type ProductPriceSet struct {
	ProductId      int     `json:"productId"`
	LowPrice       float64 `json:"lowPrice"`
	MarketPrice    float64 `json:"marketPrice"`
	MidPrice       float64 `json:"midPrice"`
	DirectLowPrice float64 `json:"directLowPrice"`
	SubTypeName    string  `json:"subTypeName"`
}

func (tcg *Client) GetMarketPricesByProducts(ctx context.Context, productIds []int) ([]ProductPriceSet, error) {
	if len(productIds) > MaxIdsInRequest {
		return nil, errors.New("too many ids in request")
	}

	ids := ints2strings(productIds)
	link := TcgApiPricingProductURL + "/" + strings.Join(ids, ",")

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

type SKUPriceSet struct {
	SkuId              int     `json:"skuId"`
	LowPrice           float64 `json:"lowPrice"`
	LowestShipping     float64 `json:"lowestShipping"`
	LowestListingPrice float64 `json:"lowestListingPrice"`
	MarketPrice        float64 `json:"marketPrice"`
	DirectLowPrice     float64 `json:"directLowPrice"`
}

func (tcg *Client) GetMarketPricesBySKUs(ctx context.Context, skuIds []int) ([]SKUPriceSet, error) {
	if len(skuIds) > MaxIdsInRequest {
		return nil, errors.New("too many ids in request")
	}

	ids := ints2strings(skuIds)
	link := TcgApiPricingSkuURL + "/" + strings.Join(ids, ",")

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
