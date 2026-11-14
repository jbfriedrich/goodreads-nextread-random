package goodreads

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// flakyServer answers with the given responses in order, then 200 "ok".
type flakyResponse struct {
	status     int
	body       string
	retryAfter string
	wafAction  string // sent as X-Amzn-Waf-Action, like AWS WAF challenges
}

func newFlakyClient(t *testing.T, responses ...flakyResponse) (*Client, *[]time.Duration, *int) {
	t.Helper()
	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		i := calls
		calls++
		if i < len(responses) {
			resp := responses[i]
			if resp.retryAfter != "" {
				w.Header().Set("Retry-After", resp.retryAfter)
			}
			if resp.wafAction != "" {
				w.Header().Set("X-Amzn-Waf-Action", resp.wafAction)
			}
			w.WriteHeader(resp.status)
			fmt.Fprint(w, resp.body)
			return
		}
		fmt.Fprint(w, "ok")
	}))
	t.Cleanup(srv.Close)

	var waits []time.Duration
	c := &Client{http: srv.Client(), base: srv.URL, sleep: func(ctx context.Context, d time.Duration) error {
		waits = append(waits, d)
		return ctx.Err()
	}}
	return c, &waits, &calls
}

func TestGetTreatsWAFChallengeAsTransient(t *testing.T) {
	// AWS WAF answers a challenge with a 2xx (202) and a JavaScript page; it
	// must not be mistaken for the real content.
	c, _, calls := newFlakyClient(t,
		flakyResponse{status: http.StatusAccepted, body: "<html>challenge</html>", wafAction: "challenge"})
	body, _, err := c.get(context.Background(), "/x", nil)
	if err != nil || string(body) != "ok" {
		t.Fatalf("get = %q, %v; want ok after retrying the challenge", body, err)
	}
	if *calls != 2 {
		t.Errorf("calls = %d, want 2 (the challenge, then the retry)", *calls)
	}
}

func TestWAFChallengeErrorSaysSo(t *testing.T) {
	var many []flakyResponse
	for i := 0; i < 10; i++ {
		many = append(many, flakyResponse{status: http.StatusAccepted, wafAction: "challenge"})
	}
	c, _, _ := newFlakyClient(t, many...)
	_, _, err := c.get(context.Background(), "/x", nil)
	if err == nil || Classify(err) != ErrTransient {
		t.Fatalf("err = %v, want a transient error", err)
	}
	if !strings.Contains(err.Error(), "WAF challenge") || !strings.Contains(err.Error(), "202") {
		t.Errorf("error %q should name the WAF challenge and the status", err)
	}
}

func TestGetRetriesTransientErrorsWithBackoff(t *testing.T) {
	c, waits, calls := newFlakyClient(t,
		flakyResponse{status: 500}, flakyResponse{status: 503})
	body, _, err := c.get(context.Background(), "/x", nil)
	if err != nil || string(body) != "ok" {
		t.Fatalf("get = %q, %v; want ok after retries", body, err)
	}
	if *calls != 3 {
		t.Errorf("calls = %d, want 3", *calls)
	}
	if want := []time.Duration{30 * time.Second, time.Minute}; !equalDurations(*waits, want) {
		t.Errorf("waits = %v, want %v", *waits, want)
	}
}

func TestGetHonoursRetryAfterOn429(t *testing.T) {
	c, waits, _ := newFlakyClient(t, flakyResponse{status: 429, retryAfter: "7"})
	if _, _, err := c.get(context.Background(), "/x", nil); err != nil {
		t.Fatal(err)
	}
	if want := []time.Duration{7 * time.Second}; !equalDurations(*waits, want) {
		t.Errorf("waits = %v, want %v", *waits, want)
	}
}

func TestGetRetriesWAFForbidden(t *testing.T) {
	c, _, calls := newFlakyClient(t,
		flakyResponse{status: 403, body: `{"errors":[{"errorType":"WAFForbiddenException","message":"403 Forbidden"}]}`})
	if _, _, err := c.get(context.Background(), "/x", nil); err != nil {
		t.Fatal(err)
	}
	if *calls != 2 {
		t.Errorf("calls = %d, want 2 (one retry)", *calls)
	}
}

func TestGetDoesNotRetryNotFound(t *testing.T) {
	c, waits, calls := newFlakyClient(t, flakyResponse{status: 404})
	_, _, err := c.get(context.Background(), "/x", nil)
	if err == nil {
		t.Fatal("expected an error for 404")
	}
	if Classify(err) != ErrPermanent {
		t.Errorf("404 classified as %v, want permanent", Classify(err))
	}
	if *calls != 1 || len(*waits) != 0 {
		t.Errorf("calls = %d, waits = %v; a 404 must not be retried", *calls, *waits)
	}
}

func TestGetGivesUpAfterFiveRetries(t *testing.T) {
	var many []flakyResponse
	for i := 0; i < 10; i++ {
		many = append(many, flakyResponse{status: 502})
	}
	c, waits, calls := newFlakyClient(t, many...)
	_, _, err := c.get(context.Background(), "/x", nil)
	if err == nil || Classify(err) != ErrTransient {
		t.Fatalf("err = %v, want the transient error after giving up", err)
	}
	if *calls != 6 || len(*waits) != 5 {
		t.Errorf("calls = %d, waits = %v; want 6 calls and 5 waits", *calls, *waits)
	}
	want := []time.Duration{30 * time.Second, time.Minute, 2 * time.Minute, 4 * time.Minute, 8 * time.Minute}
	if !equalDurations(*waits, want) {
		t.Errorf("waits = %v, want %v", *waits, want)
	}
}

func TestGetStopsRetryingWhenContextCancelled(t *testing.T) {
	c, _, calls := newFlakyClient(t, flakyResponse{status: 500}, flakyResponse{status: 500})
	ctx, cancel := context.WithCancel(context.Background())
	c.sleep = func(context.Context, time.Duration) error { cancel(); return context.Canceled }
	if _, _, err := c.get(ctx, "/x", nil); !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want context.Canceled", err)
	}
	if *calls != 1 {
		t.Errorf("calls = %d, want 1", *calls)
	}
}

func TestClassify(t *testing.T) {
	tests := []struct {
		err  error
		want ErrClass
	}{
		{&httpStatusError{Status: 404}, ErrPermanent},
		{&httpStatusError{Status: 410}, ErrPermanent},
		{&httpStatusError{Status: 400}, ErrPermanent},
		{&httpStatusError{Status: 401}, ErrAuth},
		{&httpStatusError{Status: 403}, ErrTransient},
		{&httpStatusError{Status: 429}, ErrTransient},
		{&httpStatusError{Status: 500}, ErrTransient},
		{fmt.Errorf("wrapped: %w", &httpStatusError{Status: 503}), ErrTransient},
		{&net.OpError{Op: "dial", Err: errors.New("connection refused")}, ErrTransient},
		{errors.New("parse error"), ErrPermanent},
	}
	for _, tt := range tests {
		if got := Classify(tt.err); got != tt.want {
			t.Errorf("Classify(%v) = %v, want %v", tt.err, got, tt.want)
		}
	}
}

func TestParseRetryAfter(t *testing.T) {
	now := time.Date(2026, 11, 9, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		in   string
		want time.Duration
	}{
		{"", 0},
		{"12", 12 * time.Second},
		{"Mon, 09 Nov 2026 12:01:30 GMT", 90 * time.Second},
		{"99999", maxRetryAfter}, // capped
		{"soon", 0},
	}
	for _, tt := range tests {
		if got := parseRetryAfter(tt.in, now); got != tt.want {
			t.Errorf("parseRetryAfter(%q) = %v, want %v", tt.in, got, tt.want)
		}
	}
}

func equalDurations(a, b []time.Duration) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
