# Operations

How-tos for running goodreads-nextread. Design: [architecture.md](architecture.md). Values
(settings, ports, volume, tables): [inventory.md](inventory.md).

Container commands run on the workstation with the Docker CLI, in the directory with
`docker-compose.yaml`. **Before any of them, check which host your Docker context targets**
(`docker context ls`), because a remote default context runs everything on that host.

Status of each procedure: **tested** (run against the real deployment), **tested locally**
(run on a workstation), **CLI-checked** (commands checked, not run).

## Deploy or update the container (tested 2026-11-14)
Builds the image from the working tree and replaces the running container. The data volume
and the stored shelf are kept.

`docker-compose.yaml` must set the shelf for the service:

```yaml
    environment:
      NEXTREAD_LIST_URL: "https://www.goodreads.com/review/list/<user id>?shelf=to-read"
```

```sh
docker context ls                      # confirm the target host first
docker compose build
docker compose up -d
docker compose logs -f nextbook
```

Check: with a stored shelf the log shows `serving <n> books from /app/data/nextread.db;
syncing 4 times a day` right away. On the first start with an empty volume it shows
`no stored shelf yet; …`, then `serving 100 books …` a few seconds later, a
`full sync synced: expected=<n> found=<n> …` line after about a minute, and
`enrichment complete` shortly after. `curl -s http://<host>:8080/healthz` returns `ok`.

If the container restarts in a loop, the log's last line names the cause; usually
`NEXTREAD_LIST_URL is not set` or an invalid value.

## Change a setting (tested 2026-11-14)
Settings are environment variables ([inventory](inventory.md#configuration)). Edit them in the
service's `environment` in `docker-compose.yaml`, then recreate the container; no rebuild is
needed.

```sh
docker compose up -d
docker compose logs --tail 5 nextbook
```

Check: the startup line shows the new value where it appears (`syncing <n> times a day`), and
no `environment variable NEXTREAD_…` error.

## Read the log
Every sync writes one summary line, and the log names what changed. What the lines mean:

| Line | Meaning | Action |
|---|---|---|
| `quick sync unchanged: … ; <n> books stored, <m> awaiting enrichment` | Nothing changed on the shelf | — |
| `quick sync synced: … added=<k>` | Only additions, inserted without a harvest | — |
| `shelf changed (Goodreads reports <a>, stored <b>, …); running a full harvest` | The quick check saw a removal or swap | — |
| `full sync synced: expected=<n> found=<n> …` | Complete full harvest | — |
| `added <k>: "<title>" by <author> (review <id>)` / `removed <k>: …` | The books that changed (up to 10 per line) | A removed book has usually moved to another shelf |
| `warning: harvest found <x> of <n> books within <r> requests; …` and status `partial` | The harvest hit `NEXTREAD_MAX_REQUESTS` before reaching the profile count; nothing was deleted | Raise `NEXTREAD_MAX_REQUESTS` if it repeats |
| `<request>: <error>; retry <i>/5 in <wait>` | A transient error is being retried | — unless it repeats for hours |
| `shelf sync failed, keeping the stored shelf: …` | All retries failed; the site keeps serving the stored shelf | If it persists: Goodreads is down or changed the widget or profile page |
| `… blocked by Goodreads' firewall (WAF challenge, status 202 …)` | Goodreads challenges the server's IP | Wait; the app backs off on its own |
| `enriched <k> books via graphql (<m> pending)` / `enrichment complete` | Book details being filled in | — |
| `graphql: book <id>: <type>: <message>; trying its book page` | GraphQL failed for one book | — |
| `graphql unavailable, using book pages: …` | The GraphQL API changed or rejects the key | Enrichment continues more slowly; check whether the endpoint moved ([inventory](inventory.md#endpoints-used)) |
| `enrichment paused for 5m0s: …` | An enrichment request failed after its retries; the error names the URL and the page title | — unless it repeats |
| `enrichment: "<title>": book id <a> is stale, using <b>` | A merged edition; the current id was found | — |
| `enrichment: "<title>" by <author> is not on Goodreads (book id <id>); …` | The book was removed from Goodreads; it links to a search | — |

## Run a full harvest now (CLI-checked; `sync --full` tested locally)
Forces a complete re-read of the shelf, e.g. after you changed a book on Goodreads and don't
want to wait for the next sync. The command uses the container's environment and database.

```sh
docker compose exec nextbook goodreads-nextread sync --full
```

Check: one line `full sync synced: expected=<n> found=<n> added=… removed=… requests=…`,
preceded by `added …` / `removed …` lines if anything changed. The running server shows the
change after its next scheduled sync or enrichment batch; restart the container
(`docker compose restart nextbook`) to show it immediately.

## Run locally (tested locally 2026-11-14)
Needs Go (version in `go.mod`). The database is `data/nextread.db` in the working directory.

```sh
export NEXTREAD_LIST_URL="https://www.goodreads.com/review/list/<user id>?shelf=to-read"
go run ./cmd/goodreads-nextread            # a random pick (syncs first if the store is empty)
go run ./cmd/goodreads-nextread sync       # one sync, prints its summary
go run ./cmd/goodreads-nextread serve      # web server on :8080
```

Check: `sync` prints a line such as `quick sync unchanged: expected=<n> found=<n> …`.

## Start over with an empty database (CLI-checked) — destructive
Deletes the stored shelf, the sync log and the cached GraphQL key. The next start rebuilds
everything from Goodreads: about 40 requests for the shelf and 26 for the details of ~600
books, about two minutes.

```sh
docker compose down                        # keeps the volume
docker volume rm nextbook_nb_data
docker compose up -d
```

Check: the log shows `no stored shelf yet; …` and then the first-start lines from
[Deploy or update the container](#deploy-or-update-the-container-tested-2026-11-14).
There is no undo apart from the rebuild from Goodreads.
