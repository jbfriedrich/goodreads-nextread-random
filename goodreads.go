package main

import (
	"context"
	"errors"
	"fmt"
	"html"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Book is a single entry from a Goodreads shelf.
type Book struct {
	// ReviewID identifies the shelf entry (Goodreads' "review" of the book on
	// this user's shelf). The widget always carries it, so it is the stable key.
	ReviewID      string
	Title         string
	Author        string
	AverageRating string
	NumPages      string
	Published     string
	ISBN          string
	Description   string
	BookID        string
	ImageURL      string
}

// BookURL returns the canonical Goodreads book page URL for this book. When the
// book id is not known yet (e.g. a widget entry without a cover that has not
// been enriched), it falls back to a Goodreads search for title and author.
func (b Book) BookURL() string {
	if b.BookID == "" {
		return "https://www.goodreads.com/search?q=" + url.QueryEscape(strings.TrimSpace(b.Title+" "+b.Author))
	}
	return "https://www.goodreads.com/book/show/" + b.BookID
}

var tagRe = regexp.MustCompile(`<[^>]*>`)
var wsRe = regexp.MustCompile(`\s+`)

// cleanDescription strips HTML tags, unescapes entities, collapses whitespace,
// and truncates to maxLen characters (appending "..." when truncated).
func cleanDescription(desc string, maxLen int) string {
	text := tagRe.ReplaceAllString(desc, " ")
	text = html.UnescapeString(text)
	text = strings.TrimSpace(wsRe.ReplaceAllString(text, " "))

	if maxLen > 0 {
		if runes := []rune(text); len(runes) > maxLen {
			text = strings.TrimSpace(string(runes[:maxLen])) + "..."
		}
	}
	return text
}

// userAgent identifies the app honestly to Goodreads and Open Library.
const userAgent = "Mozilla/5.0 (goodreads-nextread)"

// maxBodyBytes bounds how much of a response is read (book pages are ~650 KB).
const maxBodyBytes = 8 << 20

// grClient talks to Goodreads' public, login-free endpoints for one user's
// shelf. It paces its own requests: consecutive requests are at least delay
// apart. A client is meant to be used by one goroutine at a time; the sync and
// enrichment loops each get their own.
type grClient struct {
	http   *http.Client
	base   string // scheme://host, e.g. https://www.goodreads.com
	userID string
	shelf  string
	delay  time.Duration

	mu   sync.Mutex
	last time.Time

	// profilePath remembers where /user/show/<uid> redirects to, so later
	// profile reads skip the redirect.
	profilePath string
	// requests counts HTTP round trips, redirects included.
	requests atomic.Int64
	// sleep waits between retries; nil means a real, context-aware sleep.
	// Tests replace it to avoid waiting.
	sleep func(context.Context, time.Duration) error
}

// requestCount returns the number of HTTP round trips made so far.
func (c *grClient) requestCount() int { return int(c.requests.Load()) }

// newGRClient builds a client for the shelf described by cfg.
func newGRClient(cfg *Config, delay time.Duration) *grClient {
	return &grClient{
		http:   &http.Client{Timeout: 30 * time.Second},
		base:   cfg.BaseURL,
		userID: cfg.UserID,
		shelf:  cfg.Shelf,
		delay:  delay,
	}
}

// pace blocks until delay has passed since the previous request.
func (c *grClient) pace(ctx context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if wait := c.delay - time.Since(c.last); !c.last.IsZero() && wait > 0 {
		t := time.NewTimer(wait)
		defer t.Stop()
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-t.C:
		}
	}
	c.last = time.Now()
	return nil
}

// get fetches base+path?query and returns the body and the final URL after
// redirects. Non-2xx responses are *httpStatusError. Transient failures (network,
// 429, 5xx, 403) are retried in place with backoff; every attempt is paced and
// counted.
func (c *grClient) get(ctx context.Context, path string, query url.Values) (body []byte, final *url.URL, err error) {
	target := c.base + path
	if len(query) > 0 {
		target += "?" + query.Encode()
	}
	// Count every round trip, including redirects, against the request budget.
	cl := *c.http
	cl.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if len(via) >= 10 {
			return errors.New("stopped after 10 redirects")
		}
		c.requests.Add(1)
		return nil
	}
	err = withRetry(ctx, c.sleep, "GET "+redactQuery(target), func() error {
		if err := c.pace(ctx); err != nil {
			return err
		}
		c.requests.Add(1)
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
		if err != nil {
			return err
		}
		body, final, err = httpDo(&cl, req)
		return err
	})
	return body, final, err
}

// httpDo sends req with the app's User-Agent and returns the body and the
// final URL after redirects. A non-2xx response is an *httpStatusError.
func httpDo(client *http.Client, req *http.Request) ([]byte, *url.URL, error) {
	req.Header.Set("User-Agent", userAgent)
	resp, err := client.Do(req)
	if err != nil {
		return nil, nil, err // *url.Error already names the method and URL
	}
	defer resp.Body.Close()

	body, readErr := io.ReadAll(io.LimitReader(resp.Body, maxBodyBytes))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		snippet := string(body)
		if len(snippet) > 512 {
			snippet = snippet[:512]
		}
		return nil, nil, &httpStatusError{
			Method:     req.Method,
			URL:        redactQuery(req.URL.String()),
			Status:     resp.StatusCode,
			StatusText: resp.Status,
			RetryAfter: parseRetryAfter(resp.Header.Get("Retry-After"), time.Now()),
			Body:       snippet,
		}
	}
	if readErr != nil {
		return nil, nil, fmt.Errorf("%s %s: reading body: %w", req.Method, redactQuery(req.URL.String()), readErr)
	}
	return body, resp.Request.URL, nil
}

// redactQuery drops the query string from a URL for log messages.
func redactQuery(target string) string {
	if i := strings.IndexByte(target, '?'); i >= 0 {
		return target[:i]
	}
	return target
}

// shelfCount reads the shelf's book count from the public profile page.
func (c *grClient) shelfCount(ctx context.Context) (int, error) {
	path := c.profilePath
	if path == "" {
		path = "/user/show/" + c.userID
	}
	body, final, err := c.get(ctx, path, nil)
	if err != nil {
		return 0, fmt.Errorf("fetching profile: %w", err)
	}
	if strings.Contains(final.Path, "sign_in") {
		return 0, fmt.Errorf("profile %s redirected to sign-in; the reader's profile must be public", c.userID)
	}
	n, err := parseShelfCount(body, c.shelf)
	if err == nil && strings.HasPrefix(final.Path, "/user/show/"+c.userID) {
		c.profilePath = final.Path
	}
	return n, err
}

var widgetSeq atomic.Uint64

// newWidgetID returns an id no earlier request used. Goodreads caches widget
// output per widget_id, so reusing one would return stale results.
func newWidgetID() string {
	return fmt.Sprintf("nr%d%d", time.Now().UnixNano(), widgetSeq.Add(1))
}

// widget fetches up to 100 shelf entries in the given order.
func (c *grClient) widget(ctx context.Context, sortKey, order string) ([]Book, error) {
	q := url.Values{
		"shelf":     {c.shelf},
		"num_books": {"100"},
		"sort":      {sortKey},
		"widget_id": {newWidgetID()},
	}
	if order != "" {
		q.Set("order", order)
	}
	body, _, err := c.get(ctx, "/review/custom_widget/"+c.userID, q)
	if err != nil {
		return nil, fmt.Errorf("fetching shelf widget (sort=%s order=%s): %w", sortKey, order, err)
	}
	books, err := parseWidget(body)
	if err != nil {
		return nil, fmt.Errorf("shelf widget (sort=%s order=%s): %w", sortKey, order, err)
	}
	return books, nil
}
