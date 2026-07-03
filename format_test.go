package main

import (
	"strings"
	"testing"
)

func TestFormatBookIncludesCoreFields(t *testing.T) {
	b := Book{
		Title:         "The Magical Cheese Emporium (Spellshop, #4)",
		Author:        "Sarah Beth Durst",
		AverageRating: "3.98",
		NumPages:      "336",
		Published:     "2026",
		BookID:        "250727343",
		Description:   "A tale of cheese.",
	}
	out := formatBook(b)

	for _, want := range []string{
		"The Magical Cheese Emporium (Spellshop, #4)",
		"by Sarah Beth Durst",
		"3.98",
		"336",
		"2026",
		"https://www.goodreads.com/book/show/250727343",
		"A tale of cheese.",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("formatted output missing %q\n---\n%s", want, out)
		}
	}
}

func TestFormatBookOmitsAbsentFields(t *testing.T) {
	b := Book{Title: "Untitled", Author: "Someone", BookID: "1"}
	out := formatBook(b)

	if strings.Contains(out, "Pages:") {
		t.Errorf("expected no Pages line when pages absent\n%s", out)
	}
	if strings.Contains(out, "Published:") {
		t.Errorf("expected no Published line when year absent\n%s", out)
	}
	if strings.Contains(out, "Rating:") {
		t.Errorf("expected no Rating line when rating absent\n%s", out)
	}
	// link is always present
	if !strings.Contains(out, "https://www.goodreads.com/book/show/1") {
		t.Errorf("expected book link\n%s", out)
	}
}

func TestFormatBookTreatsZeroRatingAsAbsent(t *testing.T) {
	b := Book{Title: "Unrated", Author: "Nobody", BookID: "1", AverageRating: "0.0"}
	if out := formatBook(b); strings.Contains(out, "Rating:") {
		t.Errorf("expected no Rating line for a 0.0 (unrated) book\n%s", out)
	}
}
