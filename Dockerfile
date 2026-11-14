# syntax=docker/dockerfile:1

# ---- build the Go binary ----
FROM golang:1.27.3-alpine AS build
WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY cmd/ cmd/
COPY internal/ internal/
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /goodreads-nextread ./cmd/goodreads-nextread

# ---- runtime: nginx + the Go backend ----
FROM nginx:alpine

COPY --from=build /goodreads-nextread /usr/local/bin/goodreads-nextread
# Settings come from NEXTREAD_* environment variables; NEXTREAD_LIST_URL (the
# shelf) must be set when the container runs. The SQLite copy of the shelf
# lives in /app/data: mount a volume there so it survives container rebuilds;
# without one the shelf is re-harvested (~1 min) whenever the container is
# recreated.
ENV NEXTREAD_DB_PATH=/app/data/nextread.db
RUN mkdir -p /app/data
VOLUME /app/data
COPY deploy/nginx.conf /etc/nginx/conf.d/default.conf
COPY deploy/docker-entrypoint.sh /docker-entrypoint-app.sh
RUN chmod +x /docker-entrypoint-app.sh

EXPOSE 80
ENTRYPOINT ["/docker-entrypoint-app.sh"]
