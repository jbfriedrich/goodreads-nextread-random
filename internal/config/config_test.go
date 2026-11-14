package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"goodreads-nextread/internal/goodreads"
)

func TestParseListURL(t *testing.T) {
	tests := []struct {
		name                string
		listURL             string
		base, userID, shelf string
		wantErr             bool
	}{
		{
			name:    "standard shelf url",
			listURL: "https://www.goodreads.com/review/list/12345678?shelf=to-read",
			base:    "https://www.goodreads.com", userID: "12345678", shelf: "to-read",
		},
		{
			name:    "url with name slug and extra params",
			listURL: "https://www.goodreads.com/review/list/12345678-reader?shelf=favourites&sort=title",
			base:    "https://www.goodreads.com", userID: "12345678", shelf: "favourites",
		},
		{
			name:    "old rss url still accepted",
			listURL: "https://www.goodreads.com/review/list_rss/12345678?shelf=to-read",
			base:    "https://www.goodreads.com", userID: "12345678", shelf: "to-read",
		},
		{name: "missing shelf", listURL: "https://www.goodreads.com/review/list/12345678", wantErr: true},
		{name: "not a goodreads shelf url", listURL: "https://example.com/some/page?shelf=x", wantErr: true},
		{name: "empty", listURL: "", wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			base, userID, shelf, err := parseListURL(tt.listURL)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("expected error, got base=%q user=%q shelf=%q", base, userID, shelf)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if base != tt.base || userID != tt.userID || shelf != tt.shelf {
				t.Errorf("got (%q, %q, %q), want (%q, %q, %q)", base, userID, shelf, tt.base, tt.userID, tt.shelf)
			}
		})
	}
}

const validListURL = "https://www.goodreads.com/review/list/12345678?shelf=to-read"

// clearEnv unsets every NEXTREAD_ variable for the test, so the developer's
// environment cannot leak in.
func clearEnv(t *testing.T) {
	t.Helper()
	for _, kv := range os.Environ() {
		if name, _, _ := strings.Cut(kv, "="); strings.HasPrefix(name, EnvPrefix) {
			t.Setenv(name, "")
		}
	}
}

func TestLoadDefaults(t *testing.T) {
	clearEnv(t)
	t.Setenv("NEXTREAD_LIST_URL", validListURL)

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.UserID != "12345678" || cfg.Shelf != "to-read" || cfg.BaseURL != "https://www.goodreads.com" {
		t.Errorf("shelf = %q/%q at %q", cfg.UserID, cfg.Shelf, cfg.BaseURL)
	}
	checks := []struct {
		name      string
		got, want any
	}{
		{"ScrapesPerDay", cfg.ScrapesPerDay, 4},
		{"SyncInterval", cfg.SyncInterval(), 6 * time.Hour},
		{"DBPath", cfg.DBPath, filepath.Join("data", "nextread.db")},
		{"RequestDelay", cfg.RequestDelay, 1500 * time.Millisecond},
		{"EnrichDelay", cfg.EnrichDelay, 2 * time.Second},
		{"MaxRequests", cfg.MaxRequests, 80},
		{"GraphQLEndpoint", cfg.GraphQLEndpoint, goodreads.DefaultGraphQLEndpoint},
		{"HTTPTimeout", cfg.HTTPTimeout, 30 * time.Second},
		{"FullHarvestEvery", cfg.FullHarvestEvery, 7 * 24 * time.Hour},
		{"SyncRetryAfter", cfg.SyncRetryAfter, time.Hour},
		{"EnrichErrorPause", cfg.EnrichErrorPause, 5 * time.Minute},
	}
	for _, c := range checks {
		if c.got != c.want {
			t.Errorf("%s = %v, want %v", c.name, c.got, c.want)
		}
	}
}

func TestLoadFromEnvironment(t *testing.T) {
	clearEnv(t)
	t.Setenv("NEXTREAD_LIST_URL", "https://www.goodreads.com/review/list/999?shelf=favourites")
	t.Setenv("NEXTREAD_SCRAPES_PER_DAY", "12")
	t.Setenv("NEXTREAD_REQUEST_DELAY", "250ms")
	t.Setenv("NEXTREAD_ENRICH_DELAY", "5s")
	t.Setenv("NEXTREAD_MAX_REQUESTS", "120")
	t.Setenv("NEXTREAD_HTTP_TIMEOUT", "45s")
	t.Setenv("NEXTREAD_FULL_HARVEST_EVERY", "72h")
	t.Setenv("NEXTREAD_SYNC_RETRY_AFTER", "15m")
	t.Setenv("NEXTREAD_ENRICH_ERROR_PAUSE", "90s")
	t.Setenv("NEXTREAD_DB_PATH", "/app/data/nextread.db")
	t.Setenv("NEXTREAD_GRAPHQL_ENDPOINT", "https://example.appsync-api.us-east-1.amazonaws.com/graphql")

	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.UserID != "999" || cfg.Shelf != "favourites" {
		t.Errorf("shelf = %s/%s, want 999/favourites", cfg.UserID, cfg.Shelf)
	}
	checks := []struct {
		name      string
		got, want any
	}{
		{"ScrapesPerDay", cfg.ScrapesPerDay, 12},
		{"RequestDelay", cfg.RequestDelay, 250 * time.Millisecond},
		{"EnrichDelay", cfg.EnrichDelay, 5 * time.Second},
		{"MaxRequests", cfg.MaxRequests, 120},
		{"HTTPTimeout", cfg.HTTPTimeout, 45 * time.Second},
		{"FullHarvestEvery", cfg.FullHarvestEvery, 72 * time.Hour},
		{"SyncRetryAfter", cfg.SyncRetryAfter, 15 * time.Minute},
		{"EnrichErrorPause", cfg.EnrichErrorPause, 90 * time.Second},
		{"DBPath", cfg.DBPath, "/app/data/nextread.db"},
		{"GraphQLEndpoint", cfg.GraphQLEndpoint, "https://example.appsync-api.us-east-1.amazonaws.com/graphql"},
	}
	for _, c := range checks {
		if c.got != c.want {
			t.Errorf("%s = %v, want %v", c.name, c.got, c.want)
		}
	}
}

func TestLoadRequiresListURL(t *testing.T) {
	clearEnv(t)
	_, err := Load()
	if err == nil || !strings.Contains(err.Error(), "NEXTREAD_LIST_URL") {
		t.Fatalf("err = %v, want one naming NEXTREAD_LIST_URL", err)
	}
}

func TestEmptyVariableMeansDefault(t *testing.T) {
	clearEnv(t)
	t.Setenv("NEXTREAD_LIST_URL", validListURL)
	t.Setenv("NEXTREAD_REQUEST_DELAY", "  ")
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.RequestDelay != 1500*time.Millisecond {
		t.Errorf("RequestDelay = %v, want the default", cfg.RequestDelay)
	}
}

func TestInvalidValuesNameTheVariable(t *testing.T) {
	for env, value := range map[string]string{
		"NEXTREAD_LIST_URL":           "https://example.com/nope",
		"NEXTREAD_SCRAPES_PER_DAY":    "0",
		"NEXTREAD_REQUEST_DELAY":      "soon",
		"NEXTREAD_ENRICH_DELAY":       "-1s",
		"NEXTREAD_MAX_REQUESTS":       "1",
		"NEXTREAD_HTTP_TIMEOUT":       "0s",
		"NEXTREAD_FULL_HARVEST_EVERY": "7d",
		"NEXTREAD_SYNC_RETRY_AFTER":   "0",
		"NEXTREAD_ENRICH_ERROR_PAUSE": "x",
		"NEXTREAD_GRAPHQL_ENDPOINT":   "not-a-url",
	} {
		t.Run(env, func(t *testing.T) {
			clearEnv(t)
			t.Setenv("NEXTREAD_LIST_URL", validListURL)
			t.Setenv(env, value)
			_, err := Load()
			if err == nil {
				t.Fatalf("%s=%q accepted, want an error", env, value)
			}
			if !strings.Contains(err.Error(), env) {
				t.Errorf("error %q does not name %s", err, env)
			}
		})
	}
}

func TestScrapesPerDayUpperBound(t *testing.T) {
	clearEnv(t)
	t.Setenv("NEXTREAD_LIST_URL", validListURL)
	t.Setenv("NEXTREAD_SCRAPES_PER_DAY", "2000")
	if _, err := Load(); err == nil || !strings.Contains(err.Error(), "1440") {
		t.Errorf("err = %v, want the 1440 limit", err)
	}
}
