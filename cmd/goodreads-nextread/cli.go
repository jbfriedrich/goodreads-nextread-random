package main

import (
	"context"
	"flag"
	"fmt"
	"math/rand"
	"os"
	"strings"

	"goodreads-nextread/internal/config"
	"goodreads-nextread/internal/goodreads"
	"goodreads-nextread/internal/store"
)

func run(configPath string) error {
	cfg, err := config.Load(configPath)
	if err != nil {
		return err
	}

	st, err := store.Open(cfg.DBPath)
	if err != nil {
		return err
	}
	defer st.Close()

	books, err := st.AllBooks()
	if err != nil {
		return err
	}
	if len(books) == 0 {
		// First run: nothing stored yet, so harvest the shelf once (~1 min for
		// a few hundred books). Later runs read the stored copy instantly.
		fmt.Fprintln(os.Stderr, "No stored shelf yet; fetching it from Goodreads (this takes a minute)...")
		run, err := newSyncer(cfg, st).Sync(context.Background(), false)
		if err != nil {
			return err
		}
		fmt.Fprintln(os.Stderr, run.Describe())
		if books, err = st.AllBooks(); err != nil {
			return err
		}
	}
	if len(books) == 0 {
		return fmt.Errorf("no books found on the shelf — check list_url in %s", configPath)
	}

	pick := books[rand.Intn(len(books))]
	fmt.Print(formatBook(pick))
	return nil
}

// syncCmd syncs the shelf once (the `sync` subcommand) and prints a summary.
// It applies the same quick-check logic as the server unless --full is given.
func syncCmd(args []string) error {
	fs := flag.NewFlagSet("sync", flag.ExitOnError)
	configPath := fs.String("config", "config.yaml", "path to config file")
	full := fs.Bool("full", false, "always harvest the whole shelf instead of a quick check")
	fs.Parse(args)

	cfg, err := config.Load(*configPath)
	if err != nil {
		return err
	}
	st, err := store.Open(cfg.DBPath)
	if err != nil {
		return err
	}
	defer st.Close()

	run, err := newSyncer(cfg, st).Sync(context.Background(), *full)
	fmt.Println(run.Describe())
	return err
}

// formatBook renders a single book as human-readable text. Fields that are
// absent from the feed are omitted rather than printed blank.
func formatBook(b goodreads.Book) string {
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
	sb.WriteString(fmt.Sprintf("  Link:      %s\n", b.URL()))

	if desc := goodreads.CleanDescription(b.Description, descriptionMaxLen); desc != "" {
		sb.WriteString("\n  " + desc + "\n")
	}

	return sb.String()
}
