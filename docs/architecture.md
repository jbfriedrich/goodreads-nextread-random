# Architecture

goodreads-nextread keeps a local SQLite copy of one public Goodreads shelf and picks a random
book from it, on the command line or as a web page. Settings, endpoints and file locations
are listed in [inventory.md](inventory.md). The reasons behind the design are in
[decisions.md](decisions.md), and the terms are defined in [glossary.md](glossary.md).

## Components

| Package | Role |
|---|---|
| `cmd/goodreads-nextread` | Entry point: a random pick (default), `sync` and `serve`; maps the configuration onto the packages below |
| `internal/config` | Reads and validates the `NEXTREAD_*` environment variables |
| `internal/goodreads` | Everything that talks to Goodreads: a paced HTTP client with retries, the shelf widget and profile page, the harvest and quick check, book pages, autocomplete and the GraphQL client |
| `internal/store` | The SQLite database: books, the sync log and settings |
| `internal/shelfsync` | Keeps the store in line with Goodreads (quick check, full harvest, reconcile, schedule) |
| `internal/enrich` | Fills in book details in the background; Open Library covers |
| `internal/web` | The in-memory book cache, the HTTP handlers and the page template |

Dependencies point one way: `cmd` → `web` → `shelfsync`, `enrich` → `store` → `goodreads`.
`goodreads` has no database access; the GraphQL client stores its API key through the
`KeyStore` interface, which `store.Store` implements.

## Data sources

Every source is public and needs no login. The shelf's owner must keep the profile and the
shelf public.

| Source | Provides |
|---|---|
| Shelf widget (`/review/custom_widget/<user id>`) | The shelf: review id, title, author, cover; the book id is part of the cover's file name. At most 100 entries per request, in any sort order |
| Profile page (`/user/show/<user id>`) | The exact number of books on the shelf |
| GraphQL API (AppSync) | Book details: description, average rating, pages, publication years, ISBN, cover |
| Book page (`/book/show/<book id>`) | The same details (fallback), and the GraphQL API key |
| Autocomplete (`/book/auto_complete`) | Book ids found by title and author |
| Open Library covers | A cover by ISBN when Goodreads has none |

The GraphQL API cannot list a shelf, so the widget is the only source for the shelf itself.

## Shelf sync

A sync runs `NEXTREAD_SCRAPES_PER_DAY` times a day. It is either a quick check or a full
harvest, and every run is recorded in the `sync_runs` table.

### Quick check

Two requests: the profile count and the 100 most recently added entries
(`sort=date_added&order=d`). Compared with the store:

| Result | Action |
|---|---|
| Count equals the stored count, every newest entry is stored | Nothing changed (`quick sync unchanged`) |
| Count equals stored + the number of unknown newest entries, and not all of the newest 100 are unknown | Only additions: they are inserted directly (`quick sync synced`) |
| Anything else (a removal, a swap, 100 new entries) | Full harvest |

### Full harvest

A full harvest runs when the quick check cannot settle a change, when the store is empty,
when no complete full harvest is recorded yet, or when the last one is older than
`NEXTREAD_FULL_HARVEST_EVERY` (the safety net, see below).

1. Read the profile count.
2. Request the widget in 16 fixed orders (`date_added`, `title`, `author`, `date_pub`,
   `avg_rating`, `num_pages`, `num_ratings` and `isbn`, each ascending and descending), then
   `sort=random`, until the number of distinct entries reaches the profile count or
   `NEXTREAD_MAX_REQUESTS` is used up. One request is kept for step 3.
3. Read the profile count again. The harvest is **complete** when the number of distinct
   entries equals this count.

Every widget request uses a new `widget_id`, because Goodreads caches the widget per id. All
HTTP round trips, redirects included, count against the request cap.

### Reconcile

The harvest result is written in one transaction:

- New entries are inserted.
- Existing entries get the widget's current title, author and cover. The widget's book id is
  only written to entries that are not enriched yet; after enrichment the verified id wins.
- Entries missing from the result are deleted **only if the harvest is complete**. A partial
  harvest adds but never deletes; a failed harvest changes nothing.

Into an empty store, every widget response is saved as it arrives (add-only), so the web
server can serve the first 100 books within seconds of its first start.

The log names the books a sync added and removed (up to 10 per line); the first harvest into
an empty store is not listed.

### Safety-net harvest and the quick check's blind spots

The quick check only compares the shelf count and the review ids of the 100 most recently
added entries. It misses two kinds of change, and a full harvest every
`NEXTREAD_FULL_HARVEST_EVERY` catches them:

- **A removal plus an entry that appears outside the newest 100.** The count stays the same
  and the newest window looks familiar. A book moved back to the shelf from another one keeps
  its review id (observed); whether it also keeps its original "date added", and so sorts far
  down the shelf, is not verified ([todo](todo.md)).
- **Changes to entries that stay on the shelf**: a cover that appears later, a corrected
  title or author, a cover file name that carries a new book id after an edition merge.
  Enrichment runs once per book, so only a full harvest refreshes these fields.

### Schedule

The next sync is due one interval (24 h / `NEXTREAD_SCRAPES_PER_DAY`) after the last
recorded run, so a restart does not cause an extra sync. After a failed run, the wait is
capped at `NEXTREAD_SYNC_RETRY_AFTER`.

## Enrichment

The web server enriches books in the background; the CLI does not. A batch is up to 25
books without details, oldest first:

1. **Book ids.** Entries without a book id (no cover in the widget) are looked up through
   autocomplete. Only a result with the same title (ignoring case, spacing and curly quotes)
   is accepted, preferably by the same author; without one the book stays without details.
2. **GraphQL.** One request with one aliased `getBookByLegacyId` lookup per book. The request
   body stays under 6 KB, because larger bodies are blocked by the API's firewall. The API key
   comes from the `settings` table; without one, or after a `401`, it is read once from a book
   page.
3. **Book pages as fallback** for books GraphQL answers with an error of their own, and for the
   whole batch when GraphQL is unavailable (a query error, or a key that is still rejected
   after reading it again). The log says `graphql unavailable, using book pages`.
4. **Missing books.** A book GraphQL calls `RESOURCE_NOT_FOUND` (or a book page that is a
   "soft 404") is looked up once by title and author: the widget's book id can be stale
   after an edition merge. With a different id that exists, the book is enriched under that
   id. Otherwise its book id is cleared, it links to a Goodreads search, and it is never
   retried.
5. **Covers.** The widget's cover is kept. Without one: the cover from GraphQL or the book
   page, then the one from autocomplete, then Open Library by ISBN.

The publication year is the work's original publication; the edition's date is used only when
the work has none. Details are fetched once per book and kept until the entry leaves the shelf.

## Errors and retries

Every Goodreads request goes through one classifier:

| Class | Responses | Handling |
|---|---|---|
| Permanent | `404`, `410`, other `4xx`, unparseable data | Not retried in place. A missing book is final; an unparseable page pauses enrichment, and the batch is tried again after the pause |
| Transient | Network errors, timeouts, `429`, `5xx`, `403`, any response with an `x-amzn-waf-action` header (AWS WAF, also with a `2xx` status) | Retried in place after `Retry-After` (at most 15 min), otherwise after 30 s, 1 min, 2 min, 4 min, 8 min |
| Auth | `401` | GraphQL only: the API key is read again once |

When the retries are used up, a sync fails and is retried after at most
`NEXTREAD_SYNC_RETRY_AFTER`; enrichment pauses for `NEXTREAD_ENRICH_ERROR_PAUSE`. A firewall
challenge is never answered, only waited out.

## Serving

`serve` runs the HTTP server; in the container nginx forwards to it (see
[inventory.md](inventory.md#container)).

- **Requests never touch Goodreads.** The server keeps the shelf in memory, loaded from
  SQLite, and picks a random book per request. The cache reloads after every successful sync
  and every enrichment batch.
- **Start.** With a stored shelf the server listens immediately and makes no requests at
  startup. With an empty store it listens as soon as the first 100 books are stored; the rest
  of the harvest continues in the background. If not a single book can be fetched, the
  process exits and the container's restart policy tries again.
- **Failures stay invisible to the reader.** A failed sync keeps the stored shelf, and the
  page keeps serving it. `/` answers `503` only while the store is empty.
- **Health and status.** `/healthz` returns `ok` and nothing else, because it is publicly
  reachable. Status goes to the log ([operations.md](operations.md#read-the-log)).

## Storage

One SQLite file (`NEXTREAD_DB_PATH`), written with the pure-Go `modernc.org/sqlite` driver in
WAL mode. Tables: `books` (one row per shelf entry, keyed by review id), `sync_runs` (the
sync log) and `settings` (the GraphQL API key). Columns are listed in
[inventory.md](inventory.md#files-and-state).
