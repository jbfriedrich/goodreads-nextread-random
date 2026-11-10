package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
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

const validListURL = "list_url: \"https://www.goodreads.com/review/list/12345678?shelf=to-read\"\n"

func TestLoadConfig(t *testing.T) {
	dir := t.TempDir()

	t.Run("defaults", func(t *testing.T) {
		path := filepath.Join(dir, "ok.yaml")
		writeFile(t, path, validListURL)

		cfg, err := loadConfig(path)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if cfg.UserID != "12345678" || cfg.Shelf != "to-read" || cfg.BaseURL != "https://www.goodreads.com" {
			t.Errorf("shelf = %q/%q at %q", cfg.UserID, cfg.Shelf, cfg.BaseURL)
		}
		if cfg.ScrapesPerDay != 4 {
			t.Errorf("ScrapesPerDay = %d, want 4", cfg.ScrapesPerDay)
		}
		if cfg.SyncInterval() != 6*time.Hour {
			t.Errorf("SyncInterval = %v, want 6h", cfg.SyncInterval())
		}
		// A relative db_path is resolved against the config file's directory.
		if want := filepath.Join(dir, "data", "nextread.db"); cfg.DBPath != want {
			t.Errorf("DBPath = %q, want %q", cfg.DBPath, want)
		}
		if cfg.RequestDelay != 1500*time.Millisecond || cfg.EnrichDelay != 2*time.Second || cfg.MaxRequests != 80 {
			t.Errorf("tuning = %v/%v/%d", cfg.RequestDelay, cfg.EnrichDelay, cfg.MaxRequests)
		}
		if cfg.GraphQLEndpoint != defaultGraphQLEndpoint {
			t.Errorf("GraphQLEndpoint = %q, want the default", cfg.GraphQLEndpoint)
		}
	})

	t.Run("custom values", func(t *testing.T) {
		path := filepath.Join(dir, "custom.yaml")
		writeFile(t, path, validListURL+`scrapes_per_day: 8
db_path: "/var/lib/nextread/shelf.db"
request_delay: "2s"
enrich_delay: "1m"
max_requests: 120
graphql_endpoint: "https://example.appsync-api.us-east-1.amazonaws.com/graphql"
`)
		cfg, err := loadConfig(path)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if cfg.ScrapesPerDay != 8 || cfg.SyncInterval() != 3*time.Hour {
			t.Errorf("ScrapesPerDay = %d, interval %v", cfg.ScrapesPerDay, cfg.SyncInterval())
		}
		if cfg.DBPath != "/var/lib/nextread/shelf.db" {
			t.Errorf("DBPath = %q", cfg.DBPath)
		}
		if cfg.RequestDelay != 2*time.Second || cfg.EnrichDelay != time.Minute || cfg.MaxRequests != 120 {
			t.Errorf("tuning = %v/%v/%d", cfg.RequestDelay, cfg.EnrichDelay, cfg.MaxRequests)
		}
		if cfg.GraphQLEndpoint != "https://example.appsync-api.us-east-1.amazonaws.com/graphql" {
			t.Errorf("GraphQLEndpoint = %q", cfg.GraphQLEndpoint)
		}
	})

	rejects := []struct{ name, body, mention string }{
		{"missing list_url", "other: value\n", "list_url"},
		{"zero scrapes per day", validListURL + "scrapes_per_day: 0\n", "scrapes_per_day"},
		{"too many scrapes per day", validListURL + "scrapes_per_day: 2000\n", "scrapes_per_day"},
		{"invalid request delay", validListURL + "request_delay: soon\n", "request_delay"},
		{"negative enrich delay", validListURL + "enrich_delay: -1s\n", "enrich_delay"},
		{"request cap too small", validListURL + "max_requests: 2\n", "max_requests"},
		{"retired refresh_interval", validListURL + "refresh_interval: 30m\n", "scrapes_per_day"},
		{"bad graphql endpoint", validListURL + "graphql_endpoint: not-a-url\n", "graphql_endpoint"},
	}
	for _, tt := range rejects {
		t.Run(tt.name, func(t *testing.T) {
			path := filepath.Join(dir, strings.ReplaceAll(tt.name, " ", "-")+".yaml")
			writeFile(t, path, tt.body)
			_, err := loadConfig(path)
			if err == nil {
				t.Fatal("expected an error, got nil")
			}
			if !strings.Contains(err.Error(), tt.mention) {
				t.Errorf("error %q does not mention %q", err, tt.mention)
			}
		})
	}

	t.Run("file not found", func(t *testing.T) {
		if _, err := loadConfig(filepath.Join(dir, "does-not-exist.yaml")); err == nil {
			t.Fatal("expected error for missing file, got nil")
		}
	})
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("writing %s: %v", path, err)
	}
}
