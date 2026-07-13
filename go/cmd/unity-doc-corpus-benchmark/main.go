package main

import (
	"database/sql"
	"encoding/json"
	"flag"
	"fmt"
	"html"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode"

	_ "modernc.org/sqlite"
)

type doc struct {
	SourceRel  string
	DisplayRel string
	Text       string
	Bytes      int
}

type benchCase struct {
	Name      string
	Query     string
	Expected  string
	Generated bool
}

type hit struct {
	SourceRel string
	Score     int
}

type caseResult struct {
	Name        string
	Expected    string
	SourceOK    bool
	DerivedOK   bool
	SQLiteOK    bool
	RawFTSOK    bool
	SourceTime  time.Duration
	DerivedTime time.Duration
	SQLiteTime  time.Duration
	RawFTSTime  time.Duration
}

var defaultCases = []benchCase{
	{"Rigidbody.MovePosition API", "Rigidbody.MovePosition moves rigidbody position", "ScriptReference/Rigidbody.MovePosition.html", false},
	{"DestroyImmediate API", "Object.DestroyImmediate destroys object immediately edit mode", "ScriptReference/Object.DestroyImmediate.html", false},
	{"PrefabUtility instantiate", "PrefabUtility.InstantiatePrefab instantiate prefab asset", "ScriptReference/PrefabUtility.InstantiatePrefab.html", false},
	{"BuildPipeline.BuildPlayer API", "BuildPipeline.BuildPlayer build player options scenes locationPathName", "ScriptReference/BuildPipeline.BuildPlayer.html", false},
	{"Dynamic Resolution manual", "dynamic resolution supported platforms render targets", "Manual/DynamicResolution-introduction.html", false},
	{"Script execution order manual", "script execution order event functions update fixedupdate awake", "Manual/execution-order.html", false},
	{"YAML class ID reference", "YAML class ID reference MonoBehaviour GameObject", "Manual/ClassIDReference.html", false},
	{"Coroutines manual", "coroutines yield return WaitForSeconds", "Manual/Coroutines.html", false},
}

func defaultWorkers() int {
	workers := runtime.NumCPU() / 2
	if workers < 1 {
		return 1
	}
	return workers
}

func readText(path string) (string, error) {
	data, err := os.ReadFile(path)
	return string(data), err
}

func tokenize(query string, fts bool) []string {
	var terms []string
	var b strings.Builder
	flush := func() {
		if b.Len() > 1 {
			terms = append(terms, b.String())
		}
		b.Reset()
	}
	for _, r := range query {
		if unicode.IsLetter(r) || unicode.IsDigit(r) || (!fts && (r == '_' || r == '.' || r == '-')) {
			b.WriteRune(unicode.ToLower(r))
		} else {
			flush()
		}
	}
	flush()
	return terms
}

func scoreText(text string, terms []string) int {
	score := 0
	for _, term := range terms {
		count := strings.Count(text, term)
		if count == 0 {
			return 0
		}
		score += count
	}
	return score
}

func loadSourceDocs(source string) ([]doc, error) {
	var docs []doc
	for _, section := range []string{"Manual", "ScriptReference"} {
		root := filepath.Join(source, section)
		err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() || !strings.EqualFold(filepath.Ext(path), ".html") {
				return err
			}
			text, err := readText(path)
			if err != nil {
				return err
			}
			rel, _ := filepath.Rel(source, path)
			sourceRel := filepath.ToSlash(rel)
			docs = append(docs, doc{SourceRel: sourceRel, DisplayRel: sourceRel, Text: strings.ToLower(text), Bytes: len(text)})
			return nil
		})
		if err != nil {
			return nil, err
		}
	}
	sort.Slice(docs, func(i, j int) bool { return docs[i].SourceRel < docs[j].SourceRel })
	return docs, nil
}

// loadDerivedDocs reads the derived Markdown for the naive-scan-over-derived lane from the
// corpus docs.sqlite (the page_text table joined to pages for the source path) - the DB read
// that replaced walking a text/ directory of .md files. The Markdown is the byte-identical
// writeMarkdown output, so recall is unchanged from the file-backed lane.
func loadDerivedDocs(corpus string) ([]doc, error) {
	db, err := sql.Open("sqlite", filepath.Join(corpus, "docs.sqlite"))
	if err != nil {
		return nil, err
	}
	defer db.Close()
	rows, err := db.Query("SELECT p.source_rel, pt.md FROM page_text pt JOIN pages p ON p.page_key = pt.page_key")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var docs []doc
	for rows.Next() {
		var sourceRel, md string
		if err := rows.Scan(&sourceRel, &md); err != nil {
			return nil, err
		}
		docs = append(docs, doc{SourceRel: sourceRel, DisplayRel: sourceRel, Text: strings.ToLower(md), Bytes: len(md)})
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	sort.Slice(docs, func(i, j int) bool { return docs[i].SourceRel < docs[j].SourceRel })
	return docs, nil
}

func searchDocs(docs []doc, terms []string) ([]hit, time.Duration, int) {
	start := time.Now()
	var hits []hit
	bytes := 0
	for _, doc := range docs {
		bytes += doc.Bytes
		if score := scoreText(doc.Text, terms); score > 0 {
			hits = append(hits, hit{doc.SourceRel, score})
		}
	}
	sort.Slice(hits, func(i, j int) bool {
		if hits[i].Score != hits[j].Score {
			return hits[i].Score > hits[j].Score
		}
		return hits[i].SourceRel < hits[j].SourceRel
	})
	if len(hits) > 10 {
		hits = hits[:10]
	}
	return hits, time.Since(start), bytes
}

func sqliteHits(corpus, query string) ([]string, time.Duration) {
	start := time.Now()
	db, err := sql.Open("sqlite", filepath.Join(corpus, "docs.sqlite"))
	if err != nil {
		return nil, time.Since(start)
	}
	defer db.Close()
	terms := tokenize(query, true)
	// pages_fts is contentless (M51-S2): join on rowid (pinned equal to the pages rowid at
	// build), not the non-retrievable page_key column.
	rows, err := db.Query("SELECT p.source_rel FROM pages_fts JOIN pages p ON p.rowid = pages_fts.rowid WHERE pages_fts MATCH ? ORDER BY bm25(pages_fts, 0.0, 10.0, 1.0) LIMIT 10", strings.Join(terms, " "))
	if err != nil {
		return nil, time.Since(start)
	}
	defer rows.Close()
	var hits []string
	for rows.Next() {
		var sourceRel string
		if rows.Scan(&sourceRel) == nil {
			hits = append(hits, sourceRel)
		}
	}
	return hits, time.Since(start)
}

var titleRe = regexp.MustCompile(`(?is)<title>(.*?)</title>`)

func extractHTMLTitle(text string) string {
	m := titleRe.FindStringSubmatch(text)
	if m == nil {
		return ""
	}
	return strings.Join(strings.Fields(html.UnescapeString(m[1])), " ")
}

// buildRawFTSIndex indexes the raw, untransformed HTML into a throwaway FTS5 database:
// the (bm25, raw HTML) cell of the ranker x representation matrix. Ranker settings are
// identical to the shipped corpus lane so the two differ only in representation.
func buildRawFTSIndex(docs []doc) (string, time.Duration, error) {
	start := time.Now()
	f, err := os.CreateTemp("", "unity-raw-fts-*.sqlite")
	if err != nil {
		return "", 0, err
	}
	path := f.Name()
	f.Close()
	os.Remove(path)
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return "", 0, err
	}
	defer db.Close()
	if _, err := db.Exec("CREATE VIRTUAL TABLE raw_fts USING fts5(source_rel UNINDEXED, title, body)"); err != nil {
		return "", 0, err
	}
	tx, err := db.Begin()
	if err != nil {
		return "", 0, err
	}
	stmt, err := tx.Prepare("INSERT INTO raw_fts(source_rel, title, body) VALUES (?, ?, ?)")
	if err != nil {
		return "", 0, err
	}
	defer stmt.Close()
	for _, d := range docs {
		if _, err := stmt.Exec(d.SourceRel, extractHTMLTitle(d.Text), d.Text); err != nil {
			return "", 0, err
		}
	}
	if err := tx.Commit(); err != nil {
		return "", 0, err
	}
	return path, time.Since(start), nil
}

func rawFTSHits(path, query string) ([]string, time.Duration) {
	start := time.Now()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, time.Since(start)
	}
	defer db.Close()
	terms := tokenize(query, true)
	rows, err := db.Query("SELECT source_rel FROM raw_fts WHERE raw_fts MATCH ? ORDER BY bm25(raw_fts, 0.0, 10.0, 1.0) LIMIT 10", strings.Join(terms, " "))
	if err != nil {
		return nil, time.Since(start)
	}
	defer rows.Close()
	var hits []string
	for rows.Next() {
		var sourceRel string
		if rows.Scan(&sourceRel) == nil {
			hits = append(hits, sourceRel)
		}
	}
	return hits, time.Since(start)
}

func generatedCases(corpus string, limit int) ([]benchCase, error) {
	db, err := sql.Open("sqlite", filepath.Join(corpus, "docs.sqlite"))
	if err != nil {
		return nil, err
	}
	defer db.Close()
	// Preserve the builder's original pages.jsonl order: Manual first, then ScriptReference,
	// with each section sorted by source path. Generated cases use stride sampling, so changing
	// this order would change the reference corpus's fixed case set without changing retrieval.
	rows, err := db.Query("SELECT source_rel, title, page_id FROM pages ORDER BY section, source_rel")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var eligible []benchCase
	for rows.Next() {
		var sourceRel, title, pageID string
		if err := rows.Scan(&sourceRel, &title, &pageID); err != nil {
			return nil, err
		}
		if sourceRel == "" {
			return nil, fmt.Errorf("pages table contains a row with no source_rel")
		}
		query := title
		if len(query) < 4 {
			query = pageID
		}
		if len(query) >= 4 {
			eligible = append(eligible, benchCase{"generated:" + sourceRel, query, sourceRel, true})
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if limit <= 0 || len(eligible) <= limit {
		return eligible, nil
	}
	stride := len(eligible) / limit
	var cases []benchCase
	for i := 0; i < len(eligible) && len(cases) < limit; i += stride {
		cases = append(cases, eligible[i])
	}
	return cases, nil
}

func containsHit(hits []hit, expected string) bool {
	for _, hit := range hits {
		if hit.SourceRel == expected {
			return true
		}
	}
	return false
}

func containsString(values []string, expected string) bool {
	for _, value := range values {
		if value == expected {
			return true
		}
	}
	return false
}

func main() {
	source := flag.String("source", "", "Unity documentation root.")
	corpus := flag.String("corpus", "", "Agent corpus root.")
	output := flag.String("output", "", "Optional JSON report path.")
	generatedCount := flag.Int("generated-cases", 1000, "Generated title/page-id cases.")
	workers := flag.Int("workers", 0, "Worker count for benchmark cases. Defaults to half of logical CPUs.")
	flag.Parse()
	if *source == "" || *corpus == "" {
		fmt.Fprintln(os.Stderr, "Usage: unity-doc-corpus-benchmark --source <docs-root> --corpus <agent-corpus> [--output report.json]")
		os.Exit(2)
	}
	totalStart := time.Now()
	loadStart := time.Now()
	sourceDocs, err := loadSourceDocs(*source)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
	derivedDocs, err := loadDerivedDocs(*corpus)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
	loadDuration := time.Since(loadStart)
	fmt.Fprintln(os.Stderr, "Indexing raw HTML into the FTS5 baseline (one-time, several minutes on a full corpus)...")
	rawFTSPath, rawFTSBuild, err := buildRawFTSIndex(sourceDocs)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
	defer os.Remove(rawFTSPath)
	cases := append([]benchCase{}, defaultCases...)
	generated, err := generatedCases(*corpus, *generatedCount)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
	cases = append(cases, generated...)
	sourceBytes, derivedBytes := 0, 0
	for _, doc := range sourceDocs {
		sourceBytes += doc.Bytes
	}
	for _, doc := range derivedDocs {
		derivedBytes += doc.Bytes
	}
	if *workers < 1 {
		*workers = defaultWorkers()
	}
	results := make([]caseResult, len(cases))
	caseCh := make(chan int)
	var wg sync.WaitGroup
	for i := 0; i < *workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for index := range caseCh {
				c := cases[index]
				terms := tokenize(c.Query, false)
				sourceHits, sourceElapsed, _ := searchDocs(sourceDocs, terms)
				derivedHits, derivedElapsed, _ := searchDocs(derivedDocs, terms)
				ftsHits, ftsElapsed := sqliteHits(*corpus, c.Query)
				rawHits, rawElapsed := rawFTSHits(rawFTSPath, c.Query)
				results[index] = caseResult{
					Name:        c.Name,
					Expected:    c.Expected,
					SourceOK:    containsHit(sourceHits, c.Expected),
					DerivedOK:   containsHit(derivedHits, c.Expected),
					SQLiteOK:    containsString(ftsHits, c.Expected),
					RawFTSOK:    containsString(rawHits, c.Expected),
					SourceTime:  sourceElapsed,
					DerivedTime: derivedElapsed,
					SQLiteTime:  ftsElapsed,
					RawFTSTime:  rawElapsed,
				}
			}
		}()
	}
	for index := range cases {
		caseCh <- index
	}
	close(caseCh)
	wg.Wait()
	sourceRecall, derivedRecall, sqliteRecall, rawFTSRecall := 0, 0, 0, 0
	var sourceSearch, derivedSearch, sqliteSearch, rawFTSSearch time.Duration
	var missSamples []map[string]any
	sectionStats := map[string]map[string]int{}
	for _, result := range results {
		section := "ScriptReference"
		if strings.HasPrefix(result.Expected, "Manual/") {
			section = "Manual"
		}
		stats := sectionStats[section]
		if stats == nil {
			stats = map[string]int{}
			sectionStats[section] = stats
		}
		stats["cases"]++
		if result.SourceOK {
			sourceRecall++
			stats["source_top10_recall_count"]++
		}
		if result.DerivedOK {
			derivedRecall++
			stats["derived_top10_recall_count"]++
		}
		if result.SQLiteOK {
			sqliteRecall++
			stats["sqlite_top10_recall_count"]++
		}
		if result.RawFTSOK {
			rawFTSRecall++
			stats["raw_fts_top10_recall_count"]++
		}
		sourceSearch += result.SourceTime
		derivedSearch += result.DerivedTime
		sqliteSearch += result.SQLiteTime
		rawFTSSearch += result.RawFTSTime
		if len(missSamples) < 20 && (!result.SourceOK || !result.DerivedOK || !result.SQLiteOK || !result.RawFTSOK) {
			missSamples = append(missSamples, map[string]any{"name": result.Name, "expected": result.Expected, "source_top10_recall": result.SourceOK, "derived_top10_recall": result.DerivedOK, "sqlite_top10_recall": result.SQLiteOK, "raw_fts_top10_recall": result.RawFTSOK})
		}
	}
	report := map[string]any{
		"source":                     source,
		"corpus":                     corpus,
		"source_file_count":          len(sourceDocs),
		"derived_file_count":         len(derivedDocs),
		"case_count":                 len(cases),
		"default_case_count":         len(defaultCases),
		"generated_case_count":       len(generated),
		"source_html_bytes":          sourceBytes,
		"derived_markdown_bytes":     derivedBytes,
		"derived_to_source_ratio":    float64(derivedBytes) / float64(sourceBytes),
		"source_top10_recall_count":  sourceRecall,
		"derived_top10_recall_count": derivedRecall,
		"sqlite_top10_recall_count":  sqliteRecall,
		"raw_fts_top10_recall_count": rawFTSRecall,
		"recall_by_section":          sectionStats,
		"worker_count":               *workers,
		"timings_ms":                 map[string]float64{"load": float64(loadDuration.Microseconds()) / 1000, "raw_fts_index_build": float64(rawFTSBuild.Microseconds()) / 1000, "source_search_total": float64(sourceSearch.Microseconds()) / 1000, "derived_search_total": float64(derivedSearch.Microseconds()) / 1000, "sqlite_search_total": float64(sqliteSearch.Microseconds()) / 1000, "raw_fts_search_total": float64(rawFTSSearch.Microseconds()) / 1000, "total": float64(time.Since(totalStart).Microseconds()) / 1000},
		"miss_samples":               missSamples,
	}
	data, _ := json.MarshalIndent(report, "", "  ")
	data = append(data, '\n')
	if *output != "" {
		if err := os.WriteFile(*output, data, 0644); err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			os.Exit(1)
		}
	}
	os.Stdout.Write(data)
}
