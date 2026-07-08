package main

import (
	"bufio"
	"database/sql"
	"encoding/json"
	"flag"
	"fmt"
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
	SourceTime  time.Duration
	DerivedTime time.Duration
	SQLiteTime  time.Duration
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

func sourceRelFromMarkdown(text, path, corpus string) string {
	re := regexp.MustCompile(`(?m)^source_rel:\s*(.+)$`)
	match := re.FindStringSubmatch(text)
	if len(match) == 2 {
		return strings.TrimSpace(match[1])
	}
	rel, _ := filepath.Rel(filepath.Join(corpus, "text"), path)
	return filepath.ToSlash(rel)
}

func loadDerivedDocs(corpus string) ([]doc, error) {
	root := filepath.Join(corpus, "text")
	var docs []doc
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.EqualFold(filepath.Ext(path), ".md") {
			return err
		}
		text, err := readText(path)
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(corpus, path)
		docs = append(docs, doc{SourceRel: sourceRelFromMarkdown(text, path, corpus), DisplayRel: filepath.ToSlash(rel), Text: strings.ToLower(text), Bytes: len(text)})
		return nil
	})
	sort.Slice(docs, func(i, j int) bool { return docs[i].SourceRel < docs[j].SourceRel })
	return docs, err
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
	rows, err := db.Query("SELECT p.source_rel FROM pages_fts JOIN pages p ON p.page_key = pages_fts.page_key WHERE pages_fts MATCH ? ORDER BY bm25(pages_fts) LIMIT 10", strings.Join(terms, " "))
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
	file, err := os.Open(filepath.Join(corpus, "pages.jsonl"))
	if err != nil {
		return nil, err
	}
	defer file.Close()
	var cases []benchCase
	scanner := bufio.NewScanner(file)
	buf := make([]byte, 1024*1024)
	scanner.Buffer(buf, 16*1024*1024)
	for scanner.Scan() && len(cases) < limit {
		var rec struct {
			SourceRel string `json:"source_rel"`
			Title     string `json:"title"`
			PageID    string `json:"page_id"`
		}
		if json.Unmarshal(scanner.Bytes(), &rec) != nil || rec.SourceRel == "" {
			continue
		}
		query := rec.Title
		if len(query) < 4 {
			query = rec.PageID
		}
		if len(query) >= 4 {
			cases = append(cases, benchCase{"generated:" + rec.SourceRel, query, rec.SourceRel, true})
		}
	}
	return cases, scanner.Err()
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
				results[index] = caseResult{
					Name:        c.Name,
					Expected:    c.Expected,
					SourceOK:    containsHit(sourceHits, c.Expected),
					DerivedOK:   containsHit(derivedHits, c.Expected),
					SQLiteOK:    containsString(ftsHits, c.Expected),
					SourceTime:  sourceElapsed,
					DerivedTime: derivedElapsed,
					SQLiteTime:  ftsElapsed,
				}
			}
		}()
	}
	for index := range cases {
		caseCh <- index
	}
	close(caseCh)
	wg.Wait()
	sourceRecall, derivedRecall, sqliteRecall := 0, 0, 0
	var sourceSearch, derivedSearch, sqliteSearch time.Duration
	var missSamples []map[string]any
	for _, result := range results {
		if result.SourceOK {
			sourceRecall++
		}
		if result.DerivedOK {
			derivedRecall++
		}
		if result.SQLiteOK {
			sqliteRecall++
		}
		sourceSearch += result.SourceTime
		derivedSearch += result.DerivedTime
		sqliteSearch += result.SQLiteTime
		if len(missSamples) < 20 && (!result.SourceOK || !result.DerivedOK || !result.SQLiteOK) {
			missSamples = append(missSamples, map[string]any{"name": result.Name, "expected": result.Expected, "source_top10_recall": result.SourceOK, "derived_top10_recall": result.DerivedOK, "sqlite_top10_recall": result.SQLiteOK})
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
		"worker_count":               *workers,
		"timings_ms":                 map[string]float64{"load": float64(loadDuration.Microseconds()) / 1000, "source_search_total": float64(sourceSearch.Microseconds()) / 1000, "derived_search_total": float64(derivedSearch.Microseconds()) / 1000, "sqlite_search_total": float64(sqliteSearch.Microseconds()) / 1000, "total": float64(time.Since(totalStart).Microseconds()) / 1000},
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
