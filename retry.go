package main

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

// errClass says how a failed request should be handled.
type errClass int

const (
	// errPermanent: retrying will not help (404, other 4xx, unparseable data).
	errPermanent errClass = iota
	// errTransient: wait and retry the same request (network, 429, 5xx, WAF).
	errTransient
	// errAuth: credentials were rejected (401); only the GraphQL client acts
	// on it, by fetching a fresh API key.
	errAuth
)

func (c errClass) String() string {
	switch c {
	case errTransient:
		return "transient"
	case errAuth:
		return "auth"
	default:
		return "permanent"
	}
}

// retryBackoff is the wait before each retry of a transient failure when the
// server gives no Retry-After. Its length is the number of retries.
var retryBackoff = []time.Duration{30 * time.Second, time.Minute, 2 * time.Minute, 4 * time.Minute, 8 * time.Minute}

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
}

func (e *httpStatusError) Error() string {
	return fmt.Sprintf("%s %s: unexpected status %s", e.Method, e.URL, e.StatusText)
}

// classify sorts an error from a request into permanent, transient or auth.
func classify(err error) errClass {
	var se *httpStatusError
	if errors.As(err, &se) {
		switch {
		case se.Status == http.StatusUnauthorized:
			return errAuth
		case se.Status == http.StatusTooManyRequests, se.Status >= 500:
			return errTransient
		case se.Status == http.StatusForbidden:
			// A WAF block (AppSync answers WAFForbiddenException) or a
			// CloudFront block: back off and try again.
			return errTransient
		default:
			return errPermanent
		}
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) && !isTimeout(err) {
		return errPermanent
	}
	var netErr net.Error
	var urlErr *url.Error
	if errors.As(err, &netErr) || errors.As(err, &urlErr) {
		return errTransient
	}
	return errPermanent
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
		if classify(err) != errTransient || i >= len(retryBackoff) {
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
