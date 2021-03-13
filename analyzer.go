package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"log"
	"math"
	"net/http"
	"net/url"
	"path"
	"strconv"
	"strings"

	"github.com/alexflint/go-arg"
	"github.com/pkg/errors"
	"golang.org/x/net/html"
)

const (
	baseURL               = "https://www.zoopla.co.uk/for-sale/property"
	defaultOutputFilename = "prices.json"
)

func main() {
	if err := run(context.Background()); err != nil {
		log.Fatal(err)
	}
}

type cliArgs struct {
	Postcode       string `arg:"required"`
	PriceMin       *uint64
	PriceMax       *uint64
	BedsMin        *uint32
	BedsMax        *uint32
	Radius         uint32
	OutputFilename string
}

func run(ctx context.Context) error {
	args := parseArgs()
	prices, err := getAllPrices(&args)
	if err != nil {
		return err
	}

	log.Printf("got %d prices", len(prices))
	if len(prices) == 0 {
		return nil
	}

	if err := writePrices(prices, args.OutputFilename); err != nil {
		return err
	}
	log.Print("wrote price data to ", args.OutputFilename)

	stats := calculatePriceStats(prices)
	log.Print("price stats: ", stats)

	return nil
}

func parseArgs() cliArgs {
	cli := cliArgs{OutputFilename: defaultOutputFilename}
	arg.MustParse(&cli)
	return cli
}

func getAllPrices(args *cliArgs) ([]uint64, error) {
	var allPrices []uint64
	for pageNum := uint32(1); ; pageNum++ {
		prices, err := getPricesPage(args, pageNum)
		if err != nil {
			return nil, errors.Wrapf(err, "while getting page %d", pageNum)
		}

		if len(prices) == 0 {
			return allPrices, nil
		}

		allPrices = append(allPrices, prices...)
	}
}

func getPricesPage(args *cliArgs, pageNum uint32) ([]uint64, error) {
	pageUrl, err := getPageUrl(args, pageNum)
	if err != nil {
		return nil, errors.Wrap(err, "while getting page URL")
	}
	log.Print("pageUrl = ", pageUrl)

	pageHTML, err := getPageHTML(pageUrl)
	if err != nil {
		return nil, errors.Wrap(err, "while getting page contents")
	}

	return parseHTML(pageHTML), nil
}

func getPageUrl(args *cliArgs, pageNum uint32) (*url.URL, error) {
	u, err := url.Parse(baseURL)
	if err != nil {
		return nil, err
	}

	u.Path = path.Join(u.Path, args.Postcode)

	q := u.Query()
	if args.PriceMin != nil {
		q.Set("price_min", strconv.FormatUint(*args.PriceMin, 10))
	}

	if args.PriceMax != nil {
		q.Set("price_max", strconv.FormatUint(*args.PriceMax, 10))
	}

	if args.BedsMin != nil {
		q.Set("beds_min", strconv.FormatUint(uint64(*args.BedsMin), 10))
	}

	if args.BedsMax != nil {
		q.Set("beds_max", strconv.FormatUint(uint64(*args.BedsMax), 10))
	}

	q.Set("radius", strconv.FormatUint(uint64(args.Radius), 10))
	q.Set("pn", strconv.FormatUint(uint64(pageNum), 10))
	q.Set("is_retirement_home", "false")
	q.Set("is_shared_ownership", "false")
	u.RawQuery = q.Encode()

	return u, nil
}

func getPageHTML(pageUrl *url.URL) (*html.Node, error) {
	rsp, err := http.Get(pageUrl.String())
	if err != nil {
		return nil, errors.Wrapf(err, "while making HTTP request to %s", pageUrl)
	}

	if rsp.StatusCode != http.StatusOK {
		return nil, errors.Errorf("unexpected status %s", rsp.Status)
	}

	doc, err := html.Parse(rsp.Body)
	if err != nil {
		return nil, errors.Wrap(err, "while parsing as HTML")
	}

	return doc, nil
}

func parseHTML(root *html.Node) []uint64 {
	listings := findListingsContainer(root)
	if listings == nil {
		log.Print("no listings container in response")
		return nil
	}

	return getPricesFromListings(listings)
}

func findListingsContainer(root *html.Node) *html.Node {
	var parseHTMLNode func(n *html.Node) *html.Node
	parseHTMLNode = func(n *html.Node) *html.Node {
		if n.Type == html.ElementNode && n.Data == "div" {
			for i := range n.Attr {
				if n.Attr[i].Key != "class" {
					continue
				}

				if strings.Contains(n.Attr[i].Val, "ListingsContainer") {
					return n
				}
			}
		}

		for c := n.FirstChild; c != nil; c = c.NextSibling {
			if listingsNode := parseHTMLNode(c); listingsNode != nil {
				return listingsNode
			}
		}
		return nil
	}
	return parseHTMLNode(root)
}

func getPricesFromListings(listings *html.Node) []uint64 {
	var prices []uint64

	var parseHTMLNode func(n *html.Node)
	parseHTMLNode = func(n *html.Node) {
		if n.Type == html.ElementNode && n.Data == "div" {
			for i := range n.Attr {
				if n.Attr[i].Key != "class" {
					continue
				}

				if strings.Contains(n.Attr[i].Val, "PriceContainer") {
					price, err := parsePriceNode(n)
					if err != nil {
						log.Print(err)
						continue
					}
					prices = append(prices, price)
				}
			}
		}

		for c := n.FirstChild; c != nil; c = c.NextSibling {
			parseHTMLNode(c)
		}
	}

	parseHTMLNode(listings)

	return prices
}

func parsePriceNode(node *html.Node) (uint64, error) {
	for c := node.FirstChild; c != nil; c = c.NextSibling {
		if c.Type == html.ElementNode && c.Data == "p" {
			for i := range c.Attr {
				if c.Attr[i].Key != "class" {
					continue
				}

				if strings.Contains(c.Attr[i].Val, "Text") && !strings.Contains(c.Attr[i].Val, "PriceTitleText") {
					if c.FirstChild == nil {
						return 0, errors.New("no price in Text node")
					}
					return parsePrice(c.FirstChild.Data)
				}
			}
		}
	}

	return 0, errors.New("cannot find price data to parse")
}

func parsePrice(raw string) (uint64, error) {
	// raw will be a string like "£435,000"
	raw = strings.TrimSpace(raw)
	raw = strings.Replace(raw, ",", "", -1)
	raw = strings.Replace(raw, "£", "", 1)
	return strconv.ParseUint(raw, 10, 64)
}

func writePrices(prices []uint64, filename string) error {
	priceData, err := json.Marshal(prices)
	if err != nil {
		return errors.Wrap(err, "while marshalling price data")
	}

	return ioutil.WriteFile(filename, priceData, 0644)
}

type priceStats struct {
	mean   float64
	stddev float64
}

func calculatePriceStats(prices []uint64) priceStats {
	mean := calculateMean(prices)
	stddev := calculateStddev(prices, mean)

	return priceStats{mean: mean, stddev: stddev}
}

func calculateMean(prices []uint64) float64 {
	var sum float64
	for _, p := range prices {
		sum += float64(p)
	}

	return sum / float64(len(prices))
}

func calculateStddev(prices []uint64, mean float64) float64 {
	if len(prices) == 1 {
		return 0.0
	}

	var sumSquares float64
	for _, p := range prices {
		diff := float64(p) - mean
		sumSquares += diff * diff
	}

	variance := sumSquares / float64(len(prices)-1)
	return math.Sqrt(variance)
}

func (s priceStats) String() string {
	return fmt.Sprintf("mean = %.0f, stddev = %.0f", s.mean, s.stddev)
}
