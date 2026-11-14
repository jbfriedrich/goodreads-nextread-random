package web

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"goodreads-nextread/internal/goodreads"
	"goodreads-nextread/internal/goodreads/goodreadstest"
	"goodreads-nextread/internal/shelfsync"
	"goodreads-nextread/internal/store"
)

var t0 = time.Date(2026, 11, 9, 12, 0, 0, 0, time.UTC)

func newTestStore(t *testing.T) *store.Store {
	t.Helper()
	s, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

// newClient returns a Client for the fake's shelf that skips retry waits.
func newClient(f *goodreadstest.Fake) *goodreads.Client {
	return goodreads.NewClient(goodreads.Options{HTTP: f.Server.Client(), BaseURL: f.Server.URL,
		UserID: goodreadstest.UID, Shelf: "to-read", Sleep: goodreadstest.NoSleep})
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestShelfCacheGetIsEmptyBeforeRefresh(t *testing.T) {
	c := NewCache(func() ([]goodreads.Book, error) { return []goodreads.Book{{Title: "A"}}, nil })
	if books := c.Books(); len(books) != 0 {
		t.Errorf("expected empty cache before any refresh, got %v", books)
	}
}

func TestShelfCacheRefreshPopulatesAndSwaps(t *testing.T) {
	list := []goodreads.Book{{Title: "first"}}
	c := NewCache(func() ([]goodreads.Book, error) { return list, nil })

	if err := c.Refresh(); err != nil {
		t.Fatalf("first refresh: %v", err)
	}
	if got := c.Books(); len(got) != 1 || got[0].Title != "first" {
		t.Fatalf("after first refresh got %v", got)
	}

	// A later refresh should swap in the new shelf.
	list = []goodreads.Book{{Title: "second"}, {Title: "third"}}
	if err := c.Refresh(); err != nil {
		t.Fatalf("second refresh: %v", err)
	}
	if got := c.Books(); len(got) != 2 || got[0].Title != "second" {
		t.Errorf("after second refresh got %v", got)
	}
}

func TestShelfCacheRefreshKeepsPreviousListOnError(t *testing.T) {
	fail := false
	c := NewCache(func() ([]goodreads.Book, error) {
		if fail {
			return nil, errors.New("network down")
		}
		return []goodreads.Book{{Title: "good"}}, nil
	})

	if err := c.Refresh(); err != nil {
		t.Fatal(err)
	}
	fail = true
	if err := c.Refresh(); err == nil {
		t.Fatal("expected error from failing refresh, got nil")
	}
	// The last known-good list must still be served.
	if got := c.Books(); len(got) != 1 || got[0].Title != "good" {
		t.Errorf("expected previous list retained on error, got %v", got)
	}
}

func TestRenderPageContainsBookInfo(t *testing.T) {
	b := goodreads.Book{
		Title:         "The Book Eaters",
		Author:        "Sunyi Dean",
		AverageRating: "3.59",
		NumPages:      "298",
		Published:     "2022",
		BookID:        "58724745",
		ImageURL:      "https://example.com/cover.jpg",
		Description:   "Out on the Yorkshire Moors...",
	}
	html := renderPage(b)

	for _, want := range []string{
		"The Book Eaters",
		"Sunyi Dean",
		"3.59",
		"298",
		"2022",
		"https://www.goodreads.com/book/show/58724745",
		"https://example.com/cover.jpg",
		"Out on the Yorkshire Moors",
		"<!doctype html>",
	} {
		if !strings.Contains(html, want) {
			t.Errorf("rendered page missing %q", want)
		}
	}
}

func TestRenderPageEscapesTitle(t *testing.T) {
	b := goodreads.Book{Title: "A <script>alert(1)</script> Tale", BookID: "1"}
	html := renderPage(b)
	if strings.Contains(html, "<script>alert(1)</script>") {
		t.Error("title was not HTML-escaped")
	}
}

func TestBookHandlerServesRandomBook(t *testing.T) {
	cache := NewCache(func() ([]goodreads.Book, error) { return []goodreads.Book{{Title: "Only Book", BookID: "1"}}, nil })
	if err := cache.Refresh(); err != nil {
		t.Fatalf("priming cache: %v", err)
	}
	h := bookHandler(cache)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Errorf("Content-Type = %q", ct)
	}
	if !strings.Contains(rec.Body.String(), "Only Book") {
		t.Errorf("body missing book title:\n%s", rec.Body.String())
	}
}

func TestBookHandlerReturns503WhenListEmpty(t *testing.T) {
	// Cache never primed (e.g. startup fetch has not populated it yet).
	cache := NewCache(func() ([]goodreads.Book, error) { return nil, errors.New("down") })
	h := bookHandler(cache)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))

	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503", rec.Code)
	}
}

func TestHealthIsPlainOK(t *testing.T) {
	// /healthz is public (nginx proxies it), so it exposes nothing beyond
	// liveness; sync status goes to the logs only.
	rec := httptest.NewRecorder()
	healthHandler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))

	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", rec.Code)
	}
	if body := rec.Body.String(); body != "ok\n" {
		t.Errorf("body = %q, want %q", body, "ok\n")
	}
}

type startFixture struct {
	f     *goodreadstest.Fake
	store *store.Store
	s     *shelfsync.Syncer
}

func newStartFixture(t *testing.T, n int) (*startFixture, *Cache, context.Context) {
	t.Helper()
	fx := &startFixture{f: goodreadstest.New(t, n), store: newTestStore(t)}
	fx.s = shelfsync.New(newClient(fx.f), fx.store, shelfsync.Settings{MaxRequests: 80})
	cache := NewCache(fx.store.AllBooks)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	return fx, cache, ctx
}

func TestStartSyncServesAfterFirstBatch(t *testing.T) {
	fx, cache, ctx := newStartFixture(t, 250)
	gate := make(chan struct{})
	fx.f.Set(func(f *goodreadstest.Fake) { f.Gate = gate })

	// Every widget call after the first is held, so StartSync can only return
	// because the first batch is servable, not because the harvest finished.
	if err := StartSync(ctx, fx.s, cache, time.Hour); err != nil {
		t.Fatalf("StartSync: %v", err)
	}
	if n := len(cache.Books()); n != 100 {
		t.Errorf("cache has %d books when serving starts, want the first 100", n)
	}

	close(gate)
	waitFor(t, "the background harvest to fill the cache", func() bool { return len(cache.Books()) == 250 })
}

func TestStartSyncFailsWhenNothingCouldBeFetched(t *testing.T) {
	fx, cache, ctx := newStartFixture(t, 50)
	fx.f.Set(func(f *goodreadstest.Fake) { f.FailWidget = true })

	if err := StartSync(ctx, fx.s, cache, time.Hour); err == nil {
		t.Fatal("expected an error when the first harvest gets no books")
	}
}

func TestStartSyncWithStoredShelfDoesNotWaitForGoodreads(t *testing.T) {
	fx, cache, ctx := newStartFixture(t, 50)
	fx.store.Reconcile([]goodreads.Book{{ReviewID: "1", Title: "Stored"}}, true, t0)
	fx.store.RecordRun(store.SyncRun{StartedAt: time.Now(), FinishedAt: time.Now(), Kind: "full", Status: "synced"})
	if err := cache.Refresh(); err != nil {
		t.Fatal(err)
	}

	if err := StartSync(ctx, fx.s, cache, time.Hour); err != nil {
		t.Fatalf("StartSync: %v", err)
	}
	if requests, _ := fx.f.Stats(); requests != 0 {
		t.Errorf("StartSync made %d requests before serving a stored shelf, want 0", requests)
	}
}
