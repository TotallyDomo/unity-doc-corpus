package main

// Content-lossless audit (see AgentWorkspace spec unity-doc-corpus-lossless-audit-spec.md).
//
// The transform from Unity HTML to derived Markdown is deliberately lossy in structure but
// must be lossless in page-unique content. This verb proves that mechanically: it extracts
// each page's visible text with an independent extractor (audit_extract.go), shingles it,
// and asserts that every page-unique shingle also appears in that page's derived Markdown.
//
// Two signals:
//   - Shingle invariant (authoritative): a shingle is "page-unique content" when its corpus
//     document frequency is low (chrome repeats across thousands of pages; real content is
//     locally unique). A run of consecutive page-unique shingles missing from the Markdown
//     is a content-loss flag - the run requirement filters the isolated boundary shingles
//     that mix stripped chrome with kept content. Flagged pages set the nonzero exit.
//   - Ratio outlier (advisory): derived vs reference token ratio, flagged against the
//     section median. Cheap gross-truncation backstop; reported but does not gate the exit.
//
// Known blind spot (accepted 2026-07-09, M0042-S0001): ~1.3% of pages - short ScriptReference
// enum-member pages (KeyCode.*, BuildTargetGroup.*, ...) - flag as false positives. Unity nests
// the footer and feedback form INSIDE #content-wrap; the parser strips them by class, but this
// extractor's minimal 4-class skip list deliberately does not, and on a page whose real content
// is only a few tokens the corpus-uniform chrome sits adjacent to it, so the chrome/content
// boundary shingles are low-DF and clear the run threshold. Frequency+run cannot separate that;
// resolving it would mean adding footer/feedback to the skip list, which was declined to keep the
// list minimal (over-dropping there risks silent false negatives). These are a stable, uniform
// class - not a diverse spray - so the audit stays a reliable NEW-regression detector; a baseline
// allowlist to make it a clean CI gate despite this floor is deferred (S2/S4).

import (
	"bufio"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

type auditConfig struct {
	shingleN    int
	maxDF       uint32
	minRun      int
	ratioFactor float64
	ratioMinTok int
	maxQuotes   int
}

type pageRef struct {
	PageKey   string
	Section   string
	SourceRel string
	MDRel     string
}

type auditFlag struct {
	PageKey       string   `json:"page_key"`
	Section       string   `json:"section"`
	SourceRel     string   `json:"source_rel"`
	MDRel         string   `json:"md_rel"`
	RefTokens     int      `json:"ref_tokens"`
	MDTokens      int      `json:"md_tokens"`
	Ratio         float64  `json:"derived_to_ref_token_ratio"`
	MissingWindow int      `json:"missing_shingle_count"`
	MissingSpans  int      `json:"missing_span_count"`
	Quotes        []string `json:"missing_text_samples"`
	RatioOutlier  bool     `json:"ratio_outlier"`
}

// dfCounter is a sharded fingerprint->document-frequency table. Sharding by the low byte of
// the fingerprint keeps the per-page increment path lock-contention low across workers; it is
// the only corpus-wide structure the audit holds.
type dfCounter struct {
	shards [256]struct {
		mu sync.Mutex
		m  map[uint64]uint32
	}
}

func newDFCounter() *dfCounter {
	c := &dfCounter{}
	for i := range c.shards {
		c.shards[i].m = make(map[uint64]uint32)
	}
	return c
}

func (c *dfCounter) add(fp uint64) {
	s := &c.shards[byte(fp)]
	s.mu.Lock()
	s.m[fp]++
	s.mu.Unlock()
}

func (c *dfCounter) get(fp uint64) uint32 {
	s := &c.shards[byte(fp)]
	s.mu.Lock()
	v := s.m[fp]
	s.mu.Unlock()
	return v
}

func runAudit(args []string) {
	fs := flag.NewFlagSet("audit", flag.ExitOnError)
	source := fs.String("source", defaultFetchDestination, "Unity documentation root holding the extracted Manual/ and ScriptReference/ HTML.")
	corpus := fs.String("corpus", defaultCorpusDir, "Derived corpus directory (the builder's --output).")
	output := fs.String("output", "", "Optional JSON report path.")
	workers := fs.Int("workers", 0, "Worker count. Defaults to half of logical CPUs.")
	shingleN := fs.Int("shingle-n", 5, "Shingle width in words.")
	maxDF := fs.Int("max-shingle-df", 4, "Max corpus document frequency for a shingle to count as page-unique content.")
	minRun := fs.Int("min-run", 0, "Minimum run of consecutive missing page-unique shingles to flag a page; 0 = shingle width n. Tight by construction: a boundary between kept content and a stripped corpus-uniform chrome island produces exactly n-1 missing windows, so losing even one real content token (>= n missing windows) is the smallest thing that clears the bar.")
	ratioFactor := fs.Float64("ratio-factor", 0.4, "Advisory ratio-outlier threshold: flag pages whose derived/reference token ratio is below section median times this.")
	ratioMinTok := fs.Int("ratio-min-tokens", 30, "Skip ratio-outlier checks for pages with fewer reference tokens than this.")
	maxQuotes := fs.Int("max-quotes", 5, "Max quoted missing-text spans to report per flagged page.")
	_ = fs.Parse(args)

	if *shingleN < 1 {
		fmt.Fprintln(os.Stderr, "error: --shingle-n must be >= 1")
		os.Exit(2)
	}
	cfg := auditConfig{
		shingleN:    *shingleN,
		maxDF:       uint32(*maxDF),
		minRun:      *minRun,
		ratioFactor: *ratioFactor,
		ratioMinTok: *ratioMinTok,
		maxQuotes:   *maxQuotes,
	}
	if cfg.minRun < 1 {
		cfg.minRun = cfg.shingleN
	}
	if *workers < 1 {
		*workers = defaultWorkers()
	}

	if err := auditRun(*source, *corpus, *output, *workers, cfg); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func auditRun(source, corpus, output string, workers int, cfg auditConfig) error {
	start := time.Now()
	sourceAbs, _ := filepath.Abs(source)
	corpusAbs, _ := filepath.Abs(corpus)

	for _, section := range sectionDirs {
		if info, err := os.Stat(filepath.Join(sourceAbs, section)); err != nil || !info.IsDir() {
			return fmt.Errorf("missing extracted HTML folder %s\nthe audit reads the original HTML - rematerialize it with:\n  bin/unity-doc-corpus build --source %s --output %s --keep-source",
				filepath.Join(sourceAbs, section), source, corpus)
		}
	}

	pages, err := loadPageRefs(corpusAbs)
	if err != nil {
		return err
	}
	if len(pages) == 0 {
		return fmt.Errorf("no pages found in %s (is the corpus built?)", filepath.Join(corpusAbs, "pages.jsonl"))
	}

	// Pass 1: extract reference visible text for every page, cache it, and fold each page's
	// distinct shingles into the corpus document-frequency table.
	refJoined := make([]string, len(pages))
	df := newDFCounter()
	var skipMu sync.Mutex
	skippedTotals := map[string]int{}
	if err := auditParallel(workers, len(pages), func(i int) error {
		htmlPath := filepath.Join(sourceAbs, filepath.FromSlash(pages[i].SourceRel))
		raw, err := os.ReadFile(htmlPath)
		if err != nil {
			return fmt.Errorf("reading %s: %w", htmlPath, err)
		}
		tokens, skipped := auditExtractTokens(string(raw))
		refJoined[i] = strings.Join(tokens, " ")
		for fp := range distinctShingles(tokens, cfg.shingleN) {
			df.add(fp)
		}
		if len(skipped) > 0 {
			skipMu.Lock()
			for k, v := range skipped {
				skippedTotals[k] += v
			}
			skipMu.Unlock()
		}
		return nil
	}); err != nil {
		return err
	}

	// Pass 2: for every page, check that each page-unique reference shingle is present in the
	// derived Markdown; collect runs of consecutive misses as content-loss flags.
	flags := make([]*auditFlag, len(pages))
	ratios := make([]float64, len(pages))
	refTokCounts := make([]int, len(pages))
	if err := auditParallel(workers, len(pages), func(i int) error {
		mdPath := filepath.Join(corpusAbs, filepath.FromSlash(pages[i].MDRel))
		mdRaw, err := os.ReadFile(mdPath)
		if err != nil {
			return fmt.Errorf("reading %s: %w", mdPath, err)
		}
		mdTokens := auditTokenize(string(mdRaw))
		refTokens := auditTokenize(refJoined[i])
		refTokCounts[i] = len(refTokens)
		if len(refTokens) > 0 {
			ratios[i] = float64(len(mdTokens)) / float64(len(refTokens))
		}
		flags[i] = auditPage(pages[i], refTokens, mdTokens, df, cfg)
		if flags[i] != nil {
			flags[i].RefTokens = len(refTokens)
			flags[i].MDTokens = len(mdTokens)
			if len(refTokens) > 0 {
				flags[i].Ratio = ratios[i]
			}
		}
		return nil
	}); err != nil {
		return err
	}

	// Advisory ratio-outlier pass: per-section median, flag pages far below it.
	markRatioOutliers(pages, ratios, refTokCounts, flags, cfg)

	// Collect flagged pages (content-loss runs) in stable page order.
	var flagged []*auditFlag
	for _, f := range flags {
		if f != nil && (f.MissingSpans > 0 || f.RatioOutlier) {
			flagged = append(flagged, f)
		}
	}
	contentLoss := 0
	ratioOnly := 0
	for _, f := range flagged {
		if f.MissingSpans > 0 {
			contentLoss++
		} else {
			ratioOnly++
		}
	}

	elapsed := time.Since(start)
	printAuditReport(os.Stdout, pages, flagged, contentLoss, ratioOnly, skippedTotals, cfg, elapsed)
	if output != "" {
		if err := writeAuditJSON(output, pages, flagged, contentLoss, ratioOnly, skippedTotals, cfg, elapsed); err != nil {
			return err
		}
		fmt.Fprintln(os.Stderr, "Wrote JSON report:", output)
	}

	// Only content-loss flags gate the exit; ratio outliers are advisory.
	if contentLoss > 0 {
		os.Exit(1)
	}
	return nil
}

// auditPage checks one page and returns a flag if it has a qualifying run of missing
// page-unique shingles, else nil. refTokens/mdTokens are the two token streams.
func auditPage(p pageRef, refTokens, mdTokens []string, df *dfCounter, cfg auditConfig) *auditFlag {
	if len(refTokens) == 0 {
		return nil
	}
	// Short page: the whole token stream is one shingle; the n-gram set does not apply, so
	// check membership as a direct subsequence of the Markdown tokens.
	if len(refTokens) < cfg.shingleN {
		fp := shingleFingerprint(refTokens)
		if df.get(fp) <= cfg.maxDF && !containsSubsequence(mdTokens, refTokens) {
			return &auditFlag{
				PageKey: p.PageKey, Section: p.Section, SourceRel: p.SourceRel, MDRel: p.MDRel,
				MissingWindow: 1, MissingSpans: 1,
				Quotes: []string{clip(strings.Join(refTokens, " "))},
			}
		}
		return nil
	}

	mdSet := distinctShingles(mdTokens, cfg.shingleN)

	// missing[k] is true when the shingle starting at ref token k is page-unique yet absent
	// from the Markdown. Runs of consecutive true values are content-loss spans.
	windowCount := len(refTokens) - cfg.shingleN + 1
	var spans [][2]int // [startTok, endTokExclusive) of each qualifying run
	missingWindows := 0
	runStart := -1
	runLen := 0
	closeRun := func(endWindow int) {
		if runStart >= 0 && runLen >= cfg.minRun {
			spans = append(spans, [2]int{runStart, endWindow + cfg.shingleN - 1})
		}
		runStart = -1
		runLen = 0
	}
	for k := 0; k < windowCount; k++ {
		fp := shingleFingerprint(refTokens[k : k+cfg.shingleN])
		if _, ok := mdSet[fp]; ok || df.get(fp) > cfg.maxDF {
			// Present, or not page-unique: ends any open run.
			closeRun(k - 1)
			continue
		}
		missingWindows++
		if runStart < 0 {
			runStart = k
		}
		runLen++
	}
	closeRun(windowCount - 1)

	if len(spans) == 0 {
		return nil
	}
	quotes := make([]string, 0, cfg.maxQuotes)
	for _, s := range spans {
		if len(quotes) >= cfg.maxQuotes {
			break
		}
		quotes = append(quotes, clip(strings.Join(refTokens[s[0]:s[1]], " ")))
	}
	return &auditFlag{
		PageKey: p.PageKey, Section: p.Section, SourceRel: p.SourceRel, MDRel: p.MDRel,
		MissingWindow: missingWindows, MissingSpans: len(spans), Quotes: quotes,
	}
}

// markRatioOutliers computes the per-section median derived/reference token ratio and flags
// pages that fall well below it. Advisory only: it annotates existing flags or creates a
// ratio-only flag, but never contributes to the content-loss exit code.
func markRatioOutliers(pages []pageRef, ratios []float64, refTok []int, flags []*auditFlag, cfg auditConfig) {
	bySection := map[string][]float64{}
	for i := range pages {
		if refTok[i] >= cfg.ratioMinTok {
			bySection[pages[i].Section] = append(bySection[pages[i].Section], ratios[i])
		}
	}
	medians := map[string]float64{}
	for section, vals := range bySection {
		medians[section] = median(vals)
	}
	for i := range pages {
		if refTok[i] < cfg.ratioMinTok {
			continue
		}
		med, ok := medians[pages[i].Section]
		if !ok || med == 0 {
			continue
		}
		if ratios[i] < med*cfg.ratioFactor {
			if flags[i] == nil {
				flags[i] = &auditFlag{
					PageKey: pages[i].PageKey, Section: pages[i].Section,
					SourceRel: pages[i].SourceRel, MDRel: pages[i].MDRel,
					RefTokens: refTok[i], Ratio: ratios[i],
				}
			}
			flags[i].RatioOutlier = true
		}
	}
}

func median(vals []float64) float64 {
	if len(vals) == 0 {
		return 0
	}
	sorted := append([]float64(nil), vals...)
	sort.Float64s(sorted)
	mid := len(sorted) / 2
	if len(sorted)%2 == 1 {
		return sorted[mid]
	}
	return (sorted[mid-1] + sorted[mid]) / 2
}

// formatSkipped renders the per-class dropped-token tallies in a stable order for the report.
func formatSkipped(skipped map[string]int) string {
	if len(skipped) == 0 {
		return "(none)"
	}
	keys := make([]string, 0, len(skipped))
	for k := range skipped {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, fmt.Sprintf("%s=%d", k, skipped[k]))
	}
	return strings.Join(parts, " ")
}

// clip trims a quoted span to a readable length.
func clip(s string) string {
	const max = 200
	if len(s) <= max {
		return s
	}
	return s[:max] + "..."
}

// loadPageRefs reads the corpus pages.jsonl into the page list the audit iterates.
func loadPageRefs(corpusAbs string) ([]pageRef, error) {
	f, err := os.Open(filepath.Join(corpusAbs, "pages.jsonl"))
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var pages []pageRef
	scanner := bufio.NewScanner(f)
	buf := make([]byte, 1024*1024)
	scanner.Buffer(buf, 16*1024*1024)
	for scanner.Scan() {
		var rec struct {
			PageKey   string `json:"page_key"`
			Section   string `json:"section"`
			SourceRel string `json:"source_rel"`
			MDRel     string `json:"md_rel"`
		}
		if json.Unmarshal(scanner.Bytes(), &rec) != nil || rec.SourceRel == "" || rec.MDRel == "" {
			continue
		}
		pages = append(pages, pageRef{PageKey: rec.PageKey, Section: rec.Section, SourceRel: rec.SourceRel, MDRel: rec.MDRel})
	}
	return pages, scanner.Err()
}

// auditParallel runs fn over indices [0,count) on a fixed worker pool, returning the first
// error any worker reports.
func auditParallel(workers, count int, fn func(i int) error) error {
	jobs := make(chan int)
	var wg sync.WaitGroup
	var mu sync.Mutex
	var firstErr error
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range jobs {
				if err := fn(i); err != nil {
					mu.Lock()
					if firstErr == nil {
						firstErr = err
					}
					mu.Unlock()
				}
			}
		}()
	}
	for i := 0; i < count; i++ {
		jobs <- i
	}
	close(jobs)
	wg.Wait()
	return firstErr
}

func printAuditReport(w *os.File, pages []pageRef, flagged []*auditFlag, contentLoss, ratioOnly int, skipped map[string]int, cfg auditConfig, elapsed time.Duration) {
	fmt.Fprintf(w, "Content-lossless audit: %d pages, %d content-loss flags, %d ratio-only outliers (%.1fs)\n",
		len(pages), contentLoss, ratioOnly, elapsed.Seconds())
	fmt.Fprintf(w, "Config: shingle-n=%d max-shingle-df=%d min-run=%d ratio-factor=%.2f\n",
		cfg.shingleN, cfg.maxDF, cfg.minRun, cfg.ratioFactor)
	fmt.Fprintf(w, "Chrome dropped (ref tokens): %s\n\n", formatSkipped(skipped))
	if len(flagged) == 0 {
		fmt.Fprintln(w, "No pages flagged.")
		return
	}
	for _, f := range flagged {
		kind := "content-loss"
		if f.MissingSpans == 0 {
			kind = "ratio-outlier"
		}
		fmt.Fprintf(w, "[%s] %s\n  source: %s\n  md:     %s\n", kind, f.PageKey, f.SourceRel, f.MDRel)
		fmt.Fprintf(w, "  ref_tokens=%d md_tokens=%d ratio=%.3f", f.RefTokens, f.MDTokens, f.Ratio)
		if f.MissingSpans > 0 {
			fmt.Fprintf(w, " missing_spans=%d missing_shingles=%d", f.MissingSpans, f.MissingWindow)
		}
		if f.RatioOutlier {
			fmt.Fprint(w, " ratio_outlier")
		}
		fmt.Fprintln(w)
		for _, q := range f.Quotes {
			fmt.Fprintf(w, "    missing: %q\n", q)
		}
		fmt.Fprintln(w)
	}
}

func writeAuditJSON(path string, pages []pageRef, flagged []*auditFlag, contentLoss, ratioOnly int, skipped map[string]int, cfg auditConfig, elapsed time.Duration) error {
	report := map[string]any{
		"page_count":              len(pages),
		"content_loss_flag_count": contentLoss,
		"ratio_outlier_count":     ratioOnly,
		"skipped_chrome_tokens":   skipped,
		"config": map[string]any{
			"shingle_n":        cfg.shingleN,
			"max_shingle_df":   cfg.maxDF,
			"min_run":          cfg.minRun,
			"ratio_factor":     cfg.ratioFactor,
			"ratio_min_tokens": cfg.ratioMinTok,
		},
		"elapsed_seconds": elapsed.Seconds(),
		"flagged":         flagged,
	}
	if flagged == nil {
		report["flagged"] = []*auditFlag{}
	}
	data, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	return os.WriteFile(path, data, 0644)
}
