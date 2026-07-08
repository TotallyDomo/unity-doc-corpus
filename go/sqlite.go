package main

import (
	"database/sql"
	"os"

	_ "modernc.org/sqlite"
)

func createSQLite(path string) (*sql.DB, bool, error) {

	_ = os.Remove(path)

	db, err := sql.Open("sqlite", path)

	if err != nil {

		return nil, false, err

	}

	_, err = db.Exec("CREATE TABLE pages (page_key TEXT PRIMARY KEY, section TEXT, page_id TEXT, title TEXT, source_rel TEXT, md_rel TEXT, canonical_url TEXT, source_sha256 TEXT, text_sha256 TEXT, source_bytes INTEGER, text_bytes INTEGER)")

	if err != nil {

		db.Close()

		return nil, false, err

	}

	fts5 := true

	if _, err = db.Exec("CREATE VIRTUAL TABLE pages_fts USING fts5(page_key UNINDEXED, title, body)"); err != nil {

		fts5 = false

	}

	return db, fts5, nil

}
