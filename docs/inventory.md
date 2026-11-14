# Inventory

Facts about goodreads-nextread as tables. How it works: [architecture.md](architecture.md).
Terms: [glossary.md](glossary.md).

## Commands

| Command | What it does | Side effects |
|---|---|---|
| `goodreads-nextread` | Prints a random book from the store; with an empty store it syncs first | Writes the database on the first run |
| `goodreads-nextread sync` | Runs one sync (quick check or full harvest, as the schedule would) and prints its summary | Goodreads requests; writes the database |
| `goodreads-nextread sync --full` | Runs a full harvest regardless of the quick check | ~40 Goodreads requests; writes the database |
| `goodreads-nextread serve` | Web server, scheduled syncs and background enrichment | Goodreads, GraphQL and Open Library requests; writes the database |
| `goodreads-nextread -h` | Prints the usage | — |

`serve` flags: `--addr` (listen address, default `ADDR` or `:8080`) and
`--scrapes-per-day` (overrides `NEXTREAD_SCRAPES_PER_DAY`, 1–1440).

Source: `cmd/goodreads-nextread/main.go`, `cli.go`, `serve.go`.

## Configuration

All settings are environment variables. Durations are Go durations (`90s`, `15m`, `168h`;
there is no `d` unit). An empty variable counts as unset. An invalid value stops the app at
startup with an error naming the variable.

| Environment variable | Default | Valid values | Used for |
|---|---|---|---|
| `NEXTREAD_LIST_URL` | — (required) | `https://www.goodreads.com/review/list/<user id>?shelf=<shelf>` | The shelf to read |
| `NEXTREAD_SCRAPES_PER_DAY` | `4` | 1–1440 | Syncs per day |
| `NEXTREAD_REQUEST_DELAY` | `1.5s` | ≥ 0 | Gap between sync requests |
| `NEXTREAD_ENRICH_DELAY` | `2s` | ≥ 0 | Gap between enrichment requests |
| `NEXTREAD_MAX_REQUESTS` | `80` | ≥ 3 | Request cap per full harvest |
| `NEXTREAD_HTTP_TIMEOUT` | `30s` | > 0 | Timeout of each HTTP request |
| `NEXTREAD_FULL_HARVEST_EVERY` | `168h` | > 0 | Safety-net full harvest interval |
| `NEXTREAD_SYNC_RETRY_AFTER` | `1h` | > 0 | Longest wait before retrying a failed sync |
| `NEXTREAD_ENRICH_ERROR_PAUSE` | `5m` | > 0 | Enrichment pause after an error whose retries ran out |
| `NEXTREAD_DB_PATH` | `data/nextread.db` | a path; relative to the working directory | SQLite file (the image sets `/app/data/nextread.db`) |
| `NEXTREAD_GRAPHQL_ENDPOINT` | Goodreads' AppSync endpoint (see below) | an `http(s)` URL | GraphQL API for enrichment |
| `ADDR` | `:8080` | a listen address | `serve` listen address, when `--addr` is not given |

Precedence: `serve --scrapes-per-day` > environment > default.

Source: `internal/config/config.go`, `cmd/goodreads-nextread/serve.go`.

## Fixed limits

Values that are not configurable.

| Limit | Value | Source |
|---|---|---|
| Entries per widget request | 100 (Goodreads' cap) | `internal/goodreads/client.go` |
| Fixed widget orders per full harvest | 16 | `internal/goodreads/widget.go` (`fixedOrders`) |
| Books per GraphQL request | 25 | `internal/goodreads/graphql.go` (`BatchSize`) |
| GraphQL request body | ≤ 6 KB | `internal/goodreads/graphql.go` (`MaxQueryBody`) |
| Retry waits (transient errors) | 30 s, 1 min, 2 min, 4 min, 8 min | `internal/goodreads/retry.go` (`retryBackoff`) |
| Longest `Retry-After` honoured | 15 min | `internal/goodreads/retry.go` (`maxRetryAfter`) |
| Enrichment idle poll | 1 min | `internal/enrich/enrich.go` (`enrichIdlePoll`) |
| Books named per log line | 10 | `internal/shelfsync/sync.go` (`maxLoggedChanges`) |
| Description length shown | 700 characters (web), 300 (CLI) | `internal/web/server.go`, `cmd/goodreads-nextread/main.go` |
| User-Agent | `Mozilla/5.0 (goodreads-nextread)` | `internal/goodreads/client.go` |

## Endpoints used

| Endpoint | Method | Used for | Source |
|---|---|---|---|
| `https://www.goodreads.com/review/custom_widget/<user id>?shelf=…&num_books=100&sort=…&order=…&widget_id=…` | GET | Shelf entries | `internal/goodreads/client.go` (`widget`) |
| `https://www.goodreads.com/user/show/<user id>` | GET | Shelf count (follows a redirect to `/user/show/<user id>-<name>`) | `internal/goodreads/client.go` (`ShelfCount`) |
| `https://www.goodreads.com/book/show/<book id>` | GET | Book details (fallback) and the GraphQL API key | `internal/goodreads/bookpage.go`, `graphql.go` |
| `https://www.goodreads.com/book/auto_complete?format=json&q=<title author>` | GET | Book id by title and author | `internal/goodreads/bookpage.go` |
| `https://kxbwmqov6jgg3daaamb744ycu4.appsync-api.us-east-1.amazonaws.com/graphql` | POST, header `x-api-key` | Book details (`getBookByLegacyId`) | `internal/goodreads/graphql.go` (`DefaultGraphQLEndpoint`) |
| `https://covers.openlibrary.org/b/isbn/<isbn>-L.jpg?default=false` | GET | Fallback cover | `internal/enrich/enrich.go` |

None needs a login. The GraphQL API key is public: every book page ships it in its
`__NEXT_DATA__` JSON (`props.pageProps.apiKey`).

## Files and state

| What | Where | Written by |
|---|---|---|
| SQLite database | `NEXTREAD_DB_PATH`, plus `-wal` and `-shm` files next to it | `internal/store` |

Tables:

| Table | Key | Columns | Notes |
|---|---|---|---|
| `books` | `review_id` | `book_id`, `title`, `author`, `average_rating`, `num_pages`, `published`, `isbn`, `image_url`, `description`, `enriched_at`, `first_seen_at`, `last_seen_at` | One row per shelf entry. `enriched_at` is set once details were looked up, also when nothing was found. `enrich_retry_at` exists in the schema and is unused |
| `sync_runs` | `id` | `started_at`, `finished_at`, `kind` (`quick`, `full`), `status` (`unchanged`, `synced`, `partial`, `failed`), `expected`, `found`, `added`, `removed`, `requests`, `error` | The sync log; the schedule and the safety net read it |
| `settings` | `key` | `value` | `graphql_api_key`: the cached GraphQL API key |

Source: `internal/store/store.go`, `internal/goodreads/graphql.go` (`SettingGraphQLKey`).

## Container

| Fact | Value |
|---|---|
| Processes | nginx on port 80, forwarding to the Go server on `127.0.0.1:8080`; if either exits, the container stops |
| Exposed port | 80 |
| Data volume | `/app/data` (`VOLUME`) |
| Environment set by the image | `NEXTREAD_DB_PATH=/app/data/nextread.db` |
| Required at run time | `NEXTREAD_LIST_URL` |
| Base images | `golang:1.27.3-alpine` (build), `nginx:alpine` (runtime) |
| Health check | `GET /healthz` → `ok` |

Compose (`docker-compose.yaml`): project `nextbook`, service and container `nextbook`, port
`8080:80`, named volume `nb_data` on `/app/data` (`nextbook_nb_data` on the host), restart
`unless-stopped`, health check every 30 s with a 30 s start period. The compose file does not
set `NEXTREAD_LIST_URL`; add it to the service's `environment`.

Source: `Dockerfile`, `deploy/docker-entrypoint.sh`, `deploy/nginx.conf`, `docker-compose.yaml`.
