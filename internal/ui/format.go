// Package ui renders the controller's HTML pages with templ.
package ui

import (
	"fmt"
	"sort"
	"time"
)

// ago renders a duration since t the way a human reads an incident list.
func ago(t, now time.Time) string {
	d := now.Sub(t)
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd ago", int(d.Hours()/24))
	}
}

func stamp(t time.Time) string { return t.Local().Format("2006-01-02 15:04:05 MST") }

type kv struct{ K, V string }

// sorted returns map entries in key order, so a page renders the same way twice.
func sorted(m map[string]string) []kv {
	out := make([]kv, 0, len(m))
	for k, v := range m {
		out = append(out, kv{k, v})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].K < out[j].K })
	return out
}
