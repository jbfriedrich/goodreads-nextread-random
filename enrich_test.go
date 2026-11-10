package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestParseBookPage(t *testing.T) {
	got, err := parseBookPage(readFixture(t, "book_page.html"), "111111")
	if err != nil {
		t.Fatalf("parseBookPage: %v", err)
	}
	want := Book{
		BookID:        "111111",
		Description:   "<b>Gears</b> &amp; apples, ​ripening.",
		AverageRating: "4.50",
		NumPages:      "412",
		Published:     "2015", // the work's original publication, not this edition's
		ISBN:          "9781538784747",
		ImageURL:      "https://m.media-amazon.com/images/S/compressed.photo.goodreads.com/books/1781766398i/111111.jpg",
	}
	if got != want {
		t.Errorf("parseBookPage =\n %+v\nwant\n %+v", got, want)
	}
}

func TestParseBookPageWithoutEmbeddedData(t *testing.T) {
	if _, err := parseBookPage([]byte("<html><body>Book not found</body></html>"), "1"); err == nil {
		t.Fatal("expected an error for a page without __NEXT_DATA__")
	}
}

func TestParseBookPageFallsBackToEditionDataAndISBN10(t *testing.T) {
	page := bookPageHTML("5", bookPageData{Pages: 0, EditionTime: 1791874800000, ISBN10: "1538784742", Rating: 0}, fakeAPIKey)
	got, err := parseBookPage([]byte(page), "5")
	if err != nil {
		t.Fatal(err)
	}
	if got.Published != "2026" || got.ISBN != "1538784742" {
		t.Errorf("got %+v, want edition year 2026 and the ISBN-10", got)
	}
	if got.NumPages != "" || got.AverageRating != "" {
		t.Errorf("zero pages/rating should be empty, got %+v", got)
	}
}

func TestAPIKeyFromBookPage(t *testing.T) {
	key, err := apiKeyFromBookPage(readFixture(t, "book_page.html"))
	if err != nil || key != "da2-fixturekey" {
		t.Errorf("apiKeyFromBookPage = %q, %v; want da2-fixturekey", key, err)
	}
	if _, err := apiKeyFromBookPage([]byte(bookPageHTML("1", bookPageData{}, ""))); err == nil {
		t.Error("expected an error for a page without an apiKey")
	}
}

func TestBookQueryStaysUnderWAFBodyLimit(t *testing.T) {
	ids := make([]string, gqlBatchSize)
	for i := range ids {
		ids[i] = "999999999999" // longest realistic legacy ids
	}
	body, err := bookQueryBody(ids)
	if err != nil {
		t.Fatal(err)
	}
	if len(body) > gqlMaxBody {
		t.Errorf("a %d-book query body is %d bytes, over the %d-byte limit", gqlBatchSize, len(body), gqlMaxBody)
	}
	if _, err := bookQueryBody([]string{"12a"}); err == nil {
		t.Error("expected an error for a non-numeric book id")
	}
}

func TestParseAutoCompletePicksMatchingTitleAndAuthor(t *testing.T) {
	id, cover, err := parseAutoComplete(readFixture(t, "auto_complete.json"), "The Cartographer's Daughter", "Cy Meridian")
	if err != nil {
		t.Fatal(err)
	}
	if id != "333333" {
		t.Errorf("bookId = %q, want 333333 (not the first, unrelated result)", id)
	}
	if cover != "https://i.gr-assets.com/images/S/compressed.photo.goodreads.com/books/1762486430i/333333.jpg" {
		t.Errorf("cover = %q", cover)
	}
}

func TestParseAutoCompleteNoMatch(t *testing.T) {
	if _, _, err := parseAutoComplete(readFixture(t, "auto_complete.json"), "Unrelated", "Nobody"); err == nil {
		t.Fatal("expected an error when no result matches title or author")
	}
	if _, _, err := parseAutoComplete([]byte("[]"), "X", "Y"); err == nil {
		t.Fatal("expected an error for an empty result list")
	}
}

func TestOpenLibraryCover(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("default") != "false" {
			t.Errorf("cover lookup without default=false: %s", r.URL)
		}
		if r.URL.Path == "/b/isbn/9781538784747-L.jpg" {
			w.Header().Set("Content-Type", "image/jpeg")
			w.Write([]byte("jpeg"))
			return
		}
		http.NotFound(w, r)
	}))
	defer srv.Close()
	ol := &openLibrary{http: srv.Client(), base: srv.URL}

	got, err := ol.cover(context.Background(), "9781538784747")
	if err != nil || got != srv.URL+"/b/isbn/9781538784747-L.jpg" {
		t.Errorf("cover(found) = %q, %v", got, err)
	}
	if got, err := ol.cover(context.Background(), "0000000000"); err != nil || got != "" {
		t.Errorf("cover(missing) = %q, %v; want empty and no error", got, err)
	}
}

// --- enricher, against the fake Goodreads ---

type bookPageData struct {
	Desc, Image, ISBN13, ISBN10 string
	Pages                       int
	Rating                      float64
	EditionTime, WorkTime       int64
}

// bookPageHTML renders a minimal book page with the Apollo state shape the
// real /book/show page embeds in __NEXT_DATA__, plus the page's AppSync key
// (omitted when apiKey is "").
func bookPageHTML(bookID string, d bookPageData, apiKey string) string {
	book := map[string]any{
		"__typename":  "Book",
		"legacyId":    bookID,
		"description": d.Desc,
		"imageUrl":    d.Image,
		"details": map[string]any{
			"numPages": d.Pages, "publicationTime": d.EditionTime, "isbn13": d.ISBN13, "isbn": d.ISBN10,
		},
	}
	state := map[string]any{
		"ROOT_QUERY": map[string]any{
			`getBookByLegacyId({"legacyId":"` + bookID + `"})`: map[string]any{"__ref": "Book:b" + bookID},
		},
		"Book:b" + bookID: book,
	}
	if d.WorkTime != 0 || d.Rating != 0 {
		book["work"] = map[string]any{"__ref": "Work:w" + bookID}
		state["Work:w"+bookID] = map[string]any{
			"__typename": "Work",
			"details":    map[string]any{"publicationTime": d.WorkTime},
			"stats":      map[string]any{"averageRating": d.Rating},
		}
	}
	pageProps := map[string]any{"apolloState": state}
	if apiKey != "" {
		pageProps["apiKey"] = apiKey
	}
	data, err := json.Marshal(map[string]any{"props": map[string]any{"pageProps": pageProps}})
	if err != nil {
		panic(err)
	}
	return `<html><body><script id="__NEXT_DATA__" type="application/json">` + string(data) + `</script></body></html>`
}

type enrichFixture struct {
	f     *fakeGoodreads
	store *store
	e     *enricher
	ol    *httptest.Server
	waits []time.Duration

	mu       sync.Mutex
	olHits   []string
	olCovers map[string]bool
}

func newEnrichFixture(t *testing.T) *enrichFixture {
	t.Helper()
	fx := &enrichFixture{f: newFakeGoodreads(t, 0), store: newTestStore(t), olCovers: map[string]bool{}}
	fx.ol = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fx.mu.Lock()
		defer fx.mu.Unlock()
		fx.olHits = append(fx.olHits, r.URL.Path)
		isbn := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/b/isbn/"), "-L.jpg")
		if !fx.olCovers[isbn] {
			http.NotFound(w, r)
			return
		}
		w.Write([]byte("jpeg"))
	}))
	t.Cleanup(fx.ol.Close)

	gr := fx.f.client()
	gr.sleep = func(ctx context.Context, d time.Duration) error {
		fx.waits = append(fx.waits, d)
		return ctx.Err()
	}
	fx.e = &enricher{
		gr:    gr,
		gql:   &gqlClient{http: fx.f.srv.Client(), endpoint: fx.f.srv.URL + "/graphql", gr: gr, store: fx.store},
		ol:    &openLibrary{http: fx.ol.Client(), base: fx.ol.URL},
		store: fx.store,
		now:   func() time.Time { return t0 },
	}
	return fx
}

// addBooks stores n books with book ids "1".."n" and gives each GraphQL /
// book-page data.
func (fx *enrichFixture) addBooks(n int) {
	var books []Book
	fx.f.set(func(f *fakeGoodreads) {
		for i := 1; i <= n; i++ {
			id := strconv.Itoa(i)
			books = append(books, Book{ReviewID: "r" + id, BookID: id, Title: "T" + id, ImageURL: "https://gr/w" + id + ".jpg"})
			f.books[id] = bookPageData{Desc: "Blurb " + id, Pages: 100 + i, Rating: 4.1, ISBN13: "97800000000" + id, WorkTime: 1420099200000}
		}
	})
	fx.store.Reconcile(books, true, t0)
}

func (fx *enrichFixture) stored(t *testing.T, reviewID string) Book {
	t.Helper()
	books, err := fx.store.AllBooks()
	if err != nil {
		t.Fatal(err)
	}
	for _, b := range books {
		if b.ReviewID == reviewID {
			return b
		}
	}
	t.Fatalf("review %s not stored", reviewID)
	return Book{}
}

func (fx *enrichFixture) step(t *testing.T) (int, string) {
	t.Helper()
	n, source, err := fx.e.step(context.Background())
	if err != nil {
		t.Fatalf("step: %v", err)
	}
	return n, source
}

func pendingCount(t *testing.T, s *store) int {
	t.Helper()
	n, err := s.PendingEnrichmentCount()
	if err != nil {
		t.Fatal(err)
	}
	return n
}

func TestEnrichBatchesThroughGraphQL(t *testing.T) {
	fx := newEnrichFixture(t)
	fx.addBooks(30)

	n, source := fx.step(t)
	if n != 25 || source != "graphql" {
		t.Fatalf("first step enriched %d via %q, want 25 via graphql", n, source)
	}
	if fx.f.gqlCalls != 1 {
		t.Errorf("GraphQL calls = %d, want 1 for 25 books", fx.f.gqlCalls)
	}
	// The key is bootstrapped once from a book page, then cached.
	if fx.f.bookPageHits != 1 {
		t.Errorf("book pages fetched = %d, want 1 (key bootstrap)", fx.f.bookPageHits)
	}

	// Pending books come oldest first, then by review id as a string, so
	// "r2" is in the first batch (r1, r10..r19, r2, r20..r29, r3, r30, r4).
	b := fx.stored(t, "r2")
	if b.Description != "Blurb 2" || b.NumPages != "102" || b.AverageRating != "4.10" ||
		b.Published != "2015" || b.ISBN != "978000000002" {
		t.Errorf("details not mapped: %+v", b)
	}
	if b.ImageURL != "https://gr/w2.jpg" {
		t.Errorf("ImageURL = %q, want the widget cover kept", b.ImageURL)
	}

	if n, _ := fx.step(t); n != 5 {
		t.Errorf("second step enriched %d, want the remaining 5", n)
	}
	if fx.f.gqlCalls != 2 || fx.f.bookPageHits != 1 {
		t.Errorf("after two steps: %d GraphQL calls, %d book pages; want 2 and 1", fx.f.gqlCalls, fx.f.bookPageHits)
	}
	if got := pendingCount(t, fx.store); got != 0 {
		t.Errorf("pending = %d, want 0", got)
	}
	for _, size := range fx.f.gqlBodySizes {
		if size > gqlMaxBody {
			t.Errorf("GraphQL request body %d bytes, over %d", size, gqlMaxBody)
		}
	}
}

func TestEnrichReusesStoredKey(t *testing.T) {
	fx := newEnrichFixture(t)
	fx.addBooks(1)
	fx.store.SetSetting(settingGraphQLKey, fakeAPIKey)

	fx.step(t)
	if fx.f.bookPageHits != 0 {
		t.Errorf("book pages fetched = %d, want 0 with a stored key", fx.f.bookPageHits)
	}
}

func TestEnrichRefreshesRotatedKey(t *testing.T) {
	fx := newEnrichFixture(t)
	fx.addBooks(3)
	fx.store.SetSetting(settingGraphQLKey, "da2-stale")

	if n, source := fx.step(t); n != 3 || source != "graphql" {
		t.Fatalf("step enriched %d via %q, want 3 via graphql after a key refresh", n, source)
	}
	if fx.f.bookPageHits != 1 || fx.f.gqlCalls != 2 {
		t.Errorf("book pages = %d, GraphQL calls = %d; want 1 refresh and 2 calls (401, then success)", fx.f.bookPageHits, fx.f.gqlCalls)
	}
	if key, _ := fx.store.Setting(settingGraphQLKey); key != fakeAPIKey {
		t.Errorf("stored key = %q, want the refreshed key", key)
	}
}

func TestEnrichNotFoundIsPermanent(t *testing.T) {
	fx := newEnrichFixture(t)
	fx.store.SetSetting(settingGraphQLKey, fakeAPIKey)
	fx.store.Reconcile([]Book{{ReviewID: "r1", BookID: "404404", Title: "Gone"}}, true, t0) // unknown to GraphQL

	if n, _ := fx.step(t); n != 1 {
		t.Fatalf("step handled %d books, want 1", n)
	}
	if got := pendingCount(t, fx.store); got != 0 {
		t.Errorf("pending = %d; a book that does not exist must not be retried", got)
	}
	if n, _ := fx.step(t); n != 0 || fx.f.gqlCalls != 1 {
		t.Errorf("second step handled %d books with %d GraphQL calls; want nothing more", n, fx.f.gqlCalls)
	}
}

func TestEnrichRetriesThrottledGraphQLInPlace(t *testing.T) {
	fx := newEnrichFixture(t)
	fx.addBooks(2)
	fx.store.SetSetting(settingGraphQLKey, fakeAPIKey)
	fx.f.set(func(f *fakeGoodreads) { f.gqlThrottle = 1 })

	if n, source := fx.step(t); n != 2 || source != "graphql" {
		t.Fatalf("step enriched %d via %q, want 2 via graphql after waiting", n, source)
	}
	if len(fx.waits) != 1 || fx.waits[0] != 2*time.Second {
		t.Errorf("waits = %v, want one wait honouring Retry-After: 2", fx.waits)
	}
}

func TestEnrichTransientFailureKeepsBooksPending(t *testing.T) {
	fx := newEnrichFixture(t)
	fx.addBooks(2)
	fx.store.SetSetting(settingGraphQLKey, fakeAPIKey)
	fx.f.set(func(f *fakeGoodreads) { f.gqlFail = 100 })

	if _, _, err := fx.e.step(context.Background()); err == nil {
		t.Fatal("expected an error once retries are used up")
	}
	if got := pendingCount(t, fx.store); got != 2 {
		t.Errorf("pending = %d, want both books still pending", got)
	}
	if fx.f.gqlCalls != 1+len(retryBackoff) {
		t.Errorf("GraphQL calls = %d, want %d (first try + retries)", fx.f.gqlCalls, 1+len(retryBackoff))
	}
	if fx.f.bookPageHits != 0 {
		t.Errorf("fell back to %d book pages on a transient error; it should wait instead", fx.f.bookPageHits)
	}
}

func TestEnrichFallsBackToBookPagesWhenGraphQLBreaks(t *testing.T) {
	fx := newEnrichFixture(t)
	fx.addBooks(3)
	fx.store.SetSetting(settingGraphQLKey, fakeAPIKey)
	fx.f.set(func(f *fakeGoodreads) { f.gqlBroken = true })

	n, source := fx.step(t)
	if n != 3 || source != "book pages" {
		t.Fatalf("step enriched %d via %q, want 3 via book pages", n, source)
	}
	if b := fx.stored(t, "r2"); b.Description != "Blurb 2" || b.NumPages != "102" || b.Published != "2015" {
		t.Errorf("fallback details not mapped: %+v", b)
	}
	if fx.f.bookPageHits != 3 {
		t.Errorf("book pages fetched = %d, want 3", fx.f.bookPageHits)
	}

	// GraphQL is tried again on the next batch.
	fx.f.set(func(f *fakeGoodreads) { f.gqlBroken = false })
	fx.store.Reconcile([]Book{{ReviewID: "r9", BookID: "9"}}, false, t0)
	fx.f.set(func(f *fakeGoodreads) { f.books["9"] = bookPageData{Desc: "nine"} })
	if _, source := fx.step(t); source != "graphql" {
		t.Errorf("next batch went via %q, want graphql again", source)
	}
}

func TestParseBookPageSoftNotFound(t *testing.T) {
	_, err := parseBookPage([]byte(softNotFoundPage("220513519")), "220513519")
	if !errors.Is(err, errBookNotFound) || !isNotFound(err) {
		t.Errorf("err = %v, want errBookNotFound (a removed book's soft-404 page)", err)
	}
	// A page without the embedded data at all is a markup change, not a
	// missing book.
	if _, err := parseBookPage([]byte("<html></html>"), "1"); errors.Is(err, errBookNotFound) || isNotFound(err) {
		t.Errorf("err = %v; a page without __NEXT_DATA__ must not count as not found", err)
	}
}

func TestEnrichGraphQLResourceNotFoundIsPermanent(t *testing.T) {
	fx := newEnrichFixture(t)
	fx.addBooks(3)
	fx.store.SetSetting(settingGraphQLKey, fakeAPIKey)
	fx.f.set(func(f *fakeGoodreads) { f.invalid["2"] = true })

	n, source := fx.step(t)
	if n != 3 || source != "graphql" {
		t.Fatalf("step handled %d via %q, want all 3 via graphql", n, source)
	}
	if got := pendingCount(t, fx.store); got != 0 {
		t.Errorf("pending = %d; an invalid book must be final, not retried", got)
	}
	if fx.f.bookPageHits != 0 {
		t.Errorf("fetched %d book pages; GraphQL already said the book does not exist", fx.f.bookPageHits)
	}
	if b := fx.stored(t, "r1"); b.Description != "Blurb 1" {
		t.Errorf("the valid books in the batch were not enriched: %+v", b)
	}
}

func TestEnrichBatchOfOnlyInvalidBooksIsPermanent(t *testing.T) {
	// AppSync answers data:null when every alias failed; that is still a
	// per-book answer, not GraphQL being unavailable.
	fx := newEnrichFixture(t)
	fx.store.SetSetting(settingGraphQLKey, fakeAPIKey)
	fx.store.Reconcile([]Book{{ReviewID: "r1", BookID: "220513519", Title: "The Amber Owl"}}, true, t0)
	fx.f.set(func(f *fakeGoodreads) { f.invalid["220513519"] = true })

	if n, source := fx.step(t); n != 1 || source != "graphql" {
		t.Fatalf("step handled %d via %q, want 1 via graphql", n, source)
	}
	if got := pendingCount(t, fx.store); got != 0 || fx.f.bookPageHits != 0 {
		t.Errorf("pending = %d, book pages = %d; want 0 and 0", got, fx.f.bookPageHits)
	}
}

func TestEnrichRecoversMergedBookIDViaAutocomplete(t *testing.T) {
	// After Goodreads merges editions, the widget's cover file can keep the
	// old id (seen with "The Amber Owl": cover 220513519, book 229099178).
	fx := newEnrichFixture(t)
	fx.store.SetSetting(settingGraphQLKey, fakeAPIKey)
	fx.store.Reconcile([]Book{{ReviewID: "r1", BookID: "220513519", Title: "The Amber Owl", Author: "Juliet Marillier",
		ImageURL: "https://gr/220513519.jpg"}}, true, t0)
	fx.f.set(func(f *fakeGoodreads) {
		f.invalid["220513519"] = true
		f.autoResults["The Amber Owl Juliet Marillier"] = `[{"bookId":"229099178","title":"The Amber Owl (Heartwood, #1)",` +
			`"bookTitleBare":"The Amber Owl","imageUrl":"","author":{"name":"Juliet Marillier"}}]`
		f.books["229099178"] = bookPageData{Desc: "Owls", Pages: 432, WorkTime: 1730419200000}
	})

	if n, _ := fx.step(t); n != 1 {
		t.Fatalf("step handled %d, want 1", n)
	}
	b := fx.stored(t, "r1")
	if b.BookID != "229099178" || b.Description != "Owls" || b.NumPages != "432" {
		t.Errorf("got %+v, want the current id and its details", b)
	}
	if b.BookURL() != "https://www.goodreads.com/book/show/229099178" {
		t.Errorf("BookURL = %q, want the current book", b.BookURL())
	}

	// The next sync brings the stale cover id again; it must not win.
	fx.store.Reconcile([]Book{{ReviewID: "r1", BookID: "220513519", Title: "The Amber Owl", Author: "Juliet Marillier"}}, true, t0)
	if got := fx.stored(t, "r1").BookID; got != "229099178" {
		t.Errorf("after a re-sync BookID = %q, want the corrected 229099178", got)
	}
}

func TestEnrichUnrecoverableMissingBookLinksToSearch(t *testing.T) {
	fx := newEnrichFixture(t)
	fx.store.SetSetting(settingGraphQLKey, fakeAPIKey)
	fx.store.Reconcile([]Book{{ReviewID: "r1", BookID: "404404", Title: "Gone Book", Author: "Nobody"}}, true, t0)

	fx.step(t)
	b := fx.stored(t, "r1")
	if b.BookID != "" {
		t.Errorf("BookID = %q; a book Goodreads does not know must not keep a dead id", b.BookID)
	}
	if !strings.Contains(b.BookURL(), "/search?q=") {
		t.Errorf("BookURL = %q, want a Goodreads search", b.BookURL())
	}
	fx.store.Reconcile([]Book{{ReviewID: "r1", BookID: "404404", Title: "Gone Book"}}, true, t0)
	if got := fx.stored(t, "r1").BookID; got != "" {
		t.Errorf("after a re-sync BookID = %q, want it to stay cleared", got)
	}
}

func TestEnrichSoftNotFoundBookPageIsPermanent(t *testing.T) {
	fx := newEnrichFixture(t)
	fx.addBooks(2)
	fx.store.SetSetting(settingGraphQLKey, fakeAPIKey)
	fx.f.set(func(f *fakeGoodreads) {
		f.gqlBroken = true // forces the book-page fallback
		f.invalid["1"] = true
	})

	if n, _ := fx.step(t); n != 2 {
		t.Fatalf("step handled %d, want 2 (the soft 404 must not stall the batch)", n)
	}
	if got := pendingCount(t, fx.store); got != 0 {
		t.Errorf("pending = %d, want 0", got)
	}
}

func TestEnrichBookPageFallbackNotFoundIsPermanent(t *testing.T) {
	fx := newEnrichFixture(t)
	fx.store.SetSetting(settingGraphQLKey, fakeAPIKey)
	fx.f.set(func(f *fakeGoodreads) { f.gqlBroken = true })
	fx.store.Reconcile([]Book{{ReviewID: "r1", BookID: "404404"}}, true, t0)

	if n, _ := fx.step(t); n != 1 {
		t.Fatalf("step handled %d, want 1", n)
	}
	if got := pendingCount(t, fx.store); got != 0 {
		t.Errorf("pending = %d; a 404 book page must not be retried", got)
	}
}

func TestEnrichResolvesBookIDForEntryWithoutCover(t *testing.T) {
	fx := newEnrichFixture(t)
	fx.store.SetSetting(settingGraphQLKey, fakeAPIKey)
	fx.store.Reconcile([]Book{{ReviewID: "3", Title: "The Cartographer's Daughter", Author: "Cy Meridian"}}, true, t0)
	fx.f.set(func(f *fakeGoodreads) {
		f.autoResults["The Cartographer's Daughter Cy Meridian"] = string(readFixture(t, "auto_complete.json"))
		f.books["333333"] = bookPageData{Desc: "Maps", Image: "https://gr/333333.jpg", Pages: 544}
	})

	fx.step(t)
	b := fx.stored(t, "3")
	if b.BookID != "333333" || b.ImageURL != "https://gr/333333.jpg" || b.Description != "Maps" {
		t.Errorf("got %+v, want the id from autocomplete and details from GraphQL", b)
	}
}

func TestEnrichNoAutocompleteMatchIsPermanent(t *testing.T) {
	fx := newEnrichFixture(t)
	fx.store.SetSetting(settingGraphQLKey, fakeAPIKey)
	fx.store.Reconcile([]Book{{ReviewID: "3", Title: "Nothing Like It", Author: "Nobody"}}, true, t0)

	if n, _ := fx.step(t); n != 1 {
		t.Fatalf("step handled %d, want 1", n)
	}
	if got := pendingCount(t, fx.store); got != 0 {
		t.Errorf("pending = %d; an unmatched book must not be retried", got)
	}
	if fx.f.gqlCalls != 0 {
		t.Errorf("GraphQL called %d times with no book id to look up", fx.f.gqlCalls)
	}
}

func TestEnrichFallsBackToOpenLibraryCover(t *testing.T) {
	fx := newEnrichFixture(t)
	fx.store.SetSetting(settingGraphQLKey, fakeAPIKey)
	fx.store.Reconcile([]Book{{ReviewID: "1", BookID: "11", Title: "T"}}, true, t0)
	fx.olCovers["9780000000011"] = true
	fx.f.set(func(f *fakeGoodreads) {
		f.books["11"] = bookPageData{
			Image:  "https://s.gr-assets.com/assets/nophoto/book/111x148-bcc042a9c91a29c1d680899eff700a03.png",
			ISBN13: "9780000000011"}
	})

	fx.step(t)
	if got, want := fx.stored(t, "1").ImageURL, fx.ol.URL+"/b/isbn/9780000000011-L.jpg"; got != want {
		t.Errorf("ImageURL = %q, want Open Library %q", got, want)
	}
}

func TestEnrichWithoutAnyCoverLeavesItEmpty(t *testing.T) {
	fx := newEnrichFixture(t)
	fx.store.SetSetting(settingGraphQLKey, fakeAPIKey)
	fx.store.Reconcile([]Book{{ReviewID: "1", BookID: "11"}}, true, t0)
	fx.f.set(func(f *fakeGoodreads) { f.books["11"] = bookPageData{Desc: "d", ISBN13: "9780000000099"} })

	fx.step(t)
	if b := fx.stored(t, "1"); b.ImageURL != "" || b.Description != "d" {
		t.Errorf("got %+v", b)
	}
	if len(fx.olHits) != 1 {
		t.Errorf("Open Library asked %d times, want once", len(fx.olHits))
	}
}

func TestEnrichStepWithNothingPending(t *testing.T) {
	fx := newEnrichFixture(t)
	if n, _, err := fx.e.step(context.Background()); n != 0 || err != nil {
		t.Errorf("step = %d, %v; want nothing to do", n, err)
	}
}

func TestEnrichLoopNotifiesAfterEachBatch(t *testing.T) {
	fx := newEnrichFixture(t)
	fx.addBooks(30) // two batches

	done := make(chan struct{}, 4)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go fx.e.run(ctx, func() { done <- struct{}{} })

	for i := 0; i < 2; i++ {
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Fatalf("batch %d did not complete", i+1)
		}
	}
	waitFor(t, "the backlog to empty", func() bool { return pendingCount(t, fx.store) == 0 })
}
