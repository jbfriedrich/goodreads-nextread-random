package main

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

const sampleItem = `
  <item>
    <title><![CDATA[The Magical Cheese Emporium (Spellshop, #4)]]></title>
    <link><![CDATA[https://www.goodreads.com/review/show/8735748543?utm_medium=api]]></link>
    <book_id>250727343</book_id>
    <book_large_image_url><![CDATA[https://i.gr-assets.com/books/1783002977l/250727343._SY475_.jpg]]></book_large_image_url>
    <author_name><![CDATA[Sarah Beth Durst]]></author_name>
    <average_rating>3.98</average_rating>
    <num_pages>336</num_pages>
    <book_published>2026</book_published>
    <isbn>1250999999</isbn>
    <book_description><![CDATA[<b>Great book.</b><br/>A tale of <i>cheese</i> &amp; revolution.]]></book_description>
  </item>`

func rssPage(items ...string) string {
	return `<?xml version="1.0" encoding="UTF-8"?>
<rss version="2.0"><channel><title>shelf</title>` + strings.Join(items, "") + `</channel></rss>`
}

func TestParseItemsExtractsFields(t *testing.T) {
	books, err := parseItems([]byte(rssPage(sampleItem)))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(books) != 1 {
		t.Fatalf("got %d books, want 1", len(books))
	}
	b := books[0]
	if b.Title != "The Magical Cheese Emporium (Spellshop, #4)" {
		t.Errorf("Title = %q", b.Title)
	}
	if b.Author != "Sarah Beth Durst" {
		t.Errorf("Author = %q", b.Author)
	}
	if b.AverageRating != "3.98" {
		t.Errorf("AverageRating = %q", b.AverageRating)
	}
	if b.NumPages != "336" {
		t.Errorf("NumPages = %q", b.NumPages)
	}
	if b.Published != "2026" {
		t.Errorf("Published = %q", b.Published)
	}
	if b.BookID != "250727343" {
		t.Errorf("BookID = %q", b.BookID)
	}
	if b.ImageURL != "https://i.gr-assets.com/books/1783002977l/250727343._SY475_.jpg" {
		t.Errorf("ImageURL = %q", b.ImageURL)
	}
}

func TestBookURLUsesBookID(t *testing.T) {
	b := Book{BookID: "250727343"}
	want := "https://www.goodreads.com/book/show/250727343"
	if got := b.BookURL(); got != want {
		t.Errorf("BookURL() = %q, want %q", got, want)
	}
}

func TestCleanDescriptionStripsHTMLAndTruncates(t *testing.T) {
	got := cleanDescription("<b>Great book.</b><br/>A tale of <i>cheese</i> &amp; revolution.", 100)
	want := "Great book. A tale of cheese & revolution."
	if got != want {
		t.Errorf("cleanDescription = %q, want %q", got, want)
	}

	long := strings.Repeat("word ", 100)
	truncated := cleanDescription(long, 20)
	if len(truncated) > 23 { // 20 + up to "..."
		t.Errorf("truncated too long: %d chars: %q", len(truncated), truncated)
	}
	if !strings.HasSuffix(truncated, "...") {
		t.Errorf("expected ellipsis suffix, got %q", truncated)
	}
}

func TestFetchShelfPaginatesUntilEmpty(t *testing.T) {
	var gotPages []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		page := r.URL.Query().Get("page")
		gotPages = append(gotPages, page)
		switch page {
		case "1":
			// full page of 2 items (test threshold is small via itemsPerPage)
			fmt.Fprint(w, rssPage(sampleItem, sampleItem))
		default:
			fmt.Fprint(w, rssPage()) // empty page ends pagination
		}
	}))
	defer srv.Close()

	books, err := fetchShelf(srv.URL + "?shelf=to-read")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(books) != 2 {
		t.Fatalf("got %d books, want 2", len(books))
	}
	if len(gotPages) != 2 || gotPages[0] != "1" || gotPages[1] != "2" {
		t.Errorf("requested pages = %v, want [1 2]", gotPages)
	}
}

func TestFetchShelfEmptyShelf(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, rssPage())
	}))
	defer srv.Close()

	books, err := fetchShelf(srv.URL)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(books) != 0 {
		t.Errorf("got %d books, want 0", len(books))
	}
}
