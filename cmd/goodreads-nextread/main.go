package main

import (
	"flag"
	"fmt"
	"os"
)

const descriptionMaxLen = 300

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

	configPath := flag.String("config", "config.yaml", "path to config file")
	flag.Parse()

	if err := run(*configPath); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}
