package enrich

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
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

// readGoodreadsFixture reads a fixture shared with the goodreads package.
func readGoodreadsFixture(t *testing.T, name string) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("..", "goodreads", "testdata", name))
	if err != nil {
		t.Fatalf("reading fixture %s: %v", name, err)
	}
	return data
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

type enrichFixture struct {
	f     *goodreadstest.Fake
	store *store.Store
	e     *Enricher
	ol    *httptest.Server
	waits []time.Duration

	mu       sync.Mutex
	olHits   []string
	olCovers map[string]bool
}

func newEnrichFixture(t *testing.T) *enrichFixture {
	t.Helper()
	fx := &enrichFixture{f: goodreadstest.New(t, 0), store: newTestStore(t), olCovers: map[string]bool{}}
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

	gr := goodreads.NewClient(goodreads.Options{
		HTTP: fx.f.Server.Client(), BaseURL: fx.f.Server.URL, UserID: goodreadstest.UID, Shelf: "to-read",
		Sleep: func(ctx context.Context, d time.Duration) error {
			fx.waits = append(fx.waits, d)
			return ctx.Err()
		},
	})
	fx.e = New(gr, goodreads.NewGraphQL(fx.f.Server.Client(), fx.f.Server.URL+"/graphql", gr, fx.store), fx.store, Settings{})
	fx.e.ol = &openLibrary{http: fx.ol.Client(), base: fx.ol.URL}
	fx.e.now = func() time.Time { return t0 }
	return fx
}

// addBooks stores n books with book ids "1".."n" and gives each GraphQL /
// book-page data.
func (fx *enrichFixture) addBooks(n int) {
	var books []goodreads.Book
	fx.f.Set(func(f *goodreadstest.Fake) {
		for i := 1; i <= n; i++ {
			id := strconv.Itoa(i)
			books = append(books, goodreads.Book{ReviewID: "r" + id, BookID: id, Title: "T" + id, ImageURL: "https://gr/w" + id + ".jpg"})
			f.Books[id] = goodreadstest.BookData{Desc: "Blurb " + id, Pages: 100 + i, Rating: 4.1, ISBN13: "97800000000" + id, WorkTime: 1420099200000}
		}
	})
	fx.store.Reconcile(books, true, t0)
}

func (fx *enrichFixture) stored(t *testing.T, reviewID string) goodreads.Book {
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
	return goodreads.Book{}
}

func (fx *enrichFixture) step(t *testing.T) (int, string) {
	t.Helper()
	n, source, err := fx.e.step(context.Background())
	if err != nil {
		t.Fatalf("step: %v", err)
	}
	return n, source
}

func pendingCount(t *testing.T, s *store.Store) int {
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
	if fx.f.GQLCalls != 1 {
		t.Errorf("GraphQL calls = %d, want 1 for 25 books", fx.f.GQLCalls)
	}
	// The key is bootstrapped once from a book page, then cached.
	if fx.f.BookPageHits != 1 {
		t.Errorf("book pages fetched = %d, want 1 (key bootstrap)", fx.f.BookPageHits)
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
	if fx.f.GQLCalls != 2 || fx.f.BookPageHits != 1 {
		t.Errorf("after two steps: %d GraphQL calls, %d book pages; want 2 and 1", fx.f.GQLCalls, fx.f.BookPageHits)
	}
	if got := pendingCount(t, fx.store); got != 0 {
		t.Errorf("pending = %d, want 0", got)
	}
	for _, size := range fx.f.GQLBodySizes {
		if size > goodreads.MaxQueryBody {
			t.Errorf("GraphQL request body %d bytes, over %d", size, goodreads.MaxQueryBody)
		}
	}
}

func TestEnrichReusesStoredKey(t *testing.T) {
	fx := newEnrichFixture(t)
	fx.addBooks(1)
	fx.store.SetSetting(goodreads.SettingGraphQLKey, goodreadstest.APIKey)

	fx.step(t)
	if fx.f.BookPageHits != 0 {
		t.Errorf("book pages fetched = %d, want 0 with a stored key", fx.f.BookPageHits)
	}
}

func TestEnrichRefreshesRotatedKey(t *testing.T) {
	fx := newEnrichFixture(t)
	fx.addBooks(3)
	fx.store.SetSetting(goodreads.SettingGraphQLKey, "da2-stale")

	if n, source := fx.step(t); n != 3 || source != "graphql" {
		t.Fatalf("step enriched %d via %q, want 3 via graphql after a key refresh", n, source)
	}
	if fx.f.BookPageHits != 1 || fx.f.GQLCalls != 2 {
		t.Errorf("book pages = %d, GraphQL calls = %d; want 1 refresh and 2 calls (401, then success)", fx.f.BookPageHits, fx.f.GQLCalls)
	}
	if key, _ := fx.store.Setting(goodreads.SettingGraphQLKey); key != goodreadstest.APIKey {
		t.Errorf("stored key = %q, want the refreshed key", key)
	}
}

func TestEnrichNotFoundIsPermanent(t *testing.T) {
	fx := newEnrichFixture(t)
	fx.store.SetSetting(goodreads.SettingGraphQLKey, goodreadstest.APIKey)
	fx.store.Reconcile([]goodreads.Book{{ReviewID: "r1", BookID: "404404", Title: "Gone"}}, true, t0) // unknown to GraphQL

	if n, _ := fx.step(t); n != 1 {
		t.Fatalf("step handled %d books, want 1", n)
	}
	if got := pendingCount(t, fx.store); got != 0 {
		t.Errorf("pending = %d; a book that does not exist must not be retried", got)
	}
	if n, _ := fx.step(t); n != 0 || fx.f.GQLCalls != 1 {
		t.Errorf("second step handled %d books with %d GraphQL calls; want nothing more", n, fx.f.GQLCalls)
	}
}

func TestEnrichRetriesThrottledGraphQLInPlace(t *testing.T) {
	fx := newEnrichFixture(t)
	fx.addBooks(2)
	fx.store.SetSetting(goodreads.SettingGraphQLKey, goodreadstest.APIKey)
	fx.f.Set(func(f *goodreadstest.Fake) { f.GQLThrottle = 1 })

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
	fx.store.SetSetting(goodreads.SettingGraphQLKey, goodreadstest.APIKey)
	fx.f.Set(func(f *goodreadstest.Fake) { f.GQLFail = 100 })

	if _, _, err := fx.e.step(context.Background()); err == nil {
		t.Fatal("expected an error once retries are used up")
	}
	if got := pendingCount(t, fx.store); got != 2 {
		t.Errorf("pending = %d, want both books still pending", got)
	}
	if fx.f.GQLCalls != 1+goodreads.MaxRetries() {
		t.Errorf("GraphQL calls = %d, want %d (first try + retries)", fx.f.GQLCalls, 1+goodreads.MaxRetries())
	}
	if fx.f.BookPageHits != 0 {
		t.Errorf("fell back to %d book pages on a transient error; it should wait instead", fx.f.BookPageHits)
	}
}

func TestEnrichFallsBackToBookPagesWhenGraphQLBreaks(t *testing.T) {
	fx := newEnrichFixture(t)
	fx.addBooks(3)
	fx.store.SetSetting(goodreads.SettingGraphQLKey, goodreadstest.APIKey)
	fx.f.Set(func(f *goodreadstest.Fake) { f.GQLBroken = true })

	n, source := fx.step(t)
	if n != 3 || source != "book pages" {
		t.Fatalf("step enriched %d via %q, want 3 via book pages", n, source)
	}
	if b := fx.stored(t, "r2"); b.Description != "Blurb 2" || b.NumPages != "102" || b.Published != "2015" {
		t.Errorf("fallback details not mapped: %+v", b)
	}
	if fx.f.BookPageHits != 3 {
		t.Errorf("book pages fetched = %d, want 3", fx.f.BookPageHits)
	}

	// GraphQL is tried again on the next batch.
	fx.f.Set(func(f *goodreadstest.Fake) { f.GQLBroken = false })
	fx.store.Reconcile([]goodreads.Book{{ReviewID: "r9", BookID: "9"}}, false, t0)
	fx.f.Set(func(f *goodreadstest.Fake) { f.Books["9"] = goodreadstest.BookData{Desc: "nine"} })
	if _, source := fx.step(t); source != "graphql" {
		t.Errorf("next batch went via %q, want graphql again", source)
	}
}

func TestEnrichGraphQLResourceNotFoundIsPermanent(t *testing.T) {
	fx := newEnrichFixture(t)
	fx.addBooks(3)
	fx.store.SetSetting(goodreads.SettingGraphQLKey, goodreadstest.APIKey)
	fx.f.Set(func(f *goodreadstest.Fake) { f.Invalid["2"] = true })

	n, source := fx.step(t)
	if n != 3 || source != "graphql" {
		t.Fatalf("step handled %d via %q, want all 3 via graphql", n, source)
	}
	if got := pendingCount(t, fx.store); got != 0 {
		t.Errorf("pending = %d; an invalid book must be final, not retried", got)
	}
	if fx.f.BookPageHits != 0 {
		t.Errorf("fetched %d book pages; GraphQL already said the book does not exist", fx.f.BookPageHits)
	}
	if b := fx.stored(t, "r1"); b.Description != "Blurb 1" {
		t.Errorf("the valid books in the batch were not enriched: %+v", b)
	}
}

func TestEnrichBatchOfOnlyInvalidBooksIsPermanent(t *testing.T) {
	// AppSync answers data:null when every alias failed; that is still a
	// per-book answer, not GraphQL being unavailable.
	fx := newEnrichFixture(t)
	fx.store.SetSetting(goodreads.SettingGraphQLKey, goodreadstest.APIKey)
	fx.store.Reconcile([]goodreads.Book{{ReviewID: "r1", BookID: "220513519", Title: "The Amber Owl"}}, true, t0)
	fx.f.Set(func(f *goodreadstest.Fake) { f.Invalid["220513519"] = true })

	if n, source := fx.step(t); n != 1 || source != "graphql" {
		t.Fatalf("step handled %d via %q, want 1 via graphql", n, source)
	}
	if got := pendingCount(t, fx.store); got != 0 || fx.f.BookPageHits != 0 {
		t.Errorf("pending = %d, book pages = %d; want 0 and 0", got, fx.f.BookPageHits)
	}
}

func TestEnrichRecoversMergedBookIDViaAutocomplete(t *testing.T) {
	// After Goodreads merges editions, the widget's cover file can keep the
	// old id (seen with "The Amber Owl": cover 220513519, book 229099178).
	fx := newEnrichFixture(t)
	fx.store.SetSetting(goodreads.SettingGraphQLKey, goodreadstest.APIKey)
	fx.store.Reconcile([]goodreads.Book{{ReviewID: "r1", BookID: "220513519", Title: "The Amber Owl", Author: "Juliet Marillier",
		ImageURL: "https://gr/220513519.jpg"}}, true, t0)
	fx.f.Set(func(f *goodreadstest.Fake) {
		f.Invalid["220513519"] = true
		f.AutoResults["The Amber Owl Juliet Marillier"] = `[{"bookId":"229099178","title":"The Amber Owl (Heartwood, #1)",` +
			`"bookTitleBare":"The Amber Owl","imageUrl":"","author":{"name":"Juliet Marillier"}}]`
		f.Books["229099178"] = goodreadstest.BookData{Desc: "Owls", Pages: 432, WorkTime: 1730419200000}
	})

	if n, _ := fx.step(t); n != 1 {
		t.Fatalf("step handled %d, want 1", n)
	}
	b := fx.stored(t, "r1")
	if b.BookID != "229099178" || b.Description != "Owls" || b.NumPages != "432" {
		t.Errorf("got %+v, want the current id and its details", b)
	}
	if b.URL() != "https://www.goodreads.com/book/show/229099178" {
		t.Errorf("URL() = %q, want the current book", b.URL())
	}

	// The next sync brings the stale cover id again; it must not win.
	fx.store.Reconcile([]goodreads.Book{{ReviewID: "r1", BookID: "220513519", Title: "The Amber Owl", Author: "Juliet Marillier"}}, true, t0)
	if got := fx.stored(t, "r1").BookID; got != "229099178" {
		t.Errorf("after a re-sync BookID = %q, want the corrected 229099178", got)
	}
}

func TestEnrichUnrecoverableMissingBookLinksToSearch(t *testing.T) {
	fx := newEnrichFixture(t)
	fx.store.SetSetting(goodreads.SettingGraphQLKey, goodreadstest.APIKey)
	fx.store.Reconcile([]goodreads.Book{{ReviewID: "r1", BookID: "404404", Title: "Gone Book", Author: "Nobody"}}, true, t0)

	fx.step(t)
	b := fx.stored(t, "r1")
	if b.BookID != "" {
		t.Errorf("BookID = %q; a book Goodreads does not know must not keep a dead id", b.BookID)
	}
	if !strings.Contains(b.URL(), "/search?q=") {
		t.Errorf("URL() = %q, want a Goodreads search", b.URL())
	}
	fx.store.Reconcile([]goodreads.Book{{ReviewID: "r1", BookID: "404404", Title: "Gone Book"}}, true, t0)
	if got := fx.stored(t, "r1").BookID; got != "" {
		t.Errorf("after a re-sync BookID = %q, want it to stay cleared", got)
	}
}

func TestEnrichSoftNotFoundBookPageIsPermanent(t *testing.T) {
	fx := newEnrichFixture(t)
	fx.addBooks(2)
	fx.store.SetSetting(goodreads.SettingGraphQLKey, goodreadstest.APIKey)
	fx.f.Set(func(f *goodreadstest.Fake) {
		f.GQLBroken = true // forces the book-page fallback
		f.Invalid["1"] = true
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
	fx.store.SetSetting(goodreads.SettingGraphQLKey, goodreadstest.APIKey)
	fx.f.Set(func(f *goodreadstest.Fake) { f.GQLBroken = true })
	fx.store.Reconcile([]goodreads.Book{{ReviewID: "r1", BookID: "404404"}}, true, t0)

	if n, _ := fx.step(t); n != 1 {
		t.Fatalf("step handled %d, want 1", n)
	}
	if got := pendingCount(t, fx.store); got != 0 {
		t.Errorf("pending = %d; a 404 book page must not be retried", got)
	}
}

func TestEnrichResolvesBookIDForEntryWithoutCover(t *testing.T) {
	fx := newEnrichFixture(t)
	fx.store.SetSetting(goodreads.SettingGraphQLKey, goodreadstest.APIKey)
	fx.store.Reconcile([]goodreads.Book{{ReviewID: "3", Title: "The Cartographer's Daughter", Author: "Cy Meridian"}}, true, t0)
	fx.f.Set(func(f *goodreadstest.Fake) {
		f.AutoResults["The Cartographer's Daughter Cy Meridian"] = string(readGoodreadsFixture(t, "auto_complete.json"))
		f.Books["333333"] = goodreadstest.BookData{Desc: "Maps", Image: "https://gr/333333.jpg", Pages: 544}
	})

	fx.step(t)
	b := fx.stored(t, "3")
	if b.BookID != "333333" || b.ImageURL != "https://gr/333333.jpg" || b.Description != "Maps" {
		t.Errorf("got %+v, want the id from autocomplete and details from GraphQL", b)
	}
}

func TestEnrichNoAutocompleteMatchIsPermanent(t *testing.T) {
	fx := newEnrichFixture(t)
	fx.store.SetSetting(goodreads.SettingGraphQLKey, goodreadstest.APIKey)
	fx.store.Reconcile([]goodreads.Book{{ReviewID: "3", Title: "Nothing Like It", Author: "Nobody"}}, true, t0)

	if n, _ := fx.step(t); n != 1 {
		t.Fatalf("step handled %d, want 1", n)
	}
	if got := pendingCount(t, fx.store); got != 0 {
		t.Errorf("pending = %d; an unmatched book must not be retried", got)
	}
	if fx.f.GQLCalls != 0 {
		t.Errorf("GraphQL called %d times with no book id to look up", fx.f.GQLCalls)
	}
}

func TestEnrichFallsBackToOpenLibraryCover(t *testing.T) {
	fx := newEnrichFixture(t)
	fx.store.SetSetting(goodreads.SettingGraphQLKey, goodreadstest.APIKey)
	fx.store.Reconcile([]goodreads.Book{{ReviewID: "1", BookID: "11", Title: "T"}}, true, t0)
	fx.olCovers["9780000000011"] = true
	fx.f.Set(func(f *goodreadstest.Fake) {
		f.Books["11"] = goodreadstest.BookData{
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
	fx.store.SetSetting(goodreads.SettingGraphQLKey, goodreadstest.APIKey)
	fx.store.Reconcile([]goodreads.Book{{ReviewID: "1", BookID: "11"}}, true, t0)
	fx.f.Set(func(f *goodreadstest.Fake) {
		f.Books["11"] = goodreadstest.BookData{Desc: "d", ISBN13: "9780000000099"}
	})

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
	go fx.e.Run(ctx, func() { done <- struct{}{} })

	for i := 0; i < 2; i++ {
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Fatalf("batch %d did not complete", i+1)
		}
	}
	waitFor(t, "the backlog to empty", func() bool { return pendingCount(t, fx.store) == 0 })
}

func TestEnrichLoopPausesForConfiguredTimeAfterError(t *testing.T) {
	// With the default 5-minute pause this test would time out: it passes only
	// if the configured pause is used.
	fx := newEnrichFixture(t)
	fx.addBooks(3)
	fx.store.SetSetting(goodreads.SettingGraphQLKey, goodreadstest.APIKey)
	fx.e.errorPause = 20 * time.Millisecond
	fx.f.Set(func(f *goodreadstest.Fake) { f.GQLFail = 1 + goodreads.MaxRetries() }) // exactly one failed step

	done := make(chan struct{}, 4)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go fx.e.Run(ctx, func() { done <- struct{}{} })

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("enrichment did not resume after the configured pause")
	}
	if got := pendingCount(t, fx.store); got != 0 {
		t.Errorf("pending = %d, want 0", got)
	}
}
