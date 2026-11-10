package main

import (
	"path/filepath"
	"sort"
	"testing"
	"time"
)

var t0 = time.Date(2026, 11, 9, 12, 0, 0, 0, time.UTC)

func newTestStore(t *testing.T) *store {
	t.Helper()
	s, err := openStore(filepath.Join(t.TempDir(), "sub", "test.db"))
	if err != nil {
		t.Fatalf("openStore: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func reviewIDs(t *testing.T, s *store) []string {
	t.Helper()
	books, err := s.AllBooks()
	if err != nil {
		t.Fatalf("AllBooks: %v", err)
	}
	ids := make([]string, 0, len(books))
	for _, b := range books {
		ids = append(ids, b.ReviewID)
	}
	sort.Strings(ids)
	return ids
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestReconcileAddsBooks(t *testing.T) {
	s := newTestStore(t)
	added, removed, err := s.Reconcile([]Book{
		{ReviewID: "1", Title: "One", Author: "A", BookID: "11", ImageURL: "https://x/1.jpg"},
		{ReviewID: "2", Title: "Two", Author: "B"},
	}, true, t0)
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if added != 2 || removed != 0 {
		t.Errorf("added=%d removed=%d, want 2 and 0", added, removed)
	}

	books, err := s.AllBooks()
	if err != nil {
		t.Fatal(err)
	}
	byID := map[string]Book{}
	for _, b := range books {
		byID[b.ReviewID] = b
	}
	if got := byID["1"]; got.Title != "One" || got.Author != "A" || got.BookID != "11" || got.ImageURL != "https://x/1.jpg" {
		t.Errorf("book 1 = %+v", got)
	}
	if n, _ := s.Count(); n != 2 {
		t.Errorf("Count = %d, want 2", n)
	}
}

func TestReconcileCompleteDeletesBooksNoLongerOnShelf(t *testing.T) {
	s := newTestStore(t)
	s.Reconcile([]Book{{ReviewID: "1"}, {ReviewID: "2"}, {ReviewID: "3"}}, true, t0)

	added, removed, err := s.Reconcile([]Book{{ReviewID: "2"}, {ReviewID: "4"}}, true, t0.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if added != 1 || removed != 2 {
		t.Errorf("added=%d removed=%d, want 1 and 2", added, removed)
	}
	if got := reviewIDs(t, s); !equalStrings(got, []string{"2", "4"}) {
		t.Errorf("review ids = %v, want [2 4]", got)
	}
}

func TestReconcilePartialNeverDeletes(t *testing.T) {
	s := newTestStore(t)
	s.Reconcile([]Book{{ReviewID: "1"}, {ReviewID: "2"}}, true, t0)

	added, removed, err := s.Reconcile([]Book{{ReviewID: "3"}}, false, t0.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if added != 1 || removed != 0 {
		t.Errorf("added=%d removed=%d, want 1 and 0", added, removed)
	}
	if got := reviewIDs(t, s); !equalStrings(got, []string{"1", "2", "3"}) {
		t.Errorf("review ids = %v, want [1 2 3]", got)
	}
}

func TestReconcileCompleteWithEmptyShelfClearsIt(t *testing.T) {
	// An empty result is only passed as complete when the profile count
	// confirmed the shelf really is empty.
	s := newTestStore(t)
	s.Reconcile([]Book{{ReviewID: "1"}}, true, t0)
	if _, removed, err := s.Reconcile(nil, true, t0); err != nil || removed != 1 {
		t.Fatalf("removed=%d err=%v, want 1 and nil", removed, err)
	}
	if n, _ := s.Count(); n != 0 {
		t.Errorf("Count = %d, want 0", n)
	}
}

func TestReconcileDeduplicatesInput(t *testing.T) {
	s := newTestStore(t)
	added, _, err := s.Reconcile([]Book{{ReviewID: "1"}, {ReviewID: "1"}}, true, t0)
	if err != nil || added != 1 {
		t.Fatalf("added=%d err=%v, want 1 and nil", added, err)
	}
}

func TestReconcilePreservesEnrichment(t *testing.T) {
	s := newTestStore(t)
	s.Reconcile([]Book{{ReviewID: "1", Title: "Old title"}}, true, t0)
	if err := s.SaveEnrichment("1", Book{
		BookID: "11", Description: "A blurb", AverageRating: "4.10", NumPages: "300",
		Published: "2024", ISBN: "9780000000001", ImageURL: "https://ol/cover.jpg",
	}, t0); err != nil {
		t.Fatalf("SaveEnrichment: %v", err)
	}

	// A later scrape (no cover, no book id in the widget) must not wipe it.
	s.Reconcile([]Book{{ReviewID: "1", Title: "New title", Author: "A"}}, true, t0.Add(time.Hour))

	books, _ := s.AllBooks()
	b := books[0]
	if b.Title != "New title" || b.Author != "A" {
		t.Errorf("widget fields not refreshed: %+v", b)
	}
	if b.BookID != "11" || b.Description != "A blurb" || b.AverageRating != "4.10" ||
		b.NumPages != "300" || b.Published != "2024" || b.ISBN != "9780000000001" ||
		b.ImageURL != "https://ol/cover.jpg" {
		t.Errorf("enrichment lost: %+v", b)
	}
}

func TestReconcileWidgetBookIDOnlyFillsUnenrichedBooks(t *testing.T) {
	s := newTestStore(t)
	s.Reconcile([]Book{{ReviewID: "1", BookID: "old"}, {ReviewID: "2", BookID: "a"}}, true, t0)
	s.SaveEnrichment("1", Book{BookID: "new", Description: "d"}, t0)

	s.Reconcile([]Book{{ReviewID: "1", BookID: "old"}, {ReviewID: "2", BookID: "b"}}, true, t0)
	books, _ := s.AllBooks()
	got := map[string]string{}
	for _, b := range books {
		got[b.ReviewID] = b.BookID
	}
	if got["1"] != "new" {
		t.Errorf("enriched book id = %q, want the enrichment's %q", got["1"], "new")
	}
	if got["2"] != "b" {
		t.Errorf("unenriched book id = %q, want the widget's latest %q", got["2"], "b")
	}
}

func TestMarkMissingClearsBookIDAndFinishesEnrichment(t *testing.T) {
	s := newTestStore(t)
	s.Reconcile([]Book{{ReviewID: "1", BookID: "dead"}}, true, t0)
	if err := s.MarkMissing("1", t0); err != nil {
		t.Fatal(err)
	}
	books, _ := s.AllBooks()
	if books[0].BookID != "" {
		t.Errorf("BookID = %q, want cleared", books[0].BookID)
	}
	if n, _ := s.PendingEnrichmentCount(); n != 0 {
		t.Errorf("pending = %d, want 0", n)
	}
}

func TestReconcileWidgetCoverReplacesFallbackCover(t *testing.T) {
	s := newTestStore(t)
	s.Reconcile([]Book{{ReviewID: "1"}}, true, t0)
	s.SaveEnrichment("1", Book{ImageURL: "https://ol/cover.jpg"}, t0)

	// Goodreads later gets a real cover: it wins over the fallback.
	s.Reconcile([]Book{{ReviewID: "1", ImageURL: "https://gr/cover.jpg", BookID: "11"}}, true, t0)
	books, _ := s.AllBooks()
	if books[0].ImageURL != "https://gr/cover.jpg" {
		t.Errorf("ImageURL = %q, want the Goodreads cover", books[0].ImageURL)
	}
}

func TestSaveEnrichmentKeepsExistingValuesForEmptyFields(t *testing.T) {
	s := newTestStore(t)
	s.Reconcile([]Book{{ReviewID: "1", BookID: "11", ImageURL: "https://gr/cover.jpg"}}, true, t0)
	s.SaveEnrichment("1", Book{Description: "Blurb"}, t0)

	books, _ := s.AllBooks()
	if books[0].BookID != "11" || books[0].ImageURL != "https://gr/cover.jpg" || books[0].Description != "Blurb" {
		t.Errorf("got %+v", books[0])
	}
}

func TestPendingEnrichment(t *testing.T) {
	s := newTestStore(t)
	s.Reconcile([]Book{{ReviewID: "1"}, {ReviewID: "2"}, {ReviewID: "3"}}, true, t0)
	s.SaveEnrichment("1", Book{Description: "done"}, t0)
	// A permanent failure is saved with nothing found; it is done too.
	s.SaveEnrichment("2", Book{}, t0)

	pending, err := s.PendingEnrichment(10)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 1 || pending[0].ReviewID != "3" {
		t.Errorf("pending = %v, want only review 3", pending)
	}
	if n, _ := s.PendingEnrichmentCount(); n != 1 {
		t.Errorf("PendingEnrichmentCount = %d, want 1", n)
	}
	if pending, _ := s.PendingEnrichment(0); len(pending) != 0 {
		t.Errorf("limit 0 returned %d books", len(pending))
	}
}

func TestSettings(t *testing.T) {
	s := newTestStore(t)
	if v, err := s.Setting("k"); err != nil || v != "" {
		t.Fatalf("unset setting = %q, %v; want empty, nil", v, err)
	}
	if err := s.SetSetting("k", "one"); err != nil {
		t.Fatal(err)
	}
	s.SetSetting("k", "two")
	if v, _ := s.Setting("k"); v != "two" {
		t.Errorf("setting = %q, want two", v)
	}
}

func TestHasAll(t *testing.T) {
	s := newTestStore(t)
	s.Reconcile([]Book{{ReviewID: "1"}, {ReviewID: "2"}}, true, t0)

	if ok, err := s.HasAll([]string{"1", "2"}); err != nil || !ok {
		t.Errorf("HasAll(known) = %v, %v; want true", ok, err)
	}
	if ok, _ := s.HasAll([]string{"1", "9"}); ok {
		t.Error("HasAll with an unknown id = true, want false")
	}
	if ok, _ := s.HasAll(nil); !ok {
		t.Error("HasAll(nil) = false, want true")
	}
}

func TestRunLog(t *testing.T) {
	s := newTestStore(t)

	if _, ok, err := s.LastRun(); err != nil || ok {
		t.Fatalf("LastRun on empty log = ok %v err %v, want no run", ok, err)
	}
	if _, ok, _ := s.LastFullSync(); ok {
		t.Fatal("LastFullSync on empty log reported a run")
	}

	full := syncRun{StartedAt: t0, FinishedAt: t0.Add(time.Minute), Kind: "full", Status: "synced",
		Expected: 636, Found: 636, Added: 636, Requests: 45}
	quick := syncRun{StartedAt: t0.Add(6 * time.Hour), FinishedAt: t0.Add(6*time.Hour + time.Second),
		Kind: "quick", Status: "unchanged", Expected: 636, Found: 636, Requests: 2}
	failed := syncRun{StartedAt: t0.Add(12 * time.Hour), FinishedAt: t0.Add(12 * time.Hour),
		Kind: "full", Status: "failed", Err: "boom"}
	for _, r := range []syncRun{full, quick, failed} {
		if err := s.RecordRun(r); err != nil {
			t.Fatalf("RecordRun: %v", err)
		}
	}

	last, ok, err := s.LastRun()
	if err != nil || !ok {
		t.Fatalf("LastRun: ok %v err %v", ok, err)
	}
	if last.Status != "failed" || last.Err != "boom" || !last.FinishedAt.Equal(failed.FinishedAt) {
		t.Errorf("LastRun = %+v", last)
	}

	// Only a complete full harvest counts as the last full sync.
	at, ok, _ := s.LastFullSync()
	if !ok || !at.Equal(full.FinishedAt) {
		t.Errorf("LastFullSync = %v %v, want %v", at, ok, full.FinishedAt)
	}
}

func TestStorePersistsAcrossReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "persist.db")
	s, err := openStore(path)
	if err != nil {
		t.Fatal(err)
	}
	s.Reconcile([]Book{{ReviewID: "1", Title: "Kept"}}, true, t0)
	s.Close()

	s, err = openStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	books, _ := s.AllBooks()
	if len(books) != 1 || books[0].Title != "Kept" {
		t.Errorf("after reopen got %v", books)
	}
}
