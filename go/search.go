package main

import (
	"database/sql"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"unity-doc-corpus/retrieval"

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
	// The shared policy quotes normalized query terms, so punctuation and FTS operators are
	// literal user input. Safe fill retains every exact implicit-AND result in its original
	// rank before it considers a document-frequency-guided relaxation.
	hits, _, err := retrieval.New(db).Search(query, limit, retrieval.StrategySafeFill)
	if err != nil {
		return nil, fmt.Errorf("FTS query failed (was the corpus built with FTS5 support? check manifest.json sqlite_fts5): %w", err)
	}
	result := make([]searchHit, len(hits))
	for i, hit := range hits {
		result[i] = searchHit{Section: hit.Section, PageID: hit.PageID, Title: hit.Title, PageKey: hit.PageKey}
	}
	return result, nil
}
