package main

import (
	"flag"
	"fmt"
	"os"
)

const descriptionMaxLen = 300

func usage() {
	fmt.Fprint(os.Stderr, `Usage:
  goodreads-nextread           print a random book from the shelf
  goodreads-nextread sync      sync the stored shelf with Goodreads (--full: whole shelf)
  goodreads-nextread serve     run the web server (--addr, --scrapes-per-day)

Configured with environment variables; NEXTREAD_LIST_URL (the shelf URL from
your browser) is required. See the README for all NEXTREAD_* settings.
`)
}

func main() {
	// `serve` runs the HTTP server, `sync` syncs the shelf once; the default
	// prints a random book.
	if len(os.Args) > 1 {
		var cmd func([]string) error
		switch os.Args[1] {
		case "serve":
			cmd = serveCmd
		case "sync":
			cmd = syncCmd
		}
		if cmd != nil {
			if err := cmd(os.Args[2:]); err != nil {
				fmt.Fprintln(os.Stderr, "error:", err)
				os.Exit(1)
			}
			return
		}
	}

	flag.Usage = usage
	flag.Parse()

	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}
