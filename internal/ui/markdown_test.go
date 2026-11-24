package ui

import (
	"bytes"
	"strings"
	"testing"
)

func renderMD(t *testing.T, s string) string {
	t.Helper()
	var buf bytes.Buffer
	if err := markdown(s).Render(t.Context(), &buf); err != nil {
		t.Fatal(err)
	}
	return buf.String()
}

func TestMarkdownRendersReports(t *testing.T) {
	out := renderMD(t, "## Summary\nThe `cache-warmer` pod is **OOMKilled**.\n\n- one\n- two\n\n```diff\n- memory: 128Mi\n+ memory: 384Mi\n```\n\n| a | b |\n|---|---|\n| 1 | 2 |\n")
	for _, want := range []string{"<h2>Summary</h2>", "<code>cache-warmer</code>", "<strong>OOMKilled</strong>",
		"<li>one</li>", `<code class="language-diff">`, "+ memory: 384Mi", "<table>"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
}

// Report text can quote untrusted alert annotations and logs.
func TestMarkdownIsSafe(t *testing.T) {
	out := renderMD(t, "<script>alert(1)</script>\n\n<img src=x onerror=alert(1)>\n\n[click](javascript:alert(1)) [data](data:text/html,x) [ok](https://example.com)\n\ninline <b onmouseover=x>b</b>")
	for _, bad := range []string{"<script", "<img", "onerror", "javascript:", "data:text", "<b ", "onmouseover"} {
		if strings.Contains(out, bad) {
			t.Errorf("unsafe %q survived:\n%s", bad, out)
		}
	}
	if !strings.Contains(out, `href="https://example.com"`) {
		t.Errorf("safe link lost:\n%s", out)
	}
}
