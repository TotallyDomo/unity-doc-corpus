package main

import "testing"

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

func TestSourceRelFromMarkdownFrontMatter(t *testing.T) {
	md := "---\nsection: Manual\nsource_rel: Manual/Rigidbody2D.html\ntitle: Rigidbody2D\n---\n\n# Rigidbody2D\n"
	if got := sourceRelFromMarkdown(md, "/corpus/text/Manual/Rigidbody2D.md", "/corpus"); got != "Manual/Rigidbody2D.html" {
		t.Errorf("source_rel extraction = %q", got)
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
