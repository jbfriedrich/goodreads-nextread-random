package main

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestShelfCacheGetIsEmptyBeforeRefresh(t *testing.T) {
	c := &shelfCache{fetch: func() ([]Book, error) { return []Book{{Title: "A"}}, nil }}
	if books := c.get(); len(books) != 0 {
		t.Errorf("expected empty cache before any refresh, got %v", books)
	}
}

func TestShelfCacheRefreshPopulatesAndSwaps(t *testing.T) {
	list := []Book{{Title: "first"}}
	c := &shelfCache{fetch: func() ([]Book, error) { return list, nil }}

	if err := c.refresh(); err != nil {
		t.Fatalf("first refresh: %v", err)
	}
	if got := c.get(); len(got) != 1 || got[0].Title != "first" {
		t.Fatalf("after first refresh got %v", got)
	}

	// A later refresh should swap in the new shelf.
	list = []Book{{Title: "second"}, {Title: "third"}}
	if err := c.refresh(); err != nil {
		t.Fatalf("second refresh: %v", err)
	}
	if got := c.get(); len(got) != 2 || got[0].Title != "second" {
		t.Errorf("after second refresh got %v", got)
	}
}

func TestShelfCacheRefreshKeepsPreviousListOnError(t *testing.T) {
	fail := false
	c := &shelfCache{fetch: func() ([]Book, error) {
		if fail {
			return nil, errors.New("network down")
		}
		return []Book{{Title: "good"}}, nil
	}}

	if err := c.refresh(); err != nil {
		t.Fatal(err)
	}
	fail = true
	if err := c.refresh(); err == nil {
		t.Fatal("expected error from failing refresh, got nil")
	}
	// The last known-good list must still be served.
	if got := c.get(); len(got) != 1 || got[0].Title != "good" {
		t.Errorf("expected previous list retained on error, got %v", got)
	}
}

func TestShelfCacheRunRefreshesUntilCancelled(t *testing.T) {
	calls := make(chan struct{}, 8)
	c := &shelfCache{fetch: func() ([]Book, error) {
		calls <- struct{}{}
		return []Book{{Title: "x"}}, nil
	}}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go c.run(ctx, time.Millisecond)

	// The ticker should drive several refreshes.
	for i := 0; i < 3; i++ {
		select {
		case <-calls:
		case <-time.After(time.Second):
			t.Fatalf("expected background refresh %d to fire", i+1)
		}
	}
}

func TestRenderPageContainsBookInfo(t *testing.T) {
	b := Book{
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
	b := Book{Title: "A <script>alert(1)</script> Tale", BookID: "1"}
	html := renderPage(b)
	if strings.Contains(html, "<script>alert(1)</script>") {
		t.Error("title was not HTML-escaped")
	}
}

func TestBookHandlerServesRandomBook(t *testing.T) {
	cache := &shelfCache{fetch: func() ([]Book, error) { return []Book{{Title: "Only Book", BookID: "1"}}, nil }}
	if err := cache.refresh(); err != nil {
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
	cache := &shelfCache{fetch: func() ([]Book, error) { return nil, errors.New("down") }}
	h := bookHandler(cache)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))

	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503", rec.Code)
	}
}
