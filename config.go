package main

import (
	"fmt"
	"os"
	"strings"

	"gopkg.in/yaml.v3"
)

// Config holds the runtime configuration derived from config.yaml.
type Config struct {
	// RSSURL is the Goodreads shelf RSS endpoint derived from list_url.
	RSSURL string
}

// configFile is the on-disk shape of config.yaml.
type configFile struct {
	ListURL string `yaml:"list_url"`
}

// loadConfig reads config.yaml from path and derives the RSS endpoint.
func loadConfig(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading config file %q: %w", path, err)
	}

	var cf configFile
	if err := yaml.Unmarshal(data, &cf); err != nil {
		return nil, fmt.Errorf("parsing config file %q: %w", path, err)
	}

	if strings.TrimSpace(cf.ListURL) == "" {
		return nil, fmt.Errorf("config file %q is missing required field %q", path, "list_url")
	}

	rssURL, err := deriveRSSURL(cf.ListURL)
	if err != nil {
		return nil, err
	}

	return &Config{RSSURL: rssURL}, nil
}

// deriveRSSURL turns a browser shelf URL (…/review/list/<id>?…) into the
// corresponding RSS endpoint (…/review/list_rss/<id>?…). A URL that is already
// an RSS endpoint is returned unchanged.
func deriveRSSURL(listURL string) (string, error) {
	listURL = strings.TrimSpace(listURL)
	if listURL == "" {
		return "", fmt.Errorf("list_url is empty")
	}

	if strings.Contains(listURL, "/review/list_rss/") {
		return listURL, nil
	}
	if strings.Contains(listURL, "/review/list/") {
		return strings.Replace(listURL, "/review/list/", "/review/list_rss/", 1), nil
	}

	return "", fmt.Errorf("%q does not look like a Goodreads shelf URL (expected a .../review/list/<id>?shelf=... URL)", listURL)
}
