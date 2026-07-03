package main

import (
	"flag"
	"fmt"
	"math/rand"
	"os"
	"strings"
)

const descriptionMaxLen = 300

func main() {
	// `serve` subcommand runs the HTTP server; default runs the CLI.
	if len(os.Args) > 1 && os.Args[1] == "serve" {
		if err := serveCmd(os.Args[2:]); err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			os.Exit(1)
		}
		return
	}

	configPath := flag.String("config", "config.yaml", "path to config file")
	flag.Parse()

	if err := run(*configPath); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func run(configPath string) error {
	cfg, err := loadConfig(configPath)
	if err != nil {
		return err
	}

	books, err := fetchShelf(cfg.RSSURL)
	if err != nil {
		return err
	}
	if len(books) == 0 {
		return fmt.Errorf("no books found on the shelf — check list_url in %s", configPath)
	}

	pick := books[rand.Intn(len(books))]
	fmt.Print(formatBook(pick))
	return nil
}

// formatBook renders a single book as human-readable text. Fields that are
// absent from the feed are omitted rather than printed blank.
func formatBook(b Book) string {
	var sb strings.Builder

	sb.WriteString("\n📚 Your next read:\n\n")
	sb.WriteString("  " + b.Title + "\n")
	if b.Author != "" {
		sb.WriteString("  by " + b.Author + "\n")
	}
	sb.WriteString("\n")

	if b.AverageRating != "" && b.AverageRating != "0.0" {
		sb.WriteString(fmt.Sprintf("  Rating:    %s avg\n", b.AverageRating))
	}
	if b.NumPages != "" {
		sb.WriteString(fmt.Sprintf("  Pages:     %s\n", b.NumPages))
	}
	if b.Published != "" {
		sb.WriteString(fmt.Sprintf("  Published: %s\n", b.Published))
	}
	sb.WriteString(fmt.Sprintf("  Link:      %s\n", b.BookURL()))

	if desc := cleanDescription(b.Description, descriptionMaxLen); desc != "" {
		sb.WriteString("\n  " + desc + "\n")
	}

	return sb.String()
}
