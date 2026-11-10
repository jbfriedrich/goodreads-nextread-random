package main

import (
	"context"
	"fmt"
	"log"
	"time"
)

// defaultFullEvery is how often a full harvest runs even when quick checks
// see no change — a safety net for edits the quick check cannot see. The
// quick check's count arithmetic catches every case known so far, so weekly
// is enough.
const defaultFullEvery = 7 * 24 * time.Hour

// failedRetryAfter caps the wait before retrying after a failed sync.
const failedRetryAfter = time.Hour

// syncer keeps the stored shelf in line with Goodreads.
type syncer struct {
	gr          *grClient
	store       *store
	maxRequests int
	fullEvery   time.Duration
	now         func() time.Time

	// onProgress, if set, is called each time a batch of an initial harvest
	// (into an empty store) has been saved, so the server can start serving
	// before the whole shelf is in.
	onProgress func()
}

func newSyncer(cfg *Config, st *store) *syncer {
	return &syncer{
		gr:          newGRClient(cfg, cfg.RequestDelay),
		store:       st,
		maxRequests: cfg.MaxRequests,
		fullEvery:   defaultFullEvery,
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
func (s *syncer) sync(ctx context.Context, forceFull bool) (syncRun, error) {
	run := syncRun{StartedAt: s.now()}
	startRequests := s.gr.requestCount()
	finish := func(err error) (syncRun, error) {
		run.FinishedAt = s.now()
		run.Requests = s.gr.requestCount() - startRequests
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
		expected, newest, err := s.gr.quickCheck(ctx)
		if err != nil {
			return finish(err)
		}
		var unknown []Book
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
	var onBatch func([]Book) error
	progressive := stored == 0
	if progressive {
		onBatch = func(batch []Book) error {
			if _, _, err := s.store.Reconcile(batch, false, s.now()); err != nil {
				return fmt.Errorf("saving shelf: %w", err)
			}
			if s.onProgress != nil {
				s.onProgress()
			}
			return nil
		}
	}
	res, err := s.gr.harvest(ctx, s.maxRequests, onBatch)
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

// describe renders a run as a one-line summary for logs and the CLI.
func (r syncRun) describe() string {
	s := fmt.Sprintf("%s sync %s: expected=%d found=%d added=%d removed=%d requests=%d (%s)",
		r.Kind, r.Status, r.Expected, r.Found, r.Added, r.Removed, r.Requests,
		r.FinishedAt.Sub(r.StartedAt).Round(time.Second))
	if r.Err != "" {
		s += ": " + r.Err
	}
	return s
}

// storeSummary describes the stored shelf for log lines.
func (s *syncer) storeSummary() string {
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
func (s *syncer) untilNext(interval time.Duration) time.Duration {
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
func (s *syncer) loop(ctx context.Context, interval time.Duration, onSynced func()) {
	for {
		wait := s.untilNext(interval)
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

		run, err := s.sync(ctx, false)
		if ctx.Err() != nil {
			return
		}
		if err != nil {
			log.Printf("shelf sync failed, keeping the stored shelf: %s; %s", run.describe(), s.storeSummary())
			continue
		}
		log.Printf("%s; %s", run.describe(), s.storeSummary())
		onSynced()
	}
}
