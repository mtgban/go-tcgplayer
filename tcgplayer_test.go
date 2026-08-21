package tcgplayer

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
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
		{&TCGAPITokenURL, TCGAPITokenURL},
		{&TCGAPICatalogCategoriesURL, TCGAPICatalogCategoriesURL},
		{&TCGAPICatalogProductsURL, TCGAPICatalogProductsURL},
		{&TCGAPICatalogGroupsURL, TCGAPICatalogGroupsURL},
		{&TCGAPIPricingProductURL, TCGAPIPricingProductURL},
		{&TCGAPIPricingSkuURL, TCGAPIPricingSkuURL},
	}
	t.Cleanup(func() {
		for _, s := range saved {
			*s.v = s.orig
		}
	})
	TCGAPITokenURL = srv.URL + "/token"
	TCGAPICatalogCategoriesURL = srv.URL + "/catalog/categories"
	TCGAPICatalogProductsURL = srv.URL + "/catalog/products"
	TCGAPICatalogGroupsURL = srv.URL + "/catalog/groups"
	TCGAPIPricingProductURL = srv.URL + "/pricing/product"
	TCGAPIPricingSkuURL = srv.URL + "/pricing/sku"

	tcg, err := NewClient("test-public", "test-private")
	if err != nil {
		t.Fatal(err)
	}
	// Keep failure tests fast
	tcg.client.RetryMax = 0
	tcg.client.HTTPClient.Transport.(*authTransport).tokenClient.RetryMax = 0
	return tcg
}

func writeToken(w http.ResponseWriter, expiresIn int64) {
	json.NewEncoder(w).Encode(map[string]any{
		"access_token": "test-token",
		"expires_in":   expiresIn,
	})
}

func writeEnvelope(w http.ResponseWriter, totalItems int, results any) {
	data, _ := json.Marshal(results)
	json.NewEncoder(w).Encode(BaseResponse{
		TotalItems: totalItems,
		Success:    true,
		Results:    data,
	})
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
		writeEnvelope(w, 0, []Product{})
	})

	tcg := newTestClient(t, mux)

	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := tcg.Get(context.Background(), TCGAPICatalogProductsURL); err != nil {
				t.Error(err)
			}
		}()
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
		// Already inside the 5 minute refresh buffer
		writeToken(w, 0)
	})
	mux.HandleFunc("/catalog/products", func(w http.ResponseWriter, r *http.Request) {
		writeEnvelope(w, 0, []Product{})
	})

	tcg := newTestClient(t, mux)

	for i := 0; i < 2; i++ {
		if _, err := tcg.Get(context.Background(), TCGAPICatalogProductsURL); err != nil {
			t.Fatal(err)
		}
	}

	if got := tokenRequests.Load(); got != 2 {
		t.Errorf("token requested %d times, want 2", got)
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

	_, err := tcg.Get(context.Background(), TCGAPICatalogProductsURL)
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
		json.NewEncoder(w).Encode(BaseResponse{
			Errors: []string{"No products were found.", "100% failure"},
		})
	})

	tcg := newTestClient(t, mux)

	_, err := tcg.Get(context.Background(), TCGAPICatalogProductsURL)
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
		json.NewEncoder(w).Encode(BaseResponse{})
	})

	tcg := newTestClient(t, mux)

	_, err := tcg.Get(context.Background(), TCGAPICatalogProductsURL)
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
		writeEnvelope(w, 2, []Product{
			{ProductID: 12, Name: "Foo"},
			{ProductID: 34, Name: "Bar"},
		})
	})

	tcg := newTestClient(t, mux)

	products, err := tcg.GetProductsDetails(context.Background(), []int{12, 34}, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(products) != 2 {
		t.Fatalf("got %d products, want 2", len(products))
	}
	if products[0].ProductID != 12 || products[0].Name != "Foo" {
		t.Errorf("products[0] = %+v", products[0])
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
		writeEnvelope(w, 4321, []Product{{ProductID: 1}})
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
	mux := http.NewServeMux()
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		writeToken(w, 86400)
	})
	mux.HandleFunc("/catalog/categories/1/conditions", func(w http.ResponseWriter, r *http.Request) {
		writeEnvelope(w, 2, []Condition{
			{ConditionID: 1, Name: "Near Mint", Abbreviation: "NM", DisplayOrder: 1},
			{ConditionID: 2, Name: "Lightly Played", Abbreviation: "LP", DisplayOrder: 2},
		})
	})
	mux.HandleFunc("/catalog/categories/1/languages", func(w http.ResponseWriter, r *http.Request) {
		writeEnvelope(w, 1, []Language{
			{LanguageID: 1, Name: "English", Abbreviation: "EN"},
		})
	})
	mux.HandleFunc("/catalog/categories/1/rarities", func(w http.ResponseWriter, r *http.Request) {
		writeEnvelope(w, 1, []Rarity{
			{RarityID: 1, DisplayText: "Mythic", DBValue: "M"},
		})
	})

	tcg := newTestClient(t, mux)

	conditions, err := tcg.ListCategoryConditions(context.Background(), 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(conditions) != 2 || conditions[0].Abbreviation != "NM" {
		t.Errorf("conditions = %+v", conditions)
	}

	languages, err := tcg.ListCategoryLanguages(context.Background(), 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(languages) != 1 || languages[0].Abbreviation != "EN" {
		t.Errorf("languages = %+v", languages)
	}

	rarities, err := tcg.ListCategoryRarities(context.Background(), 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(rarities) != 1 || rarities[0].DBValue != "M" {
		t.Errorf("rarities = %+v", rarities)
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
		writeEnvelope(w, 88, []Category{{CategoryID: 1}})
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
