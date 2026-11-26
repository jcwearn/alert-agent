package store

import (
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
)

type testIncident struct {
	name, ns, sev, status string
	opened                time.Time
	parent                int64
}

func insertIncident(t *testing.T, s *Store, in testIncident) int64 {
	t.Helper()
	var parent *int64
	if in.parent != 0 {
		parent = &in.parent
	}
	if in.status == "" {
		in.status = "resolved"
	}
	var id int64
	err := s.pool.QueryRow(t.Context(), `INSERT INTO incidents (group_key, alertname, namespace, severity, alert_status, opened_at, parent_id)
		VALUES (gen_random_uuid()::text, $1, $2, $3, $4, $5, $6) RETURNING id`,
		in.name, in.ns, in.sev, in.status, in.opened, parent).Scan(&id)
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func pageIDs(p IncidentListPage) []int64 {
	var ids []int64
	for _, in := range p.Incidents {
		ids = append(ids, in.ID)
	}
	return ids
}

// A family (root plus merged children) never splits across pages, and the
// cursors walk both ways.
func TestListIncidentPageKeepsFamiliesTogether(t *testing.T) {
	s := openTestStore(t)
	ctx := t.Context()
	base := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	var roots []int64
	for i := range 7 {
		roots = append(roots, insertIncident(t, s, testIncident{name: "A", opened: base.Add(time.Duration(i) * time.Hour)}))
	}
	// Two children of the fourth-newest root, opened after the newest root:
	// they still sit with their parent, not at the top.
	c1 := insertIncident(t, s, testIncident{name: "B", opened: base.Add(10 * time.Hour), parent: roots[3]})
	c2 := insertIncident(t, s, testIncident{name: "C", opened: base.Add(11 * time.Hour), parent: roots[3]})

	p1, err := s.ListIncidentPage(ctx, IncidentFilter{}, nil, nil, 3)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := pageIDs(p1), []int64{roots[6], roots[5], roots[4]}; !slices.Equal(got, want) {
		t.Fatalf("page 1 = %v, want %v", got, want)
	}
	if p1.Newer != nil || p1.Older == nil {
		t.Fatalf("page 1 cursors: newer %v older %v", p1.Newer, p1.Older)
	}

	p2, err := s.ListIncidentPage(ctx, IncidentFilter{}, p1.Older, nil, 3)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := pageIDs(p2), []int64{roots[3], roots[2], roots[1], c2, c1}; !slices.Equal(got, want) {
		t.Fatalf("page 2 = %v, want %v (roots, then children newest first)", got, want)
	}
	if p2.Newer == nil || p2.Older == nil {
		t.Fatalf("page 2 cursors: newer %v older %v", p2.Newer, p2.Older)
	}

	p3, err := s.ListIncidentPage(ctx, IncidentFilter{}, p2.Older, nil, 3)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := pageIDs(p3), []int64{roots[0]}; !slices.Equal(got, want) || p3.Older != nil || p3.Newer == nil {
		t.Fatalf("page 3 = %v (newer %v, older %v), want %v", got, p3.Newer, p3.Older, want)
	}

	// Back up from page 3: the page above is page 2, in the same order.
	back, err := s.ListIncidentPage(ctx, IncidentFilter{}, nil, p3.Newer, 3)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(pageIDs(back), pageIDs(p2)) || back.Newer == nil || back.Older == nil {
		t.Fatalf("newer than page 3 = %v, want page 2 %v", pageIDs(back), pageIDs(p2))
	}
	top, err := s.ListIncidentPage(ctx, IncidentFilter{}, nil, back.Newer, 3)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(pageIDs(top), pageIDs(p1)) || top.Newer != nil {
		t.Fatalf("newer than page 2 = %v (newer %v), want page 1 %v", pageIDs(top), top.Newer, pageIDs(p1))
	}

	// The cursor survives its URL form.
	c, ok := ParseCursor(p1.Older.String())
	if !ok || !c.OpenedAt.Equal(p1.Older.OpenedAt) || c.ID != p1.Older.ID {
		t.Fatalf("cursor round trip: %v -> %q -> %v", *p1.Older, p1.Older.String(), c)
	}
	for _, bad := range []string{"", "x", "1-", "-1", "1-0", "a-b"} {
		if _, ok := ParseCursor(bad); ok {
			t.Errorf("ParseCursor(%q) accepted", bad)
		}
	}
}

// Roots opened in the same microsecond are ordered by id, and a page
// boundary between them neither drops nor repeats one.
func TestListIncidentPageBreaksTiesByID(t *testing.T) {
	s := openTestStore(t)
	at := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	a := insertIncident(t, s, testIncident{name: "A", opened: at})
	b := insertIncident(t, s, testIncident{name: "B", opened: at})
	c := insertIncident(t, s, testIncident{name: "C", opened: at})
	p1, err := s.ListIncidentPage(t.Context(), IncidentFilter{}, nil, nil, 2)
	if err != nil {
		t.Fatal(err)
	}
	p2, err := s.ListIncidentPage(t.Context(), IncidentFilter{}, p1.Older, nil, 2)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := append(pageIDs(p1), pageIDs(p2)...), []int64{c, b, a}; !slices.Equal(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

// Filters match a family as a whole and return the whole family.
func TestListIncidentPageFilters(t *testing.T) {
	s := openTestStore(t)
	ctx := t.Context()
	base := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	// A resolved books root with a firing gateway child.
	books := insertIncident(t, s, testIncident{name: "KubePodCrashLooping", ns: "books", sev: "warning", opened: base})
	envoy := insertIncident(t, s, testIncident{name: "EnvoyHighLatency", ns: "gateway", sev: "critical", status: "firing", opened: base.Add(time.Minute), parent: books})
	// A resolved photos root, critical, ten days earlier.
	photos := insertIncident(t, s, testIncident{name: "Foo_Bar", ns: "photos", sev: "critical", opened: base.Add(-240 * time.Hour)})
	// A firing gateway root, warning.
	gw := insertIncident(t, s, testIncident{name: "FooXBar", ns: "gateway", sev: "warning", status: "firing", opened: base.Add(time.Hour)})

	for _, c := range []struct {
		name string
		f    IncidentFilter
		want []int64
	}{
		{"everything", IncidentFilter{}, []int64{gw, books, photos, envoy}},
		{"firing rolls up a child", IncidentFilter{Status: "firing"}, []int64{gw, books, envoy}},
		{"resolved means no member fires", IncidentFilter{Status: "resolved"}, []int64{photos}},
		{"namespace matched by a child", IncidentFilter{Namespace: "gateway"}, []int64{gw, books, envoy}},
		{"namespace matched by the root", IncidentFilter{Namespace: "books"}, []int64{books, envoy}},
		{"member filters hold on one member", IncidentFilter{Namespace: "gateway", Severity: "critical"}, []int64{books, envoy}},
		{"no member is books and critical", IncidentFilter{Namespace: "books", Severity: "critical"}, nil},
		{"search is case-insensitive", IncidentFilter{Search: "envoy"}, []int64{books, envoy}},
		{"search treats _ literally", IncidentFilter{Search: "o_B"}, []int64{photos}},
		{"search treats % literally", IncidentFilter{Search: "%"}, nil},
		{"from", IncidentFilter{From: base.Add(-time.Hour)}, []int64{gw, books, envoy}},
		{"to is exclusive", IncidentFilter{To: base}, []int64{photos}},
		{"range on the root, not the child", IncidentFilter{From: base.Add(30 * time.Second), To: base.Add(2 * time.Minute)}, nil},
	} {
		p, err := s.ListIncidentPage(ctx, c.f, nil, nil, 10)
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if got := pageIDs(p); !slices.Equal(got, c.want) {
			t.Errorf("%s: got %v, want %v", c.name, got, c.want)
		}
	}

	// A filter that leaves nothing newer: no Newer link on the way back.
	p, err := s.ListIncidentPage(ctx, IncidentFilter{Status: "resolved"}, &Cursor{OpenedAt: base, ID: books}, nil, 10)
	if err != nil {
		t.Fatal(err)
	}
	if got := pageIDs(p); !slices.Equal(got, []int64{photos}) || p.Newer != nil || p.Older != nil {
		t.Errorf("resolved, older than #%d: %v newer %v older %v", books, got, p.Newer, p.Older)
	}

	ns, sev, err := s.IncidentFacets(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(ns, []string{"books", "gateway", "photos"}) || !slices.Equal(sev, []string{"critical", "warning"}) {
		t.Errorf("facets: %v %v", ns, sev)
	}
}

// The page query can walk the roots index rather than sort the table.
func TestListIncidentPageUsesIndex(t *testing.T) {
	s := openTestStore(t)
	ctx := t.Context()
	conn, err := s.pool.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Release()
	// A test table is tiny, so the planner would scan it whatever the
	// indexes; turning seq scans off asks whether an index can serve.
	if _, err := conn.Exec(ctx, "SET enable_seqscan = off"); err != nil {
		t.Fatal(err)
	}
	defer func() { _, _ = conn.Exec(ctx, "RESET enable_seqscan") }()
	where, args := familyFilter(IncidentFilter{Status: "firing", Namespace: "books"})
	args = append(args, time.Now(), int64(100))
	n := len(args)
	rows, err := conn.Query(ctx, `EXPLAIN SELECT i.id FROM incidents i WHERE `+where+
		` AND (i.opened_at, i.id) < ($`+strconv.Itoa(n-1)+`, $`+strconv.Itoa(n)+`) ORDER BY i.opened_at DESC, i.id DESC LIMIT 51`, args...)
	if err != nil {
		t.Fatal(err)
	}
	var plan []string
	for rows.Next() {
		var line string
		if err := rows.Scan(&line); err != nil {
			t.Fatal(err)
		}
		plan = append(plan, line)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if p := strings.Join(plan, "\n"); !strings.Contains(p, "incidents_roots_by_opened") || strings.Contains(p, "Sort") {
		t.Errorf("plan does not walk incidents_roots_by_opened:\n%s", p)
	}
}
