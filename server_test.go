package main

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestShelfCacheFetchesOnceWithinTTL(t *testing.T) {
	calls := 0
	now := time.Unix(1000, 0)
	c := &shelfCache{
		ttl: time.Minute,
		now: func() time.Time { return now },
		fetch: func() ([]Book, error) {
			calls++
			return []Book{{Title: "A"}}, nil
		},
	}

	for i := 0; i < 3; i++ {
		if _, err := c.get(); err != nil {
			t.Fatalf("get: %v", err)
		}
	}
	if calls != 1 {
		t.Errorf("fetch called %d times within TTL, want 1", calls)
	}
}

func TestShelfCacheRefetchesAfterTTL(t *testing.T) {
	calls := 0
	now := time.Unix(1000, 0)
	c := &shelfCache{
		ttl: time.Minute,
		now: func() time.Time { return now },
		fetch: func() ([]Book, error) {
			calls++
			return []Book{{Title: "A"}}, nil
		},
	}

	if _, err := c.get(); err != nil {
		t.Fatal(err)
	}
	now = now.Add(2 * time.Minute)
	if _, err := c.get(); err != nil {
		t.Fatal(err)
	}
	if calls != 2 {
		t.Errorf("fetch called %d times across TTL boundary, want 2", calls)
	}
}

func TestShelfCacheServesStaleOnRefreshError(t *testing.T) {
	now := time.Unix(1000, 0)
	fail := false
	c := &shelfCache{
		ttl: time.Minute,
		now: func() time.Time { return now },
		fetch: func() ([]Book, error) {
			if fail {
				return nil, errors.New("network down")
			}
			return []Book{{Title: "cached"}}, nil
		},
	}

	if _, err := c.get(); err != nil {
		t.Fatal(err)
	}
	now = now.Add(2 * time.Minute)
	fail = true

	books, err := c.get()
	if err != nil {
		t.Fatalf("expected stale data served without error, got %v", err)
	}
	if len(books) != 1 || books[0].Title != "cached" {
		t.Errorf("expected stale cached book, got %v", books)
	}
}

func TestShelfCacheErrorsWhenColdFetchFails(t *testing.T) {
	now := time.Unix(1000, 0)
	c := &shelfCache{
		ttl:   time.Minute,
		now:   func() time.Time { return now },
		fetch: func() ([]Book, error) { return nil, errors.New("boom") },
	}
	if _, err := c.get(); err == nil {
		t.Fatal("expected error on cold fetch failure, got nil")
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
	cache := &shelfCache{
		ttl:   time.Minute,
		now:   time.Now,
		fetch: func() ([]Book, error) { return []Book{{Title: "Only Book", BookID: "1"}}, nil },
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

func TestBookHandlerReturns503OnFetchFailure(t *testing.T) {
	cache := &shelfCache{
		ttl:   time.Minute,
		now:   time.Now,
		fetch: func() ([]Book, error) { return nil, errors.New("down") },
	}
	h := bookHandler(cache)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))

	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503", rec.Code)
	}
}
