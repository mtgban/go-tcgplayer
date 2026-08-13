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

	if *categoryOpt == 0 {
		fmt.Fprintln(os.Stderr, "Missing category id")
		return 1
	}

	tcgClient, err := tcgplayer.NewClient(pubKey, priKey)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}

	categories, err := tcgClient.GetCategoriesDetails(context.Background(), []int{*categoryOpt})
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	if len(categories) == 0 {
		fmt.Fprintln(os.Stderr, "No category found with id", *categoryOpt)
		return 1
	}
	fmt.Fprintln(os.Stderr, "Retrieved category details")

	conditions, err := tcgClient.ListCategoryConditions(context.Background(), *categoryOpt)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	languages, err := tcgClient.ListCategoryLanguages(context.Background(), *categoryOpt)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	printings, err := tcgClient.ListCategoryPrintings(context.Background(), *categoryOpt)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	rarities, err := tcgClient.ListCategoryRarities(context.Background(), *categoryOpt)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	fmt.Fprintln(os.Stderr, "Retrieved sku metadata")

	totalgroups, err := tcgClient.TotalGroups(context.Background(), *categoryOpt)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	var groups []tcgplayer.Group
	for i := 0; i < totalgroups; i += tcgplayer.MaxItemsInResponse {
		out, err := tcgClient.ListAllCategoryGroups(context.Background(), *categoryOpt, i)
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
	type page struct {
		productType string
		offset      int
	}
	var jobs []page
	totalProducts := 0
	for _, productType := range tcgplayer.AllProductTypes {
		total, err := tcgClient.TotalProducts(context.Background(), *categoryOpt, []string{productType})
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
			jobs = append(jobs, page{productType, i})
		}
	}
	fmt.Fprintln(os.Stderr, "Found", totalProducts, "products")

	// The per-type totals should partition the union; a drift means a
	// product carries several types (duplicated below) or none (missed)
	unionTotal, err := tcgClient.TotalProducts(context.Background(), *categoryOpt, tcgplayer.AllProductTypes)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	if unionTotal != totalProducts {
		fmt.Fprintln(os.Stderr, "per-type totals sum to", totalProducts, "but the union counts", unionTotal)
	}

	totalPages := len(jobs)

	pages := make(chan page)
	channel := make(chan tcgplayer.Product)
	var wg sync.WaitGroup
	var failedPages, donePages atomic.Int64

	for i := 0; i < *threadOpt; i++ {
		wg.Add(1)
		go func() {
			for job := range pages {
				products, err := tcgClient.ListAllProducts(context.Background(), *categoryOpt, []string{job.productType}, true, job.offset)
				if err != nil {
					fmt.Fprintln(os.Stderr, job.productType, "page at offset", job.offset, "failed:", err)
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
			wg.Done()
		}()
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

	enc := json.NewEncoder(os.Stdout)
	if prettyOpt {
		enc.SetIndent("", "  ")
	}
	err = enc.Encode(output)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	fmt.Fprintln(os.Stderr, "Dumped", len(products), "products and", len(groups), "groups")

	if failed := failedPages.Load(); failed > 0 {
		fmt.Fprintln(os.Stderr, failed, "pages failed to download, output is incomplete")
		return 1
	}

	return 0
}

func main() {
	os.Exit(run())
}
