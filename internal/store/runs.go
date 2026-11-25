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

// Run kinds.
const (
	KindDiagnose = "diagnose"
	// KindChat is a reviewer's turn on an agent PR: it resumes the PR run's
	// session on the PR branch.
	KindChat = "chat"
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
	Kind       string
	// PRID is the open PR of a chat turn's incident when it was queued.
	PRID *int64
	// ThreadID is a chat turn's thread. A general thread's turns have no
	// incident: IncidentID is 0.
	ThreadID *int64
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

const runColumns = `id, COALESCE(incident_id, 0), status, session_id::text, job_name, model, created_at,
	started_at, finished_at, report, error, num_turns, duration_ms, kind, pr_id, thread_id`

func scanRun(row pgx.Row) (Run, error) {
	var r Run
	err := row.Scan(&r.ID, &r.IncidentID, &r.Status, &r.SessionID, &r.JobName, &r.Model, &r.CreatedAt,
		&r.StartedAt, &r.FinishedAt, &r.Report, &r.Error, &r.NumTurns, &r.DurationMS, &r.Kind, &r.PRID, &r.ThreadID)
	return r, err
}

// EnqueueRun queues a run for an incident. With a hold, the run waits that
// long before it can start, and an incident that opens meanwhile in the same
// namespace joins it instead of getting a run of its own: a burst of alerts
// about one problem costs one run. batched says the incident joined an
// existing held run, whose id is returned.
//
// The window is fixed from the first incident; it does not slide.
func (s *Store) EnqueueRun(ctx context.Context, incidentID int64, hold time.Duration) (runID int64, batched bool, err error) {
	err = pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		batched = false
		// Serialise enqueues, so two incidents of one burst cannot both miss
		// each other's held run.
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext('alert-agent enqueue'))`); err != nil {
			return err
		}
		if hold > 0 {
			err := tx.QueryRow(ctx, `
				SELECT r.id FROM runs r
				JOIN incidents primary_i ON primary_i.id = r.incident_id
				JOIN incidents new_i ON new_i.id = $1
				WHERE r.status = 'queued' AND r.hold_until > now()
					AND primary_i.namespace = new_i.namespace
				ORDER BY r.id LIMIT 1`, incidentID).Scan(&runID)
			switch {
			case err == nil:
				batched = true
			case !errors.Is(err, pgx.ErrNoRows):
				return fmt.Errorf("find held run: %w", err)
			}
		}
		if !batched {
			// The database's clock, like the claim's comparison.
			if err := tx.QueryRow(ctx, `
				INSERT INTO runs (incident_id, status, hold_until)
				VALUES ($1, 'queued', CASE WHEN $2 > 0 THEN now() + make_interval(secs => $2) END)
				RETURNING id`,
				incidentID, hold.Seconds()).Scan(&runID); err != nil {
				return fmt.Errorf("insert run: %w", err)
			}
		}
		_, err := tx.Exec(ctx, `INSERT INTO run_incidents (run_id, incident_id) VALUES ($1, $2)
			ON CONFLICT DO NOTHING`, runID, incidentID)
		return err
	})
	return runID, batched, err
}

// liveMember is true for a run's incident that still needs the agent: firing,
// and not merged into another.
const liveMember = `EXISTS (SELECT 1 FROM run_incidents ri JOIN incidents i ON i.id = ri.incident_id
	WHERE ri.run_id = r.id AND i.alert_status = 'firing' AND i.parent_id IS NULL)`

// ClaimNextRun moves the oldest queued run whose hold has passed to running,
// recording the token hash, Job name and model. Queued runs none of whose
// incidents still need the agent (all resolved or merged) are marked skipped
// on the way: there is nothing left to diagnose, and every run spends the
// subscription's usage. If the claimed run's primary incident no longer
// needs the agent but another of its incidents does, that one becomes the
// primary.
//
// Chat turns go first: a reviewer is waiting on them, and a new alert's run
// can wait a turn. Chat runs keep their incident (a general thread's have
// none) and are skipped only when their thread is archived.
//
// It returns false when nothing is ready, or when a run is already running
// (the runs_one_running index refuses a second one).
func (s *Store) ClaimNextRun(ctx context.Context, tokenHash []byte, jobPrefix, model string) (Run, bool, error) {
	return s.claim(ctx, tokenHash, jobPrefix, model, false)
}

// ClaimNextChatRun is ClaimNextRun for chat turns only: the dispatcher uses
// it when the alert runs' hourly cap is reached, since chat turns have their
// own budget (checked when a message arrives).
func (s *Store) ClaimNextChatRun(ctx context.Context, tokenHash []byte, jobPrefix, model string) (Run, bool, error) {
	return s.claim(ctx, tokenHash, jobPrefix, model, true)
}

func (s *Store) claim(ctx context.Context, tokenHash []byte, jobPrefix, model string, chatOnly bool) (Run, bool, error) {
	var run Run
	var claimed bool
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		claimed = false
		if _, err := tx.Exec(ctx, `
			UPDATE runs r SET status = 'skipped', finished_at = now(),
				error = 'every incident resolved or was merged before the agent started'
			WHERE r.status = 'queued' AND r.kind = 'diagnose' AND NOT `+liveMember,
		); err != nil {
			return fmt.Errorf("skip resolved: %w", err)
		}
		// A chat turn outlives its alert and its PR; only an archived thread
		// drops it.
		if _, err := tx.Exec(ctx, `
			WITH skipped AS (
				UPDATE runs r SET status = 'skipped', finished_at = now(),
					error = 'the conversation was archived before the turn started'
				WHERE r.status = 'queued' AND r.kind = 'chat'
					AND EXISTS (SELECT 1 FROM threads t WHERE t.id = r.thread_id AND t.archived_at IS NOT NULL)
				RETURNING r.id)
			UPDATE chat_messages SET status = 'failed' WHERE run_id IN (SELECT id FROM skipped)`,
		); err != nil {
			return fmt.Errorf("skip archived chats: %w", err)
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
				job_name = $2 || id::text, model = $3,
				incident_id = CASE WHEN kind = 'chat' THEN incident_id ELSE COALESCE((
					SELECT i.id FROM incidents i
					WHERE i.id = runs.incident_id AND i.alert_status = 'firing' AND i.parent_id IS NULL
				), (
					SELECT min(i.id) FROM run_incidents ri JOIN incidents i ON i.id = ri.incident_id
					WHERE ri.run_id = runs.id AND i.alert_status = 'firing' AND i.parent_id IS NULL
				), runs.incident_id) END
			WHERE id = (
				SELECT id FROM runs WHERE status = 'queued' AND (hold_until IS NULL OR hold_until <= now())
					AND (NOT $4 OR kind = 'chat')
				ORDER BY kind = 'chat' DESC, id LIMIT 1 FOR UPDATE SKIP LOCKED
			)
			RETURNING `+runColumns, tokenHash, jobPrefix, model, chatOnly)
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

// RunIncidents returns the incidents a run covers: its primary first, then
// the rest of its batch in the order they joined.
func (s *Store) RunIncidents(ctx context.Context, runID int64) ([]int64, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT ri.incident_id FROM run_incidents ri JOIN runs r ON r.id = ri.run_id
		WHERE ri.run_id = $1
		ORDER BY ri.incident_id = r.incident_id DESC, ri.added_at, ri.incident_id`, runID)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowTo[int64])
}

// LeftoverIncidents returns a finished run's batch members that still need
// the agent: not the primary, still firing, not merged, and with no other
// run queued or running for them. Each gets a run of its own.
func (s *Store) LeftoverIncidents(ctx context.Context, runID int64) ([]int64, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT i.id FROM run_incidents ri
		JOIN runs r ON r.id = ri.run_id
		JOIN incidents i ON i.id = ri.incident_id
		WHERE ri.run_id = $1 AND i.id <> r.incident_id
			AND i.alert_status = 'firing' AND i.parent_id IS NULL
			AND NOT EXISTS (
				SELECT 1 FROM run_incidents o JOIN runs orun ON orun.id = o.run_id
				WHERE o.incident_id = i.id AND o.run_id <> $1 AND orun.status IN ('queued', 'running'))
		ORDER BY i.id`, runID)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowTo[int64])
}

// RunningRun returns the run currently running, if any.
func (s *Store) RunningRun(ctx context.Context) (Run, bool, error) {
	r, err := scanRun(s.pool.QueryRow(ctx, `SELECT `+runColumns+` FROM runs WHERE status = 'running'`))
	if errors.Is(err, pgx.ErrNoRows) {
		return Run{}, false, nil
	}
	return r, err == nil, err
}

// RunsStartedSince counts alert runs (diagnoses) that started after t, for
// their hourly cap. Chat turns have a budget of their own.
func (s *Store) RunsStartedSince(ctx context.Context, t time.Time) (int, error) {
	var n int
	err := s.pool.QueryRow(ctx, `SELECT count(*) FROM runs WHERE started_at > $1 AND kind = 'diagnose'`, t).Scan(&n)
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
	return s.RunEventsAfter(ctx, runID, -1)
}
