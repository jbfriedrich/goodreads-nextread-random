package shelfsync

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"goodreads-nextread/internal/goodreads"
	"goodreads-nextread/internal/goodreads/goodreadstest"
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

// --- syncer ---

type syncFixture struct {
	f     *goodreadstest.Fake
	store *store.Store
	s     *Syncer
	now   time.Time
}

func newSyncFixture(t *testing.T, n int) *syncFixture {
	t.Helper()
	fx := &syncFixture{f: goodreadstest.New(t, n), store: newTestStore(t), now: t0}
	fx.s = New(newClient(fx.f), fx.store, 80)
	fx.s.now = func() time.Time { return fx.now }
	return fx
}

func (fx *syncFixture) sync(t *testing.T) store.SyncRun {
	t.Helper()
	run, err := fx.s.Sync(context.Background(), false)
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
	fx.s.OnProgress = func() {
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
	fx.f.Set(func(f *goodreadstest.Fake) { f.FailAfter = 1 })

	if _, err := fx.s.Sync(context.Background(), false); err == nil {
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
	fx.s.OnProgress = func() { called = true }
	fx.f.Remove(0)
	fx.f.AddEntry()

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
	before, _ := fx.f.Stats()

	fx.now = fx.now.Add(6 * time.Hour)
	run := fx.sync(t)

	if run.Kind != "quick" || run.Status != "unchanged" {
		t.Errorf("run = %s/%s, want quick/unchanged", run.Kind, run.Status)
	}
	after, _ := fx.f.Stats()
	if after-before != 2 || run.Requests != 2 {
		t.Errorf("quick check used %d requests (reported %d), want 2", after-before, run.Requests)
	}
}

func TestSyncAddsNewBooksFromQuickCheckWithoutHarvest(t *testing.T) {
	fx := newSyncFixture(t, 150)
	fx.sync(t)
	a, b := fx.f.AddEntry(), fx.f.AddEntry()
	before, _ := fx.f.Stats()

	fx.now = fx.now.Add(6 * time.Hour)
	run := fx.sync(t)

	if run.Kind != "quick" || run.Status != "synced" || run.Added != 2 || run.Removed != 0 {
		t.Errorf("run = %+v, want quick/synced with 2 added", run)
	}
	if after, _ := fx.f.Stats(); after-before != 2 {
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
	gone := fx.f.Remove(5)
	fx.f.AddEntry()
	fx.f.AddEntry()

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
		fx.f.AddEntry()
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
	gone := fx.f.Remove(10)

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
	fx.f.Remove(3)
	fx.f.AddEntry()

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
	fx.s.fullEvery = DefaultFullEvery
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
	fx.f.Remove(0)
	fx.f.AddEntry()

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
	fx.f.Set(func(f *goodreadstest.Fake) { f.FailWidget = true })
	fx.f.AddEntry()

	fx.now = fx.now.Add(6 * time.Hour)
	if _, err := fx.s.Sync(context.Background(), false); err == nil {
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
	run, err := fx.s.Sync(context.Background(), true)
	if err != nil {
		t.Fatal(err)
	}
	if run.Kind != "full" {
		t.Errorf("forced run kind = %s, want full", run.Kind)
	}
}

func TestUntilNextSchedulesFromRunLog(t *testing.T) {
	fx := newSyncFixture(t, 5)
	if d := fx.s.UntilNext(6 * time.Hour); d != 0 {
		t.Errorf("untilNext with no runs = %v, want 0 (sync now)", d)
	}

	fx.sync(t) // finishes at t0
	fx.now = t0.Add(time.Hour)
	if d := fx.s.UntilNext(6 * time.Hour); d != 5*time.Hour {
		t.Errorf("untilNext 1h after a sync = %v, want 5h", d)
	}

	fx.now = t0.Add(7 * time.Hour)
	if d := fx.s.UntilNext(6 * time.Hour); d != 0 {
		t.Errorf("untilNext when overdue = %v, want 0", d)
	}
}

func TestUntilNextRetriesFailuresSooner(t *testing.T) {
	fx := newSyncFixture(t, 5)
	fx.store.RecordRun(store.SyncRun{StartedAt: t0, FinishedAt: t0, Kind: "full", Status: "failed", Err: "x"})
	if d := fx.s.UntilNext(6 * time.Hour); d != time.Hour {
		t.Errorf("untilNext after a failure = %v, want 1h", d)
	}
}

func TestSyncLoopRunsAndNotifies(t *testing.T) {
	fx := newSyncFixture(t, 5)
	fx.s.now = time.Now

	synced := make(chan struct{}, 8)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go fx.s.Loop(ctx, time.Millisecond, func() { synced <- struct{}{} })

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
