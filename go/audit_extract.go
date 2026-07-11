package main

// Independent reference text extractor for the content-lossless audit.
//
// This file shares NO code with go/html_parser.go on purpose. The audit's whole value is
// that a bug in the production parser cannot hide itself here: the two extractors use
// different algorithms, so a page the parser silently truncates still yields its full text
// through this path, and the mismatch surfaces. Keep this boundary intact - do not refactor
// shared helpers out of html_parser.go into here or vice versa.
//
// The algorithm is deliberately dumb: a tag scanner that skips non-visible subtrees
// (script/style/svg/head) and page-local nav chrome, decodes HTML entities, concatenates the
// remaining text - inserting a separator at BLOCK-level boundaries only, so an inline tag mid
// word (e.g. <code>ChangeEvent</code>s) does not split a token while adjacent table cells still
// stay apart - and finally splits into runs of letters and digits. Corpus-wide shingle frequency
// (see audit.go) does the remaining chrome-vs-content discrimination.

import (
	"html"
	"strings"
	"unicode"
)

// auditSkipSubtrees are elements whose descendant text is never visible page content.
// head carries <title>/<meta>/<link> (not rendered as body text); the rest are non-textual.
// This is intentionally a different, minimal list from the parser's skipTags + skipClassOrID.
var auditSkipSubtrees = map[string]bool{
	"head": true, "script": true, "style": true, "svg": true, "noscript": true, "template": true,
}

// auditVoidElements never open a subtree, so a start tag for one must not push a skip level.
var auditVoidElements = map[string]bool{
	"area": true, "base": true, "br": true, "col": true, "embed": true, "hr": true, "img": true,
	"input": true, "link": true, "meta": true, "param": true, "source": true, "track": true, "wbr": true,
}

// auditBlockElements are the elements that introduce a text-separator boundary: text on their
// two sides is distinct and must not fuse into one token. Everything NOT in this set is treated
// as inline (span, a, code, b, i, sub, sup, wbr, ...), so a tag inside a word joins rather than
// splits. This is the audit's OWN independent block classification - not imported from the
// parser - and it deliberately covers block elements the parser does not separate (dl/dt/dd,
// article, ...): where the parser fuses across one of those and this extractor does not, that is
// a real transform defect to surface, exactly as td/th cell fusion was.
var auditBlockElements = map[string]bool{
	"p": true, "div": true, "section": true, "article": true, "aside": true, "nav": true,
	"header": true, "footer": true, "main": true, "figure": true, "figcaption": true,
	"table": true, "thead": true, "tbody": true, "tfoot": true, "tr": true, "td": true,
	"th": true, "caption": true, "colgroup": true, "ul": true, "ol": true, "li": true,
	"dl": true, "dt": true, "dd": true, "pre": true, "blockquote": true, "address": true,
	"h1": true, "h2": true, "h3": true, "h4": true, "h5": true, "h6": true, "hr": true,
	"br": true, "form": true, "fieldset": true, "legend": true, "details": true, "summary": true,
}

// navSkipTokens are class/id fragments for page-LOCAL navigation chrome that lives inside the
// content container: the breadcrumb (ancestor page titles) and the prev/next bar (adjacent
// page titles). Unlike headers, sidebars, footers, and glossary tooltips - whose text repeats
// across the corpus and is discriminated by shingle frequency - this chrome is unique per page
// (it names that page's neighbours), so frequency cannot filter it. It is skipped structurally.
// This is the ONLY chrome blocklist here, and it can only ever DROP text: if it dropped too
// much it would risk hiding content loss, so it is kept minimal and never grown speculatively.
var navSkipTokens = []string{"breadcrumb", "nextprev", "tooltiptext", "switch-link"}

// auditExtractTokens returns the visible-text word tokens of an HTML page in document order
// (original casing preserved; fingerprinting lowercases later) plus a per-skip-class tally of
// how many tokens were dropped as chrome. Malformed input never panics: an unterminated comment
// or tag simply ends extraction at that point.
//
// Text is collected only inside the #content-wrap container (the coarse structural anchor the
// pages share), with its OWN element-nesting counter - deliberately not the production parser's
// contentDepth/skipDepth logic, which is exactly where the 2026-07-09 truncation bug lived. If
// no content-wrap is found the whole body is used as a fallback so the page is still audited.
func auditExtractTokens(page string) ([]string, map[string]int) {
	tokens, skipped := auditExtractScoped(page, true)
	if len(tokens) == 0 {
		// No content-wrap located: audit the whole document rather than silently skip it.
		tokens, skipped = auditExtractScoped(page, false)
	}
	return tokens, skipped
}

// skipEntry records one open skip subtree by the element-nesting depth of its root and the
// class/name that triggered it. Popping on a DEPTH match (not a name match) is what makes nested
// same-name elements - e.g. the <div>s inside a <div class="nextprev"> - close the skip at the
// right place. A name-match pop was the extractor's own false-positive-driving bug (a nextprev's
// first inner </div> ended the skip early and leaked its prev/next tips as page-local content).
type skipEntry struct {
	level int
	label string
}

func auditExtractScoped(page string, requireContentWrap bool) ([]string, map[string]int) {
	var buf strings.Builder
	skipped := map[string]int{}
	// collecting is true once inside the content anchor (or from the start in fallback mode).
	// cd is element-nesting depth below the anchor root (the anchor element itself is level 0).
	collecting := !requireContentWrap
	cd := 0
	var skipStack []skipEntry
	// text emits a run of character data: into buf when kept, into the skip tally when dropped.
	text := func(seg string) {
		if !collecting || seg == "" {
			return
		}
		seg = html.UnescapeString(seg)
		if len(skipStack) > 0 {
			// Attribute dropped volume to the OUTERMOST active skip class (it owns the removal).
			skipped[skipStack[0].label] += countWordTokens(seg)
			return
		}
		buf.WriteString(seg)
	}
	// sep inserts a token boundary at a block-level tag edge, but only in kept content.
	sep := func() {
		if collecting && len(skipStack) == 0 {
			buf.WriteByte(' ')
		}
	}

	i, n := 0, len(page)
	for i < n {
		lt := strings.IndexByte(page[i:], '<')
		if lt < 0 {
			text(page[i:])
			break
		}
		lt += i
		if lt > i {
			text(page[i:lt])
		}
		if strings.HasPrefix(page[lt:], "<!--") {
			end := strings.Index(page[lt+4:], "-->")
			if end < 0 {
				break // unterminated comment: stop
			}
			i = lt + 4 + end + 3
			continue
		}
		gt := auditFindTagEnd(page, lt+1)
		if gt < 0 {
			break // unterminated tag: stop
		}
		content := page[lt+1 : gt]
		i = gt + 1
		if content == "" || content[0] == '!' || content[0] == '?' {
			continue // empty, declaration, or processing instruction
		}
		closing := content[0] == '/'
		name := auditTagName(content, closing)
		if name == "" {
			continue
		}
		if auditBlockElements[name] {
			sep() // block-level open or close is a token boundary on both sides
		}
		if closing {
			if !collecting {
				continue
			}
			// A close tag for a void element never had a matching open subtree, so it must
			// not decrement the depth. Real pages have these: every ScriptReference page's
			// feedback form emits <input ...></input> pairs inside #content-wrap, and an
			// unbalanced decrement here ended collection early - silently blinding the audit
			// to everything after the form.
			if auditVoidElements[name] {
				continue
			}
			if len(skipStack) > 0 && skipStack[len(skipStack)-1].level == cd {
				skipStack = skipStack[:len(skipStack)-1]
			}
			cd--
			if cd < 0 {
				collecting = false // the content anchor element itself closed
			}
			continue
		}
		selfClosing := strings.HasSuffix(strings.TrimSpace(content), "/")
		if auditVoidElements[name] || selfClosing {
			continue // never opens a subtree; no depth change
		}
		if !collecting {
			// Enter content-wrap the first time we see it; its root sits at level 0.
			if requireContentWrap && strings.Contains(auditIdentity(content), "content-wrap") {
				collecting = true
				cd = 0
			}
			continue
		}
		cd++
		if label := auditSkipLabel(name, content); label != "" {
			skipStack = append(skipStack, skipEntry{level: cd, label: label})
		}
	}
	return auditTokenize(buf.String()), skipped
}

// auditIdentity returns the lowercased id + class + role + aria-label of a tag's content, the
// string the skip checks match against.
func auditIdentity(content string) string {
	var b strings.Builder
	for _, attr := range []string{"id", "class", "role", "aria-label"} {
		if v, ok := auditAttrValue(content, attr); ok {
			b.WriteString(strings.ToLower(v))
			b.WriteByte(' ')
		}
	}
	return b.String()
}

// auditSkipLabel returns the skip-class label for an opening tag - a page-local nav token, a
// non-visible subtree tag name, or "" when the element is not skipped. The label doubles as the
// key for per-class dropped-volume reporting.
func auditSkipLabel(name, content string) string {
	identity := auditIdentity(content)
	for _, token := range navSkipTokens {
		if strings.Contains(identity, token) {
			return token
		}
	}
	if auditSkipSubtrees[name] {
		return name
	}
	return ""
}

// auditAttrValue extracts one attribute's (entity-decoded) value from a tag's inner content.
// A small independent scanner - it does not share the production parser's parseAttrs.
func auditAttrValue(content, want string) (string, bool) {
	lc := strings.ToLower(content)
	from := 0
	for {
		idx := strings.Index(lc[from:], want)
		if idx < 0 {
			return "", false
		}
		p := from + idx
		// Require an attribute-name boundary before and '=' (optionally spaced) after.
		before := p == 0 || content[p-1] == ' ' || content[p-1] == '\t' || content[p-1] == '\n' || content[p-1] == '"' || content[p-1] == '\''
		q := p + len(want)
		for q < len(content) && (content[q] == ' ' || content[q] == '\t' || content[q] == '\n') {
			q++
		}
		if before && q < len(content) && content[q] == '=' {
			q++
			for q < len(content) && (content[q] == ' ' || content[q] == '\t' || content[q] == '\n') {
				q++
			}
			if q < len(content) && (content[q] == '"' || content[q] == '\'') {
				quote := content[q]
				q++
				start := q
				for q < len(content) && content[q] != quote {
					q++
				}
				return html.UnescapeString(content[start:q]), true
			}
			start := q
			for q < len(content) && content[q] != ' ' && content[q] != '\t' && content[q] != '\n' && content[q] != '/' {
				q++
			}
			return html.UnescapeString(content[start:q]), true
		}
		from = p + len(want)
	}
}

// auditFindTagEnd returns the index of the '>' closing the tag that starts at start,
// treating '>' inside a quoted attribute value as ordinary text.
func auditFindTagEnd(text string, start int) int {
	var quote byte
	for i := start; i < len(text); i++ {
		c := text[i]
		switch {
		case quote != 0:
			if c == quote {
				quote = 0
			}
		case c == '"' || c == '\'':
			quote = c
		case c == '>':
			return i
		}
	}
	return -1
}

// auditTagName reads the lowercased element name from a tag's inner content (the text
// between '<' and '>'), skipping a leading '/' for close tags.
func auditTagName(content string, closing bool) string {
	i := 0
	if closing {
		i = 1
	}
	for i < len(content) && unicode.IsSpace(rune(content[i])) {
		i++
	}
	start := i
	for i < len(content) {
		c := content[i]
		if unicode.IsLetter(rune(c)) || unicode.IsDigit(rune(c)) || c == '-' || c == ':' {
			i++
		} else {
			break
		}
	}
	return strings.ToLower(content[start:i])
}

// appendWordTokens splits seg into maximal runs of Unicode letters/digits and appends each
// as a token (original casing). Everything else (punctuation, whitespace, entities-decoded
// to non-word characters) is a token boundary - so both this path and the markdown path
// tokenize identically regardless of the source's spacing or markup.
func appendWordTokens(dst *[]string, seg string) {
	start := -1
	for i, r := range seg {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			if start < 0 {
				start = i
			}
		} else if start >= 0 {
			*dst = append(*dst, seg[start:i])
			start = -1
		}
	}
	if start >= 0 {
		*dst = append(*dst, seg[start:])
	}
}

// auditTokenize splits arbitrary text (e.g. a derived Markdown file) into word tokens with
// the same rule as the reference extractor, so shingles line up across the two sides.
func auditTokenize(text string) []string {
	var tokens []string
	appendWordTokens(&tokens, text)
	return tokens
}

// countWordTokens counts word tokens in seg without allocating a slice (for skipped-volume
// tallies). Same word rule as appendWordTokens.
func countWordTokens(seg string) int {
	count := 0
	inWord := false
	for _, r := range seg {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			if !inWord {
				count++
				inWord = true
			}
		} else {
			inWord = false
		}
	}
	return count
}

// shingleFingerprint hashes a window of tokens into a 64-bit fingerprint, lowercasing ASCII
// so casing differences between the two sides never matter. FNV-1a, computed inline to avoid
// per-shingle allocation across tens of millions of windows. A separator byte between tokens
// keeps ["ab","c"] distinct from ["a","bc"].
func shingleFingerprint(window []string) uint64 {
	const (
		offset uint64 = 14695981039346656037
		prime  uint64 = 1099511628211
		sep    byte   = 0x1f
	)
	h := offset
	for i, t := range window {
		if i > 0 {
			h ^= uint64(sep)
			h *= prime
		}
		for j := 0; j < len(t); j++ {
			c := t[j]
			if c >= 'A' && c <= 'Z' {
				c += 'a' - 'A'
			}
			h ^= uint64(c)
			h *= prime
		}
	}
	return h
}

// distinctShingles returns the set of distinct shingle fingerprints for a token slice. For a
// slice shorter than n it returns a single fingerprint over the whole slice, so short pages
// still contribute (and get checked). Empty input yields nil.
func distinctShingles(tokens []string, n int) map[uint64]struct{} {
	if len(tokens) == 0 {
		return nil
	}
	set := make(map[uint64]struct{})
	if len(tokens) < n {
		set[shingleFingerprint(tokens)] = struct{}{}
		return set
	}
	for i := 0; i+n <= len(tokens); i++ {
		set[shingleFingerprint(tokens[i:i+n])] = struct{}{}
	}
	return set
}

// containsSubsequence reports whether needle appears as a contiguous, case-insensitive token
// subsequence of hay. Used only for pages shorter than the shingle width, where the n-gram
// membership set does not apply.
func containsSubsequence(hay, needle []string) bool {
	if len(needle) == 0 {
		return true
	}
	if len(needle) > len(hay) {
		return false
	}
	for i := 0; i+len(needle) <= len(hay); i++ {
		match := true
		for j := range needle {
			if !strings.EqualFold(hay[i+j], needle[j]) {
				match = false
				break
			}
		}
		if match {
			return true
		}
	}
	return false
}
