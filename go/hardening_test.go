package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// compactText is the text-normalization contract that determines derived corpus body
// quality: collapse intra-line whitespace to single spaces, drop spaces around newlines,
// cap blank-line runs at two, unescape entities, normalize CR/CRLF, trim the ends.
func TestCompactTextNormalization(t *testing.T) {
	cases := []struct{ in, want string }{
		{"a    b", "a b"},
		{"a\t\tb", "a b"},
		{"  hello  ", "hello"},
		{"a\r\nb", "a\nb"},
		{"a &amp; b", "a & b"},
		{"a   \nb", "a\nb"},      // trailing spaces before newline dropped
		{"a\n   b", "a\nb"},      // leading spaces after newline dropped
		{"a\n\n\n\nb", "a\n\nb"}, // blank-line run capped at 2
	}
	for _, c := range cases {
		if got := compactText(c.in); got != c.want {
			t.Errorf("compactText(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// parseHTML is the core extraction contract: title with the Unity prefix stripped, the
// canonical link, headings, in-content anchor links, and a body limited to #content-wrap
// with script and page-chrome (header/sidebar/footer) stripped out.
func TestParseHTMLExtractionContract(t *testing.T) {
	const doc = `<html><head>
<title>Unity - Manual: Rigidbody2D</title>
<link rel="canonical" href="https://docs.unity3d.com/Manual/Rigidbody2D.html">
</head><body>
<div class="header">CHROME NAV</div>
<div id="content-wrap">
<h1>Rigidbody2D</h1>
<p>The <a href="Physics2D.html">Physics 2D</a> body.</p>
<script>var scriptLeak = 1 > 0;</script>
<p>Version: 6000.3</p>
</div>
<div class="footer">FOOTER</div>
</body></html>`

	p := parseHTML(doc)

	if p.Title != "Rigidbody2D" {
		t.Errorf("Title = %q, want Rigidbody2D (prefix must be stripped)", p.Title)
	}
	if p.Canonical != "https://docs.unity3d.com/Manual/Rigidbody2D.html" {
		t.Errorf("Canonical = %q", p.Canonical)
	}
	if len(p.Headings) != 1 || p.Headings[0] != "Rigidbody2D" {
		t.Errorf("Headings = %v, want [Rigidbody2D]", p.Headings)
	}
	if len(p.Links) != 1 || p.Links[0].Text != "Physics 2D" || p.Links[0].Href != "Physics2D.html" {
		t.Errorf("Links = %+v", p.Links)
	}
	if !strings.Contains(p.Body, "Physics 2D") {
		t.Errorf("Body missing anchor text: %q", p.Body)
	}
	for _, leak := range []string{"CHROME NAV", "FOOTER", "scriptLeak"} {
		if strings.Contains(p.Body, leak) {
			t.Errorf("Body leaked stripped content %q: %q", leak, p.Body)
		}
	}
}

// Glossary tooltip popups and the Switch to Manual button are page chrome: the visible
// term must survive, the tooltiptext subtree (definition + More info + See in Glossary
// boilerplate) and the switch button must not reach the body or the link list.
func TestParseHTMLStripsTooltipAndSwitchLink(t *testing.T) {
	const doc = `<div id="content-wrap">
<p>Explore the <span class="tooltip" tabindex="0"><strong>Camera</strong><span class="tooltiptext">A component which creates an image. <a class="tooltipMoreInfoLink" href="CamerasOverview.html">More info</a><br/><span class="tooltipGlossaryLink">See in <a href="Glossary.html#Camera">Glossary</a></span></span></span> component window.</p>
<a class="switch-link gray-btn sbtn left show" href="../Manual/class-Rigidbody.html">Switch to Manual</a>
</div>`
	p := parseHTML(doc)
	if !strings.Contains(p.Body, "Camera") {
		t.Errorf("visible tooltip term must survive: %q", p.Body)
	}
	for _, leak := range []string{"See in", "More info", "Switch to Manual", "A component which creates"} {
		if strings.Contains(p.Body, leak) {
			t.Errorf("Body leaked tooltip/switch chrome %q: %q", leak, p.Body)
		}
	}
	for _, l := range p.Links {
		if l.Text == "More info" || l.Text == "Glossary" || l.Text == "Switch to Manual" {
			t.Errorf("chrome link leaked into link list: %+v", l)
		}
	}
	// The <br/> inside tooltiptext is the real-page shape; a depth-tracking slip there
	// ends the skip early AND corrupts contentDepth, dropping text after the tooltip.
	if !strings.Contains(p.Body, "component window") {
		t.Errorf("content after the tooltip must survive: %q", p.Body)
	}
}

// Adjacent table cells must not fuse: without a td/th separator the parser glued a member
// name to its description (e.g. "AllAreasArea mask..."), which then tokenized as one FTS token
// so the member name could not be found in the body. The audit's first real catch (M0042-S0001).
func TestParseHTMLSeparatesTableCells(t *testing.T) {
	const doc = `<div id="content-wrap"><h2>Static Properties</h2>
<table class="list"><tbody>
<tr><td>AllAreas</td><td>Area mask constant that includes all areas</td></tr>
<tr><th>Method</th><th>Description</th></tr>
</tbody></table></div>`
	p := parseHTML(doc)
	if strings.Contains(p.Body, "AllAreasArea") {
		t.Errorf("adjacent cell texts fused: %q", p.Body)
	}
	if strings.Contains(p.Body, "areasMethod") || strings.Contains(p.Body, "MethodDescription") {
		t.Errorf("adjacent header/row cells fused: %q", p.Body)
	}
	for _, want := range []string{"AllAreas", "Area mask constant that includes all areas", "Method", "Description"} {
		if !strings.Contains(p.Body, want) {
			t.Errorf("cell text %q missing from body: %q", want, p.Body)
		}
	}
}

// A member-name link whose aria-label/href contains a chrome token as a substring (e.g.
// "navigation" inside "AndroidNavigation") must NOT be skipped: chrome matching is on structural
// attributes only, not on descriptive aria-label prose. Regression for the dropped enum names
// the audit found (M0042-S0001).
func TestParseHTMLKeepsMemberLinkWithChromeTokenInAriaLabel(t *testing.T) {
	const doc = `<div id="content-wrap"><table class="list"><tbody>
<tr><td class="lbl"><a href="Android.AndroidNavigation.Undefined.html" aria-label="Go to Android.AndroidNavigation.Undefined.html">Undefined</a></td>
<td class="desc">Mirrors the Android property value NAVIGATION_UNDEFINED.</td></tr>
</tbody></table></div>`
	p := parseHTML(doc)
	if !strings.Contains(p.Body, "Undefined") {
		t.Errorf("member name dropped by aria-label chrome-token match: %q", p.Body)
	}
	found := false
	for _, l := range p.Links {
		if l.Text == "Undefined" {
			found = true
		}
	}
	if !found {
		t.Errorf("member link not captured: %+v", p.Links)
	}
}

func TestParseHTMLDoesNotPanicOnMalformed(t *testing.T) {
	// Unterminated comment and unclosed tag must not panic; content before them survives.
	p := parseHTML(`<div id="content-wrap"><p>keep<!-- never closed`)
	if !strings.Contains(p.Body, "keep") {
		t.Errorf("expected surviving content, got %q", p.Body)
	}
}

// findTagEnd must treat '>' inside a quoted attribute value as ordinary text, not the
// tag terminator - otherwise attributes containing '>' would truncate the tag.
func TestFindTagEndRespectsQuotes(t *testing.T) {
	s := `p title="a>b">` // the '>' at index 10 is inside quotes; the real end is index 13
	if got := findTagEnd(s, 0); got != 13 {
		t.Errorf("findTagEnd = %d, want 13", got)
	}
}

func TestParseAttrs(t *testing.T) {
	attrs := parseAttrs(`a href="x.html" class='c' data-x=y disabled`)
	want := map[string]string{"href": "x.html", "class": "c", "data-x": "y", "disabled": ""}
	for k, v := range want {
		if attrs[k] != v {
			t.Errorf("attr %q = %q, want %q (all: %v)", k, attrs[k], v, attrs)
		}
	}
	if got := parseAttrs(`img alt="x &amp; y"`)["alt"]; got != "x & y" {
		t.Errorf("entity in attr value not unescaped: %q", got)
	}
}

func TestPageIDForStripsSectionAndExtension(t *testing.T) {
	root := t.TempDir()
	id, err := pageIDFor("Manual", filepath.Join(root, "Manual", "Physics", "Colliders.html"), root)
	if err != nil || id != "Physics/Colliders" {
		t.Errorf("pageIDFor nested = %q, err=%v, want Physics/Colliders", id, err)
	}
	id, _ = pageIDFor("ScriptReference", filepath.Join(root, "ScriptReference", "Rigidbody2D.html"), root)
	if id != "Rigidbody2D" {
		t.Errorf("pageIDFor flat = %q, want Rigidbody2D", id)
	}
}

func TestSha256HexDeterministic(t *testing.T) {
	const abc = "ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad"
	if got := sha256Hex([]byte("abc")); got != abc {
		t.Errorf("sha256Hex(abc) = %q, want %q", got, abc)
	}
	if sha256Hex([]byte("abc")) != sha256Hex([]byte("abc")) {
		t.Error("sha256Hex not deterministic")
	}
	if sha256Hex([]byte("abc")) == sha256Hex([]byte("abd")) {
		t.Error("sha256Hex collided on distinct input")
	}
}

// safePrepareOutput is a destructive-operation guard: it may only RemoveAll an existing
// output directory that carries the builder's own marker file. An unmarked directory must
// be refused untouched - this is what stops a mistyped --output from nuking real data.
func TestSafePrepareOutputRefusesUnmarkedDir(t *testing.T) {
	// Fresh (non-existent) output: created and marked.
	fresh := filepath.Join(t.TempDir(), "out")
	if err := safePrepareOutput(fresh); err != nil {
		t.Fatalf("fresh output should succeed: %v", err)
	}
	if _, err := os.Stat(filepath.Join(fresh, ".unity-doc-agent-corpus")); err != nil {
		t.Fatalf("marker not written: %v", err)
	}

	// Adversarial poke: an existing dir WITHOUT the marker, holding real data, must be
	// refused and left intact.
	unmarked := t.TempDir()
	sentinel := filepath.Join(unmarked, "precious.txt")
	if err := os.WriteFile(sentinel, []byte("do not delete"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := safePrepareOutput(unmarked); err == nil {
		t.Fatal("expected refusal on unmarked existing directory")
	}
	if _, err := os.Stat(sentinel); err != nil {
		t.Fatalf("guard deleted data it should have refused to touch: %v", err)
	}

	// A previously-marked dir may be reused (wiped and re-marked).
	marked := t.TempDir()
	if err := os.WriteFile(filepath.Join(marked, ".unity-doc-agent-corpus"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := safePrepareOutput(marked); err != nil {
		t.Fatalf("marked dir should be reusable: %v", err)
	}
}

// prepareFetchDestination mirrors safePrepareOutput's guard for the fetch side:
// --force may only delete a directory carrying the fetch marker.
func TestPrepareFetchDestinationRefusesUnmarkedDir(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "docs")
	if err := prepareFetchDestination(missing, false); err != nil {
		t.Fatalf("non-existent destination should pass: %v", err)
	}

	unmarked := t.TempDir()
	sentinel := filepath.Join(unmarked, "precious.txt")
	if err := os.WriteFile(sentinel, []byte("do not delete"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := prepareFetchDestination(unmarked, false); err == nil {
		t.Fatal("existing destination without --force must be refused")
	}
	if err := prepareFetchDestination(unmarked, true); err == nil {
		t.Fatal("--force on an unmarked directory must be refused")
	}
	if _, err := os.Stat(sentinel); err != nil {
		t.Fatalf("guard deleted data it should have refused to touch: %v", err)
	}

	invalid := t.TempDir()
	if err := os.WriteFile(filepath.Join(invalid, fetchMarkerName), []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := prepareFetchDestination(invalid, true); err == nil {
		t.Fatal("an empty marker must not authorize recursive deletion")
	}

	marked := t.TempDir()
	writeFetchMarker(t, marked, "")
	if err := prepareFetchDestination(marked, true); err != nil {
		t.Fatalf("marked dir with --force should be replaceable: %v", err)
	}
	if _, err := os.Stat(marked); err == nil {
		t.Fatal("marked dir should have been removed")
	}
}

// Host pinning must hold on every redirect hop, not just the initial URL: TLS to the
// pinned hosts is the only integrity control on the download.
func TestHTTPClientRefusesOffHostRedirect(t *testing.T) {
	evil := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("evil bytes"))
	}))
	defer evil.Close()
	redirector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, evil.URL+"/UnityDocumentation.zip", http.StatusFound)
	}))
	defer redirector.Close()

	resp, err := httpClient.Get(redirector.URL)
	if err == nil {
		resp.Body.Close()
		t.Fatal("redirect to an unpinned host must be refused")
	}
	if !strings.Contains(err.Error(), "refused") {
		t.Errorf("unexpected refusal error: %v", err)
	}

	for _, url := range []string{
		"https://docs.unity3d.com/x",
		"https://cloudmedia-docs.unity3d.com/x",
		"https://storage.googleapis.com/docscloudstorage/en/2019.4/UnityDocumentation.zip",
	} {
		req, _ := http.NewRequest("GET", url, nil)
		if err := httpClient.CheckRedirect(req, nil); err != nil {
			t.Errorf("pinned URL %s wrongly refused: %v", url, err)
		}
	}

	// Tightened pinning (blind-E2E findings item 9): a pinned HOST is not enough - a hop must
	// stay on https, and on the shared GCS host it must stay inside the docscloudstorage
	// bucket. TLS is the only integrity control on the download, and storage.googleapis.com
	// serves arbitrary tenants' buckets.
	for _, url := range []string{
		"http://docs.unity3d.com/x",                        // scheme downgrade
		"https://storage.googleapis.com/evil-bucket/x.zip", // foreign GCS bucket
		"https://storage.googleapis.com/x.zip",             // bucketless GCS path
	} {
		req, _ := http.NewRequest("GET", url, nil)
		if err := httpClient.CheckRedirect(req, nil); err == nil {
			t.Errorf("hop to %s must be refused", url)
		}
	}
}

// The source verb must not misdiagnose a misspelled page as missing docs: with the extracted
// tree present and no retained zip, the error names the page and points at the spelling.
func TestWriteSourceHTMLDistinguishesMissingPageFromMissingDocs(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "Manual"), 0o755); err != nil {
		t.Fatal(err)
	}
	_, err := writeSourceHTML(root, "Manual/NoSuchPage.html", io.Discard)
	if err == nil {
		t.Fatal("missing page must error")
	}
	if !strings.Contains(err.Error(), "source_rel spelling") {
		t.Errorf("want the misspelling diagnosis, got: %v", err)
	}

	// With no extracted tree AND no zip, the original re-run-fetch diagnosis still applies.
	empty := t.TempDir()
	_, err = writeSourceHTML(empty, "Manual/NoSuchPage.html", io.Discard)
	if err == nil || !strings.Contains(err.Error(), "re-run fetch") {
		t.Errorf("want the missing-docs diagnosis, got: %v", err)
	}
}

// ftsSanitize is the fallback that keeps dotted API names and stray operators from
// surfacing FTS5 syntax errors: reduce to alphanumeric terms, drop single characters.
func TestFtsSanitize(t *testing.T) {
	cases := []struct{ in, want string }{
		{"Rigidbody.MovePosition", "Rigidbody MovePosition"},
		{`"addressables memory"`, "addressables memory"},
		{"ab - cd", "ab cd"},
		{"a - b", ""},
		{"---", ""},
	}
	for _, c := range cases {
		if got := ftsSanitize(c.in); got != c.want {
			t.Errorf("ftsSanitize(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestWriteMarkdownFallbacksAndLinkDedup(t *testing.T) {
	rec := record{Section: "Manual", PageID: "Foo", SourceRel: "Manual/Foo.html"}
	links := []link{
		{"A", "a.html"},
		{"A", "a.html"},
		{"A2", "a.html"},
		{"Anchored", "a.html#frag"},
		{"Self", "Foo.html"},
		{"SelfFrag", "Foo.html#part"},
		{"FragOnly", "#top"},
		{"B", "b.html"},
	}
	out := string(writeMarkdown(rec, links))

	if !strings.Contains(out, "section: Manual") {
		t.Error("missing front-matter section")
	}
	if !strings.Contains(out, "title: Foo") {
		t.Error("empty title should fall back to page id in the front matter")
	}
	if !strings.Contains(out, "[No extracted content]") {
		t.Error("empty body should emit the no-content marker")
	}
	for _, banned := range []string{"# Foo", "Source:", "Canonical:", "## Content\n"} {
		if strings.Contains(out, banned) {
			t.Errorf("body must not repeat front-matter metadata, found %q", banned)
		}
	}
	if n := strings.Count(out, "-> a.html"); n != 2 {
		t.Errorf("want the deduped a.html link plus its distinct-fragment variant, got %d a.html links", n)
	}
	if strings.Contains(out, "A2") {
		t.Error("second text for an already-seen href should be dropped")
	}
	for _, banned := range []string{"Foo.html", "#top"} {
		if strings.Contains(strings.SplitN(out, "## Content Links", 2)[1], banned) {
			t.Errorf("self or fragment-only link leaked into Content Links: %q", banned)
		}
	}
	if got := string(writeMarkdown(rec, []link{{"Self", "Foo.html"}})); strings.Contains(got, "## Content Links") {
		t.Error("Content Links header must be omitted when every link is filtered out")
	}
}
