package main

import (
	"database/sql"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	_ "modernc.org/sqlite"
)

// defaultCorpusDir mirrors the builder's default --output so `search` works with no flags
// right after a default build.
const defaultCorpusDir = "unity-docs/_agent"

type searchHit struct {
	Section string
	PageID  string
	Title   string
	MDRel   string
}

// runSearch is the dependency-free lookup path: it runs the same FTS5 query the docs skill
// documents, using the pure-Go SQLite driver already linked for the builder. No sqlite3 CLI
// and no Python are required.
func runSearch(args []string) {
	fs := flag.NewFlagSet("search", flag.ExitOnError)
	corpus := fs.String("corpus", defaultCorpusDir, "Derived corpus directory (the builder's --output).")
	limit := fs.Int("limit", 10, "Maximum number of results.")
	_ = fs.Parse(args)
	query := strings.TrimSpace(strings.Join(fs.Args(), " "))
	if query == "" {
		fmt.Fprintln(os.Stderr, "Usage: unity-doc-corpus search [--corpus <agent-output>] [--limit N] <query>")
		os.Exit(2)
	}
	hits, err := searchCorpus(*corpus, query, *limit)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
	if len(hits) == 0 {
		fmt.Fprintf(os.Stderr, "no matches for %q\n", query)
		return
	}
	for i, h := range hits {
		fmt.Printf("%2d. [%s] %s\n    %s\n", i+1, h.Section, h.Title, h.MDRel)
	}
}

func searchCorpus(corpusDir, query string, limit int) ([]searchHit, error) {
	if limit < 1 {
		limit = 10
	}
	dbPath := filepath.Join(corpusDir, "docs.sqlite")
	if _, err := os.Stat(dbPath); err != nil {
		return nil, fmt.Errorf("no corpus database at %s - run the builder quickstart first (fetch then build)", dbPath)
	}
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		return nil, err
	}
	defer db.Close()
	const q = `SELECT p.section, p.page_id, p.title, p.md_rel
FROM pages_fts f JOIN pages p ON p.page_key = f.page_key
WHERE pages_fts MATCH ?
ORDER BY bm25(pages_fts) LIMIT ?`
	rows, err := db.Query(q, query, limit)
	if err != nil {
		return nil, fmt.Errorf("FTS query failed (was the corpus built with FTS5 support? check manifest.json sqlite_fts5): %w", err)
	}
	defer rows.Close()
	var hits []searchHit
	for rows.Next() {
		var h searchHit
		if err := rows.Scan(&h.Section, &h.PageID, &h.Title, &h.MDRel); err != nil {
			return nil, err
		}
		hits = append(hits, h)
	}
	return hits, rows.Err()
}
