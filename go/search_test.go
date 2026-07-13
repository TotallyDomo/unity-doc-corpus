package main

import (
	"path/filepath"
	"testing"
)

// buildTestCorpus writes a minimal docs.sqlite (pages + pages_fts) so searchCorpus can be
// exercised without a full fetch/build.
func buildTestCorpus(t *testing.T, dir string, pages []searchHit, bodies map[string]string) {
	t.Helper()
	db, fts5, err := createSQLite(filepath.Join(dir, "docs.sqlite"))
	if err != nil {
		t.Fatalf("createSQLite: %v", err)
	}
	defer db.Close()
	if !fts5 {
		t.Skip("FTS5 not available in this SQLite build")
	}
	for _, p := range pages {
		key := p.Section + "/" + p.PageID
		res, err := db.Exec(
			"INSERT INTO pages VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)",
			key, p.Section, p.PageID, p.Title, p.PageID+".html", "text/"+key+".md", "", "", "", 0, 0,
		)
		if err != nil {
			t.Fatalf("insert pages: %v", err)
		}
		// pages_fts is contentless: pin its rowid to the pages rowid so the rowid join resolves.
		rowid, err := res.LastInsertId()
		if err != nil {
			t.Fatalf("pages LastInsertId: %v", err)
		}
		if _, err := db.Exec(
			"INSERT INTO pages_fts(rowid, page_key, title, body) VALUES (?, ?, ?, ?)",
			rowid, key, p.Title, bodies[p.PageID],
		); err != nil {
			t.Fatalf("insert pages_fts: %v", err)
		}
	}
}

func TestSearchCorpus(t *testing.T) {
	dir := t.TempDir()
	buildTestCorpus(t,
		dir,
		[]searchHit{
			{Section: "Manual", PageID: "transform-sync", Title: "Optimize transform value syncing"},
			{Section: "ScriptReference", PageID: "Rigidbody", Title: "Rigidbody"},
		},
		map[string]string{
			"transform-sync": "auto sync transforms physics performance",
			"Rigidbody":      "a rigidbody component for physics",
		},
	)

	hits, err := searchCorpus(dir, "auto sync transforms", 5)
	if err != nil {
		t.Fatalf("searchCorpus: %v", err)
	}
	if len(hits) == 0 {
		t.Fatal("expected at least one hit")
	}
	if hits[0].PageID != "transform-sync" {
		t.Errorf("top hit = %q, want transform-sync", hits[0].PageID)
	}
}

func TestSearchCorpusMissingDB(t *testing.T) {
	if _, err := searchCorpus(t.TempDir(), "anything", 5); err == nil {
		t.Fatal("expected an error when docs.sqlite is absent")
	}
}

// Queries that break the raw FTS5 parser must fall back to the sanitized, term-quoted retry
// instead of surfacing a parse error: unbalanced quotes ("unterminated string", which does
// not contain the substring "syntax error" the old retry keyed on) and bareword operators
// (whose sanitized form equals the original query, which the old retry skipped).
func TestSearchCorpusSanitizeRetry(t *testing.T) {
	dir := t.TempDir()
	buildTestCorpus(t,
		dir,
		[]searchHit{{Section: "Manual", PageID: "not-page", Title: "Do not destroy on load"}},
		map[string]string{"not-page": "objects marked do not destroy survive scene loads and reloads"},
	)
	cases := []struct {
		query   string
		wantHit bool
	}{
		{`"unbalanced phrase`, false},
		{`AND OR NOT`, false},     // pure barewords: literal match, no parse error
		{`destroy AND NOT`, true}, // operator barewords quoted to literals still match real terms
	}
	for _, c := range cases {
		hits, err := searchCorpus(dir, c.query, 5)
		if err != nil {
			t.Errorf("searchCorpus(%q) must sanitize-retry, got error: %v", c.query, err)
			continue
		}
		if c.wantHit && len(hits) == 0 {
			t.Errorf("searchCorpus(%q) expected a hit after quoting", c.query)
		}
	}
}

func TestFtsQuoteTerms(t *testing.T) {
	if got := ftsQuoteTerms("AND OR NOT"); got != `"AND" "OR" "NOT"` {
		t.Errorf("ftsQuoteTerms = %q", got)
	}
	if got := ftsQuoteTerms(""); got != "" {
		t.Errorf("empty input must stay empty, got %q", got)
	}
}
