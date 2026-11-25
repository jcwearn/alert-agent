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

// ChatMessage is one message in a thread.
type ChatMessage struct {
	ID       int64
	ThreadID int64
	// PRID is the PR a GitHub comment (or a reply to one) is on; 0 otherwise.
	PRID  int64
	RunID *int64
	// Page is the UI path the message was sent from, if any.
	Page   string
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

const chatColumns = `id, thread_id, COALESCE(pr_id, 0), run_id, page, source, author, body, status, COALESCE(gh_kind, ''), COALESCE(gh_id, 0),
	COALESCE(gh_thread_id, 0), path, line, diff_hunk, html_url, reacted_at, created_at`

func scanChat(row pgx.Row) (ChatMessage, error) {
	var m ChatMessage
	err := row.Scan(&m.ID, &m.ThreadID, &m.PRID, &m.RunID, &m.Page, &m.Source, &m.Author, &m.Body, &m.Status, &m.GHKind, &m.GHID,
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
	// PerThreadHour caps the turns started for one thread in an hour.
	PerThreadHour int
	// AllHour caps the chat turns all threads together can start in an hour.
	// Alert runs have their own cap in the dispatcher.
	AllHour int
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

// ErrThreadArchived refuses a message to an archived thread.
var ErrThreadArchived = errors.New("the thread is archived")

// EnqueueChat records a reviewer's message in its thread and gives it a
// turn: the thread's waiting turn if it has one, or a new chat run within
// the limits. A new turn continues the thread's session, and carries the
// open PR of the thread's incident, if there is one.
func (s *Store) EnqueueChat(ctx context.Context, m ChatMessage, lim ChatLimits) (ChatEnqueue, error) {
	var res ChatEnqueue
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		res = ChatEnqueue{}
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext('alert-agent chat'), $1::int)`, m.ThreadID); err != nil {
			return err
		}
		var archived *time.Time
		if err := tx.QueryRow(ctx, `SELECT archived_at FROM threads WHERE id = $1`, m.ThreadID).Scan(&archived); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return ErrNotFound
			}
			return err
		}
		if archived != nil {
			return ErrThreadArchived
		}

		insert := func(runID *int64, status string) error {
			err := tx.QueryRow(ctx, `
				INSERT INTO chat_messages (thread_id, pr_id, run_id, page, source, author, body, status, gh_kind, gh_id,
					gh_thread_id, path, line, diff_hunk, html_url)
				VALUES ($1, NULLIF($2::bigint, 0), $3, $4, $5, $6, $7, $8, NULLIF($9::text, ''), NULLIF($10::bigint, 0),
					NULLIF($11::bigint, 0), $12, $13, $14, $15)
				ON CONFLICT (gh_kind, gh_id) DO NOTHING
				RETURNING id`,
				m.ThreadID, m.PRID, runID, m.Page, m.Source, m.Author, m.Body, status, m.GHKind, m.GHID, m.GHThreadID,
				m.Path, m.Line, m.DiffHunk, m.HTMLURL).Scan(&res.MessageID)
			if errors.Is(err, pgx.ErrNoRows) {
				res.Duplicate = true
				return nil
			}
			if err != nil {
				return err
			}
			// A general thread is named after its first message.
			_, err = tx.Exec(ctx, `UPDATE threads SET updated_at = now(),
				title = CASE WHEN title = '' AND kind = 'general' THEN left($2, 80) ELSE title END
				WHERE id = $1`, m.ThreadID, m.Body)
			return err
		}

		var waiting int64
		err := tx.QueryRow(ctx, `SELECT id FROM runs WHERE thread_id = $1 AND kind = 'chat' AND status = 'queued'`, m.ThreadID).Scan(&waiting)
		switch {
		case err == nil:
			res.RunID = waiting
			return insert(&waiting, ChatQueued)
		case !errors.Is(err, pgx.ErrNoRows):
			return err
		}

		var threadTurns, allTurns int
		var threadOldest, allOldest *time.Time
		const recentTurns = `FROM runs WHERE kind = 'chat' AND status <> 'skipped' AND created_at > now() - interval '1 hour'`
		if err := tx.QueryRow(ctx, `SELECT count(*), min(created_at) `+recentTurns+` AND thread_id = $1`, m.ThreadID).
			Scan(&threadTurns, &threadOldest); err != nil {
			return err
		}
		if err := tx.QueryRow(ctx, `SELECT count(*), min(created_at) `+recentTurns).Scan(&allTurns, &allOldest); err != nil {
			return err
		}
		switch {
		case lim.PerThreadHour > 0 && threadTurns >= lim.PerThreadHour:
			res.Capped = fmt.Sprintf("this conversation already had %d agent turns in the last hour (the limit)", threadTurns)
			if threadOldest != nil {
				res.RetryAt = threadOldest.Add(time.Hour)
			}
		case lim.AllHour > 0 && allTurns >= lim.AllHour:
			res.Capped = fmt.Sprintf("the agent already took %d chat turns in the last hour (the limit across all conversations)", allTurns)
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
		var incidentID *int64
		if err := tx.QueryRow(ctx, `
			INSERT INTO runs (incident_id, status, kind, thread_id, pr_id, session_id)
			SELECT t.incident_id, 'queued', 'chat', t.id,
				(SELECT p.id FROM pull_requests p WHERE p.incident_id = t.incident_id AND p.state = 'open'),
				t.session_id
			FROM threads t WHERE t.id = $1
			RETURNING id, incident_id`, m.ThreadID).Scan(&res.RunID, &incidentID); err != nil {
			return fmt.Errorf("insert chat run: %w", err)
		}
		res.NewRun = true
		if incidentID != nil {
			if _, err := tx.Exec(ctx, `INSERT INTO run_incidents (run_id, incident_id) VALUES ($1, $2)`, res.RunID, *incidentID); err != nil {
				return err
			}
		}
		_, err = tx.Exec(ctx, `UPDATE chat_messages SET run_id = $2 WHERE id = $1`, res.MessageID, res.RunID)
		return err
	})
	return res, err
}

// ChatMessages returns a thread's conversation, oldest first.
func (s *Store) ChatMessages(ctx context.Context, threadID int64) ([]ChatMessage, error) {
	return collectChat(s.pool.Query(ctx, `SELECT `+chatColumns+` FROM chat_messages WHERE thread_id = $1 ORDER BY id`, threadID))
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
		var threadID int64
		var prID *int64
		if err := tx.QueryRow(ctx, `SELECT thread_id, pr_id FROM runs WHERE id = $1 AND kind = 'chat' FOR UPDATE`, runID).
			Scan(&threadID, &prID); err != nil {
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
			INSERT INTO chat_messages (thread_id, pr_id, run_id, source, author, body, status)
			VALUES ($1, $2, $3, 'agent', 'agent', $4, 'reply') RETURNING `+chatColumns, threadID, prID, runID, reply))
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

// ActiveChatRun returns the thread's chat turn that is waiting or running, if any.
func (s *Store) ActiveChatRun(ctx context.Context, threadID int64) (Run, bool, error) {
	r, err := scanRun(s.pool.QueryRow(ctx, `SELECT `+runColumns+` FROM runs
		WHERE thread_id = $1 AND kind = 'chat' AND status IN ('queued', 'running') ORDER BY id LIMIT 1`, threadID))
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
