package store

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
)

// Action statuses.
const (
	ActionStarted   = "started"
	ActionSucceeded = "succeeded"
	ActionRejected  = "rejected"
	ActionFailed    = "failed"
)

// StartAction records an agent's tool call before the tool does anything,
// and returns its id for FinishAction.
func (s *Store) StartAction(ctx context.Context, runID, incidentID int64, tool string, input json.RawMessage) (int64, error) {
	return s.RecordAction(ctx, ActionStart{
		Actor: ActorAgent, RunID: runID, IncidentID: incidentID, Tool: tool, Input: input,
	})
}

// Actors in the audit trail.
const (
	ActorAgent = "agent"
	ActorUI    = "ui"
)

// ActionStart is an audit entry about to happen.
type ActionStart struct {
	Actor      string
	RunID      int64 // 0 for a human
	IncidentID int64
	// TargetIncidentID is the other incident a merge or link touches; 0 if none.
	TargetIncidentID int64
	Tool             string
	Input            json.RawMessage
}

// RecordAction writes an audit entry before its action runs, and returns
// its id for FinishAction. A target that does not exist is recorded only in
// the input: the action is then refused, but still audited.
func (s *Store) RecordAction(ctx context.Context, a ActionStart) (int64, error) {
	var id int64
	err := s.pool.QueryRow(ctx, `
		INSERT INTO actions (actor, run_id, incident_id, target_incident_id, tool, input)
		VALUES ($1, NULLIF($2, 0), $3, (SELECT id FROM incidents WHERE id = $4), $5, $6) RETURNING id`,
		a.Actor, a.RunID, a.IncidentID, a.TargetIncidentID, a.Tool, []byte(a.Input)).Scan(&id)
	return id, err
}

// Action is one audit entry.
type Action struct {
	ID               int64
	Actor            string
	RunID            *int64
	IncidentID       int64
	TargetIncidentID *int64
	Tool             string
	Input            json.RawMessage
	Status           string
	Result           string
	CreatedAt        time.Time
	FinishedAt       *time.Time
}

// IncidentActions returns the audit trail of everything done to or about an
// incident, oldest first: its runs' tool calls, and merges or links that
// name it from either side.
func (s *Store) IncidentActions(ctx context.Context, incidentID int64) ([]Action, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id, actor, run_id, incident_id, target_incident_id, tool, input, status, result,
			created_at, finished_at
		FROM actions WHERE incident_id = $1 OR target_incident_id = $1
		ORDER BY id`, incidentID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Action
	for rows.Next() {
		var a Action
		var input []byte
		if err := rows.Scan(&a.ID, &a.Actor, &a.RunID, &a.IncidentID, &a.TargetIncidentID, &a.Tool, &input,
			&a.Status, &a.Result, &a.CreatedAt, &a.FinishedAt); err != nil {
			return nil, err
		}
		a.Input = input
		out = append(out, a)
	}
	return out, rows.Err()
}

// FinishAction records how a tool call ended.
func (s *Store) FinishAction(ctx context.Context, id int64, status, result string) error {
	_, err := s.pool.Exec(ctx, `
		UPDATE actions SET status = $2, result = $3, finished_at = now() WHERE id = $1`,
		id, status, result)
	return err
}

// CountActions counts a run's calls to one tool, however they ended.
func (s *Store) CountActions(ctx context.Context, runID int64, tool string) (int, error) {
	var n int
	err := s.pool.QueryRow(ctx,
		`SELECT count(*) FROM actions WHERE run_id = $1 AND tool = $2`, runID, tool).Scan(&n)
	return n, err
}

// PR states and check rollups.
const (
	PROpen   = "open"
	PRMerged = "merged"
	PRClosed = "closed"

	ChecksPending = "pending"
	ChecksSuccess = "success"
	ChecksFailure = "failure"
)

// PullRequest is a PR the controller opened for an incident.
type PullRequest struct {
	ID         int64
	IncidentID int64
	RunID      int64
	Number     int
	URL        string
	Title      string
	Branch     string
	BaseSHA    string
	HeadSHA    string
	State      string
	Checks     string
	NotifiedAt *time.Time
	CreatedAt  time.Time
	UpdatedAt  time.Time
	ClosedAt   *time.Time
	// Files the PR changes, as the controller committed them.
	Files []string

	// From the incident, for lists.
	AlertName string
	Namespace string
}

const prColumns = `p.id, p.incident_id, p.run_id, p.number, p.url, p.title, p.branch, p.base_sha,
	p.head_sha, p.state, p.checks, p.notified_at, p.created_at, p.updated_at, p.closed_at, p.files,
	i.alertname, i.namespace`

const prFrom = ` FROM pull_requests p JOIN incidents i ON i.id = p.incident_id`

func scanPR(row pgx.Row) (PullRequest, error) {
	var p PullRequest
	err := row.Scan(&p.ID, &p.IncidentID, &p.RunID, &p.Number, &p.URL, &p.Title, &p.Branch, &p.BaseSHA,
		&p.HeadSHA, &p.State, &p.Checks, &p.NotifiedAt, &p.CreatedAt, &p.UpdatedAt, &p.ClosedAt, &p.Files,
		&p.AlertName, &p.Namespace)
	return p, err
}

// InsertPullRequest records a newly opened PR.
func (s *Store) InsertPullRequest(ctx context.Context, p PullRequest) (int64, error) {
	var id int64
	err := s.pool.QueryRow(ctx, `
		INSERT INTO pull_requests (incident_id, run_id, number, url, title, branch, base_sha, head_sha, files)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9) RETURNING id`,
		p.IncidentID, p.RunID, p.Number, p.URL, p.Title, p.Branch, p.BaseSHA, p.HeadSHA, nonNilSlice(p.Files)).Scan(&id)
	return id, err
}

// PullRequestForIncident returns the incident's PR, or ErrNotFound.
func (s *Store) PullRequestForIncident(ctx context.Context, incidentID int64) (PullRequest, error) {
	p, err := scanPR(s.pool.QueryRow(ctx, `SELECT `+prColumns+prFrom+` WHERE p.incident_id = $1`, incidentID))
	if errors.Is(err, pgx.ErrNoRows) {
		return PullRequest{}, ErrNotFound
	}
	return p, err
}

// OpenPullRequests lists PRs still awaiting review, oldest first.
func (s *Store) OpenPullRequests(ctx context.Context) ([]PullRequest, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+prColumns+prFrom+` WHERE p.state = 'open' ORDER BY p.created_at`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []PullRequest
	for rows.Next() {
		p, err := scanPR(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// PRStatus is what a poll of GitHub found.
type PRStatus struct {
	State   string
	Checks  string
	HeadSHA string
}

// UpdatePullRequest stores a poll result. closed_at is set the first time the
// PR is seen merged or closed.
func (s *Store) UpdatePullRequest(ctx context.Context, id int64, st PRStatus) error {
	_, err := s.pool.Exec(ctx, `
		UPDATE pull_requests SET
			updated_at = CASE WHEN state <> $2 OR checks <> $3 OR head_sha <> $4 THEN now() ELSE updated_at END,
			state = $2, checks = $3, head_sha = $4, polled_at = now(),
			closed_at = CASE WHEN $2 <> 'open' AND closed_at IS NULL THEN now() ELSE closed_at END
		WHERE id = $1`, id, st.State, st.Checks, st.HeadSHA)
	return err
}

// MarkPullRequestNotified records that the review push went out. It returns
// false if it already had, so a push is sent at most once.
func (s *Store) MarkPullRequestNotified(ctx context.Context, id int64) (bool, error) {
	tag, err := s.pool.Exec(ctx,
		`UPDATE pull_requests SET notified_at = now() WHERE id = $1 AND notified_at IS NULL`, id)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() == 1, nil
}

// SaveRunSession stores a run's gzipped claude session file. A re-upload
// replaces it.
func (s *Store) SaveRunSession(ctx context.Context, runID int64, gz []byte, size int) error {
	_, err := s.pool.Exec(ctx, `
		INSERT INTO run_sessions (run_id, jsonl_gz, size) VALUES ($1, $2, $3)
		ON CONFLICT (run_id) DO UPDATE SET jsonl_gz = $2, size = $3, uploaded_at = now()`,
		runID, gz, size)
	return err
}

func nonNilSlice(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}
