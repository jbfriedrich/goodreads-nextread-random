package web

import (
	"context"
	_ "embed"
	"fmt"
	"html/template"
	"log"
	"math/rand"
	"net/http"
	"strings"
	"sync"
	"time"

	"goodreads-nextread/internal/goodreads"
	"goodreads-nextread/internal/shelfsync"
)

// Cache holds the shelf's books in memory so requests never touch the
// network or the disk. It is loaded from the store at startup and reloaded
// after every successful sync and enrichment. On a refresh failure it keeps
// serving the last good copy.
type Cache struct {
	fetch func() ([]goodreads.Book, error)

	mu    sync.RWMutex
	books []goodreads.Book
}

// refresh fetches the shelf and swaps in the new list. On failure it leaves the
// existing list in place and returns the error, so callers can keep serving the
// last known-good copy.
func (c *Cache) Refresh() error {
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
func (c *Cache) Books() []goodreads.Book {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.books
}

// bookHandler serves a random book from the cache as an HTML page.
func bookHandler(cache *Cache) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}

		books := cache.Books()
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

func newPageView(b goodreads.Book) pageView {
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
		Description: goodreads.CleanDescription(b.Description, 700),
		ImageURL:    b.ImageURL,
		BookURL:     b.URL(),
	}
}

//go:embed page.html
var pageHTML string

var pageTemplate = template.Must(template.New("page").Parse(pageHTML))

// renderPage renders the HTML page for a single book.
func renderPage(b goodreads.Book) string {
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

// StartSync gets the cache ready to serve and keeps the shelf synced in the
// background. With a stored shelf it returns immediately. With an empty store
// it starts the initial harvest and returns as soon as the first batch (the
// newest 100 books) is servable, a few seconds in; the rest arrives in the
// background. It returns an error only if the initial harvest fails before
// anything could be stored.
func StartSync(ctx context.Context, sy *shelfsync.Syncer, cache *Cache, interval time.Duration) error {
	reload := func() {
		if err := cache.Refresh(); err != nil {
			log.Printf("reloading shelf from the store failed, keeping previous list: %v", err)
		}
	}
	if len(cache.Books()) > 0 {
		go sy.Loop(ctx, interval, reload)
		return nil
	}

	ready := make(chan struct{})
	var once sync.Once
	markReady := func() { once.Do(func() { close(ready) }) }
	sy.OnProgress = func() {
		reload()
		if len(cache.Books()) > 0 {
			markReady()
		}
	}

	failed := make(chan error, 1)
	go func() {
		run, err := sy.Sync(ctx, false)
		reload()
		if err != nil {
			if len(cache.Books()) == 0 {
				failed <- err
				return // nothing to serve: the caller exits, so no loop
			}
			log.Printf("initial shelf harvest failed after storing %d books, serving those: %s", len(cache.Books()), run.Describe())
		} else {
			log.Print(run.Describe())
		}
		markReady() // also covers a genuinely empty shelf
		sy.Loop(ctx, interval, reload)
	}()

	select {
	case <-ready:
		return nil
	case err := <-failed:
		return fmt.Errorf("initial shelf harvest: %w", err)
	}
}

// NewCache returns an empty Cache that loads its books with fetch.
func NewCache(fetch func() ([]goodreads.Book, error)) *Cache {
	return &Cache{fetch: fetch}
}

// Handler serves the random-book page at / and liveness at /healthz.
func Handler(cache *Cache) http.Handler {
	mux := http.NewServeMux()
	mux.Handle("/", bookHandler(cache))
	mux.Handle("/healthz", healthHandler())
	return mux
}
