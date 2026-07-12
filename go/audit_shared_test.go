package main

// Seeded-regression proof for shared-content (corpus-common) loss detection (M0042-S6). These
// tests plant the failure class the blind-E2E validation surfaced (findings item 2): a sentence
// repeated across many pages, stripped from the derived Markdown. The page-local shingle
// invariant ignores it (high ref-DF), so before S6 the strip passed clean. Part A (live md-DF)
// catches a partial strip; Part B (the shared-content manifest) catches a total strip.

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// sharedContentSentence repeats verbatim on every fixture page, so its shingles have ref-DF ==
// page count (well above max-shingle-df) - a page-unique check can never see it. 10 tokens ->
// 6 shingles, enough for a min-run=5 miss when stripped.
const sharedContentSentence = "sharedbodyalpha sharedbodybeta sharedbodygamma sharedbodydelta " +
	"sharedbodyepsilon sharedbodyzeta sharedbodyeta sharedbodytheta sharedbodyiota sharedbodykappa"

const sharedFixturePages = 8

// buildSharedContentFixture writes a synthetic corpus where every page carries a page-unique
// paragraph plus the shared sentence (in both HTML and Markdown) and corpus-uniform footer
// chrome (HTML only). strip[p] removes the shared sentence from page p's Markdown, simulating a
// transform regression that drops shared content.
func buildSharedContentFixture(t *testing.T, strip map[int]bool) (string, string) {
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
	for p := 0; p < sharedFixturePages; p++ {
		name := fmt.Sprintf("Shared%02d", p)
		key := "Manual/" + name
		uniq := fixturePara(p, 0)
		html := "<html><head><title>Unity - Manual: " + name + "</title></head><body>\n" +
			"<div id=\"content-wrap\">\n" +
			"<div class=\"breadcrumb\"><a href=\"index.html\">crumb</a></div>\n" +
			"<h1>" + name + "</h1>\n" +
			"<p>" + uniq + "</p>\n" +
			"<p>" + sharedContentSentence + "</p>\n" +
			"<div class=\"footer\"><div class=\"feedbackbox\">" + fixtureChrome + "</div></div>\n" +
			"</div>\n</body></html>\n"
		if err := os.WriteFile(filepath.Join(sourceDir, "Manual", name+".html"), []byte(html), 0o644); err != nil {
			t.Fatal(err)
		}
		mdBody := uniq
		if !strip[p] {
			mdBody += "\n\n" + sharedContentSentence
		}
		md := "---\ntitle: " + name + "\n---\n\n" + mdBody + "\n"
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

func stripAllPages() map[int]bool {
	m := map[int]bool{}
	for p := 0; p < sharedFixturePages; p++ {
		m[p] = true
	}
	return m
}

func contentLossFlag(res *auditResult, key string) *auditFlag {
	for _, f := range res.flagged {
		if f.PageKey == key && f.MissingSpans > 0 {
			return f
		}
	}
	return nil
}

// Part A: a PARTIAL strip (shared sentence removed from one page, still present on the others)
// is caught live, with no manifest - the surviving pages keep the shingle's md-DF high, so it
// classifies as content and its absence on the stripped page is a miss.
func TestAuditSharedContentPartialStripFlaggedLive(t *testing.T) {
	sourceDir, corpusDir := buildSharedContentFixture(t, map[int]bool{3: true})
	res, err := auditCorpus(sourceDir, corpusDir, 4, fixtureAuditConfig(), nil)
	if err != nil {
		t.Fatal(err)
	}
	f := contentLossFlag(res, "Manual/Shared03")
	if f == nil {
		t.Fatalf("partial shared-content strip on Shared03 not flagged: %+v", res.flagged)
	}
	// The run must reach INTO the shared content, not merely the page-unique boundary shingles
	// (which alone are only n-1=4 windows, under the min-run bar).
	if q := strings.Join(f.Quotes, " "); !strings.Contains(q, "sharedbodyalpha") {
		t.Errorf("missing quote must cover the stripped shared sentence: %q", q)
	}
	for p := 0; p < sharedFixturePages; p++ {
		if p == 3 {
			continue
		}
		if g := contentLossFlag(res, fmt.Sprintf("Manual/Shared%02d", p)); g != nil {
			t.Errorf("unstripped page Shared%02d wrongly flagged: %+v", p, g)
		}
	}
	if res.sharedLoss != 0 {
		t.Errorf("no manifest supplied, shared-loss gate must be inert, got %d", res.sharedLoss)
	}
}

// Part B: a TOTAL strip (shared sentence gone from EVERY page's Markdown) is invisible to the
// live check - md-DF collapses to 0 so the shingle re-reads as chrome - and is caught ONLY
// against the manifest. This is the fundamental single-run limit the manifest exists to close.
func TestAuditSharedContentTotalStripNeedsManifest(t *testing.T) {
	cfg := fixtureAuditConfig()

	// Clean corpus: pin the content set. Only the shared sentence qualifies (page-unique paras
	// are low-DF; footer chrome has md-DF 0), so exactly its 6 shingles are pinned.
	cleanSrc, cleanCorpus := buildSharedContentFixture(t, nil)
	clean, err := auditCorpus(cleanSrc, cleanCorpus, 4, cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := len(clean.contentShingles); got != 6 {
		t.Fatalf("content set must pin only the 6 shared-sentence shingles, got %d", got)
	}
	manifestPath := filepath.Join(t.TempDir(), "shared-baseline.json")
	if err := writeSharedBaseline(manifestPath, clean.contentShingles, cfg); err != nil {
		t.Fatal(err)
	}
	shared, err := loadSharedBaseline(manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	if len(shared.set) != 6 {
		t.Fatalf("loaded manifest must round-trip 6 fingerprints, got %d", len(shared.set))
	}

	// Total strip, NO manifest: the live check is blind (proves the blind spot is real).
	stripSrc, stripCorpus := buildSharedContentFixture(t, stripAllPages())
	blind, err := auditCorpus(stripSrc, stripCorpus, 4, cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range blind.flagged {
		if f.MissingSpans > 0 {
			t.Errorf("total strip must be invisible to the live check, but %s flagged: %+v", f.PageKey, f)
		}
	}
	if blind.sharedLoss != 0 {
		t.Errorf("no manifest: shared-loss must be 0, got %d", blind.sharedLoss)
	}

	// Total strip, WITH manifest: every pinned shingle is still shared upstream (ref-DF 8) but
	// gone downstream (md-DF 0) -> collapsed -> gate.
	caught, err := auditCorpus(stripSrc, stripCorpus, 4, cfg, shared)
	if err != nil {
		t.Fatal(err)
	}
	if caught.sharedLoss == 0 {
		t.Fatal("total shared-content strip must gate against the manifest, got shared-loss 0")
	}
	if len(caught.sharedQuotes) == 0 || !strings.Contains(strings.Join(caught.sharedQuotes, " "), "sharedbody") {
		t.Errorf("collapsed report must quote the missing shared content: %v", caught.sharedQuotes)
	}

	// Clean corpus WITH the manifest: nothing collapsed, no false gate.
	cleanGated, err := auditCorpus(cleanSrc, cleanCorpus, 4, cfg, shared)
	if err != nil {
		t.Fatal(err)
	}
	if cleanGated.sharedLoss != 0 {
		t.Errorf("clean corpus must not trip the shared-content gate, got %d", cleanGated.sharedLoss)
	}
}

// The manifest fingerprint codec round-trips an unsorted, duplicated fingerprint list into the
// same membership set, and stays compact (delta-varint-base64).
func TestSharedBaselineFingerprintCodec(t *testing.T) {
	fps := []uint64{42, 1, 1 << 40, 5, 42, 0}
	set, err := decodeFingerprints(encodeFingerprints(fps))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []uint64{0, 1, 5, 42, 1 << 40} {
		if _, ok := set[want]; !ok {
			t.Errorf("fingerprint %d missing after round-trip", want)
		}
	}
	if len(set) != 5 {
		t.Errorf("duplicates must collapse: got %d distinct, want 5", len(set))
	}
	if _, err := decodeFingerprints("not_base64!!"); err == nil {
		t.Error("corrupt manifest fingerprints must error, not silently yield an empty set")
	}
}

// --- S7 hardening probes (round 2, adversarial review of the S5/S6 surfaces) ---

// A shared-content manifest generated under one shingle geometry cannot gate a run using a
// different one: the pinned fingerprints are width-n hashes, so at another --shingle-n every
// pinned fingerprint's document frequency reads as 0 and Part B silently passes even through a
// total strip. The guard must refuse the mismatch rather than certify a gate that cannot fire.
func TestSharedBaselineConfigMismatchRefused(t *testing.T) {
	cfg := fixtureAuditConfig()
	src, corpus := buildSharedContentFixture(t, nil)
	clean, err := auditCorpus(src, corpus, 4, cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "shared-baseline.json")
	if err := writeSharedBaseline(path, clean.contentShingles, cfg); err != nil {
		t.Fatal(err)
	}
	sb, err := loadSharedBaseline(path)
	if err != nil {
		t.Fatal(err)
	}

	if mm := sb.configMismatch(cfg); mm != "" {
		t.Errorf("matching config must report no mismatch, got %q", mm)
	}
	wrong := cfg
	wrong.shingleN = cfg.shingleN + 1
	if mm := sb.configMismatch(wrong); mm == "" {
		t.Error("a drifted --shingle-n must be reported as a config mismatch")
	}

	// Prove the silent failure the guard exists to prevent: audited at the wrong width, a TOTAL
	// strip leaves the manifest completely inert (0 collapsed) though every shared sentence is
	// gone. This is why configMismatch must refuse before the gate ever runs.
	stripSrc, stripCorpus := buildSharedContentFixture(t, stripAllPages())
	blind, err := auditCorpus(stripSrc, stripCorpus, 4, wrong, sb)
	if err != nil {
		t.Fatal(err)
	}
	if blind.sharedLoss != 0 {
		t.Fatalf("sanity: at a mismatched width the manifest cannot see collapse, got sharedLoss=%d", blind.sharedLoss)
	}

	// A manifest that predates the recorded-config fields (all zero) is not second-guessed - the
	// guard degrades to the prior behavior instead of hard-failing on an old file.
	sb.ShingleN, sb.MaxShingleDF, sb.ContentMinDF = 0, 0, 0
	if mm := sb.configMismatch(cfg); mm != "" {
		t.Errorf("unspecified manifest config must not trip the guard, got %q", mm)
	}
}

// False-positive probe: a shared sentence GENUINELY removed upstream (gone from both the source
// HTML and the derived Markdown across the whole corpus, e.g. Unity reworded boilerplate in a new
// docs version) is legitimate churn, not a transform regression. Part B anchors on ref-DF >
// max-df precisely so this does not gate - a naive "pinned shingle absent from the Markdown"
// check would false-alarm on every such edit.
func TestSharedBaselineIgnoresLegitimateSourceRemoval(t *testing.T) {
	cfg := fixtureAuditConfig()
	src, corpus := buildSharedContentFixture(t, nil)
	clean, err := auditCorpus(src, corpus, 4, cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(clean.contentShingles) == 0 {
		t.Fatal("fixture must pin some shared content to make the probe meaningful")
	}
	path := filepath.Join(t.TempDir(), "shared-baseline.json")
	if err := writeSharedBaseline(path, clean.contentShingles, cfg); err != nil {
		t.Fatal(err)
	}
	sb, err := loadSharedBaseline(path)
	if err != nil {
		t.Fatal(err)
	}

	rmSrc, rmCorpus := buildNoSharedFixture(t)
	res, err := auditCorpus(rmSrc, rmCorpus, 4, cfg, sb)
	if err != nil {
		t.Fatal(err)
	}
	if res.sharedLoss != 0 {
		t.Errorf("legitimate corpus-wide source removal must not trip the shared-content gate, got sharedLoss=%d (%v)", res.sharedLoss, res.sharedQuotes)
	}
}

// buildNoSharedFixture writes the same page set as buildSharedContentFixture but with the shared
// sentence absent from BOTH the source HTML and the Markdown - the "boilerplate genuinely deleted
// upstream" state, where the pinned shingles' ref-DF has dropped to 0.
func buildNoSharedFixture(t *testing.T) (string, string) {
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
	for p := 0; p < sharedFixturePages; p++ {
		name := fmt.Sprintf("Shared%02d", p)
		uniq := fixturePara(p, 0)
		html := "<html><body><div id=\"content-wrap\"><h1>" + name + "</h1><p>" + uniq + "</p></div></body></html>\n"
		if err := os.WriteFile(filepath.Join(sourceDir, "Manual", name+".html"), []byte(html), 0o644); err != nil {
			t.Fatal(err)
		}
		md := "---\ntitle: " + name + "\n---\n\n" + uniq + "\n"
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
	return sourceDir, corpusDir
}
