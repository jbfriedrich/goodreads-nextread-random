# Goodreads NextRead — Widget sync into SQLite

**Date:** 2026-11-09
**Status:** Implemented. A design record from 2026-11-09, kept as written; the current design
is in [architecture.md](../../architecture.md), the reasons in [decisions.md](../../decisions.md).
**Supersedes:** the RSS data source in
[2026-07-03-goodreads-nextread-design.md](2026-07-03-goodreads-nextread-design.md)

## Why

Goodreads discontinued its RSS feeds in October 2026 and points users to the
manual CSV export. Checked on 2026-11-09:

- `/review/list_rss/<uid>?shelf=to-read` returns **404**.
- `/review/list/<uid>?shelf=to-read` (the HTML shelf) **redirects to `/user/sign_in`**.

A logged-in scrape using a pasted session cookie was considered and rejected.
Nobody has published how long a Goodreads/Amazon SSO session lasts. Signing in
automatically means Amazon's captcha and OTP, which the app must not try to get
past. And the reader is not the operator, so every expired cookie would mean the
reader asking the operator to paste a new one. The CSV export is manual for the
same reason.

## What is still public (measured 2026-11-09, plain GETs, no cookies)

| Endpoint | Finding |
|---|---|
| `/review/custom_widget/<uid>?shelf=S&num_books=100&sort=K&order=a\|d&widget_id=W` | 200. JavaScript that assigns HTML to `var widget_code = '…'`. **At most 100 entries**, and `page` is ignored. `sort`/`order` work, including `sort=random`. Each entry has the review id, title, author and a thumbnail cover (`…/books/<ts>l/<book_id>._SY75_.jpg`, or a `nophoto` placeholder with no book id). Output is cached per `widget_id`, so every call needs a fresh one. |
| `/user/show/<uid>` | 301 to `/user/show/<uid>-<name>`, then 200. Lists each shelf with its exact count, e.g. `to-read (636)`. |
| `/book/show/<book_id>` | 200, about 640 KB. `__NEXT_DATA__` → `apolloState` has the Book (description, imageUrl, `details.numPages`/`publicationTime`/`isbn13`/`isbn`) and the Work (`stats.averageRating`, `details.publicationTime` = original publication). |
| `/book/auto_complete?format=json&q=<title author>` | 200, about 1 KB JSON with `bookId`, `title`, `imageUrl`, `author.name`. |
| thumbnail URL with `._SY75_` removed | 200, the full-size cover (45 KB instead of the thumbnail). |

**Coverage of a 636-book shelf.** The 16 fixed orders (`date_added`, `title`,
`author`, `date_pub`, `avg_rating`, `num_pages`, `num_ratings`, `isbn`, each
`a` and `d`) reached 590 books. Random samples took that to 636 after **45
requests** in total, about 7.7 MB. `position` added nothing. The real
implementation's first harvest took 40 requests in 61 s.

**GraphQL (measured the same day).** The book page gets its data from AppSync
GraphQL (`kxbwmqov6jgg3daaamb744ycu4.appsync-api.us-east-1.amazonaws.com`) with the
key every page ships in `__NEXT_DATA__.props.pageProps.apiKey`, sent as
`x-api-key`.
- **One book:** `getBookByLegacyId(legacyId: Int!)` returns 1.2 KB gzip.
- **25 aliased lookups in one request:** 19.8 KB, about 790 B per book, 0.6 s.
- **50 aliased lookups:** a 403 `WAFForbiddenException`. The query body was
  11.7 KB, consistent with AWS WAF's 8 KB body rule, and a single query
  straight afterwards still worked.
- **Book pages** are 641 KB and never compressed, whatever `Accept-Encoding` asks
  for.
- **No shelf listing.** None of the GraphQL operations in the site's bundles lists
  a user's shelf contents, so the shelf harvest stays on the widget.
- **Grid widget rejected.** `/review/grid_widget` returns 200 entries per call
  (about 10 KB), but silently drops books without a cover, so a harvest stalled
  at 635/636. It can't prove completeness.

## Design

Code (since the 2026-11-10 restructure): `internal/goodreads` (client, widget,
harvest, book pages, GraphQL), `internal/store`, `internal/shelfsync`,
`internal/enrich`, `internal/web`, `internal/config`, and the entry point in
`cmd/goodreads-nextread`.

- **Quick check (2 requests).** Profile count, plus the widget sorted
  `date_added desc`. Additions always appear among the newest, re-adds included.
  - Count == stored and every newest entry known: nothing changed.
  - Count == stored + the unknown newest entries, with fewer than all 100 unknown:
    only additions, inserted directly with no harvest.
  - Anything else (a removal, a swap, a window full of new books): full harvest.
- **Full harvest.** Runs when the quick check can't settle it, on an empty DB, or
  when the last complete full harvest is ≥ 7 days old. It walks the fixed orders, then random
  samples, stopping when the distinct count reaches the profile count or
  `max_requests` (default 80) is used. One request is kept for a final profile
  recount. Requests are paced 1.5 s apart, and all HTTP round trips, redirects
  included, count against the cap.
- **Reconcile** in one transaction, keyed by **review id**. Rows are upserted,
  and the widget refreshes title, author, book id and cover only when they are
  non-empty. Enrichment columns are untouched. Missing rows are deleted **only
  when the harvest is complete** (found == recount). A partial harvest only
  adds, and a failed one writes nothing except the run log.
- **Enrichment.** Batches of 25 go through GraphQL, `enrich_delay` (2 s) apart,
  so the first fill of 636 books is about 26 requests.
  - **Ids:** entries without a book id are looked up by title and author through
    autocomplete; only an exact title match is accepted.
  - **Key:** read once from a book page, cached in the `settings` table, and read
    again once on a 401.
  - **Fallback:** a GraphQL failure that waiting won't fix (key still rejected,
    validation error, unexpected response) sends that batch through the book
    pages, and GraphQL is tried again on the next batch. A per-book error also
    goes through that book's page.
  - **Stale ids:** the widget's cover file name can keep an old book id after
    Goodreads merges editions (seen with 2 of 636 books). GraphQL reports these
    as `RESOURCE_NOT_FOUND`, and the book page is a soft 404 (HTTP 200,
    "Page not found"). The book is looked up once by title and author, and once
    enriched the verified id wins over the widget's. If nothing matches, the id
    is cleared, so the link becomes a Goodreads search.
  - **Covers:** widget cover, then GraphQL or book page, then autocomplete, then
    Open Library `covers.openlibrary.org/b/isbn/<isbn>-L.jpg?default=false`
    (404 means none).
- **Errors.** One classifier covers every request.
  - **Permanent:** 404/410, other 4xx, a GraphQL `null`, no autocomplete match.
    These are never retried; the book is marked done with what it has.
  - **Transient:** network errors, 429, 5xx, 403/WAF. These are retried in place:
    `Retry-After` (capped at 15 min) or a backoff of 30 s, 1 m, 2 m, 4 m, 8 m.
    After that the sync fails (and is retried within 1 h) or enrichment pauses
    for 5 min.
  - **Auth:** 401, which triggers a GraphQL key refresh.
- **Schedule.** `NEXTREAD_SCRAPES_PER_DAY` (default 4), resumed from the `sync_runs` log
  so a restart doesn't trigger an extra scrape.
- **Serving.** An in-memory `shelfCache` loaded from SQLite, reloaded after each
  sync and each enrichment batch.
  - **Empty DB:** the initial harvest saves each widget batch as it arrives
    (add-only), and the server listens once the first 100 books are stored, a
    few seconds in.
  - **Health:** `/healthz` returns plain `ok`, because it's publicly proxied.
    Sync and enrichment status go to the logs only.
- **Storage.** `modernc.org/sqlite` (pure Go, so `CGO_ENABLED=0` builds keep
  working), at `NEXTREAD_DB_PATH` (default `data/nextread.db`; the image uses
  `/app/data/nextread.db`). Since 2026-11-14 all settings are environment
  variables; there is no config file.
  HTML parsing uses `goquery`.

## Risks

- **Goodreads could retire or change the widget.** The sync then fails and is
  logged, and the site keeps serving the stored shelf.
- **GraphQL is undocumented.** If the key rotates, it is read again from a book
  page. If the schema changes or the endpoint closes, enrichment falls back to
  book pages automatically: slower, but it keeps working.
  The cookie-based scrape is the documented fallback, but it is not built.
- **A shelf where random sampling can't converge within `max_requests`.** For
  example, several thousand books: the run is `partial`, adds what it found and
  deletes nothing. Raise `max_requests` if this shows up in the logs.
