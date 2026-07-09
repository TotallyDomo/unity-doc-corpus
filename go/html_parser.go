package main

import (
	"html"
	"strings"
	"unicode"
)

var skipTags = map[string]bool{"script": true, "style": true, "template": true, "noscript": true, "svg": true}
var voidTags = map[string]bool{"area": true, "base": true, "br": true, "col": true, "embed": true, "hr": true, "img": true, "input": true, "link": true, "meta": true, "param": true, "source": true, "track": true, "wbr": true}
var skipClassOrID = []string{"header", "header-wrapper", "sidebar", "sidebar-wrap", "sidebar-menu", "search-form", "apisearch", "toolbar", "footer", "footer-wrapper", "suggest", "suggest-wrap", "lang-switcher", "otherversionscontent", "version-number", "scrolltofeedback", "feedback", "mobilelogo", "navigation", "switch-link", "tooltiptext", "breadcrumbs", "nextprev"}

func findTagEnd(text string, start int) int {
	quote := byte(0)
	for i := start; i < len(text); i++ {
		ch := text[i]
		if quote != 0 {
			if ch == quote {
				quote = 0
			}
		} else if ch == '"' || ch == '\'' {
			quote = ch
		} else if ch == '>' {
			return i
		}
	}
	return -1
}

func tagName(content string, end bool) string {
	i := 0
	if end {
		i = 1
	}
	for i < len(content) && unicode.IsSpace(rune(content[i])) {
		i++
	}
	start := i
	for i < len(content) {
		r := rune(content[i])
		if unicode.IsLetter(r) || unicode.IsDigit(r) || content[i] == '-' || content[i] == ':' {
			i++
		} else {
			break
		}
	}
	return strings.ToLower(content[start:i])
}

func parseAttrs(content string) map[string]string {
	attrs := map[string]string{}
	i := 0
	for i < len(content) && !unicode.IsSpace(rune(content[i])) {
		i++
	}
	for i < len(content) {
		for i < len(content) && unicode.IsSpace(rune(content[i])) {
			i++
		}
		if i >= len(content) || content[i] == '/' {
			break
		}
		nameStart := i
		for i < len(content) && !unicode.IsSpace(rune(content[i])) && content[i] != '=' && content[i] != '/' {
			i++
		}
		name := strings.ToLower(content[nameStart:i])
		for i < len(content) && unicode.IsSpace(rune(content[i])) {
			i++
		}
		value := ""
		if i < len(content) && content[i] == '=' {
			i++
			for i < len(content) && unicode.IsSpace(rune(content[i])) {
				i++
			}
			if i < len(content) && (content[i] == '"' || content[i] == '\'') {
				quote := content[i]
				i++
				valueStart := i
				for i < len(content) && content[i] != quote {
					i++
				}
				value = content[valueStart:i]
				if i < len(content) {
					i++
				}
			} else {
				valueStart := i
				for i < len(content) && !unicode.IsSpace(rune(content[i])) && content[i] != '/' {
					i++
				}
				value = content[valueStart:i]
			}
		}
		if name != "" {
			attrs[name] = html.UnescapeString(value)
		}
	}
	return attrs
}

type unityParser struct {
	page              parsedPage
	inTitle           bool
	titleParts        []string
	contentDepth      int
	skipDepth         int
	parts             []string
	hasCurrentHeading bool
	currentHeading    []string
	hasAnchor         bool
	anchorHref        string
	anchorParts       []string
}

func (p *unityParser) collecting() bool { return p.contentDepth > 0 }

func (p *unityParser) start(tag string, attrs map[string]string) {
	if tag == "title" {
		p.inTitle = true
	}
	if tag == "link" && strings.ToLower(attrs["rel"]) == "canonical" {
		p.page.Canonical = attrs["href"]
	}
	// Match chrome tokens against structural attributes only. aria-label is human-readable prose
	// (e.g. "Go to Android.AndroidNavigation.Undefined.html") that substring-matched skip tokens
	// like "navigation"/"header" and silently dropped enum/property member-name links; the audit
	// caught it (M0042-S0001). All chrome containers are identified by class/id/role.
	identity := strings.ToLower(attrs["id"] + " " + attrs["class"] + " " + attrs["role"])
	identitySkip := false
	for _, token := range skipClassOrID {
		if strings.Contains(identity, token) {
			identitySkip = true
			break
		}
	}
	if p.skipDepth > 0 || skipTags[tag] || identitySkip {
		if !voidTags[tag] {
			p.skipDepth++
		}
		return
	}
	if !voidTags[tag] {
		if p.contentDepth > 0 {
			p.contentDepth++
		} else if strings.ToLower(attrs["id"]) == "content-wrap" || strings.Contains(strings.ToLower(attrs["class"]), "content-wrap") {
			p.contentDepth++
		}
	}
	if p.collecting() && (tag == "p" || tag == "div" || tag == "section" || tag == "table" || tag == "tr" || tag == "td" || tag == "th" || tag == "ul" || tag == "ol" || tag == "li" || tag == "pre" || tag == "blockquote") {
		p.parts = append(p.parts, "\n")
	}
	if p.collecting() && (tag == "h1" || tag == "h2" || tag == "h3" || tag == "h4") {
		p.parts = append(p.parts, "\n\n")
		p.currentHeading = nil
		p.hasCurrentHeading = true
	}
	if p.collecting() && tag == "br" {
		p.parts = append(p.parts, "\n")
	}
	if p.collecting() && tag == "a" {
		p.anchorHref = attrs["href"]
		p.anchorParts = nil
		p.hasAnchor = true
	}
}

func (p *unityParser) end(tag string) {
	// Void tags never opened a depth level in start(), so a stray </br> or the
	// synthetic end() for self-closing <br/> must not unbalance skip/content depth.
	if voidTags[tag] {
		return
	}
	if tag == "title" {
		p.inTitle = false
		p.page.Title = compactText(strings.Join(p.titleParts, ""))
	}
	if p.skipDepth > 0 {
		p.skipDepth--
		return
	}
	if p.collecting() && (tag == "h1" || tag == "h2" || tag == "h3" || tag == "h4") && p.hasCurrentHeading {
		heading := compactText(strings.Join(p.currentHeading, ""))
		if heading != "" {
			p.page.Headings = append(p.page.Headings, heading)
			p.parts = append(p.parts, heading, "\n")
		}
		p.hasCurrentHeading = false
		p.currentHeading = nil
	}
	if p.collecting() && tag == "a" && p.hasAnchor && p.anchorHref != "" {
		text := compactText(strings.Join(p.anchorParts, ""))
		if text != "" {
			p.page.Links = append(p.page.Links, link{text, p.anchorHref})
		}
		p.hasAnchor = false
		p.anchorHref = ""
		p.anchorParts = nil
	}
	if p.collecting() && (tag == "p" || tag == "div" || tag == "section" || tag == "table" || tag == "tr" || tag == "td" || tag == "th" || tag == "ul" || tag == "ol" || tag == "li" || tag == "pre" || tag == "blockquote") {
		p.parts = append(p.parts, "\n")
	}
	if p.contentDepth > 0 {
		p.contentDepth--
	}
}

func (p *unityParser) data(value string) {
	if p.inTitle {
		p.titleParts = append(p.titleParts, value)
		return
	}
	if p.skipDepth > 0 {
		return
	}
	if !p.collecting() {
		return
	}
	if p.hasCurrentHeading {
		p.currentHeading = append(p.currentHeading, value)
	} else {
		p.parts = append(p.parts, value)
	}
	if p.hasAnchor {
		p.anchorParts = append(p.anchorParts, value)
	}
}

func (p *unityParser) finish() parsedPage {
	p.page.Body = compactText(strings.Join(p.parts, ""))
	manual := "Unity - Manual:"
	scripting := "Unity - Scripting API:"
	if strings.HasPrefix(p.page.Title, manual) {
		p.page.Title = compactText(p.page.Title[len(manual):])
	}
	if strings.HasPrefix(p.page.Title, scripting) {
		p.page.Title = compactText(p.page.Title[len(scripting):])
	}
	return p.page
}

func findCaseInsensitive(haystack, needle string, start int) int {
	idx := strings.Index(strings.ToLower(haystack[start:]), strings.ToLower(needle))
	if idx < 0 {
		return -1
	}
	return start + idx
}

func parseHTML(text string) parsedPage {
	parser := &unityParser{}
	i := 0
	for i < len(text) {
		lt := strings.IndexByte(text[i:], '<')
		if lt < 0 {
			parser.data(text[i:])
			break
		}
		lt += i
		if lt > i {
			parser.data(text[i:lt])
		}
		if strings.HasPrefix(text[lt:], "<!--") {
			end := strings.Index(text[lt+4:], "-->")
			if end < 0 {
				break
			}
			i = lt + 4 + end + 3
			continue
		}
		gt := findTagEnd(text, lt+1)
		if gt < 0 {
			break
		}
		content := text[lt+1 : gt]
		endTag := strings.HasPrefix(content, "/")
		declaration := strings.HasPrefix(content, "!") || strings.HasPrefix(content, "?")
		if !declaration {
			tag := tagName(content, endTag)
			if tag != "" {
				if endTag {
					parser.end(tag)
				} else {
					selfClosing := strings.HasSuffix(strings.TrimSpace(content), "/")
					attrs := parseAttrs(content)
					parser.start(tag, attrs)
					if skipTags[tag] && !voidTags[tag] {
						closePos := findCaseInsensitive(text, "</"+tag, gt+1)
						if closePos >= 0 {
							closeGt := findTagEnd(text, closePos+2)
							parser.end(tag)
							if closeGt < 0 {
								break
							}
							i = closeGt + 1
							continue
						}
					}
					if selfClosing {
						parser.end(tag)
					}
				}
			}
		}
		i = gt + 1
	}
	return parser.finish()
}
