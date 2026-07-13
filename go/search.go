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
	PageKey string
}

// runSearch is the dependency-free lookup path: it runs the same FTS5 query the docs skill
// documents, using the pure-Go SQLite driver already linked for the builder. No sqlite3 CLI
// and no Python are required.
func runSearch(args []string) {
	fs := flag.NewFlagSet("search", flag.ExitOnError)
	corpus := fs.String("corpus", defaultCorpusDir, "Derived corpus directory (the builder's --output).")
	limit := fs.Int("limit", 10, "Maximum number of results.")
	_ = fs.Parse(args)
	for _, arg := range fs.Args() {
		if strings.HasPrefix(arg, "-") {
			fmt.Fprintf(os.Stderr, "error: flag %q found after the query - flags must come before it\nUsage: unity-doc-corpus search [--corpus <agent-output>] [--limit N] <query>\n", arg)
			os.Exit(2)
		}
	}
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
		fmt.Printf("%2d. [%s] %s\n    page %s\n", i+1, h.Section, h.Title, h.PageKey)
	}
}

func searchCorpus(corpusDir, query string, limit int) ([]searchHit, error) {
	if limit < 1 {
		limit = 10
	}
	dbPath := filepath.Join(corpusDir, "docs.sqlite")
	if _, err := os.Stat(dbPath); err != nil {
		absDB, aerr := filepath.Abs(dbPath)
		if aerr != nil {
			absDB = dbPath
		}
		// For the default layout the docs root is the corpus dir's parent; point the user at
		// the exact step they are missing instead of the whole quickstart.
		srcRoot := filepath.Dir(filepath.Clean(corpusDir))
		if _, serr := os.Stat(filepath.Join(srcRoot, "Manual")); serr == nil {
			return nil, fmt.Errorf("no corpus database at %s\nthe docs are fetched but the corpus is not built yet - run:\n  bin/unity-doc-corpus build --source %s --output %s", absDB, srcRoot, corpusDir)
		}
		return nil, fmt.Errorf("no corpus database at %s - run the quickstart first:\n  bin/unity-doc-corpus fetch --version <ver>\n  bin/unity-doc-corpus build --source %s --output %s", absDB, srcRoot, corpusDir)
	}
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		return nil, err
	}
	defer db.Close()
	// bm25 weights: page_key (unindexed), title, body. Title outweighs body 10:1 - measured
	// on the reference benchmark, unweighted bm25 buries short canonical pages (a bare class
	// name ranks the class page below its member pages).
	// pages_fts is contentless (M51-S2): f.page_key is not retrievable, so join on rowid,
	// which build.go pins equal to the pages rowid. bm25 weights are unchanged.
	const q = `SELECT p.section, p.page_id, p.title, p.page_key
FROM pages_fts f JOIN pages p ON p.rowid = f.rowid
WHERE pages_fts MATCH ?
ORDER BY bm25(pages_fts, 0.0, 10.0, 1.0) LIMIT ?`
	rows, err := db.Query(q, query, limit)
	if err != nil {
		// Raw FTS5 parsing choked (dots in API names, stray operators, unbalanced quotes,
		// bareword AND/OR/NOT). Retry once with the query reduced to plain alphanumeric terms,
		// each double-quoted so FTS5 reads them as literals rather than operators. Any first
		// query error takes this path: with a valid database the only failures here are query
		// parse errors, and one extra retry against a genuinely broken database is harmless.
		if quoted := ftsQuoteTerms(ftsSanitize(query)); quoted != "" {
			rows, err = db.Query(q, quoted, limit)
		}
	}
	if err != nil {
		return nil, fmt.Errorf("FTS query failed even after sanitizing (was the corpus built with FTS5 support? check manifest.json sqlite_fts5): %w", err)
	}
	defer rows.Close()
	var hits []searchHit
	for rows.Next() {
		var h searchHit
		if err := rows.Scan(&h.Section, &h.PageID, &h.Title, &h.PageKey); err != nil {
			return nil, err
		}
		hits = append(hits, h)
	}
	return hits, rows.Err()
}

// ftsSanitize reduces a query to space-separated alphanumeric terms - the same shape the
// benchmark feeds FTS5 - dropping single-character fragments.
func ftsSanitize(query string) string {
	var terms []string
	var b strings.Builder
	flush := func() {
		if b.Len() > 1 {
			terms = append(terms, b.String())
		}
		b.Reset()
	}
	for _, r := range query {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
		} else {
			flush()
		}
	}
	flush()
	return strings.Join(terms, " ")
}

// ftsQuoteTerms wraps each space-separated term in FTS5 string quotes, so sanitized barewords
// that collide with FTS5 operators (AND, OR, NOT, NEAR) match as literal terms instead of
// erroring. Input comes from ftsSanitize, so the terms are plain alphanumerics.
func ftsQuoteTerms(sanitized string) string {
	if sanitized == "" {
		return ""
	}
	terms := strings.Fields(sanitized)
	for i, t := range terms {
		terms[i] = `"` + t + `"`
	}
	return strings.Join(terms, " ")
}
