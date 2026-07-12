package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// tokenize drives retrieval: tokens are lowercased and single-character tokens dropped.
// With fts=false the '.', '_', '-' joiners stay inside a token (so "Rigidbody.MovePosition"
// stays one plain-search term); with fts=true they split (matching how the FTS query is
// built). This split is the contract the benchmark relies on to compare plain vs FTS recall.
func TestTokenizeFtsVsPlainSplitting(t *testing.T) {
	eq := func(got, want []string) bool {
		if len(got) != len(want) {
			return false
		}
		for i := range got {
			if got[i] != want[i] {
				return false
			}
		}
		return true
	}
	if got := tokenize("Rigidbody.MovePosition", false); !eq(got, []string{"rigidbody.moveposition"}) {
		t.Errorf("plain tokenize kept joiners wrong: %v", got)
	}
	if got := tokenize("Rigidbody.MovePosition", true); !eq(got, []string{"rigidbody", "moveposition"}) {
		t.Errorf("fts tokenize should split on '.': %v", got)
	}
	if got := tokenize("a big X", false); !eq(got, []string{"big"}) {
		t.Errorf("single-char tokens should be dropped: %v", got)
	}
}

// scoreText is AND semantics: every term must be present or the document scores zero;
// otherwise the score is the summed term frequency.
func TestScoreTextRequiresAllTerms(t *testing.T) {
	if got := scoreText("foo bar foo", []string{"foo", "bar"}); got != 3 {
		t.Errorf("score = %d, want 3 (2*foo + 1*bar)", got)
	}
	if got := scoreText("foo foo", []string{"foo", "baz"}); got != 0 {
		t.Errorf("score = %d, want 0 (missing term must zero the doc)", got)
	}
}

// searchDocs ranks by score desc, breaks ties by SourceRel asc, and caps at 10 hits.
func TestSearchDocsRankingAndCap(t *testing.T) {
	docs := []doc{
		{SourceRel: "b.html", Text: "x", Bytes: 1},
		{SourceRel: "a.html", Text: "x x", Bytes: 3},
		{SourceRel: "c.html", Text: "no match here", Bytes: 13},
	}
	hits, _, _ := searchDocs(docs, []string{"x"})
	if len(hits) != 2 {
		t.Fatalf("expected 2 hits, got %d (%+v)", len(hits), hits)
	}
	if hits[0].SourceRel != "a.html" || hits[1].SourceRel != "b.html" {
		t.Errorf("ranking wrong: %+v", hits)
	}

	many := make([]doc, 0, 12)
	for _, name := range []string{"01", "02", "03", "04", "05", "06", "07", "08", "09", "10", "11", "12"} {
		many = append(many, doc{SourceRel: name + ".html", Text: "x", Bytes: 1})
	}
	capped, _, _ := searchDocs(many, []string{"x"})
	if len(capped) != 10 {
		t.Errorf("expected hits capped at 10, got %d", len(capped))
	}
}

func TestContainsHelpers(t *testing.T) {
	if !containsHit([]hit{{"a.html", 2}}, "a.html") || containsHit([]hit{{"a.html", 2}}, "z.html") {
		t.Error("containsHit wrong")
	}
	if !containsString([]string{"a", "b"}, "b") || containsString([]string{"a"}, "z") {
		t.Error("containsString wrong")
	}
}

// extractHTMLTitle feeds the raw-HTML FTS5 baseline's title column; it must survive
// attribute-free tags, entities, and whitespace, and return "" when no title exists.
func TestExtractHTMLTitle(t *testing.T) {
	cases := []struct{ in, want string }{
		{"<html><head><title>Unity - Scripting API:  Rigidbody</title></head>", "Unity - Scripting API: Rigidbody"},
		{"<TITLE>a &amp; b</TITLE>", "a & b"},
		{"<html><body>no title</body></html>", ""},
	}
	for _, c := range cases {
		if got := extractHTMLTitle(c.in); got != c.want {
			t.Errorf("extractHTMLTitle(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// generatedCases must sample evenly across the whole (sorted) pages.jsonl, not take its
// head: pages.jsonl sorts Manual before ScriptReference, so a head slice would test only
// Manual pages while the corpus is ~91% ScriptReference.
func TestGeneratedCasesStrideSampleSpansCorpus(t *testing.T) {
	dir := t.TempDir()
	var lines []string
	for i := 0; i < 300; i++ {
		lines = append(lines, fmt.Sprintf(`{"source_rel":"Manual/page%03d.html","title":"Manual Page %03d","page_id":"page%03d"}`, i, i, i))
	}
	for i := 0; i < 700; i++ {
		lines = append(lines, fmt.Sprintf(`{"source_rel":"ScriptReference/Class%03d.html","title":"Class%03d.Member","page_id":"Class%03d"}`, i, i, i))
	}
	if err := os.WriteFile(filepath.Join(dir, "pages.jsonl"), []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	cases, err := generatedCases(dir, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(cases) != 100 {
		t.Fatalf("expected 100 cases, got %d", len(cases))
	}
	manual, script := 0, 0
	for _, c := range cases {
		if strings.HasPrefix(c.Expected, "Manual/") {
			manual++
		} else {
			script++
		}
	}
	if manual != 30 || script != 70 {
		t.Errorf("sample mix = %d Manual / %d ScriptReference, want 30/70 (corpus proportions)", manual, script)
	}

	all, err := generatedCases(dir, 5000)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 1000 {
		t.Errorf("limit above corpus size should return every eligible page, got %d", len(all))
	}
}
