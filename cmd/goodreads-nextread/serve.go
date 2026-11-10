package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"time"

	"goodreads-nextread/internal/config"
	"goodreads-nextread/internal/enrich"
	"goodreads-nextread/internal/goodreads"
	"goodreads-nextread/internal/shelfsync"
	"goodreads-nextread/internal/store"
	"goodreads-nextread/internal/web"
)

// serveCmd runs the HTTP server (the `serve` subcommand).
func serveCmd(args []string) error {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	configPath := fs.String("config", "config.yaml", "path to config file")
	addr := fs.String("addr", envOr("ADDR", ":8080"), "address to listen on")
	perDay := fs.Int("scrapes-per-day", 0, "how many times a day to sync the shelf (0 = use config)")
	fs.Parse(args)

	cfg, err := config.Load(*configPath)
	if err != nil {
		return err
	}
	if *perDay > 0 {
		if *perDay > config.MaxScrapesPerDay {
			return fmt.Errorf("--scrapes-per-day must be at most %d", config.MaxScrapesPerDay)
		}
		cfg.ScrapesPerDay = *perDay
	}

	st, err := store.Open(cfg.DBPath)
	if err != nil {
		return err
	}
	defer st.Close()

	cache := web.NewCache(st.AllBooks)
	if err := cache.Refresh(); err != nil {
		return fmt.Errorf("loading stored shelf: %w", err)
	}

	ctx := context.Background()
	if len(cache.Books()) == 0 {
		log.Printf("no stored shelf yet; harvesting %s/%s, serving once the first books are in", cfg.UserID, cfg.Shelf)
	}
	// A failure here (nothing could be fetched at all) is fatal so the
	// container's restart policy retries.
	if err := web.StartSync(ctx, newSyncer(cfg, st), cache, cfg.SyncInterval()); err != nil {
		return err
	}
	log.Printf("serving %d books from %s; syncing %d times a day", len(cache.Books()), cfg.DBPath, cfg.ScrapesPerDay)

	// Fill in descriptions, ratings and covers in the background; the cache
	// reloads after each batch so details appear as they arrive.
	go newEnricher(cfg, st).Run(ctx, func() {
		if err := cache.Refresh(); err != nil {
			log.Printf("reloading shelf from the store failed, keeping previous list: %v", err)
		}
	})

	srv := &http.Server{
		Addr:         *addr,
		Handler:      web.Handler(cache),
		ReadTimeout:  10 * time.Second,
		WriteTimeout: 15 * time.Second,
	}

	log.Printf("goodreads-nextread serving on %s", *addr)
	return srv.ListenAndServe()
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// newSyncer builds the shelf syncer from the config.
func newSyncer(cfg *config.Config, st *store.Store) *shelfsync.Syncer {
	gr := goodreads.NewClient(goodreads.Options{BaseURL: cfg.BaseURL, UserID: cfg.UserID, Shelf: cfg.Shelf, Delay: cfg.RequestDelay})
	return shelfsync.New(gr, st, cfg.MaxRequests)
}

// newEnricher builds the enricher from the config, with its own paced client.
func newEnricher(cfg *config.Config, st *store.Store) *enrich.Enricher {
	gr := goodreads.NewClient(goodreads.Options{BaseURL: cfg.BaseURL, UserID: cfg.UserID, Shelf: cfg.Shelf, Delay: cfg.EnrichDelay})
	return enrich.New(gr, goodreads.NewGraphQL(nil, cfg.GraphQLEndpoint, gr, st), st)
}
