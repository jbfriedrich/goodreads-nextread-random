# Goodreads NextRead — Design

**Date:** 2026-07-03
**Status:** Approved (pending spec review)

## Purpose

A small Go CLI that reads all books from a Goodreads shelf/list and prints one
random book from it, with metadata and a link to that book's Goodreads page.
The shelf URL is configurable in `config.yaml`.

## Data source

Goodreads retired its public API (2020), but shelves still expose a per-shelf
**RSS feed**, which is stable and easy to parse. Given a browser shelf URL like:

```
https://www.goodreads.com/review/list/12345678?shelf=to-read
```

the corresponding RSS endpoint is:

```
https://www.goodreads.com/review/list_rss/12345678?shelf=to-read
```

The tool derives the `list_rss` endpoint from the configured `list/` URL.

Each RSS `<item>` exposes (confirmed against a live feed):
`title`, `author_name`, `average_rating`, `num_pages`, `book_published`,
`isbn`, `book_description`, `book_id`, `book_*_image_url`, and `link`
(the review page). The **book page** is constructed as
`https://www.goodreads.com/book/show/<book_id>` — not the RSS `link`, which
points at the review, not the book.

The feed returns up to **100 items per page**; pagination uses `&page=N`
starting at 1. The tool fetches successive pages until a page returns zero
items, so sampling is fair across the entire shelf regardless of size.

## Configuration — `config.yaml`

```yaml
# The Goodreads shelf URL, copied from the browser.
list_url: "https://www.goodreads.com/review/list/12345678?shelf=to-read"
```

`list_url` is the only field and is required. The shelf URL is provided via
`config.yaml` only (no CLI override).

## Architecture

Single small Go module, three files:

- **`config.go`** — load and parse `config.yaml`; validate `list_url` is present;
  derive the `list_rss` endpoint (rewrite `/review/list/` → `/review/list_rss/`,
  preserving query string). Returns a `Config` with the RSS base URL.
- **`goodreads.go`** — fetch and parse the shelf.
  - `Book` struct: Title, Author, AverageRating, NumPages, Published, ISBN,
    Description, BookID, and a computed `BookURL()`.
  - `FetchShelf(rssURL) ([]Book, error)` — loops pages 1..N via `&page=N`,
    parses each page with stdlib `encoding/xml`, appends items, stops on an
    empty page. Sends a browser-like `User-Agent`.
- **`main.go`** — load config → `FetchShelf` → pick a random index →
  print the selected book.

Dependencies: `gopkg.in/yaml.v3` only. RSS parsing uses stdlib `encoding/xml`;
HTTP uses stdlib `net/http`.

## Output (rich)

Prints the one random book, e.g.:

```
📚 Your next read:

  The Magical Cheese Emporium (Spellshop, #4)
  by Sarah Beth Durst

  Rating:    3.98 avg
  Pages:     336
  Published: 2026
  Link:      https://www.goodreads.com/book/show/250727343

  When Eloren joined the revolution against the empire, she didn't
  expect to lose everything… (truncated to ~300 chars, HTML stripped)
```

Fields absent from the feed (e.g. blank pages/year) are omitted gracefully.
The description has HTML tags stripped and is truncated to a readable length.

## Randomness

Uses `math/rand` seeded per run (Go 1.20+ auto-seeds the global source, so no
explicit seeding needed) to pick a uniform random index over all collected
books.

## Error handling

Clear, actionable messages (non-zero exit) for:
- missing/unreadable `config.yaml` or missing `list_url`
- URL that isn't a recognizable Goodreads shelf URL
- network/HTTP failures fetching the feed
- an empty shelf (no books found)

## Testing

- `config_test.go` — RSS URL derivation from a `list/` URL; missing-field and
  malformed-URL errors.
- `goodreads_test.go` — XML parsing of a sample RSS fixture into `Book`s;
  `BookURL()` construction; HTML-strip/truncate helper. Pagination loop tested
  against an `httptest` server serving one full page then an empty page.

## Out of scope (YAGNI)

- CLI flags / URL override
- Caching, filtering, or interactive selection
- Non-RSS (HTML) scraping fallback
