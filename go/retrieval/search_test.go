package retrieval

import (
	"database/sql"
	"reflect"
	"testing"

	_ "modernc.org/sqlite"
)

func testSearcher(t *testing.T) *Searcher {
	t.Helper()
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if _, err := db.Exec(`CREATE TABLE pages (
		page_key TEXT, section TEXT, page_id TEXT, title TEXT, source_rel TEXT
	)`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec("CREATE VIRTUAL TABLE pages_fts USING fts5(page_key UNINDEXED, title, body)"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec("CREATE VIRTUAL TABLE pages_fts_vocab USING fts5vocab(pages_fts, row)"); err != nil {
		t.Fatal(err)
	}
	for _, page := range []struct {
		name string
		body string
	}{
		{"exact-a", "common rare"},
		{"exact-b", "common rare"},
		{"rare-c", "rare"},
		{"rare-d", "rare"},
		{"rare-e", "rare"},
		{"common-a", "common"},
		{"common-b", "common"},
		{"common-c", "common"},
	} {
		res, err := db.Exec("INSERT INTO pages(page_key, section, page_id, title, source_rel) VALUES (?, 'Manual', ?, ?, ?)", page.name, page.name, page.name, page.name+".html")
		if err != nil {
			t.Fatal(err)
		}
		rowID, err := res.LastInsertId()
		if err != nil {
			t.Fatal(err)
		}
		if _, err := db.Exec("INSERT INTO pages_fts(rowid, page_key, title, body) VALUES (?, ?, ?, ?)", rowID, page.name, page.name, page.body); err != nil {
			t.Fatal(err)
		}
	}
	return New(db)
}

func sourceRels(hits []Hit) []string {
	paths := make([]string, len(hits))
	for i, hit := range hits {
		paths[i] = hit.SourceRel
	}
	return paths
}

func TestSafeFillPreservesExactResults(t *testing.T) {
	searcher := testSearcher(t)
	exact, _, err := searcher.Search("common rare", 5, StrategyExact)
	if err != nil {
		t.Fatal(err)
	}
	safe, stats, err := searcher.Search("common rare", 5, StrategySafeFill)
	if err != nil {
		t.Fatal(err)
	}
	if len(safe) <= len(exact) {
		t.Fatalf("safe fill did not append relaxed results: exact=%v safe=%v", sourceRels(exact), sourceRels(safe))
	}
	if !reflect.DeepEqual(sourceRels(safe[:len(exact)]), sourceRels(exact)) {
		t.Errorf("safe fill displaced exact results: exact=%v safe=%v", sourceRels(exact), sourceRels(safe))
	}
	if stats.QueryCount != 2 || stats.VocabularyLookup != 2 || !stats.Relaxed || stats.UsedORFallback {
		t.Errorf("stats = %+v, want two retrieval queries and vocabulary-guided relaxation", stats)
	}
}

func TestRequestedLimitKeepsExactOnlyPath(t *testing.T) {
	searcher := testSearcher(t)
	hits, stats, err := searcher.Search("common rare", 2, StrategySafeFill)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 2 || stats.QueryCount != 1 || stats.Relaxed {
		t.Errorf("limited exact path = hits %v, stats %+v", sourceRels(hits), stats)
	}
}

func TestZeroResultUsesORAsFinalFallback(t *testing.T) {
	searcher := testSearcher(t)
	hits, stats, err := searcher.Search("common missing", 5, StrategySafeFill)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) == 0 || stats.QueryCount != 3 || !stats.UsedORFallback {
		t.Errorf("zero-result fallback = hits %v, stats %+v", sourceRels(hits), stats)
	}
}

func TestFusedRankingIsDeterministicAndExactBiased(t *testing.T) {
	searcher := testSearcher(t)
	exact, _, err := searcher.Search("common rare", 5, StrategyExact)
	if err != nil {
		t.Fatal(err)
	}
	first, _, err := searcher.Search("common rare", 5, StrategyFused)
	if err != nil {
		t.Fatal(err)
	}
	second, _, err := searcher.Search("common rare", 5, StrategyFused)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(sourceRels(first), sourceRels(second)) {
		t.Errorf("fused order is not deterministic: %v / %v", sourceRels(first), sourceRels(second))
	}
	if !reflect.DeepEqual(sourceRels(first[:len(exact)]), sourceRels(exact)) {
		t.Errorf("fused ranking displaced exact results: exact=%v fused=%v", sourceRels(exact), sourceRels(first))
	}
}

func TestTermsTreatsOperatorsAsLiterals(t *testing.T) {
	if got, want := Terms("A C# callback, 2D AND file-name"), []string{"callback", "2d", "and", "file", "name"}; !reflect.DeepEqual(got, want) {
		t.Errorf("Terms = %v, want %v", got, want)
	}
}
