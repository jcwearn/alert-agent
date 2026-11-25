package ui

import (
	"math"
	"os"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// The colour tokens in layout.templ, light and dark, must keep text
// readable (WCAG AA, 4.5:1) and, in dark mode, keep borders visible
// against the page and cards (1.5:1): a dark card is told apart from the
// page by its border, not by its fill.
func TestColourContrast(t *testing.T) {
	src, err := os.ReadFile("layout.templ")
	if err != nil {
		t.Fatal(err)
	}
	light, dark := themes(t, string(src))
	text := [][2]string{
		{"fg", "bg"}, {"fg", "card"}, {"fg", "chip"},
		{"muted", "bg"}, {"muted", "card"}, {"muted", "chip"},
		{"firing", "firing-bg"}, {"resolved", "resolved-bg"}, {"resolved", "card"}, {"firing", "card"},
		{"on-accent", "accent"},
	}
	for name, theme := range map[string]map[string]string{"light": light, "dark": dark} {
		for _, p := range text {
			if r := contrast(t, theme, p[0], p[1]); r < 4.5 {
				t.Errorf("%s: %s on %s is %.2f:1, under 4.5:1", name, p[0], p[1], r)
			}
		}
	}
	for _, p := range [][2]string{{"line", "bg"}, {"line", "card"}} {
		if r := contrast(t, dark, p[0], p[1]); r < 1.5 {
			t.Errorf("dark: %s on %s is %.2f:1, under 1.5:1", p[0], p[1], r)
		}
	}
}

var tokenRE = regexp.MustCompile(`--([a-z-]+):\s*(#[0-9a-fA-F]{6})`)

// themes reads the light tokens (the first :root block) and the dark ones
// (the :root block inside the prefers-color-scheme: dark query).
func themes(t *testing.T, src string) (light, dark map[string]string) {
	t.Helper()
	i := strings.Index(src, "@media (prefers-color-scheme: dark)")
	if i < 0 {
		t.Fatal("no dark block in layout.templ")
	}
	read := func(from string) map[string]string {
		_, rest, ok := strings.Cut(from, ":root {")
		block, _, closed := strings.Cut(rest, "}")
		if !ok || !closed {
			t.Fatal("no :root block in layout.templ")
		}
		out := map[string]string{}
		for _, m := range tokenRE.FindAllStringSubmatch(block, -1) {
			out[m[1]] = m[2]
		}
		return out
	}
	light, dark = read(src), read(src[i:])
	for _, k := range []string{"bg", "fg", "card", "chip", "line", "muted", "accent", "on-accent"} {
		if light[k] == "" || dark[k] == "" {
			t.Fatalf("token --%s missing (light %q, dark %q)", k, light[k], dark[k])
		}
	}
	return light, dark
}

func contrast(t *testing.T, theme map[string]string, a, b string) float64 {
	t.Helper()
	la, lb := luminance(t, theme[a]), luminance(t, theme[b])
	return (math.Max(la, lb) + 0.05) / (math.Min(la, lb) + 0.05)
}

func luminance(t *testing.T, hex string) float64 {
	t.Helper()
	if len(hex) != 7 {
		t.Fatalf("bad colour %q", hex)
	}
	var l float64
	for i, w := range []float64{0.2126, 0.7152, 0.0722} {
		v, err := strconv.ParseUint(hex[1+2*i:3+2*i], 16, 8)
		if err != nil {
			t.Fatalf("bad colour %q", hex)
		}
		c := float64(v) / 255
		if c <= 0.03928 {
			c /= 12.92
		} else {
			c = math.Pow((c+0.055)/1.055, 2.4)
		}
		l += w * c
	}
	return l
}
