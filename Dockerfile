# syntax=docker/dockerfile:1

# ---- build the Go binary ----
FROM golang:1.26-alpine AS build
WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY *.go ./
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /goodreads-nextread .

# ---- runtime: nginx + the Go backend ----
FROM nginx:alpine

COPY --from=build /goodreads-nextread /usr/local/bin/goodreads-nextread
COPY config.yaml /app/config.yaml
COPY deploy/nginx.conf /etc/nginx/conf.d/default.conf
COPY deploy/docker-entrypoint.sh /docker-entrypoint-app.sh
RUN chmod +x /docker-entrypoint-app.sh

EXPOSE 80
ENTRYPOINT ["/docker-entrypoint-app.sh"]
