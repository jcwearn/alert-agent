package store

import (
	"context"
	"slices"
	"strconv"
	"strings"
	"time"
)

// IncidentFilter narrows the home page's list. The zero value matches
// everything. Filters match a family, a root incident and the incidents
// merged into it, as a whole: Status is the family's rolled-up status
// (firing while any member fires), and the member filters match when the
// root or any child matches all of them. From and To bound the root's
// opened_at, which is what the list sorts by.
type IncidentFilter struct {
	From, To  time.Time // [From, To); zero is unbounded
	Status    string    // "firing", "resolved" or ""
	Namespace string    // exact
	Severity  string    // exact
	Search    string    // case-insensitive substring of the alert name
}

// Cursor is a position in the list: a root incident's (opened_at, id).
type Cursor struct {
	OpenedAt time.Time
	ID       int64
}

// String is the cursor as a URL parameter: Unix microseconds, a dash, the id.
// Postgres keeps microseconds, so the round trip is exact.
func (c Cursor) String() string {
	return strconv.FormatInt(c.OpenedAt.UnixMicro(), 10) + "-" + strconv.FormatInt(c.ID, 10)
}

// ParseCursor reads Cursor.String.
func ParseCursor(s string) (Cursor, bool) {
	us, id, ok := strings.Cut(s, "-")
	if !ok {
		return Cursor{}, false
	}
	u, err1 := strconv.ParseInt(us, 10, 64)
	i, err2 := strconv.ParseInt(id, 10, 64)
	if err1 != nil || err2 != nil || i <= 0 {
		return Cursor{}, false
	}
	return Cursor{OpenedAt: time.UnixMicro(u).UTC(), ID: i}, true
}

// IncidentListPage is one page of families, newest first.
type IncidentListPage struct {
	// Incidents are the page's roots in order, then their children (newest
	// first), which is the shape GroupIncidents folds.
	Incidents []Incident
	// Newer and Older are the cursors for the neighbouring pages, nil when
	// there is nothing that way.
	Newer, Older *Cursor
}

// ListIncidentPage returns up to limit families matching f. With before, the
// page is the families older than it; with after, the families newer than
// it (the page just above); with neither, the newest.
func (s *Store) ListIncidentPage(ctx context.Context, f IncidentFilter, before, after *Cursor, limit int) (IncidentListPage, error) {
	where, args := familyFilter(f)
	cond, order := where, "DESC"
	qargs := slices.Clone(args)
	switch {
	case before != nil:
		qargs = append(qargs, before.OpenedAt, before.ID)
		cond += " AND (i.opened_at, i.id) < ($" + strconv.Itoa(len(qargs)-1) + ", $" + strconv.Itoa(len(qargs)) + ")"
	case after != nil:
		qargs = append(qargs, after.OpenedAt, after.ID)
		cond += " AND (i.opened_at, i.id) > ($" + strconv.Itoa(len(qargs)-1) + ", $" + strconv.Itoa(len(qargs)) + ")"
		order = "ASC"
	}
	qargs = append(qargs, limit+1)
	rows, err := s.pool.Query(ctx, `SELECT `+incidentColumns+` FROM incidents i WHERE `+cond+
		` ORDER BY i.opened_at `+order+`, i.id `+order+` LIMIT $`+strconv.Itoa(len(qargs)), qargs...)
	if err != nil {
		return IncidentListPage{}, err
	}
	roots, err := collectIncidents(rows)
	if err != nil {
		return IncidentListPage{}, err
	}
	more := len(roots) > limit
	if more {
		roots = roots[:limit]
	}
	if after != nil {
		slices.Reverse(roots)
	}

	var page IncidentListPage
	if len(roots) == 0 {
		return page, nil
	}
	first, last := cursorOf(roots[0]), cursorOf(roots[len(roots)-1])
	// The side the query walked towards is known from the extra row; the
	// side it came from needs a look, since a filter may leave nothing there.
	switch {
	case after != nil:
		if more {
			page.Newer = &first
		}
		if ok, err := s.familyExists(ctx, where, args, "<", last); err != nil {
			return page, err
		} else if ok {
			page.Older = &last
		}
	default:
		if more {
			page.Older = &last
		}
		if before != nil {
			if ok, err := s.familyExists(ctx, where, args, ">", first); err != nil {
				return page, err
			} else if ok {
				page.Newer = &first
			}
		}
	}

	ids := make([]int64, len(roots))
	for i, r := range roots {
		ids[i] = r.ID
	}
	rows, err = s.pool.Query(ctx, `SELECT `+incidentColumns+` FROM incidents i
		WHERE i.parent_id = ANY($1) ORDER BY i.opened_at DESC, i.id DESC`, ids)
	if err != nil {
		return page, err
	}
	children, err := collectIncidents(rows)
	if err != nil {
		return page, err
	}
	page.Incidents = slices.Concat(roots, children)
	return page, nil
}

func (s *Store) familyExists(ctx context.Context, where string, args []any, op string, c Cursor) (bool, error) {
	args = append(slices.Clone(args), c.OpenedAt, c.ID)
	n := len(args)
	var ok bool
	err := s.pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM incidents i WHERE `+where+
		` AND (i.opened_at, i.id) `+op+` ($`+strconv.Itoa(n-1)+`, $`+strconv.Itoa(n)+`))`, args...).Scan(&ok)
	return ok, err
}

// familyFilter is the WHERE clause, on roots aliased i, for f.
func familyFilter(f IncidentFilter) (where string, args []any) {
	arg := func(v any) string {
		args = append(args, v)
		return "$" + strconv.Itoa(len(args))
	}
	conds := []string{"i.parent_id IS NULL"}
	if !f.From.IsZero() {
		conds = append(conds, "i.opened_at >= "+arg(f.From))
	}
	if !f.To.IsZero() {
		conds = append(conds, "i.opened_at < "+arg(f.To))
	}
	firing := "(i.alert_status = 'firing' OR EXISTS (SELECT 1 FROM incidents c WHERE c.parent_id = i.id AND c.alert_status = 'firing'))"
	switch f.Status {
	case "firing":
		conds = append(conds, firing)
	case "resolved":
		conds = append(conds, "NOT "+firing)
	}
	// The member filters apply to one member together: "books and critical"
	// means a member that is both, not a books member and a critical one.
	var member []string // column = placeholder, without the table alias
	if f.Namespace != "" {
		member = append(member, "namespace = "+arg(f.Namespace))
	}
	if f.Severity != "" {
		member = append(member, "severity = "+arg(f.Severity))
	}
	if f.Search != "" {
		member = append(member, "alertname ILIKE "+arg("%"+likeEscape(f.Search)+"%"))
	}
	if len(member) > 0 {
		match := func(alias string) string {
			parts := make([]string, len(member))
			for i, m := range member {
				parts[i] = alias + "." + m
			}
			return strings.Join(parts, " AND ")
		}
		conds = append(conds, "(("+match("i")+") OR EXISTS (SELECT 1 FROM incidents c WHERE c.parent_id = i.id AND "+match("c")+"))")
	}
	return strings.Join(conds, " AND "), args
}

// likeEscape makes s match literally inside a LIKE pattern (whose default
// escape character is the backslash).
func likeEscape(s string) string {
	return strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(s)
}

func cursorOf(in Incident) Cursor { return Cursor{OpenedAt: in.OpenedAt, ID: in.ID} }

type incidentRows interface {
	Next() bool
	Scan(dest ...any) error
	Err() error
	Close()
}

func collectIncidents(rows incidentRows) ([]Incident, error) {
	defer rows.Close()
	var out []Incident
	for rows.Next() {
		in, err := scanIncident(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, in)
	}
	return out, rows.Err()
}

// IncidentFacets are the values the list's namespace and severity selects
// offer: every value an incident has, sorted.
func (s *Store) IncidentFacets(ctx context.Context) (namespaces, severities []string, err error) {
	rows, err := s.pool.Query(ctx, `
		SELECT 'ns', namespace FROM (SELECT DISTINCT namespace FROM incidents WHERE namespace <> '') n
		UNION ALL
		SELECT 'sev', severity FROM (SELECT DISTINCT severity FROM incidents WHERE severity <> '') s
		ORDER BY 1, 2`)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var kind, v string
		if err := rows.Scan(&kind, &v); err != nil {
			return nil, nil, err
		}
		if kind == "ns" {
			namespaces = append(namespaces, v)
		} else {
			severities = append(severities, v)
		}
	}
	return namespaces, severities, rows.Err()
}
