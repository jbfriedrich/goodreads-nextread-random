package main

import (
	"context"
	"flag"
	"fmt"
	"html/template"
	"log"
	"math/rand"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"
)

// shelfCache holds the shelf's books in memory so requests never touch the
// network or the disk. It is loaded from the store at startup and reloaded
// after every successful sync and enrichment. On a refresh failure it keeps
// serving the last good copy.
type shelfCache struct {
	fetch func() ([]Book, error)

	mu    sync.RWMutex
	books []Book
}

// refresh fetches the shelf and swaps in the new list. On failure it leaves the
// existing list in place and returns the error, so callers can keep serving the
// last known-good copy.
func (c *shelfCache) refresh() error {
	books, err := c.fetch()
	if err != nil {
		return err
	}

	c.mu.Lock()
	c.books = books
	c.mu.Unlock()
	return nil
}

// get returns the current in-memory shelf without triggering a fetch. It never
// blocks on the network; the background refresher keeps the list current.
func (c *shelfCache) get() []Book {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.books
}

// bookHandler serves a random book from the cache as an HTML page.
func bookHandler(cache *shelfCache) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}

		books := cache.get()
		if len(books) == 0 {
			http.Error(w, "The reading list is not available right now. Try again shortly.", http.StatusServiceUnavailable)
			return
		}

		book := books[rand.Intn(len(books))]
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		fmt.Fprint(w, renderPage(book))
	})
}

// pageView is the template's data model for one book.
type pageView struct {
	Title       string
	Author      string
	Rating      string
	Pages       string
	Published   string
	Description string
	ImageURL    string
	BookURL     string
}

func newPageView(b Book) pageView {
	rating := b.AverageRating
	if rating == "0.0" {
		rating = ""
	}
	return pageView{
		Title:       b.Title,
		Author:      b.Author,
		Rating:      rating,
		Pages:       b.NumPages,
		Published:   b.Published,
		Description: cleanDescription(b.Description, 700),
		ImageURL:    b.ImageURL,
		BookURL:     b.BookURL(),
	}
}

var pageTemplate = template.Must(template.New("page").Parse(pageHTML))

// renderPage renders the HTML page for a single book.
func renderPage(b Book) string {
	var sb strings.Builder
	if err := pageTemplate.Execute(&sb, newPageView(b)); err != nil {
		// Templates are static and validated at init; execution should not fail.
		return "<!doctype html><title>Error</title>Could not render page."
	}
	return sb.String()
}

// healthHandler reports liveness only. /healthz is reachable from outside
// (nginx proxies it), so sync status goes to the logs, never here. It is always
// 200: a failed sync does not stop the site serving the stored shelf, and
// restarting the container would not fix Goodreads.
func healthHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintln(w, "ok")
	})
}

// startSync gets the cache ready to serve and keeps the shelf synced in the
// background. With a stored shelf it returns immediately. With an empty store
// it starts the initial harvest and returns as soon as the first batch (the
// newest 100 books) is servable, a few seconds in; the rest arrives in the
// background. It returns an error only if the initial harvest fails before
// anything could be stored.
func startSync(ctx context.Context, sy *syncer, cache *shelfCache, interval time.Duration) error {
	reload := func() {
		if err := cache.refresh(); err != nil {
			log.Printf("reloading shelf from the store failed, keeping previous list: %v", err)
		}
	}
	if len(cache.get()) > 0 {
		go sy.loop(ctx, interval, reload)
		return nil
	}

	ready := make(chan struct{})
	var once sync.Once
	markReady := func() { once.Do(func() { close(ready) }) }
	sy.onProgress = func() {
		reload()
		if len(cache.get()) > 0 {
			markReady()
		}
	}

	failed := make(chan error, 1)
	go func() {
		run, err := sy.sync(ctx, false)
		reload()
		if err != nil {
			if len(cache.get()) == 0 {
				failed <- err
				return // nothing to serve: the caller exits, so no loop
			}
			log.Printf("initial shelf harvest failed after storing %d books, serving those: %s", len(cache.get()), run.describe())
		} else {
			log.Print(run.describe())
		}
		markReady() // also covers a genuinely empty shelf
		sy.loop(ctx, interval, reload)
	}()

	select {
	case <-ready:
		return nil
	case err := <-failed:
		return fmt.Errorf("initial shelf harvest: %w", err)
	}
}

// serveCmd runs the HTTP server (the `serve` subcommand).
func serveCmd(args []string) error {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	configPath := fs.String("config", "config.yaml", "path to config file")
	addr := fs.String("addr", envOr("ADDR", ":8080"), "address to listen on")
	perDay := fs.Int("scrapes-per-day", 0, "how many times a day to sync the shelf (0 = use config)")
	fs.Parse(args)

	cfg, err := loadConfig(*configPath)
	if err != nil {
		return err
	}
	if *perDay > 0 {
		if *perDay > maxScrapesPerDay {
			return fmt.Errorf("--scrapes-per-day must be at most %d", maxScrapesPerDay)
		}
		cfg.ScrapesPerDay = *perDay
	}

	st, err := openStore(cfg.DBPath)
	if err != nil {
		return err
	}
	defer st.Close()

	cache := &shelfCache{fetch: st.AllBooks}
	if err := cache.refresh(); err != nil {
		return fmt.Errorf("loading stored shelf: %w", err)
	}

	ctx := context.Background()
	if len(cache.get()) == 0 {
		log.Printf("no stored shelf yet; harvesting %s/%s, serving once the first books are in", cfg.UserID, cfg.Shelf)
	}
	// A failure here (nothing could be fetched at all) is fatal so the
	// container's restart policy retries.
	if err := startSync(ctx, newSyncer(cfg, st), cache, cfg.SyncInterval()); err != nil {
		return err
	}
	log.Printf("serving %d books from %s; syncing %d times a day", len(cache.get()), cfg.DBPath, cfg.ScrapesPerDay)

	// Fill in descriptions, ratings and covers in the background; the cache
	// reloads after each batch so details appear as they arrive.
	go newEnricher(cfg, st).run(ctx, func() {
		if err := cache.refresh(); err != nil {
			log.Printf("reloading shelf from the store failed, keeping previous list: %v", err)
		}
	})

	mux := http.NewServeMux()
	mux.Handle("/", bookHandler(cache))
	mux.Handle("/healthz", healthHandler())

	srv := &http.Server{
		Addr:         *addr,
		Handler:      mux,
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
