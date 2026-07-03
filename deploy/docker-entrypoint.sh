#!/bin/sh
# Start the Go backend and nginx in one container. If either process exits,
# stop the container so the orchestrator's restart policy can recover it.
set -eu

CONFIG_PATH="${CONFIG_PATH:-/app/config.yaml}"

/usr/local/bin/goodreads-nextread serve --config "$CONFIG_PATH" --addr 127.0.0.1:8080 &
GO_PID=$!

nginx -g 'daemon off;' &
NGINX_PID=$!

# Poll both processes; exit as soon as one dies.
while kill -0 "$GO_PID" 2>/dev/null && kill -0 "$NGINX_PID" 2>/dev/null; do
    sleep 2
done

echo "a process exited (go=$GO_PID nginx=$NGINX_PID); shutting down container" >&2
kill "$GO_PID" "$NGINX_PID" 2>/dev/null || true
exit 1
