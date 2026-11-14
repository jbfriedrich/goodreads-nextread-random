package goodreads

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// ErrClass says how a failed request should be handled.
type ErrClass int

const (
	// ErrPermanent: retrying will not help (404, other 4xx, unparseable data).
	ErrPermanent ErrClass = iota
	// ErrTransient: wait and retry the same request (network, 429, 5xx, WAF).
	ErrTransient
	// ErrAuth: credentials were rejected (401); only the GraphQL client acts
	// on it, by fetching a fresh API key.
	ErrAuth
)

func (c ErrClass) String() string {
	switch c {
	case ErrTransient:
		return "transient"
	case ErrAuth:
		return "auth"
	default:
		return "permanent"
	}
}

// retryBackoff is the wait before each retry of a transient failure when the
// server gives no Retry-After. Its length is the number of retries.
var retryBackoff = []time.Duration{30 * time.Second, time.Minute, 2 * time.Minute, 4 * time.Minute, 8 * time.Minute}

// MaxRetries is how many times a transient failure is retried before the
// error is returned.
func MaxRetries() int { return len(retryBackoff) }

// maxRetryAfter caps a server-supplied Retry-After.
const maxRetryAfter = 15 * time.Minute

// httpStatusError is a non-2xx response.
type httpStatusError struct {
	Method     string
	URL        string // without query string
	Status     int
	StatusText string
	RetryAfter time.Duration
	Body       string // start of the body, for diagnosis (e.g. WAF responses)
	// WAFAction is AWS WAF's X-Amzn-Waf-Action header (e.g. "challenge",
	// "captcha"). WAF challenges can arrive with a 2xx status; the app never
	// tries to solve them, it backs off.
	WAFAction string
}

func (e *httpStatusError) Error() string {
	if e.WAFAction != "" {
		return fmt.Sprintf("%s %s: blocked by Goodreads' firewall (WAF %s, status %s)", e.Method, e.URL, e.WAFAction, e.StatusText)
	}
	return fmt.Sprintf("%s %s: unexpected status %s", e.Method, e.URL, e.StatusText)
}

// Classify sorts an error from a request into permanent, transient or auth.
func Classify(err error) ErrClass {
	var se *httpStatusError
	if errors.As(err, &se) {
		switch {
		case se.WAFAction != "":
			return ErrTransient
		case se.Status == http.StatusUnauthorized:
			return ErrAuth
		case se.Status == http.StatusTooManyRequests, se.Status >= 500:
			return ErrTransient
		case se.Status == http.StatusForbidden:
			// A WAF block (AppSync answers WAFForbiddenException) or a
			// CloudFront block: back off and try again.
			return ErrTransient
		default:
			return ErrPermanent
		}
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) && !isTimeout(err) {
		return ErrPermanent
	}
	var netErr net.Error
	var urlErr *url.Error
	if errors.As(err, &netErr) || errors.As(err, &urlErr) {
		return ErrTransient
	}
	return ErrPermanent
}

// isTimeout reports a per-request timeout (as opposed to the caller's context
// ending), which is worth retrying.
func isTimeout(err error) bool {
	var netErr net.Error
	return errors.As(err, &netErr) && netErr.Timeout()
}

// parseRetryAfter reads a Retry-After header (seconds or an HTTP date),
// capped at maxRetryAfter. Unparseable or past values give 0.
func parseRetryAfter(v string, now time.Time) time.Duration {
	v = strings.TrimSpace(v)
	if v == "" {
		return 0
	}
	var d time.Duration
	if secs, err := strconv.Atoi(v); err == nil {
		d = time.Duration(secs) * time.Second
	} else if t, err := http.ParseTime(v); err == nil {
		d = t.Sub(now)
	}
	if d <= 0 {
		return 0
	}
	if d > maxRetryAfter {
		return maxRetryAfter
	}
	return d
}

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

// withRetry runs attempt until it succeeds, fails non-transiently, or the
// retries in retryBackoff are used up. The wait before a retry is the
// server's Retry-After if it sent one, else the next backoff step.
func withRetry(ctx context.Context, sleep func(context.Context, time.Duration) error, what string, attempt func() error) error {
	if sleep == nil {
		sleep = sleepCtx
	}
	for i := 0; ; i++ {
		err := attempt()
		if err == nil {
			return nil
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if Classify(err) != ErrTransient || i >= len(retryBackoff) {
			return err
		}
		wait := retryBackoff[i]
		var se *httpStatusError
		if errors.As(err, &se) && se.RetryAfter > 0 {
			wait = se.RetryAfter
		}
		log.Printf("%s: %v; retry %d/%d in %s", what, err, i+1, len(retryBackoff), wait)
		if err := sleep(ctx, wait); err != nil {
			return err
		}
	}
}
