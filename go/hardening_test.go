package main

import (
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
	if !strings.Contains(p.Version, "6000.3") {
		t.Errorf("Version = %q, want to contain 6000.3", p.Version)
	}
	for _, leak := range []string{"CHROME NAV", "FOOTER", "scriptLeak"} {
		if strings.Contains(p.Body, leak) {
			t.Errorf("Body leaked stripped content %q: %q", leak, p.Body)
		}
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

func TestWriteMarkdownFallbacksAndLinkDedup(t *testing.T) {
	rec := record{Section: "Manual", PageID: "Foo", SourceRel: "Manual/Foo.html"}
	links := []link{{"A", "a.html"}, {"A", "a.html"}, {"B", "b.html"}}
	out := string(writeMarkdown(rec, links))

	if !strings.Contains(out, "section: Manual") {
		t.Error("missing front-matter section")
	}
	if !strings.Contains(out, "# Foo") {
		t.Error("empty title should fall back to page id in the heading")
	}
	if !strings.Contains(out, "[No extracted content]") {
		t.Error("empty body should emit the no-content marker")
	}
	if n := strings.Count(out, "A -> a.html"); n != 1 {
		t.Errorf("duplicate link not deduped: appeared %d times", n)
	}
}
