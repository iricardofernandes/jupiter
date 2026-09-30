package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/iricardofernandes/jupiter/pkg/loadgen"
)

// loadgen sends traffic to a running Jupiter API with a merchant's secret key.
func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "loadgen:", err)
		os.Exit(2)
	}
}

func run() error {
	url := flag.String("url", "http://127.0.0.1:8080", "the API's base URL")
	scenario := flag.String("scenario", "steady", "steady, or card-testing")
	n := flag.Int("n", 100, "how many payments")
	workers := flag.Int("workers", 4, "how many at a time")
	seed := flag.Uint64("seed", 1, "the random seed for card testing")
	flag.Parse()
	key := os.Getenv("JUPITER_KEY")
	if key == "" {
		return errors.New("set JUPITER_KEY to a merchant's secret key")
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	client := loadgen.Client{BaseURL: *url, Key: key}
	var report *loadgen.Report
	switch *scenario {
	case "steady":
		report = loadgen.Steady(ctx, client, []loadgen.Card{{Number: "4242424242424242", ExpMonth: 12, ExpYear: 2030}}, *n, *workers)
	case "card-testing":
		report = loadgen.CardTesting(ctx, client, *n, *workers, *seed)
	default:
		return errors.New("-scenario must be steady or card-testing")
	}
	fmt.Println(report)
	return nil
}
