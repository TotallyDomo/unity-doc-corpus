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
	"sort"
	"strings"
	"time"
	"unity-doc-corpus/retrieval"

	_ "modernc.org/sqlite"
)

const defaultEvalFile = "docs/concept-queries-6000.3.json"

type evalSuite struct {
	CorpusVersion string     `json:"corpus_version"`
	Role          string     `json:"role"`
	Method        string     `json:"method"`
	Cases         []evalCase `json:"cases"`
}

type evalCase struct {
	ID    string   `json:"id"`
	Query string   `json:"query"`
	Gold  []string `json:"gold"`
}

type result struct {
	ID                string   `json:"id"`
	Section           string   `json:"section"`
	Query             string   `json:"query"`
	Gold              []string `json:"gold"`
	Hits              []string `json:"hits"`
	Passed            bool     `json:"passed"`
	ElapsedMS         float64  `json:"elapsed_ms"`
	RetrievalQueries  int      `json:"retrieval_queries"`
	VocabularyLookups int      `json:"vocabulary_lookups"`
}

type sectionReport struct {
	Section   string  `json:"section"`
	CaseCount int     `json:"case_count"`
	HitCount  int     `json:"top_n_recall_count"`
	Recall    float64 `json:"top_n_recall"`
}

type performanceReport struct {
	Policy            string  `json:"policy"`
	P50MS             float64 `json:"p50_ms"`
	P95MS             float64 `json:"p95_ms"`
	RetrievalQueries  int     `json:"retrieval_queries"`
	QueriesPerCase    float64 `json:"queries_per_case"`
	VocabularyLookups int     `json:"vocabulary_lookups"`
	VocabularyPerCase float64 `json:"vocabulary_lookups_per_case"`
}

func main() {
	corpus := flag.String("corpus", "unity-docs/_agent", "Derived corpus directory.")
	evalFile := flag.String("eval", defaultEvalFile, "Concept query suite JSON file.")
	limit := flag.Int("limit", 10, "Number of FTS hits to inspect per query.")
	policy := flag.String("policy", "safe-fill", "Retrieval policy: exact, safe-fill, or fused.")
	jsonOutput := flag.Bool("json", false, "Write the per-query result report as JSON.")
	flag.Parse()
	if *limit < 1 {
		fmt.Fprintln(os.Stderr, "error: --limit must be at least 1")
		os.Exit(2)
	}
	strategy, err := retrieval.ParseStrategy(*policy)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
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
	searcher := retrieval.New(db)
	if err := verifyGoldPages(db, suite.Cases); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}

	results := make([]result, 0, len(suite.Cases))
	passed := 0
	sectionTotals := map[string]int{}
	sectionHits := map[string]int{}
	for _, c := range suite.Cases {
		section, err := sectionForGold(c.Gold)
		if err != nil {
			fmt.Fprintf(os.Stderr, "error: %s: %v\n", c.ID, err)
			os.Exit(1)
		}
		started := time.Now()
		hits, stats, err := searcher.Search(c.Query, *limit, strategy)
		elapsed := time.Since(started)
		if err != nil {
			fmt.Fprintf(os.Stderr, "error: %s: %v\n", c.ID, err)
			os.Exit(1)
		}
		hitPaths := make([]string, len(hits))
		for i, hit := range hits {
			hitPaths[i] = hit.SourceRel
		}
		ok := containsAny(hitPaths, c.Gold)
		if ok {
			passed++
			sectionHits[section]++
		}
		sectionTotals[section]++
		results = append(results, result{ID: c.ID, Section: section, Query: c.Query, Gold: c.Gold, Hits: hitPaths, Passed: ok, ElapsedMS: float64(elapsed.Microseconds()) / 1000, RetrievalQueries: stats.QueryCount, VocabularyLookups: stats.VocabularyLookup})
	}
	sections := makeSectionReports(sectionTotals, sectionHits)
	performance := summarizePerformance(results, strategy)

	if *jsonOutput {
		report := struct {
			CorpusVersion string            `json:"corpus_version"`
			Role          string            `json:"role"`
			Method        string            `json:"method"`
			CaseCount     int               `json:"case_count"`
			HitCount      int               `json:"top_n_recall_count"`
			Recall        float64           `json:"top_n_recall"`
			Sections      []sectionReport   `json:"sections"`
			Performance   performanceReport `json:"performance"`
			Results       []result          `json:"results"`
		}{
			CorpusVersion: suite.CorpusVersion,
			Role:          suite.Role,
			Method:        suite.Method,
			CaseCount:     len(results),
			HitCount:      passed,
			Recall:        float64(passed) / float64(len(results)),
			Sections:      sections,
			Performance:   performance,
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
	for _, section := range sections {
		fmt.Printf("%s recall@%d: %d/%d (%.1f%%)\n", section.Section, *limit, section.HitCount, section.CaseCount, 100*section.Recall)
	}
	fmt.Printf("retrieval policy: %s; latency p50/p95: %.3f/%.3f ms; FTS queries: %d (%.2f/query); vocabulary lookups: %d (%.2f/query)\n", performance.Policy, performance.P50MS, performance.P95MS, performance.RetrievalQueries, performance.QueriesPerCase, performance.VocabularyLookups, performance.VocabularyPerCase)
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
	if suite.Role == "" {
		suite.Role = "custom"
	}
	if suite.Role != "development" && suite.Role != "held_out" && suite.Role != "custom" {
		return evalSuite{}, fmt.Errorf("%s has unsupported role %q", path, suite.Role)
	}
	seen := make(map[string]bool, len(suite.Cases))
	for _, c := range suite.Cases {
		if c.ID == "" || c.Query == "" || len(c.Gold) == 0 {
			return evalSuite{}, fmt.Errorf("each case needs id, query, and at least one gold page")
		}
		if seen[c.ID] {
			return evalSuite{}, fmt.Errorf("duplicate case id %q", c.ID)
		}
		if _, err := sectionForGold(c.Gold); err != nil {
			return evalSuite{}, fmt.Errorf("%s: %w", c.ID, err)
		}
		seen[c.ID] = true
	}
	return suite, nil
}

func sectionForGold(gold []string) (string, error) {
	section := ""
	for _, path := range gold {
		candidate := ""
		switch {
		case strings.HasPrefix(path, "Manual/"):
			candidate = "Manual"
		case strings.HasPrefix(path, "ScriptReference/"):
			candidate = "Scripting API"
		default:
			return "", fmt.Errorf("unsupported gold path %q", path)
		}
		if section != "" && section != candidate {
			return "", fmt.Errorf("gold pages span both Manual and Scripting API")
		}
		section = candidate
	}
	return section, nil
}

func makeSectionReports(totals, hits map[string]int) []sectionReport {
	sections := []string{"Manual", "Scripting API"}
	reports := make([]sectionReport, 0, len(sections))
	for _, section := range sections {
		count := totals[section]
		if count == 0 {
			continue
		}
		reports = append(reports, sectionReport{
			Section:   section,
			CaseCount: count,
			HitCount:  hits[section],
			Recall:    float64(hits[section]) / float64(count),
		})
	}
	return reports
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

func ftsTerms(query string) []string {
	return retrieval.Terms(query)
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

func summarizePerformance(results []result, policy retrieval.Strategy) performanceReport {
	latencies := make([]float64, len(results))
	performance := performanceReport{Policy: string(policy)}
	for i, result := range results {
		latencies[i] = result.ElapsedMS
		performance.RetrievalQueries += result.RetrievalQueries
		performance.VocabularyLookups += result.VocabularyLookups
	}
	if len(results) == 0 {
		return performance
	}
	sort.Float64s(latencies)
	performance.P50MS = percentile(latencies, 0.50)
	performance.P95MS = percentile(latencies, 0.95)
	performance.QueriesPerCase = float64(performance.RetrievalQueries) / float64(len(results))
	performance.VocabularyPerCase = float64(performance.VocabularyLookups) / float64(len(results))
	return performance
}

func percentile(sorted []float64, percentile float64) float64 {
	if len(sorted) == 0 {
		return 0
	}
	index := int(float64(len(sorted))*percentile+0.999999) - 1
	if index < 0 {
		index = 0
	}
	if index >= len(sorted) {
		index = len(sorted) - 1
	}
	return sorted[index]
}
