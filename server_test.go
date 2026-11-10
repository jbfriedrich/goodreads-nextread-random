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

func newStartFixture(t *testing.T, n int) (*syncFixture, *shelfCache, context.Context) {
	t.Helper()
	fx := newSyncFixture(t, n)
	fx.s.now = time.Now
	cache := &shelfCache{fetch: fx.store.AllBooks}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	return fx, cache, ctx
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

func TestStartSyncServesAfterFirstBatch(t *testing.T) {
	fx, cache, ctx := newStartFixture(t, 250)
	gate := make(chan struct{})
	fx.f.set(func(f *fakeGoodreads) { f.gate = gate })

	// Every widget call after the first is held, so startSync can only return
	// because the first batch is servable, not because the harvest finished.
	if err := startSync(ctx, fx.s, cache, time.Hour); err != nil {
		t.Fatalf("startSync: %v", err)
	}
	if n := len(cache.get()); n != 100 {
		t.Errorf("cache has %d books when serving starts, want the first 100", n)
	}

	close(gate)
	waitFor(t, "the background harvest to fill the cache", func() bool { return len(cache.get()) == 250 })
}

func TestStartSyncFailsWhenNothingCouldBeFetched(t *testing.T) {
	fx, cache, ctx := newStartFixture(t, 50)
	fx.f.set(func(f *fakeGoodreads) { f.failWidget = true })

	if err := startSync(ctx, fx.s, cache, time.Hour); err == nil {
		t.Fatal("expected an error when the first harvest gets no books")
	}
}

func TestStartSyncWithStoredShelfDoesNotWaitForGoodreads(t *testing.T) {
	fx, cache, ctx := newStartFixture(t, 50)
	fx.store.Reconcile([]Book{{ReviewID: "1", Title: "Stored"}}, true, t0)
	fx.store.RecordRun(syncRun{StartedAt: time.Now(), FinishedAt: time.Now(), Kind: "full", Status: "synced"})
	if err := cache.refresh(); err != nil {
		t.Fatal(err)
	}

	if err := startSync(ctx, fx.s, cache, time.Hour); err != nil {
		t.Fatalf("startSync: %v", err)
	}
	if requests, _ := fx.f.stats(); requests != 0 {
		t.Errorf("startSync made %d requests before serving a stored shelf, want 0", requests)
	}
}

func TestUntilNextSchedulesFromRunLog(t *testing.T) {
	fx := newSyncFixture(t, 5)
	if d := fx.s.untilNext(6 * time.Hour); d != 0 {
		t.Errorf("untilNext with no runs = %v, want 0 (sync now)", d)
	}

	fx.sync(t) // finishes at t0
	fx.now = t0.Add(time.Hour)
	if d := fx.s.untilNext(6 * time.Hour); d != 5*time.Hour {
		t.Errorf("untilNext 1h after a sync = %v, want 5h", d)
	}

	fx.now = t0.Add(7 * time.Hour)
	if d := fx.s.untilNext(6 * time.Hour); d != 0 {
		t.Errorf("untilNext when overdue = %v, want 0", d)
	}
}

func TestUntilNextRetriesFailuresSooner(t *testing.T) {
	fx := newSyncFixture(t, 5)
	fx.store.RecordRun(syncRun{StartedAt: t0, FinishedAt: t0, Kind: "full", Status: "failed", Err: "x"})
	if d := fx.s.untilNext(6 * time.Hour); d != time.Hour {
		t.Errorf("untilNext after a failure = %v, want 1h", d)
	}
}

func TestSyncLoopRunsAndNotifies(t *testing.T) {
	fx := newSyncFixture(t, 5)
	fx.s.now = time.Now

	synced := make(chan struct{}, 8)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go fx.s.loop(ctx, time.Millisecond, func() { synced <- struct{}{} })

	for i := 0; i < 2; i++ {
		select {
		case <-synced:
		case <-time.After(5 * time.Second):
			t.Fatalf("expected sync %d to run and notify", i+1)
		}
	}
	if n, _ := fx.store.Count(); n != 5 {
		t.Errorf("stored %d books, want 5", n)
	}
}
