package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"github.com/mtgban/go-tcgplayer"
)

// The catalog serveCatalog stands in for, written as the json the API sends.
// Every field the dump carries is set, each to a value no other field in its
// object shares, so a field read or written under the wrong name shows up in
// the dump rather than passing as a zero.
const (
	categoryJSON = `{"categoryId": 1, "name": "Test", "modifiedOn": "2026-01-01T00:00:00",
		"displayName": "Test Game", "seoCategoryName": "test-game", "sealedLabel": "Sealed",
		"nonSealedLabel": "Singles", "conditionGuideUrl": "https://example.com/guide",
		"isScannable": true, "popularity": 42, "isDirect": true,
		"categoryDescription": "A test game", "categoryPageTitle": "Buy Test Game"}`
	conditionsJSON = `[{"conditionId": 3, "name": "Near Mint", "abbreviation": "NM", "displayOrder": 4}]`
	languagesJSON  = `[{"languageId": 1, "name": "English", "abbr": "EN"}]`
	printingsJSON  = `[{"printingId": 2, "name": "Foil", "displayOrder": 5, "modifiedOn": "2026-01-02T00:00:00"}]`
	raritiesJSON   = `[{"rarityId": 6, "displayText": "Rare", "dbValue": "R"}]`
	groupJSON      = `{"groupId": 10, "name": "Set One", "abbreviation": "SO", "isSupplemental": true,
		"publishedOn": "2026-01-03T00:00:00", "modifiedOn": "2026-01-04T00:00:00", "categoryId": 1}`
)

// productJSON is a product as the API sends it. Like the API, it carries skus
// and extended data only when the request asks for them.
func productJSON(id int, skus, extended bool) string {
	fields := []string{fmt.Sprintf(`"productId": %d, "name": "Card %d", "cleanName": "Card %d Clean", `+
		`"imageUrl": "https://example.com/%d.jpg", "groupId": 10, "categoryId": 1, "url": "https://example.com/product/%d", `+
		`"modifiedOn": "2026-01-05T00:00:00", "imageCount": 2, `+
		`"presaleInfo": {"isPresale": true, "releasedOn": "2026-10-23T00:00:00", "note": "Presale %d"}`,
		id, id, id, id, id, id)}
	if extended {
		fields = append(fields, fmt.Sprintf(
			`"extendedData": [{"name": "Number", "displayName": "Card Number", "value": "%03d"}]`, id))
	}
	if skus {
		fields = append(fields, fmt.Sprintf(
			`"skus": [{"skuId": %d, "productId": %d, "languageId": 1, "printingId": 2, "conditionId": 3}]`, id*10, id))
	}
	return "{" + strings.Join(fields, ", ") + "}"
}

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
		envelope(w, 1, "["+categoryJSON+"]")
	})
	mux.HandleFunc("/catalog/categories/1/conditions", func(w http.ResponseWriter, r *http.Request) {
		envelope(w, 1, conditionsJSON)
	})
	mux.HandleFunc("/catalog/categories/1/languages", func(w http.ResponseWriter, r *http.Request) {
		envelope(w, 1, languagesJSON)
	})
	mux.HandleFunc("/catalog/categories/1/printings", func(w http.ResponseWriter, r *http.Request) {
		envelope(w, 1, printingsJSON)
	})
	mux.HandleFunc("/catalog/categories/1/rarities", func(w http.ResponseWriter, r *http.Request) {
		envelope(w, 1, raritiesJSON)
	})
	mux.HandleFunc("/catalog/groups", func(w http.ResponseWriter, r *http.Request) {
		envelope(w, 1, "["+groupJSON+"]")
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
			items = append(items, productJSON(ids[i], q.Get("includeSkus") == "true", q.Get("getExtendedFields") == "true"))
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

// runDumperOutput runs the program against the stubbed API with args added to
// its command line, returning its exit code, what it wrote to stderr, and the
// dump it wrote to stdout.
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
	defer outFile.Close()
	errFile, err := os.CreateTemp(t.TempDir(), "log")
	if err != nil {
		t.Fatal(err)
	}
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
	dump, err := os.ReadFile(outFile.Name())
	if err != nil {
		t.Fatal(err)
	}
	if code != 0 && len(dump) != 0 {
		t.Error("failed dump wrote JSON")
	}
	return code, string(logged), dump
}

// runDumper runs the program against the stubbed API, returning its exit
// code and what it wrote to stderr.
func runDumper(t *testing.T) (int, string) {
	t.Helper()
	code, logged, _ := runDumperOutput(t)
	return code, logged
}

// decodeJSON reads raw json into the generic values encoding/json produces,
// so that a comparison sees the field names as written, not as any struct
// in this module would read them.
func decodeJSON(t *testing.T, raw string) any {
	t.Helper()
	var v any
	if err := json.Unmarshal([]byte(raw), &v); err != nil {
		t.Fatalf("decoding %s: %v", raw, err)
	}
	return v
}

func indentJSON(v any) string {
	out, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return fmt.Sprint(v)
	}
	return string(out)
}

// TestDumpOutput holds the dump to the format its readers decode: every field
// under the API's own name, each product stamped with the type it was fetched
// by, and products sorted by id. The expected document is assembled from the
// same wire json the stub serves. Decoding the output into CatalogDump instead
// would read it back through the tags that wrote it, and agree with a wrong one.
func TestDumpOutput(t *testing.T) {
	// Out of order within a page, and across two types, so the sort shows
	serveCatalog(t, map[string][]int{"Cards": {205, 101}, "Sealed Products": {150}}, 3, 0)

	code, logged, dump := runDumperOutput(t)
	if code != 0 {
		t.Fatalf("exit = %d, want 0\n%s", code, logged)
	}

	product := func(id int, productType string) map[string]any {
		var p map[string]any
		if err := json.Unmarshal([]byte(productJSON(id, true, true)), &p); err != nil {
			t.Fatal(err)
		}
		p["productType"] = productType
		return p
	}
	want := map[string]any{
		"category":   decodeJSON(t, categoryJSON),
		"conditions": decodeJSON(t, conditionsJSON),
		"languages":  decodeJSON(t, languagesJSON),
		"printings":  decodeJSON(t, printingsJSON),
		"rarities":   decodeJSON(t, raritiesJSON),
		"groups":     decodeJSON(t, "["+groupJSON+"]"),
		"products": []any{
			product(101, "Cards"),
			product(150, "Sealed Products"),
			product(205, "Cards"),
		},
	}
	if got := decodeJSON(t, string(dump)); !reflect.DeepEqual(got, want) {
		t.Errorf("dump =\n%s\nwant\n%s", indentJSON(got), indentJSON(want))
	}
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

// Groups are paged too, and a group page answering short loses sets the same
// way a product page loses cards.
func TestShortGroupPageFailsTheDump(t *testing.T) {
	serveCatalog(t, map[string][]int{"Cards": {1, 2, 3}}, 3, 0)
	// The count promises two groups, the page hands back one. serveCatalog
	// restores the endpoint when the test ends.
	groups := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `{"totalItems": 2, "success": true, "errors": [], "results": [%s]}`, groupJSON)
	}))
	t.Cleanup(groups.Close)
	tcgplayer.CatalogGroupsURL = groups.URL

	code, logged := runDumper(t)
	if code == 0 {
		t.Errorf("exit = 0, want non-zero: a group was never handed over\n%s", logged)
	}
	if !strings.Contains(logged, "expected 2 groups but collected 1") {
		t.Errorf("stderr does not report the shortfall:\n%s", logged)
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
