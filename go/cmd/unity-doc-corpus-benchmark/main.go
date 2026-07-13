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
	"unity-doc-corpus/retrieval"

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

type benchmarkMode string

const (
	recallMode     benchmarkMode = "recall"
	comparisonMode benchmarkMode = "comparison"

	defaultRecallGeneratedCases  = 1000
	extendedRecallGeneratedCases = 10000
	comparisonGeneratedCases     = 1000

	// referenceBaseline is the frozen title-derived result that storage and retrieval
	// changes compare against. The default recall tier deliberately preserves its case set.
	referenceBaseline = "docs/benchmark-6000.3.json"
)

type hit struct {
	SourceRel string
	Score     int
}

type caseResult struct {
	Name              string
	Expected          string
	SourceOK          bool
	DerivedOK         bool
	SQLiteOK          bool
	SQLiteQueries     int
	VocabularyLookups int
	RawFTSOK          bool
	SourceTime        time.Duration
	DerivedTime       time.Duration
	SQLiteTime        time.Duration
	RawFTSTime        time.Duration
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

func sqliteHits(corpus, query string, strategy retrieval.Strategy) ([]string, time.Duration, retrieval.Stats) {
	start := time.Now()
	db, err := sql.Open("sqlite", filepath.Join(corpus, "docs.sqlite"))
	if err != nil {
		return nil, time.Since(start), retrieval.Stats{}
	}
	defer db.Close()
	hits, stats, err := retrieval.New(db).Search(query, 10, strategy)
	if err != nil {
		return nil, time.Since(start), stats
	}
	paths := make([]string, len(hits))
	for i, hit := range hits {
		paths[i] = hit.SourceRel
	}
	return paths, time.Since(start), stats
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

// markdownBody drops generated front matter and the synthetic link appendix before selecting a
// body-derived query. Neither is document prose an agent would use to express an information
// need.
func markdownBody(md string) string {
	const delimiter = "---\n"
	if !strings.HasPrefix(md, delimiter) {
		return strings.TrimSpace(md)
	}
	if end := strings.Index(md[len(delimiter):], delimiter); end >= 0 {
		body := md[len(delimiter)+end+len(delimiter):]
		if links := strings.Index(body, "\n## Content Links"); links >= 0 {
			body = body[:links]
		}
		return strings.TrimSpace(body)
	}
	return strings.TrimSpace(md)
}

// bodyQuery returns a short, contiguous body snippet made only from terms that do not occur in
// the page title or page id. It selects the most specific snippet by corpus document frequency,
// avoiding boilerplate openings such as "this page describes" when a later concept phrase is
// available. This keeps the extended tier independent of title weighting and of generated links.
func bodyQuery(md, title, pageID string, termDocFrequency map[string]int) string {
	excluded := map[string]bool{}
	for _, term := range tokenize(title+" "+pageID, true) {
		excluded[term] = true
	}
	terms := tokenize(markdownBody(md), true)
	bestStart, bestScore := -1, -1
	for start := 0; start+4 <= len(terms); start++ {
		score := 0
		for _, term := range terms[start : start+4] {
			if excluded[term] {
				score = -1
				break
			}
			frequency := termDocFrequency[term]
			if frequency < 1 {
				frequency = 1
			}
			score += 1_000_000 / frequency
		}
		if score > bestScore {
			bestStart, bestScore = start, score
		}
	}
	if bestStart < 0 {
		return ""
	}
	return strings.Join(terms[bestStart:bestStart+4], " ")
}

// generatedBodyCases is the harder, non-title-derived distribution for the fixed extended
// tier. Unlike the legacy title sample, it uses even index mapping so a 10k run spans the whole
// corpus rather than stopping after the first len(eligible)/limit stride range.
func generatedBodyCases(corpus string, limit int) ([]benchCase, error) {
	type candidate struct {
		sourceRel string
		title     string
		pageID    string
		md        string
	}
	db, err := sql.Open("sqlite", filepath.Join(corpus, "docs.sqlite"))
	if err != nil {
		return nil, err
	}
	defer db.Close()
	rows, err := db.Query("SELECT p.source_rel, p.title, p.page_id, pt.md FROM pages p JOIN page_text pt ON pt.page_key = p.page_key ORDER BY p.section, p.source_rel")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var candidates []candidate
	termDocFrequency := map[string]int{}
	for rows.Next() {
		var sourceRel, title, pageID, md string
		if err := rows.Scan(&sourceRel, &title, &pageID, &md); err != nil {
			return nil, err
		}
		if sourceRel == "" {
			return nil, fmt.Errorf("pages table contains a row with no source_rel")
		}
		seen := map[string]bool{}
		for _, term := range tokenize(markdownBody(md), true) {
			seen[term] = true
		}
		for term := range seen {
			termDocFrequency[term]++
		}
		candidates = append(candidates, candidate{sourceRel: sourceRel, title: title, pageID: pageID, md: md})
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	var eligible []benchCase
	for _, candidate := range candidates {
		if query := bodyQuery(candidate.md, candidate.title, candidate.pageID, termDocFrequency); query != "" {
			eligible = append(eligible, benchCase{"generated-body:" + candidate.sourceRel, query, candidate.sourceRel, true})
		}
	}
	if limit <= 0 || len(eligible) <= limit {
		return eligible, nil
	}
	cases := make([]benchCase, 0, limit)
	for i := 0; i < limit; i++ {
		cases = append(cases, eligible[i*len(eligible)/limit])
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

func sqlitePercentile(results []caseResult, percentile float64) float64 {
	if len(results) == 0 {
		return 0
	}
	latencies := make([]float64, len(results))
	for i, result := range results {
		latencies[i] = float64(result.SQLiteTime.Microseconds()) / 1000
	}
	sort.Float64s(latencies)
	index := int(float64(len(latencies))*percentile+0.999999) - 1
	if index < 0 {
		index = 0
	}
	if index >= len(latencies) {
		index = len(latencies) - 1
	}
	return latencies[index]
}

func main() {
	source := flag.String("source", "", "Unity documentation root.")
	corpus := flag.String("corpus", "", "Agent corpus root.")
	output := flag.String("output", "", "Optional JSON report path.")
	extended := flag.Bool("extended", false, "Run the fixed 10k body-snippet recall tier.")
	comparison := flag.Bool("comparison", false, "Run the fixed 1k title-derived, four-lane FTS-vs-grep comparison.")
	legacyGeneratedCount := flag.Int("generated-cases", 0, "Deprecated compatibility flag; only 1000 is accepted. Use --extended for the fixed 10k tier.")
	workers := flag.Int("workers", 0, "Worker count for benchmark cases. Defaults to half of logical CPUs.")
	policy := flag.String("policy", "safe-fill", "Retrieval policy: exact, safe-fill, or fused.")
	flag.Parse()
	if *corpus == "" {
		fmt.Fprintln(os.Stderr, "Usage: unity-doc-corpus-benchmark --corpus <agent-corpus> [--extended | --comparison --source <docs-root>] [--output report.json]")
		os.Exit(2)
	}
	strategy, err := retrieval.ParseStrategy(*policy)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(2)
	}
	if *extended && *comparison {
		fmt.Fprintln(os.Stderr, "error: --extended and --comparison are mutually exclusive")
		os.Exit(2)
	}
	if *legacyGeneratedCount != 0 && *legacyGeneratedCount != defaultRecallGeneratedCases {
		fmt.Fprintf(os.Stderr, "error: --generated-cases only accepts %d for compatibility; use --extended for the fixed %d-case tier\n", defaultRecallGeneratedCases, extendedRecallGeneratedCases)
		os.Exit(2)
	}
	if *comparison && *source == "" {
		fmt.Fprintln(os.Stderr, "error: --comparison requires --source <docs-root> for the raw and derived scan lanes")
		os.Exit(2)
	}
	totalStart := time.Now()
	mode := recallMode
	distribution := "title-derived"
	generatedLimit := defaultRecallGeneratedCases
	if *extended {
		distribution = "body-snippet"
		generatedLimit = extendedRecallGeneratedCases
	}
	if *comparison {
		mode = comparisonMode
		generatedLimit = comparisonGeneratedCases
	}
	cases := append([]benchCase{}, defaultCases...)
	var generated []benchCase
	if *extended {
		generated, err = generatedBodyCases(*corpus, generatedLimit)
	} else {
		// Keep the title sample's legacy stride selection unchanged: this is the frozen
		// case set recorded in referenceBaseline, not a new distribution to tune against.
		generated, err = generatedCases(*corpus, generatedLimit)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
	cases = append(cases, generated...)

	var sourceDocs, derivedDocs []doc
	var loadDuration, rawFTSBuild time.Duration
	rawFTSPath := ""
	if mode == comparisonMode {
		loadStart := time.Now()
		sourceDocs, err = loadSourceDocs(*source)
		if err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			os.Exit(1)
		}
		derivedDocs, err = loadDerivedDocs(*corpus)
		if err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			os.Exit(1)
		}
		loadDuration = time.Since(loadStart)
		fmt.Fprintln(os.Stderr, "Indexing raw HTML into the FTS5 baseline (one-time, several minutes on a full corpus)...")
		rawFTSPath, rawFTSBuild, err = buildRawFTSIndex(sourceDocs)
		if err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			os.Exit(1)
		}
		defer os.Remove(rawFTSPath)
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
				ftsHits, ftsElapsed, ftsStats := sqliteHits(*corpus, c.Query, strategy)
				result := caseResult{Name: c.Name, Expected: c.Expected, SQLiteOK: containsString(ftsHits, c.Expected), SQLiteQueries: ftsStats.QueryCount, VocabularyLookups: ftsStats.VocabularyLookup, SQLiteTime: ftsElapsed}
				if mode == comparisonMode {
					terms := tokenize(c.Query, false)
					sourceHits, sourceElapsed, _ := searchDocs(sourceDocs, terms)
					derivedHits, derivedElapsed, _ := searchDocs(derivedDocs, terms)
					rawHits, rawElapsed := rawFTSHits(rawFTSPath, c.Query)
					result.SourceOK = containsHit(sourceHits, c.Expected)
					result.DerivedOK = containsHit(derivedHits, c.Expected)
					result.RawFTSOK = containsString(rawHits, c.Expected)
					result.SourceTime = sourceElapsed
					result.DerivedTime = derivedElapsed
					result.RawFTSTime = rawElapsed
				}
				results[index] = result
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
	sqliteQueries, vocabularyLookups := 0, 0
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
		if result.SQLiteOK {
			sqliteRecall++
			stats["sqlite_top10_recall_count"]++
		}
		sqliteSearch += result.SQLiteTime
		sqliteQueries += result.SQLiteQueries
		vocabularyLookups += result.VocabularyLookups
		if mode == comparisonMode {
			if result.SourceOK {
				sourceRecall++
				stats["source_top10_recall_count"]++
			}
			if result.DerivedOK {
				derivedRecall++
				stats["derived_top10_recall_count"]++
			}
			if result.RawFTSOK {
				rawFTSRecall++
				stats["raw_fts_top10_recall_count"]++
			}
			sourceSearch += result.SourceTime
			derivedSearch += result.DerivedTime
			rawFTSSearch += result.RawFTSTime
		}
		miss := !result.SQLiteOK
		if mode == comparisonMode {
			miss = miss || !result.SourceOK || !result.DerivedOK || !result.RawFTSOK
		}
		if len(missSamples) < 20 && miss {
			sample := map[string]any{"name": result.Name, "expected": result.Expected, "sqlite_top10_recall": result.SQLiteOK}
			if mode == comparisonMode {
				sample["source_top10_recall"] = result.SourceOK
				sample["derived_top10_recall"] = result.DerivedOK
				sample["raw_fts_top10_recall"] = result.RawFTSOK
			}
			missSamples = append(missSamples, sample)
		}
	}
	timings := map[string]float64{
		"sqlite_search_total": float64(sqliteSearch.Microseconds()) / 1000,
		"sqlite_search_p50":   sqlitePercentile(results, 0.50),
		"sqlite_search_p95":   sqlitePercentile(results, 0.95),
		"total":               float64(time.Since(totalStart).Microseconds()) / 1000,
	}
	report := map[string]any{
		"mode":                        mode,
		"case_distribution":           distribution,
		"reference_baseline":          referenceBaseline,
		"corpus":                      *corpus,
		"case_count":                  len(cases),
		"default_case_count":          len(defaultCases),
		"generated_case_count":        len(generated),
		"sqlite_top10_recall_count":   sqliteRecall,
		"retrieval_policy":            string(strategy),
		"sqlite_fts_query_count":      sqliteQueries,
		"sqlite_fts_queries_per_case": float64(sqliteQueries) / float64(len(results)),
		"vocabulary_lookup_count":     vocabularyLookups,
		"vocabulary_lookups_per_case": float64(vocabularyLookups) / float64(len(results)),
		"recall_by_section":           sectionStats,
		"worker_count":                *workers,
		"timings_ms":                  timings,
		"miss_samples":                missSamples,
	}
	if mode == comparisonMode {
		sourceBytes, derivedBytes := 0, 0
		for _, doc := range sourceDocs {
			sourceBytes += doc.Bytes
		}
		for _, doc := range derivedDocs {
			derivedBytes += doc.Bytes
		}
		timings["load"] = float64(loadDuration.Microseconds()) / 1000
		timings["raw_fts_index_build"] = float64(rawFTSBuild.Microseconds()) / 1000
		timings["source_search_total"] = float64(sourceSearch.Microseconds()) / 1000
		timings["derived_search_total"] = float64(derivedSearch.Microseconds()) / 1000
		timings["raw_fts_search_total"] = float64(rawFTSSearch.Microseconds()) / 1000
		report["source"] = *source
		report["source_file_count"] = len(sourceDocs)
		report["derived_file_count"] = len(derivedDocs)
		report["source_html_bytes"] = sourceBytes
		report["derived_markdown_bytes"] = derivedBytes
		report["derived_to_source_ratio"] = float64(derivedBytes) / float64(sourceBytes)
		report["source_top10_recall_count"] = sourceRecall
		report["derived_top10_recall_count"] = derivedRecall
		report["raw_fts_top10_recall_count"] = rawFTSRecall
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
