package goodreads

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"goodreads-nextread/internal/goodreads/goodreadstest"
)

func TestParseBookPage(t *testing.T) {
	got, err := parseBookPage(readFixture(t, "book_page.html"), "111111")
	if err != nil {
		t.Fatalf("parseBookPage: %v", err)
	}
	want := Book{
		BookID:        "111111",
		Description:   "<b>Gears</b> &amp; apples, ​ripening.",
		AverageRating: "4.50",
		NumPages:      "412",
		Published:     "2015", // the work's original publication, not this edition's
		ISBN:          "9781538784747",
		ImageURL:      "https://m.media-amazon.com/images/S/compressed.photo.goodreads.com/books/1781766398i/111111.jpg",
	}
	if got != want {
		t.Errorf("parseBookPage =\n %+v\nwant\n %+v", got, want)
	}
}

func TestBookDetailsErrorNamesURLAndPageTitle(t *testing.T) {
	// A 200 page that is not a book page (seen once in production) must be
	// diagnosable from the log line alone.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "<html><head><title>Something odd</title></head><body>nope</body></html>")
	}))
	defer srv.Close()
	c := NewClient(Options{HTTP: srv.Client(), BaseURL: srv.URL, Sleep: goodreadstest.NoSleep})

	_, err := c.BookDetails(context.Background(), "42")
	if err == nil {
		t.Fatal("expected an error for a page without book data")
	}
	for _, want := range []string{"/book/show/42", `"Something odd"`, "__NEXT_DATA__"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %s", err, want)
		}
	}
	if IsNotFound(err) {
		t.Error("an unparseable page must not count as not found")
	}
}

func TestParseBookPageWithoutEmbeddedData(t *testing.T) {
	if _, err := parseBookPage([]byte("<html><body>Book not found</body></html>"), "1"); err == nil {
		t.Fatal("expected an error for a page without __NEXT_DATA__")
	}
}

func TestParseBookPageFallsBackToEditionDataAndISBN10(t *testing.T) {
	page := goodreadstest.BookPageHTML("5", goodreadstest.BookData{Pages: 0, EditionTime: 1791874800000, ISBN10: "1538784742", Rating: 0}, goodreadstest.APIKey)
	got, err := parseBookPage([]byte(page), "5")
	if err != nil {
		t.Fatal(err)
	}
	if got.Published != "2026" || got.ISBN != "1538784742" {
		t.Errorf("got %+v, want edition year 2026 and the ISBN-10", got)
	}
	if got.NumPages != "" || got.AverageRating != "" {
		t.Errorf("zero pages/rating should be empty, got %+v", got)
	}
}

func TestParseAutoCompletePicksMatchingTitleAndAuthor(t *testing.T) {
	id, cover, err := parseAutoComplete(readFixture(t, "auto_complete.json"), "The Cartographer's Daughter", "Cy Meridian")
	if err != nil {
		t.Fatal(err)
	}
	if id != "333333" {
		t.Errorf("bookId = %q, want 333333 (not the first, unrelated result)", id)
	}
	if cover != "https://i.gr-assets.com/images/S/compressed.photo.goodreads.com/books/1762486430i/333333.jpg" {
		t.Errorf("cover = %q", cover)
	}
}

func TestParseAutoCompleteNoMatch(t *testing.T) {
	if _, _, err := parseAutoComplete(readFixture(t, "auto_complete.json"), "Unrelated", "Nobody"); err == nil {
		t.Fatal("expected an error when no result matches title or author")
	}
	if _, _, err := parseAutoComplete([]byte("[]"), "X", "Y"); err == nil {
		t.Fatal("expected an error for an empty result list")
	}
}

func TestParseBookPageSoftNotFound(t *testing.T) {
	_, err := parseBookPage([]byte(goodreadstest.SoftNotFoundPage("220513519")), "220513519")
	if !errors.Is(err, ErrBookNotFound) || !IsNotFound(err) {
		t.Errorf("err = %v, want ErrBookNotFound (a removed book's soft-404 page)", err)
	}
	// A page without the embedded data at all is a markup change, not a
	// missing book.
	if _, err := parseBookPage([]byte("<html></html>"), "1"); errors.Is(err, ErrBookNotFound) || IsNotFound(err) {
		t.Errorf("err = %v; a page without __NEXT_DATA__ must not count as not found", err)
	}
}
