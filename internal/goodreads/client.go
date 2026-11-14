package goodreads

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

// UserAgent identifies the app honestly to Goodreads and Open Library.
const UserAgent = "Mozilla/5.0 (goodreads-nextread)"

// maxBodyBytes bounds how much of a response is read (book pages are ~650 KB).
const maxBodyBytes = 8 << 20

// Client talks to Goodreads' public, login-free endpoints for one user's
// shelf. It paces its own requests: consecutive requests are at least delay
// apart. A client is meant to be used by one goroutine at a time; the sync and
// enrichment loops each get their own.
type Client struct {
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

// RequestCount returns the number of HTTP round trips made so far.
func (c *Client) RequestCount() int { return int(c.requests.Load()) }

// Options configures a Client.
type Options struct {
	// HTTP is the client to use; nil means one with a 30 s timeout.
	HTTP *http.Client
	// BaseURL is scheme://host, e.g. https://www.goodreads.com.
	BaseURL string
	// UserID and Shelf identify the shelf.
	UserID, Shelf string
	// Delay is the minimum gap between requests.
	Delay time.Duration
	// Sleep waits between retries; nil means a real, context-aware sleep.
	Sleep func(context.Context, time.Duration) error
}

// NewClient builds a client for one user's shelf.
func NewClient(o Options) *Client {
	if o.HTTP == nil {
		o.HTTP = &http.Client{Timeout: 30 * time.Second}
	}
	return &Client{http: o.HTTP, base: o.BaseURL, userID: o.UserID, shelf: o.Shelf, delay: o.Delay, sleep: o.Sleep}
}

// pace blocks until delay has passed since the previous request.
func (c *Client) pace(ctx context.Context) error {
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
func (c *Client) get(ctx context.Context, path string, query url.Values) (body []byte, final *url.URL, err error) {
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
	req.Header.Set("User-Agent", UserAgent)
	resp, err := client.Do(req)
	if err != nil {
		return nil, nil, err // *url.Error already names the method and URL
	}
	defer resp.Body.Close()

	body, readErr := io.ReadAll(io.LimitReader(resp.Body, maxBodyBytes))
	waf := resp.Header.Get("X-Amzn-Waf-Action")
	if waf != "" || resp.StatusCode < 200 || resp.StatusCode >= 300 {
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
			WAFAction:  waf,
		}
	}
	if readErr != nil {
		return nil, nil, fmt.Errorf("%s %s: reading body: %w", req.Method, redactQuery(req.URL.String()), readErr)
	}
	return body, resp.Request.URL, nil
}

// pageTitle returns a page's <title> (trimmed, at most 80 characters) to
// identify unexpected pages in error messages.
func pageTitle(page []byte) string {
	m := titleRe.FindSubmatch(page)
	if m == nil {
		return ""
	}
	t := strings.Join(strings.Fields(html.UnescapeString(string(m[1]))), " ")
	if r := []rune(t); len(r) > 80 {
		t = string(r[:80]) + "…"
	}
	return t
}

var titleRe = regexp.MustCompile(`(?is)<title[^>]*>(.*?)</title>`)

// pageError adds the URL and title of an unexpected page to a parse error.
func pageError(final *url.URL, page []byte, err error) error {
	where := "?"
	if final != nil {
		where = redactQuery(final.String())
	}
	return fmt.Errorf("page %s (title %q, %d bytes): %w", where, pageTitle(page), len(page), err)
}

// redactQuery drops the query string from a URL for log messages.
func redactQuery(target string) string {
	if i := strings.IndexByte(target, '?'); i >= 0 {
		return target[:i]
	}
	return target
}

// shelfCount reads the shelf's book count from the public profile page.
func (c *Client) ShelfCount(ctx context.Context) (int, error) {
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
func (c *Client) widget(ctx context.Context, sortKey, order string) ([]Book, error) {
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
