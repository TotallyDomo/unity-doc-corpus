package main

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"

	_ "modernc.org/sqlite"
)

func createSQLite(path string) (*sql.DB, bool, error) {

	_ = os.Remove(path)

	db, err := sql.Open("sqlite", path)

	if err != nil {

		return nil, false, err

	}

	_, err = db.Exec("CREATE TABLE pages (page_key TEXT PRIMARY KEY, section TEXT NOT NULL, page_id TEXT NOT NULL, title TEXT NOT NULL, source_rel TEXT UNIQUE NOT NULL, canonical_url TEXT NOT NULL, source_sha256 BLOB NOT NULL, text_sha256 BLOB NOT NULL, source_bytes INTEGER NOT NULL, text_bytes INTEGER NOT NULL)")

	if err != nil {

		db.Close()

		return nil, false, err

	}

	// page_text holds the rendered Markdown per page - the read payload that used to live in the
	// on-disk text/ directory. Storing it here lets `page`, the audit, and the benchmark read the
	// exact writeMarkdown bytes from one file instead of 39k small ones.
	_, err = db.Exec("CREATE TABLE page_text (page_key TEXT PRIMARY KEY, md TEXT)")

	if err != nil {

		db.Close()

		return nil, false, err

	}

	fts5 := true

	// Contentless FTS5 (content=''): stores only the inverted index, not a %_content shadow
	// copy of title/body. The read payload (the body text) lives once in page_text; recall is
	// unchanged because bm25 runs off the index statistics, not the content shadow - verified
	// bit-identical to the content-backed table over the corpus (M51-S2). page_key stays as an
	// UNINDEXED column so the bm25 weight vector (0, 10, 1) over (page_key, title, body) is
	// unchanged; contentless makes it non-retrievable, so search/benchmark join pages by rowid
	// (build.go aligns the FTS rowid to the pages rowid) rather than reading f.page_key.
	if _, err = db.Exec("CREATE VIRTUAL TABLE pages_fts USING fts5(page_key UNINDEXED, title, body, content='')"); err != nil {

		fts5 = false

	}

	return db, fts5, nil

}

// loadPageText loads the page_text table (page_key -> rendered Markdown) from the corpus
// docs.sqlite into memory. It is the read path that replaced walking the on-disk text/
// directory: callers tokenize this Markdown, so it is the byte-identical writeMarkdown output.
func loadPageText(corpusAbs string) (map[string]string, error) {
	dbPath := filepath.Join(corpusAbs, "docs.sqlite")
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		return nil, err
	}
	defer db.Close()
	rows, err := db.Query("SELECT page_key, md FROM page_text")
	if err != nil {
		return nil, fmt.Errorf("reading page_text from %s (is the corpus built with this tool? rebuild it): %w", dbPath, err)
	}
	defer rows.Close()
	out := make(map[string]string)
	for rows.Next() {
		var key, md string
		if err := rows.Scan(&key, &md); err != nil {
			return nil, err
		}
		out[key] = md
	}
	return out, rows.Err()
}
