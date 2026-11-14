package goodreads

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// ErrBookNotFound means Goodreads says the book does not exist. Removed books
// get a "soft 404": HTTP 200, "Page not found", and page data whose lookup for
// the id is null.
var ErrBookNotFound = errors.New("book not found on Goodreads")

// IsNotFound reports a 404/410 response or a soft-404 book page.
func IsNotFound(err error) bool {
	if errors.Is(err, ErrBookNotFound) {
		return true
	}
	var se *httpStatusError
	return errors.As(err, &se) && (se.Status == http.StatusNotFound || se.Status == http.StatusGone)
}

// LookupBookID finds a book id (and cover) for a widget entry that has none,
// via Goodreads' public autocomplete search.
func (c *Client) LookupBookID(ctx context.Context, title, author string) (bookID, cover string, err error) {
	body, _, err := c.get(ctx, "/book/auto_complete", url.Values{
		"format": {"json"},
		"q":      {strings.TrimSpace(title + " " + author)},
	})
	if err != nil {
		return "", "", fmt.Errorf("searching for book id: %w", err)
	}
	return parseAutoComplete(body, title, author)
}

// BookDetails fetches and parses the public book page.
func (c *Client) BookDetails(ctx context.Context, bookID string) (Book, error) {
	body, final, err := c.get(ctx, "/book/show/"+url.PathEscape(bookID), nil)
	if err != nil {
		return Book{}, fmt.Errorf("fetching book page of %s: %w", bookID, err)
	}
	b, err := parseBookPage(body, bookID)
	if err != nil && !IsNotFound(err) {
		return Book{}, pageError(final, body, err)
	}
	return b, err
}

type autoCompleteResult struct {
	BookID        string `json:"bookId"`
	Title         string `json:"title"`
	BookTitleBare string `json:"bookTitleBare"`
	ImageURL      string `json:"imageUrl"`
	Author        struct {
		Name string `json:"name"`
	} `json:"author"`
}

// parseAutoComplete picks the result whose title matches (preferring one by
// the same author). Results with a different title are never taken: a wrong
// match would show the wrong book.
func parseAutoComplete(data []byte, title, author string) (bookID, cover string, err error) {
	var results []autoCompleteResult
	if err := json.Unmarshal(data, &results); err != nil {
		return "", "", fmt.Errorf("parsing autocomplete response: %w", err)
	}

	var best *autoCompleteResult
	for i := range results {
		r := &results[i]
		if r.BookID == "" || !(sameText(r.Title, title) || sameText(r.BookTitleBare, title)) {
			continue
		}
		if sameText(r.Author.Name, author) {
			best = r
			break
		}
		if best == nil {
			best = r
		}
	}
	if best == nil {
		return "", "", fmt.Errorf("no Goodreads search result matches %q", title)
	}
	return best.BookID, normalizeCoverURL(best.ImageURL), nil
}

var textNormalizer = strings.NewReplacer("’", "'", "‘", "'", "“", `"`, "”", `"`)

// sameText compares titles/names ignoring case, whitespace runs and curly quotes.
func sameText(a, b string) bool {
	norm := func(s string) string {
		return strings.Join(strings.Fields(textNormalizer.Replace(s)), " ")
	}
	return strings.EqualFold(norm(a), norm(b))
}

// flexString accepts a JSON string or number.
type flexString string

func (f *flexString) UnmarshalJSON(b []byte) error {
	if s, err := strconv.Unquote(string(b)); err == nil {
		*f = flexString(s)
		return nil
	}
	*f = flexString(bytes.TrimSpace(b))
	return nil
}

type apolloRef struct {
	Ref string `json:"__ref"`
}

type apolloBook struct {
	LegacyID    flexString `json:"legacyId"`
	Description string     `json:"description"`
	ImageURL    string     `json:"imageUrl"`
	Details     struct {
		NumPages        int     `json:"numPages"`
		PublicationTime float64 `json:"publicationTime"`
		ISBN13          string  `json:"isbn13"`
		ISBN            string  `json:"isbn"`
	} `json:"details"`
	Work *apolloRef `json:"work"`
}

type apolloWork struct {
	Details struct {
		PublicationTime float64 `json:"publicationTime"`
	} `json:"details"`
	Stats struct {
		AverageRating float64 `json:"averageRating"`
	} `json:"stats"`
}

// bookFields are the raw details both sources (GraphQL and the book page's
// embedded data) provide, before they are turned into a Book.
type bookFields struct {
	Description, ImageURL, ISBN13, ISBN string
	NumPages                            int
	EditionTime, WorkTime               float64 // ms since epoch
	AverageRating                       float64
}

// book maps fields to a Book: the work's original publication year beats the
// edition's, ISBN-13 beats ISBN-10, placeholder covers and zero values are
// dropped. The description stays HTML; CleanDescription renders it.
func (f bookFields) book(bookID string) Book {
	out := Book{
		BookID:      bookID,
		Description: strings.TrimSpace(f.Description),
		ISBN:        strings.TrimSpace(f.ISBN13),
	}
	if out.ISBN == "" {
		out.ISBN = strings.TrimSpace(f.ISBN)
	}
	if f.NumPages > 0 {
		out.NumPages = strconv.Itoa(f.NumPages)
	}
	if !isPlaceholderCover(f.ImageURL) {
		out.ImageURL = strings.TrimSpace(f.ImageURL)
	}
	if f.AverageRating > 0 {
		out.AverageRating = strconv.FormatFloat(f.AverageRating, 'f', 2, 64)
	}
	published := f.EditionTime
	if f.WorkTime != 0 {
		published = f.WorkTime
	}
	if published != 0 {
		out.Published = strconv.Itoa(time.UnixMilli(int64(published)).UTC().Year())
	}
	return out
}

var (
	nextDataStart = []byte(`<script id="__NEXT_DATA__" type="application/json">`)
	scriptEnd     = []byte(`</script>`)
)

// nextData returns the JSON in a page's __NEXT_DATA__ script.
func nextData(page []byte) ([]byte, error) {
	i := bytes.Index(page, nextDataStart)
	if i < 0 {
		return nil, errors.New("book page has no __NEXT_DATA__; has the page changed?")
	}
	rest := page[i+len(nextDataStart):]
	j := bytes.Index(rest, scriptEnd)
	if j < 0 {
		return nil, errors.New("book page __NEXT_DATA__ is not terminated")
	}
	return rest[:j], nil
}

// parseBookPage extracts book details from the Apollo state that the book page
// embeds in its __NEXT_DATA__ script.
func parseBookPage(page []byte, bookID string) (Book, error) {
	data, err := nextData(page)
	if err != nil {
		return Book{}, err
	}
	var next struct {
		Props struct {
			PageProps struct {
				ApolloState map[string]json.RawMessage `json:"apolloState"`
			} `json:"pageProps"`
		} `json:"props"`
	}
	if err := json.Unmarshal(data, &next); err != nil {
		return Book{}, fmt.Errorf("parsing book page data: %w", err)
	}
	state := next.Props.PageProps.ApolloState

	book, err := findApolloBook(state, bookID)
	if err != nil {
		return Book{}, err
	}
	f := bookFields{
		Description: book.Description,
		ImageURL:    book.ImageURL,
		ISBN13:      book.Details.ISBN13,
		ISBN:        book.Details.ISBN,
		NumPages:    book.Details.NumPages,
		EditionTime: book.Details.PublicationTime,
	}
	if book.Work != nil {
		var work apolloWork
		if raw, ok := state[book.Work.Ref]; ok && json.Unmarshal(raw, &work) == nil {
			f.AverageRating = work.Stats.AverageRating
			f.WorkTime = work.Details.PublicationTime
		}
	}
	return f.book(bookID), nil
}

// findApolloBook locates the page's main book: via the ROOT_QUERY lookup by
// legacy id, falling back to scanning every Book entry.
func findApolloBook(state map[string]json.RawMessage, bookID string) (apolloBook, error) {
	var root map[string]json.RawMessage
	lookedUp := false // the page looked the id up (as opposed to markup we do not understand)
	if raw, ok := state["ROOT_QUERY"]; ok && json.Unmarshal(raw, &root) == nil {
		for key, raw := range root {
			if !strings.HasPrefix(key, "getBookByLegacyId(") || !strings.Contains(key, `"`+bookID+`"`) {
				continue
			}
			lookedUp = true
			if string(bytes.TrimSpace(raw)) == "null" {
				return apolloBook{}, fmt.Errorf("book %s: %w", bookID, ErrBookNotFound)
			}
			var ref apolloRef
			if json.Unmarshal(raw, &ref) != nil {
				continue
			}
			var book apolloBook
			if bookRaw, ok := state[ref.Ref]; ok && json.Unmarshal(bookRaw, &book) == nil {
				return book, nil
			}
		}
	}
	for key, raw := range state {
		if !strings.HasPrefix(key, "Book:") {
			continue
		}
		var book apolloBook
		if json.Unmarshal(raw, &book) == nil && string(book.LegacyID) == bookID {
			return book, nil
		}
	}
	if lookedUp {
		return apolloBook{}, fmt.Errorf("book %s: %w", bookID, ErrBookNotFound)
	}
	return apolloBook{}, fmt.Errorf("book %s not found in the book page data; has the page changed?", bookID)
}
