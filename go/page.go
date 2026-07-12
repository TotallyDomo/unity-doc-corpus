package main

import (
	"database/sql"
	"flag"
	"fmt"
	"os"
	"path/filepath"

	_ "modernc.org/sqlite"
)

// runPage prints one page's rendered Markdown from the corpus docs.sqlite page_text table - the
// DB-served read that replaced opening a text/<section>/<key>.md file. The key is the page_key
// (section + "/" + page id) that search prints, e.g. Manual/execution-order.
func runPage(args []string) {
	fs := flag.NewFlagSet("page", flag.ExitOnError)
	corpus := fs.String("corpus", defaultCorpusDir, "Derived corpus directory (the builder's --output).")
	_ = fs.Parse(args)
	if fs.NArg() != 1 {
		fmt.Fprintln(os.Stderr, "Usage: unity-doc-corpus page [--corpus <agent-output>] <page_key>")
		fmt.Fprintln(os.Stderr, "  <page_key> as printed by search, e.g. Manual/execution-order")
		os.Exit(2)
	}
	md, err := pageMarkdown(*corpus, fs.Arg(0))
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
	os.Stdout.WriteString(md)
}

func pageMarkdown(corpusDir, pageKey string) (string, error) {
	dbPath := filepath.Join(corpusDir, "docs.sqlite")
	if _, err := os.Stat(dbPath); err != nil {
		absDB, aerr := filepath.Abs(dbPath)
		if aerr != nil {
			absDB = dbPath
		}
		return "", fmt.Errorf("no corpus database at %s - build the corpus first (bin/unity-doc-corpus build ...)", absDB)
	}
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		return "", err
	}
	defer db.Close()
	var md string
	err = db.QueryRow("SELECT md FROM page_text WHERE page_key = ?", pageKey).Scan(&md)
	if err == sql.ErrNoRows {
		return "", fmt.Errorf("no page %q in the corpus (check the page_key against a search result)", pageKey)
	}
	if err != nil {
		return "", err
	}
	return md, nil
}
