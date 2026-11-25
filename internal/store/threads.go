package store

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
)

// Thread kinds.
const (
	ThreadIncident = "incident"
	ThreadGeneral  = "general"
)

// Thread is one conversation with the agent, with its own claude session.
type Thread struct {
	ID         int64
	Kind       string
	IncidentID *int64
	Title      string
	SessionID  string
	CreatedAt  time.Time
	UpdatedAt  time.Time
	ArchivedAt *time.Time

	// For lists: the incident's alert, and the conversation's size.
	AlertName string
	Namespace string
	Messages  int
}

const threadColumns = `t.id, t.kind, t.incident_id, t.title, t.session_id::text, t.created_at, t.updated_at, t.archived_at,
	COALESCE(i.alertname, ''), COALESCE(i.namespace, ''),
	(SELECT count(*) FROM chat_messages m WHERE m.thread_id = t.id)`

const threadFrom = ` FROM threads t LEFT JOIN incidents i ON i.id = t.incident_id`

func scanThread(row pgx.Row) (Thread, error) {
	var t Thread
	err := row.Scan(&t.ID, &t.Kind, &t.IncidentID, &t.Title, &t.SessionID, &t.CreatedAt, &t.UpdatedAt, &t.ArchivedAt,
		&t.AlertName, &t.Namespace, &t.Messages)
	return t, err
}

// GetThread returns one thread, or ErrNotFound.
func (s *Store) GetThread(ctx context.Context, id int64) (Thread, error) {
	t, err := scanThread(s.pool.QueryRow(ctx, `SELECT `+threadColumns+threadFrom+` WHERE t.id = $1`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return Thread{}, ErrNotFound
	}
	return t, err
}

// IncidentThread returns the incident's thread. With create, a missing one
// is made, continuing the agent's own session for the incident: the run
// that opened its PR, or else its latest run that stored a session; with
// neither, a new session. It reports whether the thread already existed.
// The caller resolves a merged incident to its parent first.
func (s *Store) IncidentThread(ctx context.Context, incidentID int64, create bool) (Thread, bool, error) {
	t, err := scanThread(s.pool.QueryRow(ctx, `SELECT `+threadColumns+threadFrom+` WHERE t.incident_id = $1`, incidentID))
	if err == nil {
		return t, true, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return Thread{}, false, err
	}
	if !create {
		return Thread{}, false, ErrNotFound
	}
	if _, err := s.pool.Exec(ctx, `
		INSERT INTO threads (kind, incident_id, session_id)
		VALUES ('incident', $1, COALESCE(
			(SELECT r.session_id FROM pull_requests p JOIN runs r ON r.id = p.run_id WHERE p.incident_id = $1),
			(SELECT r.session_id FROM runs r JOIN run_sessions rs ON rs.run_id = r.id
				WHERE r.incident_id = $1 AND r.kind = 'diagnose' ORDER BY r.id DESC LIMIT 1),
			gen_random_uuid()))
		ON CONFLICT (incident_id) DO NOTHING`, incidentID); err != nil {
		return Thread{}, false, err
	}
	t, err = scanThread(s.pool.QueryRow(ctx, `SELECT `+threadColumns+threadFrom+` WHERE t.incident_id = $1`, incidentID))
	return t, false, err
}

// CreateGeneralThread starts a conversation about the cluster as a whole.
// It is named after its first message.
func (s *Store) CreateGeneralThread(ctx context.Context) (Thread, error) {
	var id int64
	if err := s.pool.QueryRow(ctx, `INSERT INTO threads (kind) VALUES ('general') RETURNING id`).Scan(&id); err != nil {
		return Thread{}, err
	}
	return s.GetThread(ctx, id)
}

// RecentThreads lists threads that have messages, most recently active
// first, plus empty general threads (a just-created one must be listed).
func (s *Store) RecentThreads(ctx context.Context, limit int) ([]Thread, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+threadColumns+threadFrom+`
		WHERE t.archived_at IS NULL
			AND (t.kind = 'general' OR EXISTS (SELECT 1 FROM chat_messages m WHERE m.thread_id = t.id))
		ORDER BY t.updated_at DESC, t.id DESC LIMIT $1`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Thread
	for rows.Next() {
		t, err := scanThread(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// ArchiveThread hides a thread from the lists and refuses new messages.
func (s *Store) ArchiveThread(ctx context.Context, id int64) error {
	_, err := s.pool.Exec(ctx, `UPDATE threads SET archived_at = now() WHERE id = $1 AND archived_at IS NULL`, id)
	return err
}

// HasStoredSession reports whether a thread's session has a stored file a
// turn before beforeRunID can resume; without one, the turn starts it.
func (s *Store) HasStoredSession(ctx context.Context, sessionID string, beforeRunID int64) (bool, error) {
	var ok bool
	err := s.pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM run_sessions s JOIN runs r ON r.id = s.run_id
		WHERE r.session_id = $1::uuid AND r.id < $2)`, sessionID, beforeRunID).Scan(&ok)
	return ok, err
}
