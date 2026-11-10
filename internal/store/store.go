package store

import (
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"goodreads-nextread/internal/goodreads"

	_ "modernc.org/sqlite" // pure-Go driver: keeps CGO_ENABLED=0 builds working
)

// Store is the SQLite-backed copy of the shelf plus a log of sync runs. It is
// the source of truth between scrapes: the web server's in-memory cache is loaded from
// it, and a failed scrape never changes it.
type Store struct {
	db *sql.DB
}

// SyncRun is one entry of the sync log.
type SyncRun struct {
	StartedAt  time.Time
	FinishedAt time.Time
	Kind       string // "quick" or "full"
	Status     string // "unchanged", "synced", "partial" or "failed"
	Expected   int    // shelf size reported by the profile page
	Found      int    // distinct entries collected
	Added      int
	Removed    int
	Requests   int
	Err        string
}

const schema = `
CREATE TABLE IF NOT EXISTS books (
	review_id       TEXT PRIMARY KEY,
	book_id         TEXT NOT NULL DEFAULT '',
	title           TEXT NOT NULL DEFAULT '',
	author          TEXT NOT NULL DEFAULT '',
	average_rating  TEXT NOT NULL DEFAULT '',
	num_pages       TEXT NOT NULL DEFAULT '',
	published       TEXT NOT NULL DEFAULT '',
	isbn            TEXT NOT NULL DEFAULT '',
	image_url       TEXT NOT NULL DEFAULT '',
	description     TEXT NOT NULL DEFAULT '',
	enriched_at     TEXT,
	enrich_retry_at TEXT, -- unused since retries happen in place; kept for old databases
	first_seen_at   TEXT NOT NULL,
	last_seen_at    TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS settings (
	key   TEXT PRIMARY KEY,
	value TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS sync_runs (
	id          INTEGER PRIMARY KEY,
	started_at  TEXT NOT NULL,
	finished_at TEXT NOT NULL,
	kind        TEXT NOT NULL,
	status      TEXT NOT NULL,
	expected    INTEGER NOT NULL DEFAULT 0,
	found       INTEGER NOT NULL DEFAULT 0,
	added       INTEGER NOT NULL DEFAULT 0,
	removed     INTEGER NOT NULL DEFAULT 0,
	requests    INTEGER NOT NULL DEFAULT 0,
	error       TEXT NOT NULL DEFAULT ''
);
`

const bookColumns = `review_id, book_id, title, author, average_rating, num_pages, published, isbn, image_url, description`

// Open opens (creating if needed) the database at path and applies the
// schema. The parent directory is created if missing.
func Open(path string) (*Store, error) {
	if dir := filepath.Dir(path); dir != "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, fmt.Errorf("creating database directory: %w", err)
		}
	}

	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)")
	if err != nil {
		return nil, fmt.Errorf("opening database %q: %w", path, err)
	}
	// One connection: serialises the sync and enrichment writers, and keeps the
	// temp table used by Reconcile on the transaction's connection.
	db.SetMaxOpenConns(1)

	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("initialising database %q: %w", path, err)
	}
	return &Store{db: db}, nil
}

func (s *Store) Close() error { return s.db.Close() }

func ts(t time.Time) string { return t.UTC().Format(time.RFC3339Nano) }

// Reconcile writes a scrape result. Every book is inserted or has its widget
// fields (title, author, cover when present) refreshed; enrichment columns are
// left alone. The widget's book id (taken from the cover file name, which can
// keep a stale id after Goodreads merges editions) only fills books that are
// not enriched yet: after enrichment, the verified id wins. Only when complete is true — the scrape collected
// exactly the number of books the profile reports — are books missing from the
// result deleted. A partial scrape therefore never shrinks the shelf.
func (s *Store) Reconcile(books []goodreads.Book, complete bool, now time.Time) (added, removed int, err error) {
	tx, err := s.db.Begin()
	if err != nil {
		return 0, 0, err
	}
	defer tx.Rollback()

	if _, err := tx.Exec(`CREATE TEMP TABLE IF NOT EXISTS seen (review_id TEXT PRIMARY KEY); DELETE FROM seen;`); err != nil {
		return 0, 0, err
	}
	for _, b := range books {
		if _, err := tx.Exec(`INSERT OR IGNORE INTO seen (review_id) VALUES (?)`, b.ReviewID); err != nil {
			return 0, 0, err
		}
	}
	if err := tx.QueryRow(`SELECT COUNT(*) FROM seen WHERE review_id NOT IN (SELECT review_id FROM books)`).Scan(&added); err != nil {
		return 0, 0, err
	}

	upsert, err := tx.Prepare(`
		INSERT INTO books (review_id, book_id, title, author, image_url, first_seen_at, last_seen_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (review_id) DO UPDATE SET
			title        = excluded.title,
			author       = excluded.author,
			book_id      = CASE WHEN books.enriched_at IS NULL AND excluded.book_id <> '' THEN excluded.book_id ELSE books.book_id END,
			image_url    = CASE WHEN excluded.image_url <> '' THEN excluded.image_url ELSE books.image_url END,
			last_seen_at = excluded.last_seen_at`)
	if err != nil {
		return 0, 0, err
	}
	defer upsert.Close()
	for _, b := range books {
		if _, err := upsert.Exec(b.ReviewID, b.BookID, b.Title, b.Author, b.ImageURL, ts(now), ts(now)); err != nil {
			return 0, 0, err
		}
	}

	if complete {
		res, err := tx.Exec(`DELETE FROM books WHERE review_id NOT IN (SELECT review_id FROM seen)`)
		if err != nil {
			return 0, 0, err
		}
		n, _ := res.RowsAffected()
		removed = int(n)
	}

	if err := tx.Commit(); err != nil {
		return 0, 0, err
	}
	return added, removed, nil
}

func scanBooks(rows *sql.Rows) ([]goodreads.Book, error) {
	defer rows.Close()
	var books []goodreads.Book
	for rows.Next() {
		var b goodreads.Book
		if err := rows.Scan(&b.ReviewID, &b.BookID, &b.Title, &b.Author, &b.AverageRating,
			&b.NumPages, &b.Published, &b.ISBN, &b.ImageURL, &b.Description); err != nil {
			return nil, err
		}
		books = append(books, b)
	}
	return books, rows.Err()
}

// AllBooks returns every book on the stored shelf.
func (s *Store) AllBooks() ([]goodreads.Book, error) {
	rows, err := s.db.Query(`SELECT ` + bookColumns + ` FROM books ORDER BY first_seen_at, review_id`)
	if err != nil {
		return nil, err
	}
	return scanBooks(rows)
}

// Count returns the number of books on the stored shelf.
func (s *Store) Count() (int, error) {
	var n int
	err := s.db.QueryRow(`SELECT COUNT(*) FROM books`).Scan(&n)
	return n, err
}

// HasAll reports whether every review id is already stored.
func (s *Store) HasAll(reviewIDs []string) (bool, error) {
	stmt, err := s.db.Prepare(`SELECT EXISTS (SELECT 1 FROM books WHERE review_id = ?)`)
	if err != nil {
		return false, err
	}
	defer stmt.Close()
	for _, id := range reviewIDs {
		var ok bool
		if err := stmt.QueryRow(id).Scan(&ok); err != nil {
			return false, err
		}
		if !ok {
			return false, nil
		}
	}
	return true, nil
}

// PendingEnrichment returns up to limit books that have not been enriched
// yet, oldest first.
func (s *Store) PendingEnrichment(limit int) ([]goodreads.Book, error) {
	rows, err := s.db.Query(`SELECT `+bookColumns+` FROM books
		WHERE enriched_at IS NULL ORDER BY first_seen_at, review_id LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	return scanBooks(rows)
}

// PendingEnrichmentCount returns how many books still lack enrichment.
func (s *Store) PendingEnrichmentCount() (int, error) {
	var n int
	err := s.db.QueryRow(`SELECT COUNT(*) FROM books WHERE enriched_at IS NULL`).Scan(&n)
	return n, err
}

// SaveEnrichment stores the details looked up for a book and marks it
// enriched. Empty fields in e leave the stored value unchanged, so saving an
// empty Book marks a permanent failure (e.g. the book no longer exists) as
// done without touching what is stored.
func (s *Store) SaveEnrichment(reviewID string, e goodreads.Book, now time.Time) error {
	_, err := s.db.Exec(`UPDATE books SET
			book_id        = CASE WHEN ?1 <> '' THEN ?1 ELSE book_id        END,
			average_rating = CASE WHEN ?2 <> '' THEN ?2 ELSE average_rating END,
			num_pages      = CASE WHEN ?3 <> '' THEN ?3 ELSE num_pages      END,
			published      = CASE WHEN ?4 <> '' THEN ?4 ELSE published      END,
			isbn           = CASE WHEN ?5 <> '' THEN ?5 ELSE isbn           END,
			image_url      = CASE WHEN ?6 <> '' THEN ?6 ELSE image_url      END,
			description    = CASE WHEN ?7 <> '' THEN ?7 ELSE description    END,
			enriched_at    = ?8,
			enrich_retry_at = NULL
		WHERE review_id = ?9`,
		e.BookID, e.AverageRating, e.NumPages, e.Published, e.ISBN, e.ImageURL, e.Description, ts(now), reviewID)
	return err
}

// MarkMissing records that Goodreads has no book for this entry: it clears the
// (dead) book id, so the page links to a Goodreads search instead, and marks
// enrichment done so it is not retried.
func (s *Store) MarkMissing(reviewID string, now time.Time) error {
	_, err := s.db.Exec(`UPDATE books SET book_id = '', enriched_at = ? WHERE review_id = ?`, ts(now), reviewID)
	return err
}

// Setting returns a stored setting, or "" if unset.
func (s *Store) Setting(key string) (string, error) {
	var v string
	err := s.db.QueryRow(`SELECT value FROM settings WHERE key = ?`, key).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return v, err
}

// SetSetting stores a setting, replacing any previous value.
func (s *Store) SetSetting(key, value string) error {
	_, err := s.db.Exec(`INSERT INTO settings (key, value) VALUES (?, ?)
		ON CONFLICT (key) DO UPDATE SET value = excluded.value`, key, value)
	return err
}

// RecordRun appends a run to the sync log.
func (s *Store) RecordRun(r SyncRun) error {
	_, err := s.db.Exec(`INSERT INTO sync_runs
		(started_at, finished_at, kind, status, expected, found, added, removed, requests, error)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		ts(r.StartedAt), ts(r.FinishedAt), r.Kind, r.Status, r.Expected, r.Found, r.Added, r.Removed, r.Requests, r.Err)
	return err
}

// LastRun returns the most recent sync run, if any.
func (s *Store) LastRun() (SyncRun, bool, error) {
	var r SyncRun
	var started, finished string
	err := s.db.QueryRow(`SELECT started_at, finished_at, kind, status, expected, found, added, removed, requests, error
		FROM sync_runs ORDER BY id DESC LIMIT 1`).
		Scan(&started, &finished, &r.Kind, &r.Status, &r.Expected, &r.Found, &r.Added, &r.Removed, &r.Requests, &r.Err)
	if errors.Is(err, sql.ErrNoRows) {
		return SyncRun{}, false, nil
	}
	if err != nil {
		return SyncRun{}, false, err
	}
	if r.StartedAt, err = time.Parse(time.RFC3339Nano, started); err != nil {
		return SyncRun{}, false, err
	}
	if r.FinishedAt, err = time.Parse(time.RFC3339Nano, finished); err != nil {
		return SyncRun{}, false, err
	}
	return r, true, nil
}

// LastFullSync returns when the last complete full harvest finished.
func (s *Store) LastFullSync() (time.Time, bool, error) {
	var finished string
	err := s.db.QueryRow(`SELECT finished_at FROM sync_runs
		WHERE kind = 'full' AND status = 'synced' ORDER BY id DESC LIMIT 1`).Scan(&finished)
	if errors.Is(err, sql.ErrNoRows) {
		return time.Time{}, false, nil
	}
	if err != nil {
		return time.Time{}, false, err
	}
	t, err := time.Parse(time.RFC3339Nano, finished)
	return t, err == nil, err
}

// describe renders a run as a one-line summary for logs and the CLI.
func (r SyncRun) Describe() string {
	s := fmt.Sprintf("%s sync %s: expected=%d found=%d added=%d removed=%d requests=%d (%s)",
		r.Kind, r.Status, r.Expected, r.Found, r.Added, r.Removed, r.Requests,
		r.FinishedAt.Sub(r.StartedAt).Round(time.Second))
	if r.Err != "" {
		s += ": " + r.Err
	}
	return s
}
