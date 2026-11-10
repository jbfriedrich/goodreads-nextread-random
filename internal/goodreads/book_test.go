package goodreads

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func TestBookURLUsesBookID(t *testing.T) {
	b := Book{BookID: "250727343"}
	want := "https://www.goodreads.com/book/show/250727343"
	if got := b.URL(); got != want {
		t.Errorf("URL() = %q, want %q", got, want)
	}
}

func TestBookURLFallsBackToSearchWithoutBookID(t *testing.T) {
	b := Book{Title: "Salt & Static", Author: "Bo Lantern"}
	want := "https://www.goodreads.com/search?q=Salt+%26+Static+Bo+Lantern"
	if got := b.URL(); got != want {
		t.Errorf("URL() = %q, want %q", got, want)
	}
}

func TestCleanDescriptionStripsHTMLAndTruncates(t *testing.T) {
	got := CleanDescription("<b>Great book.</b><br/>A tale of <i>cheese</i> &amp; revolution.", 100)
	want := "Great book. A tale of cheese & revolution."
	if got != want {
		t.Errorf("CleanDescription = %q, want %q", got, want)
	}

	long := strings.Repeat("word ", 100)
	truncated := CleanDescription(long, 20)
	if len(truncated) > 23 { // 20 + up to "..."
		t.Errorf("truncated too long: %d chars: %q", len(truncated), truncated)
	}
	if !strings.HasSuffix(truncated, "...") {
		t.Errorf("expected ellipsis suffix, got %q", truncated)
	}
}

func TestCleanDescriptionTruncatesOnRuneBoundary(t *testing.T) {
	// "é" is two bytes; a byte-based cut at 3 would split it.
	got := CleanDescription("ééééé", 3)
	if !utf8.ValidString(got) {
		t.Fatalf("truncated description is not valid UTF-8: %q", got)
	}
	if got != "ééé..." {
		t.Errorf("CleanDescription = %q, want %q", got, "ééé...")
	}
}
