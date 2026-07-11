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
// class - not a diverse spray - so the audit stays a reliable NEW-regression detector. The
// --baseline allowlist (M0042-S0002) turns that floor into a clean CI gate: capture the accepted
// flag set once with --write-baseline, then gate on new flags only - exit is zero iff every
// content-loss flag's page is in the baseline, and stale baseline entries (pages that no longer
// flag) are reported so the floor can only shrink visibly, never grow silently.

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
	Baselined     bool     `json:"baselined"`
}

// auditResult is one full-corpus audit pass, separated from reporting and exit-code policy so
// tests can run the audit end-to-end (seeded-regression proof) without triggering os.Exit.
type auditResult struct {
	pages       []pageRef
	flagged     []*auditFlag
	contentLoss int
	ratioOnly   int
	skipped     map[string]int
	elapsed     time.Duration
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
	baseline := fs.String("baseline", "", "Baseline allowlist JSON of accepted content-loss page keys. With it, exit is zero iff every content-loss flag is baselined; only NEW flags gate.")
	writeBaseline := fs.String("write-baseline", "", "Write this run's content-loss page keys as a baseline allowlist JSON and exit zero (explicit acceptance of the current flag set).")
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

	res, err := auditCorpus(*source, *corpus, *workers, cfg)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}

	var base *auditBaseline
	if *baseline != "" {
		base, err = loadAuditBaseline(*baseline)
		if err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			os.Exit(1)
		}
	}
	newFlags, stale := applyAuditBaseline(res.flagged, base)

	printAuditReport(os.Stdout, res, base, newFlags, stale, cfg)
	if *output != "" {
		if err := writeAuditJSON(*output, res, base, newFlags, stale, cfg); err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			os.Exit(1)
		}
		fmt.Fprintln(os.Stderr, "Wrote JSON report:", *output)
	}
	if *writeBaseline != "" {
		if err := writeAuditBaseline(*writeBaseline, res.flagged); err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			os.Exit(1)
		}
		fmt.Fprintln(os.Stderr, "Wrote baseline:", *writeBaseline)
		// Writing a baseline IS the acceptance of the current flag set: exit zero.
		return
	}
	// Only content-loss flags outside the baseline gate the exit; ratio outliers are advisory.
	if newFlags > 0 {
		os.Exit(1)
	}
}

func auditCorpus(source, corpus string, workers int, cfg auditConfig) (*auditResult, error) {
	start := time.Now()
	sourceAbs, _ := filepath.Abs(source)
	corpusAbs, _ := filepath.Abs(corpus)

	for _, section := range sectionDirs {
		if info, err := os.Stat(filepath.Join(sourceAbs, section)); err != nil || !info.IsDir() {
			return nil, fmt.Errorf("missing extracted HTML folder %s\nthe audit reads the original HTML - rematerialize it with:\n  bin/unity-doc-corpus build --source %s --output %s --keep-source",
				filepath.Join(sourceAbs, section), source, corpus)
		}
	}

	pages, err := loadPageRefs(corpusAbs)
	if err != nil {
		return nil, err
	}
	if len(pages) == 0 {
		return nil, fmt.Errorf("no pages found in %s (is the corpus built?)", filepath.Join(corpusAbs, "pages.jsonl"))
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
		return nil, err
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
		return nil, err
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

	return &auditResult{
		pages:       pages,
		flagged:     flagged,
		contentLoss: contentLoss,
		ratioOnly:   ratioOnly,
		skipped:     skippedTotals,
		elapsed:     time.Since(start),
	}, nil
}

// auditBaselineDescription is written into every baseline file so the file explains itself.
const auditBaselineDescription = "Accepted content-loss flags for the unity-doc-corpus audit " +
	"(known false-positive classes, triaged per the M0042 spec). audit --baseline exits zero " +
	"iff every content-loss flag's page_key is listed here; regenerate with --write-baseline " +
	"only after a human triages the change."

type auditBaseline struct {
	Description string   `json:"description"`
	PageKeys    []string `json:"page_keys"`
}

func loadAuditBaseline(path string) (*auditBaseline, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var b auditBaseline
	if err := json.Unmarshal(data, &b); err != nil {
		return nil, fmt.Errorf("parsing baseline %s: %w", path, err)
	}
	return &b, nil
}

// writeAuditBaseline captures the run's content-loss page keys, sorted for a stable diff.
func writeAuditBaseline(path string, flagged []*auditFlag) error {
	keys := make([]string, 0, len(flagged))
	for _, f := range flagged {
		if f.MissingSpans > 0 {
			keys = append(keys, f.PageKey)
		}
	}
	sort.Strings(keys)
	data, err := json.MarshalIndent(auditBaseline{Description: auditBaselineDescription, PageKeys: keys}, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(data, '\n'), 0644)
}

// applyAuditBaseline marks content-loss flags listed in the baseline, returning the count of
// NEW (unbaselined) content-loss flags and of stale baseline entries (listed pages that no
// longer flag; drop them from the file on the next triaged --write-baseline). A nil baseline
// leaves every content-loss flag new.
func applyAuditBaseline(flagged []*auditFlag, base *auditBaseline) (newFlags, stale int) {
	known := map[string]bool{}
	if base != nil {
		for _, k := range base.PageKeys {
			known[k] = true
		}
	}
	seen := map[string]bool{}
	for _, f := range flagged {
		if f.MissingSpans == 0 {
			continue
		}
		if known[f.PageKey] {
			f.Baselined = true
			seen[f.PageKey] = true
		} else {
			newFlags++
		}
	}
	for k := range known {
		if !seen[k] {
			stale++
		}
	}
	return newFlags, stale
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

func printAuditReport(w *os.File, res *auditResult, base *auditBaseline, newFlags, stale int, cfg auditConfig) {
	fmt.Fprintf(w, "Content-lossless audit: %d pages, %d content-loss flags, %d ratio-only outliers (%.1fs)\n",
		len(res.pages), res.contentLoss, res.ratioOnly, res.elapsed.Seconds())
	fmt.Fprintf(w, "Config: shingle-n=%d max-shingle-df=%d min-run=%d ratio-factor=%.2f\n",
		cfg.shingleN, cfg.maxDF, cfg.minRun, cfg.ratioFactor)
	fmt.Fprintf(w, "Chrome dropped (ref tokens): %s\n", formatSkipped(res.skipped))
	if base != nil {
		fmt.Fprintf(w, "Baseline: %d baselined (known exceptions), %d new, %d stale baseline entries\n",
			res.contentLoss-newFlags, newFlags, stale)
	}
	fmt.Fprintln(w)
	if len(res.flagged) == 0 {
		fmt.Fprintln(w, "No pages flagged.")
		return
	}
	suppressed := 0
	for _, f := range res.flagged {
		if f.Baselined {
			suppressed++
			continue
		}
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
	if suppressed > 0 {
		fmt.Fprintf(w, "(%d baselined flags suppressed; full detail in the --output JSON report)\n", suppressed)
	}
}

func writeAuditJSON(path string, res *auditResult, base *auditBaseline, newFlags, stale int, cfg auditConfig) error {
	report := map[string]any{
		"page_count":              len(res.pages),
		"content_loss_flag_count": res.contentLoss,
		"ratio_outlier_count":     res.ratioOnly,
		"skipped_chrome_tokens":   res.skipped,
		"config": map[string]any{
			"shingle_n":        cfg.shingleN,
			"max_shingle_df":   cfg.maxDF,
			"min_run":          cfg.minRun,
			"ratio_factor":     cfg.ratioFactor,
			"ratio_min_tokens": cfg.ratioMinTok,
		},
		"elapsed_seconds": res.elapsed.Seconds(),
		"flagged":         res.flagged,
	}
	if base != nil {
		report["baselined_flag_count"] = res.contentLoss - newFlags
		report["new_content_loss_flag_count"] = newFlags
		report["stale_baseline_count"] = stale
	}
	if res.flagged == nil {
		report["flagged"] = []*auditFlag{}
	}
	data, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	return os.WriteFile(path, data, 0644)
}
