# goodreads-nextread

Picks one random book from a Goodreads shelf and prints it with metadata and a
link to the book's Goodreads page. Handy for deciding what to read next off your
`to-read` shelf.

It reads the shelf's public RSS feed (Goodreads' public API is retired), paging
through the whole shelf so the random pick is fair regardless of shelf size.

## Configure

Edit `config.yaml` and set `list_url` to your shelf URL (copy it from your
browser address bar):

```yaml
list_url: "https://www.goodreads.com/review/list/12345678?shelf=to-read"
```

The shelf must be public. The tool derives the RSS endpoint automatically.

## Run

```sh
go run .
```

Or build a binary:

```sh
go build -o goodreads-nextread .
./goodreads-nextread
```

Use a different config file with `--config`:

```sh
go run . --config other.yaml
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

The shelf comes from the same `config.yaml`, which is baked into the image.

**Shelf caching (why the site is fast).** The Go server keeps the whole shelf in
memory so it never hits Goodreads on the request path:

- **Primed at startup** — the shelf is fetched once *before* the server accepts
  traffic, so no visitor ever waits on the initial (slow) Goodreads fetch. The
  container takes a little longer to become ready in exchange. If this initial
  fetch fails, the process exits and the container's restart policy retries.
- **Refreshed in the background** — a goroutine re-fetches the shelf on an
  interval (default 30 min, set `refresh_interval` in `config.yaml`), so shelf
  changes show up within one interval. Refreshes happen off the request path.
- **Stale-on-failure** — if a background refresh fails (Goodreads down or
  throttling), the error is logged and the last known-good list keeps being
  served, so a Goodreads hiccup never takes the site down.
- **Random per request** — every visit independently picks a random book from
  the in-memory list, so reloading always gives a fresh pick.

The net effect: the ~20s cold fetch is paid once at container startup instead of
by whichever visitor happens to arrive after an idle period.

### Build and push

```sh
docker build -t <your-registry>/nextbook:latest .
docker push <your-registry>/nextbook:latest
```

Since your Docker CLI targets your remote server (context `lloyd`), a plain
`docker build` builds straight onto it — no push needed if you run it there.

### Run

```sh
docker run -d --name nextbook --restart unless-stopped -p 8080:80 \
  <your-registry>/nextbook:latest
```

Then point `https://nextbook.geekshelter.net` (via your SSL/reverse-proxy layer)
at the container's port 80. Health check: `GET /healthz` → `ok`.

### Configuration knobs

- **Different shelf without rebuilding:** mount your own config over the baked
  one — `-v /path/to/config.yaml:/app/config.yaml:ro`.
- **Background refresh interval:** set `refresh_interval` in `config.yaml`
  (a Go duration like `"30m"`, `"1h"`; default 30 min). A `--refresh-interval`
  flag overrides the config value when set.
- **Listen address:** the server accepts `--addr` (or the `ADDR` env var);
  the default is fine for the container.

### Run the server locally (no Docker)

```sh
go run . serve                     # listens on :8080
# then open http://localhost:8080
```

## Test

```sh
go test ./...
```
