package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"os"
	"sort"
	"sync"

	"github.com/mtgban/go-tcgplayer"
)

func run() int {
	categoryOpt := flag.Int("category", 0, "category id to dump")
	tcgPublicKeyOpt := flag.String("pub", "", "TCGplayer public key")
	tcgPrivateKeyOpt := flag.String("pri", "", "TCGplayer private key")
	threadOpt := flag.Int("thread", 8, "How many threads to spawn")
	flag.Parse()

	pubEnv := os.Getenv("TCGPLAYER_PUBLIC_KEY")
	if pubEnv != "" {
		tcgPublicKeyOpt = &pubEnv
	}
	priEnv := os.Getenv("TCGPLAYER_PRIVATE_KEY")
	if priEnv != "" {
		tcgPrivateKeyOpt = &priEnv
	}

	if *tcgPublicKeyOpt == "" || *tcgPrivateKeyOpt == "" {
		log.Fatalln("Missing TCGplayer keys")
	}

	if *categoryOpt == 0 {
		log.Fatalln("Missing category id")
	}

	tcgClient := tcgplayer.NewClient(*tcgPublicKeyOpt, *tcgPrivateKeyOpt)

	categories, err := tcgClient.GetCategoriesDetails([]int{*categoryOpt})
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	fmt.Fprintln(os.Stderr, "Retrieved category details")

	totalgroups, err := tcgClient.TotalGroups(*categoryOpt)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	var groups []tcgplayer.Group
	for i := 0; i < totalgroups; i += tcgplayer.MaxItemsInResponse {
		out, err := tcgClient.ListAllCategoryGroups(*categoryOpt, i)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		groups = append(groups, out...)
	}
	fmt.Fprintln(os.Stderr, "Found", len(groups), "groups")

	totalProducts, err := tcgClient.TotalProducts(*categoryOpt, tcgplayer.AllProductTypes)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	fmt.Fprintln(os.Stderr, "Found", totalProducts, "products")

	pages := make(chan int)
	channel := make(chan tcgplayer.Product)
	var wg sync.WaitGroup

	for i := 0; i < *threadOpt; i++ {
		wg.Add(1)
		go func() {
			for page := range pages {
				products, err := tcgClient.ListAllProducts(*categoryOpt, tcgplayer.AllProductTypes, true, page)
				if err != nil {
					fmt.Fprintln(os.Stderr, err)
					continue
				}
				for _, product := range products {
					channel <- product
				}
			}
			wg.Done()
		}()
	}

	go func() {
		for i := 0; i < totalProducts; i += tcgplayer.MaxItemsInResponse {
			pages <- i
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
		return products[i].ProductId < products[j].ProductId
	})

	var output struct {
		Category tcgplayer.Category  `json:"category"`
		Groups   []tcgplayer.Group   `json:"groups"`
		Products []tcgplayer.Product `json:"products"`
	}
	output.Category = categories[0]
	output.Products = products
	output.Groups = groups

	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	err = enc.Encode(output)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	fmt.Fprintln(os.Stderr, "Dumped", len(products), "products and", len(groups), "groups")

	return 0
}

func main() {
	os.Exit(run())
}
