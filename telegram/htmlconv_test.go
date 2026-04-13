package telegram

import (
	"strings"
	"testing"
)

func TestConvertToTelegramHTMLPlainText(t *testing.T) {
	in := "Just plain text with <angle> & ampersand."
	out := convertToTelegramHTML(in)
	want := "Just plain text with &lt;angle&gt; &amp; ampersand."
	if out != want {
		t.Fatalf("plain escape mismatch.\nwant: %q\ngot:  %q", want, out)
	}
}

func TestConvertToTelegramHTMLBold(t *testing.T) {
	out := convertToTelegramHTML("hello **world** and **two**")
	if out != "hello <b>world</b> and <b>two</b>" {
		t.Fatalf("bold mismatch:\n%s", out)
	}
}

func TestConvertToTelegramHTMLInlineCode(t *testing.T) {
	out := convertToTelegramHTML("run `go test ./...` now")
	if out != "run <code>go test ./...</code> now" {
		t.Fatalf("inline code mismatch:\n%s", out)
	}
}

func TestConvertToTelegramHTMLInlineCodeEscapesContent(t *testing.T) {
	out := convertToTelegramHTML("call `<script>` to test")
	if !strings.Contains(out, "<code>&lt;script&gt;</code>") {
		t.Fatalf("inline code body must be HTML-escaped:\n%s", out)
	}
}

// The headline diff use case: ```diff fenced block must become
// <pre><code class="language-diff">…</code></pre> with body escaped.
func TestConvertToTelegramHTMLDiffBlock(t *testing.T) {
	in := "before:\n\n```diff\n- old\n+ new\n```\n\nafter"
	out := convertToTelegramHTML(in)
	want := `<pre><code class="language-diff">- old
+ new</code></pre>`
	if !strings.Contains(out, want) {
		t.Fatalf("diff block render failed.\nwant substring: %q\ngot:\n%s", want, out)
	}
	if !strings.Contains(out, "before:") || !strings.Contains(out, "after") {
		t.Fatalf("surrounding prose lost:\n%s", out)
	}
}

func TestConvertToTelegramHTMLFencedNoLang(t *testing.T) {
	in := "```\nplain block\n```"
	out := convertToTelegramHTML(in)
	if out != "<pre>plain block</pre>" {
		t.Fatalf("plain pre mismatch:\n%s", out)
	}
}

// Markdown special chars inside a code block must not be re-interpreted as
// formatting. This is the main reason for the placeholder pass.
func TestConvertToTelegramHTMLNoFormattingInsideCode(t *testing.T) {
	in := "```python\nprint(\"**not bold**\")\n```"
	out := convertToTelegramHTML(in)
	if !strings.Contains(out, `<pre><code class="language-python">print(&quot;**not bold**&quot;)</code></pre>`) {
		// Quote isn't escaped (Telegram doesn't require it outside attrs)
		// — actually our escape skips ", so the body keeps literal ".
		// Adjust assertion accordingly.
		if !strings.Contains(out, `<pre><code class="language-python">print("**not bold**")</code></pre>`) {
			t.Fatalf("formatting inside code block must not be transformed:\n%s", out)
		}
	}
}

func TestConvertToTelegramHTMLLink(t *testing.T) {
	in := "see [docs](https://example.com/x?a=1&b=2)"
	out := convertToTelegramHTML(in)
	// & in URL is escaped to &amp; by the escape pass before link rewrite.
	if !strings.Contains(out, `<a href="https://example.com/x?a=1&amp;b=2">docs</a>`) {
		t.Fatalf("link mismatch:\n%s", out)
	}
}

// Diff lines with markdown-significant characters (-, +) must survive
// untouched inside the <pre><code> wrapper.
func TestConvertToTelegramHTMLDiffPreservesMinusPlus(t *testing.T) {
	in := "```diff\n- removed\n+ added\n@@ -1,2 +1,2 @@\n```"
	out := convertToTelegramHTML(in)
	for _, want := range []string{"- removed", "+ added", "@@ -1,2 +1,2 @@"} {
		if !strings.Contains(out, want) {
			t.Fatalf("expected %q preserved, got:\n%s", want, out)
		}
	}
}

// Sanitize bad language hint to avoid emitting class="language-../foo".
func TestConvertToTelegramHTMLLangSanitize(t *testing.T) {
	in := "```c++\nint x;\n```"
	out := convertToTelegramHTML(in)
	if !strings.Contains(out, `<pre><code class="language-c++">int x;</code></pre>`) {
		t.Fatalf("c++ should pass sanitize, got:\n%s", out)
	}
}
