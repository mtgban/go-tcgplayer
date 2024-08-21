package tcgplayer

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/hashicorp/go-cleanhttp"
	"github.com/hashicorp/go-retryablehttp"
	"golang.org/x/time/rate"
)

const (
	MaxItemsInResponse = 100
	MaxIdsInRequest    = 250
)

const (
	tcgApiVersion = "v1.39.0"

	tcgApiTokenURL = "https://api.tcgplayer.com/token"

	tcgApiCatalogCategoriesURL = "https://api.tcgplayer.com/" + tcgApiVersion + "/catalog/categories"
	tcgApiCatalogProductsURL   = "https://api.tcgplayer.com/" + tcgApiVersion + "/catalog/products"
	tcgApiCatalogGroupsURL     = "https://api.tcgplayer.com/" + tcgApiVersion + "/catalog/groups"

	tcgApiPricingProductURL = "https://api.tcgplayer.com/" + tcgApiVersion + "/pricing/product"
	tcgApiPricingSkuURL     = "https://api.tcgplayer.com/" + tcgApiVersion + "/pricing/sku"
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

func NewClient(publicKey, privateKey string) *Client {
	tcg := Client{}
	tcg.client = retryablehttp.NewClient()
	tcg.client.Logger = nil
	tcg.client.HTTPClient.Transport = &authTransport{
		parent:     tcg.client.HTTPClient.Transport,
		publicKey:  publicKey,
		privateKey: privateKey,

		// Set a relatively high rate to prevent unexpected limits later
		limiter: rate.NewLimiter(80, 20),

		mtx: sync.RWMutex{},
	}
	return &tcg
}

type authTransport struct {
	parent     http.RoundTripper
	publicKey  string
	privateKey string
	token      string
	expires    time.Time
	limiter    *rate.Limiter
	mtx        sync.RWMutex
}

func (t *authTransport) requestToken() (string, time.Time, error) {
	params := url.Values{}
	params.Set("grant_type", "client_credentials")
	params.Set("client_id", t.publicKey)
	params.Set("client_secret", t.privateKey)

	resp, err := cleanhttp.DefaultClient().PostForm(tcgApiTokenURL, params)
	if err != nil {
		return "", time.Time{}, err
	}
	defer resp.Body.Close()

	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", time.Time{}, err
	}

	var response struct {
		AccessToken string        `json:"access_token"`
		ExpiresIn   time.Duration `json:"expires_in"`
	}
	err = json.Unmarshal(data, &response)
	if err != nil {
		return "", time.Time{}, err
	}

	expires := time.Now().Add(response.ExpiresIn * time.Second)
	return response.AccessToken, expires, nil
}

func (t *authTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	err := t.limiter.Wait(context.Background())
	if err != nil {
		return nil, err
	}

	if t.publicKey == "" || t.privateKey == "" {
		return nil, fmt.Errorf("missing public or private key")
	}

	// Retrieve the static values
	t.mtx.RLock()
	token := t.token
	expires := t.expires
	t.mtx.RUnlock()

	// If there is a token, make sure it's still valid
	if token != "" || time.Now().After(expires.Add(-1*time.Hour)) {
		// If not valid, ask for generating a new one
		t.mtx.Lock()
		token = ""
		t.mtx.Unlock()
	}

	// Generate a new token
	if token == "" {
		t.mtx.Lock()
		// Only perform this action once, for the routine that got the mutex first
		// The others will just use the updated token immediately after
		if token == t.token {
			t.token, t.expires, err = t.requestToken()
		}
		token = t.token
		t.mtx.Unlock()
		// If anything fails
		if err != nil {
			return nil, err
		}
	}

	req.Header.Add("Authorization", fmt.Sprintf("Bearer %s", token))
	return t.parent.RoundTrip(req)
}

type BaseResponse struct {
	TotalItems int             `json:"totalItems"`
	Success    bool            `json:"success"`
	Errors     []string        `json:"errors"`
	Results    json.RawMessage `json:"results"`
}

// Perform an authenticated GET request and partially parse the response
func (tcg *Client) GetRequest(link string) (*BaseResponse, error) {
	resp, err := tcg.client.Get(link)
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
		return nil, fmt.Errorf("%s: %s", err.Error(), string(data))
	}
	// Return error details only if the request fully failed
	// Otherwise return as much as possible to the callee
	if resp.StatusCode/200 != 1 && len(response.Errors) > 0 {
		return nil, fmt.Errorf(strings.Join(response.Errors, " "))
	}

	return &response, nil
}

func (tcg *Client) TotalProducts(category int, productTypes []string) (int, error) {
	return tcg.queryTotal(tcgApiCatalogProductsURL, category, productTypes)
}

func (tcg *Client) TotalGroups(category int) (int, error) {
	return tcg.queryTotal(tcgApiCatalogGroupsURL, category, nil)
}

func (tcg *Client) TotalCategories(category int) (int, error) {
	return tcg.queryTotal(tcgApiCatalogCategoriesURL, category, nil)
}

// Retrieve how many items a full call will be
func (tcg *Client) queryTotal(link string, category int, productTypes []string) (int, error) {
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

	response, err := tcg.GetRequest(u.String())
	if err != nil {
		return 0, err
	}
	return response.TotalItems, nil
}

type Printing struct {
	PrintingId   int    `json:"printingId"`
	Name         string `json:"name"`
	DisplayOrder int    `json:"displayOrder"`
	ModifiedOn   string `json:"modifiedOn`
}

func (tcg *Client) ListCategoryPrintings(category int) ([]Printing, error) {
	resp, err := tcg.GetRequest(fmt.Sprintf("%s/%d/printings", tcgApiCatalogCategoriesURL, category))
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

func (tcg *Client) GetProductsDetails(productIds []int, includeSkus bool) ([]Product, error) {
	if len(productIds) > MaxIdsInRequest {
		return nil, errors.New("too many ids in request")
	}

	ids := ints2strings(productIds)
	link := tcgApiCatalogProductsURL + "/" + strings.Join(ids, ",")

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

	resp, err := tcg.GetRequest(u.String())
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

func (tcg *Client) ListAllProducts(category int, productTypes []string, includeSkus bool, offset int) ([]Product, error) {
	u, err := url.Parse(tcgApiCatalogProductsURL)
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

	resp, err := tcg.GetRequest(u.String())
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

func (tcg *Client) ListProductSKUs(productId int) ([]SKU, error) {
	link := fmt.Sprintf("%s/product/%d/skus", tcgApiCatalogProductsURL, productId)
	resp, err := tcg.GetRequest(link)
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

func (tcg *Client) ListAllCategoryGroups(category, offset int) ([]Group, error) {
	u, err := url.Parse(tcgApiCatalogGroupsURL)
	if err != nil {
		return nil, err
	}
	v := url.Values{}
	v.Set("categoryId", fmt.Sprint(category))
	v.Set("offset", fmt.Sprint(offset))
	v.Set("limit", fmt.Sprint(MaxItemsInResponse))
	u.RawQuery = v.Encode()

	resp, err := tcg.GetRequest(u.String())
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

func (tcg *Client) GetCategoriesDetails(categoryIds []int) ([]Category, error) {
	if len(categoryIds) > MaxIdsInRequest {
		return nil, errors.New("too many ids in request")
	}

	ids := ints2strings(categoryIds)
	link := tcgApiCatalogCategoriesURL + "/" + strings.Join(ids, ",")

	resp, err := tcg.GetRequest(link)
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
		out = append(out, fmt.Sprintf("%d", ids[i]))
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

func (tcg *Client) GetMarketPricesByProducts(productIds []int) ([]ProductPriceSet, error) {
	if len(productIds) > MaxIdsInRequest {
		return nil, errors.New("too many ids in request")
	}

	ids := ints2strings(productIds)
	link := tcgApiPricingProductURL + "/" + strings.Join(ids, ",")

	resp, err := tcg.GetRequest(link)
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

func (tcg *Client) GetMarketPricesBySKUs(skuIds []int) ([]SKUPriceSet, error) {
	if len(skuIds) > MaxIdsInRequest {
		return nil, errors.New("too many ids in request")
	}

	ids := ints2strings(skuIds)
	link := tcgApiPricingSkuURL + "/" + strings.Join(ids, ",")

	resp, err := tcg.GetRequest(link)
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
