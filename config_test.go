package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestDeriveRSSURL(t *testing.T) {
	tests := []struct {
		name    string
		listURL string
		want    string
		wantErr bool
	}{
		{
			name:    "standard shelf url",
			listURL: "https://www.goodreads.com/review/list/12345678?shelf=to-read",
			want:    "https://www.goodreads.com/review/list_rss/12345678?shelf=to-read",
		},
		{
			name:    "already an rss url is left intact",
			listURL: "https://www.goodreads.com/review/list_rss/12345678?shelf=to-read",
			want:    "https://www.goodreads.com/review/list_rss/12345678?shelf=to-read",
		},
		{
			name:    "no query string",
			listURL: "https://www.goodreads.com/review/list/12345678",
			want:    "https://www.goodreads.com/review/list_rss/12345678",
		},
		{
			name:    "not a goodreads shelf url",
			listURL: "https://example.com/some/page",
			wantErr: true,
		},
		{
			name:    "empty",
			listURL: "",
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := deriveRSSURL(tt.listURL)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("expected error, got nil (result %q)", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tt.want {
				t.Errorf("deriveRSSURL(%q) = %q, want %q", tt.listURL, got, tt.want)
			}
		})
	}
}

func TestLoadConfig(t *testing.T) {
	dir := t.TempDir()

	t.Run("valid config", func(t *testing.T) {
		path := filepath.Join(dir, "ok.yaml")
		writeFile(t, path, "list_url: \"https://www.goodreads.com/review/list/12345678?shelf=to-read\"\n")

		cfg, err := loadConfig(path)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if cfg.RSSURL != "https://www.goodreads.com/review/list_rss/12345678?shelf=to-read" {
			t.Errorf("RSSURL = %q", cfg.RSSURL)
		}
	})

	t.Run("missing list_url", func(t *testing.T) {
		path := filepath.Join(dir, "empty.yaml")
		writeFile(t, path, "other: value\n")

		if _, err := loadConfig(path); err == nil {
			t.Fatal("expected error for missing list_url, got nil")
		}
	})

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
