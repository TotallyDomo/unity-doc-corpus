package main

import (
	"path/filepath"
	"strings"
	"testing"
	"unicode"
)

func TestConceptSuitesHaveSeparateBalancedPartitions(t *testing.T) {
	development, err := loadSuite(filepath.Join("..", "..", "..", "docs", "concept-queries-6000.3.json"))
	if err != nil {
		t.Fatal(err)
	}
	heldOut, err := loadSuite(filepath.Join("..", "..", "..", "docs", "concept-queries-6000.3-heldout.json"))
	if err != nil {
		t.Fatal(err)
	}
	if development.CorpusVersion != "6000.3" || heldOut.CorpusVersion != "6000.3" {
		t.Fatalf("corpus versions = %q and %q, want 6000.3", development.CorpusVersion, heldOut.CorpusVersion)
	}
	if development.Role != "development" || heldOut.Role != "held_out" {
		t.Fatalf("roles = %q and %q, want development and held_out", development.Role, heldOut.Role)
	}
	assertSectionBalance(t, development, 100, 50, 50)
	assertSectionBalance(t, heldOut, 200, 100, 100)

	developmentQueries := make(map[string]bool, len(development.Cases))
	for _, c := range development.Cases {
		developmentQueries[normalizeQuery(c.Query)] = true
	}
	heldOutQueries := make(map[string]bool, len(heldOut.Cases))
	for _, c := range heldOut.Cases {
		normalized := normalizeQuery(c.Query)
		if developmentQueries[normalized] {
			t.Errorf("%s duplicates a development query: %q", c.ID, c.Query)
		}
		if heldOutQueries[normalized] {
			t.Errorf("duplicate held-out query: %q", c.Query)
		}
		heldOutQueries[normalized] = true
		for _, gold := range c.Gold {
			if copiedSourcePathBigram(c.Query, gold) {
				t.Errorf("%s copies a meaningful consecutive source-path term pair: %q / %q", c.ID, c.Query, gold)
			}
		}
	}
}

func assertSectionBalance(t *testing.T, suite evalSuite, wantCases, wantManual, wantAPI int) {
	t.Helper()
	if len(suite.Cases) != wantCases {
		t.Fatalf("%s case count = %d, want %d", suite.Role, len(suite.Cases), wantCases)
	}
	manual, api := 0, 0
	for _, c := range suite.Cases {
		if len(strings.Fields(c.Query)) < 3 {
			t.Errorf("%s query is too short to be a concept query: %q", c.ID, c.Query)
		}
		section, err := sectionForGold(c.Gold)
		if err != nil {
			t.Errorf("%s: %v", c.ID, err)
			continue
		}
		switch section {
		case "Manual":
			manual++
		case "Scripting API":
			api++
		}
		for _, gold := range c.Gold {
			if !strings.HasSuffix(gold, ".html") {
				t.Errorf("%s gold path is not an HTML source path: %q", c.ID, gold)
			}
		}
	}
	if manual != wantManual || api != wantAPI {
		t.Errorf("%s distribution = %d Manual / %d Scripting API, want %d/%d", suite.Role, manual, api, wantManual, wantAPI)
	}
}

func normalizeQuery(query string) string {
	return strings.Join(strings.Fields(strings.ToLower(query)), " ")
}

func copiedSourcePathBigram(query, sourcePath string) bool {
	queryTokens := strings.Fields(normalizeQuery(query))
	pathTokens := sourcePathTokens(sourcePath)
	for i := 0; i+1 < len(pathTokens); i++ {
		for j := 0; j+1 < len(queryTokens); j++ {
			if pathTokens[i] == queryTokens[j] && pathTokens[i+1] == queryTokens[j+1] {
				return true
			}
		}
	}
	return false
}

func sourcePathTokens(sourcePath string) []string {
	base := strings.TrimSuffix(filepath.Base(sourcePath), filepath.Ext(sourcePath))
	var normalized strings.Builder
	var previous rune
	for _, r := range base {
		if unicode.IsUpper(r) && unicode.IsLower(previous) {
			normalized.WriteByte(' ')
		}
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			normalized.WriteRune(unicode.ToLower(r))
		} else {
			normalized.WriteByte(' ')
		}
		previous = r
	}
	ignored := map[string]bool{"class": true, "html": true, "manual": true, "scriptreference": true}
	var tokens []string
	for _, token := range strings.Fields(normalized.String()) {
		if len(token) >= 3 && !ignored[token] {
			tokens = append(tokens, token)
		}
	}
	return tokens
}

func TestFTSTermsDropsPunctuationAndSingleCharacters(t *testing.T) {
	got := strings.Join(ftsTerms("A C# callback, 2D, and file-name"), " ")
	if got != "callback 2d and file name" {
		t.Fatalf("fts terms = %q", got)
	}
}
