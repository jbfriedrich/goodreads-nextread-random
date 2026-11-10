package goodreads

import (
	"os"
	"testing"
)

func readFixture(t *testing.T, name string) []byte {
	t.Helper()
	data, err := os.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatalf("reading fixture %s: %v", name, err)
	}
	return data
}

func TestParseWidgetExtractsEntries(t *testing.T) {
	books, err := parseWidget(readFixture(t, "widget.js"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(books) != 3 {
		t.Fatalf("got %d books, want 3", len(books))
	}

	first := books[0]
	if first.ReviewID != "9000000001" {
		t.Errorf("ReviewID = %q", first.ReviewID)
	}
	if first.Title != "The Clockwork Orchard" {
		t.Errorf("Title = %q", first.Title)
	}
	if first.Author != "Ada Quill" {
		t.Errorf("Author = %q", first.Author)
	}
	if first.BookID != "111111" {
		t.Errorf("BookID = %q, want it taken from the cover URL", first.BookID)
	}
	// The thumbnail size suffix is stripped to get the full-size cover.
	wantCover := "https://i.gr-assets.com/images/S/compressed.photo.goodreads.com/books/1781766398l/111111.jpg"
	if first.ImageURL != wantCover {
		t.Errorf("ImageURL = %q, want %q", first.ImageURL, wantCover)
	}
}

func TestParseWidgetUnescapesEntitiesAndQuotes(t *testing.T) {
	books, err := parseWidget(readFixture(t, "widget.js"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := books[1].Title; got != "Salt & Static (Tidewater, #2)" {
		t.Errorf("Title = %q", got)
	}
	if got := books[2].Title; got != "The Cartographer's Daughter" {
		t.Errorf("Title = %q", got)
	}
}

func TestParseWidgetPlaceholderCoverHasNoImageOrBookID(t *testing.T) {
	books, err := parseWidget(readFixture(t, "widget.js"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	b := books[2]
	if b.ImageURL != "" {
		t.Errorf("ImageURL = %q, want empty for the nophoto placeholder", b.ImageURL)
	}
	if b.BookID != "" {
		t.Errorf("BookID = %q, want empty (placeholder carries no id)", b.BookID)
	}
	if b.ReviewID != "9000000003" {
		t.Errorf("ReviewID = %q", b.ReviewID)
	}
}

func TestParseWidgetRejectsNonWidgetResponse(t *testing.T) {
	if _, err := parseWidget([]byte("<html>Page not found</html>")); err == nil {
		t.Fatal("expected an error for a response that is not a widget script")
	}
}

func TestParseWidgetEmptyShelf(t *testing.T) {
	js := `  var widget_code = '  <div class=\"gr_custom_container_e\">\n  <\/div>\n'
  document.write(widget_code)`
	books, err := parseWidget([]byte(js))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(books) != 0 {
		t.Errorf("got %d books, want 0", len(books))
	}
}

func TestParseShelfCount(t *testing.T) {
	page := readFixture(t, "profile.html")
	tests := []struct {
		shelf string
		want  int
	}{
		{"to-read", 636},
		{"read", 1204}, // thousands separator
		{"currently-reading", 3},
		{"to-read-2027", 12}, // must not be confused with "to-read"
	}
	for _, tt := range tests {
		got, err := parseShelfCount(page, tt.shelf)
		if err != nil {
			t.Errorf("parseShelfCount(%q): unexpected error: %v", tt.shelf, err)
			continue
		}
		if got != tt.want {
			t.Errorf("parseShelfCount(%q) = %d, want %d", tt.shelf, got, tt.want)
		}
	}
}

func TestParseShelfCountMissingShelf(t *testing.T) {
	if _, err := parseShelfCount(readFixture(t, "profile.html"), "favourites"); err == nil {
		t.Fatal("expected an error when the shelf is not listed on the profile")
	}
}

func TestNormalizeCoverURL(t *testing.T) {
	tests := []struct{ in, want string }{
		{"https://i.gr-assets.com/books/1l/1._SY75_.jpg", "https://i.gr-assets.com/books/1l/1.jpg"},
		{"https://i.gr-assets.com/books/1l/1._SX50_SY75_.jpg", "https://i.gr-assets.com/books/1l/1.jpg"},
		{"https://i.gr-assets.com/books/1l/1.jpg", "https://i.gr-assets.com/books/1l/1.jpg"},
		{"https://s.gr-assets.com/assets/nophoto/book/50x75-abc.png", ""},
		{"", ""},
	}
	for _, tt := range tests {
		if got := normalizeCoverURL(tt.in); got != tt.want {
			t.Errorf("normalizeCoverURL(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}
