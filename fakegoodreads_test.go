package main

import (
	"context"
	"encoding/json"
	"fmt"
	"hash/fnv"
	"io"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeGoodreads emulates the public Goodreads endpoints the app uses: the
// profile page (with the shelf count, behind a redirect like the real site),
// the custom shelf widget (max 100 entries, honouring sort/order/random), the
// book page and the autocomplete JSON. Shelf contents can be changed between
// calls to simulate the reader editing their shelf.
type fakeGoodreads struct {
	t   *testing.T
	srv *httptest.Server

	mu          sync.Mutex
	shelf       []fakeEntry // in date-added order (last = newest)
	nextID      int
	widgetIDs   map[string]int
	requests    int
	widgetCalls int
	failWidget  bool
	emptyWidget bool
	countDelta  int // added to the reported profile count (simulates a mismatch)
	failAfter   int // if > 0, widget calls after this many fail with HTTP 500
	throttle    int // the next this-many widget calls answer 429 (Retry-After: 1)
	// gate, if set, holds every widget call after the first until it is closed,
	// so a test can observe the state in the middle of a harvest.
	gate         chan struct{}
	books        map[string]bookPageData // served as book pages and via GraphQL
	autoResults  map[string]string
	bookPageHits int

	// GraphQL emulation (POST /graphql).
	apiKey       string // the key GraphQL accepts; book pages embed fakeAPIKey
	gqlCalls     int
	gqlBodySizes []int
	gqlThrottle  int  // the next this-many GraphQL calls answer 429
	gqlFail      int  // the next this-many GraphQL calls answer 500
	gqlBroken    bool // answer every query with a validation error (schema change)
	// invalid book ids behave like Goodreads' removed books: GraphQL answers
	// RESOURCE_NOT_FOUND for the alias, the book page is a soft 404 (200,
	// "Page not found", getBookByLegacyId -> null).
	invalid map[string]bool
}

type fakeEntry struct {
	ReviewID, BookID, Title, Author string
	NoCover                         bool
}

const fakeUID = "12345678"

func newFakeGoodreads(t *testing.T, n int) *fakeGoodreads {
	t.Helper()
	f := &fakeGoodreads{t: t, widgetIDs: map[string]int{}, books: map[string]bookPageData{}, autoResults: map[string]string{}, apiKey: fakeAPIKey, invalid: map[string]bool{}}
	for i := 0; i < n; i++ {
		f.add()
	}
	f.srv = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.srv.Close)
	return f
}

// add appends a new (newest) entry to the shelf and returns it.
func (f *fakeGoodreads) add() fakeEntry {
	f.nextID++
	e := fakeEntry{
		ReviewID: strconv.Itoa(9000000000 + f.nextID),
		BookID:   strconv.Itoa(100000 + f.nextID),
		Title:    fmt.Sprintf("Book %d", f.nextID),
		Author:   fmt.Sprintf("Author %d", f.nextID%17),
	}
	f.shelf = append(f.shelf, e)
	return e
}

func (f *fakeGoodreads) addEntry() fakeEntry {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.add()
}

// remove deletes the entry at index i of the date-added order.
func (f *fakeGoodreads) remove(i int) fakeEntry {
	f.mu.Lock()
	defer f.mu.Unlock()
	e := f.shelf[i]
	f.shelf = append(f.shelf[:i:i], f.shelf[i+1:]...)
	return e
}

func (f *fakeGoodreads) set(fn func(f *fakeGoodreads)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	fn(f)
}

func (f *fakeGoodreads) stats() (requests, widgetCalls int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.requests, f.widgetCalls
}

func (f *fakeGoodreads) listURL() string {
	return f.srv.URL + "/review/list/" + fakeUID + "?shelf=to-read"
}

func (f *fakeGoodreads) client() *grClient {
	return &grClient{http: f.srv.Client(), base: f.srv.URL, userID: fakeUID, shelf: "to-read", sleep: noSleep}
}

// noSleep skips retry waits in tests.
func noSleep(ctx context.Context, _ time.Duration) error { return ctx.Err() }

func (f *fakeGoodreads) serve(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/review/custom_widget/"+fakeUID {
		// Wait outside the lock so other requests are not blocked.
		f.mu.Lock()
		gate, calls := f.gate, f.widgetCalls
		f.mu.Unlock()
		if gate != nil && calls >= 1 {
			<-gate
		}
	}

	f.mu.Lock()
	defer f.mu.Unlock()
	f.requests++

	switch {
	case r.URL.Path == "/user/show/"+fakeUID:
		http.Redirect(w, r, "/user/show/"+fakeUID+"-reader", http.StatusMovedPermanently)
	case r.URL.Path == "/user/show/"+fakeUID+"-reader":
		fmt.Fprintf(w, `<html><body>
<a class="actionLinkLite userShowPageShelfListItem" href="/review/list/%[1]s?shelf=read">read&lrm; (5)</a>
<a class="actionLinkLite userShowPageShelfListItem" href="/review/list/%[1]s?shelf=to-read">
  to-read&lrm;
  (%[2]d)
</a></body></html>`, fakeUID, len(f.shelf)+f.countDelta)
	case r.URL.Path == "/review/custom_widget/"+fakeUID:
		f.serveWidget(w, r)
	case strings.HasPrefix(r.URL.Path, "/book/show/"):
		f.bookPageHits++
		id := strings.TrimPrefix(r.URL.Path, "/book/show/")
		if f.invalid[id] {
			fmt.Fprint(w, softNotFoundPage(id))
			return
		}
		d, ok := f.books[id]
		if !ok {
			http.NotFound(w, r)
			return
		}
		fmt.Fprint(w, bookPageHTML(id, d, fakeAPIKey))
	case r.URL.Path == "/graphql":
		f.serveGraphQL(w, r)
	case r.URL.Path == "/book/auto_complete":
		res, ok := f.autoResults[r.URL.Query().Get("q")]
		if !ok {
			res = "[]"
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, res)
	default:
		http.NotFound(w, r)
	}
}

func (f *fakeGoodreads) serveWidget(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	f.widgetCalls++
	f.widgetIDs[q.Get("widget_id")]++
	if q.Get("shelf") != "to-read" {
		f.t.Errorf("widget requested for shelf %q", q.Get("shelf"))
	}
	if f.throttle > 0 && f.widgetCalls > 1 {
		f.throttle--
		w.Header().Set("Retry-After", "1")
		http.Error(w, "slow down", http.StatusTooManyRequests)
		return
	}
	if f.failWidget || (f.failAfter > 0 && f.widgetCalls > f.failAfter) {
		http.Error(w, "boom", http.StatusInternalServerError)
		return
	}

	entries := append([]fakeEntry(nil), f.shelf...)
	switch key := q.Get("sort"); key {
	case "date_added":
		// already in date-added order
	case "random":
		rand.Shuffle(len(entries), func(i, j int) { entries[i], entries[j] = entries[j], entries[i] })
	default:
		// Every other sort key is a different deterministic permutation.
		h := fnv.New64a()
		h.Write([]byte(key))
		rng := rand.New(rand.NewSource(int64(h.Sum64())))
		perm := rng.Perm(len(entries))
		shuffled := make([]fakeEntry, len(entries))
		for i, p := range perm {
			shuffled[i] = entries[p]
		}
		entries = shuffled
	}
	if q.Get("order") == "d" {
		for i, j := 0, len(entries)-1; i < j; i, j = i+1, j-1 {
			entries[i], entries[j] = entries[j], entries[i]
		}
	}

	limit, _ := strconv.Atoi(q.Get("num_books"))
	if limit <= 0 || limit > 100 {
		limit = 100 // Goodreads caps the widget at 100 books
	}
	if len(entries) > limit {
		entries = entries[:limit]
	}
	if f.emptyWidget {
		entries = nil
	}

	w.Header().Set("Content-Type", "text/javascript; charset=utf-8")
	fmt.Fprint(w, widgetJS(q.Get("widget_id"), entries))
}

// softNotFoundPage is what Goodreads serves for a removed book: HTTP 200,
// "Page not found", and Apollo state whose lookup for the id is null.
func softNotFoundPage(id string) string {
	data, _ := json.Marshal(map[string]any{"props": map[string]any{"pageProps": map[string]any{
		"apiKey": fakeAPIKey,
		"apolloState": map[string]any{"ROOT_QUERY": map[string]any{
			"__typename": "Query",
			`getBookByLegacyId({"legacyId":"` + id + `"})`: nil,
		}},
	}}})
	return `<html><head><title>Page not found</title></head><body><script id="__NEXT_DATA__" type="application/json">` + string(data) + `</script></body></html>`
}

// fakeAPIKey is the AppSync key the fake's book pages embed.
const fakeAPIKey = "da2-fakekey"

var gqlAliasRe = regexp.MustCompile(`(b\d+):getBookByLegacyId\(legacyId:(\d+)\)`)

// serveGraphQL emulates AppSync's getBookByLegacyId with aliases: unknown ids
// come back null, a wrong x-api-key is a 401 UnauthorizedException.
func (f *fakeGoodreads) serveGraphQL(w http.ResponseWriter, r *http.Request) {
	f.gqlCalls++
	body, _ := io.ReadAll(r.Body)
	f.gqlBodySizes = append(f.gqlBodySizes, len(body))
	w.Header().Set("Content-Type", "application/json")

	if r.Header.Get("x-api-key") != f.apiKey {
		w.WriteHeader(http.StatusUnauthorized)
		fmt.Fprint(w, `{"errors":[{"errorType":"UnauthorizedException","message":"You are not authorized to make this call."}]}`)
		return
	}
	if f.gqlThrottle > 0 {
		f.gqlThrottle--
		w.Header().Set("Retry-After", "2")
		w.WriteHeader(http.StatusTooManyRequests)
		fmt.Fprint(w, `{"errors":[{"errorType":"TooManyRequestsException"}]}`)
		return
	}
	if f.gqlFail > 0 {
		f.gqlFail--
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	if f.gqlBroken {
		fmt.Fprint(w, `{"data":null,"errors":[{"errorType":"ValidationError","message":"Validation error of type FieldUndefined: Field 'legacyId' in type 'Book' is undefined"}]}`)
		return
	}

	var req struct {
		Query string `json:"query"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		http.Error(w, "bad json", http.StatusBadRequest)
		return
	}
	data := map[string]any{}
	var errs []map[string]any
	matches := gqlAliasRe.FindAllStringSubmatch(req.Query, -1)
	for _, m := range matches {
		switch d, ok := f.books[m[2]]; {
		case f.invalid[m[2]]:
			data[m[1]] = nil
			errs = append(errs, map[string]any{"path": []any{m[1]}, "errorType": "RESOURCE_NOT_FOUND", "message": "Book is invalid"})
		case ok:
			data[m[1]] = gqlBookJSON(m[2], d)
		default:
			data[m[1]] = nil
		}
	}
	resp := map[string]any{"data": data}
	if len(errs) > 0 {
		resp["errors"] = errs
		if len(errs) == len(matches) {
			resp["data"] = nil // AppSync drops data when every alias failed
		}
	}
	json.NewEncoder(w).Encode(resp)
}

// gqlBookJSON renders d the way AppSync returns a Book (publicationTime as a
// float, like the real API).
func gqlBookJSON(id string, d bookPageData) map[string]any {
	legacy, _ := strconv.Atoi(id)
	b := map[string]any{
		"legacyId":    legacy,
		"description": d.Desc,
		"imageUrl":    d.Image,
		"details": map[string]any{
			"numPages": d.Pages, "publicationTime": float64(d.EditionTime), "isbn13": d.ISBN13, "isbn": d.ISBN10,
		},
		"work": nil,
	}
	if d.WorkTime != 0 || d.Rating != 0 {
		b["work"] = map[string]any{
			"stats":   map[string]any{"averageRating": d.Rating},
			"details": map[string]any{"publicationTime": float64(d.WorkTime)},
		}
	}
	return b
}

// widgetJS renders entries the way Goodreads does: HTML inside a
// single-quoted JS string with \" \/ \n \' escapes.
func widgetJS(widgetID string, entries []fakeEntry) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, `<div class="gr_custom_container_%s">`, widgetID)
	for _, e := range entries {
		cover := fmt.Sprintf("https://i.gr-assets.com/images/S/compressed.photo.goodreads.com/books/1700000000l/%s._SY75_.jpg", e.BookID)
		if e.NoCover {
			cover = "https://s.gr-assets.com/assets/nophoto/book/50x75-1b903c9eac1b2ba789662c08a0ae41e6.png"
		}
		review := "https://www.goodreads.com/review/show/" + e.ReviewID + "?utm_medium=api&amp;utm_source=custom_widget"
		fmt.Fprintf(&sb, `
      <div class="gr_custom_each_container_%[1]s">
          <div class="gr_custom_book_container_%[1]s">
            <a title="%[2]s" rel="nofollow" href="%[3]s"><img alt="%[2]s" border="0" src="%[4]s" /></a>
          </div>
          <div class="gr_custom_title_%[1]s">
            <a rel="nofollow" href="%[3]s">%[2]s</a>
          </div>
          <div class="gr_custom_author_%[1]s">
            by <a rel="nofollow" href="https://www.goodreads.com/author/show/1.X">%[5]s</a>
          </div>
      </div>`, widgetID, e.Title, review, cover, e.Author)
	}
	sb.WriteString("\n</div>\n")

	escaped := strings.NewReplacer(`\`, `\\`, `"`, `\"`, `/`, `\/`, "\n", `\n`, `'`, `\'`).Replace(sb.String())
	return "  var widget_code = '" + escaped + "'\n  document.write(widget_code)\n"
}
