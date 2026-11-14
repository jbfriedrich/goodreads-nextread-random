package config

import (
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"goodreads-nextread/internal/goodreads"

	"gopkg.in/yaml.v3"
)

// Defaults for optional config.yaml fields.
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

// EnvPrefix starts every environment variable that overrides a config.yaml
// setting: the key in upper case, e.g. NEXTREAD_REQUEST_DELAY for
// request_delay. The environment wins over the file (which is baked into the
// Docker image); serve's command-line flags win over both.
const EnvPrefix = "NEXTREAD_"

// Config holds the runtime configuration derived from config.yaml.
type Config struct {
	// BaseURL, UserID and Shelf identify the shelf, parsed from list_url.
	BaseURL string
	UserID  string
	Shelf   string

	// ScrapesPerDay is how often the shelf is checked against Goodreads.
	ScrapesPerDay int
	// DBPath is the SQLite database holding the shelf (absolute, or resolved
	// relative to the config file's directory).
	DBPath string

	// RequestDelay is the minimum gap between scrape requests.
	RequestDelay time.Duration
	// EnrichDelay is the gap between book-page lookups for enrichment.
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

// configFile is the on-disk shape of config.yaml.
type configFile struct {
	ListURL       string `yaml:"list_url"`
	ScrapesPerDay *int   `yaml:"scrapes_per_day"`
	DBPath        string `yaml:"db_path"`
	RequestDelay  string `yaml:"request_delay"`
	EnrichDelay   string `yaml:"enrich_delay"`
	MaxRequests   *int   `yaml:"max_requests"`
	GraphQL       string `yaml:"graphql_endpoint"`

	HTTPTimeout      string `yaml:"http_timeout"`
	FullHarvestEvery string `yaml:"full_harvest_every"`
	SyncRetryAfter   string `yaml:"sync_retry_after"`
	EnrichErrorPause string `yaml:"enrich_error_pause"`

	// RefreshInterval is the pre-widget setting; it is rejected with a pointer
	// to its replacement rather than silently ignored.
	RefreshInterval string `yaml:"refresh_interval"`
}

// Load reads and validates config.yaml from path, applying NEXTREAD_*
// environment overrides.
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading config file %q: %w", path, err)
	}

	var cf configFile
	if err := yaml.Unmarshal(data, &cf); err != nil {
		return nil, fmt.Errorf("parsing config file %q: %w", path, err)
	}
	// bad reports an invalid value, naming where it came from: a key in the
	// file or an environment variable.
	bad := func(from, format string, args ...any) error {
		msg := fmt.Sprintf(format, args...)
		if strings.HasPrefix(from, EnvPrefix) {
			return fmt.Errorf("environment variable %s: %s", from, msg)
		}
		return fmt.Errorf("config file %q: %s: %s", path, from, msg)
	}

	if cf.RefreshInterval != "" {
		return nil, bad("refresh_interval", "no longer supported; set scrapes_per_day (how many times a day the shelf is synced, default %d) instead", defaultScrapesPerDay)
	}

	cfg := &Config{
		DBPath:          defaultDBPath,
		GraphQLEndpoint: goodreads.DefaultGraphQLEndpoint,
	}

	listURL, from := pick("list_url", cf.ListURL)
	if strings.TrimSpace(listURL) == "" {
		return nil, fmt.Errorf("config file %q is missing required field %q (or set %sLIST_URL)", path, "list_url", EnvPrefix)
	}
	if cfg.BaseURL, cfg.UserID, cfg.Shelf, err = parseListURL(listURL); err != nil {
		return nil, bad(from, "%v", err)
	}

	if cfg.ScrapesPerDay, err = intSetting("scrapes_per_day", cf.ScrapesPerDay, defaultScrapesPerDay, 1, MaxScrapesPerDay, bad); err != nil {
		return nil, err
	}
	if cfg.MaxRequests, err = intSetting("max_requests", cf.MaxRequests, defaultMaxRequests, goodreads.MinMaxRequests, 0, bad); err != nil {
		return nil, err
	}

	for _, d := range []struct {
		key      string
		file     string
		fallback time.Duration
		positive bool // zero is not allowed
		dst      *time.Duration
	}{
		{"request_delay", cf.RequestDelay, defaultRequestDelay, false, &cfg.RequestDelay},
		{"enrich_delay", cf.EnrichDelay, defaultEnrichDelay, false, &cfg.EnrichDelay},
		{"http_timeout", cf.HTTPTimeout, defaultHTTPTimeout, true, &cfg.HTTPTimeout},
		{"full_harvest_every", cf.FullHarvestEvery, defaultFullHarvestEvery, true, &cfg.FullHarvestEvery},
		{"sync_retry_after", cf.SyncRetryAfter, defaultSyncRetryAfter, true, &cfg.SyncRetryAfter},
		{"enrich_error_pause", cf.EnrichErrorPause, defaultEnrichErrorPause, true, &cfg.EnrichErrorPause},
	} {
		raw, from := pick(d.key, d.file)
		v, err := parseDelay(raw, d.fallback)
		if err == nil && d.positive && v == 0 {
			err = fmt.Errorf("must be greater than zero")
		}
		if err != nil {
			return nil, bad(from, "invalid duration %q (a Go duration such as 90s, 15m or 168h): %v", raw, err)
		}
		*d.dst = v
	}

	if p := strings.TrimSpace(cf.DBPath); p != "" {
		cfg.DBPath = p
	}
	if !filepath.IsAbs(cfg.DBPath) {
		cfg.DBPath = filepath.Join(filepath.Dir(path), cfg.DBPath)
	}

	if ep := strings.TrimSpace(cf.GraphQL); ep != "" {
		u, err := url.Parse(ep)
		if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
			return nil, bad("graphql_endpoint", "%q is not an http(s) URL", ep)
		}
		cfg.GraphQLEndpoint = ep
	}

	return cfg, nil
}

// pick returns the environment override for key (NEXTREAD_<KEY>) when it is
// set and not empty, else the file's value, together with where it came from.
func pick(key, fileValue string) (value, from string) {
	env := EnvPrefix + strings.ToUpper(key)
	if v := strings.TrimSpace(os.Getenv(env)); v != "" {
		return v, env
	}
	return fileValue, key
}

// intSetting resolves an integer setting from the environment or the file,
// within [min, max] (max 0 means unbounded).
func intSetting(key string, file *int, fallback, min, max int, bad func(string, string, ...any) error) (int, error) {
	raw := ""
	if file != nil {
		raw = strconv.Itoa(*file)
	}
	raw, from := pick(key, raw)
	if raw == "" {
		return fallback, nil
	}
	n, err := strconv.Atoi(strings.TrimSpace(raw))
	if err != nil {
		return 0, bad(from, "%q is not a whole number", raw)
	}
	if n < min || (max > 0 && n > max) {
		if max > 0 {
			return 0, bad(from, "must be between %d and %d, got %d", min, max, n)
		}
		return 0, bad(from, "must be at least %d, got %d", min, n)
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
		return "", "", "", fmt.Errorf("list_url is empty")
	}
	u, err := url.Parse(listURL)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return "", "", "", fmt.Errorf("list_url %q is not a valid URL", listURL)
	}
	m := listPathRe.FindStringSubmatch(u.Path)
	if m == nil {
		return "", "", "", fmt.Errorf("%q does not look like a Goodreads shelf URL (expected .../review/list/<user id>?shelf=<shelf>)", listURL)
	}
	shelf = strings.TrimSpace(u.Query().Get("shelf"))
	if shelf == "" {
		return "", "", "", fmt.Errorf("list_url %q has no ?shelf=… parameter; add the shelf to pick from, e.g. ?shelf=to-read", listURL)
	}
	return u.Scheme + "://" + u.Host, m[1], shelf, nil
}
