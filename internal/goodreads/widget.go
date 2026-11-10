package goodreads

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/PuerkitoBio/goquery"
)

// Goodreads' public shelf widget (/review/custom_widget/<uid>) is the only
// login-free source of shelf contents since the RSS feeds were retired in
// October 2026. It is a JavaScript snippet that assigns the widget's HTML to
// a single-quoted string variable; we extract and parse that HTML.

const widgetCodeMarker = "var widget_code = '"

var (
	reviewIDRe    = regexp.MustCompile(`/review/show/(\d+)`)
	coverBookIDRe = regexp.MustCompile(`/books/\d+[a-z]?/(\d+)[._]`)
	coverSizeRe   = regexp.MustCompile(`\._[A-Za-z0-9_,]+_(\.[A-Za-z]+)$`)
	shelfCountRe  = regexp.MustCompile(`\(([\d,.]+)\)`)
)

// parseWidget decodes one custom_widget response into Books. Only the fields
// the widget carries are set: ReviewID, Title, Author, and — when the book has
// a real cover — ImageURL and BookID.
func parseWidget(data []byte) ([]Book, error) {
	i := bytes.Index(data, []byte(widgetCodeMarker))
	if i < 0 {
		return nil, errors.New("response is not a Goodreads widget script (widget_code not found)")
	}
	markup, err := jsSingleQuoted(data[i+len(widgetCodeMarker):])
	if err != nil {
		return nil, fmt.Errorf("decoding widget script: %w", err)
	}

	doc, err := goquery.NewDocumentFromReader(strings.NewReader(markup))
	if err != nil {
		return nil, fmt.Errorf("parsing widget markup: %w", err)
	}

	var books []Book
	var parseErr error
	doc.Find(`div[class^="gr_custom_each_container_"]`).EachWithBreak(func(_ int, entry *goquery.Selection) bool {
		titleLink := entry.Find(`div[class^="gr_custom_title_"] a`).First()
		href, _ := titleLink.Attr("href")
		m := reviewIDRe.FindStringSubmatch(href)
		if m == nil {
			parseErr = fmt.Errorf("widget entry without a review link (href %q); has the widget markup changed?", href)
			return false
		}

		cover, _ := entry.Find(`div[class^="gr_custom_book_container_"] img`).First().Attr("src")
		b := Book{
			ReviewID: m[1],
			Title:    collapseSpace(titleLink.Text()),
			Author:   collapseSpace(entry.Find(`div[class^="gr_custom_author_"] a`).First().Text()),
			ImageURL: normalizeCoverURL(cover),
		}
		if b.ImageURL != "" {
			if id := coverBookIDRe.FindStringSubmatch(cover); id != nil {
				b.BookID = id[1]
			}
		}
		books = append(books, b)
		return true
	})
	if parseErr != nil {
		return nil, parseErr
	}
	return books, nil
}

// collapseSpace trims s and reduces internal whitespace runs to one space.
// Goodreads pads author names that have an empty middle name ("Rebecca   Ross").
func collapseSpace(s string) string { return strings.Join(strings.Fields(s), " ") }

// jsSingleQuoted decodes a JavaScript single-quoted string literal whose
// opening quote has already been consumed, stopping at the closing quote.
func jsSingleQuoted(src []byte) (string, error) {
	var sb strings.Builder
	for i := 0; i < len(src); {
		c := src[i]
		switch {
		case c == '\'':
			return sb.String(), nil
		case c == '\\':
			if i+1 >= len(src) {
				return "", errors.New("unterminated escape sequence")
			}
			e := src[i+1]
			i += 2
			switch e {
			case 'n':
				sb.WriteByte('\n')
			case 't':
				sb.WriteByte('\t')
			case 'r':
				sb.WriteByte('\r')
			case 'u':
				if i+4 > len(src) {
					return "", errors.New("truncated \\u escape")
				}
				r, err := strconv.ParseUint(string(src[i:i+4]), 16, 32)
				if err != nil {
					return "", fmt.Errorf("invalid \\u escape: %w", err)
				}
				sb.WriteRune(rune(r))
				i += 4
			default: // \" \' \/ \\ and any other char escape themselves
				sb.WriteByte(e)
			}
		default:
			_, size := utf8.DecodeRune(src[i:])
			sb.Write(src[i : i+size])
			i += size
		}
	}
	return "", errors.New("unterminated string literal")
}

// normalizeCoverURL turns a widget thumbnail URL into the full-size cover by
// dropping Goodreads' size suffix (e.g. "._SY75_"). The "nophoto" placeholder
// is not a real cover and yields "".
func normalizeCoverURL(src string) string {
	src = strings.TrimSpace(src)
	if isPlaceholderCover(src) {
		return ""
	}
	return coverSizeRe.ReplaceAllString(src, "$1")
}

// isPlaceholderCover reports whether u is empty or Goodreads' "no cover" image.
func isPlaceholderCover(u string) bool {
	u = strings.TrimSpace(u)
	return u == "" || strings.Contains(u, "nophoto") || strings.Contains(u, "no-cover") || strings.Contains(u, "no_cover")
}

// parseShelfCount reads the number of books on shelf from a public profile
// page (/user/show/<uid>), which lists each shelf as a link like
// "to-read (636)".
func parseShelfCount(page []byte, shelf string) (int, error) {
	doc, err := goquery.NewDocumentFromReader(bytes.NewReader(page))
	if err != nil {
		return 0, fmt.Errorf("parsing profile page: %w", err)
	}

	count := -1
	doc.Find(`a[href*="/review/list/"]`).EachWithBreak(func(_ int, a *goquery.Selection) bool {
		href, _ := a.Attr("href")
		u, err := url.Parse(href)
		if err != nil || u.Query().Get("shelf") != shelf {
			return true
		}
		m := shelfCountRe.FindStringSubmatch(a.Text())
		if m == nil {
			return true
		}
		n, err := strconv.Atoi(strings.NewReplacer(",", "", ".", "").Replace(m[1]))
		if err != nil {
			return true
		}
		count = n
		return false
	})

	if count < 0 {
		return 0, fmt.Errorf("shelf %q with a book count not found on the profile page; is the profile public?", shelf)
	}
	return count, nil
}

// MinMaxRequests is the smallest request budget a harvest can work with:
// profile count, one widget call and the final recount.
const MinMaxRequests = 3

// fixedOrders are the deterministic widget orderings a harvest walks before
// falling back to random samples. Each returns the first 100 books of the
// shelf in that order; together they covered 590 of a 636-book shelf in a
// test on 2026-11-09 ("position" added nothing and is left out). Newest-first
// comes first because it is also what the quick check looks at.
var fixedOrders = [][2]string{
	{"date_added", "d"}, {"date_added", "a"},
	{"title", "a"}, {"title", "d"},
	{"author", "a"}, {"author", "d"},
	{"date_pub", "a"}, {"date_pub", "d"},
	{"avg_rating", "a"}, {"avg_rating", "d"},
	{"num_pages", "a"}, {"num_pages", "d"},
	{"num_ratings", "a"}, {"num_ratings", "d"},
	{"isbn", "a"}, {"isbn", "d"},
}

// HarvestResult is the outcome of collecting a whole shelf.
type HarvestResult struct {
	Books    []Book // distinct entries, by review id
	Expected int    // shelf size from the profile, read again at the end
	Complete bool   // len(Books) == Expected
	Requests int
}

// harvest collects the whole shelf through the 100-book widget: it walks the
// fixed orders, then random samples, until it has as many distinct entries as
// the profile reports or maxRequests is used up. The profile count is read
// again at the end so a shelf edited mid-harvest is not mistaken for complete.
//
// If onBatch is set, it is called after each widget call with the entries that
// call newly found (the first batch is the newest 100); an error from it
// aborts the harvest.
func (c *Client) Harvest(ctx context.Context, maxRequests int, onBatch func([]Book) error) (res HarvestResult, err error) {
	if maxRequests < MinMaxRequests {
		maxRequests = MinMaxRequests
	}
	start := c.RequestCount()
	used := func() int { return c.RequestCount() - start }
	defer func() { res.Requests = used() }()

	expected, err := c.ShelfCount(ctx)
	if err != nil {
		return res, err
	}

	seen := make(map[string]bool)
	// Keep one request in reserve for the final recount.
	for i := 0; used() < maxRequests-1; i++ {
		if i > 0 && len(res.Books) >= expected {
			break
		}
		sortKey, order := "random", ""
		if i < len(fixedOrders) {
			sortKey, order = fixedOrders[i][0], fixedOrders[i][1]
		}

		books, err := c.widget(ctx, sortKey, order)
		if err != nil {
			return res, err
		}
		if len(books) == 0 && expected > 0 {
			return res, fmt.Errorf("shelf widget returned no books although the profile reports %d; has the widget changed?", expected)
		}
		var fresh []Book
		for _, b := range books {
			if !seen[b.ReviewID] {
				seen[b.ReviewID] = true
				fresh = append(fresh, b)
			}
		}
		res.Books = append(res.Books, fresh...)
		if onBatch != nil && len(fresh) > 0 {
			if err := onBatch(fresh); err != nil {
				return res, err
			}
		}
	}

	if res.Expected, err = c.ShelfCount(ctx); err != nil {
		return res, err
	}
	res.Complete = len(res.Books) == res.Expected
	return res, nil
}

// QuickCheck returns the profile's shelf count and the 100 most recently added
// entries — enough to tell whether the shelf changed since the last harvest.
func (c *Client) QuickCheck(ctx context.Context) (expected int, newest []Book, err error) {
	if expected, err = c.ShelfCount(ctx); err != nil {
		return 0, nil, err
	}
	if newest, err = c.widget(ctx, "date_added", "d"); err != nil {
		return 0, nil, err
	}
	return expected, newest, nil
}
