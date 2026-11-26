package controller

import (
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/jcwearn/alert-agent/internal/store"
)

func TestParseListQuery(t *testing.T) {
	now := time.Date(2026, 11, 26, 15, 0, 0, 0, time.Local)
	day := func(s string) time.Time {
		d, _ := time.ParseInLocation(time.DateOnly, s, time.Local)
		return d
	}
	parse := func(raw string) listQuery {
		v, err := url.ParseQuery(raw)
		if err != nil {
			t.Fatal(err)
		}
		return parseListQuery(v, now)
	}

	q := parse("range=7d&status=firing&ns=books&sev=critical&q=+crash+")
	if !q.filter.From.Equal(now.Add(-7*24*time.Hour)) || !q.filter.To.IsZero() || q.form.Range != "7d" ||
		q.filter.Status != "firing" || q.filter.Namespace != "books" || q.filter.Severity != "critical" || q.filter.Search != "crash" || len(q.problems) > 0 {
		t.Errorf("range/status/ns/sev/q: %+v", q)
	}

	// Dates win over a preset, and to includes its whole day.
	q = parse("range=24h&from=2026-11-01&to=2026-11-03")
	if !q.filter.From.Equal(day("2026-11-01")) || !q.filter.To.Equal(day("2026-11-04")) || q.form.Range != "" || len(q.problems) > 0 {
		t.Errorf("from/to over range: %+v", q)
	}

	// Anything malformed is ignored and named, never fatal.
	q = parse("range=1y&from=yesterday&status=open&before=nope")
	if !q.filter.From.IsZero() || q.filter.Status != "" || q.before != nil || q.form.Active() {
		t.Errorf("bad values leaked through: %+v", q)
	}
	if got := strings.Join(q.problems, "; "); got != "from=yesterday; range=1y; status=open; before=nope" {
		t.Errorf("problems: %q", got)
	}
	q = parse("from=2026-11-05&to=2026-11-01")
	if !q.filter.From.IsZero() || !q.filter.To.IsZero() || len(q.problems) != 1 {
		t.Errorf("from after to: %+v", q)
	}

	c := store.Cursor{OpenedAt: time.Date(2026, 11, 2, 3, 4, 5, 6000, time.UTC), ID: 9}
	if q = parse("before=" + c.String() + "&after=" + c.String()); q.before == nil || q.after != nil || q.before.ID != 9 {
		t.Errorf("before wins over after: %+v", q)
	}
	if q = parse("after=" + c.String()); q.after == nil || !q.after.OpenedAt.Equal(c.OpenedAt) {
		t.Errorf("after: %+v", q)
	}
	if q = parse("q=" + strings.Repeat("x", 300)); len(q.filter.Search) != 200 {
		t.Errorf("search not capped: %d", len(q.filter.Search))
	}
}

// The home page passes the URL's filter and cursor to the store, shows the
// form filled in, and names what it ignored.
func TestIncidentListFilters(t *testing.T) {
	opened := time.Date(2026, 11, 24, 10, 0, 0, 0, time.UTC)
	fs := &fakeStore{incidents: []store.Incident{{ID: 42, AlertName: "KubePodCrashLooping", Namespace: "ebooks",
		Severity: "warning", AlertStatus: "firing", OpenedAt: opened, UpdatedAt: opened}}}
	s := New(fs, Options{}, nil, nil)
	c := store.Cursor{OpenedAt: opened, ID: 50}

	status, body := do(t, s.UIHandler(), http.MethodGet, "/?status=resolved&ns=ebooks&q=crash&range=bogus&before="+c.String(), "")
	if status != http.StatusOK {
		t.Fatalf("status %d", status)
	}
	if fs.listFilter.Status != "resolved" || fs.listFilter.Namespace != "ebooks" || fs.listFilter.Search != "crash" ||
		fs.listBefore == nil || fs.listBefore.ID != 50 || fs.listAfter != nil {
		t.Errorf("store got %+v before %v after %v", fs.listFilter, fs.listBefore, fs.listAfter)
	}
	for _, want := range []string{
		`<form class="card filters" method="get" action="/">`,
		`<option value="resolved" selected>resolved</option>`,
		`<option value="ebooks" selected>ebooks</option>`,
		`name="q" value="crash"`,
		`Ignored range=bogus.`,
		`« Newest</a>`,
		`href="/?ns=ebooks&amp;q=crash&amp;status=resolved"`, // Newest keeps the filters
	} {
		if !strings.Contains(body, want) {
			t.Errorf("page lacks %s", want)
		}
	}
}
