# Glossary

Terms as used in this repo. Settings, endpoints and tables are in [inventory.md](inventory.md).

## Goodreads

**shelf** — one of the reader's Goodreads shelves, e.g. `to-read`; the app reads the one in
`NEXTREAD_LIST_URL`.

**shelf entry** — one book on the shelf. Goodreads calls it a "review", even without a written
review. (not: book, when the entry is meant)

**review id** — the id of a shelf entry. It stays the same when the entry moves to another
shelf, so it is the key of the `books` table.

**book id** — Goodreads' id of a book edition (the number in `/book/show/<book id>`); the
GraphQL API calls it `legacyId`. One book id can appear on many readers' shelves.

**stale book id** — a book id taken from a cover's file name that no longer exists, because
Goodreads merged the edition into another one.

**profile count** — the number of books on the shelf as the public profile page shows it,
e.g. "to-read (636)".

**widget** — the custom shelf widget (`/review/custom_widget`), a public JavaScript snippet
meant for blogs; the source of shelf entries. (not: grid widget, which is not used)

**soft 404** — a book page that answers HTTP 200 but says "Page not found"; Goodreads serves it
for removed books.

## Sync

**sync** — one scheduled comparison of the store with Goodreads: a quick check or a full
harvest. Recorded as a **sync run** in the `sync_runs` table (the **run log**).

**quick check** — the two-request sync: profile count plus the 100 most recently added
entries.

**full harvest** — the sync that collects every shelf entry through the widget, in fixed sort
orders and random samples, until the profile count is reached.

**complete harvest** — a full harvest that found exactly the profile count. Only a complete
harvest deletes entries. (opposite: **partial harvest**, run status `partial`)

**safety-net harvest** — a full harvest run because the last complete one is older than
`NEXTREAD_FULL_HARVEST_EVERY`, even though the quick check saw no change.

**reconcile** — writing a harvest result into the store: insert new entries, refresh existing
ones, delete missing ones if the harvest is complete.

## Details

**enrichment** — looking up a book's details (description, rating, pages, year, ISBN, cover)
after it was added; done once per book by the web server.

**page fallback** — enrichment through the book page instead of the GraphQL API.

**store** — the SQLite database with the shelf, the run log and settings.
