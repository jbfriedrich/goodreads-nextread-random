// Package config reads the app's settings from NEXTREAD_* environment
// variables.
package config

import (
	"fmt"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	"goodreads-nextread/internal/goodreads"
)

// EnvPrefix starts every setting's environment variable, e.g.
// NEXTREAD_LIST_URL. serve's command-line flags win over the environment.
const EnvPrefix = "NEXTREAD_"

// Defaults for the optional settings.
const (
	defaultScrapesPerDay = 4
	defaultDBPath        = "data/nextread.db"
	defaultRequestDelay  = 1500 * time.Millisecond
	defaultEnrichDelay   = 2 * time.Second
	defaultMaxRequests   = 80
	MaxScrapesPerDay     = 24 * 60

	defaultHTTPTimeout      = 30 * time.Second
	defaultFullHarvestEvery = 7 * 24 * time.Hour
	defaultSyncRetryAfter   = time.Hour
	defaultEnrichErrorPause = 5 * time.Minute
)

// Config holds the runtime configuration.
type Config struct {
	// BaseURL, UserID and Shelf identify the shelf, parsed from
	// NEXTREAD_LIST_URL.
	BaseURL string
	UserID  string
	Shelf   string

	// ScrapesPerDay is how often the shelf is checked against Goodreads.
	ScrapesPerDay int
	// DBPath is the SQLite database holding the shelf (relative paths are
	// relative to the working directory).
	DBPath string

	// RequestDelay is the minimum gap between sync requests.
	RequestDelay time.Duration
	// EnrichDelay is the gap between enrichment requests.
	EnrichDelay time.Duration
	// MaxRequests caps the requests one full harvest may make.
	MaxRequests int
	// GraphQLEndpoint is Goodreads' (undocumented) GraphQL API used for
	// enrichment.
	GraphQLEndpoint string

	// HTTPTimeout bounds each request to Goodreads and Open Library.
	HTTPTimeout time.Duration
	// FullHarvestEvery is how often a full harvest runs even when quick checks
	// see no change (the safety net).
	FullHarvestEvery time.Duration
	// SyncRetryAfter caps the wait before retrying a failed sync.
	SyncRetryAfter time.Duration
	// EnrichErrorPause is how long enrichment pauses after an error whose
	// retries ran out.
	EnrichErrorPause time.Duration
}

// SyncInterval is the time between scheduled syncs.
func (c *Config) SyncInterval() time.Duration {
	return 24 * time.Hour / time.Duration(c.ScrapesPerDay)
}

// Load reads and validates the settings from the environment. Only
// NEXTREAD_LIST_URL is required; an empty variable counts as unset.
func Load() (*Config, error) {
	bad := func(env, format string, args ...any) error {
		return fmt.Errorf("environment variable %s: %s", env, fmt.Sprintf(format, args...))
	}

	cfg := &Config{DBPath: defaultDBPath, GraphQLEndpoint: goodreads.DefaultGraphQLEndpoint}

	listURL, env := lookup("list_url")
	if listURL == "" {
		return nil, fmt.Errorf("%s is not set; set it to the shelf URL from your browser, e.g. %s=\"https://www.goodreads.com/review/list/12345678?shelf=to-read\"", env, env)
	}
	var err error
	if cfg.BaseURL, cfg.UserID, cfg.Shelf, err = parseListURL(listURL); err != nil {
		return nil, bad(env, "%v", err)
	}

	if cfg.ScrapesPerDay, err = intSetting("scrapes_per_day", defaultScrapesPerDay, 1, MaxScrapesPerDay, bad); err != nil {
		return nil, err
	}
	if cfg.MaxRequests, err = intSetting("max_requests", defaultMaxRequests, goodreads.MinMaxRequests, 0, bad); err != nil {
		return nil, err
	}

	for _, d := range []struct {
		key      string
		fallback time.Duration
		positive bool // zero is not allowed
		dst      *time.Duration
	}{
		{"request_delay", defaultRequestDelay, false, &cfg.RequestDelay},
		{"enrich_delay", defaultEnrichDelay, false, &cfg.EnrichDelay},
		{"http_timeout", defaultHTTPTimeout, true, &cfg.HTTPTimeout},
		{"full_harvest_every", defaultFullHarvestEvery, true, &cfg.FullHarvestEvery},
		{"sync_retry_after", defaultSyncRetryAfter, true, &cfg.SyncRetryAfter},
		{"enrich_error_pause", defaultEnrichErrorPause, true, &cfg.EnrichErrorPause},
	} {
		raw, env := lookup(d.key)
		v, err := parseDelay(raw, d.fallback)
		if err == nil && d.positive && v == 0 {
			err = fmt.Errorf("must be greater than zero")
		}
		if err != nil {
			return nil, bad(env, "invalid duration %q (a Go duration such as 90s, 15m or 168h): %v", raw, err)
		}
		*d.dst = v
	}

	if p, _ := lookup("db_path"); p != "" {
		cfg.DBPath = p
	}

	if ep, env := lookup("graphql_endpoint"); ep != "" {
		u, err := url.Parse(ep)
		if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
			return nil, bad(env, "%q is not an http(s) URL", ep)
		}
		cfg.GraphQLEndpoint = ep
	}

	return cfg, nil
}

// lookup returns the trimmed value of NEXTREAD_<KEY> and the variable's name.
func lookup(key string) (value, env string) {
	env = EnvPrefix + strings.ToUpper(key)
	return strings.TrimSpace(os.Getenv(env)), env
}

// intSetting reads an integer setting within [min, max] (max 0 means
// unbounded).
func intSetting(key string, fallback, min, max int, bad func(string, string, ...any) error) (int, error) {
	raw, env := lookup(key)
	if raw == "" {
		return fallback, nil
	}
	n, err := strconv.Atoi(raw)
	if err != nil {
		return 0, bad(env, "%q is not a whole number", raw)
	}
	if n < min || (max > 0 && n > max) {
		if max > 0 {
			return 0, bad(env, "must be between %d and %d, got %d", min, max, n)
		}
		return 0, bad(env, "must be at least %d, got %d", min, n)
	}
	return n, nil
}

// parseDelay parses an optional non-negative Go duration.
func parseDelay(raw string, fallback time.Duration) (time.Duration, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return fallback, nil
	}
	d, err := time.ParseDuration(raw)
	if err != nil {
		return 0, err
	}
	if d < 0 {
		return 0, fmt.Errorf("must not be negative, got %q", raw)
	}
	return d, nil
}

var listPathRe = regexp.MustCompile(`^/review/list(?:_rss)?/(\d+)(?:-[^/]*)?/?$`)

// parseListURL extracts the site, user id and shelf from a browser shelf URL
// such as https://www.goodreads.com/review/list/12345678?shelf=to-read.
func parseListURL(listURL string) (base, userID, shelf string, err error) {
	listURL = strings.TrimSpace(listURL)
	if listURL == "" {
		return "", "", "", fmt.Errorf("the shelf URL is empty")
	}
	u, err := url.Parse(listURL)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return "", "", "", fmt.Errorf("%q is not a valid URL", listURL)
	}
	m := listPathRe.FindStringSubmatch(u.Path)
	if m == nil {
		return "", "", "", fmt.Errorf("%q does not look like a Goodreads shelf URL (expected .../review/list/<user id>?shelf=<shelf>)", listURL)
	}
	shelf = strings.TrimSpace(u.Query().Get("shelf"))
	if shelf == "" {
		return "", "", "", fmt.Errorf("%q has no ?shelf=… parameter; add the shelf to pick from, e.g. ?shelf=to-read", listURL)
	}
	return u.Scheme + "://" + u.Host, m[1], shelf, nil
}
