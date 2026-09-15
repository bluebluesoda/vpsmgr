package markdown

import (
	"strings"
	"testing"
)

func TestRenderBlocks(t *testing.T) {
	cases := []struct{ name, in, want string }{
		{"heading", "# Hi", "<h1>Hi</h1>"},
		{"h3", "### a b", "<h3>a b</h3>"},
		{"hr", "---", "<hr>"},
		{"ul", "- a\n- b", "<ul>\n<li>a</li>\n<li>b</li>\n</ul>"},
		{"ol", "1. a\n2. b", "<ol>\n<li>a</li>\n<li>b</li>\n</ol>"},
		{"quote", "> hello", "<blockquote>hello</blockquote>"},
		{"paragraph", "a\nb", "<p>a<br>\nb</p>"},
		{"fence", "```\nx < y\n```", "<pre><code>x &lt; y</code></pre>"},
		{"fence-lang", "```go\nfmt.Println()\n```", `<pre><code class="language-go">fmt.Println()</code></pre>`},
	}
	for _, tc := range cases {
		got := string(Render(tc.in))
		if !strings.Contains(got, tc.want) {
			t.Errorf("%s: Render(%q) = %q, want it to contain %q", tc.name, tc.in, got, tc.want)
		}
	}
}

func TestRenderInline(t *testing.T) {
	cases := []struct{ in, want string }{
		{"**bold**", "<strong>bold</strong>"},
		{"*italic*", "<em>italic</em>"},
		{"`code`", "<code>code</code>"},
		{"[x](https://e.com)", `<a href="https://e.com" target="_blank" rel="noopener noreferrer">x</a>`},
		{"[rel](/docs)", `<a href="/docs"`},
		{"[mail](mailto:a@b.c)", `<a href="mailto:a@b.c"`},
		// A code span must not have its contents formatted.
		{"`**not bold**`", "<code>**not bold**</code>"},
	}
	for _, tc := range cases {
		if got := string(Render(tc.in)); !strings.Contains(got, tc.want) {
			t.Errorf("Render(%q) = %q, want it to contain %q", tc.in, got, tc.want)
		}
	}
}

func TestRenderEscapesHTML(t *testing.T) {
	got := string(Render("<script>alert(1)</script>"))
	if strings.Contains(got, "<script>") {
		t.Fatalf("raw HTML leaked: %q", got)
	}
	if !strings.Contains(got, "&lt;script&gt;") {
		t.Fatalf("HTML not escaped: %q", got)
	}
	// Inside a fence too.
	if got := string(Render("```\n<b>x</b>\n```")); strings.Contains(got, "<b>") {
		t.Fatalf("raw HTML leaked from a code fence: %q", got)
	}
}

func TestRenderRejectsUnsafeLinks(t *testing.T) {
	for _, bad := range []string{"javascript:alert(1)", "data:text/html,x", "vbscript:x"} {
		got := string(Render("[click](" + bad + ")"))
		if strings.Contains(got, "<a ") {
			t.Errorf("unsafe URL %q produced a link: %q", bad, got)
		}
		if !strings.Contains(got, "click") {
			t.Errorf("unsafe URL %q dropped the link text: %q", bad, got)
		}
	}
}
