package controller

import (
	"net/url"
	"strings"
	"time"

	"github.com/jcwearn/alert-agent/internal/store"
	"github.com/jcwearn/alert-agent/internal/ui"
)

// defaultListPageSize is how many incident families the home page shows.
const defaultListPageSize = 50

var listRanges = map[string]time.Duration{"24h": 24 * time.Hour, "7d": 7 * 24 * time.Hour, "30d": 30 * 24 * time.Hour}

// listQuery is the home page's URL, read: the filter for the store, the
// cursor, the form's state, and any parameter that was ignored (named for
// the notice; a bad parameter never fails the page).
type listQuery struct {
	filter        store.IncidentFilter
	before, after *store.Cursor
	form          ui.ListQuery
	problems      []string
}

// parseListQuery reads range, from, to, status, ns, sev, q, before and
// after. Dates are days in the server's time zone (TZ), to inclusive.
func parseListQuery(v url.Values, now time.Time) listQuery {
	var q listQuery
	bad := func(name, value string) { q.problems = append(q.problems, name+"="+value) }

	from, to := strings.TrimSpace(v.Get("from")), strings.TrimSpace(v.Get("to"))
	var fromT, toT time.Time
	if from != "" {
		if t, err := time.ParseInLocation(time.DateOnly, from, time.Local); err == nil {
			fromT, q.form.From = t, from
		} else {
			bad("from", from)
		}
	}
	if to != "" {
		if t, err := time.ParseInLocation(time.DateOnly, to, time.Local); err == nil {
			toT, q.form.To = t.AddDate(0, 0, 1), to
		} else {
			bad("to", to)
		}
	}
	if !fromT.IsZero() && !toT.IsZero() && !fromT.Before(toT) {
		bad("from", from+" (after to)")
		fromT, toT, q.form.From, q.form.To = time.Time{}, time.Time{}, "", ""
	}
	q.filter.From, q.filter.To = fromT, toT
	if r := v.Get("range"); r != "" {
		switch d, ok := listRanges[r]; {
		case !ok:
			bad("range", r)
		case q.form.From == "" && q.form.To == "":
			q.form.Range, q.filter.From = r, now.Add(-d)
		}
	}

	switch st := v.Get("status"); st {
	case "", "firing", "resolved":
		q.form.Status, q.filter.Status = st, st
	default:
		bad("status", st)
	}
	q.form.Namespace = capped(v.Get("ns"), 253)
	q.form.Severity = capped(v.Get("sev"), 64)
	q.form.Search = capped(v.Get("q"), 200)
	q.filter.Namespace, q.filter.Severity, q.filter.Search = q.form.Namespace, q.form.Severity, q.form.Search

	if c := v.Get("before"); c != "" {
		if cur, ok := store.ParseCursor(c); ok {
			q.before = &cur
		} else {
			bad("before", c)
		}
	} else if c := v.Get("after"); c != "" {
		if cur, ok := store.ParseCursor(c); ok {
			q.after = &cur
		} else {
			bad("after", c)
		}
	}
	return q
}

func capped(s string, n int) string {
	s = strings.TrimSpace(s)
	if r := []rune(s); len(r) > n {
		return string(r[:n])
	}
	return s
}
