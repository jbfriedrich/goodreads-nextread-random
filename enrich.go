package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
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

// enricher fills in book details for stored books that lack them.
type enricher struct {
	gr    *grClient // paced at the enrichment delay; shared with gql
	gql   *gqlClient
	ol    *openLibrary
	store *store
	now   func() time.Time
}

func newEnricher(cfg *Config, st *store) *enricher {
	gr := newGRClient(cfg, cfg.EnrichDelay)
	return &enricher{
		gr:    gr,
		gql:   &gqlClient{http: &http.Client{Timeout: 30 * time.Second}, endpoint: cfg.GraphQLEndpoint, gr: gr, store: st},
		ol:    &openLibrary{http: &http.Client{Timeout: 30 * time.Second}, base: openLibraryCoversBase},
		store: st,
		now:   time.Now,
	}
}

// run enriches batches until ctx is cancelled, calling onEnriched after each
// batch and logging progress. Pacing comes from the client's delay. After an
// error (transient failures whose retries ran out) it pauses enrichErrorPause;
// with nothing pending it polls the store every enrichIdlePoll.
func (e *enricher) run(ctx context.Context, onEnriched func()) {
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
func (e *enricher) step(ctx context.Context) (int, string, error) {
	pending, err := e.store.PendingEnrichment(gqlBatchSize)
	if err != nil || len(pending) == 0 {
		return 0, "", err
	}
	now := e.now()
	handled := 0
	done := func(b Book, details Book) error {
		if err := e.store.SaveEnrichment(b.ReviewID, details, now); err != nil {
			return err
		}
		handled++
		return nil
	}

	// Entries without a cover carry no book id: find it by title and author.
	autoCovers := map[string]string{}
	resolved := map[string]bool{} // book id already came from autocomplete
	var batch []Book
	for _, b := range pending {
		if b.BookID != "" {
			batch = append(batch, b)
			continue
		}
		id, cover, err := e.gr.lookupBookID(ctx, b.Title, b.Author)
		if err != nil {
			if ctx.Err() != nil || classify(err) == errTransient {
				return handled, "", err
			}
			log.Printf("enrichment: %q by %s: %v; leaving it without details", b.Title, b.Author, err)
			if err := done(b, Book{}); err != nil {
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
			id, cover, err := e.gr.lookupBookID(ctx, b.Title, b.Author)
			switch {
			case err != nil && (ctx.Err() != nil || classify(err) == errTransient):
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
func (e *enricher) details(ctx context.Context, ids []string) (map[string]*Book, string, error) {
	source := "graphql"
	details, err := e.gql.lookup(ctx, ids)
	if err != nil {
		if !errors.Is(err, errGraphQLUnavailable) {
			return nil, "", err // transient: wait, then retry this batch
		}
		log.Printf("graphql unavailable, using book pages: %v", err)
		source, details = "book pages", map[string]*Book{}
	}
	for _, id := range ids {
		if _, ok := details[id]; ok {
			continue
		}
		if source == "graphql" {
			source = "graphql and book pages"
		}
		d, err := e.gr.bookDetails(ctx, id)
		switch {
		case err == nil:
			details[id] = &d
		case isNotFound(err):
			details[id] = nil
		default:
			return nil, "", err
		}
	}
	return details, source, nil
}

// errBookNotFound means Goodreads says the book does not exist. Removed books
// get a "soft 404": HTTP 200, "Page not found", and page data whose lookup for
// the id is null.
var errBookNotFound = errors.New("book not found on Goodreads")

// isNotFound reports a 404/410 response or a soft-404 book page.
func isNotFound(err error) bool {
	if errors.Is(err, errBookNotFound) {
		return true
	}
	var se *httpStatusError
	return errors.As(err, &se) && (se.Status == http.StatusNotFound || se.Status == http.StatusGone)
}

// lookupBookID finds a book id (and cover) for a widget entry that has none,
// via Goodreads' public autocomplete search.
func (c *grClient) lookupBookID(ctx context.Context, title, author string) (bookID, cover string, err error) {
	body, _, err := c.get(ctx, "/book/auto_complete", url.Values{
		"format": {"json"},
		"q":      {strings.TrimSpace(title + " " + author)},
	})
	if err != nil {
		return "", "", fmt.Errorf("searching for book id: %w", err)
	}
	return parseAutoComplete(body, title, author)
}

// bookDetails fetches and parses the public book page.
func (c *grClient) bookDetails(ctx context.Context, bookID string) (Book, error) {
	body, _, err := c.get(ctx, "/book/show/"+url.PathEscape(bookID), nil)
	if err != nil {
		return Book{}, fmt.Errorf("fetching book page: %w", err)
	}
	return parseBookPage(body, bookID)
}

type autoCompleteResult struct {
	BookID        string `json:"bookId"`
	Title         string `json:"title"`
	BookTitleBare string `json:"bookTitleBare"`
	ImageURL      string `json:"imageUrl"`
	Author        struct {
		Name string `json:"name"`
	} `json:"author"`
}

// parseAutoComplete picks the result whose title matches (preferring one by
// the same author). Results with a different title are never taken: a wrong
// match would show the wrong book.
func parseAutoComplete(data []byte, title, author string) (bookID, cover string, err error) {
	var results []autoCompleteResult
	if err := json.Unmarshal(data, &results); err != nil {
		return "", "", fmt.Errorf("parsing autocomplete response: %w", err)
	}

	var best *autoCompleteResult
	for i := range results {
		r := &results[i]
		if r.BookID == "" || !(sameText(r.Title, title) || sameText(r.BookTitleBare, title)) {
			continue
		}
		if sameText(r.Author.Name, author) {
			best = r
			break
		}
		if best == nil {
			best = r
		}
	}
	if best == nil {
		return "", "", fmt.Errorf("no Goodreads search result matches %q", title)
	}
	return best.BookID, normalizeCoverURL(best.ImageURL), nil
}

var textNormalizer = strings.NewReplacer("’", "'", "‘", "'", "“", `"`, "”", `"`)

// sameText compares titles/names ignoring case, whitespace runs and curly quotes.
func sameText(a, b string) bool {
	norm := func(s string) string {
		return strings.Join(strings.Fields(textNormalizer.Replace(s)), " ")
	}
	return strings.EqualFold(norm(a), norm(b))
}

// flexString accepts a JSON string or number.
type flexString string

func (f *flexString) UnmarshalJSON(b []byte) error {
	if s, err := strconv.Unquote(string(b)); err == nil {
		*f = flexString(s)
		return nil
	}
	*f = flexString(bytes.TrimSpace(b))
	return nil
}

type apolloRef struct {
	Ref string `json:"__ref"`
}

type apolloBook struct {
	LegacyID    flexString `json:"legacyId"`
	Description string     `json:"description"`
	ImageURL    string     `json:"imageUrl"`
	Details     struct {
		NumPages        int     `json:"numPages"`
		PublicationTime float64 `json:"publicationTime"`
		ISBN13          string  `json:"isbn13"`
		ISBN            string  `json:"isbn"`
	} `json:"details"`
	Work *apolloRef `json:"work"`
}

type apolloWork struct {
	Details struct {
		PublicationTime float64 `json:"publicationTime"`
	} `json:"details"`
	Stats struct {
		AverageRating float64 `json:"averageRating"`
	} `json:"stats"`
}

// bookFields are the raw details both sources (GraphQL and the book page's
// embedded data) provide, before they are turned into a Book.
type bookFields struct {
	Description, ImageURL, ISBN13, ISBN string
	NumPages                            int
	EditionTime, WorkTime               float64 // ms since epoch
	AverageRating                       float64
}

// book maps fields to a Book: the work's original publication year beats the
// edition's, ISBN-13 beats ISBN-10, placeholder covers and zero values are
// dropped. The description stays HTML; cleanDescription renders it.
func (f bookFields) book(bookID string) Book {
	out := Book{
		BookID:      bookID,
		Description: strings.TrimSpace(f.Description),
		ISBN:        strings.TrimSpace(f.ISBN13),
	}
	if out.ISBN == "" {
		out.ISBN = strings.TrimSpace(f.ISBN)
	}
	if f.NumPages > 0 {
		out.NumPages = strconv.Itoa(f.NumPages)
	}
	if !isPlaceholderCover(f.ImageURL) {
		out.ImageURL = strings.TrimSpace(f.ImageURL)
	}
	if f.AverageRating > 0 {
		out.AverageRating = strconv.FormatFloat(f.AverageRating, 'f', 2, 64)
	}
	published := f.EditionTime
	if f.WorkTime != 0 {
		published = f.WorkTime
	}
	if published != 0 {
		out.Published = strconv.Itoa(time.UnixMilli(int64(published)).UTC().Year())
	}
	return out
}

var (
	nextDataStart = []byte(`<script id="__NEXT_DATA__" type="application/json">`)
	scriptEnd     = []byte(`</script>`)
)

// nextData returns the JSON in a page's __NEXT_DATA__ script.
func nextData(page []byte) ([]byte, error) {
	i := bytes.Index(page, nextDataStart)
	if i < 0 {
		return nil, errors.New("book page has no __NEXT_DATA__; has the page changed?")
	}
	rest := page[i+len(nextDataStart):]
	j := bytes.Index(rest, scriptEnd)
	if j < 0 {
		return nil, errors.New("book page __NEXT_DATA__ is not terminated")
	}
	return rest[:j], nil
}

// parseBookPage extracts book details from the Apollo state that the book page
// embeds in its __NEXT_DATA__ script.
func parseBookPage(page []byte, bookID string) (Book, error) {
	data, err := nextData(page)
	if err != nil {
		return Book{}, err
	}
	var next struct {
		Props struct {
			PageProps struct {
				ApolloState map[string]json.RawMessage `json:"apolloState"`
			} `json:"pageProps"`
		} `json:"props"`
	}
	if err := json.Unmarshal(data, &next); err != nil {
		return Book{}, fmt.Errorf("parsing book page data: %w", err)
	}
	state := next.Props.PageProps.ApolloState

	book, err := findApolloBook(state, bookID)
	if err != nil {
		return Book{}, err
	}
	f := bookFields{
		Description: book.Description,
		ImageURL:    book.ImageURL,
		ISBN13:      book.Details.ISBN13,
		ISBN:        book.Details.ISBN,
		NumPages:    book.Details.NumPages,
		EditionTime: book.Details.PublicationTime,
	}
	if book.Work != nil {
		var work apolloWork
		if raw, ok := state[book.Work.Ref]; ok && json.Unmarshal(raw, &work) == nil {
			f.AverageRating = work.Stats.AverageRating
			f.WorkTime = work.Details.PublicationTime
		}
	}
	return f.book(bookID), nil
}

// findApolloBook locates the page's main book: via the ROOT_QUERY lookup by
// legacy id, falling back to scanning every Book entry.
func findApolloBook(state map[string]json.RawMessage, bookID string) (apolloBook, error) {
	var root map[string]json.RawMessage
	lookedUp := false // the page looked the id up (as opposed to markup we do not understand)
	if raw, ok := state["ROOT_QUERY"]; ok && json.Unmarshal(raw, &root) == nil {
		for key, raw := range root {
			if !strings.HasPrefix(key, "getBookByLegacyId(") || !strings.Contains(key, `"`+bookID+`"`) {
				continue
			}
			lookedUp = true
			if string(bytes.TrimSpace(raw)) == "null" {
				return apolloBook{}, fmt.Errorf("book %s: %w", bookID, errBookNotFound)
			}
			var ref apolloRef
			if json.Unmarshal(raw, &ref) != nil {
				continue
			}
			var book apolloBook
			if bookRaw, ok := state[ref.Ref]; ok && json.Unmarshal(bookRaw, &book) == nil {
				return book, nil
			}
		}
	}
	for key, raw := range state {
		if !strings.HasPrefix(key, "Book:") {
			continue
		}
		var book apolloBook
		if json.Unmarshal(raw, &book) == nil && string(book.LegacyID) == bookID {
			return book, nil
		}
	}
	if lookedUp {
		return apolloBook{}, fmt.Errorf("book %s: %w", bookID, errBookNotFound)
	}
	return apolloBook{}, fmt.Errorf("book %s not found in the book page data; has the page changed?", bookID)
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
	req.Header.Set("User-Agent", userAgent)
	resp, err := o.http.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, io.LimitReader(resp.Body, maxBodyBytes))

	switch {
	case resp.StatusCode == http.StatusNotFound:
		return "", nil
	case resp.StatusCode >= 200 && resp.StatusCode < 300:
		return coverURL, nil
	default:
		return "", fmt.Errorf("unexpected status %s", resp.Status)
	}
}
