package shelfsync

import (
	"context"
	"fmt"
	"log"
	"time"

	"goodreads-nextread/internal/goodreads"
	"goodreads-nextread/internal/store"
)

// DefaultFullEvery is how often a full harvest runs even when quick checks
// see no change — a safety net for edits the quick check cannot see. The
// quick check's count arithmetic catches every case known so far, so weekly
// is enough.
const DefaultFullEvery = 7 * 24 * time.Hour

// failedRetryAfter caps the wait before retrying after a failed sync.
const failedRetryAfter = time.Hour

// Syncer keeps the stored shelf in line with Goodreads.
type Syncer struct {
	gr          *goodreads.Client
	store       *store.Store
	maxRequests int
	fullEvery   time.Duration
	now         func() time.Time

	// OnProgress, if set, is called each time a batch of an initial harvest
	// (into an empty store) has been saved, so the server can start serving
	// before the whole shelf is in.
	OnProgress func()
}

// New builds a Syncer that harvests with gr (at most maxRequests per full
// harvest) into st.
func New(gr *goodreads.Client, st *store.Store, maxRequests int) *Syncer {
	return &Syncer{
		gr:          gr,
		store:       st,
		maxRequests: maxRequests,
		fullEvery:   DefaultFullEvery,
		now:         time.Now,
	}
}

// sync runs one sync and records it in the run log. Usually it is a quick
// check (2 requests) of the profile count and the 100 newest entries:
//   - count == stored and all newest known: nothing changed;
//   - count == stored + the unknown newest (and not all 100 are unknown):
//     only additions, which are inserted directly;
//   - anything else (a removal, a swap, a window full of new books): a full
//     harvest of the whole shelf, then reconcile.
//
// A full harvest also runs when the store is empty, the last one is older than
// fullEvery, or forceFull is set. Books are only deleted when the harvest is
// complete; a failure leaves the store untouched.
func (s *Syncer) Sync(ctx context.Context, forceFull bool) (store.SyncRun, error) {
	run := store.SyncRun{StartedAt: s.now()}
	startRequests := s.gr.RequestCount()
	finish := func(err error) (store.SyncRun, error) {
		run.FinishedAt = s.now()
		run.Requests = s.gr.RequestCount() - startRequests
		if err != nil {
			run.Status = "failed"
			run.Err = err.Error()
		}
		if recErr := s.store.RecordRun(run); recErr != nil && err == nil {
			err = fmt.Errorf("recording sync run: %w", recErr)
		}
		return run, err
	}

	stored, err := s.store.Count()
	if err != nil {
		run.Kind = "quick"
		return finish(err)
	}
	lastFull, haveFull, err := s.store.LastFullSync()
	if err != nil {
		run.Kind = "quick"
		return finish(err)
	}

	needFull := forceFull || stored == 0 || !haveFull || s.now().Sub(lastFull) >= s.fullEvery
	if !needFull {
		run.Kind = "quick"
		expected, newest, err := s.gr.QuickCheck(ctx)
		if err != nil {
			return finish(err)
		}
		var unknown []goodreads.Book
		for _, b := range newest {
			known, err := s.store.HasAll([]string{b.ReviewID})
			if err != nil {
				return finish(err)
			}
			if !known {
				unknown = append(unknown, b)
			}
		}
		run.Expected = expected
		switch {
		case expected == stored && len(unknown) == 0:
			run.Status = "unchanged"
			run.Found = stored
			return finish(nil)
		case len(unknown) > 0 && len(unknown) < len(newest) && expected == stored+len(unknown):
			// Only additions: every one of them is in the newest window.
			if run.Added, _, err = s.store.Reconcile(unknown, false, s.now()); err != nil {
				return finish(fmt.Errorf("saving shelf: %w", err))
			}
			run.Status = "synced"
			run.Found = expected
			return finish(nil)
		}
		log.Printf("shelf changed (Goodreads reports %d, stored %d, %d new among the newest %d); running a full harvest",
			expected, stored, len(unknown), len(newest))
	}

	run.Kind = "full"
	// Into an empty store, save each batch as it arrives (add-only) so the
	// first books are servable within seconds and a failure keeps what was
	// fetched. Later harvests stay all-or-nothing: a half-reconciled shelf
	// would be worse than the previous complete one.
	var onBatch func([]goodreads.Book) error
	progressive := stored == 0
	if progressive {
		onBatch = func(batch []goodreads.Book) error {
			if _, _, err := s.store.Reconcile(batch, false, s.now()); err != nil {
				return fmt.Errorf("saving shelf: %w", err)
			}
			if s.OnProgress != nil {
				s.OnProgress()
			}
			return nil
		}
	}
	res, err := s.gr.Harvest(ctx, s.maxRequests, onBatch)
	run.Expected, run.Found = res.Expected, len(res.Books)
	if err != nil {
		return finish(err)
	}

	run.Added, run.Removed, err = s.store.Reconcile(res.Books, res.Complete, s.now())
	if err != nil {
		return finish(fmt.Errorf("saving shelf: %w", err))
	}
	if progressive {
		// The batches already inserted everything; count what is there.
		if run.Added, err = s.store.Count(); err != nil {
			return finish(err)
		}
	}
	run.Status = "synced"
	if !res.Complete {
		run.Status = "partial"
		log.Printf("warning: harvest found %d of %d books within %d requests; added new books but deleted none", run.Found, run.Expected, run.Requests)
	}
	return finish(nil)
}

// storeSummary describes the stored shelf for log lines.
func (s *Syncer) storeSummary() string {
	n, err := s.store.Count()
	if err != nil {
		return "stored count unavailable: " + err.Error()
	}
	pending, _ := s.store.PendingEnrichmentCount()
	return fmt.Sprintf("%d books stored, %d awaiting enrichment", n, pending)
}

// untilNext returns how long to wait before the next scheduled sync, based on
// the run log so that restarts do not cause extra scrapes. After a failure the
// wait is capped at failedRetryAfter.
func (s *Syncer) UntilNext(interval time.Duration) time.Duration {
	last, ok, err := s.store.LastRun()
	if err != nil || !ok {
		return 0
	}
	if last.Status == "failed" && interval > failedRetryAfter {
		interval = failedRetryAfter
	}
	if d := last.FinishedAt.Add(interval).Sub(s.now()); d > 0 {
		return d
	}
	return 0
}

// loop runs scheduled syncs until ctx is cancelled, calling onSynced after
// every successful run so the in-memory cache can reload from the store.
func (s *Syncer) Loop(ctx context.Context, interval time.Duration, onSynced func()) {
	for {
		wait := s.UntilNext(interval)
		if wait > 0 {
			log.Printf("next shelf sync in %s", wait.Round(time.Second))
		}
		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}

		run, err := s.Sync(ctx, false)
		if ctx.Err() != nil {
			return
		}
		if err != nil {
			log.Printf("shelf sync failed, keeping the stored shelf: %s; %s", run.Describe(), s.storeSummary())
			continue
		}
		log.Printf("%s; %s", run.Describe(), s.storeSummary())
		onSynced()
	}
}
