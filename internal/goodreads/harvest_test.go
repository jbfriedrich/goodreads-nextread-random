package goodreads

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"goodreads-nextread/internal/goodreads/goodreadstest"
)

// newClient returns a Client for the fake's shelf that skips retry waits.
func newClient(f *goodreadstest.Fake) *Client {
	return NewClient(Options{HTTP: f.Server.Client(), BaseURL: f.Server.URL, UserID: goodreadstest.UID,
		Shelf: "to-read", Sleep: goodreadstest.NoSleep})
}

func TestShelfCountFollowsProfileRedirect(t *testing.T) {
	f := goodreadstest.New(t, 42)
	n, err := newClient(f).ShelfCount(context.Background())
	if err != nil {
		t.Fatalf("shelfCount: %v", err)
	}
	if n != 42 {
		t.Errorf("shelfCount = %d, want 42", n)
	}
}

func TestHarvestCollectsWholeShelfBeyondWidgetLimit(t *testing.T) {
	f := goodreadstest.New(t, 250) // well over the widget's 100-book cap
	res, err := newClient(f).Harvest(context.Background(), 80, nil)
	if err != nil {
		t.Fatalf("harvest: %v", err)
	}
	if !res.Complete {
		t.Fatalf("harvest incomplete: found %d of %d", len(res.Books), res.Expected)
	}
	if len(res.Books) != 250 || res.Expected != 250 {
		t.Errorf("found %d, expected %d; want 250 and 250", len(res.Books), res.Expected)
	}

	requests, _ := f.Stats()
	if res.Requests != requests {
		t.Errorf("harvest reported %d requests, server saw %d", res.Requests, requests)
	}
	if res.Requests > 80 {
		t.Errorf("harvest used %d requests, cap is 80", res.Requests)
	}
	for id, n := range f.WidgetIDs {
		if n > 1 {
			t.Errorf("widget_id %q reused %d times (Goodreads caches per widget_id)", id, n)
		}
	}
}

func TestHarvestStopsEarlyOnSmallShelf(t *testing.T) {
	f := goodreadstest.New(t, 30)
	res, err := newClient(f).Harvest(context.Background(), 80, nil)
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
	f := goodreadstest.New(t, 250)
	res, err := newClient(f).Harvest(context.Background(), 4, nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.Complete {
		t.Error("harvest with a 4-request cap reported complete for a 250-book shelf")
	}
	if requests, _ := f.Stats(); requests > 4 {
		t.Errorf("server saw %d requests, cap is 4", requests)
	}
	if len(res.Books) == 0 {
		t.Error("partial harvest returned no books")
	}
}

func TestHarvestEmptyShelf(t *testing.T) {
	f := goodreadstest.New(t, 0)
	res, err := newClient(f).Harvest(context.Background(), 80, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !res.Complete || len(res.Books) != 0 {
		t.Errorf("complete=%v found=%d, want a complete empty harvest", res.Complete, len(res.Books))
	}
}

func TestHarvestCountMismatchIsIncomplete(t *testing.T) {
	f := goodreadstest.New(t, 20)
	f.Set(func(f *goodreadstest.Fake) { f.CountDelta = 1 }) // profile claims one more than exists
	res, err := newClient(f).Harvest(context.Background(), 10, nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.Complete {
		t.Error("harvest that could not reach the profile count reported complete")
	}
}

func TestHarvestFailsWhenWidgetReturnsNothingForNonEmptyShelf(t *testing.T) {
	f := goodreadstest.New(t, 20)
	f.Set(func(f *goodreadstest.Fake) { f.EmptyWidget = true })
	if _, err := newClient(f).Harvest(context.Background(), 80, nil); err == nil {
		t.Fatal("expected an error when the widget returns no books but the profile reports 20")
	}
}

func TestHarvestRetriesThrottledRequestInPlace(t *testing.T) {
	f := goodreadstest.New(t, 250)
	f.Set(func(f *goodreadstest.Fake) { f.Throttle = 2 }) // second widget call gets 429 twice
	var waits []time.Duration
	c := newClient(f)
	c.sleep = func(_ context.Context, d time.Duration) error { waits = append(waits, d); return nil }

	res, err := c.Harvest(context.Background(), 80, nil)
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
	f := goodreadstest.New(t, 20)
	f.Set(func(f *goodreadstest.Fake) { f.FailWidget = true })
	if _, err := newClient(f).Harvest(context.Background(), 80, nil); err == nil {
		t.Fatal("expected an error when the widget returns HTTP 500")
	}
}

func TestHarvestReportsEachBatchOfNewBooks(t *testing.T) {
	f := goodreadstest.New(t, 250)
	var batches []int
	seen := map[string]bool{}
	res, err := newClient(f).Harvest(context.Background(), 80, func(batch []Book) error {
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
	f := goodreadstest.New(t, 250)
	_, err := newClient(f).Harvest(context.Background(), 80, func([]Book) error {
		return errors.New("disk full")
	})
	if err == nil || !strings.Contains(err.Error(), "disk full") {
		t.Fatalf("err = %v, want the callback's error", err)
	}
	if _, calls := f.Stats(); calls != 1 {
		t.Errorf("widget called %d times after the callback failed, want 1", calls)
	}
}
