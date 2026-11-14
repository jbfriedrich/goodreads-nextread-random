# goodreads-nextread

Picks a random book from a Goodreads shelf, for deciding what to read next off your
`to-read` shelf. It runs as a command-line tool or as a small web page that shows a new pick
on every visit.

It needs no Goodreads login. It keeps a local SQLite copy of one **public** shelf, synced a few
times a day through Goodreads' public shelf widget, and fills in descriptions, ratings and
covers through the GraphQL API behind Goodreads' book pages. The web page never waits for
Goodreads.

## Quick start

Set the shelf, copied from your browser's address bar (the profile and shelf must be public):

```sh
export NEXTREAD_LIST_URL="https://www.goodreads.com/review/list/12345678?shelf=to-read"
```

Then:

```sh
go run ./cmd/goodreads-nextread           # print a random book (the first run syncs the shelf, ~1 min)
go run ./cmd/goodreads-nextread sync      # sync the stored shelf with Goodreads
go run ./cmd/goodreads-nextread serve     # web page on http://localhost:8080
```

Only `serve` fills in book details, so CLI picks show title, author and link until `serve` has
run against the same database. All settings are `NEXTREAD_*` environment variables
([inventory](docs/inventory.md#configuration)).

Example pick:

```
📚 Your next read:

  Never Ever After (Never Ever After #1)
  by Sue Lynn Tan

  Rating:    3.83 avg
  Published: 2025
  Link:      https://www.goodreads.com/book/show/219174189

  Not all fairy tales end happily ever after in this Cinderella-inspired...
```

## Docker

One container runs nginx on port 80 in front of the Go server. TLS is expected to be
terminated by a reverse proxy in front of it. With the compose file, set `NEXTREAD_LIST_URL`
in the service's `environment`, then:

```sh
docker compose build && docker compose up -d
```

Or without compose:

```sh
docker build -t nextbook .
docker run -d --name nextbook --restart unless-stopped -p 8080:80 \
  -e NEXTREAD_LIST_URL="https://www.goodreads.com/review/list/12345678?shelf=to-read" \
  -v nb_data:/app/data \
  nextbook
```

Keep `/app/data` on a volume so the shelf survives rebuilds. Health check: `GET /healthz` →
`ok`. Updates, the log and other procedures: [docs/operations.md](docs/operations.md).

## Documentation

| Doc | What's in it |
|---|---|
| [architecture.md](docs/architecture.md) | How it works: shelf sync, quick check and full harvest, enrichment, errors, serving |
| [inventory.md](docs/inventory.md) | Commands, settings and defaults, endpoints, database tables, container |
| [operations.md](docs/operations.md) | Deploy, change settings, read the log, force a harvest, start over |
| [decisions.md](docs/decisions.md) | Why it is built this way, and what was rejected |
| [glossary.md](docs/glossary.md) | Terms such as shelf entry, quick check, full harvest |
| [todo.md](docs/todo.md) | Every open task |

## Layout

```
cmd/goodreads-nextread/     entry point and subcommands
internal/                   the packages (roles: docs/architecture.md#components)
  goodreads/goodreadstest/  a fake Goodreads server used by the tests
deploy/                     nginx config and the container entrypoint
docs/                       documentation; docs/superpowers/specs/ holds the original design specs
```

## Test

```sh
go test ./...
```

The tests run against the fake Goodreads server and need no network.
