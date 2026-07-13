package main

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestConceptSuiteHasBalancedCuratedCases(t *testing.T) {
	suite, err := loadSuite(filepath.Join("..", "..", "..", "docs", "concept-queries-6000.3.json"))
	if err != nil {
		t.Fatal(err)
	}
	if suite.CorpusVersion != "6000.3" {
		t.Fatalf("corpus version = %q, want 6000.3", suite.CorpusVersion)
	}
	if len(suite.Cases) != 100 {
		t.Fatalf("case count = %d, want 100", len(suite.Cases))
	}
	manual, api := 0, 0
	for _, c := range suite.Cases {
		if len(strings.Fields(c.Query)) < 3 {
			t.Errorf("%s query is too short to be a concept query: %q", c.ID, c.Query)
		}
		switch {
		case strings.HasPrefix(c.Gold[0], "Manual/"):
			manual++
		case strings.HasPrefix(c.Gold[0], "ScriptReference/"):
			api++
		default:
			t.Errorf("%s uses an unsupported gold path %q", c.ID, c.Gold[0])
		}
		for _, gold := range c.Gold {
			if !strings.HasSuffix(gold, ".html") {
				t.Errorf("%s gold path is not an HTML source path: %q", c.ID, gold)
			}
		}
	}
	if manual != 50 || api != 50 {
		t.Errorf("case distribution = %d Manual / %d ScriptReference, want 50/50", manual, api)
	}
}

func TestFTSTermsDropsPunctuationAndSingleCharacters(t *testing.T) {
	got := strings.Join(ftsTerms("A C# callback, 2D, and file-name"), " ")
	if got != "callback 2d and file name" {
		t.Fatalf("fts terms = %q", got)
	}
}
