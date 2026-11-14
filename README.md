# goodreads-nextread

Picks one random book from a Goodreads shelf and prints it with metadata and a
link to the book's Goodreads page. Handy for deciding what to read next off your
`to-read` shelf.

It keeps a local SQLite copy of the shelf, synced from Goodreads a few times a
day, so picks are instant and the random choice covers the whole shelf.

## Where the data comes from

Goodreads retired its RSS feeds in October 2026, and the HTML shelf page now
requires a login. The app needs **no login, cookie or account**:

| Endpoint | Used for |
|---|---|
| `/review/custom_widget/<uid>?shelf=…` | The shelf itself: title, author, cover and book id. At most 100 books per call, in any sort order, including `random`. |
| `/user/show/<uid>` | The exact shelf count (e.g. "to-read (636)"), used to prove a harvest is complete. |
| Goodreads GraphQL (AppSync) | Description, average rating, pages, publication year and ISBN, for 25 books per request (~800 bytes per book). It's the API Goodreads' own book pages use, with the public key every book page ships. It's undocumented, so see the fallback below. |
| `/book/show/<id>` | Fallback for the same details when GraphQL is unavailable (641 KB per book, uncompressed). Also where the GraphQL key is read from. |
| `/book/auto_complete` | Finds the book id for the rare entry without a cover. |
| covers.openlibrary.org | Fallback cover (by ISBN) when Goodreads has none. |

**Syncing** happens `scrapes_per_day` times a day (default 4):

- **Quick check (2 requests).** Fetch the profile count and the 100 newest
  entries. If the count matches the stored shelf and every newest entry is known,
  nothing changed. If the count grew by exactly the number of unknown newest
  entries, those are additions and get inserted directly, with no harvest.
- **Full harvest (~40 requests for ~600 books).** This runs when books were
  removed (or swapped), when the database is empty, and once a week as a safety
  net. It walks 16 fixed sort orders, then random samples, until it has as many
  distinct books as the profile reports. Requests are 1.5s apart.
- **Reconcile.** New books are added. Books that left the shelf are deleted
  **only if the harvest is complete**, meaning it found exactly the profile's
  count. A partial harvest only adds, and a failed one changes nothing.
- **Enrichment.** In the background, books without details are looked up in
  batches of 25 through GraphQL, 2s apart. The first fill of ~600 books is about
  26 requests and takes about a minute. After that only new books are looked up.
  If GraphQL fails in a way waiting won't fix (the key is rejected even after
  re-reading it, or the schema changed), that batch falls back to book pages and
  the log says so. Until a book is enriched, it shows with title, author and cover.
- **Stale book ids.** The widget's book id comes from the cover file name, which
  can keep an old id after Goodreads merges editions. When an id turns out not to
  exist, the book is looked up once by title and author, and the current id is
  kept from then on. If that finds nothing, the book links to a Goodreads search
  instead of a dead page.
- **Errors.** A 404 is final: the book is left without details and never retried.
  A 429, 5xx, firewall 403 or network error is retried in place after a wait.
  The wait follows `Retry-After` when Goodreads sends one, and otherwise backs off
  30s, 1m, 2m, 4m, 8m. If that still fails, the sync is retried within an hour and
  enrichment pauses for 5 minutes.

The reader's Goodreads **profile and shelf must be public**.

## Configure

Edit `config.yaml` and set `list_url` to your shelf URL (copy it from your
browser address bar):

```yaml
list_url: "https://www.goodreads.com/review/list/12345678?shelf=to-read"
```

The shelf and profile must be public.

All other settings are optional. Most can also be set with an environment
variable, which **overrides** `config.yaml`. That is handy because the file is
baked into the Docker image. Durations are Go durations such as `90s`, `15m` or
`168h` (there is no `d` unit).

| `config.yaml` key | Environment variable | Default | Meaning |
|---|---|---|---|
| `list_url` | `NEXTREAD_LIST_URL` | (required) | The shelf to pick from |
| `scrapes_per_day` | `NEXTREAD_SCRAPES_PER_DAY` | `4` | Syncs per day (1–1440) |
| `request_delay` | `NEXTREAD_REQUEST_DELAY` | `1.5s` | Gap between sync requests |
| `enrich_delay` | `NEXTREAD_ENRICH_DELAY` | `2s` | Gap between enrichment requests |
| `max_requests` | `NEXTREAD_MAX_REQUESTS` | `80` | Request cap per full harvest (at least 3) |
| `http_timeout` | `NEXTREAD_HTTP_TIMEOUT` | `30s` | Timeout of each HTTP request |
| `full_harvest_every` | `NEXTREAD_FULL_HARVEST_EVERY` | `168h` | Safety-net full harvest, even without changes |
| `sync_retry_after` | `NEXTREAD_SYNC_RETRY_AFTER` | `1h` | Longest wait before retrying a failed sync |
| `enrich_error_pause` | `NEXTREAD_ENRICH_ERROR_PAUSE` | `5m` | Enrichment pause after an error whose retries ran out |
| `db_path` | – | `data/nextread.db` | SQLite file, relative to the config file |
| `graphql_endpoint` | – | Goodreads' | Only if Goodreads moves its GraphQL API |

Precedence: `serve --scrapes-per-day` beats the environment, which beats
`config.yaml`, which beats the default. An empty variable counts as unset.
Invalid values stop the app at startup with an error naming the variable.

## Run

```sh
go run ./cmd/goodreads-nextread
```

The first run has no stored shelf yet, so it harvests it (about a minute). Later
runs pick from the database instantly. Only the server enriches books, so until
`serve` has run against the same database, CLI picks show title, author and link
but no rating, pages or description.

Sync the shelf on demand, which also shows whether Goodreads is reachable:

```sh
go run ./cmd/goodreads-nextread sync          # quick check, or a full harvest if needed
go run ./cmd/goodreads-nextread sync --full   # always harvest the whole shelf
# full sync synced: expected=636 found=636 added=636 removed=0 requests=40 (1m1s)
```

Or build a binary:

```sh
go build -o goodreads-nextread ./cmd/goodreads-nextread
./goodreads-nextread
```

Use a different config file with `--config`:

```sh
go run ./cmd/goodreads-nextread --config other.yaml
```

## Example output

```
📚 Your next read:

  Never Ever After (Never Ever After #1)
  by Sue Lynn Tan

  Rating:    3.83 avg
  Published: 2025
  Link:      https://www.goodreads.com/book/show/219174189

  Not all fairy tales end happily ever after in this Cinderella-inspired...
```

## Web version (Docker + nginx)

The same tool can run as a web page: every visit shows a random book from the
shelf with the same info as the CLI, plus the cover image.

Architecture (one container): **nginx** listens on port 80 and reverse-proxies
to a small **Go** HTTP server on `127.0.0.1:8080` that renders a random book per
request. SSL is expected to be terminated upstream by your existing setup.

The shelf settings come from the same `config.yaml`, which is baked into the
image. The SQLite database lives in `/app/data`. Mount a volume there (the
compose file does) so the shelf and enrichment survive container rebuilds.

**Why the site is fast and never shows auth errors.** Requests never touch
Goodreads:

- **Served from memory.** The Go server loads the shelf from SQLite into memory
  and picks a random book per request.
- **Fast start.** With a stored shelf, the server serves right away and makes no
  requests at startup. With an empty database it starts serving as soon as the
  first batch, the newest 100 books, is stored, a few seconds in. The rest of the
  shelf arrives in the background. If nothing at all can be fetched, the process
  exits and the restart policy retries.
- **Scheduled sync.** The schedule resumes from the run log, so a restart doesn't
  trigger an extra scrape. A failed sync is retried after at most 1 hour.
- **Stale on failure.** If Goodreads is down, throttling, or has changed its
  markup, the sync fails and is logged. The site keeps serving the stored shelf,
  so the reader never sees an error.
- **Status is in the logs only.** `GET /healthz` is public, so it returns just
  `ok`. Every sync logs a line like this:

  ```
  quick sync unchanged: expected=636 found=636 added=0 removed=0 requests=2 (2s); 636 books stored, 0 awaiting enrichment
  ```

  When books were added or removed, the sync names them first, up to 10 per line
  (the initial harvest is not listed):

  ```
  removed 1: "As You Wake, Break the Shell" by Becky Chambers (review 8385834369)
  added 1: "And the River Drags Her Down" by Jihyun Yun (review 9098829870)
  ```

  A book that leaves the shelf has usually moved to another one, e.g.
  `currently-reading`; it keeps its review id there.

  Enrichment logs each batch (`enriched 25 books via graphql (611 pending)`) and
  `enrichment complete` at the end. Errors name the book, the URL and what came
  back (status, or the page title when a page was not what was expected). Watch
  for these:
  - `shelf sync failed` (after its retries) means Goodreads is down or changed
    something.
  - A `partial` status means the harvest hit `max_requests` before reaching the
    profile count.
  - `graphql unavailable, using book pages` means the GraphQL API changed.
  - `blocked by Goodreads' firewall (WAF challenge …)` means Goodreads is
    challenging the server's IP. The app backs off and retries; it never tries
    to pass the challenge.

### Build and push

```sh
docker build -t <your-registry>/nextbook:latest .
docker push <your-registry>/nextbook:latest
```

Since your Docker CLI targets your remote server (context `<context>`), a plain
`docker build` builds straight onto it — no push needed if you run it there.

### Run

```sh
docker run -d --name nextbook --restart unless-stopped -p 8080:80 \
  -v nextbook-data:/app/data \
  <your-registry>/nextbook:latest
```

Then point `https://nextbook.example.com` (via your SSL/reverse-proxy layer)
at the container's port 80. Health check: `GET /healthz` → `ok`.

### Configuration knobs

- **Without rebuilding:** set environment variables (see the table under
  [Configure](#configure)), for example in `docker-compose.yaml`:

  ```yaml
      environment:
        NEXTREAD_LIST_URL: "https://www.goodreads.com/review/list/12345678?shelf=to-read"
        NEXTREAD_SCRAPES_PER_DAY: "6"
        NEXTREAD_HTTP_TIMEOUT: "45s"
  ```

  Or mount your own config over the baked one with
  `-v /path/to/config.yaml:/app/config.yaml:ro`.
- **Sync frequency:** `scrapes_per_day` / `NEXTREAD_SCRAPES_PER_DAY` (default 4).
  The `--scrapes-per-day` flag of `serve` overrides both. The old
  `refresh_interval` setting is rejected with a pointer to its replacement.
- **Listen address:** the server accepts `--addr` (or the `ADDR` env var);
  the default is fine for the container.

### Run the server locally (no Docker)

```sh
go run ./cmd/goodreads-nextread serve                     # listens on :8080
# then open http://localhost:8080
```

## Test

```sh
go test ./...
```

## Code layout

```
cmd/goodreads-nextread/   entry point: the CLI pick, `sync` and `serve` subcommands
internal/
  goodreads/              everything that talks to Goodreads: client with pacing and
                          retries, shelf widget and profile, harvest and quick check,
                          book pages, autocomplete, GraphQL
    goodreadstest/        a fake Goodreads server for tests in every package
  store/                  the SQLite copy of the shelf and the sync log
  shelfsync/              keeps the store in line with Goodreads (quick check / harvest)
  enrich/                 fills in book details in the background, plus Open Library covers
  config/                 config.yaml loading and validation
  web/                    the in-memory cache, HTTP handlers and the page template
deploy/                   nginx config and the container entrypoint
docs/                     design specs
```

Dependencies only point downwards, from `cmd` through `web`, `shelfsync` and
`enrich` to `store` and `goodreads`. `goodreads` uses no database: the GraphQL
key is stored through a small `KeyStore` interface.
