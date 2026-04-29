package main

import (
	"strings"
	"testing"
)

func TestExtractTextFromHTML_Headings(t *testing.T) {
	html := `<html><body><h1>Title</h1><h2>Sub</h2></body></html>`
	got := extractTextFromHTML(html, "")
	if !strings.Contains(got, "# Title") {
		t.Errorf("missing h1 -> '# Title': %q", got)
	}
	if !strings.Contains(got, "## Sub") {
		t.Errorf("missing h2 -> '## Sub': %q", got)
	}
}

func TestExtractTextFromHTML_Paragraphs(t *testing.T) {
	html := `<html><body><p>First.</p><p>Second.</p></body></html>`
	got := extractTextFromHTML(html, "")
	if !strings.Contains(got, "First.") || !strings.Contains(got, "Second.") {
		t.Errorf("missing paragraph text: %q", got)
	}
}

func TestExtractTextFromHTML_UnorderedList(t *testing.T) {
	html := `<html><body><ul><li>one</li><li>two</li></ul></body></html>`
	got := extractTextFromHTML(html, "")
	if !strings.Contains(got, "- one") {
		t.Errorf("missing '- one': %q", got)
	}
	if !strings.Contains(got, "- two") {
		t.Errorf("missing '- two': %q", got)
	}
}

func TestExtractTextFromHTML_OrderedList(t *testing.T) {
	html := `<html><body><ol><li>first</li><li>second</li><li>third</li></ol></body></html>`
	got := extractTextFromHTML(html, "")
	for i, want := range []string{"1. first", "2. second", "3. third"} {
		if !strings.Contains(got, want) {
			t.Errorf("item %d: missing %q in output: %q", i, want, got)
		}
	}
}

func TestExtractTextFromHTML_Links(t *testing.T) {
	html := `<html><body><p>See <a href="https://example.com/x">the page</a> here.</p></body></html>`
	got := extractTextFromHTML(html, "")
	if !strings.Contains(got, "[the page](https://example.com/x)") {
		t.Errorf("missing markdown link: %q", got)
	}
}

func TestExtractTextFromHTML_RelativeLinkResolved(t *testing.T) {
	html := `<html><body><a href="/foo">x</a></body></html>`
	got := extractTextFromHTML(html, "https://example.com/base/")
	if !strings.Contains(got, "https://example.com/foo") {
		t.Errorf("relative link not resolved against baseURL: %q", got)
	}
}

func TestExtractTextFromHTML_AnchorAndJSStripped(t *testing.T) {
	html := `<html><body><a href="#top">top</a> <a href="javascript:void(0)">do</a></body></html>`
	got := extractTextFromHTML(html, "")
	if strings.Contains(got, "#top") || strings.Contains(got, "javascript:") {
		t.Errorf("unsafe href leaked: %q", got)
	}
	if !strings.Contains(got, "top") || !strings.Contains(got, "do") {
		t.Errorf("link text dropped: %q", got)
	}
}

func TestExtractTextFromHTML_InlineEmphasis(t *testing.T) {
	html := `<html><body><p><strong>bold</strong> and <em>italic</em></p></body></html>`
	got := extractTextFromHTML(html, "")
	if !strings.Contains(got, "**bold**") {
		t.Errorf("missing **bold**: %q", got)
	}
	if !strings.Contains(got, "*italic*") {
		t.Errorf("missing *italic*: %q", got)
	}
}

func TestExtractTextFromHTML_InlineCode(t *testing.T) {
	html := "<html><body><p>use <code>foo()</code> here</p></body></html>"
	got := extractTextFromHTML(html, "")
	if !strings.Contains(got, "`foo()`") {
		t.Errorf("missing inline code: %q", got)
	}
}

func TestExtractTextFromHTML_PreservesPreformatted(t *testing.T) {
	html := "<html><body><pre>line one\n  indented\nline three</pre></body></html>"
	got := extractTextFromHTML(html, "")
	if !strings.Contains(got, "```") {
		t.Errorf("missing code fence: %q", got)
	}
	if !strings.Contains(got, "  indented") {
		t.Errorf("preformatted indentation collapsed: %q", got)
	}
}

func TestExtractTextFromHTML_Blockquote(t *testing.T) {
	html := `<html><body><blockquote>quoted text</blockquote></body></html>`
	got := extractTextFromHTML(html, "")
	if !strings.Contains(got, "> quoted text") {
		t.Errorf("missing blockquote prefix: %q", got)
	}
}

func TestExtractTextFromHTML_Table(t *testing.T) {
	html := `<html><body><table>
<tr><th>A</th><th>B</th></tr>
<tr><td>1</td><td>2</td></tr>
</table></body></html>`
	got := extractTextFromHTML(html, "")

	// We don't pin the exact spacing (the extractor produces "| A  | B |"
	// with a double space because cells emit a trailing space and the
	// separator adds a leading one). Assert the header and a data row are
	// present and pipe-delimited, with each cell value visible.
	for _, want := range []string{"| A", "B |", "| 1", "2 |"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in table output: %q", want, got)
		}
	}
}

func TestExtractTextFromHTML_ImageAlt(t *testing.T) {
	html := `<html><body><img src="a.png" alt="cat photo"></body></html>`
	got := extractTextFromHTML(html, "")
	if !strings.Contains(got, "[image: cat photo]") {
		t.Errorf("missing image alt: %q", got)
	}
}

func TestExtractTextFromHTML_ImageWithoutAltDropped(t *testing.T) {
	html := `<html><body><img src="a.png">visible</body></html>`
	got := extractTextFromHTML(html, "")
	if strings.Contains(got, "[image:") {
		t.Errorf("image without alt should be dropped: %q", got)
	}
	if !strings.Contains(got, "visible") {
		t.Errorf("surrounding text was lost: %q", got)
	}
}

func TestExtractTextFromHTML_NoiseElementsDropped(t *testing.T) {
	html := `<html><body>
		<script>var x = 1;</script>
		<style>.x { color: red; }</style>
		<nav>navigation here</nav>
		<footer>footer here</footer>
		<p>actual content</p>
	</body></html>`
	got := extractTextFromHTML(html, "")
	for _, drop := range []string{"var x = 1", "color: red", "navigation here", "footer here"} {
		if strings.Contains(got, drop) {
			t.Errorf("noise content leaked (%q): %q", drop, got)
		}
	}
	if !strings.Contains(got, "actual content") {
		t.Errorf("missing real content: %q", got)
	}
}

func TestExtractTextFromHTML_HiddenElementsDropped(t *testing.T) {
	html := `<html><body>
		<p hidden>secret</p>
		<p aria-hidden="true">also secret</p>
		<p style="display:none">hidden too</p>
		<p>visible</p>
	</body></html>`
	got := extractTextFromHTML(html, "")
	for _, drop := range []string{"secret", "also secret", "hidden too"} {
		if strings.Contains(got, drop) {
			t.Errorf("hidden content leaked (%q): %q", drop, got)
		}
	}
	if !strings.Contains(got, "visible") {
		t.Errorf("visible text was lost: %q", got)
	}
}

func TestExtractTextFromHTML_PrefersMain(t *testing.T) {
	// findContentRoot prefers <main> over <body>
	html := `<html><body>
		<header>SITE NAV</header>
		<main><p>main content</p></main>
		<aside>SIDEBAR</aside>
	</body></html>`
	got := extractTextFromHTML(html, "")
	if !strings.Contains(got, "main content") {
		t.Errorf("main content missing: %q", got)
	}
	if strings.Contains(got, "SITE NAV") {
		t.Errorf("site nav leaked when <main> should have been chosen: %q", got)
	}
}

func TestCollapseWhitespace(t *testing.T) {
	tests := map[string]string{
		"":                  "",
		"hello world":       "hello world",
		"hello  world":      "hello world",
		"hello\n\tworld":    "hello world",
		"  multi   space  ": " multi space ",
		"\n\nleading":       " leading",
	}
	for in, want := range tests {
		if got := collapseWhitespace(in); got != want {
			t.Errorf("collapseWhitespace(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestCleanupText(t *testing.T) {
	t.Run("collapses excessive blank lines", func(t *testing.T) {
		in := "first\n\n\n\n\nsecond"
		got := cleanupText(in)
		// Should keep at most 2 blank lines between paragraphs
		if strings.Contains(got, "\n\n\n\n") {
			t.Errorf("did not collapse blank lines: %q", got)
		}
		if !strings.Contains(got, "first") || !strings.Contains(got, "second") {
			t.Errorf("content lost: %q", got)
		}
	})
	t.Run("trims trailing whitespace per line", func(t *testing.T) {
		got := cleanupText("hello   \nworld\t")
		if got != "hello\nworld" {
			t.Errorf("got %q, want %q", got, "hello\nworld")
		}
	})
	t.Run("trims overall", func(t *testing.T) {
		got := cleanupText("\n\n\nbody\n\n\n")
		if got != "body" {
			t.Errorf("got %q, want %q", got, "body")
		}
	})
}

func TestBasicStripTags(t *testing.T) {
	in := `<p>hello <b>world</b></p>`
	got := basicStripTags(in)
	// Tags become spaces; we just need the text content present without angle brackets
	if strings.Contains(got, "<") || strings.Contains(got, ">") {
		t.Errorf("output still contains angle brackets: %q", got)
	}
	if !strings.Contains(got, "hello") || !strings.Contains(got, "world") {
		t.Errorf("text content lost: %q", got)
	}
}

func TestExtractTextFromHTML_FallsBackOnNoBody(t *testing.T) {
	// No body, no main, no article: extractor falls back to whole document.
	// We only assert it doesn't panic and produces something sensible.
	got := extractTextFromHTML(`<div>bare content</div>`, "")
	if !strings.Contains(got, "bare content") {
		t.Errorf("missing content: %q", got)
	}
}
