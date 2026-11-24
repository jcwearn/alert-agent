package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// Run statuses.
const (
	RunQueued    = "queued"
	RunRunning   = "running"
	RunSucceeded = "succeeded"
	RunFailed    = "failed"
	RunSkipped   = "skipped"
)

// Run is one agent attempt at an incident.
type Run struct {
	ID         int64
	IncidentID int64
	Status     string
	SessionID  string
	JobName    string
	Model      string
	CreatedAt  time.Time
	StartedAt  *time.Time
	FinishedAt *time.Time
	Report     string
	Error      string
	NumTurns   *int
	DurationMS *int64
}

// RunEvent is one line of a run's transcript.
type RunEvent struct {
	Seq        int
	Type       string
	Payload    json.RawMessage
	ReceivedAt time.Time
}

// RunResult is what a runner reports when it finishes.
type RunResult struct {
	Status     string
	Report     string
	Error      string
	NumTurns   *int
	DurationMS *int64
	Usage      json.RawMessage
}

const runColumns = `id, incident_id, status, session_id::text, job_name, model, created_at,
	started_at, finished_at, report, error, num_turns, duration_ms`

func scanRun(row pgx.Row) (Run, error) {
	var r Run
	err := row.Scan(&r.ID, &r.IncidentID, &r.Status, &r.SessionID, &r.JobName, &r.Model, &r.CreatedAt,
		&r.StartedAt, &r.FinishedAt, &r.Report, &r.Error, &r.NumTurns, &r.DurationMS)
	return r, err
}

// EnqueueRun queues a run for an incident.
func (s *Store) EnqueueRun(ctx context.Context, incidentID int64) (int64, error) {
	var id int64
	err := s.pool.QueryRow(ctx,
		`INSERT INTO runs (incident_id, status) VALUES ($1, 'queued') RETURNING id`, incidentID,
	).Scan(&id)
	return id, err
}

// ClaimNextRun moves the oldest queued run to running, recording the token
// hash, Job name and model. Queued runs whose incident has already resolved
// are marked skipped on the way: there is nothing left to diagnose, and every
// run spends the subscription's usage.
//
// It returns false when nothing is queued, or when a run is already running
// (the runs_one_running index refuses a second one).
func (s *Store) ClaimNextRun(ctx context.Context, tokenHash []byte, jobPrefix, model string) (Run, bool, error) {
	var run Run
	var claimed bool
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		claimed = false
		if _, err := tx.Exec(ctx, `
			UPDATE runs r SET status = 'skipped', finished_at = now(),
				error = 'incident resolved before the agent started'
			FROM incidents i
			WHERE r.incident_id = i.id AND r.status = 'queued' AND i.alert_status = 'resolved'`,
		); err != nil {
			return fmt.Errorf("skip resolved: %w", err)
		}

		var running bool
		if err := tx.QueryRow(ctx,
			`SELECT EXISTS (SELECT 1 FROM runs WHERE status = 'running')`).Scan(&running); err != nil {
			return err
		}
		if running {
			return nil
		}

		row := tx.QueryRow(ctx, `
			UPDATE runs SET status = 'running', started_at = now(), token_hash = $1,
				job_name = $2 || id::text, model = $3
			WHERE id = (
				SELECT id FROM runs WHERE status = 'queued'
				ORDER BY id LIMIT 1 FOR UPDATE SKIP LOCKED
			)
			RETURNING `+runColumns, tokenHash, jobPrefix, model)
		r, err := scanRun(row)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("claim: %w", err)
		}
		run, claimed = r, true
		return nil
	})
	return run, claimed, err
}

// RunningRun returns the run currently running, if any.
func (s *Store) RunningRun(ctx context.Context) (Run, bool, error) {
	r, err := scanRun(s.pool.QueryRow(ctx, `SELECT `+runColumns+` FROM runs WHERE status = 'running'`))
	if errors.Is(err, pgx.ErrNoRows) {
		return Run{}, false, nil
	}
	return r, err == nil, err
}

// RunsStartedSince counts runs that started after t, for the hourly cap.
func (s *Store) RunsStartedSince(ctx context.Context, t time.Time) (int, error) {
	var n int
	err := s.pool.QueryRow(ctx, `SELECT count(*) FROM runs WHERE started_at > $1`, t).Scan(&n)
	return n, err
}

// RunByTokenHash returns the running run a runner token belongs to.
func (s *Store) RunByTokenHash(ctx context.Context, hash []byte) (Run, error) {
	r, err := scanRun(s.pool.QueryRow(ctx,
		`SELECT `+runColumns+` FROM runs WHERE token_hash = $1 AND status = 'running'`, hash))
	if errors.Is(err, pgx.ErrNoRows) {
		return Run{}, ErrNotFound
	}
	return r, err
}

// AppendRunEvents stores transcript lines. Lines already stored (a retried
// batch) are ignored.
func (s *Store) AppendRunEvents(ctx context.Context, runID int64, events []RunEvent) error {
	batch := &pgx.Batch{}
	for _, e := range events {
		batch.Queue(`INSERT INTO run_events (run_id, seq, type, payload) VALUES ($1, $2, $3, $4)
			ON CONFLICT (run_id, seq) DO NOTHING`, runID, e.Seq, e.Type, []byte(e.Payload))
	}
	return s.pool.SendBatch(ctx, batch).Close()
}

// FinishRun records a run's outcome and revokes its token. It returns false
// if the run was not running (already finished, or never started).
func (s *Store) FinishRun(ctx context.Context, runID int64, res RunResult) (bool, error) {
	if res.Status != RunSucceeded && res.Status != RunFailed {
		return false, fmt.Errorf("invalid finish status %q", res.Status)
	}
	var usage []byte
	if len(res.Usage) > 0 {
		usage = res.Usage
	}
	tag, err := s.pool.Exec(ctx, `
		UPDATE runs SET status = $2, finished_at = now(), token_hash = NULL,
			report = $3, error = $4, num_turns = $5, duration_ms = $6, usage = $7
		WHERE id = $1 AND status = 'running'`,
		runID, res.Status, res.Report, res.Error, res.NumTurns, res.DurationMS, usage)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() == 1, nil
}

// GetRun returns one run.
func (s *Store) GetRun(ctx context.Context, id int64) (Run, error) {
	r, err := scanRun(s.pool.QueryRow(ctx, `SELECT `+runColumns+` FROM runs WHERE id = $1`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return Run{}, ErrNotFound
	}
	return r, err
}

// ListRuns returns an incident's runs, newest first.
func (s *Store) ListRuns(ctx context.Context, incidentID int64) ([]Run, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT `+runColumns+` FROM runs WHERE incident_id = $1 ORDER BY id DESC`, incidentID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Run
	for rows.Next() {
		r, err := scanRun(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// RunEvents returns a run's transcript in order.
func (s *Store) RunEvents(ctx context.Context, runID int64) ([]RunEvent, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT seq, type, payload, received_at FROM run_events WHERE run_id = $1 ORDER BY seq`, runID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []RunEvent
	for rows.Next() {
		var e RunEvent
		var payload []byte
		if err := rows.Scan(&e.Seq, &e.Type, &payload, &e.ReceivedAt); err != nil {
			return nil, err
		}
		e.Payload = payload
		out = append(out, e)
	}
	return out, rows.Err()
}
