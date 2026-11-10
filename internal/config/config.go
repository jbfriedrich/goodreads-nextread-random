package config

import (
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
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
)

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

	// RefreshInterval is the pre-widget setting; it is rejected with a pointer
	// to its replacement rather than silently ignored.
	RefreshInterval string `yaml:"refresh_interval"`
}

// Load reads and validates config.yaml from path.
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading config file %q: %w", path, err)
	}

	var cf configFile
	if err := yaml.Unmarshal(data, &cf); err != nil {
		return nil, fmt.Errorf("parsing config file %q: %w", path, err)
	}
	bad := func(format string, args ...any) error {
		return fmt.Errorf("config file %q: %s", path, fmt.Sprintf(format, args...))
	}

	if strings.TrimSpace(cf.ListURL) == "" {
		return nil, bad("missing required field %q", "list_url")
	}
	if cf.RefreshInterval != "" {
		return nil, bad("refresh_interval is no longer supported; set scrapes_per_day (how many times a day the shelf is synced, default %d) instead", defaultScrapesPerDay)
	}

	cfg := &Config{
		ScrapesPerDay:   defaultScrapesPerDay,
		DBPath:          defaultDBPath,
		RequestDelay:    defaultRequestDelay,
		EnrichDelay:     defaultEnrichDelay,
		MaxRequests:     defaultMaxRequests,
		GraphQLEndpoint: goodreads.DefaultGraphQLEndpoint,
	}

	if cfg.BaseURL, cfg.UserID, cfg.Shelf, err = parseListURL(cf.ListURL); err != nil {
		return nil, bad("%v", err)
	}

	if cf.ScrapesPerDay != nil {
		cfg.ScrapesPerDay = *cf.ScrapesPerDay
	}
	if cfg.ScrapesPerDay < 1 || cfg.ScrapesPerDay > MaxScrapesPerDay {
		return nil, bad("scrapes_per_day must be between 1 and %d, got %d", MaxScrapesPerDay, cfg.ScrapesPerDay)
	}

	if p := strings.TrimSpace(cf.DBPath); p != "" {
		cfg.DBPath = p
	}
	if !filepath.IsAbs(cfg.DBPath) {
		cfg.DBPath = filepath.Join(filepath.Dir(path), cfg.DBPath)
	}

	if cfg.RequestDelay, err = parseDelay(cf.RequestDelay, defaultRequestDelay); err != nil {
		return nil, bad("invalid request_delay: %v", err)
	}
	if cfg.EnrichDelay, err = parseDelay(cf.EnrichDelay, defaultEnrichDelay); err != nil {
		return nil, bad("invalid enrich_delay: %v", err)
	}

	if cf.MaxRequests != nil {
		cfg.MaxRequests = *cf.MaxRequests
	}
	if cfg.MaxRequests < goodreads.MinMaxRequests {
		return nil, bad("max_requests must be at least %d, got %d", goodreads.MinMaxRequests, cfg.MaxRequests)
	}

	if ep := strings.TrimSpace(cf.GraphQL); ep != "" {
		u, err := url.Parse(ep)
		if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
			return nil, bad("graphql_endpoint %q is not an http(s) URL", ep)
		}
		cfg.GraphQLEndpoint = ep
	}

	return cfg, nil
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
