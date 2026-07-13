// unity-doc-corpus-concept-eval reports corpus FTS recall for the hand-curated
// agent-style query suite in docs/concept-queries-6000.3.json.
package main

import (
	"database/sql"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"unicode"

	_ "modernc.org/sqlite"
)

const defaultEvalFile = "docs/concept-queries-6000.3.json"

type evalSuite struct {
	CorpusVersion string     `json:"corpus_version"`
	Method        string     `json:"method"`
	Cases         []evalCase `json:"cases"`
}

type evalCase struct {
	ID    string   `json:"id"`
	Query string   `json:"query"`
	Gold  []string `json:"gold"`
}

type result struct {
	ID     string   `json:"id"`
	Query  string   `json:"query"`
	Gold   []string `json:"gold"`
	Hits   []string `json:"hits"`
	Passed bool     `json:"passed"`
}

func main() {
	corpus := flag.String("corpus", "unity-docs/_agent", "Derived corpus directory.")
	evalFile := flag.String("eval", defaultEvalFile, "Concept query suite JSON file.")
	limit := flag.Int("limit", 10, "Number of FTS hits to inspect per query.")
	jsonOutput := flag.Bool("json", false, "Write the per-query result report as JSON.")
	flag.Parse()
	if *limit < 1 {
		fmt.Fprintln(os.Stderr, "error: --limit must be at least 1")
		os.Exit(2)
	}

	suite, err := loadSuite(*evalFile)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
	db, err := sql.Open("sqlite", filepath.Join(*corpus, "docs.sqlite"))
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
	defer db.Close()
	if err := verifyGoldPages(db, suite.Cases); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}

	results := make([]result, 0, len(suite.Cases))
	passed := 0
	for _, c := range suite.Cases {
		hits, err := search(db, c.Query, *limit)
		if err != nil {
			fmt.Fprintf(os.Stderr, "error: %s: %v\n", c.ID, err)
			os.Exit(1)
		}
		ok := containsAny(hits, c.Gold)
		if ok {
			passed++
		}
		results = append(results, result{ID: c.ID, Query: c.Query, Gold: c.Gold, Hits: hits, Passed: ok})
	}

	if *jsonOutput {
		report := struct {
			CorpusVersion string   `json:"corpus_version"`
			Method        string   `json:"method"`
			CaseCount     int      `json:"case_count"`
			HitCount      int      `json:"top_n_recall_count"`
			Recall        float64  `json:"top_n_recall"`
			Results       []result `json:"results"`
		}{
			CorpusVersion: suite.CorpusVersion,
			Method:        suite.Method,
			CaseCount:     len(results),
			HitCount:      passed,
			Recall:        float64(passed) / float64(len(results)),
			Results:       results,
		}
		data, err := json.MarshalIndent(report, "", "  ")
		if err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			os.Exit(1)
		}
		_, _ = os.Stdout.Write(append(data, '\n'))
		return
	}

	for _, r := range results {
		status := "MISS"
		if r.Passed {
			status = "HIT"
		}
		fmt.Printf("%s %s\n  query: %s\n  gold: %s\n  hits: %s\n", status, r.ID, r.Query, strings.Join(r.Gold, ", "), strings.Join(r.Hits, ", "))
	}
	fmt.Printf("concept-query recall@%d: %d/%d (%.1f%%)\n", *limit, passed, len(results), 100*float64(passed)/float64(len(results)))
}

func loadSuite(path string) (evalSuite, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return evalSuite{}, err
	}
	var suite evalSuite
	if err := json.Unmarshal(data, &suite); err != nil {
		return evalSuite{}, err
	}
	if suite.CorpusVersion == "" || suite.Method == "" || len(suite.Cases) == 0 {
		return evalSuite{}, fmt.Errorf("%s is missing corpus_version, method, or cases", path)
	}
	seen := make(map[string]bool, len(suite.Cases))
	for _, c := range suite.Cases {
		if c.ID == "" || c.Query == "" || len(c.Gold) == 0 {
			return evalSuite{}, fmt.Errorf("each case needs id, query, and at least one gold page")
		}
		if seen[c.ID] {
			return evalSuite{}, fmt.Errorf("duplicate case id %q", c.ID)
		}
		seen[c.ID] = true
	}
	return suite, nil
}

func verifyGoldPages(db *sql.DB, cases []evalCase) error {
	stmt, err := db.Prepare("SELECT 1 FROM pages WHERE source_rel = ?")
	if err != nil {
		return err
	}
	defer stmt.Close()
	for _, c := range cases {
		for _, gold := range c.Gold {
			var present int
			err := stmt.QueryRow(gold).Scan(&present)
			if err == sql.ErrNoRows {
				return fmt.Errorf("%s names gold page not present in corpus: %s", c.ID, gold)
			}
			if err != nil {
				return err
			}
		}
	}
	return nil
}

func search(db *sql.DB, query string, limit int) ([]string, error) {
	terms := ftsTerms(query)
	if len(terms) == 0 {
		return nil, fmt.Errorf("query has no FTS terms")
	}
	rows, err := db.Query(`SELECT p.source_rel
FROM pages_fts f JOIN pages p ON p.rowid = f.rowid
WHERE pages_fts MATCH ?
ORDER BY bm25(pages_fts, 0.0, 10.0, 1.0) LIMIT ?`, strings.Join(terms, " "), limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var hits []string
	for rows.Next() {
		var hit string
		if err := rows.Scan(&hit); err != nil {
			return nil, err
		}
		hits = append(hits, hit)
	}
	return hits, rows.Err()
}

func ftsTerms(query string) []string {
	var terms []string
	var word strings.Builder
	flush := func() {
		if word.Len() > 1 {
			terms = append(terms, word.String())
		}
		word.Reset()
	}
	for _, r := range query {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			word.WriteRune(unicode.ToLower(r))
		} else {
			flush()
		}
	}
	flush()
	return terms
}

func containsAny(hits, gold []string) bool {
	for _, hit := range hits {
		for _, expected := range gold {
			if hit == expected {
				return true
			}
		}
	}
	return false
}
