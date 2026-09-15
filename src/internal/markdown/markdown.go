// Package markdown renders a small, safe subset of Markdown to HTML for the
// knowledge base. It is deliberately hand-written and dependency-free: no
// tables, footnotes, HTML pass-through or other extended syntax.
//
// Supported: ATX headings (#..######), fenced code blocks (``` or ~~~), inline
// code, **bold**, *italic*, [links](url) (http/https/mailto/#// only), ordered
// and unordered lists, blockquotes, horizontal rules, paragraphs with single
// newlines kept as <br>.
//
// Everything is HTML-escaped first, so source content can never inject markup.
package markdown

import (
	"html"
	"html/template"
	"regexp"
	"strconv"
	"strings"
)

// Render converts src to HTML. The result is safe to insert unescaped.
func Render(src string) template.HTML { return template.HTML(render(src)) }

var (
	reHeading   = regexp.MustCompile(`^(#{1,6})\s+(.*)$`)
	reHR        = regexp.MustCompile(`^\s{0,3}(?:-{3,}|\*{3,}|_{3,})\s*$`)
	reUL        = regexp.MustCompile(`^\s{0,3}[-*+]\s+(.*)$`)
	reOL        = regexp.MustCompile(`^\s{0,3}\d+\.\s+(.*)$`)
	reCode      = regexp.MustCompile("`([^`]+)`")
	reLink      = regexp.MustCompile(`\[([^\]]*)\]\(([^)\s]*)\)`)
	reBold      = regexp.MustCompile(`\*\*([^*]+)\*\*`)
	reItalic    = regexp.MustCompile(`\*([^*]+)\*`)
	reCodePlace = regexp.MustCompile("\x00(\\d+)\x00")
	reLang      = regexp.MustCompile(`[^A-Za-z0-9_+.-]`)
)

func render(src string) string {
	lines := strings.Split(strings.ReplaceAll(src, "\r\n", "\n"), "\n")
	var b strings.Builder
	for i := 0; i < len(lines); {
		trimmed := strings.TrimSpace(lines[i])

		// Fenced code block: ``` or ~~~ (info string ignored but kept as a
		// class for styling).
		if strings.HasPrefix(trimmed, "```") || strings.HasPrefix(trimmed, "~~~") {
			fence := trimmed[:3]
			info := strings.TrimSpace(strings.TrimPrefix(trimmed, fence))
			i++
			var code []string
			for i < len(lines) && !strings.HasPrefix(strings.TrimSpace(lines[i]), fence) {
				code = append(code, lines[i])
				i++
			}
			if i < len(lines) {
				i++ // closing fence
			}
			b.WriteString(codeBlock(info, strings.Join(code, "\n")))
			continue
		}
		if trimmed == "" {
			i++
			continue
		}
		if reHR.MatchString(lines[i]) {
			b.WriteString("<hr>\n")
			i++
			continue
		}
		if m := reHeading.FindStringSubmatch(lines[i]); m != nil {
			n := len(m[1])
			b.WriteString("<h" + strconv.Itoa(n) + ">" + inline(m[2]) + "</h" + strconv.Itoa(n) + ">\n")
			i++
			continue
		}
		if strings.HasPrefix(trimmed, ">") {
			var quote []string
			for i < len(lines) && strings.HasPrefix(strings.TrimSpace(lines[i]), ">") {
				quote = append(quote, strings.TrimPrefix(strings.TrimPrefix(strings.TrimSpace(lines[i]), ">"), " "))
				i++
			}
			b.WriteString("<blockquote>" + inline(strings.Join(quote, "\n")) + "</blockquote>\n")
			continue
		}
		if reUL.MatchString(lines[i]) {
			b.WriteString("<ul>\n")
			for i < len(lines) {
				m := reUL.FindStringSubmatch(lines[i])
				if m == nil {
					break
				}
				b.WriteString("<li>" + inline(m[1]) + "</li>\n")
				i++
			}
			b.WriteString("</ul>\n")
			continue
		}
		if reOL.MatchString(lines[i]) {
			b.WriteString("<ol>\n")
			for i < len(lines) {
				m := reOL.FindStringSubmatch(lines[i])
				if m == nil {
					break
				}
				b.WriteString("<li>" + inline(m[1]) + "</li>\n")
				i++
			}
			b.WriteString("</ol>\n")
			continue
		}
		// Paragraph: consecutive plain lines, up to the next block start.
		var para []string
		for i < len(lines) {
			t := strings.TrimSpace(lines[i])
			if t == "" || startsBlock(lines[i]) {
				break
			}
			para = append(para, lines[i])
			i++
		}
		b.WriteString("<p>" + inline(strings.Join(para, "\n")) + "</p>\n")
	}
	return b.String()
}

func startsBlock(line string) bool {
	t := strings.TrimSpace(line)
	return strings.HasPrefix(t, "```") || strings.HasPrefix(t, "~~~") ||
		strings.HasPrefix(t, ">") ||
		reHR.MatchString(line) || reHeading.MatchString(line) ||
		reUL.MatchString(line) || reOL.MatchString(line)
}

func codeBlock(info, code string) string {
	cls := ""
	if info != "" {
		lang := reLang.ReplaceAllString(info, "")
		if lang != "" {
			cls = ` class="language-` + html.EscapeString(lang) + `"`
		}
	}
	return "<pre><code" + cls + ">" + html.EscapeString(code) + "</code></pre>\n"
}

// inline applies span-level formatting to one already-block-scoped string.
func inline(s string) string {
	s = html.EscapeString(s)

	// Pull code spans out first so their contents are not re-processed.
	var codes []string
	s = reCode.ReplaceAllStringFunc(s, func(m string) string {
		codes = append(codes, m[1:len(m)-1])
		return "\x00" + strconv.Itoa(len(codes)-1) + "\x00"
	})

	s = reLink.ReplaceAllStringFunc(s, func(m string) string {
		sub := reLink.FindStringSubmatch(m)
		href := sanitizeURL(sub[2])
		if href == "" {
			return sub[1]
		}
		return `<a href="` + href + `" target="_blank" rel="noopener noreferrer">` + sub[1] + `</a>`
	})

	s = reBold.ReplaceAllString(s, "<strong>$1</strong>")
	s = reItalic.ReplaceAllString(s, "<em>$1</em>")
	s = strings.ReplaceAll(s, "\n", "<br>\n")

	s = reCodePlace.ReplaceAllStringFunc(s, func(m string) string {
		n, _ := strconv.Atoi(m[1 : len(m)-1])
		if n < 0 || n >= len(codes) {
			return ""
		}
		return "<code>" + codes[n] + "</code>"
	})
	return s
}

// sanitizeURL returns an escaped href for http/https/mailto/#/ links, or ""
// for anything else (so "javascript:" cannot sneak through). The caller falls
// back to plain text when it gets "".
func sanitizeURL(raw string) string {
	u := strings.TrimSpace(html.UnescapeString(raw))
	if u == "" {
		return ""
	}
	low := strings.ToLower(u)
	for _, ok := range []string{"http://", "https://", "mailto:", "#", "/"} {
		if strings.HasPrefix(low, ok) {
			return html.EscapeString(u)
		}
	}
	return ""
}
