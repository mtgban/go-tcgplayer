package tcgplayer

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// newTestClient points all endpoint URLs at the given handler and returns a
// client wired to it. Tests using it cannot run in parallel since the
// endpoint URLs are package-level variables.
func newTestClient(t *testing.T, handler http.Handler) *Client {
	t.Helper()

	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)

	saved := []struct {
		v    *string
		orig string
	}{
		{&TokenURL, TokenURL},
		{&CatalogCategoriesURL, CatalogCategoriesURL},
		{&CatalogProductsURL, CatalogProductsURL},
		{&CatalogGroupsURL, CatalogGroupsURL},
		{&PricingProductURL, PricingProductURL},
		{&PricingSKUURL, PricingSKUURL},
	}
	t.Cleanup(func() {
		for _, s := range saved {
			*s.v = s.orig
		}
	})
	TokenURL = srv.URL + "/token"
	CatalogCategoriesURL = srv.URL + "/catalog/categories"
	CatalogProductsURL = srv.URL + "/catalog/products"
	CatalogGroupsURL = srv.URL + "/catalog/groups"
	PricingProductURL = srv.URL + "/pricing/product"
	PricingSKUURL = srv.URL + "/pricing/sku"

	tcg, err := NewClient("test-public", "test-private")
	if err != nil {
		t.Fatal(err)
	}
	// Keep failure tests fast
	tcg.client.RetryMax = 0
	tcg.client.HTTPClient.Transport.(*authTransport).tokenClient.RetryMax = 0
	return tcg
}

// Fixtures are written as the json the API sends, rather than encoded from
// the types under test. Encoding our own types would round trip through the
// same struct tags being tested, so a wrong tag would still decode back to
// the value it was written from and the test would pass.

func writeToken(w http.ResponseWriter, expiresIn int64) {
	fmt.Fprintf(w, `{"access_token": "test-token", "token_type": "bearer", "expires_in": %d}`, expiresIn)
}

// writeEnvelope wraps results, itself raw json, in the response envelope.
func writeEnvelope(w http.ResponseWriter, totalItems int, results string) {
	fmt.Fprintf(w, `{"totalItems": %d, "success": true, "errors": [], "results": %s}`, totalItems, results)
}

func TestProductTypesPerCategory(t *testing.T) {
	// A category naming its types for itself, where asking for "Cards"
	// would match nothing at all
	if got := SinglesProductTypes(CategoryDragonBallSuper); !reflect.DeepEqual(got, []string{"Dragon Ball Super Singles"}) {
		t.Errorf("SinglesProductTypes(dragon ball super) = %q, want [Dragon Ball Super Singles]", got)
	}
	// The types Magic's list does not name, and used to lose
	sealed := SealedProductTypes(CategoryYuGiOh)
	for _, want := range []string{"Tin", "YGO Start Decks"} {
		if !slices.Contains(sealed, want) {
			t.Errorf("SealedProductTypes(yugioh) = %q, want it to hold %q", sealed, want)
		}
	}
	if slices.Contains(sealed, "Cards") {
		t.Errorf("SealedProductTypes(yugioh) = %q, want no singles type in it", sealed)
	}
	// Singles and sealed have to partition the category's types
	for _, category := range []int{CategoryMagic, CategoryYuGiOh, CategoryLorcana, CategoryDragonBallSuper} {
		got := len(SinglesProductTypes(category)) + len(SealedProductTypes(category))
		if want := len(ProductTypes(category)); got != want {
			t.Errorf("category %d: singles+sealed = %d types, want %d", category, got, want)
		}
	}
	// An unlisted category falls back to every known name, which a caller
	// counting its results will find comes up short rather than silently
	// dumping nothing
	if got := ProductTypes(-1); !reflect.DeepEqual(got, AllProductTypes) {
		t.Errorf("ProductTypes(unlisted) = %q, want every known type", got)
	}
}

func TestNewClientMissingKeys(t *testing.T) {
	if _, err := NewClient("", "private"); err == nil {
		t.Error("expected error for missing public key")
	}
	if _, err := NewClient("public", ""); err == nil {
		t.Error("expected error for missing private key")
	}
}

func TestTokenFetchedOnceForConcurrentRequests(t *testing.T) {
	var tokenRequests atomic.Int64

	mux := http.NewServeMux()
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		tokenRequests.Add(1)
		if err := r.ParseForm(); err != nil {
			t.Error(err)
		}
		if got := r.PostForm.Get("client_id"); got != "test-public" {
			t.Errorf("client_id = %q, want %q", got, "test-public")
		}
		if got := r.PostForm.Get("grant_type"); got != "client_credentials" {
			t.Errorf("grant_type = %q, want %q", got, "client_credentials")
		}
		writeToken(w, 86400)
	})
	mux.HandleFunc("/catalog/products", func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer test-token" {
			t.Errorf("Authorization = %q, want %q", got, "Bearer test-token")
		}
		writeEnvelope(w, 0, `[]`)
	})

	tcg := newTestClient(t, mux)

	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Go(func() {
			if _, err := tcg.Get(context.Background(), CatalogProductsURL); err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()

	if got := tokenRequests.Load(); got != 1 {
		t.Errorf("token requested %d times, want 1", got)
	}
}

func TestTokenRefreshedNearExpiry(t *testing.T) {
	var tokenRequests atomic.Int64

	mux := http.NewServeMux()
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		tokenRequests.Add(1)
		// Good for another minute, which is inside the 5 minute refresh
		// buffer: a token that has not expired yet must still be replaced
		writeToken(w, 60)
	})
	mux.HandleFunc("/catalog/products", func(w http.ResponseWriter, r *http.Request) {
		writeEnvelope(w, 0, `[]`)
	})

	tcg := newTestClient(t, mux)

	for i := 0; i < 2; i++ {
		if _, err := tcg.Get(context.Background(), CatalogProductsURL); err != nil {
			t.Fatal(err)
		}
	}

	if got := tokenRequests.Load(); got != 2 {
		t.Errorf("token requested %d times, want 2", got)
	}
}

func TestTokenReusedUntilExpiry(t *testing.T) {
	var tokenRequests atomic.Int64

	mux := http.NewServeMux()
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		tokenRequests.Add(1)
		writeToken(w, 86400)
	})
	mux.HandleFunc("/catalog/products", func(w http.ResponseWriter, r *http.Request) {
		writeEnvelope(w, 0, `[]`)
	})

	tcg := newTestClient(t, mux)

	// Sequential, so the singleflight cannot be what collapses these into
	// one fetch: a token good for a day has to be reused on its own.
	for i := 0; i < 3; i++ {
		if _, err := tcg.Get(context.Background(), CatalogProductsURL); err != nil {
			t.Fatal(err)
		}
	}

	if got := tokenRequests.Load(); got != 1 {
		t.Errorf("token requested %d times, want 1", got)
	}
}

func TestBadCredentialsAreNotRetried(t *testing.T) {
	var tokenRequests atomic.Int64

	mux := http.NewServeMux()
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		tokenRequests.Add(1)
		w.WriteHeader(http.StatusUnauthorized)
		fmt.Fprint(w, `{"error":"invalid_client"}`)
	})

	tcg := newTestClient(t, mux)
	// Restore outer retries to prove token failures bypass them
	tcg.client.RetryMax = 2
	tcg.client.RetryWaitMin = time.Millisecond
	tcg.client.RetryWaitMax = time.Millisecond

	_, err := tcg.Get(context.Background(), CatalogProductsURL)
	if err == nil {
		t.Fatal("expected error with bad credentials")
	}
	if !strings.Contains(err.Error(), "token http 401") {
		t.Errorf("error = %q, want it to mention token http 401", err.Error())
	}
	if got := tokenRequests.Load(); got != 1 {
		t.Errorf("token requested %d times, want 1", got)
	}
}

func TestGetErrorFromEnvelope(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		writeToken(w, 86400)
	})
	mux.HandleFunc("/catalog/products", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		fmt.Fprint(w, `{"success": false, "errors": ["No products were found.", "100% failure"]}`)
	})

	tcg := newTestClient(t, mux)

	_, err := tcg.Get(context.Background(), CatalogProductsURL)
	if err == nil {
		t.Fatal("expected error on non-2xx response")
	}
	want := "No products were found. 100% failure"
	if err.Error() != want {
		t.Errorf("error = %q, want %q", err.Error(), want)
	}
}

func TestGetErrorWithEmptyEnvelope(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		writeToken(w, 86400)
	})
	mux.HandleFunc("/catalog/products", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		fmt.Fprint(w, `{}`)
	})

	tcg := newTestClient(t, mux)

	_, err := tcg.Get(context.Background(), CatalogProductsURL)
	if err == nil {
		t.Fatal("expected error on non-2xx response with empty errors")
	}
	if !strings.Contains(err.Error(), "http 403") {
		t.Errorf("error = %q, want it to mention http 403", err.Error())
	}
}

func TestGetProductsDetails(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		writeToken(w, 86400)
	})
	mux.HandleFunc("/catalog/products/12,34", func(w http.ResponseWriter, r *http.Request) {
		if got := r.URL.Query().Get("getExtendedFields"); got != "true" {
			t.Errorf("getExtendedFields = %q, want %q", got, "true")
		}
		if got := r.URL.Query().Get("includeSkus"); got != "true" {
			t.Errorf("includeSkus = %q, want %q", got, "true")
		}
		writeEnvelope(w, 2, `[
			{"productId": 12, "name": "Foo"},
			{"productId": 34, "name": "Bar"}
		]`)
	})

	tcg := newTestClient(t, mux)

	products, err := tcg.GetProductsDetails(context.Background(), []int{12, 34}, true)
	if err != nil {
		t.Fatal(err)
	}
	want := []Product{
		{ProductID: 12, Name: "Foo"},
		{ProductID: 34, Name: "Bar"},
	}
	if !reflect.DeepEqual(products, want) {
		t.Errorf("GetProductsDetails() = %+v, want %+v", products, want)
	}
}

func TestBatchedIdBounds(t *testing.T) {
	// No requests should be issued, the handler always fails
	tcg := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("unexpected request to %s", r.URL)
	}))

	tooMany := make([]int, MaxIDsInRequest+1)

	ctx := context.Background()
	for name, call := range map[string]func([]int) error{
		"GetProductsDetails": func(ids []int) error {
			_, err := tcg.GetProductsDetails(ctx, ids, false)
			return err
		},
		"GetCategoriesDetails": func(ids []int) error {
			_, err := tcg.GetCategoriesDetails(ctx, ids)
			return err
		},
		"GetMarketPricesByProducts": func(ids []int) error {
			_, err := tcg.GetMarketPricesByProducts(ctx, ids)
			return err
		},
		"GetMarketPricesBySKUs": func(ids []int) error {
			_, err := tcg.GetMarketPricesBySKUs(ctx, ids)
			return err
		},
	} {
		if err := call(nil); err == nil {
			t.Errorf("%s: expected error for empty ids", name)
		}
		if err := call(tooMany); err == nil {
			t.Errorf("%s: expected error for more than %d ids", name, MaxIDsInRequest)
		}
	}
}

func TestTotalProducts(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		writeToken(w, 86400)
	})
	mux.HandleFunc("/catalog/products", func(w http.ResponseWriter, r *http.Request) {
		if got := r.URL.Query().Get("categoryId"); got != "3" {
			t.Errorf("categoryId = %q, want %q", got, "3")
		}
		if got := r.URL.Query().Get("productTypes"); got != "Cards" {
			t.Errorf("productTypes = %q, want %q", got, "Cards")
		}
		if got := r.URL.Query().Get("limit"); got != "1" {
			t.Errorf("limit = %q, want %q", got, "1")
		}
		writeEnvelope(w, 4321, `[{"productId": 1}]`)
	})

	tcg := newTestClient(t, mux)

	total, err := tcg.TotalProducts(context.Background(), 3, ProductTypesSingles)
	if err != nil {
		t.Fatal(err)
	}
	if total != 4321 {
		t.Errorf("total = %d, want 4321", total)
	}
}

func TestListCategoryMetadata(t *testing.T) {
	wantConditions := []Condition{
		{ConditionID: 1, Name: "Near Mint", Abbreviation: "NM", DisplayOrder: 1},
		{ConditionID: 2, Name: "Lightly Played", Abbreviation: "LP", DisplayOrder: 2},
	}
	wantLanguages := []Language{
		{LanguageID: 1, Name: "English", Abbreviation: "EN"},
	}
	wantRarities := []Rarity{
		{RarityID: 1, DisplayText: "Mythic", DBValue: "M"},
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		writeToken(w, 86400)
	})
	mux.HandleFunc("/catalog/categories/1/conditions", func(w http.ResponseWriter, r *http.Request) {
		writeEnvelope(w, len(wantConditions), `[
			{"conditionId": 1, "name": "Near Mint", "abbreviation": "NM", "displayOrder": 1},
			{"conditionId": 2, "name": "Lightly Played", "abbreviation": "LP", "displayOrder": 2}
		]`)
	})
	mux.HandleFunc("/catalog/categories/1/languages", func(w http.ResponseWriter, r *http.Request) {
		writeEnvelope(w, len(wantLanguages), `[
			{"languageId": 1, "name": "English", "abbr": "EN"}
		]`)
	})
	mux.HandleFunc("/catalog/categories/1/rarities", func(w http.ResponseWriter, r *http.Request) {
		writeEnvelope(w, len(wantRarities), `[
			{"rarityId": 1, "displayText": "Mythic", "dbValue": "M"}
		]`)
	})

	tcg := newTestClient(t, mux)

	conditions, err := tcg.ListCategoryConditions(context.Background(), 1)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(conditions, wantConditions) {
		t.Errorf("ListCategoryConditions() = %+v, want %+v", conditions, wantConditions)
	}

	languages, err := tcg.ListCategoryLanguages(context.Background(), 1)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(languages, wantLanguages) {
		t.Errorf("ListCategoryLanguages() = %+v, want %+v", languages, wantLanguages)
	}

	rarities, err := tcg.ListCategoryRarities(context.Background(), 1)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(rarities, wantRarities) {
		t.Errorf("ListCategoryRarities() = %+v, want %+v", rarities, wantRarities)
	}
}

func TestTotalCategoriesHasNoCategoryFilter(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		writeToken(w, 86400)
	})
	mux.HandleFunc("/catalog/categories", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Has("categoryId") {
			t.Error("unexpected categoryId filter on categories endpoint")
		}
		writeEnvelope(w, 88, `[{"categoryId": 1}]`)
	})

	tcg := newTestClient(t, mux)

	total, err := tcg.TotalCategories(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if total != 88 {
		t.Errorf("total = %d, want 88", total)
	}
}

func TestInts2Strings(t *testing.T) {
	got := ints2strings([]int{1, 20, 300})
	want := []string{"1", "20", "300"}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("ints2strings = %v, want %v", got, want)
	}
	if out := ints2strings(nil); len(out) != 0 {
		t.Errorf("ints2strings(nil) = %v, want empty", out)
	}
}

// categoriesWithoutProductTypes are the categories ProductTypesByCategory
// leaves out, and why. A category in neither this nor the map is one nobody
// decided about, which is how Palworld and Cyberpunk came to be dumped
// against every product type name the platform has rather than their own two.
var categoriesWithoutProductTypes = map[int]string{
	// The platform serves products for these, filed under type names it
	// does not report through the catalog or the search facets. Dumping one
	// would come up short, and the count check would say so.
	CategoryAxisAllies:               "product type names unknown",
	CategoryBoardgames:               "product type names unknown",
	CategoryStarWarsMiniatures:       "product type names unknown",
	CategoryOrganizersStores:         "product type names unknown",
	CategoryWarhammerBooks:           "product type names unknown",
	CategoryWarhammerBigBoxGames:     "product type names unknown",
	CategoryWarhammerBoxSets:         "product type names unknown",
	CategoryWarhammerClampacks:       "product type names unknown",
	CategoryCitadelPaints:            "product type names unknown",
	CategoryCitadelTools:             "product type names unknown",
	CategoryWarhammerGameAccessories: "product type names unknown",

	// The platform lists these but serves no product under them, so there
	// is no vocabulary to record.
	CategoryMonsterpocalypse:          "serves no products",
	CategoryRedakai:                   "serves no products",
	CategoryWorldOfWarcraftMiniatures: "serves no products",
	CategorySupplies:                  "serves no products",
	21:                                "My Little Pony, serves no products",
	CategoryArchitect:                 "serves no products",
	CategoryMarvelComics:              "serves no products",
	CategoryDCComics:                  "serves no products",
	CategoryNeopetsBattledome:         "serves no products",
}

// TestEveryCategoryIsAccountedFor is the guard on adding a category: name one
// without saying what it files products under, here or in the map, and this
// fails rather than leaving ProductTypes to fall back silently.
func TestEveryCategoryIsAccountedFor(t *testing.T) {
	for id := 1; id < categoryCount; id++ {
		_, mapped := ProductTypesByCategory[id]
		reason, excused := categoriesWithoutProductTypes[id]
		switch {
		case mapped && excused:
			t.Errorf("category %d is in ProductTypesByCategory and also excused as %q, want one or the other", id, reason)
		case !mapped && !excused:
			t.Errorf("category %d names no product types: add them to ProductTypesByCategory, "+
				"or say in categoriesWithoutProductTypes why it has none", id)
		}
	}
	for id := range ProductTypesByCategory {
		if id < 1 || id >= categoryCount {
			t.Errorf("ProductTypesByCategory holds %d, which is not a category", id)
		}
	}
	for id := range categoriesWithoutProductTypes {
		if id < 1 || id >= categoryCount {
			t.Errorf("categoriesWithoutProductTypes holds %d, which is not a category", id)
		}
	}
}

// TestProductTypesByCategoryIsWellFormed catches an entry that would query a
// name the platform never answers to, which returns nothing and reads exactly
// like a category that simply has none of that type.
func TestProductTypesByCategoryIsWellFormed(t *testing.T) {
	if !slices.IsSorted(AllProductTypes) {
		t.Error("AllProductTypes is not sorted, so entries below cannot be checked against it by eye")
	}
	for id, types := range ProductTypesByCategory {
		if len(types) == 0 {
			t.Errorf("category %d maps to no product types; excuse it in categoriesWithoutProductTypes instead", id)
		}
		if !slices.IsSorted(types) {
			t.Errorf("category %d: product types are not sorted: %q", id, types)
		}
		for _, productType := range types {
			if !slices.Contains(AllProductTypes, productType) {
				t.Errorf("category %d names product type %q, which AllProductTypes does not list", id, productType)
			}
		}
	}
}

// TestTotalOfNothingIsZero covers the API answering an empty result set with
// a not-found: for a count, that is the answer and not a failure.
func TestTotalOfNothingIsZero(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		writeToken(w, 86400)
	})
	mux.HandleFunc("/catalog/products", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		fmt.Fprint(w, `{"totalItems": 0, "success": false, "errors": ["No products were found."], "results": []}`)
	})

	tcg := newTestClient(t, mux)

	total, err := tcg.TotalProducts(context.Background(), 3, []string{"Tin"})
	if err != nil {
		t.Fatalf("TotalProducts() error = %v, want nil", err)
	}
	if total != 0 {
		t.Errorf("TotalProducts() = %d, want 0", total)
	}
}

func TestRoundTripLeavesTheRequestAlone(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		writeToken(w, 86400)
	})
	mux.HandleFunc("/catalog/products", func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer test-token" {
			t.Errorf("Authorization = %q, want %q", got, "Bearer test-token")
		}
		writeEnvelope(w, 0, `[]`)
	})

	tcg := newTestClient(t, mux)

	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, CatalogProductsURL, nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := tcg.client.HTTPClient.Transport.RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if got := req.Header.Get("Authorization"); got != "" {
		t.Errorf("caller's request carries Authorization = %q after RoundTrip, want none", got)
	}
}

// extendedField is the element type of Product.ExtendedData, which is an
// unnamed struct; an alias lets the tests below spell it.
type extendedField = struct {
	Name        string `json:"name"`
	DisplayName string `json:"displayName"`
	Value       string `json:"value"`
}

// The fixtures from here on set every field of the type they decode into, each
// to a value no other field in the object shares, so a tag naming the wrong
// field fails as surely as a tag naming none.

func TestListAllProducts(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		writeToken(w, 86400)
	})
	mux.HandleFunc("/catalog/products", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		for key, want := range map[string]string{
			"getExtendedFields": "true",
			"categoryId":        "2",
			"productTypes":      "Cards,Tin",
			"includeSkus":       "true",
			"offset":            "200",
			"limit":             "100",
		} {
			if got := q.Get(key); got != want {
				t.Errorf("%s = %q, want %q", key, got, want)
			}
		}
		writeEnvelope(w, 201, `[{
			"productId": 101, "name": "Dark Magician", "cleanName": "Dark Magician Clean",
			"imageUrl": "https://example.com/101.jpg", "groupId": 10, "categoryId": 2,
			"url": "https://example.com/product/101", "modifiedOn": "2026-01-05T00:00:00",
			"imageCount": 3,
			"presaleInfo": {"isPresale": true, "releasedOn": "2026-10-23T00:00:00", "note": "Details may change"},
			"extendedData": [
				{"name": "Number", "displayName": "Card Number", "value": "LOB-005"},
				{"name": "Rarity", "displayName": "Rarity", "value": "Ultra Rare"}
			],
			"skus": [{"skuId": 1010, "productId": 101, "languageId": 1, "printingId": 2, "conditionId": 3}]
		}]`)
	})

	tcg := newTestClient(t, mux)

	products, err := tcg.ListAllProducts(context.Background(), 2, []string{"Cards", "Tin"}, true, 200)
	if err != nil {
		t.Fatal(err)
	}
	want := []Product{{
		ProductID:  101,
		Name:       "Dark Magician",
		CleanName:  "Dark Magician Clean",
		ImageURL:   "https://example.com/101.jpg",
		GroupID:    10,
		CategoryID: 2,
		URL:        "https://example.com/product/101",
		ModifiedOn: "2026-01-05T00:00:00",
		ImageCount: 3,
		PresaleInfo: &PresaleInfo{
			IsPresale:  true,
			ReleasedOn: "2026-10-23T00:00:00",
			Note:       "Details may change",
		},
		Skus: []SKU{{SKUID: 1010, ProductID: 101, LanguageID: 1, PrintingID: 2, ConditionID: 3}},
		ExtendedData: []extendedField{
			{Name: "Number", DisplayName: "Card Number", Value: "LOB-005"},
			{Name: "Rarity", DisplayName: "Rarity", Value: "Ultra Rare"},
		},
	}}
	if !reflect.DeepEqual(products, want) {
		t.Errorf("ListAllProducts() = %+v, want %+v", products, want)
	}
}

func TestListProductSKUs(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		writeToken(w, 86400)
	})
	mux.HandleFunc("/catalog/products/101/skus", func(w http.ResponseWriter, r *http.Request) {
		writeEnvelope(w, 2, `[
			{"skuId": 1010, "productId": 101, "languageId": 1, "printingId": 2, "conditionId": 3},
			{"skuId": 1011, "productId": 101, "languageId": 4, "printingId": 5, "conditionId": 6}
		]`)
	})

	tcg := newTestClient(t, mux)

	skus, err := tcg.ListProductSKUs(context.Background(), 101)
	if err != nil {
		t.Fatal(err)
	}
	want := []SKU{
		{SKUID: 1010, ProductID: 101, LanguageID: 1, PrintingID: 2, ConditionID: 3},
		{SKUID: 1011, ProductID: 101, LanguageID: 4, PrintingID: 5, ConditionID: 6},
	}
	if !reflect.DeepEqual(skus, want) {
		t.Errorf("ListProductSKUs() = %+v, want %+v", skus, want)
	}
}

func TestListAllCategoryGroups(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		writeToken(w, 86400)
	})
	mux.HandleFunc("/catalog/groups", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		for key, want := range map[string]string{"categoryId": "2", "offset": "100", "limit": "100"} {
			if got := q.Get(key); got != want {
				t.Errorf("%s = %q, want %q", key, got, want)
			}
		}
		writeEnvelope(w, 101, `[{
			"groupId": 10, "name": "Legend of Blue Eyes White Dragon", "abbreviation": "LOB",
			"isSupplemental": true, "publishedOn": "2002-03-08T00:00:00",
			"modifiedOn": "2026-01-04T00:00:00", "categoryId": 2
		}]`)
	})

	tcg := newTestClient(t, mux)

	groups, err := tcg.ListAllCategoryGroups(context.Background(), 2, 100)
	if err != nil {
		t.Fatal(err)
	}
	want := []Group{{
		GroupID:      10,
		Name:         "Legend of Blue Eyes White Dragon",
		Abbreviation: "LOB",
		Supplemental: true,
		PublishedOn:  "2002-03-08T00:00:00",
		ModifiedOn:   "2026-01-04T00:00:00",
		CategoryID:   2,
	}}
	if !reflect.DeepEqual(groups, want) {
		t.Errorf("ListAllCategoryGroups() = %+v, want %+v", groups, want)
	}
}

func TestGetCategoriesDetails(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		writeToken(w, 86400)
	})
	mux.HandleFunc("/catalog/categories/2", func(w http.ResponseWriter, r *http.Request) {
		writeEnvelope(w, 1, `[{
			"categoryId": 2, "name": "YuGiOh", "modifiedOn": "2026-01-01T00:00:00",
			"displayName": "YuGiOh Display", "seoCategoryName": "yugioh-seo",
			"sealedLabel": "Sealed Label", "nonSealedLabel": "Singles Label",
			"conditionGuideUrl": "https://example.com/guide", "isScannable": true, "popularity": 42,
			"isDirect": true, "categoryDescription": "Duel with the best", "categoryPageTitle": "Buy YuGiOh"
		}]`)
	})

	tcg := newTestClient(t, mux)

	categories, err := tcg.GetCategoriesDetails(context.Background(), []int{2})
	if err != nil {
		t.Fatal(err)
	}
	want := []Category{{
		CategoryID:        2,
		Name:              "YuGiOh",
		ModifiedOn:        "2026-01-01T00:00:00",
		DisplayName:       "YuGiOh Display",
		SeoCategoryName:   "yugioh-seo",
		SealedLabel:       "Sealed Label",
		NonSealedLabel:    "Singles Label",
		ConditionGuideURL: "https://example.com/guide",
		IsScannable:       true,
		Popularity:        42,
		IsDirect:          true,

		CategoryDescription: "Duel with the best",
		CategoryPageTitle:   "Buy YuGiOh",
	}}
	if !reflect.DeepEqual(categories, want) {
		t.Errorf("GetCategoriesDetails() = %+v, want %+v", categories, want)
	}
}

func TestListCategoryPrintings(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		writeToken(w, 86400)
	})
	mux.HandleFunc("/catalog/categories/2/printings", func(w http.ResponseWriter, r *http.Request) {
		writeEnvelope(w, 2, `[
			{"printingId": 7, "name": "1st Edition", "displayOrder": 1, "modifiedOn": "2026-01-02T00:00:00"},
			{"printingId": 8, "name": "Unlimited", "displayOrder": 2, "modifiedOn": "2026-01-03T00:00:00"}
		]`)
	})

	tcg := newTestClient(t, mux)

	printings, err := tcg.ListCategoryPrintings(context.Background(), 2)
	if err != nil {
		t.Fatal(err)
	}
	want := []Printing{
		{PrintingID: 7, Name: "1st Edition", DisplayOrder: 1, ModifiedOn: "2026-01-02T00:00:00"},
		{PrintingID: 8, Name: "Unlimited", DisplayOrder: 2, ModifiedOn: "2026-01-03T00:00:00"},
	}
	if !reflect.DeepEqual(printings, want) {
		t.Errorf("ListCategoryPrintings() = %+v, want %+v", printings, want)
	}
}

func TestGetMarketPricesByProducts(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		writeToken(w, 86400)
	})
	mux.HandleFunc("/pricing/product/101,205", func(w http.ResponseWriter, r *http.Request) {
		writeEnvelope(w, 2, `[
			{"productId": 101, "lowPrice": 1.25, "marketPrice": 2.5, "midPrice": 3.75,
			 "highPrice": 9.5, "directLowPrice": 4.5, "subTypeName": "Normal"},
			{"productId": 205, "lowPrice": 5.25, "marketPrice": 6.5, "midPrice": 7.75,
			 "highPrice": 10.5, "directLowPrice": 8.5, "subTypeName": "Foil"}
		]`)
	})

	tcg := newTestClient(t, mux)

	prices, err := tcg.GetMarketPricesByProducts(context.Background(), []int{101, 205})
	if err != nil {
		t.Fatal(err)
	}
	want := []ProductPriceSet{
		{ProductID: 101, LowPrice: 1.25, MarketPrice: 2.5, MidPrice: 3.75, HighPrice: 9.5, DirectLowPrice: 4.5, SubTypeName: "Normal"},
		{ProductID: 205, LowPrice: 5.25, MarketPrice: 6.5, MidPrice: 7.75, HighPrice: 10.5, DirectLowPrice: 8.5, SubTypeName: "Foil"},
	}
	if !reflect.DeepEqual(prices, want) {
		t.Errorf("GetMarketPricesByProducts() = %+v, want %+v", prices, want)
	}
}

func TestGetMarketPricesBySKUs(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		writeToken(w, 86400)
	})
	mux.HandleFunc("/pricing/sku/1010,2050", func(w http.ResponseWriter, r *http.Request) {
		writeEnvelope(w, 2, `[
			{"skuId": 1010, "lowPrice": 1.5, "lowestShipping": 0.99, "lowestListingPrice": 1.49,
			 "marketPrice": 2.25, "directLowPrice": 1.75},
			{"skuId": 2050, "lowPrice": 3.5, "lowestShipping": 1.99, "lowestListingPrice": 3.49,
			 "marketPrice": 4.25, "directLowPrice": 3.75}
		]`)
	})

	tcg := newTestClient(t, mux)

	prices, err := tcg.GetMarketPricesBySKUs(context.Background(), []int{1010, 2050})
	if err != nil {
		t.Fatal(err)
	}
	want := []SKUPriceSet{
		{SKUID: 1010, LowPrice: 1.5, LowestShipping: 0.99, LowestListingPrice: 1.49, MarketPrice: 2.25, DirectLowPrice: 1.75},
		{SKUID: 2050, LowPrice: 3.5, LowestShipping: 1.99, LowestListingPrice: 3.49, MarketPrice: 4.25, DirectLowPrice: 3.75},
	}
	if !reflect.DeepEqual(prices, want) {
		t.Errorf("GetMarketPricesBySKUs() = %+v, want %+v", prices, want)
	}
}

func TestProductExtended(t *testing.T) {
	var product Product
	err := json.Unmarshal([]byte(`{"productId": 101, "extendedData": [
		{"name": "Number", "displayName": "Card Number", "value": "LOB-005"},
		{"name": "Rarity", "displayName": "Rarity", "value": "Ultra Rare"}
	]}`), &product)
	if err != nil {
		t.Fatal(err)
	}

	for _, tt := range []struct {
		name string
		want string
	}{
		{"Number", "LOB-005"},
		{"Rarity", "Ultra Rare"},
		// Looked up by name, never by the label the storefront shows
		{"Card Number", ""},
		{"Attribute", ""},
	} {
		if got := product.Extended(tt.name); got != tt.want {
			t.Errorf("Extended(%q) = %q, want %q", tt.name, got, tt.want)
		}
	}
	// Sealed products routinely carry no extended data at all
	if got := (Product{}).Extended("Number"); got != "" {
		t.Errorf("Extended(%q) on a product without extended data = %q, want empty", "Number", got)
	}
}

func TestGroupReleaseDate(t *testing.T) {
	for _, tt := range []struct {
		publishedOn string
		want        string
	}{
		{"2002-03-08T00:00:00", "2002-03-08"},
		{"2002-03-08", "2002-03-08"},
		{"", ""},
	} {
		if got := (Group{PublishedOn: tt.publishedOn}).ReleaseDate(); got != tt.want {
			t.Errorf("ReleaseDate() of %q = %q, want %q", tt.publishedOn, got, tt.want)
		}
	}
}

func TestPrintingNames(t *testing.T) {
	// The listing order differs from id order, so an ordering by id shows
	var dump CatalogDump
	err := json.Unmarshal([]byte(`{
		"printings": [
			{"printingId": 3, "name": "Normal"},
			{"printingId": 1, "name": "Foil"},
			{"printingId": 2, "name": "Cold Foil"}
		],
		"products": [
			{"productId": 101, "skus": [{"skuId": 1, "printingId": 1}, {"skuId": 2, "printingId": 3}, {"skuId": 3, "printingId": 1}]},
			{"productId": 102, "skus": [{"skuId": 4, "printingId": 2}]},
			{"productId": 103, "skus": [{"skuId": 5, "printingId": 99}]},
			{"productId": 104}
		]
	}`), &dump)
	if err != nil {
		t.Fatal(err)
	}

	want := map[int][]string{
		101: {"Normal", "Foil"},
		102: {"Cold Foil"},
		103: nil,
		104: nil,
	}
	if got := dump.PrintingNames(); !reflect.DeepEqual(got, want) {
		t.Errorf("PrintingNames() = %v, want %v", got, want)
	}
}

// TestProductTypesForNoneAreEmpty covers the categories with nothing of one
// kind: their list must be empty rather than nil, since nil asks the product
// endpoints for every product.
func TestProductTypesForNoneAreEmpty(t *testing.T) {
	if got := SinglesProductTypes(CategoryKeyForge); got == nil || len(got) != 0 {
		t.Errorf("SinglesProductTypes(keyforge) = %#v, want an empty, non-nil list", got)
	}
	if got := SealedProductTypes(CategoryEpic); got == nil || len(got) != 0 {
		t.Errorf("SealedProductTypes(epic) = %#v, want an empty, non-nil list", got)
	}
	for id := range ProductTypesByCategory {
		if SinglesProductTypes(id) == nil {
			t.Errorf("SinglesProductTypes(%d) = nil, want a non-nil list", id)
		}
		if SealedProductTypes(id) == nil {
			t.Errorf("SealedProductTypes(%d) = nil, want a non-nil list", id)
		}
	}
}

func TestEmptyProductTypeFilterIsRefused(t *testing.T) {
	// No request should be issued, the handler always fails
	tcg := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("unexpected request to %s", r.URL)
	}))

	ctx := context.Background()
	if _, err := tcg.TotalProducts(ctx, CategoryKeyForge, SinglesProductTypes(CategoryKeyForge)); !errors.Is(err, errNoProductTypes) {
		t.Errorf("TotalProducts() with no types error = %v, want %v", err, errNoProductTypes)
	}
	if _, err := tcg.ListAllProducts(ctx, CategoryEpic, SealedProductTypes(CategoryEpic), false, 0); !errors.Is(err, errNoProductTypes) {
		t.Errorf("ListAllProducts() with no types error = %v, want %v", err, errNoProductTypes)
	}
}

// TestNilProductTypeFilterAsksForEverything keeps nil meaning no filter, the
// count the dumper checks every category against.
func TestNilProductTypeFilterAsksForEverything(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		writeToken(w, 86400)
	})
	mux.HandleFunc("/catalog/products", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Has("productTypes") {
			t.Errorf("productTypes = %q, want no filter", r.URL.Query().Get("productTypes"))
		}
		writeEnvelope(w, 4321, `[]`)
	})

	tcg := newTestClient(t, mux)

	total, err := tcg.TotalProducts(context.Background(), CategoryKeyForge, nil)
	if err != nil {
		t.Fatal(err)
	}
	if total != 4321 {
		t.Errorf("TotalProducts() = %d, want 4321", total)
	}
	if _, err := tcg.ListAllProducts(context.Background(), CategoryKeyForge, nil, false, 0); err != nil {
		t.Fatal(err)
	}
}

// rejectedToken is how the API answers a bearer token it does not accept,
// as read off the live API.
const rejectedToken = `{"success":false,"errors":["Missing or invalid bearer token."],"results":[]}`

// tokenSequence serves test-token-1, test-token-2 and so on, one per fetch,
// and reports how many it has served.
func tokenSequence(mux *http.ServeMux) *atomic.Int64 {
	var served atomic.Int64
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		n := served.Add(1)
		fmt.Fprintf(w, `{"access_token": "test-token-%d", "token_type": "bearer", "expires_in": 86400}`, n)
	})
	return &served
}

// TestRejectedTokenIsReplaced covers a token the server stops accepting before
// its expiry, as after a key rotation: it is replaced, and the request goes
// through, rather than failing every call until the token would have expired.
func TestRejectedTokenIsReplaced(t *testing.T) {
	var calls atomic.Int64
	mux := http.NewServeMux()
	tokens := tokenSequence(mux)
	mux.HandleFunc("/catalog/products", func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Header.Get("Authorization") == "Bearer test-token-1" {
			w.WriteHeader(http.StatusUnauthorized)
			fmt.Fprint(w, rejectedToken)
			return
		}
		writeEnvelope(w, 0, `[]`)
	})

	tcg := newTestClient(t, mux)

	if _, err := tcg.Get(context.Background(), CatalogProductsURL); err != nil {
		t.Fatalf("Get() error = %v, want the request to go through on a new token", err)
	}
	if got := tokens.Load(); got != 2 {
		t.Errorf("token requested %d times, want 2", got)
	}
	// The new token is kept
	if _, err := tcg.Get(context.Background(), CatalogProductsURL); err != nil {
		t.Fatal(err)
	}
	if got, want := [2]int64{tokens.Load(), calls.Load()}, [2]int64{2, 3}; got != want {
		t.Errorf("token fetches and API calls = %v, want %v", got, want)
	}
}

// TestSecondRejectionIsTheAnswer keeps a server that rejects every token from
// costing more than one retry.
func TestSecondRejectionIsTheAnswer(t *testing.T) {
	var calls atomic.Int64
	mux := http.NewServeMux()
	tokens := tokenSequence(mux)
	mux.HandleFunc("/catalog/products", func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusUnauthorized)
		fmt.Fprint(w, rejectedToken)
	})

	tcg := newTestClient(t, mux)

	_, err := tcg.Get(context.Background(), CatalogProductsURL)
	want := &APIError{StatusCode: http.StatusUnauthorized, Messages: []string{"Missing or invalid bearer token."}}
	var got *APIError
	if !errors.As(err, &got) || !reflect.DeepEqual(got, want) {
		t.Fatalf("Get() error = %#v, want %#v", err, want)
	}
	if got, want := [2]int64{tokens.Load(), calls.Load()}, [2]int64{2, 2}; got != want {
		t.Errorf("token fetches and API calls = %v, want %v", got, want)
	}
}

// TestConcurrentRejectionsRefreshOnce keeps a burst of requests all rejected
// with the same token from fetching one replacement each.
func TestConcurrentRejectionsRefreshOnce(t *testing.T) {
	mux := http.NewServeMux()
	tokens := tokenSequence(mux)
	mux.HandleFunc("/catalog/products", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") == "Bearer test-token-1" {
			w.WriteHeader(http.StatusUnauthorized)
			fmt.Fprint(w, rejectedToken)
			return
		}
		writeEnvelope(w, 0, `[]`)
	})

	tcg := newTestClient(t, mux)

	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Go(func() {
			if _, err := tcg.Get(context.Background(), CatalogProductsURL); err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()

	if got := tokens.Load(); got != 2 {
		t.Errorf("token requested %d times, want 2", got)
	}
}

// TestCancelledCallerDoesNotFailOthers covers the token fetch every waiting
// request shares: the caller that started it giving up must not take the
// others down with it, and must itself stop waiting.
func TestCancelledCallerDoesNotFailOthers(t *testing.T) {
	var tokens atomic.Int64
	arrived, release := make(chan struct{}), make(chan struct{})
	mux := http.NewServeMux()
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		if tokens.Add(1) == 1 {
			close(arrived)
		}
		<-release
		writeToken(w, 86400)
	})
	mux.HandleFunc("/catalog/products", func(w http.ResponseWriter, r *http.Request) {
		writeEnvelope(w, 0, `[]`)
	})

	tcg := newTestClient(t, mux)
	var once sync.Once
	t.Cleanup(func() { once.Do(func() { close(release) }) })

	ctx, cancel := context.WithCancel(context.Background())
	first := make(chan error, 1)
	go func() {
		_, err := tcg.Get(ctx, CatalogProductsURL)
		first <- err
	}()
	<-arrived

	second := make(chan error, 1)
	go func() {
		_, err := tcg.Get(context.Background(), CatalogProductsURL)
		second <- err
	}()
	// Give the second request time to join the fetch already in flight
	time.Sleep(100 * time.Millisecond)

	cancel()
	select {
	case err := <-first:
		if err == nil {
			t.Error("cancelled Get() error = nil, want its cancellation")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("cancelled Get() still waiting on the token fetch")
	}

	once.Do(func() { close(release) })
	if err := <-second; err != nil {
		t.Errorf("Get() sharing the fetch error = %v, want nil", err)
	}
	if got := tokens.Load(); got != 1 {
		t.Errorf("token requested %d times, want 1", got)
	}
}

// TestRejectedRequestBodyIsSentAgain covers the retry for a request with a
// body: sent again when it can be rebuilt, and left alone when it cannot.
func TestRejectedRequestBodyIsSentAgain(t *testing.T) {
	var bodies []string
	var mtx sync.Mutex
	mux := http.NewServeMux()
	tokenSequence(mux)
	mux.HandleFunc("/catalog/products", func(w http.ResponseWriter, r *http.Request) {
		data, err := io.ReadAll(r.Body)
		if err != nil {
			t.Error(err)
		}
		mtx.Lock()
		bodies = append(bodies, string(data))
		mtx.Unlock()
		if r.Header.Get("Authorization") == "Bearer test-token-1" {
			w.WriteHeader(http.StatusUnauthorized)
			fmt.Fprint(w, rejectedToken)
			return
		}
		writeEnvelope(w, 0, `[]`)
	})

	tcg := newTestClient(t, mux)
	transport := tcg.client.HTTPClient.Transport

	// strings.Reader gives the request a GetBody to rebuild it with
	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, CatalogProductsURL, strings.NewReader("payload"))
	if err != nil {
		t.Fatal(err)
	}
	resp, err := transport.RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || !reflect.DeepEqual(bodies, []string{"payload", "payload"}) {
		t.Errorf("RoundTrip() = %d with bodies %q, want 200 with the body sent twice", resp.StatusCode, bodies)
	}

	// A body with no way to rebuild it gets the rejection back untouched,
	// once test-token-2 is rejected too
	var groupCalls atomic.Int64
	mux.HandleFunc("/catalog/groups", func(w http.ResponseWriter, r *http.Request) {
		groupCalls.Add(1)
		w.WriteHeader(http.StatusUnauthorized)
		fmt.Fprint(w, rejectedToken)
	})
	req, err = http.NewRequestWithContext(context.Background(), http.MethodPost, CatalogGroupsURL, io.NopCloser(strings.NewReader("payload")))
	if err != nil {
		t.Fatal(err)
	}
	resp, err = transport.RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized || groupCalls.Load() != 1 {
		t.Errorf("RoundTrip() without GetBody = %d after %d calls, want the 401 back after 1", resp.StatusCode, groupCalls.Load())
	}
}

// TestLateRejectionReusesTheReplacement covers a request rejected with the
// old token that reaches the fetch only after another request has replaced
// it: the replacement is the answer, not a third token.
func TestLateRejectionReusesTheReplacement(t *testing.T) {
	mux := http.NewServeMux()
	tokens := tokenSequence(mux)
	mux.HandleFunc("/catalog/products", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") == "Bearer test-token-1" {
			w.WriteHeader(http.StatusUnauthorized)
			fmt.Fprint(w, rejectedToken)
			return
		}
		writeEnvelope(w, 0, `[]`)
	})

	tcg := newTestClient(t, mux)
	if _, err := tcg.Get(context.Background(), CatalogProductsURL); err != nil {
		t.Fatal(err)
	}

	transport, ok := tcg.client.HTTPClient.Transport.(*authTransport)
	if !ok {
		t.Fatalf("transport is %T, want *authTransport", tcg.client.HTTPClient.Transport)
	}
	token, err := transport.refreshToken(context.Background(), "test-token-1")
	if err != nil {
		t.Fatal(err)
	}
	if token != "test-token-2" || tokens.Load() != 2 {
		t.Errorf("refreshToken() after a replacement = %q with %d fetches, want %q with 2", token, tokens.Load(), "test-token-2")
	}
}
