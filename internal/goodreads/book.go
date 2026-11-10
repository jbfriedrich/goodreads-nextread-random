package goodreads

import (
	"html"
	"net/url"
	"regexp"
	"strings"
)

// Book is a single entry from a Goodreads shelf.
type Book struct {
	// ReviewID identifies the shelf entry (Goodreads' "review" of the book on
	// this user's shelf). The widget always carries it, so it is the stable key.
	ReviewID      string
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

// BookURL returns the canonical Goodreads book page URL for this book. When the
// book id is not known yet (e.g. a widget entry without a cover that has not
// been enriched), it falls back to a Goodreads search for title and author.
func (b Book) URL() string {
	if b.BookID == "" {
		return "https://www.goodreads.com/search?q=" + url.QueryEscape(strings.TrimSpace(b.Title+" "+b.Author))
	}
	return "https://www.goodreads.com/book/show/" + b.BookID
}

var tagRe = regexp.MustCompile(`<[^>]*>`)
var wsRe = regexp.MustCompile(`\s+`)

// CleanDescription strips HTML tags, unescapes entities, collapses whitespace,
// and truncates to maxLen characters (appending "..." when truncated).
func CleanDescription(desc string, maxLen int) string {
	text := tagRe.ReplaceAllString(desc, " ")
	text = html.UnescapeString(text)
	text = strings.TrimSpace(wsRe.ReplaceAllString(text, " "))

	if maxLen > 0 {
		if runes := []rune(text); len(runes) > maxLen {
			text = strings.TrimSpace(string(runes[:maxLen])) + "..."
		}
	}
	return text
}
