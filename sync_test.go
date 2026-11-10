package main

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestShelfCountFollowsProfileRedirect(t *testing.T) {
	f := newFakeGoodreads(t, 42)
	n, err := f.client().shelfCount(context.Background())
	if err != nil {
		t.Fatalf("shelfCount: %v", err)
	}
	if n != 42 {
		t.Errorf("shelfCount = %d, want 42", n)
	}
}

func TestHarvestCollectsWholeShelfBeyondWidgetLimit(t *testing.T) {
	f := newFakeGoodreads(t, 250) // well over the widget's 100-book cap
	res, err := f.client().harvest(context.Background(), 80, nil)
	if err != nil {
		t.Fatalf("harvest: %v", err)
	}
	if !res.Complete {
		t.Fatalf("harvest incomplete: found %d of %d", len(res.Books), res.Expected)
	}
	if len(res.Books) != 250 || res.Expected != 250 {
		t.Errorf("found %d, expected %d; want 250 and 250", len(res.Books), res.Expected)
	}

	requests, _ := f.stats()
	if res.Requests != requests {
		t.Errorf("harvest reported %d requests, server saw %d", res.Requests, requests)
	}
	if res.Requests > 80 {
		t.Errorf("harvest used %d requests, cap is 80", res.Requests)
	}
	for id, n := range f.widgetIDs {
		if n > 1 {
			t.Errorf("widget_id %q reused %d times (Goodreads caches per widget_id)", id, n)
		}
	}
}

func TestHarvestStopsEarlyOnSmallShelf(t *testing.T) {
	f := newFakeGoodreads(t, 30)
	res, err := f.client().harvest(context.Background(), 80, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !res.Complete || len(res.Books) != 30 {
		t.Fatalf("complete=%v found=%d, want a complete harvest of 30", res.Complete, len(res.Books))
	}
	// profile (redirect + page) + one widget call (all 30 fit) + final
	// recount, which goes straight to the remembered profile path.
	if res.Requests != 4 {
		t.Errorf("Requests = %d, want 4", res.Requests)
	}
}

func TestHarvestRespectsRequestCap(t *testing.T) {
	f := newFakeGoodreads(t, 250)
	res, err := f.client().harvest(context.Background(), 4, nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.Complete {
		t.Error("harvest with a 4-request cap reported complete for a 250-book shelf")
	}
	if requests, _ := f.stats(); requests > 4 {
		t.Errorf("server saw %d requests, cap is 4", requests)
	}
	if len(res.Books) == 0 {
		t.Error("partial harvest returned no books")
	}
}

func TestHarvestEmptyShelf(t *testing.T) {
	f := newFakeGoodreads(t, 0)
	res, err := f.client().harvest(context.Background(), 80, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !res.Complete || len(res.Books) != 0 {
		t.Errorf("complete=%v found=%d, want a complete empty harvest", res.Complete, len(res.Books))
	}
}

func TestHarvestCountMismatchIsIncomplete(t *testing.T) {
	f := newFakeGoodreads(t, 20)
	f.set(func(f *fakeGoodreads) { f.countDelta = 1 }) // profile claims one more than exists
	res, err := f.client().harvest(context.Background(), 10, nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.Complete {
		t.Error("harvest that could not reach the profile count reported complete")
	}
}

func TestHarvestFailsWhenWidgetReturnsNothingForNonEmptyShelf(t *testing.T) {
	f := newFakeGoodreads(t, 20)
	f.set(func(f *fakeGoodreads) { f.emptyWidget = true })
	if _, err := f.client().harvest(context.Background(), 80, nil); err == nil {
		t.Fatal("expected an error when the widget returns no books but the profile reports 20")
	}
}

func TestHarvestRetriesThrottledRequestInPlace(t *testing.T) {
	f := newFakeGoodreads(t, 250)
	f.set(func(f *fakeGoodreads) { f.throttle = 2 }) // second widget call gets 429 twice
	var waits []time.Duration
	c := f.client()
	c.sleep = func(_ context.Context, d time.Duration) error { waits = append(waits, d); return nil }

	res, err := c.harvest(context.Background(), 80, nil)
	if err != nil {
		t.Fatalf("harvest failed on a 429 instead of retrying: %v", err)
	}
	if !res.Complete {
		t.Errorf("harvest incomplete after retries: %d of %d", len(res.Books), res.Expected)
	}
	if len(waits) != 2 || waits[0] != time.Second {
		t.Errorf("waits = %v, want two waits honouring Retry-After: 1", waits)
	}
}

func TestHarvestPropagatesHTTPErrors(t *testing.T) {
	f := newFakeGoodreads(t, 20)
	f.set(func(f *fakeGoodreads) { f.failWidget = true })
	if _, err := f.client().harvest(context.Background(), 80, nil); err == nil {
		t.Fatal("expected an error when the widget returns HTTP 500")
	}
}

func TestHarvestReportsEachBatchOfNewBooks(t *testing.T) {
	f := newFakeGoodreads(t, 250)
	var batches []int
	seen := map[string]bool{}
	res, err := f.client().harvest(context.Background(), 80, func(batch []Book) error {
		batches = append(batches, len(batch))
		for _, b := range batch {
			if seen[b.ReviewID] {
				t.Errorf("review %s reported in more than one batch", b.ReviewID)
			}
			seen[b.ReviewID] = true
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(batches) < 2 || batches[0] != 100 {
		t.Errorf("batches = %v, want the first widget call's 100 books first", batches)
	}
	if len(seen) != len(res.Books) || len(seen) != 250 {
		t.Errorf("batches covered %d books, harvest found %d, want 250", len(seen), len(res.Books))
	}
}

func TestHarvestStopsWhenBatchCallbackFails(t *testing.T) {
	f := newFakeGoodreads(t, 250)
	_, err := f.client().harvest(context.Background(), 80, func([]Book) error {
		return errors.New("disk full")
	})
	if err == nil || !strings.Contains(err.Error(), "disk full") {
		t.Fatalf("err = %v, want the callback's error", err)
	}
	if _, calls := f.stats(); calls != 1 {
		t.Errorf("widget called %d times after the callback failed, want 1", calls)
	}
}

// --- syncer ---

type syncFixture struct {
	f     *fakeGoodreads
	store *store
	s     *syncer
	now   time.Time
}

func newSyncFixture(t *testing.T, n int) *syncFixture {
	t.Helper()
	fx := &syncFixture{f: newFakeGoodreads(t, n), store: newTestStore(t), now: t0}
	fx.s = &syncer{
		gr:          fx.f.client(),
		store:       fx.store,
		maxRequests: 80,
		fullEvery:   defaultFullEvery,
		now:         func() time.Time { return fx.now },
	}
	return fx
}

func (fx *syncFixture) sync(t *testing.T) syncRun {
	t.Helper()
	run, err := fx.s.sync(context.Background(), false)
	if err != nil {
		t.Fatalf("sync: %v", err)
	}
	return run
}

func TestSyncFirstRunHarvestsEverything(t *testing.T) {
	fx := newSyncFixture(t, 150)
	run := fx.sync(t)

	if run.Kind != "full" || run.Status != "synced" {
		t.Errorf("run = %s/%s, want full/synced", run.Kind, run.Status)
	}
	if run.Added != 150 || run.Expected != 150 || run.Found != 150 {
		t.Errorf("run = %+v", run)
	}
	if n, _ := fx.store.Count(); n != 150 {
		t.Errorf("stored %d books, want 150", n)
	}
	if last, ok, _ := fx.store.LastRun(); !ok || last.Status != "synced" {
		t.Errorf("run not recorded: %+v", last)
	}
}

func TestSyncInitialHarvestSavesProgressively(t *testing.T) {
	fx := newSyncFixture(t, 250)
	var counts []int
	fx.s.onProgress = func() {
		n, _ := fx.store.Count()
		counts = append(counts, n)
	}

	run := fx.sync(t)

	if len(counts) < 2 || counts[0] != 100 {
		t.Errorf("stored counts at each progress call = %v, want 100 after the first batch", counts)
	}
	if run.Status != "synced" || run.Added != 250 || run.Removed != 0 {
		t.Errorf("run = %+v, want synced with 250 added", run)
	}
}

func TestSyncInitialHarvestKeepsProgressOnFailure(t *testing.T) {
	fx := newSyncFixture(t, 250)
	fx.f.set(func(f *fakeGoodreads) { f.failAfter = 1 })

	if _, err := fx.s.sync(context.Background(), false); err == nil {
		t.Fatal("expected the sync to fail")
	}
	// The first batch was saved and stays servable.
	if n, _ := fx.store.Count(); n != 100 {
		t.Errorf("stored %d books after a failed initial harvest, want the first 100", n)
	}
}

func TestSyncLaterHarvestDoesNotReportProgress(t *testing.T) {
	// Progressive saving is only for an empty store; a later harvest must stay
	// all-or-nothing so a failure cannot leave a half-reconciled shelf.
	fx := newSyncFixture(t, 150)
	fx.sync(t)
	called := false
	fx.s.onProgress = func() { called = true }
	fx.f.remove(0)
	fx.f.addEntry()

	fx.now = fx.now.Add(6 * time.Hour)
	if run := fx.sync(t); run.Kind != "full" || run.Added != 1 || run.Removed != 1 {
		t.Fatalf("run = %+v, want a full harvest adding 1 and removing 1", run)
	}
	if called {
		t.Error("onProgress called during a harvest of a non-empty store")
	}
}

func TestSyncUnchangedShelfUsesQuickCheck(t *testing.T) {
	fx := newSyncFixture(t, 150)
	fx.sync(t)
	before, _ := fx.f.stats()

	fx.now = fx.now.Add(6 * time.Hour)
	run := fx.sync(t)

	if run.Kind != "quick" || run.Status != "unchanged" {
		t.Errorf("run = %s/%s, want quick/unchanged", run.Kind, run.Status)
	}
	after, _ := fx.f.stats()
	if after-before != 2 || run.Requests != 2 {
		t.Errorf("quick check used %d requests (reported %d), want 2", after-before, run.Requests)
	}
}

func TestSyncAddsNewBooksFromQuickCheckWithoutHarvest(t *testing.T) {
	fx := newSyncFixture(t, 150)
	fx.sync(t)
	a, b := fx.f.addEntry(), fx.f.addEntry()
	before, _ := fx.f.stats()

	fx.now = fx.now.Add(6 * time.Hour)
	run := fx.sync(t)

	if run.Kind != "quick" || run.Status != "synced" || run.Added != 2 || run.Removed != 0 {
		t.Errorf("run = %+v, want quick/synced with 2 added", run)
	}
	if after, _ := fx.f.stats(); after-before != 2 {
		t.Errorf("used %d requests, want 2 (no full harvest)", after-before)
	}
	if ok, _ := fx.store.HasAll([]string{a.ReviewID, b.ReviewID}); !ok {
		t.Error("new books not stored")
	}
	if n, _ := fx.store.Count(); n != 152 {
		t.Errorf("stored %d, want 152", n)
	}
}

func TestSyncAdditionPlusRemovalNeedsFullHarvest(t *testing.T) {
	fx := newSyncFixture(t, 150)
	fx.sync(t)
	gone := fx.f.remove(5)
	fx.f.addEntry()
	fx.f.addEntry()

	fx.now = fx.now.Add(6 * time.Hour)
	run := fx.sync(t)

	if run.Kind != "full" || run.Added != 2 || run.Removed != 1 {
		t.Errorf("run = %+v, want a full harvest with 2 added and 1 removed", run)
	}
	if ok, _ := fx.store.HasAll([]string{gone.ReviewID}); ok {
		t.Error("removed book still stored")
	}
}

func TestSyncFullWindowOfNewBooksNeedsFullHarvest(t *testing.T) {
	// If all 100 newest entries are new there may be more beyond the window,
	// so the quick check cannot know it has seen every addition.
	fx := newSyncFixture(t, 150)
	fx.sync(t)
	for i := 0; i < 100; i++ {
		fx.f.addEntry()
	}

	fx.now = fx.now.Add(6 * time.Hour)
	run := fx.sync(t)

	if run.Kind != "full" || run.Added != 100 {
		t.Errorf("run = %+v, want a full harvest adding 100", run)
	}
}

func TestSyncDetectsRemovedBook(t *testing.T) {
	fx := newSyncFixture(t, 150)
	fx.sync(t)
	gone := fx.f.remove(10)

	fx.now = fx.now.Add(6 * time.Hour)
	run := fx.sync(t)

	if run.Status != "synced" || run.Removed != 1 || run.Added != 0 {
		t.Errorf("run = %+v, want synced with 1 removed", run)
	}
	if ok, _ := fx.store.HasAll([]string{gone.ReviewID}); ok {
		t.Error("removed book still stored")
	}
}

func TestSyncDetectsSwapWithUnchangedCount(t *testing.T) {
	fx := newSyncFixture(t, 150)
	fx.sync(t)
	fx.f.remove(3)
	fx.f.addEntry()

	fx.now = fx.now.Add(6 * time.Hour)
	run := fx.sync(t)

	if run.Added != 1 || run.Removed != 1 {
		t.Errorf("run = %+v, want 1 added and 1 removed", run)
	}
	if n, _ := fx.store.Count(); n != 150 {
		t.Errorf("stored %d, want 150", n)
	}
}

func TestSyncRunsFullHarvestWeeklyAsSafetyNet(t *testing.T) {
	fx := newSyncFixture(t, 150)
	fx.s.fullEvery = defaultFullEvery
	fx.sync(t)

	fx.now = fx.now.Add(25 * time.Hour)
	if run := fx.sync(t); run.Kind != "quick" {
		t.Errorf("run kind = %s after 25h, want quick (safety net is weekly)", run.Kind)
	}
	fx.now = t0.Add(7*24*time.Hour + time.Minute)
	if run := fx.sync(t); run.Kind != "full" {
		t.Errorf("run kind = %s after a week, want full", run.Kind)
	}
}

func TestSyncPartialHarvestAddsButNeverDeletes(t *testing.T) {
	fx := newSyncFixture(t, 150)
	fx.sync(t)
	fx.f.remove(0)
	fx.f.addEntry()

	fx.s.maxRequests = 3 // too few to cover the shelf
	fx.now = fx.now.Add(6 * time.Hour)
	run := fx.sync(t)

	if run.Status != "partial" || run.Removed != 0 {
		t.Errorf("run = %+v, want partial with nothing removed", run)
	}
	if n, _ := fx.store.Count(); n < 150 {
		t.Errorf("stored %d books; a partial harvest must not shrink the shelf", n)
	}
}

func TestSyncFailureIsRecordedAndLeavesShelfIntact(t *testing.T) {
	fx := newSyncFixture(t, 150)
	fx.sync(t)
	fx.f.set(func(f *fakeGoodreads) { f.failWidget = true })
	fx.f.addEntry()

	fx.now = fx.now.Add(6 * time.Hour)
	if _, err := fx.s.sync(context.Background(), false); err == nil {
		t.Fatal("expected sync to fail")
	}
	if n, _ := fx.store.Count(); n != 150 {
		t.Errorf("stored %d after failed sync, want 150", n)
	}
	last, _, _ := fx.store.LastRun()
	if last.Status != "failed" || last.Err == "" {
		t.Errorf("last run = %+v, want a recorded failure", last)
	}
}

func TestSyncForceFull(t *testing.T) {
	fx := newSyncFixture(t, 50)
	fx.sync(t)
	fx.now = fx.now.Add(time.Hour)
	run, err := fx.s.sync(context.Background(), true)
	if err != nil {
		t.Fatal(err)
	}
	if run.Kind != "full" {
		t.Errorf("forced run kind = %s, want full", run.Kind)
	}
}
