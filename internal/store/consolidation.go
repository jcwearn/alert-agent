package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// ErrConflict is a merge or link the data model cannot take, such as a
// merge that would make an incident its own ancestor. Its message says why.
var ErrConflict = errors.New("conflict")

// MergeIncident folds child into into, recording who did it and why. Merges
// are one level deep: if into is itself merged, child goes to into's parent
// (the returned root), and child's own children move to the root with it.
// The child keeps its alerts, runs and history.
func (s *Store) MergeIncident(ctx context.Context, childID, intoID int64, mergedBy, reason string) (root int64, err error) {
	if childID == intoID {
		return 0, fmt.Errorf("%w: an incident cannot be merged into itself", ErrConflict)
	}
	err = pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx,
			`SELECT id, parent_id FROM incidents WHERE id = ANY($1) ORDER BY id FOR UPDATE`,
			[]int64{childID, intoID})
		if err != nil {
			return err
		}
		parents := map[int64]*int64{}
		for rows.Next() {
			var id int64
			var parent *int64
			if err := rows.Scan(&id, &parent); err != nil {
				rows.Close()
				return err
			}
			parents[id] = parent
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		intoParent, ok := parents[intoID]
		if _, childOK := parents[childID]; !ok || !childOK {
			return ErrNotFound
		}

		root = intoID
		if intoParent != nil {
			root = *intoParent
		}
		if root == childID {
			return fmt.Errorf("%w: #%d is merged into #%d; merging back would make a loop", ErrConflict, intoID, childID)
		}

		if _, err := tx.Exec(ctx,
			`UPDATE incidents SET parent_id = $2, updated_at = now() WHERE parent_id = $1`, childID, root); err != nil {
			return fmt.Errorf("move children: %w", err)
		}
		_, err = tx.Exec(ctx, `
			UPDATE incidents SET parent_id = $2, merged_at = now(), merged_by = $3, merge_reason = $4,
				updated_at = now()
			WHERE id = $1`, childID, root, mergedBy, reason)
		return err
	})
	return root, err
}

// UnmergeIncident makes a merged incident stand alone again, and returns
// the incident it had been merged into. Children that moved with it at
// merge time stay where they are. ErrNotFound if it was not merged.
func (s *Store) UnmergeIncident(ctx context.Context, id int64) (formerParent int64, err error) {
	err = s.pool.QueryRow(ctx, `
		WITH old AS (SELECT parent_id FROM incidents WHERE id = $1 AND parent_id IS NOT NULL FOR UPDATE)
		UPDATE incidents SET parent_id = NULL, merged_at = NULL, merged_by = '', merge_reason = '',
			updated_at = now()
		FROM old WHERE id = $1
		RETURNING old.parent_id`, id).Scan(&formerParent)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, ErrNotFound
	}
	return formerParent, err
}

// LinkIncidents records that two incidents are related. Linking a pair that
// is already linked keeps the first link and returns false.
func (s *Store) LinkIncidents(ctx context.Context, x, y int64, reason, actor string, runID int64) (bool, error) {
	if x == y {
		return false, fmt.Errorf("%w: an incident cannot be linked to itself", ErrConflict)
	}
	a, b := min(x, y), max(x, y)
	tag, err := s.pool.Exec(ctx, `
		INSERT INTO incident_links (a, b, reason, actor, run_id) VALUES ($1, $2, $3, $4, NULLIF($5, 0))
		ON CONFLICT (a, b) DO NOTHING`, a, b, reason, actor, runID)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() == 1, nil
}

// UnlinkIncidents removes a link. It returns false if there was none.
func (s *Store) UnlinkIncidents(ctx context.Context, x, y int64) (bool, error) {
	tag, err := s.pool.Exec(ctx, `DELETE FROM incident_links WHERE a = $1 AND b = $2`, min(x, y), max(x, y))
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() == 1, nil
}

// Link is a related incident, seen from one side.
type Link struct {
	Other     Incident
	Reason    string
	Actor     string
	RunID     *int64
	CreatedAt time.Time
}

// IncidentLinks returns the incidents linked to id, oldest link first.
func (s *Store) IncidentLinks(ctx context.Context, id int64) ([]Link, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT `+incidentColumns+`, l.reason, l.actor, l.run_id, l.created_at
		FROM incident_links l
		JOIN incidents i ON i.id = CASE WHEN l.a = $1 THEN l.b ELSE l.a END
		WHERE l.a = $1 OR l.b = $1
		ORDER BY l.created_at`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Link
	for rows.Next() {
		var l Link
		in, err := scanIncident(rows, &l.Reason, &l.Actor, &l.RunID, &l.CreatedAt)
		if err != nil {
			return nil, err
		}
		l.Other = in
		out = append(out, l)
	}
	return out, rows.Err()
}

// IncidentChildren returns the incidents merged into id.
func (s *Store) IncidentChildren(ctx context.Context, id int64) ([]Incident, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT `+incidentColumns+` FROM incidents i WHERE i.parent_id = $1 ORDER BY i.id`, id)
	if err != nil {
		return nil, err
	}
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

// RecentIncident is an incident as a run's context lists it: enough for
// the agent to judge whether its own incident is the same problem.
type RecentIncident struct {
	Incident
	// Labels holds each alert's labels.
	Labels []map[string]string
	// Report is the latest succeeded run's report, if any.
	Report string
	// PRNumber is the incident's agent PR, if it has one.
	PRNumber *int
}

// RecentIncidents returns incidents still firing or opened after since,
// newest first.
func (s *Store) RecentIncidents(ctx context.Context, since time.Time, limit int) ([]RecentIncident, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT `+incidentColumns+`,
			(SELECT coalesce(jsonb_agg(a.labels ORDER BY a.starts_at), '[]'::jsonb)
				FROM incident_alerts a WHERE a.incident_id = i.id),
			coalesce((SELECT r.report FROM runs r WHERE r.incident_id = i.id AND r.status = 'succeeded'
				ORDER BY r.id DESC LIMIT 1), ''),
			(SELECT p.number FROM pull_requests p WHERE p.incident_id = i.id)
		FROM incidents i
		WHERE i.alert_status = 'firing' OR i.opened_at > $1
		ORDER BY i.opened_at DESC, i.id DESC LIMIT $2`, since, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []RecentIncident
	for rows.Next() {
		var r RecentIncident
		in, err := scanIncident(rows, &r.Labels, &r.Report, &r.PRNumber)
		if err != nil {
			return nil, err
		}
		r.Incident = in
		out = append(out, r)
	}
	return out, rows.Err()
}
