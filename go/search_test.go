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
		if _, err := db.Exec(
			"INSERT INTO pages VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)",
			key, p.Section, p.PageID, p.Title, p.PageID+".html", p.MDRel, "", "", "", 0, 0,
		); err != nil {
			t.Fatalf("insert pages: %v", err)
		}
		if _, err := db.Exec(
			"INSERT INTO pages_fts(page_key, title, body) VALUES (?, ?, ?)",
			key, p.Title, bodies[p.PageID],
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
			{Section: "Manual", PageID: "transform-sync", Title: "Optimize transform value syncing", MDRel: "text/Manual/transform-sync.md"},
			{Section: "ScriptReference", PageID: "Rigidbody", Title: "Rigidbody", MDRel: "text/ScriptReference/Rigidbody.md"},
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
