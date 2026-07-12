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
// gating flag's page is covered by the baseline, and stale baseline entries (pages that no longer
// flag) are reported so the floor can only shrink visibly, never grow silently. Baseline entries
// pin each accepted page's flag MAGNITUDE (missing spans/shingles, derived token count) and the
// corpus page count, so a baselined page that worsens re-gates and silent page loss re-gates
// (M0042-S5; a legacy key-only baseline still loads, magnitude-unchecked, and asks to be
// regenerated).
//
// What the gate does NOT prove - the documented false-negative classes, the mirror of the FP
// candor above (blind E2E validation, 2026-07-12; see docs/DESIGN.md):
//   - Corpus-common content: a shingle repeated on more than max-shingle-df pages (shared
//     sentences like the hideFlags description, on 327 pages) is not page-unique. This class is
//     now DETECTED (M0042-S6, audit_shared.go): the derived-Markdown document frequency (mdDF)
//     separates shared CONTENT from chrome, so a high-ref-DF shingle missing from a page still
//     present broadly is a miss (Part A, live), and a --shared-baseline manifest catches a
//     total corpus-wide strip that drops mdDF to 0 (Part B) - the only case a single run cannot
//     see, because a totally stripped shingle is indistinguishable from chrome without a prior.
//   - Word-token granularity: tokens are letter/digit runs, so punctuation, operators, and
//     signs are invisible ("return -1" vs "return 1" does not flag). "Lossless" here always
//     means word-token-lossless.
//   - Stream edges: a loss shorter than min-run at the very start or end of the visible-text
//     stream can fall under the run bar; only INTERIOR single-token losses are guaranteed to
//     clear it. In practice kept chrome usually borders real pages and rescues detection.
//   - Duplicate-page families: near-identical pages push every shingle above max-shingle-df,
//     so a blanked family member produces no page-unique missing run; the gating ratio floor
//     (--ratio-gate-factor) is the backstop for that class.

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
	"unicode/utf8"
)

type auditConfig struct {
	shingleN     int
	maxDF        uint32
	minRun       int
	contentMinDF int
	ratioFactor  float64
	ratioGate    float64
	ratioMinTok  int
	maxQuotes    int
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
	RatioGated    bool     `json:"ratio_gated"`
	Baselined     bool     `json:"baselined"`
}

// gates reports whether a flag participates in the exit gate: a missing-content run, or a
// ratio collapse below the gating floor. Plain ratio outliers stay advisory.
func (f *auditFlag) gates() bool {
	return f.MissingSpans > 0 || f.RatioGated
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

	// Shared-content (corpus-common) detection (M0042-S6, audit_shared.go).
	contentShingles map[uint64]struct{} // content-classified high-ref-DF set, for --write-shared-baseline
	sharedLoss      int                 // manifest shingles that collapsed in the derived Markdown
	sharedQuotes    []string            // human-readable samples of the collapsed shared content
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
	contentMinDF := fs.Int("content-min-df", 5, "Min derived-Markdown document frequency for a high-ref-DF shingle to count as SHARED CONTENT (present in the Markdown broadly) rather than chrome. A high-ref-DF shingle missing from a page is a content-loss miss when it is content, ignored when it is chrome (md-DF below this).")
	minRun := fs.Int("min-run", 0, "Minimum run of consecutive missing page-unique shingles to flag a page; 0 = shingle width n. Tight by construction: a boundary between kept content and a stripped corpus-uniform chrome island produces exactly n-1 missing windows, so losing even one INTERIOR content token (>= n missing windows) is the smallest thing that clears the bar; a loss shorter than the bar hard against the stream's start or end can fall under it.")
	ratioFactor := fs.Float64("ratio-factor", 0.4, "Advisory ratio-outlier threshold: flag pages whose derived/reference token ratio is below section median times this.")
	ratioGate := fs.Float64("ratio-gate-factor", 0.25, "Gating ratio-collapse threshold: a page whose derived/reference token ratio falls below section median times this counts as content loss and gates the exit (baseline-coverable). 0 disables.")
	ratioMinTok := fs.Int("ratio-min-tokens", 30, "Skip ratio-outlier checks for pages with fewer reference tokens than this.")
	maxQuotes := fs.Int("max-quotes", 5, "Max quoted missing-text spans to report per flagged page.")
	baseline := fs.String("baseline", "", "Baseline allowlist JSON of accepted content-loss page keys. With it, exit is zero iff every content-loss flag is baselined; only NEW flags gate.")
	writeBaseline := fs.String("write-baseline", "", "Write this run's content-loss page keys as a baseline allowlist JSON and exit zero (explicit acceptance of the current flag set).")
	sharedBaselinePath := fs.String("shared-baseline", "", "Shared-content manifest JSON (see --write-shared-baseline). With it, the run gates when a pinned shared-content shingle has collapsed in the derived Markdown - a corpus-wide shared-content strip the page-local check cannot see.")
	writeSharedBaseline := fs.String("write-shared-baseline", "", "Write this run's content-classified shingle set as a shared-content manifest JSON and exit zero (explicit acceptance of the current shared content).")
	_ = fs.Parse(args)

	if *shingleN < 1 {
		fmt.Fprintln(os.Stderr, "error: --shingle-n must be >= 1")
		os.Exit(2)
	}
	if *maxDF < 0 || *maxQuotes < 0 || *ratioFactor < 0 || *ratioGate < 0 || *ratioMinTok < 0 || *minRun < 0 || *contentMinDF < 0 {
		fmt.Fprintln(os.Stderr, "error: --max-shingle-df, --content-min-df, --max-quotes, --ratio-factor, --ratio-gate-factor, --ratio-min-tokens, and --min-run must not be negative")
		os.Exit(2)
	}
	cfg := auditConfig{
		shingleN:     *shingleN,
		maxDF:        uint32(*maxDF),
		minRun:       *minRun,
		contentMinDF: *contentMinDF,
		ratioFactor:  *ratioFactor,
		ratioGate:    *ratioGate,
		ratioMinTok:  *ratioMinTok,
		maxQuotes:    *maxQuotes,
	}
	if cfg.minRun < 1 {
		cfg.minRun = cfg.shingleN
	}
	if *workers < 1 {
		*workers = defaultWorkers()
	}

	var shared *sharedBaseline
	if *sharedBaselinePath != "" {
		s, err := loadSharedBaseline(*sharedBaselinePath)
		if err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			os.Exit(1)
		}
		if mm := s.configMismatch(cfg); mm != "" {
			fmt.Fprintf(os.Stderr, "error: shared baseline %s was generated under a different audit config (%s); its pinned fingerprints do not correspond to this run, so the shared-content gate would silently pass - regenerate it with --write-shared-baseline or match the flags\n", *sharedBaselinePath, mm)
			os.Exit(1)
		}
		shared = s
	}

	res, err := auditCorpus(*source, *corpus, *workers, cfg, shared)
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
	shrunk := baselinePageShrink(base, len(res.pages))

	printAuditReport(os.Stdout, res, base, shared, newFlags, stale, shrunk, cfg)
	if *output != "" {
		if err := writeAuditJSON(*output, res, base, shared, newFlags, stale, shrunk, cfg); err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			os.Exit(1)
		}
		fmt.Fprintln(os.Stderr, "Wrote JSON report:", *output)
	}
	wrote := false
	if *writeBaseline != "" {
		if err := writeAuditBaseline(*writeBaseline, res); err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			os.Exit(1)
		}
		fmt.Fprintln(os.Stderr, "Wrote baseline:", *writeBaseline)
		wrote = true
	}
	if *writeSharedBaseline != "" {
		if err := writeSharedBaselineFile(*writeSharedBaseline, res, cfg); err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			os.Exit(1)
		}
		fmt.Fprintf(os.Stderr, "Wrote shared-content manifest: %s (%d content shingles)\n", *writeSharedBaseline, len(res.contentShingles))
		wrote = true
	}
	// Writing a baseline IS the acceptance of the current state: exit zero.
	if wrote {
		return
	}
	// Gating flags outside the baseline, a shrunken page count, or a collapsed shared-content
	// shingle fail the run; plain ratio outliers are advisory.
	if newFlags > 0 || shrunk > 0 || res.sharedLoss > 0 {
		os.Exit(1)
	}
}

// writeSharedBaselineFile writes the run's content-classified shingle set as a manifest.
func writeSharedBaselineFile(path string, res *auditResult, cfg auditConfig) error {
	return writeSharedBaseline(path, res.contentShingles, cfg)
}

func auditCorpus(source, corpus string, workers int, cfg auditConfig, shared *sharedBaseline) (*auditResult, error) {
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

	// Whole-page-loss gate: the builder derives one corpus page per source HTML file, so the
	// counts must match exactly. A mismatch means either a build-discovery regression silently
	// dropped pages or this source tree is not the one the corpus was built from - both make
	// the audit's per-page verdicts meaningless, so refuse instead of certifying.
	htmlCount, err := countSectionHTML(sourceAbs)
	if err != nil {
		return nil, err
	}
	if htmlCount != len(pages) {
		return nil, fmt.Errorf("corpus lists %d pages but %s holds %d source HTML files - a build regression dropped pages, or source and corpus are from different doc versions; rebuild with:\n  bin/unity-doc-corpus build --source %s --output %s --keep-source",
			len(pages), sourceAbs, htmlCount, source, corpus)
	}

	// Pass 1: extract reference visible text for every page, cache it, and fold each page's
	// distinct shingles into the reference document-frequency table. Also read the derived
	// Markdown and fold its distinct shingles into a second table (mdDF) - the md-DF of a
	// high-ref-DF shingle is what separates shared CONTENT (present in the Markdown broadly)
	// from chrome (absent by design), the discrimination the shared-content check turns on.
	refJoined := make([]string, len(pages))
	df := newDFCounter()
	mdDF := newDFCounter()
	var skipMu sync.Mutex
	skippedTotals := map[string]int{}
	if err := auditParallel(workers, len(pages), func(i int) error {
		htmlPath := filepath.Join(sourceAbs, filepath.FromSlash(pages[i].SourceRel))
		raw, err := os.ReadFile(htmlPath)
		if err != nil {
			if os.IsNotExist(err) {
				return fmt.Errorf("missing source page %s\nthe audit reads the original HTML - rematerialize it with:\n  bin/unity-doc-corpus build --source %s --output %s --keep-source",
					htmlPath, source, corpus)
			}
			return fmt.Errorf("reading %s: %w", htmlPath, err)
		}
		tokens, skipped := auditExtractTokens(string(raw))
		refJoined[i] = strings.Join(tokens, " ")
		for fp := range distinctShingles(tokens, cfg.shingleN) {
			df.add(fp)
		}
		mdPath := filepath.Join(corpusAbs, filepath.FromSlash(pages[i].MDRel))
		mdRaw, err := os.ReadFile(mdPath)
		if err != nil {
			if os.IsNotExist(err) {
				return fmt.Errorf("missing derived page %s (corpus incomplete - rebuild with: bin/unity-doc-corpus build --source %s --output %s)", mdPath, source, corpus)
			}
			return fmt.Errorf("reading %s: %w", mdPath, err)
		}
		for fp := range distinctShingles(auditTokenize(string(mdRaw)), cfg.shingleN) {
			mdDF.add(fp)
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

	// The content-classified shingle set (shared in source, present in the Markdown broadly)
	// backs --write-shared-baseline. When a shared-content manifest is supplied, the collapsed
	// set is the pinned shingles that are still shared upstream but have vanished downstream.
	contentShingles := buildContentShingles(df, mdDF, cfg)
	var collapsed map[uint64]struct{}
	if shared != nil {
		collapsed = sharedCollapsed(shared, df, mdDF, cfg)
	}

	// Pass 2: for every page, check that each page-unique OR shared-content reference shingle is
	// present in the derived Markdown; collect runs of consecutive misses as content-loss flags.
	// While here, when a shared-content manifest supplied a collapsed set, capture a readable
	// quote for each collapsed shingle from the reference stream that still carries it.
	flags := make([]*auditFlag, len(pages))
	ratios := make([]float64, len(pages))
	refTokCounts := make([]int, len(pages))
	mdTokCounts := make([]int, len(pages))
	var quoteMu sync.Mutex
	collapsedQuotes := map[uint64]string{}
	if err := auditParallel(workers, len(pages), func(i int) error {
		mdPath := filepath.Join(corpusAbs, filepath.FromSlash(pages[i].MDRel))
		mdRaw, err := os.ReadFile(mdPath)
		if err != nil {
			if os.IsNotExist(err) {
				return fmt.Errorf("missing derived page %s (corpus incomplete - rebuild with: bin/unity-doc-corpus build --source %s --output %s)", mdPath, source, corpus)
			}
			return fmt.Errorf("reading %s: %w", mdPath, err)
		}
		mdTokens := auditTokenize(string(mdRaw))
		refTokens := auditTokenize(refJoined[i])
		refTokCounts[i] = len(refTokens)
		mdTokCounts[i] = len(mdTokens)
		if len(refTokens) > 0 {
			ratios[i] = float64(len(mdTokens)) / float64(len(refTokens))
		}
		flags[i] = auditPage(pages[i], refTokens, mdTokens, df, mdDF, cfg)
		if flags[i] != nil {
			flags[i].RefTokens = len(refTokens)
			flags[i].MDTokens = len(mdTokens)
			if len(refTokens) > 0 {
				flags[i].Ratio = ratios[i]
			}
		}
		if len(collapsed) > 0 {
			collectCollapsedQuotes(refTokens, collapsed, cfg.shingleN, &quoteMu, collapsedQuotes)
		}
		return nil
	}); err != nil {
		return nil, err
	}

	// Ratio pass: per-section median; advisory outliers plus the gating collapse tier.
	markRatioOutliers(pages, ratios, refTokCounts, mdTokCounts, flags, cfg)

	// Collect flagged pages (content-loss runs, ratio collapses, advisory outliers) in stable
	// page order.
	var flagged []*auditFlag
	for _, f := range flags {
		if f != nil && (f.MissingSpans > 0 || f.RatioOutlier || f.RatioGated) {
			flagged = append(flagged, f)
		}
	}
	contentLoss := 0
	ratioOnly := 0
	for _, f := range flagged {
		if f.gates() {
			contentLoss++
		} else {
			ratioOnly++
		}
	}

	var sharedQuotes []string
	for _, q := range collapsedQuotes {
		sharedQuotes = append(sharedQuotes, q)
	}
	sort.Strings(sharedQuotes)
	if cfg.maxQuotes >= 0 && len(sharedQuotes) > cfg.maxQuotes {
		sharedQuotes = sharedQuotes[:cfg.maxQuotes]
	}

	return &auditResult{
		pages:           pages,
		flagged:         flagged,
		contentLoss:     contentLoss,
		ratioOnly:       ratioOnly,
		skipped:         skippedTotals,
		elapsed:         time.Since(start),
		contentShingles: contentShingles,
		sharedLoss:      len(collapsed),
		sharedQuotes:    sharedQuotes,
	}, nil
}

// collectCollapsedQuotes scans a page's reference tokens for windows whose fingerprint is in
// the collapsed set and records one readable quote per collapsed shingle (first occurrence
// wins). Only called when the collapsed set is non-empty, so clean runs pay nothing.
func collectCollapsedQuotes(refTokens []string, collapsed map[uint64]struct{}, n int, mu *sync.Mutex, out map[uint64]string) {
	for k := 0; k+n <= len(refTokens); k++ {
		fp := shingleFingerprint(refTokens[k : k+n])
		if _, ok := collapsed[fp]; !ok {
			continue
		}
		mu.Lock()
		if _, seen := out[fp]; !seen {
			out[fp] = clip(strings.Join(refTokens[k:k+n], " "))
		}
		mu.Unlock()
	}
}

// auditBaselineDescription is written into every baseline file so the file explains itself.
const auditBaselineDescription = "Accepted content-loss flags for the unity-doc-corpus audit " +
	"(known false-positive classes, triaged per the M0042 spec). audit --baseline exits zero " +
	"iff every gating flag is covered by an entry AT OR BELOW its recorded magnitude and the " +
	"corpus page count has not shrunk below page_count; a covered page that worsens re-gates. " +
	"Regenerate with --write-baseline only after a human triages the change."

// auditBaselineEntry pins one accepted page's flag magnitude. A flag is covered only while
// it is no worse than recorded: no more missing spans/shingles, no fewer derived tokens.
type auditBaselineEntry struct {
	PageKey         string `json:"page_key"`
	MissingSpans    int    `json:"missing_spans"`
	MissingShingles int    `json:"missing_shingles"`
	MDTokens        int    `json:"md_tokens"`
}

type auditBaseline struct {
	Description string               `json:"description"`
	PageCount   int                  `json:"page_count,omitempty"`
	Pages       []auditBaselineEntry `json:"pages,omitempty"`
	// PageKeys is the legacy v1 format: bare keys, no magnitude. Still honored (a listed page
	// is covered at ANY magnitude) so old baselines keep working, but reported as legacy so
	// they get regenerated.
	PageKeys []string `json:"page_keys,omitempty"`
}

func (b *auditBaseline) legacy() bool { return len(b.PageKeys) > 0 && len(b.Pages) == 0 }

func (b *auditBaseline) entryCount() int {
	if b.legacy() {
		return len(b.PageKeys)
	}
	return len(b.Pages)
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

// writeAuditBaseline captures the run's gating flags with their magnitudes plus the corpus
// page count, sorted for a stable diff.
func writeAuditBaseline(path string, res *auditResult) error {
	entries := make([]auditBaselineEntry, 0, len(res.flagged))
	for _, f := range res.flagged {
		if f.gates() {
			entries = append(entries, auditBaselineEntry{
				PageKey:         f.PageKey,
				MissingSpans:    f.MissingSpans,
				MissingShingles: f.MissingWindow,
				MDTokens:        f.MDTokens,
			})
		}
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].PageKey < entries[j].PageKey })
	b := auditBaseline{Description: auditBaselineDescription, PageCount: len(res.pages), Pages: entries}
	data, err := json.MarshalIndent(b, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(data, '\n'), 0644)
}

// applyAuditBaseline marks gating flags covered by the baseline, returning the count of NEW
// gating flags (unlisted pages OR listed pages that worsened beyond their recorded magnitude)
// and of stale baseline entries (listed pages that no longer flag; drop them from the file on
// the next triaged --write-baseline). A nil baseline leaves every gating flag new.
func applyAuditBaseline(flagged []*auditFlag, base *auditBaseline) (newFlags, stale int) {
	entries := map[string]auditBaselineEntry{}
	legacyKeys := map[string]bool{}
	if base != nil {
		for _, e := range base.Pages {
			entries[e.PageKey] = e
		}
		for _, k := range base.PageKeys {
			legacyKeys[k] = true
		}
	}
	covered := func(f *auditFlag) bool {
		if e, ok := entries[f.PageKey]; ok {
			return f.MissingSpans <= e.MissingSpans && f.MissingWindow <= e.MissingShingles && f.MDTokens >= e.MDTokens
		}
		return legacyKeys[f.PageKey] // legacy entries carry no magnitude to hold against
	}
	seen := map[string]bool{}
	for _, f := range flagged {
		if !f.gates() {
			continue
		}
		if covered(f) {
			f.Baselined = true
		} else {
			newFlags++
		}
		seen[f.PageKey] = true // a worsened listed page is not stale, just uncovered
	}
	for k := range entries {
		if !seen[k] {
			stale++
		}
	}
	for k := range legacyKeys {
		if !seen[k] {
			stale++
		}
	}
	return newFlags, stale
}

// baselinePageShrink returns how many pages the corpus lost against the baseline's recorded
// page count (0 when no baseline, no recorded count, or growth - growth means a new docs
// version and shows up as ordinary new/stale churn instead).
func baselinePageShrink(base *auditBaseline, pageCount int) int {
	if base == nil || base.PageCount == 0 || pageCount >= base.PageCount {
		return 0
	}
	return base.PageCount - pageCount
}

// countSectionHTML counts the source HTML files under the section trees with an independent
// walk - deliberately not the builder's collectHTML, mirroring the extractor's independence.
func countSectionHTML(sourceAbs string) (int, error) {
	count := 0
	for _, section := range sectionDirs {
		err := filepath.WalkDir(filepath.Join(sourceAbs, section), func(path string, d os.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if !d.IsDir() && strings.EqualFold(filepath.Ext(path), ".html") {
				count++
			}
			return nil
		})
		if err != nil {
			return 0, err
		}
	}
	return count, nil
}

// auditPage checks one page and returns a flag if it has a qualifying run of missing
// checkable shingles, else nil. refTokens/mdTokens are the two token streams; df/mdDF are the
// corpus reference and derived-Markdown document-frequency tables (mdDF may be nil in unit
// tests that exercise only page-unique shingles).
//
// A reference shingle is CHECKABLE (must appear in the Markdown) when it is page-unique
// (ref-DF <= max-df) OR shared CONTENT (ref-DF > max-df AND md-DF >= content-min-df, i.e. it is
// present across the Markdown broadly, not stripped chrome). Only CHROME (high ref-DF, low
// md-DF) is ignored - the same corpus-uniform chrome the frequency step always dropped, now
// pinned down by its md-DF instead of by ref-DF alone.
func auditPage(p pageRef, refTokens, mdTokens []string, df, mdDF *dfCounter, cfg auditConfig) *auditFlag {
	if len(refTokens) == 0 {
		return nil
	}
	mdGet := func(fp uint64) uint32 {
		if mdDF == nil {
			return 0
		}
		return mdDF.get(fp)
	}
	// isChrome: a high-ref-DF shingle that is NOT present in the Markdown broadly - stripped by
	// design, so its absence from a page is not a loss.
	isChrome := func(fp uint64) bool {
		return df.get(fp) > cfg.maxDF && mdGet(fp) < uint32(cfg.contentMinDF)
	}
	// Short page: the whole token stream is one shingle; the n-gram set does not apply, so
	// check membership as a direct subsequence of the Markdown tokens.
	if len(refTokens) < cfg.shingleN {
		fp := shingleFingerprint(refTokens)
		if !isChrome(fp) && !containsSubsequence(mdTokens, refTokens) {
			return &auditFlag{
				PageKey: p.PageKey, Section: p.Section, SourceRel: p.SourceRel, MDRel: p.MDRel,
				MissingWindow: 1, MissingSpans: 1,
				Quotes: []string{clip(strings.Join(refTokens, " "))},
			}
		}
		return nil
	}

	mdSet := distinctShingles(mdTokens, cfg.shingleN)

	// missing[k] is true when the shingle starting at ref token k is checkable yet absent from
	// the Markdown. Runs of consecutive true values are content-loss spans.
	windowCount := len(refTokens) - cfg.shingleN + 1
	var spans [][2]int // [startTok, endTokExclusive) of each qualifying run
	missingWindows := 0
	runStart := -1
	runLen := 0
	closeRun := func(endWindow int) {
		if runStart >= 0 && runLen >= cfg.minRun {
			// The final missing window covers tokens [endWindow, endWindow+n-1], so the
			// exclusive span end is endWindow + n.
			spans = append(spans, [2]int{runStart, endWindow + cfg.shingleN})
		}
		runStart = -1
		runLen = 0
	}
	for k := 0; k < windowCount; k++ {
		fp := shingleFingerprint(refTokens[k : k+cfg.shingleN])
		if _, ok := mdSet[fp]; ok || isChrome(fp) {
			// Present, or chrome (stripped by design): ends any open run.
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
	quotes := make([]string, 0, max(cfg.maxQuotes, 0))
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
// pages that fall well below it. Two tiers: below median*ratioFactor is an advisory outlier;
// below median*ratioGate is a gating ratio collapse (the backstop for losses the shingle
// invariant cannot see, e.g. a blanked page in a duplicate family).
func markRatioOutliers(pages []pageRef, ratios []float64, refTok, mdTok []int, flags []*auditFlag, cfg auditConfig) {
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
		gated := cfg.ratioGate > 0 && ratios[i] < med*cfg.ratioGate
		if ratios[i] < med*cfg.ratioFactor || gated {
			if flags[i] == nil {
				flags[i] = &auditFlag{
					PageKey: pages[i].PageKey, Section: pages[i].Section,
					SourceRel: pages[i].SourceRel, MDRel: pages[i].MDRel,
					RefTokens: refTok[i], MDTokens: mdTok[i], Ratio: ratios[i],
				}
			}
			flags[i].RatioOutlier = true
			flags[i].RatioGated = gated
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

// clip trims a quoted span to a readable length, backing up to a rune boundary so a
// multi-byte character is never cut in half.
func clip(s string) string {
	const limit = 200
	if len(s) <= limit {
		return s
	}
	cut := limit
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + "..."
}

// loadPageRefs reads the corpus pages.jsonl into the page list the audit iterates.
func loadPageRefs(corpusAbs string) ([]pageRef, error) {
	path := filepath.Join(corpusAbs, "pages.jsonl")
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("%s not found (is the corpus built? run: bin/unity-doc-corpus build --source <docs-root> --output %s)", path, corpusAbs)
		}
		return nil, err
	}
	defer f.Close()
	var pages []pageRef
	scanner := bufio.NewScanner(f)
	buf := make([]byte, 1024*1024)
	scanner.Buffer(buf, 16*1024*1024)
	line := 0
	for scanner.Scan() {
		line++
		if len(strings.TrimSpace(string(scanner.Bytes()))) == 0 {
			continue
		}
		var rec struct {
			PageKey   string `json:"page_key"`
			Section   string `json:"section"`
			SourceRel string `json:"source_rel"`
			MDRel     string `json:"md_rel"`
		}
		// A malformed or field-less record is corpus corruption, not noise to skip: silently
		// dropping it would shrink the audited page set - the same class as silent page loss.
		if err := json.Unmarshal(scanner.Bytes(), &rec); err != nil {
			return nil, fmt.Errorf("%s line %d: malformed page record (%v) - rebuild the corpus", path, line, err)
		}
		if rec.SourceRel == "" || rec.MDRel == "" {
			return nil, fmt.Errorf("%s line %d: page record missing source_rel/md_rel - rebuild the corpus", path, line)
		}
		pages = append(pages, pageRef{PageKey: rec.PageKey, Section: rec.Section, SourceRel: rec.SourceRel, MDRel: rec.MDRel})
	}
	return pages, scanner.Err()
}

// auditParallel runs fn over indices [0,count) on a fixed worker pool, returning the first
// error any worker reports.
func auditParallel(workers, count int, fn func(i int) error) error {
	if workers < 1 {
		workers = 1 // a non-positive count would leave the job feed blocking forever
	}
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

func printAuditReport(w *os.File, res *auditResult, base *auditBaseline, shared *sharedBaseline, newFlags, stale, shrunk int, cfg auditConfig) {
	fmt.Fprintf(w, "Content-lossless audit: %d pages, %d gating flags, %d advisory ratio outliers (%.1fs)\n",
		len(res.pages), res.contentLoss, res.ratioOnly, res.elapsed.Seconds())
	fmt.Fprintf(w, "Config: shingle-n=%d max-shingle-df=%d content-min-df=%d min-run=%d ratio-factor=%.2f ratio-gate-factor=%.2f\n",
		cfg.shingleN, cfg.maxDF, cfg.contentMinDF, cfg.minRun, cfg.ratioFactor, cfg.ratioGate)
	fmt.Fprintf(w, "Chrome dropped (ref tokens): %s\n", formatSkipped(res.skipped))
	if base != nil {
		fmt.Fprintf(w, "Baseline: %d baselined (known exceptions), %d new, %d stale baseline entries\n",
			res.contentLoss-newFlags, newFlags, stale)
		if base.legacy() {
			fmt.Fprintln(w, "Baseline is legacy v1 (bare page keys, no magnitudes or page count) - regenerate with --write-baseline to pin them")
		}
		if shrunk > 0 {
			fmt.Fprintf(w, "PAGE COUNT SHRANK: corpus has %d pages, baseline recorded %d (%d lost) - gating\n",
				len(res.pages), base.PageCount, shrunk)
		}
	}
	if shared != nil {
		if res.sharedLoss == 0 {
			fmt.Fprintf(w, "Shared-content: %d shingles pinned, 0 collapsed\n", len(shared.set))
		} else {
			fmt.Fprintf(w, "SHARED-CONTENT LOSS: %d pinned shingles are still shared in the source but vanished from the Markdown - gating\n", res.sharedLoss)
			for _, q := range res.sharedQuotes {
				fmt.Fprintf(w, "    missing (shared): %q\n", q)
			}
		}
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
			if f.RatioGated {
				kind = "ratio-collapse"
			}
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

func writeAuditJSON(path string, res *auditResult, base *auditBaseline, shared *sharedBaseline, newFlags, stale, shrunk int, cfg auditConfig) error {
	report := map[string]any{
		"page_count":              len(res.pages),
		"content_loss_flag_count": res.contentLoss,
		"ratio_outlier_count":     res.ratioOnly,
		"skipped_chrome_tokens":   res.skipped,
		"config": map[string]any{
			"shingle_n":         cfg.shingleN,
			"max_shingle_df":    cfg.maxDF,
			"content_min_df":    cfg.contentMinDF,
			"min_run":           cfg.minRun,
			"ratio_factor":      cfg.ratioFactor,
			"ratio_gate_factor": cfg.ratioGate,
			"ratio_min_tokens":  cfg.ratioMinTok,
		},
		"elapsed_seconds": res.elapsed.Seconds(),
		"flagged":         res.flagged,
	}
	if base != nil {
		report["baselined_flag_count"] = res.contentLoss - newFlags
		report["new_content_loss_flag_count"] = newFlags
		report["stale_baseline_count"] = stale
		report["baseline_page_shrink"] = shrunk
	}
	if shared != nil {
		report["shared_content_pinned"] = len(shared.set)
		report["shared_content_loss_count"] = res.sharedLoss
		report["shared_content_loss_samples"] = res.sharedQuotes
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
