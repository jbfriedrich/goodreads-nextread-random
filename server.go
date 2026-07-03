package main

import (
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

// shelfCache holds the shelf's books in memory and refreshes them lazily once
// the TTL elapses. On a refresh failure it keeps serving the last good copy.
type shelfCache struct {
	fetch func() ([]Book, error)
	ttl   time.Duration
	now   func() time.Time

	mu        sync.Mutex
	books     []Book
	fetchedAt time.Time
}

// get returns the cached books, refreshing them when the TTL has elapsed.
func (c *shelfCache) get() ([]Book, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	fresh := c.books != nil && c.now().Sub(c.fetchedAt) < c.ttl
	if fresh {
		return c.books, nil
	}

	books, err := c.fetch()
	if err != nil {
		if c.books != nil {
			// Serve stale data rather than failing the request.
			return c.books, nil
		}
		return nil, err
	}

	c.books = books
	c.fetchedAt = c.now()
	return c.books, nil
}

// bookHandler serves a random book from the cache as an HTML page.
func bookHandler(cache *shelfCache) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}

		books, err := cache.get()
		if err != nil {
			log.Printf("fetching shelf: %v", err)
			http.Error(w, "Could not load the reading list right now. Try again shortly.", http.StatusServiceUnavailable)
			return
		}
		if len(books) == 0 {
			http.Error(w, "The reading list is empty.", http.StatusServiceUnavailable)
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
	ttl := fs.Duration("cache-ttl", 15*time.Minute, "how long to cache the shelf")
	fs.Parse(args)

	cfg, err := loadConfig(*configPath)
	if err != nil {
		return err
	}

	cache := &shelfCache{
		ttl: *ttl,
		now: time.Now,
		fetch: func() ([]Book, error) {
			log.Printf("refreshing shelf from %s", cfg.RSSURL)
			return fetchShelf(cfg.RSSURL)
		},
	}

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
