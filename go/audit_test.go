package main

// Seeded-regression proof for the content-lossless audit (M0042-S0002, spec Design 5).
// The audit is only trusted if it demonstrably catches the known failure class: these tests
// build a small synthetic corpus, plant the regressions the 2026-07-09 truncation bug family
// produces (tail truncation, dropped middle section), and assert the audit flags exactly
// those pages - while a page whose markdown differs from the HTML only by stripped chrome
// (the transform's normal, deliberate difference) stays unflagged.

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"unicode/utf8"
)

// fixtureChrome is the corpus-uniform chrome sentence shared by every fixture page. With
// fixturePageCount pages it repeats far above max-shingle-df, so its interior shingles are
// not page-unique - exactly how the real footer/feedback chrome is discriminated.
const fixtureChrome = "Is something described here not working as you expect it to " +
	"Please check with the Issue Tracker Copyright Unity Technologies again"

const (
	fixturePageCount  = 12
	fixtureParasPer   = 3
	fixtureTokensPara = 20
	fixtureTruncated  = "Manual/TailTruncated"  // markdown lost its final paragraph
	fixtureDroppedMid = "Manual/DroppedMiddle"  // markdown lost its middle paragraph
	fixtureChromeCtrl = "Manual/ChromeOnlyDiff" // markdown differs by stripped chrome only
)

// fixturePara builds one page-unique paragraph: every token embeds the page and paragraph
// index, so its shingles have corpus document frequency 1 (unambiguously page-unique).
func fixturePara(page, para int) string {
	tokens := make([]string, fixtureTokensPara)
	for i := range tokens {
		tokens[i] = fmt.Sprintf("pg%dpr%dw%d", page, para, i)
	}
	return strings.Join(tokens, " ")
}

// buildAuditFixture writes a synthetic source tree + derived corpus into a temp dir and
// returns (sourceDir, corpusDir). Page 1 is seeded with tail truncation, page 2 with a
// dropped middle section; every other page is a chrome-only-difference control.
func buildAuditFixture(t *testing.T) (string, string) {
	t.Helper()
	root := t.TempDir()
	sourceDir := filepath.Join(root, "source")
	corpusDir := filepath.Join(root, "corpus")
	for _, d := range []string{
		filepath.Join(sourceDir, "Manual"),
		filepath.Join(sourceDir, "ScriptReference"),
		filepath.Join(corpusDir, "text", "Manual"),
	} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}

	var jsonl strings.Builder
	for p := 0; p < fixturePageCount; p++ {
		name := fmt.Sprintf("Page%02d", p)
		key := "Manual/" + name
		switch p {
		case 1:
			name, key = "TailTruncated", fixtureTruncated
		case 2:
			name, key = "DroppedMiddle", fixtureDroppedMid
		case 3:
			name, key = "ChromeOnlyDiff", fixtureChromeCtrl
		}

		paras := make([]string, fixtureParasPer)
		for i := range paras {
			paras[i] = fixturePara(p, i)
		}

		// Source HTML mirrors the real page shape: breadcrumb (page-local chrome the
		// extractor skips structurally) and footer chrome nested INSIDE #content-wrap.
		html := "<html><head><title>Unity - Manual: " + name + "</title></head><body>\n" +
			"<div class=\"header\">SITE HEADER OUTSIDE CONTENT WRAP</div>\n" +
			"<div id=\"content-wrap\">\n" +
			"<div class=\"breadcrumb\"><a href=\"index.html\">Unity Manual</a> &gt; " + name + "</div>\n" +
			"<h1>" + name + "</h1>\n"
		for _, para := range paras {
			html += "<p>" + para + "</p>\n"
		}
		html += "<div class=\"footer\"><div class=\"feedbackbox\">" + fixtureChrome + "</div></div>\n" +
			"</div>\n</body></html>\n"
		if err := os.WriteFile(filepath.Join(sourceDir, "Manual", name+".html"), []byte(html), 0o644); err != nil {
			t.Fatal(err)
		}

		// Derived markdown: the transform keeps the content paragraphs and strips the
		// chrome. The seeds damage the CONTENT side only - source HTML stays intact,
		// exactly like a real transform regression.
		mdParas := paras
		switch key {
		case fixtureTruncated:
			mdParas = paras[:fixtureParasPer-1]
		case fixtureDroppedMid:
			mdParas = []string{paras[0], paras[2]}
		}
		md := "---\ntitle: " + name + "\n---\n\n" + strings.Join(mdParas, "\n\n") + "\n"
		if err := os.WriteFile(filepath.Join(corpusDir, "text", "Manual", name+".md"), []byte(md), 0o644); err != nil {
			t.Fatal(err)
		}

		jsonl.WriteString(fmt.Sprintf(
			`{"page_key":%q,"section":"Manual","source_rel":"Manual/%s.html","md_rel":"text/Manual/%s.md"}`+"\n",
			key, name, name))
	}
	if err := os.WriteFile(filepath.Join(corpusDir, "pages.jsonl"), []byte(jsonl.String()), 0o644); err != nil {
		t.Fatal(err)
	}
	return sourceDir, corpusDir
}

func fixtureAuditConfig() auditConfig {
	return auditConfig{shingleN: 5, maxDF: 4, minRun: 5, ratioFactor: 0.4, ratioGate: 0.25, ratioMinTok: 30, maxQuotes: 5}
}

// runFixtureAudit audits the synthetic corpus and returns the flags keyed by page.
func runFixtureAudit(t *testing.T) map[string]*auditFlag {
	t.Helper()
	sourceDir, corpusDir := buildAuditFixture(t)
	res, err := auditCorpus(sourceDir, corpusDir, 4, fixtureAuditConfig())
	if err != nil {
		t.Fatal(err)
	}
	if len(res.pages) != fixturePageCount {
		t.Fatalf("audited %d pages, want %d", len(res.pages), fixturePageCount)
	}
	byKey := map[string]*auditFlag{}
	for _, f := range res.flagged {
		byKey[f.PageKey] = f
	}
	return byKey
}

// Seed (a): the 2026-07-09 failure class - the markdown lost the tail of its page.
func TestAuditFlagsSeededTailTruncation(t *testing.T) {
	flags := runFixtureAudit(t)
	f := flags[fixtureTruncated]
	if f == nil || f.MissingSpans == 0 {
		t.Fatalf("tail-truncated page not flagged as content loss: %+v", f)
	}
	// Quotes are clipped for readability, so assert the span STARTS at the dropped tail and
	// that the window count proves the whole paragraph is inside it (k tokens -> k+n-1 windows).
	if quotes := strings.Join(f.Quotes, " "); !strings.Contains(quotes, "pg1pr2w0") {
		t.Errorf("missing-text quotes must cover the dropped tail: %q", quotes)
	}
	if f.MissingWindow < fixtureTokensPara {
		t.Errorf("missing windows = %d, want >= %d (the full dropped paragraph)", f.MissingWindow, fixtureTokensPara)
	}
}

// Seed (b): a dropped middle section, invisible to naive length checks.
func TestAuditFlagsSeededDroppedMiddleSection(t *testing.T) {
	flags := runFixtureAudit(t)
	f := flags[fixtureDroppedMid]
	if f == nil || f.MissingSpans == 0 {
		t.Fatalf("dropped-middle page not flagged as content loss: %+v", f)
	}
	if quotes := strings.Join(f.Quotes, " "); !strings.Contains(quotes, "pg2pr1w0") {
		t.Errorf("missing-text quotes must cover the dropped section: %q", quotes)
	}
	if f.MissingWindow < fixtureTokensPara {
		t.Errorf("missing windows = %d, want >= %d (the full dropped section)", f.MissingWindow, fixtureTokensPara)
	}
}

// Seed (c), the false-positive control: a markdown that differs from the HTML only by the
// stripped chrome (breadcrumb + corpus-uniform footer) must NOT be flagged - that difference
// is the transform working as designed. Every non-seeded fixture page is such a control.
func TestAuditChromeOnlyDifferenceNotFlagged(t *testing.T) {
	flags := runFixtureAudit(t)
	for key, f := range flags {
		if key == fixtureTruncated || key == fixtureDroppedMid {
			continue
		}
		t.Errorf("chrome-only-difference page wrongly flagged: %s %+v", key, f)
	}
	if _, ok := flags[fixtureChromeCtrl]; ok {
		t.Errorf("designated control page %s must not be flagged", fixtureChromeCtrl)
	}
}

// The baseline allowlist is the CI gate over the accepted false-positive floor: known flags
// are baselined AT THEIR RECORDED MAGNITUDE, new flags and worsened known flags gate, and
// entries that stop flagging are reported as stale.
func TestAuditBaselinePartitionAndRoundTrip(t *testing.T) {
	mk := func(key string, spans, windows, mdTok int) *auditFlag {
		return &auditFlag{PageKey: key, MissingSpans: spans, MissingWindow: windows, MDTokens: mdTok}
	}
	flagged := []*auditFlag{mk("Manual/A", 1, 5, 100), mk("Manual/B", 2, 10, 200), mk("Manual/RatioOnly", 0, 0, 50)}

	// No baseline: every gating flag is new; advisory ratio-only flags never count.
	if newFlags, stale := applyAuditBaseline(flagged, nil); newFlags != 2 || stale != 0 {
		t.Errorf("nil baseline: new=%d stale=%d, want 2/0", newFlags, stale)
	}

	// Round-trip through the v2 file format: magnitudes and page count are pinned.
	path := filepath.Join(t.TempDir(), "baseline.json")
	if err := writeAuditBaseline(path, &auditResult{pages: make([]pageRef, 12), flagged: flagged}); err != nil {
		t.Fatal(err)
	}
	base, err := loadAuditBaseline(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(base.Pages) != 2 || base.Pages[0].PageKey != "Manual/A" || base.Pages[1].PageKey != "Manual/B" {
		t.Fatalf("baseline must hold the sorted gating entries only: %+v", base.Pages)
	}
	if base.Pages[0].MissingSpans != 1 || base.Pages[0].MissingShingles != 5 || base.Pages[0].MDTokens != 100 {
		t.Fatalf("entry must pin the flag magnitude: %+v", base.Pages[0])
	}
	if base.PageCount != 12 {
		t.Fatalf("baseline must pin the corpus page count, got %d", base.PageCount)
	}
	if base.legacy() {
		t.Fatal("v2 baseline must not read as legacy")
	}

	// Full coverage at identical magnitude: nothing new, flags marked baselined.
	flagged = []*auditFlag{mk("Manual/A", 1, 5, 100), mk("Manual/B", 2, 10, 200)}
	if newFlags, stale := applyAuditBaseline(flagged, base); newFlags != 0 || stale != 0 {
		t.Errorf("covered run: new=%d stale=%d, want 0/0", newFlags, stale)
	}
	if !flagged[0].Baselined || !flagged[1].Baselined {
		t.Error("covered flags must be marked baselined")
	}

	// An IMPROVED baselined page stays covered (floor shrinks on the next regeneration).
	flagged = []*auditFlag{mk("Manual/A", 1, 3, 150), mk("Manual/B", 2, 10, 200)}
	if newFlags, _ := applyAuditBaseline(flagged, base); newFlags != 0 {
		t.Errorf("improved baselined page must stay covered, new=%d", newFlags)
	}

	// A WORSENED baselined page re-gates - this is the anti-hiding guarantee: more spans,
	// more missing shingles, or fewer derived tokens each break coverage.
	for _, worse := range []*auditFlag{
		mk("Manual/A", 2, 5, 100), // more spans
		mk("Manual/A", 1, 9, 100), // more missing shingles
		mk("Manual/A", 1, 5, 40),  // derived markdown shrank
	} {
		flagged = []*auditFlag{worse, mk("Manual/B", 2, 10, 200)}
		newFlags, stale := applyAuditBaseline(flagged, base)
		if newFlags != 1 {
			t.Errorf("worsened baselined page %+v must gate, new=%d", worse, newFlags)
		}
		if stale != 0 {
			t.Errorf("worsened page is not stale, stale=%d", stale)
		}
	}

	// A new regression alongside the known floor gates; a fixed page goes stale.
	flagged = []*auditFlag{mk("Manual/A", 1, 5, 100), mk("Manual/NewRegression", 3, 20, 10)}
	newFlags, stale := applyAuditBaseline(flagged, base)
	if newFlags != 1 || stale != 1 {
		t.Errorf("new+stale run: new=%d stale=%d, want 1/1", newFlags, stale)
	}
	if flagged[1].Baselined {
		t.Error("unbaselined new flag must not be marked baselined")
	}

	// A gating ratio collapse is baseline-coverable through its pinned md_tokens too.
	gated := &auditFlag{PageKey: "Manual/A", RatioGated: true, MDTokens: 100}
	if newFlags, _ := applyAuditBaseline([]*auditFlag{gated, mk("Manual/B", 2, 10, 200)}, base); newFlags != 0 {
		t.Errorf("ratio collapse at recorded magnitude must be covered, new=%d", newFlags)
	}
	gated = &auditFlag{PageKey: "Manual/A", RatioGated: true, MDTokens: 10}
	if newFlags, _ := applyAuditBaseline([]*auditFlag{gated, mk("Manual/B", 2, 10, 200)}, base); newFlags != 1 {
		t.Errorf("deepened ratio collapse must gate, new=%d", newFlags)
	}

	// Legacy v1 baselines (bare page keys) still cover - at any magnitude - and read as legacy.
	legacy := &auditBaseline{PageKeys: []string{"Manual/A"}}
	if !legacy.legacy() {
		t.Fatal("key-only baseline must read as legacy")
	}
	flagged = []*auditFlag{mk("Manual/A", 9, 99, 1)}
	if newFlags, stale := applyAuditBaseline(flagged, legacy); newFlags != 0 || stale != 0 {
		t.Errorf("legacy coverage: new=%d stale=%d, want 0/0", newFlags, stale)
	}

	// Page-count shrink math: only a shrink against a recorded count gates.
	if got := baselinePageShrink(base, 12); got != 0 {
		t.Errorf("equal page count must not gate, got %d", got)
	}
	if got := baselinePageShrink(base, 11); got != 1 {
		t.Errorf("shrunken page count must gate, got %d", got)
	}
	if got := baselinePageShrink(base, 13); got != 0 {
		t.Errorf("grown page count must not gate, got %d", got)
	}
	if got := baselinePageShrink(legacy, 1); got != 0 {
		t.Errorf("legacy baseline has no recorded count to gate on, got %d", got)
	}
}

// --- S-last hardening probes (M0042-S0004) ---

// Real Unity pages carry stray void-element close tags INSIDE #content-wrap: every
// ScriptReference page's feedback form emits <input ...></input> pairs. A close tag whose
// element never opened a subtree must not decrement the nesting counter - before this fix,
// two </input> tags unbalanced cd, collection ended levels early, and everything after the
// feedback form was silently invisible to the audit (a false-negative blind window on ~35K
// pages).
func TestAuditExtractSurvivesVoidCloseTags(t *testing.T) {
	const doc = `<html><body>
<div id="content-wrap">
<p>alpha beta</p>
<form><input type="text"></input><input type="submit"></input></form>
<p>gamma one</br>gamma two</p>
<p>omega closing words</p>
</div>
<div class="footer">outside content wrap</div>
</body></html>`
	tokens, _ := auditExtractTokens(doc)
	joined := " " + strings.Join(tokens, " ") + " "
	for _, want := range []string{"alpha", "gamma", "two", "omega", "words"} {
		if !strings.Contains(joined, " "+want+" ") {
			t.Errorf("token %q lost after stray void close tags: %v", want, tokens)
		}
	}
	if strings.Contains(joined, "outside") {
		t.Errorf("collection leaked past content-wrap close: %v", tokens)
	}
}

// End-to-end false-negative probe beyond the S0002 seeds: a page whose HTML has a real
// feedback-form-shaped <input></input> pair BEFORE its final paragraph, with that paragraph
// dropped from the markdown. If the extractor truncates at the stray closes, the loss is
// invisible; the audit must flag it.
func TestAuditFlagsLossAfterVoidCloseTag(t *testing.T) {
	root := t.TempDir()
	sourceDir := filepath.Join(root, "source")
	corpusDir := filepath.Join(root, "corpus")
	for _, d := range []string{
		filepath.Join(sourceDir, "Manual"),
		filepath.Join(sourceDir, "ScriptReference"),
		filepath.Join(corpusDir, "text", "Manual"),
	} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	kept := fixturePara(90, 0)
	dropped := fixturePara(90, 1)
	html := `<html><body><div id="content-wrap"><h1>VoidClose</h1><p>` + kept + `</p>` +
		`<form><input type="text"></input><input type="submit"></input></form>` +
		`<p>` + dropped + `</p></div></body></html>`
	if err := os.WriteFile(filepath.Join(sourceDir, "Manual", "VoidClose.html"), []byte(html), 0o644); err != nil {
		t.Fatal(err)
	}
	md := "---\ntitle: VoidClose\n---\n\nVoidClose\n\n" + kept + "\n"
	if err := os.WriteFile(filepath.Join(corpusDir, "text", "Manual", "VoidClose.md"), []byte(md), 0o644); err != nil {
		t.Fatal(err)
	}
	jsonl := `{"page_key":"Manual/VoidClose","section":"Manual","source_rel":"Manual/VoidClose.html","md_rel":"text/Manual/VoidClose.md"}` + "\n"
	if err := os.WriteFile(filepath.Join(corpusDir, "pages.jsonl"), []byte(jsonl), 0o644); err != nil {
		t.Fatal(err)
	}

	res, err := auditCorpus(sourceDir, corpusDir, 2, fixtureAuditConfig())
	if err != nil {
		t.Fatal(err)
	}
	var f *auditFlag
	for _, fl := range res.flagged {
		if fl.PageKey == "Manual/VoidClose" {
			f = fl
		}
	}
	if f == nil || f.MissingSpans == 0 {
		t.Fatalf("content loss after a stray void close tag not flagged (extractor truncated?): %+v", f)
	}
	if quotes := strings.Join(f.Quotes, " "); !strings.Contains(quotes, "pg90pr1w0") {
		t.Errorf("missing-text quotes must cover the dropped paragraph: %q", quotes)
	}
}

// A content-loss span's quote must cover the run's full token range: the last token of the
// final missing window belongs to the span ([startTok, endTokExclusive) = endWindow + n).
func TestAuditPageQuoteCoversFullSpan(t *testing.T) {
	ref := []string{"qa1", "qb2", "qc3", "qd4", "qe5", "qf6", "qg7", "qh8"}
	md := ref[:3] // tail dropped from qd4 onward
	cfg := auditConfig{shingleN: 3, maxDF: 4, minRun: 3, maxQuotes: 5}
	df := newDFCounter()
	for fp := range distinctShingles(ref, cfg.shingleN) {
		df.add(fp)
	}
	f := auditPage(pageRef{PageKey: "Manual/Tail"}, ref, md, df, cfg)
	if f == nil || f.MissingSpans == 0 {
		t.Fatalf("tail drop not flagged: %+v", f)
	}
	if quotes := strings.Join(f.Quotes, " "); !strings.Contains(quotes, "qh8") {
		t.Errorf("quote must include the final token of the last missing window: %q", quotes)
	}
}

// Pruned-source behavior: the standard post-build prune removes the section trees, and the
// audit must answer with the rematerialization command, not a bare stat error.
func TestAuditCorpusMissingSourceSection(t *testing.T) {
	_, corpusDir := buildAuditFixture(t)
	_, err := auditCorpus(t.TempDir(), corpusDir, 1, fixtureAuditConfig())
	if err == nil {
		t.Fatal("missing section dirs must error")
	}
	if !strings.Contains(err.Error(), "--keep-source") {
		t.Errorf("error must point at rematerialization via --keep-source: %v", err)
	}
}

// A partially pruned or torn source tree (dirs present, a page file gone) must also point
// at rematerialization instead of surfacing a bare read error.
func TestAuditCorpusMissingSourcePage(t *testing.T) {
	sourceDir, corpusDir := buildAuditFixture(t)
	if err := os.Remove(filepath.Join(sourceDir, "Manual", "Page00.html")); err != nil {
		t.Fatal(err)
	}
	_, err := auditCorpus(sourceDir, corpusDir, 2, fixtureAuditConfig())
	if err == nil {
		t.Fatal("missing source page must error")
	}
	if !strings.Contains(err.Error(), "--keep-source") {
		t.Errorf("error must point at rematerialization via --keep-source: %v", err)
	}
}

// An unbuilt corpus (no pages.jsonl) must say so instead of surfacing a bare open error.
func TestAuditCorpusMissingPagesJSONL(t *testing.T) {
	sourceDir, _ := buildAuditFixture(t)
	emptyCorpus := t.TempDir()
	_, err := auditCorpus(sourceDir, emptyCorpus, 1, fixtureAuditConfig())
	if err == nil {
		t.Fatal("missing pages.jsonl must error")
	}
	if !strings.Contains(err.Error(), "is the corpus built") {
		t.Errorf("error must hint the corpus is unbuilt: %v", err)
	}
}

// Defensive-input probes: a hostile-but-plausible config must not panic or deadlock.
func TestAuditPageToleratesNegativeMaxQuotes(t *testing.T) {
	ref := []string{"na1", "nb2", "nc3", "nd4", "ne5", "nf6"}
	cfg := auditConfig{shingleN: 3, maxDF: 4, minRun: 3, maxQuotes: -1}
	df := newDFCounter()
	for fp := range distinctShingles(ref, cfg.shingleN) {
		df.add(fp)
	}
	f := auditPage(pageRef{PageKey: "Manual/NegQuotes"}, ref, nil, df, cfg) // must not panic
	if f == nil || f.MissingSpans == 0 {
		t.Fatalf("fully missing page not flagged: %+v", f)
	}
}

func TestAuditParallelClampsWorkerCount(t *testing.T) {
	var mu sync.Mutex
	seen := 0
	if err := auditParallel(0, 3, func(i int) error {
		mu.Lock()
		seen++
		mu.Unlock()
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if seen != 3 {
		t.Errorf("ran %d jobs, want 3", seen)
	}
}

// clip must not cut a multi-byte rune in half when trimming a long quote.
func TestClipRespectsRuneBoundaries(t *testing.T) {
	long := strings.Repeat("a", 199) + "éé" // 199 ASCII + 2 two-byte runes
	got := clip(long)
	if !utf8.ValidString(got) {
		t.Errorf("clip produced invalid UTF-8: %q", got)
	}
}

// End-to-end over the synthetic corpus: baselining the two seeded pages makes the run gate
// zero new flags - the exact CI-bootstrap flow for the real corpus' accepted floor.
func TestAuditBaselineGatesFixtureRun(t *testing.T) {
	sourceDir, corpusDir := buildAuditFixture(t)
	res, err := auditCorpus(sourceDir, corpusDir, 4, fixtureAuditConfig())
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "baseline.json")
	if err := writeAuditBaseline(path, res); err != nil {
		t.Fatal(err)
	}
	base, err := loadAuditBaseline(path)
	if err != nil {
		t.Fatal(err)
	}
	if newFlags, stale := applyAuditBaseline(res.flagged, base); newFlags != 0 || stale != 0 {
		t.Errorf("self-baselined run: new=%d stale=%d, want 0/0", newFlags, stale)
	}

	// Blind-E2E probe replay (findings item 1): gut a BASELINED page's markdown body - its
	// flag must escape the baseline and gate, not hide behind its page key.
	gutted := filepath.Join(corpusDir, "text", "Manual", "TailTruncated.md")
	if err := os.WriteFile(gutted, []byte("---\ntitle: TailTruncated\n---\n\nTailTruncated\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	res2, err := auditCorpus(sourceDir, corpusDir, 4, fixtureAuditConfig())
	if err != nil {
		t.Fatal(err)
	}
	newFlags, _ := applyAuditBaseline(res2.flagged, base)
	if newFlags < 1 {
		t.Error("gutting a baselined page must produce a NEW gating flag (magnitude pin)")
	}

	// Blind-E2E probe replay (findings item 3, upstream variant): remove a page from BOTH
	// the source tree and pages.jsonl - count parity holds, but the baseline's recorded page
	// count catches the shrink.
	if err := os.Remove(filepath.Join(sourceDir, "Manual", "Page00.html")); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(corpusDir, "pages.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	var kept []string
	for _, line := range strings.Split(strings.TrimRight(string(raw), "\n"), "\n") {
		if !strings.Contains(line, "Page00") {
			kept = append(kept, line)
		}
	}
	if err := os.WriteFile(filepath.Join(corpusDir, "pages.jsonl"), []byte(strings.Join(kept, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	res3, err := auditCorpus(sourceDir, corpusDir, 4, fixtureAuditConfig())
	if err != nil {
		t.Fatal(err)
	}
	if got := baselinePageShrink(base, len(res3.pages)); got != 1 {
		t.Errorf("silent whole-page loss must gate via the baseline page count, shrink=%d", got)
	}
}

// Blind-E2E probe replay (findings item 3, corpus-side variant): pages.jsonl listing fewer
// pages than the source tree holds is a build regression or a mismatched pair - the audit
// must refuse rather than certify the smaller set.
func TestAuditCorpusRefusesPageCountMismatch(t *testing.T) {
	sourceDir, corpusDir := buildAuditFixture(t)
	raw, err := os.ReadFile(filepath.Join(corpusDir, "pages.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimRight(string(raw), "\n"), "\n")
	if err := os.WriteFile(filepath.Join(corpusDir, "pages.jsonl"), []byte(strings.Join(lines[:len(lines)-1], "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err = auditCorpus(sourceDir, corpusDir, 2, fixtureAuditConfig())
	if err == nil {
		t.Fatal("page-count mismatch must refuse the audit")
	}
	if !strings.Contains(err.Error(), "build regression") {
		t.Errorf("mismatch error must explain itself: %v", err)
	}
}

// A malformed pages.jsonl record is corpus corruption: silently skipping it would shrink the
// audited page set, so the audit must error instead.
func TestAuditCorpusRefusesMalformedPageRecord(t *testing.T) {
	_, corpusDir := buildAuditFixture(t)
	path := filepath.Join(corpusDir, "pages.jsonl")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, append(raw, []byte("{not json}\n")...), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := loadPageRefs(filepath.Dir(path)); err == nil || !strings.Contains(err.Error(), "malformed page record") {
		t.Errorf("malformed record must error with its line, got: %v", err)
	}
}

// Duplicate-family backstop (blind-E2E findings item 6): near-identical pages push every
// shingle above max-shingle-df, so a blanked family member produces no missing-run flag -
// the gating ratio-collapse tier must catch it instead.
func TestAuditRatioGateCatchesBlankedDuplicate(t *testing.T) {
	root := t.TempDir()
	sourceDir := filepath.Join(root, "source")
	corpusDir := filepath.Join(root, "corpus")
	for _, d := range []string{
		filepath.Join(sourceDir, "Manual"),
		filepath.Join(sourceDir, "ScriptReference"),
		filepath.Join(corpusDir, "text", "Manual"),
	} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	// Six byte-identical pages (only the file name differs): every shingle has DF=6 > 4.
	shared := make([]string, 40)
	for i := range shared {
		shared[i] = fmt.Sprintf("dupfam%d", i)
	}
	body := strings.Join(shared, " ")
	var jsonl strings.Builder
	for p := 0; p < 6; p++ {
		name := fmt.Sprintf("Dup%02d", p)
		html := `<html><body><div id="content-wrap"><p>` + body + `</p></div></body></html>`
		if err := os.WriteFile(filepath.Join(sourceDir, "Manual", name+".html"), []byte(html), 0o644); err != nil {
			t.Fatal(err)
		}
		md := body
		if p == 3 {
			md = "" // the blanked family member
		}
		if err := os.WriteFile(filepath.Join(corpusDir, "text", "Manual", name+".md"), []byte(md), 0o644); err != nil {
			t.Fatal(err)
		}
		jsonl.WriteString(fmt.Sprintf(
			`{"page_key":"Manual/%s","section":"Manual","source_rel":"Manual/%s.html","md_rel":"text/Manual/%s.md"}`+"\n",
			name, name, name))
	}
	if err := os.WriteFile(filepath.Join(corpusDir, "pages.jsonl"), []byte(jsonl.String()), 0o644); err != nil {
		t.Fatal(err)
	}
	res, err := auditCorpus(sourceDir, corpusDir, 2, fixtureAuditConfig())
	if err != nil {
		t.Fatal(err)
	}
	var blanked *auditFlag
	for _, f := range res.flagged {
		if f.PageKey == "Manual/Dup03" {
			blanked = f
		}
		if f.MissingSpans > 0 {
			t.Errorf("duplicate family must not produce missing-run flags, got %+v", f)
		}
	}
	if blanked == nil || !blanked.RatioGated || !blanked.gates() {
		t.Fatalf("blanked duplicate must trip the gating ratio collapse: %+v", blanked)
	}
}
