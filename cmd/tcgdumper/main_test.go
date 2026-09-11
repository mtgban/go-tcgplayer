package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"testing"

	"github.com/mtgban/go-tcgplayer"
)

// serveCatalog stands in for the API. byType holds the products the catalog
// files under each type, and categoryTotal is how many products the category
// holds in total, which is larger than they sum to when some of them are
// filed under a type the dumper never asks for.
func serveCatalog(t *testing.T, byType map[string][]int, categoryTotal, dropFromPages int) {
	t.Helper()

	envelope := func(w http.ResponseWriter, total int, results string) {
		fmt.Fprintf(w, `{"totalItems": %d, "success": true, "errors": [], "results": %s}`, total, results)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"access_token": "t", "token_type": "bearer", "expires_in": 86400}`)
	})
	mux.HandleFunc("/catalog/categories/1", func(w http.ResponseWriter, r *http.Request) {
		envelope(w, 1, `[{"categoryId": 1, "name": "Test"}]`)
	})
	mux.HandleFunc("/catalog/categories/1/conditions", func(w http.ResponseWriter, r *http.Request) {
		envelope(w, 1, `[{"conditionId": 1, "name": "Near Mint", "abbreviation": "NM", "displayOrder": 1}]`)
	})
	mux.HandleFunc("/catalog/categories/1/languages", func(w http.ResponseWriter, r *http.Request) {
		envelope(w, 1, `[{"languageId": 1, "name": "English", "abbr": "EN"}]`)
	})
	mux.HandleFunc("/catalog/categories/1/printings", func(w http.ResponseWriter, r *http.Request) {
		envelope(w, 1, `[{"printingId": 1, "name": "Normal", "displayOrder": 1}]`)
	})
	mux.HandleFunc("/catalog/categories/1/rarities", func(w http.ResponseWriter, r *http.Request) {
		envelope(w, 1, `[{"rarityId": 1, "displayText": "Common", "dbValue": "C"}]`)
	})
	mux.HandleFunc("/catalog/groups", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("limit") == "1" {
			envelope(w, 1, `[{"groupId": 10, "name": "Set One"}]`)
			return
		}
		envelope(w, 1, `[{"groupId": 10, "name": "Set One", "categoryId": 1}]`)
	})
	mux.HandleFunc("/catalog/products", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		productType := q.Get("productTypes")

		// limit=1 marks the count queries the dumper opens with
		if q.Get("limit") == "1" {
			if productType == "" {
				envelope(w, categoryTotal, `[]`)
				return
			}
			envelope(w, len(byType[productType]), `[]`)
			return
		}

		offset, _ := strconv.Atoi(q.Get("offset"))
		ids := byType[productType]
		// Still counted, simply not handed over: a page answering short
		// without an error to go with it
		served := len(ids) - dropFromPages
		var items []string
		for i := offset; i < served && i < offset+tcgplayer.MaxItemsInResponse; i++ {
			items = append(items, fmt.Sprintf(
				`{"productId": %d, "name": "P%d", "groupId": 10, "skus": [{"skuId": %d, "productId": %d, "languageId": 1, "printingId": 1, "conditionId": 1}]}`,
				ids[i], ids[i], ids[i]*10, ids[i]))
		}
		envelope(w, len(ids), "["+strings.Join(items, ",")+"]")
	})

	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	saved := []struct {
		v    *string
		orig string
	}{
		{&tcgplayer.TokenURL, tcgplayer.TokenURL},
		{&tcgplayer.CatalogCategoriesURL, tcgplayer.CatalogCategoriesURL},
		{&tcgplayer.CatalogProductsURL, tcgplayer.CatalogProductsURL},
		{&tcgplayer.CatalogGroupsURL, tcgplayer.CatalogGroupsURL},
	}
	t.Cleanup(func() {
		for _, s := range saved {
			*s.v = s.orig
		}
	})
	tcgplayer.TokenURL = srv.URL + "/token"
	tcgplayer.CatalogCategoriesURL = srv.URL + "/catalog/categories"
	tcgplayer.CatalogProductsURL = srv.URL + "/catalog/products"
	tcgplayer.CatalogGroupsURL = srv.URL + "/catalog/groups"
}

// runDumper runs the program against the stubbed API, returning its exit
// code and what it wrote to stderr.
func runDumperOutput(t *testing.T, args ...string) (int, string, []byte) {
	t.Helper()

	savedFlags, savedArgs := flag.CommandLine, os.Args
	t.Cleanup(func() { flag.CommandLine, os.Args = savedFlags, savedArgs })
	flag.CommandLine = flag.NewFlagSet("tcgdumper", flag.ContinueOnError)
	os.Args = append([]string{"tcgdumper", "-category", "1", "-thread", "2", "-pub", "k", "-pri", "k"}, args...)

	outFile, err := os.CreateTemp(t.TempDir(), "dump")
	if err != nil {
		t.Fatal(err)
	}
	errFile, err := os.CreateTemp(t.TempDir(), "log")
	if err != nil {
		t.Fatal(err)
	}
	defer outFile.Close()
	defer errFile.Close()
	stdout, stderr := os.Stdout, os.Stderr
	defer func() { os.Stdout, os.Stderr = stdout, stderr }()
	os.Stdout, os.Stderr = outFile, errFile
	code := run()
	os.Stdout, os.Stderr = stdout, stderr

	logged, err := os.ReadFile(errFile.Name())
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(outFile.Name())
	if err != nil {
		t.Fatal(err)
	}
	if code != 0 && len(data) != 0 {
		t.Error("failed dump wrote JSON")
	}
	return code, string(logged), data
}

func TestDumpsEveryProduct(t *testing.T) {
	serveCatalog(t, map[string][]int{"Cards": {1, 2, 3}}, 3, 0)

	code, logged := runDumper(t)
	if code != 0 {
		t.Errorf("exit = %d, want 0\n%s", code, logged)
	}
}

// A product type the catalog uses but the category's list does not name is
// never queried, so its products are missing from the dump with nothing in
// the output to show for it. Only the unfiltered count can see them.
func TestProductTypeOutsideTheKnownListFailsTheDump(t *testing.T) {
	serveCatalog(t, map[string][]int{"Cards": {1, 2, 3}}, 5, 0)

	code, logged := runDumper(t)
	if code == 0 {
		t.Errorf("exit = 0, want non-zero: two products were never fetched\n%s", logged)
	}
	if !strings.Contains(logged, "missing from ProductTypesByCategory") {
		t.Errorf("stderr does not name the cause:\n%s", logged)
	}
}

// A page that answers short without reporting an error drops products with
// no failed page to count.
func TestShortPageFailsTheDump(t *testing.T) {
	// The count promises three products, the page hands back two
	serveCatalog(t, map[string][]int{"Cards": {1, 2, 3}}, 3, 1)

	code, logged := runDumper(t)
	if code == 0 {
		t.Errorf("exit = 0, want non-zero: a product was never handed over\n%s", logged)
	}
	if !strings.Contains(logged, "expected 3 products but collected 2") {
		t.Errorf("stderr does not report the shortfall:\n%s", logged)
	}
}

func runDumper(t *testing.T) (int, string) {
	t.Helper()
	code, logged, _ := runDumperOutput(t)
	return code, logged
}

func TestMultiPageProductIdentities(t *testing.T) {
	ids := make([]int, tcgplayer.MaxItemsInResponse+3)
	for i := range ids {
		ids[i] = i + 1
	}
	serveCatalog(t, map[string][]int{"Cards": ids}, len(ids), 0)
	code, logged, data := runDumperOutput(t)
	if code != 0 {
		t.Fatalf("exit %d: %s", code, logged)
	}
	var dump tcgplayer.CatalogDump
	if err := json.Unmarshal(data, &dump); err != nil {
		t.Fatal(err)
	}
	if len(dump.Products) != len(ids) {
		t.Fatalf("got %d products", len(dump.Products))
	}
	for i, p := range dump.Products {
		if p.ProductID != ids[i] || p.ProductType != "Cards" || len(p.Skus) != 1 {
			t.Errorf("product %d: %+v", i, p)
		}
	}
}

func TestDuplicateProductMasksMissingProduct(t *testing.T) {
	serveCatalog(t, map[string][]int{"Cards": {1, 1, 3}}, 3, 0)
	code, logged := runDumper(t)
	if code == 0 || !strings.Contains(logged, "duplicate product") {
		t.Fatalf("exit %d: %s", code, logged)
	}
}

func TestOverlappingTypes(t *testing.T) {
	for _, total := range []int{3, 4} {
		t.Run(strconv.Itoa(total), func(t *testing.T) {
			serveCatalog(t, map[string][]int{"Cards": {1, 2}, "Sealed Products": {2, 3}}, total, 0)
			code, logged := runDumper(t)
			if (code == 0) != (total == 3) {
				t.Fatalf("exit %d: %s", code, logged)
			}
		})
	}
}

func TestGroupIntegrity(t *testing.T) {
	for _, tt := range []struct {
		name     string
		groups   []tcgplayer.Group
		products []tcgplayer.Product
	}{
		{"duplicate", []tcgplayer.Group{{GroupID: 1}, {GroupID: 1}}, nil},
		{"missing", []tcgplayer.Group{{GroupID: 1}}, []tcgplayer.Product{{ProductID: 1, GroupID: 2}}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if err := validateCatalog(tt.groups, tt.products, len(tt.groups), len(tt.products), len(tt.products), 0); err == nil {
				t.Fatal("accepted invalid groups")
			}
		})
	}
}

func TestInvalidWorkerCount(t *testing.T) {
	for _, threads := range []string{"0", "-1"} {
		t.Run(threads, func(t *testing.T) {
			code, logged, _ := runDumperOutput(t, "-thread", threads)
			if code == 0 || !strings.Contains(logged, "thread must be positive") {
				t.Fatalf("exit %d: %s", code, logged)
			}
		})
	}
}
