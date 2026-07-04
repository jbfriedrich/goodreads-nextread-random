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

// shelfCache holds the shelf's books in memory. The list is primed once at
// startup and refreshed by a background goroutine (see run), so requests never
// block on the network. On a refresh failure it keeps serving the last good
// copy.
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

// run refreshes the shelf every interval until ctx is cancelled. A failed
// refresh is logged and the previous list is retained.
func (c *shelfCache) run(ctx context.Context, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := c.refresh(); err != nil {
				log.Printf("background shelf refresh failed, keeping previous list: %v", err)
			}
		}
	}
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

// serveCmd runs the HTTP server (the `serve` subcommand).
func serveCmd(args []string) error {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	configPath := fs.String("config", "config.yaml", "path to config file")
	addr := fs.String("addr", envOr("ADDR", ":8080"), "address to listen on")
	refresh := fs.Duration("refresh-interval", 0, "how often to refresh the shelf in the background (0 = use config)")
	fs.Parse(args)

	cfg, err := loadConfig(*configPath)
	if err != nil {
		return err
	}

	interval := cfg.RefreshInterval
	if *refresh > 0 {
		interval = *refresh
	}

	cache := &shelfCache{
		fetch: func() ([]Book, error) {
			log.Printf("refreshing shelf from %s", cfg.RSSURL)
			return fetchShelf(cfg.RSSURL)
		},
	}

	// Prime the shelf before accepting traffic so no visitor waits on the
	// initial fetch. A failure here is fatal: the container's restart policy
	// will retry the startup.
	log.Printf("priming shelf before serving...")
	if err := cache.refresh(); err != nil {
		return fmt.Errorf("priming shelf on startup: %w", err)
	}

	// Keep the shelf current in the background so requests never block on
	// Goodreads and shelf changes get picked up within one interval.
	go cache.run(context.Background(), interval)
	log.Printf("shelf primed; refreshing every %s in the background", interval)

	mux := http.NewServeMux()
	mux.Handle("/", bookHandler(cache))
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintln(w, "ok")
	})

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
