// Command tcgdumper writes a category's whole TCGplayer catalog as one
// json document: its groups, its products with their skus, and the
// metadata naming the ids those skus carry.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"sort"
	"sync"
	"sync/atomic"

	"github.com/mtgban/go-tcgplayer"
)

func run() int {
	categoryOpt := flag.Int("category", 0, "category id to dump")
	tcgPublicKeyOpt := flag.String("pub", "", "TCGplayer public key")
	tcgPrivateKeyOpt := flag.String("pri", "", "TCGplayer private key")
	threadOpt := flag.Int("thread", 8, "How many threads to spawn")
	var prettyOpt bool
	flag.BoolVar(&prettyOpt, "pretty", false, "indent the JSON output")
	flag.BoolVar(&prettyOpt, "p", false, "indent the JSON output (shorthand)")
	flag.Parse()

	pubKey, priKey := *tcgPublicKeyOpt, *tcgPrivateKeyOpt
	if pubKey == "" {
		pubKey = os.Getenv("TCGPLAYER_PUBLIC_KEY")
	}
	if priKey == "" {
		priKey = os.Getenv("TCGPLAYER_PRIVATE_KEY")
	}

	if *threadOpt < 1 {
		fmt.Fprintln(os.Stderr, "thread must be positive")
		return 1
	}

	if *categoryOpt == 0 {
		fmt.Fprintln(os.Stderr, "Missing category id")
		return 1
	}

	category := tcgplayer.CategoryID(*categoryOpt)

	tcgClient, err := tcgplayer.NewClient(pubKey, priKey)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}

	categories, err := tcgClient.GetCategoriesDetails(context.Background(), []tcgplayer.CategoryID{category})
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	if len(categories) == 0 {
		fmt.Fprintln(os.Stderr, "No category found with id", *categoryOpt)
		return 1
	}
	fmt.Fprintln(os.Stderr, "Retrieved category details")

	conditions, err := tcgClient.ListCategoryConditions(context.Background(), category)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	languages, err := tcgClient.ListCategoryLanguages(context.Background(), category)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	printings, err := tcgClient.ListCategoryPrintings(context.Background(), category)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	rarities, err := tcgClient.ListCategoryRarities(context.Background(), category)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	fmt.Fprintln(os.Stderr, "Retrieved sku metadata")

	totalgroups, err := tcgClient.TotalGroups(context.Background(), category)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	var groups []tcgplayer.Group
	for i := 0; i < totalgroups; i += tcgplayer.MaxItemsInResponse {
		out, err := tcgClient.ListAllCategoryGroups(context.Background(), category, i)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		groups = append(groups, out...)
	}
	fmt.Fprintln(os.Stderr, "Found", len(groups), "groups")

	// The API never reports which product type a product is filed under,
	// so the type must be established at fetch time: page each type
	// separately and stamp the products with the type they answered to.
	// Each category names its own types, so ask for that category's.
	type page struct {
		productType tcgplayer.ProductType
		offset      int
		expected    int
	}
	var jobs []page
	totalProducts := 0
	for _, productType := range tcgplayer.ProductTypes(category) {
		total, err := tcgClient.TotalProducts(context.Background(), category, []tcgplayer.ProductType{productType})
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		if total == 0 {
			continue
		}
		fmt.Fprintln(os.Stderr, "Found", total, productType, "products")
		totalProducts += total
		for i := 0; i < total; i += tcgplayer.MaxItemsInResponse {
			jobs = append(jobs, page{productType, i, min(tcgplayer.MaxItemsInResponse, total-i)})
		}
	}
	fmt.Fprintln(os.Stderr, "Found", totalProducts, "products")

	// The per-type totals have to account for the whole category. Counting
	// with no filter at all is the only way to see a product whose type is
	// missing from AllProductTypes: counting the union of that same list
	// cannot report what the list does not name.
	categoryTotal, err := tcgClient.TotalProducts(context.Background(), category, nil)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	if categoryTotal > totalProducts {
		fmt.Fprintf(os.Stderr, "the category holds %d products but its known types account for only %d, "+
			"so some product type is missing from ProductTypesByCategory and its products would go undumped\n",
			categoryTotal, totalProducts)
		return 1
	}
	if categoryTotal < totalProducts {
		fmt.Fprintf(os.Stderr, "warning: per-type totals sum to %d but the category holds %d, "+
			"so some products carry more than one type and appear once per type\n",
			totalProducts, categoryTotal)
	}

	totalPages := len(jobs)

	pages := make(chan page)
	channel := make(chan tcgplayer.Product)
	var wg sync.WaitGroup
	var failedPages, donePages atomic.Int64

	for i := 0; i < *threadOpt; i++ {
		wg.Go(func() {
			for job := range pages {
				products, err := tcgClient.ListAllProducts(context.Background(), category, []tcgplayer.ProductType{job.productType}, true, job.offset)
				if err != nil {
					fmt.Fprintln(os.Stderr, job.productType, "page at offset", job.offset, "failed:", err)
					failedPages.Add(1)
				} else if len(products) != job.expected {
					fmt.Fprintf(os.Stderr, "%s page at offset %d: expected %d products but collected %d\n", job.productType, job.offset, job.expected, len(products))
					failedPages.Add(1)
				}
				if done := donePages.Add(1); done%50 == 0 || done == int64(totalPages) {
					fmt.Fprintln(os.Stderr, "Fetched", done, "of", totalPages, "pages")
				}
				for _, product := range products {
					product.ProductType = job.productType
					channel <- product
				}
			}
		})
	}

	go func() {
		for _, job := range jobs {
			pages <- job
		}
		close(pages)

		wg.Wait()
		close(channel)
	}()

	var products []tcgplayer.Product
	for result := range channel {
		products = append(products, result)
	}

	sort.Slice(products, func(i, j int) bool {
		return products[i].ProductID < products[j].ProductID
	})

	var output tcgplayer.CatalogDump
	output.Category = categories[0]
	output.Conditions = conditions
	output.Languages = languages
	output.Printings = printings
	output.Rarities = rarities
	output.Products = products
	output.Groups = groups

	if err := validateCatalog(groups, products, totalgroups, totalProducts, categoryTotal, failedPages.Load()); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}

	enc := json.NewEncoder(os.Stdout)
	if prettyOpt {
		enc.SetIndent("", "  ")
	}
	if err := enc.Encode(output); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	fmt.Fprintln(os.Stderr, "Dumped", len(products), "products and", len(groups), "groups")

	return 0
}

// validateCatalog checks identities as well as counts before any JSON is written.
// Counts are a snapshot, so a catalog changing during pagination can still
// require a retry; they cannot establish an atomic view of the remote catalog.
func validateCatalog(groups []tcgplayer.Group, products []tcgplayer.Product, totalGroups, totalProducts, categoryTotal int, failedPages int64) error {
	if failedPages != 0 {
		return fmt.Errorf("%d pages failed to download", failedPages)
	}
	if len(groups) != totalGroups {
		return fmt.Errorf("expected %d groups but collected %d", totalGroups, len(groups))
	}
	if len(products) != totalProducts {
		return fmt.Errorf("expected %d products but collected %d", totalProducts, len(products))
	}
	groupIDs := make(map[tcgplayer.GroupID]bool, len(groups))
	for _, group := range groups {
		if groupIDs[group.GroupID] {
			return fmt.Errorf("duplicate group %d", group.GroupID)
		}
		groupIDs[group.GroupID] = true
	}
	productIDs := make(map[tcgplayer.ProductID]bool, len(products))
	type membership struct {
		id          tcgplayer.ProductID
		productType tcgplayer.ProductType
	}
	memberships := make(map[membership]bool, len(products))
	for _, product := range products {
		key := membership{product.ProductID, product.ProductType}
		if memberships[key] {
			return fmt.Errorf("duplicate product %d within type %q", product.ProductID, product.ProductType)
		}
		memberships[key] = true
		productIDs[product.ProductID] = true
		if !groupIDs[product.GroupID] {
			return fmt.Errorf("product %d references missing group %d", product.ProductID, product.GroupID)
		}
	}
	if len(productIDs) != categoryTotal {
		return fmt.Errorf("expected %d unique products but collected %d", categoryTotal, len(productIDs))
	}
	return nil
}

func main() {
	os.Exit(run())
}
