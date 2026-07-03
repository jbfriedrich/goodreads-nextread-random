package main

import (
	"encoding/xml"
	"fmt"
	"html"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Book is a single entry from a Goodreads shelf.
type Book struct {
	Title         string
	Author        string
	AverageRating string
	NumPages      string
	Published     string
	ISBN          string
	Description   string
	BookID        string
	ImageURL      string
}

// BookURL returns the canonical Goodreads book page URL for this book,
// built from its BookID rather than the RSS review link.
func (b Book) BookURL() string {
	return "https://www.goodreads.com/book/show/" + b.BookID
}

// rssFeed mirrors the parts of the Goodreads shelf RSS we care about.
type rssFeed struct {
	Items []rssItem `xml:"channel>item"`
}

type rssItem struct {
	Title         string `xml:"title"`
	Author        string `xml:"author_name"`
	AverageRating string `xml:"average_rating"`
	NumPages      string `xml:"num_pages"`
	Published     string `xml:"book_published"`
	ISBN          string `xml:"isbn"`
	Description   string `xml:"book_description"`
	BookID        string `xml:"book_id"`
	ImageURL      string `xml:"book_large_image_url"`
}

// fetchShelf retrieves every book on the shelf identified by rssURL, paging
// through the feed until a page returns no items.
func fetchShelf(rssURL string) ([]Book, error) {
	client := &http.Client{Timeout: 30 * time.Second}

	var books []Book
	for page := 1; ; page++ {
		pageURL, err := withPage(rssURL, page)
		if err != nil {
			return nil, err
		}

		body, err := getBody(client, pageURL)
		if err != nil {
			return nil, err
		}

		pageBooks, err := parseItems(body)
		if err != nil {
			return nil, fmt.Errorf("parsing shelf page %d: %w", page, err)
		}
		if len(pageBooks) == 0 {
			break
		}
		books = append(books, pageBooks...)
	}

	return books, nil
}

// withPage sets the page query parameter on a URL.
func withPage(rawURL string, page int) (string, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return "", fmt.Errorf("invalid shelf URL %q: %w", rawURL, err)
	}
	q := u.Query()
	q.Set("page", strconv.Itoa(page))
	u.RawQuery = q.Encode()
	return u.String(), nil
}

// getBody performs an HTTP GET with a browser-like User-Agent and returns the
// response body, failing on non-2xx status codes.
func getBody(client *http.Client, rawURL string) ([]byte, error) {
	req, err := http.NewRequest(http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (goodreads-nextread)")

	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetching shelf feed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("fetching shelf feed: unexpected status %s", resp.Status)
	}

	return io.ReadAll(resp.Body)
}

// parseItems decodes a shelf RSS page into Books.
func parseItems(data []byte) ([]Book, error) {
	var feed rssFeed
	if err := xml.Unmarshal(data, &feed); err != nil {
		return nil, err
	}

	books := make([]Book, 0, len(feed.Items))
	for _, it := range feed.Items {
		books = append(books, Book{
			Title:         strings.TrimSpace(it.Title),
			Author:        strings.TrimSpace(it.Author),
			AverageRating: strings.TrimSpace(it.AverageRating),
			NumPages:      strings.TrimSpace(it.NumPages),
			Published:     strings.TrimSpace(it.Published),
			ISBN:          strings.TrimSpace(it.ISBN),
			Description:   strings.TrimSpace(it.Description),
			BookID:        strings.TrimSpace(it.BookID),
			ImageURL:      strings.TrimSpace(it.ImageURL),
		})
	}
	return books, nil
}

var tagRe = regexp.MustCompile(`<[^>]*>`)
var wsRe = regexp.MustCompile(`\s+`)

// cleanDescription strips HTML tags, unescapes entities, collapses whitespace,
// and truncates to maxLen characters (appending "..." when truncated).
func cleanDescription(desc string, maxLen int) string {
	text := tagRe.ReplaceAllString(desc, " ")
	text = html.UnescapeString(text)
	text = strings.TrimSpace(wsRe.ReplaceAllString(text, " "))

	if maxLen > 0 && len(text) > maxLen {
		text = strings.TrimSpace(text[:maxLen]) + "..."
	}
	return text
}
