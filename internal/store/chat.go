package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// Chat message sources.
const (
	SourceUI     = "ui"
	SourceGitHub = "github"
	SourceAgent  = "agent"
)

// Chat message statuses.
const (
	ChatQueued   = "queued"
	ChatAnswered = "answered"
	ChatFailed   = "failed"
	ChatCapped   = "capped"
	ChatReply    = "reply"
)

// GitHub comment kinds.
const (
	GHIssueComment  = "issue_comment"
	GHReviewComment = "review_comment"
	GHReview        = "review"
)

// ChatMessage is one message in a PR's conversation.
type ChatMessage struct {
	ID     int64
	PRID   int64
	RunID  *int64
	Source string
	Author string
	Body   string
	Status string
	// GitHub comments only.
	GHKind     string
	GHID       int64
	GHThreadID int64
	Path       string
	Line       *int
	DiffHunk   string
	HTMLURL    string
	ReactedAt  *time.Time
	CreatedAt  time.Time
}

const chatColumns = `id, pr_id, run_id, source, author, body, status, COALESCE(gh_kind, ''), COALESCE(gh_id, 0),
	COALESCE(gh_thread_id, 0), path, line, diff_hunk, html_url, reacted_at, created_at`

func scanChat(row pgx.Row) (ChatMessage, error) {
	var m ChatMessage
	err := row.Scan(&m.ID, &m.PRID, &m.RunID, &m.Source, &m.Author, &m.Body, &m.Status, &m.GHKind, &m.GHID,
		&m.GHThreadID, &m.Path, &m.Line, &m.DiffHunk, &m.HTMLURL, &m.ReactedAt, &m.CreatedAt)
	return m, err
}

func collectChat(rows pgx.Rows, err error) ([]ChatMessage, error) {
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ChatMessage
	for rows.Next() {
		m, err := scanChat(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// ChatLimits bound how many turns a reviewer can start.
type ChatLimits struct {
	// PerPRHour caps the turns started for one PR in an hour.
	PerPRHour int
	// GlobalHour is the dispatcher's cap on runs of any kind in an hour.
	GlobalHour int
}

// ChatEnqueue is what became of a message.
type ChatEnqueue struct {
	MessageID int64
	RunID     int64
	// NewRun is false when the message joined a turn that was still waiting.
	NewRun bool
	// Capped says why no turn was queued, if one was not; the message is
	// stored with status capped.
	Capped string
	// RetryAt is when the limit that capped it frees a slot.
	RetryAt time.Time
	// Duplicate is true for a GitHub comment already recorded: nothing was done.
	Duplicate bool
}

// ErrPRNotOpen refuses a message about a PR that is merged or closed.
var ErrPRNotOpen = errors.New("the PR is no longer open")

// EnqueueChat records a reviewer's message and gives it a turn: the PR's
// waiting turn if it has one, or a new chat run within the limits. A new
// turn resumes the session of the run that opened the PR.
func (s *Store) EnqueueChat(ctx context.Context, m ChatMessage, lim ChatLimits) (ChatEnqueue, error) {
	var res ChatEnqueue
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		res = ChatEnqueue{}
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext('alert-agent chat'), $1::int)`, m.PRID); err != nil {
			return err
		}
		var (
			incidentID, prRunID int64
			state               string
		)
		if err := tx.QueryRow(ctx, `SELECT incident_id, run_id, state FROM pull_requests WHERE id = $1`, m.PRID).
			Scan(&incidentID, &prRunID, &state); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return ErrNotFound
			}
			return err
		}
		if state != PROpen {
			return ErrPRNotOpen
		}

		insert := func(runID *int64, status string) error {
			err := tx.QueryRow(ctx, `
				INSERT INTO chat_messages (pr_id, run_id, source, author, body, status, gh_kind, gh_id,
					gh_thread_id, path, line, diff_hunk, html_url)
				VALUES ($1, $2, $3, $4, $5, $6, NULLIF($7::text, ''), NULLIF($8::bigint, 0), NULLIF($9::bigint, 0), $10, $11, $12, $13)
				ON CONFLICT (gh_kind, gh_id) DO NOTHING
				RETURNING id`,
				m.PRID, runID, m.Source, m.Author, m.Body, status, m.GHKind, m.GHID, m.GHThreadID,
				m.Path, m.Line, m.DiffHunk, m.HTMLURL).Scan(&res.MessageID)
			if errors.Is(err, pgx.ErrNoRows) {
				res.Duplicate = true
				return nil
			}
			return err
		}

		var waiting int64
		err := tx.QueryRow(ctx, `SELECT id FROM runs WHERE pr_id = $1 AND kind = 'chat' AND status = 'queued'`, m.PRID).Scan(&waiting)
		switch {
		case err == nil:
			res.RunID = waiting
			return insert(&waiting, ChatQueued)
		case !errors.Is(err, pgx.ErrNoRows):
			return err
		}

		var prTurns, allRuns int
		var prOldest, allOldest *time.Time
		if err := tx.QueryRow(ctx, `
			SELECT count(*), min(created_at) FROM runs
			WHERE pr_id = $1 AND kind = 'chat' AND status <> 'skipped' AND created_at > now() - interval '1 hour'`,
			m.PRID).Scan(&prTurns, &prOldest); err != nil {
			return err
		}
		if err := tx.QueryRow(ctx, `SELECT count(*), min(started_at) FROM runs WHERE started_at > now() - interval '1 hour'`).
			Scan(&allRuns, &allOldest); err != nil {
			return err
		}
		switch {
		case lim.PerPRHour > 0 && prTurns >= lim.PerPRHour:
			res.Capped = fmt.Sprintf("this PR already had %d agent turns in the last hour (the limit)", prTurns)
			if prOldest != nil {
				res.RetryAt = prOldest.Add(time.Hour)
			}
		case lim.GlobalHour > 0 && allRuns >= lim.GlobalHour:
			res.Capped = fmt.Sprintf("the agent already started %d runs in the last hour (the limit across all incidents)", allRuns)
			if allOldest != nil {
				res.RetryAt = allOldest.Add(time.Hour)
			}
		}
		if res.Capped != "" {
			return insert(nil, ChatCapped)
		}

		// Recorded first: a duplicate comment must not cost a turn.
		if err := insert(nil, ChatQueued); err != nil || res.Duplicate {
			return err
		}
		if err := tx.QueryRow(ctx, `
			INSERT INTO runs (incident_id, status, kind, pr_id, session_id)
			SELECT $1, 'queued', 'chat', $2, session_id FROM runs WHERE id = $3
			RETURNING id`, incidentID, m.PRID, prRunID).Scan(&res.RunID); err != nil {
			return fmt.Errorf("insert chat run: %w", err)
		}
		res.NewRun = true
		if _, err := tx.Exec(ctx, `INSERT INTO run_incidents (run_id, incident_id) VALUES ($1, $2)`, res.RunID, incidentID); err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `UPDATE chat_messages SET run_id = $2 WHERE id = $1`, res.MessageID, res.RunID)
		return err
	})
	return res, err
}

// ChatMessages returns a PR's conversation, oldest first.
func (s *Store) ChatMessages(ctx context.Context, prID int64) ([]ChatMessage, error) {
	return collectChat(s.pool.Query(ctx, `SELECT `+chatColumns+` FROM chat_messages WHERE pr_id = $1 ORDER BY id`, prID))
}

// TurnMessages returns the reviewer's messages a chat turn answers.
func (s *Store) TurnMessages(ctx context.Context, runID int64) ([]ChatMessage, error) {
	return collectChat(s.pool.Query(ctx,
		`SELECT `+chatColumns+` FROM chat_messages WHERE run_id = $1 AND source <> 'agent' ORDER BY id`, runID))
}

// FinishChatTurn closes a chat turn's messages and records the agent's
// reply. It is done once: a second call (a retried finish) does nothing and
// returns false.
func (s *Store) FinishChatTurn(ctx context.Context, runID int64, answered bool, reply string) (ChatMessage, bool, error) {
	var out ChatMessage
	var first bool
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		status := ChatAnswered
		if !answered {
			status = ChatFailed
		}
		var prID int64
		if err := tx.QueryRow(ctx, `SELECT pr_id FROM runs WHERE id = $1 AND kind = 'chat' FOR UPDATE`, runID).Scan(&prID); err != nil {
			return err
		}
		var exists bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM chat_messages WHERE run_id = $1 AND source = 'agent')`, runID).
			Scan(&exists); err != nil || exists {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE chat_messages SET status = $2 WHERE run_id = $1 AND status = 'queued'`, runID, status); err != nil {
			return err
		}
		m, err := scanChat(tx.QueryRow(ctx, `
			INSERT INTO chat_messages (pr_id, run_id, source, author, body, status)
			VALUES ($1, $2, 'agent', 'agent', $3, 'reply') RETURNING `+chatColumns, prID, runID, reply))
		if err != nil {
			return err
		}
		out, first = m, true
		return nil
	})
	return out, first, err
}

// LatestSession returns the newest stored session file of a conversation:
// the last run with this session id before beforeRunID that uploaded one.
func (s *Store) LatestSession(ctx context.Context, sessionID string, beforeRunID int64) ([]byte, error) {
	var gz []byte
	err := s.pool.QueryRow(ctx, `
		SELECT s.jsonl_gz FROM run_sessions s JOIN runs r ON r.id = s.run_id
		WHERE r.session_id = $1::uuid AND r.id < $2
		ORDER BY r.id DESC LIMIT 1`, sessionID, beforeRunID).Scan(&gz)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	return gz, err
}

// ActiveChatRun returns the PR's chat turn that is waiting or running, if any.
func (s *Store) ActiveChatRun(ctx context.Context, prID int64) (Run, bool, error) {
	r, err := scanRun(s.pool.QueryRow(ctx, `SELECT `+runColumns+` FROM runs
		WHERE pr_id = $1 AND kind = 'chat' AND status IN ('queued', 'running') ORDER BY id LIMIT 1`, prID))
	if errors.Is(err, pgx.ErrNoRows) {
		return Run{}, false, nil
	}
	return r, err == nil, err
}

// RunEventsAfter returns a run's transcript lines after seq, in order.
func (s *Store) RunEventsAfter(ctx context.Context, runID int64, seq int) ([]RunEvent, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT seq, type, payload, received_at FROM run_events WHERE run_id = $1 AND seq > $2 ORDER BY seq`, runID, seq)
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

// AdvanceCommentsSince moves a PR's comment cursor forward (never back).
func (s *Store) AdvanceCommentsSince(ctx context.Context, prID int64, t time.Time) error {
	_, err := s.pool.Exec(ctx, `UPDATE pull_requests SET comments_since = GREATEST(comments_since, $2) WHERE id = $1`, prID, t)
	return err
}

// MarkChatReacted records the acknowledgement reaction on a comment.
func (s *Store) MarkChatReacted(ctx context.Context, id int64) error {
	_, err := s.pool.Exec(ctx, `UPDATE chat_messages SET reacted_at = now() WHERE id = $1`, id)
	return err
}

// SetChatMessageURL records where a reply was posted on GitHub.
func (s *Store) SetChatMessageURL(ctx context.Context, id int64, url string) error {
	_, err := s.pool.Exec(ctx, `UPDATE chat_messages SET html_url = $2 WHERE id = $1`, id, url)
	return err
}
