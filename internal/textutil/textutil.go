// Package textutil converts and sanitises message bodies.
package textutil

import (
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/microcosm-cc/bluemonday"
	"golang.org/x/net/html"
)

var hiddenStyle = regexp.MustCompile(`(?i)(display\s*:\s*none|visibility\s*:\s*hidden|font-size\s*:\s*0(px|pt|em|%)?\s*(;|$)|max-height\s*:\s*0|opacity\s*:\s*0(\.0+)?\s*(;|$)|mso-hide\s*:\s*all)`)

// HTMLToText renders HTML as readable plain text. Content a human cannot see
// (display:none, zero font, hidden attribute, scripts, styles) is dropped, which also
// removes the most common hiding places for prompt-injection payloads.
func HTMLToText(src string) string {
	doc, err := html.Parse(strings.NewReader(src))
	if err != nil {
		return StripTags(src)
	}
	b := &strings.Builder{}
	var walk func(n *html.Node)
	// paragraph-level blocks get a blank line around them; line-level blocks just start a new line
	paraTags := map[string]bool{"p": true, "h1": true, "h2": true, "h3": true, "h4": true, "h5": true, "h6": true, "table": true, "ul": true, "ol": true, "pre": true}
	lineTags := map[string]bool{"div": true, "tr": true, "li": true, "hr": true}
	endLine := func() {
		if b.Len() > 0 && !strings.HasSuffix(b.String(), "\n") {
			b.WriteString("\n")
		}
	}
	endPara := func() {
		endLine()
		if b.Len() > 0 && !strings.HasSuffix(b.String(), "\n\n") {
			b.WriteString("\n")
		}
	}
	walk = func(n *html.Node) {
		if n.Type == html.ElementNode {
			switch n.Data {
			case "script", "style", "head", "title", "noscript", "template":
				return
			}
			for _, a := range n.Attr {
				if a.Key == "hidden" || (a.Key == "aria-hidden" && a.Val == "true" && n.Data != "body") || (a.Key == "style" && hiddenStyle.MatchString(a.Val)) {
					return
				}
			}
			if n.Data == "blockquote" {
				outer := b
				b = &strings.Builder{}
				for c := n.FirstChild; c != nil; c = c.NextSibling {
					walk(c)
				}
				inner := Tidy(b.String())
				b = outer
				endLine()
				for _, l := range strings.Split(inner, "\n") {
					b.WriteString("> " + l + "\n")
				}
				return
			}
			switch {
			case n.Data == "br":
				b.WriteString("\n")
			case paraTags[n.Data]:
				endPara()
			case lineTags[n.Data]:
				endLine()
			}
			if n.Data == "li" {
				b.WriteString("• ")
			}
			if n.Data == "a" {
				var href string
				for _, a := range n.Attr {
					if a.Key == "href" {
						href = a.Val
					}
				}
				start := b.Len()
				for c := n.FirstChild; c != nil; c = c.NextSibling {
					walk(c)
				}
				txt := strings.TrimSpace(b.String()[start:])
				if href != "" && strings.HasPrefix(href, "http") && txt != href && txt != "" {
					b.WriteString(" (" + href + ")")
				}
				return
			}
			if n.Data == "img" {
				for _, a := range n.Attr {
					if a.Key == "alt" && strings.TrimSpace(a.Val) != "" {
						b.WriteString("[" + strings.TrimSpace(a.Val) + "]")
					}
				}
			}
		}
		if n.Type == html.TextNode {
			b.WriteString(n.Data)
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
		if n.Type == html.ElementNode {
			switch {
			case paraTags[n.Data]:
				endPara()
			case lineTags[n.Data]:
				endLine()
			}
		}
	}
	walk(doc)
	return Tidy(b.String())
}

var tagRe = regexp.MustCompile(`<[^>]*>`)

func StripTags(s string) string { return Tidy(tagRe.ReplaceAllString(s, " ")) }

var (
	spaceRun = regexp.MustCompile(`[ \t\x{00a0}]+`)
	nlRun    = regexp.MustCompile(`\n{3,}`)
)

// Tidy normalises whitespace and removes zero-width and bidi control characters.
func Tidy(s string) string {
	s = StripInvisible(s)
	s = strings.ReplaceAll(s, "\r\n", "\n")
	s = spaceRun.ReplaceAllString(s, " ")
	lines := strings.Split(s, "\n")
	for i, l := range lines {
		lines[i] = strings.TrimRight(l, " ")
	}
	s = strings.Join(lines, "\n")
	s = nlRun.ReplaceAllString(s, "\n\n")
	return strings.TrimSpace(s)
}

// StripInvisible removes zero-width, bidi override, tag-block and other format characters.
func StripInvisible(s string) string {
	return strings.Map(func(r rune) rune {
		switch {
		case r == '\n' || r == '\t' || r == '\r':
			return r
		case r >= 0xE0000 && r <= 0xE007F: // Unicode tag block (used to smuggle hidden text)
			return -1
		case unicode.Is(unicode.Cf, r) && r != '‍': // format chars incl. zero-width and bidi
			return -1
		case unicode.IsControl(r):
			return -1
		}
		return r
	}, s)
}

// Snippet returns a one-line preview of at most n runes.
func Snippet(text string, n int) string {
	var b strings.Builder
	last := ' '
	count := 0
	for _, r := range text {
		if r == '>' && (last == '\n' || last == ' ') && count == 0 {
			continue
		}
		if unicode.IsSpace(r) {
			if last != ' ' {
				b.WriteRune(' ')
				last = ' '
				count++
			}
		} else {
			b.WriteRune(r)
			last = r
			count++
		}
		if count >= n {
			break
		}
	}
	return strings.TrimSpace(b.String())
}

func Truncate(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	r := []rune(s)
	return string(r[:n]) + "…"
}

// QuoteReply formats text as an attribution plus quoted lines.
func QuoteReply(who, when, text string) string {
	var b strings.Builder
	b.WriteString("On " + when + ", " + who + " wrote:\n")
	for _, l := range strings.Split(strings.TrimSpace(text), "\n") {
		b.WriteString("> " + l + "\n")
	}
	return b.String()
}

var sanitizer = func() *bluemonday.Policy {
	p := bluemonday.UGCPolicy()
	p.AllowAttrs("style").OnElements("p", "div", "span", "td", "th", "table", "tr", "a", "font", "h1", "h2", "h3", "h4", "b", "i", "strong", "em", "ul", "ol", "li", "blockquote", "img")
	p.AllowAttrs("align", "valign", "width", "height", "bgcolor", "border", "cellpadding", "cellspacing", "colspan", "rowspan", "color", "face", "size").Globally()
	p.AllowElements("center", "font", "table", "thead", "tbody", "tfoot", "tr", "td", "th")
	p.AllowDataURIImages()
	p.AllowURLSchemes("http", "https", "mailto", "tel", "cid", "data")
	p.RequireNoFollowOnLinks(false)
	p.AddTargetBlankToFullyQualifiedLinks(true)
	return p
}()

var (
	imgSrcRe = regexp.MustCompile(`(?i)(<img\b[^>]*?\s)src=("|')(https?:)?//`)
	cssURLRe = regexp.MustCompile(`(?i)url\(\s*['"]?\s*(https?:)?//[^)]*\)`)
)

// SanitizeHTML makes message HTML safe to display. With blockRemote, every remote image
// and CSS url() is neutralised so opening a message never tells the sender anything.
// Reports whether remote content was found (and blocked).
func SanitizeHTML(src string, blockRemote bool) (out string, hadRemote bool) {
	out = sanitizer.Sanitize(src)
	if imgSrcRe.MatchString(out) || cssURLRe.MatchString(out) {
		hadRemote = true
	}
	if blockRemote {
		out = imgSrcRe.ReplaceAllString(out, `${1}data-blocked-src=${2}${3}//`)
		out = cssURLRe.ReplaceAllString(out, "none")
	}
	return out, hadRemote
}
