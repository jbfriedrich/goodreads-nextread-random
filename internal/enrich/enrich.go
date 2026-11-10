package enrich

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"time"

	"goodreads-nextread/internal/goodreads"
	"goodreads-nextread/internal/store"
)

// The shelf widget only carries title, author and cover. Everything else the
// page shows (description, rating, pages, year) is looked up once per book in
// the background: in batches through Goodreads' GraphQL API, falling back to
// the public book page when GraphQL is unavailable.

const (
	enrichIdlePoll        = time.Minute
	enrichErrorPause      = 5 * time.Minute
	openLibraryCoversBase = "https://covers.openlibrary.org"
)

// Enricher fills in book details for stored books that lack them.
type Enricher struct {
	gr    *goodreads.Client // paced at the enrichment delay; shared with gql
	gql   *goodreads.GraphQL
	ol    *openLibrary
	store *store.Store
	now   func() time.Time
}

// New builds an Enricher. gr is a Client dedicated to enrichment (its delay
// paces the enrichment requests) and gql the GraphQL client built on it.
func New(gr *goodreads.Client, gql *goodreads.GraphQL, st *store.Store) *Enricher {
	return &Enricher{
		gr:    gr,
		gql:   gql,
		ol:    &openLibrary{http: &http.Client{Timeout: 30 * time.Second}, base: openLibraryCoversBase},
		store: st,
		now:   time.Now,
	}
}

// run enriches batches until ctx is cancelled, calling onEnriched after each
// batch and logging progress. Pacing comes from the client's delay. After an
// error (transient failures whose retries ran out) it pauses enrichErrorPause;
// with nothing pending it polls the store every enrichIdlePoll.
func (e *Enricher) Run(ctx context.Context, onEnriched func()) {
	for {
		n, source, err := e.step(ctx)
		if ctx.Err() != nil {
			return
		}
		wait := enrichIdlePoll
		switch {
		case err != nil:
			log.Printf("enrichment paused for %s: %v", enrichErrorPause, err)
			wait = enrichErrorPause
		case n > 0:
			onEnriched()
			pending, _ := e.store.PendingEnrichmentCount()
			log.Printf("enriched %d books via %s (%d pending)", n, source, pending)
			if pending == 0 {
				log.Print("enrichment complete")
			}
			continue
		}
		if sleepCtx(ctx, wait) != nil {
			return
		}
	}
}

// step enriches up to one batch of pending books and returns how many it
// handled and through which source. Permanent failures (no such book, no
// search match) are saved as done so they are never retried. A transient
// failure returns an error and leaves the remaining books pending.
func (e *Enricher) step(ctx context.Context) (int, string, error) {
	pending, err := e.store.PendingEnrichment(goodreads.BatchSize)
	if err != nil || len(pending) == 0 {
		return 0, "", err
	}
	now := e.now()
	handled := 0
	done := func(b goodreads.Book, details goodreads.Book) error {
		if err := e.store.SaveEnrichment(b.ReviewID, details, now); err != nil {
			return err
		}
		handled++
		return nil
	}

	// Entries without a cover carry no book id: find it by title and author.
	autoCovers := map[string]string{}
	resolved := map[string]bool{} // book id already came from autocomplete
	var batch []goodreads.Book
	for _, b := range pending {
		if b.BookID != "" {
			batch = append(batch, b)
			continue
		}
		id, cover, err := e.gr.LookupBookID(ctx, b.Title, b.Author)
		if err != nil {
			if ctx.Err() != nil || goodreads.Classify(err) == goodreads.ErrTransient {
				return handled, "", err
			}
			log.Printf("enrichment: %q by %s: %v; leaving it without details", b.Title, b.Author, err)
			if err := done(b, goodreads.Book{}); err != nil {
				return handled, "", err
			}
			continue
		}
		b.BookID = id
		autoCovers[b.ReviewID] = cover
		resolved[b.ReviewID] = true
		batch = append(batch, b)
	}
	if len(batch) == 0 {
		return handled, "autocomplete", nil
	}

	ids := make([]string, len(batch))
	for i, b := range batch {
		ids[i] = b.BookID
	}
	details, source, err := e.details(ctx, ids)
	if err != nil {
		return handled, "", err
	}

	for _, b := range batch {
		d := details[b.BookID]
		if d == nil && !resolved[b.ReviewID] && b.Title != "" {
			// The id came from the widget's cover file name, which can keep an
			// old id after Goodreads merges editions. Look the book up by title
			// and author once before giving up on it.
			id, cover, err := e.gr.LookupBookID(ctx, b.Title, b.Author)
			switch {
			case err != nil && (ctx.Err() != nil || goodreads.Classify(err) == goodreads.ErrTransient):
				return handled, "", err
			case err == nil && id != b.BookID:
				found, _, err := e.details(ctx, []string{id})
				if err != nil {
					return handled, "", err
				}
				if found[id] != nil {
					log.Printf("enrichment: %q: book id %s is stale, using %s", b.Title, b.BookID, id)
					d, b.BookID, autoCovers[b.ReviewID] = found[id], id, cover
				}
			}
		}
		if d == nil {
			log.Printf("enrichment: %q by %s is not on Goodreads (book id %s); leaving it without details, linking to a search", b.Title, b.Author, b.BookID)
			if err := e.store.MarkMissing(b.ReviewID, now); err != nil {
				return handled, "", err
			}
			handled++
			continue
		}
		out := *d
		out.BookID = b.BookID
		// Cover preference: the widget's (already stored), then GraphQL /
		// book page, then autocomplete, then Open Library by ISBN.
		switch {
		case b.ImageURL != "":
			out.ImageURL = ""
		case out.ImageURL == "" && autoCovers[b.ReviewID] != "":
			out.ImageURL = autoCovers[b.ReviewID]
		case out.ImageURL == "" && out.ISBN != "":
			cover, err := e.ol.cover(ctx, out.ISBN)
			if err != nil {
				log.Printf("enrichment: Open Library cover lookup for %s failed: %v", out.ISBN, err)
			}
			out.ImageURL = cover
		}
		if err := done(b, out); err != nil {
			return handled, "", err
		}
	}
	return handled, source, nil
}

// details looks ids up through GraphQL, using book pages for whatever GraphQL
// cannot answer (or for all of them when GraphQL is unavailable). In the
// result a nil entry means the book does not exist. A transient failure is
// returned as an error.
func (e *Enricher) details(ctx context.Context, ids []string) (map[string]*goodreads.Book, string, error) {
	source := "graphql"
	details, err := e.gql.Lookup(ctx, ids)
	if err != nil {
		if !errors.Is(err, goodreads.ErrGraphQLUnavailable) {
			return nil, "", err // transient: wait, then retry this batch
		}
		log.Printf("graphql unavailable, using book pages: %v", err)
		source, details = "book pages", map[string]*goodreads.Book{}
	}
	for _, id := range ids {
		if _, ok := details[id]; ok {
			continue
		}
		if source == "graphql" {
			source = "graphql and book pages"
		}
		d, err := e.gr.BookDetails(ctx, id)
		switch {
		case err == nil:
			details[id] = &d
		case goodreads.IsNotFound(err):
			details[id] = nil
		default:
			return nil, "", err
		}
	}
	return details, source, nil
}

// openLibrary looks up covers on covers.openlibrary.org (no key needed).
type openLibrary struct {
	http *http.Client
	base string
}

// cover returns the large cover URL for isbn, or "" if Open Library has none.
// default=false makes Open Library answer 404 instead of a blank image.
func (o *openLibrary) cover(ctx context.Context, isbn string) (string, error) {
	coverURL := o.base + "/b/isbn/" + url.PathEscape(isbn) + "-L.jpg"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, coverURL+"?default=false", nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", goodreads.UserAgent)
	resp, err := o.http.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, io.LimitReader(resp.Body, maxCoverBytes))

	switch {
	case resp.StatusCode == http.StatusNotFound:
		return "", nil
	case resp.StatusCode >= 200 && resp.StatusCode < 300:
		return coverURL, nil
	default:
		return "", fmt.Errorf("unexpected status %s", resp.Status)
	}
}

// maxCoverBytes bounds how much of an Open Library response is read.
const maxCoverBytes = 8 << 20

// sleepCtx waits for d or until ctx ends.
func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
