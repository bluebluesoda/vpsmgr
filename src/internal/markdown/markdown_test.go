package markdown

import (
	"regexp"
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

var reNode = regexp.MustCompile(`data-node="([^"]+)"`)

func TestRenderFillBlanks(t *testing.T) {
	got := string(Render("```yaml\nREGINNAME=*#*#填写地域信息#*#*\n```"))
	if !strings.Contains(got, `<input type="text" class="kb-blank" data-node="`) {
		t.Fatalf("directive did not render a fill input: %q", got)
	}
	if !strings.Contains(got, `placeholder="填写地域信息"`) {
		t.Fatalf("label was not used as the placeholder: %q", got)
	}
	if !strings.Contains(got, "REGINNAME=") {
		t.Fatalf("literal code around the directive was dropped: %q", got)
	}
	if strings.Contains(got, "*#*#") {
		t.Fatalf("directive leaked into the output: %q", got)
	}
	// The answer lives in the browser only: never emit a value attribute.
	if strings.Contains(got, "value=") {
		t.Fatalf("a value attribute was emitted: %q", got)
	}
}

func TestRenderFillBlanksEscapesLabel(t *testing.T) {
	got := string(Render("```\n*#*#a\"b<c#*#*\n```"))
	if !strings.Contains(got, `placeholder="a&#34;b&lt;c"`) {
		t.Fatalf("label not escaped into the attribute: %q", got)
	}
	if strings.Contains(got, "b<c") || strings.Contains(got, `placeholder="a"`) {
		t.Fatalf("label injected raw markup into the attribute: %q", got)
	}
}

func TestRenderFillBlanksNodeIDs(t *testing.T) {
	const src = "```\na=*#*#host#*#*\nb=*#*#host#*#*\n```"
	got := string(Render(src))
	ids := reNode.FindAllStringSubmatch(got, -1)
	if len(ids) != 2 {
		t.Fatalf("want 2 fills, got %d: %q", len(ids), got)
	}
	if ids[0][1] == ids[1][1] {
		t.Fatalf("a repeated label reused node id %q", ids[0][1])
	}
	// A label repeated across blocks must not collide either.
	got2 := string(Render("```\nx=*#*#host#*#*\n```\n\ntext\n\n```\ny=*#*#host#*#*\n```"))
	ids2 := reNode.FindAllStringSubmatch(got2, -1)
	if len(ids2) != 2 || ids2[0][1] == ids2[1][1] {
		t.Fatalf("cross-block node ids collided: %q", got2)
	}
	// Ids derive from the label, so the same source renders identically (which
	// is what keeps a reader's saved answer attached across reloads).
	if again := string(Render(src)); again != got {
		t.Fatalf("render is not deterministic:\n%q\n%q", got, again)
	}
}

func TestRenderFillBlanksOnlyInFences(t *testing.T) {
	for _, src := range []string{
		"text *#*#label#*#* text",
		"# heading *#*#label#*#*",
		"- item *#*#label#*#*",
		"> quote *#*#label#*#*",
	} {
		if got := string(Render(src)); strings.Contains(got, "kb-blank") {
			t.Errorf("directive rendered outside a fence for %q: %q", src, got)
		}
	}
}

func TestRenderFillBlanksMalformed(t *testing.T) {
	cases := []string{
		"```\n*#*##*#*\n```",     // empty label
		"```\n*#*#   #*#*\n```",  // whitespace-only label
		"```\n*#*#unclosed\n```", // no closing delimiter
		"```\nno directives\n```",
	}
	for _, src := range cases {
		if got := string(Render(src)); strings.Contains(got, "kb-blank") {
			t.Errorf("malformed directive produced a fill for %q: %q", src, got)
		}
	}
}
