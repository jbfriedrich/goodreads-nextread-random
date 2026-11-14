package main

import (
	"testing"
	"time"

	"goodreads-nextread/internal/config"
)

func TestClientOptionsUseConfiguredTimeoutAndDelay(t *testing.T) {
	cfg := &config.Config{BaseURL: "https://example.test", UserID: "1", Shelf: "to-read", HTTPTimeout: 7 * time.Second}
	o := clientOptions(cfg, 3*time.Second)
	if o.HTTP == nil || o.HTTP.Timeout != 7*time.Second {
		t.Errorf("HTTP client = %+v, want the configured 7s timeout", o.HTTP)
	}
	if o.Delay != 3*time.Second || o.BaseURL != "https://example.test" || o.UserID != "1" || o.Shelf != "to-read" {
		t.Errorf("options = %+v", o)
	}
}
