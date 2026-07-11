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
	"testing"
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
	return auditConfig{shingleN: 5, maxDF: 4, minRun: 5, ratioFactor: 0.4, ratioMinTok: 30, maxQuotes: 5}
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
// are baselined, NEW flags gate, and entries that stop flagging are reported as stale.
func TestAuditBaselinePartitionAndRoundTrip(t *testing.T) {
	mk := func(key string, spans int) *auditFlag {
		return &auditFlag{PageKey: key, MissingSpans: spans}
	}
	flagged := []*auditFlag{mk("Manual/A", 1), mk("Manual/B", 2), mk("Manual/RatioOnly", 0)}

	// No baseline: every content-loss flag is new; ratio-only flags never count.
	if newFlags, stale := applyAuditBaseline(flagged, nil); newFlags != 2 || stale != 0 {
		t.Errorf("nil baseline: new=%d stale=%d, want 2/0", newFlags, stale)
	}

	// Round-trip through the file format.
	path := filepath.Join(t.TempDir(), "baseline.json")
	if err := writeAuditBaseline(path, flagged); err != nil {
		t.Fatal(err)
	}
	base, err := loadAuditBaseline(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(base.PageKeys) != 2 || base.PageKeys[0] != "Manual/A" || base.PageKeys[1] != "Manual/B" {
		t.Fatalf("baseline must hold the sorted content-loss keys only: %v", base.PageKeys)
	}

	// Full coverage: nothing new, flags marked baselined.
	flagged = []*auditFlag{mk("Manual/A", 1), mk("Manual/B", 2)}
	if newFlags, stale := applyAuditBaseline(flagged, base); newFlags != 0 || stale != 0 {
		t.Errorf("covered run: new=%d stale=%d, want 0/0", newFlags, stale)
	}
	if !flagged[0].Baselined || !flagged[1].Baselined {
		t.Error("covered flags must be marked baselined")
	}

	// A new regression alongside the known floor gates; a fixed page goes stale.
	flagged = []*auditFlag{mk("Manual/A", 1), mk("Manual/NewRegression", 3)}
	newFlags, stale := applyAuditBaseline(flagged, base)
	if newFlags != 1 || stale != 1 {
		t.Errorf("new+stale run: new=%d stale=%d, want 1/1", newFlags, stale)
	}
	if flagged[1].Baselined {
		t.Error("unbaselined new flag must not be marked baselined")
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
	if err := writeAuditBaseline(path, res.flagged); err != nil {
		t.Fatal(err)
	}
	base, err := loadAuditBaseline(path)
	if err != nil {
		t.Fatal(err)
	}
	if newFlags, stale := applyAuditBaseline(res.flagged, base); newFlags != 0 || stale != 0 {
		t.Errorf("self-baselined run: new=%d stale=%d, want 0/0", newFlags, stale)
	}
}
