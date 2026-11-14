# Decisions

Why goodreads-nextread is built the way [architecture.md](architecture.md) describes. Oldest
first. The design specs in [superpowers/specs/](superpowers/specs/) hold more background for
the 2026-07 and 2026-11 entries.

## Requests never wait for Goodreads (2026-07)
The web server keeps the shelf in memory and syncs in the background; a page request only
picks from memory. A cold fetch from Goodreads took about 20 s, and a visitor should never
wait for it or see it fail. The cost is that changes on Goodreads appear only after the next
sync.

## The shelf comes from the public widget, without a login (2026-11-09)
Goodreads retired its RSS feeds in October 2026 (the shelf feed answered `404`), and the HTML
shelf page redirects to the sign-in page. Rejected:
- A logged-in scrape with a session cookie copied from a browser: no lifetime is published for
  the Amazon sign-in cookies, signing in automatically means captchas and one-time codes, and
  the operator would have to paste a new cookie whenever it expires.
- The CSV export Goodreads recommends: manual.
- The grid widget (`/review/grid_widget`): 200 entries per request, but it silently leaves out
  books without a cover; a harvest stalled at 635 of 636.

The custom widget is public, returns 100 entries per request in any sort order (including
random), and needs no account. The shelf's owner has to keep the profile and shelf public.

## Completeness is proven with the profile count (2026-11-09)
The widget cannot page past 100 entries, so a harvest collects entries through 16 fixed sort
orders and random samples. The public profile page shows the exact shelf size, and books are
only deleted when the harvest found exactly that many. Measured on a 636-book shelf: the fixed
orders found 590, random samples the rest; 40–45 requests in total. A partial harvest may add
but never deletes, so a short harvest can never shrink the stored shelf.

## SQLite with a pure-Go driver (2026-11-09)
The shelf and the sync log live in one SQLite file (`modernc.org/sqlite`). The pure-Go driver
keeps `CGO_ENABLED=0` builds and the static binary in the Alpine image working. One open
connection serialises the sync and enrichment writers.

## Book details come from Goodreads' GraphQL API, book pages are the fallback (2026-11-09)
A book page is about 640 KB and is never compressed (gzip, br, zstd and deflate were all
tried), so the first fill of 636 books took about 3.5 h at a polite pace and about 400 MB.
The book pages load their data from an AppSync GraphQL API with a public key shipped in every
page. Measured: one book 1.2 KB, 25 aliased lookups 19.8 KB (about 800 B per book); the first
fill takes 26 requests and about a minute. 50 lookups in one request (11.7 KB body) were
blocked with `403 WAFForbiddenException`, while a single request right after went through,
which matches AWS WAF's 8 KB body rule; batches are therefore 25 books with a body cap of 6 KB.
The API is undocumented, so anything except a transient error falls back to the book pages.
The GraphQL API has no query that lists a user's shelf; the widget remains the shelf source.

## A 404 is final, everything else is retried in place (2026-11-10)
An earlier version deferred every failed book by 24 h, so a short outage delayed every book it
touched by a day. Now a missing book is never retried, and a `429`, `5xx`, firewall `403` or
network error is retried in the same request after `Retry-After` or a growing wait. A harvest
keeps its progress through a throttled request instead of starting over.

## A quick check before every harvest; a weekly safety net (2026-11-10)
A full harvest costs about 40 requests. Two requests (profile count, newest 100 entries)
settle most syncs: unchanged, or only additions, which are inserted directly. Removals still
need a full harvest. The quick check cannot see every change (see
[architecture.md](architecture.md#safety-net-harvest-and-the-quick-checks-blind-spots)), so a
full harvest also runs once per `NEXTREAD_FULL_HARVEST_EVERY` (default a week).

## The web server serves as soon as the first books are stored (2026-11-10)
With an empty store the server used to harvest the whole shelf before listening (about a
minute), and nginx answered `502` in the meantime. The first harvest now saves each batch as it
arrives, and the server listens after the newest 100 books, a few seconds in.

## `/healthz` returns only `ok` (2026-11-10)
nginx forwards `/healthz` publicly. With sync details it exposed the shelf size, the sync
schedule and raw errors containing the reader's Goodreads user id. Status goes to the log.

## A stale book id is recovered by title and author (2026-11-10)
The book id in the widget comes from the cover's file name, which keeps the old id after
Goodreads merges editions (2 of 636 books). GraphQL answers such ids with `RESOURCE_NOT_FOUND`
and the book page is a "soft 404". The enricher looks the book up once through autocomplete
and keeps the current id; a later sync must not overwrite it, so the widget's book id only
fills entries that are not enriched yet. Without a match, the id is cleared and the book links
to a Goodreads search instead of a dead page.

## Packages by responsibility (2026-11-10)
The code moved from one `main` package in the repo root to `cmd/` and `internal/` packages
([architecture.md](architecture.md#components)). Only the `goodreads` package talks to
Goodreads, and it has no database access, so the HTTP logic can be tested against the fake
server in `internal/goodreads/goodreadstest`.

## Syncs name changed books; WAF challenges count as transient (2026-11-14)
A removed book appeared in the log only as `removed=1` and had to be recovered from the SQLite
file afterwards; it had moved to the `currently-reading` shelf. A transient enrichment failure
in production logged only "book page has no __NEXT_DATA__". Syncs now name added and removed
books, errors name the URL and the page title, and a response with an `x-amzn-waf-action`
header counts as a transient block even with a `2xx` status. The app never tries to pass a
challenge.

## Configuration only through environment variables (2026-11-14)
A `config.yaml` baked into the image meant a rebuild for every setting change and put the
shelf URL into the image. All settings are `NEXTREAD_*` environment variables; the image sets
`NEXTREAD_DB_PATH` so the database stays on the data volume. Rejected: keeping the file as an
optional second source, which leaves two ways to configure the same thing.
