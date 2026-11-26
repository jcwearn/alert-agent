package ui

import (
	"net/url"
	"slices"

	"github.com/a-h/templ"
)

// ListQuery is the home page's filter form as the URL carries it, so a
// filtered view is a link. The controller turns it into a store filter.
type ListQuery struct {
	Range     string // "24h", "7d", "30d" or "" (any time)
	From, To  string // YYYY-MM-DD; either one overrides Range
	Status    string // "firing", "resolved" or ""
	Namespace string
	Severity  string
	Search    string
}

// Active reports whether any filter is set.
func (q ListQuery) Active() bool { return q != ListQuery{} }

func (q ListQuery) values() url.Values {
	v := url.Values{}
	for _, kv := range [][2]string{{"range", q.Range}, {"from", q.From}, {"to", q.To}, {"status", q.Status},
		{"ns", q.Namespace}, {"sev", q.Severity}, {"q", q.Search}} {
		if kv[1] != "" {
			v.Set(kv[0], kv[1])
		}
	}
	return v
}

// pageURL is the list with these filters, at a cursor ("before" or "after")
// or at the top when key is "".
func (q ListQuery) pageURL(key, cursor string) templ.SafeURL {
	v := q.values()
	if key != "" {
		v.Set(key, cursor)
	}
	if len(v) == 0 {
		return "/"
	}
	return templ.SafeURL("/?" + v.Encode())
}

// rangePreset is one of the "Opened" links.
type rangePreset struct{ Value, Label string }

var rangePresets = []rangePreset{{"", "Any time"}, {"24h", "24h"}, {"7d", "7 days"}, {"30d", "30 days"}}

func (q ListQuery) presetURL(r string) templ.SafeURL {
	q.Range, q.From, q.To = r, "", ""
	return q.pageURL("", "")
}

func (q ListQuery) presetCurrent(r string) bool {
	return q.From == "" && q.To == "" && q.Range == r
}

// IncidentListView is the home page's incident list: one page of families
// and what the filter form and the pager need.
type IncidentListView struct {
	Rows  []IncidentRow
	Query ListQuery
	// Namespaces and Severities fill the selects.
	Namespaces, Severities []string
	// Newer and Older are the neighbouring pages' cursors, "" when none.
	Newer, Older string
	// Paged is set when the URL holds a cursor, so the pager offers the top.
	Paged bool
	// Problems name the parameters that were ignored.
	Problems []string
}

// options are a select's values, with the current one kept even when no
// incident has it any more (a link from last month).
func options(values []string, current string) []string {
	if current != "" && !slices.Contains(values, current) {
		return append(slices.Clone(values), current)
	}
	return values
}
