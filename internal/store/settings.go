package store

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
)

// Pause is the state of the switch that stops autonomous actions.
type Pause struct {
	Paused bool
	By     string
	At     time.Time
}

// ActionsPause reads the pause switch. Never set means not paused.
func (s *Store) ActionsPause(ctx context.Context) (Pause, error) {
	var p Pause
	err := s.pool.QueryRow(ctx, `
		SELECT COALESCE((value->>'paused')::boolean, false), updated_by, updated_at
		FROM settings WHERE key = 'actions_paused'`).Scan(&p.Paused, &p.By, &p.At)
	if errors.Is(err, pgx.ErrNoRows) {
		return Pause{}, nil
	}
	return p, err
}

// SetActionsPaused flips the pause switch and records who did it in the
// audit trail, in one transaction. changed is false if it was already so.
func (s *Store) SetActionsPaused(ctx context.Context, paused bool, by string) (changed bool, err error) {
	err = pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`, clusterActionLock); err != nil {
			return err
		}
		var was bool
		err := tx.QueryRow(ctx, `SELECT COALESCE((value->>'paused')::boolean, false) FROM settings WHERE key = 'actions_paused'`).Scan(&was)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		if was == paused {
			return nil
		}
		changed = true
		if _, err := tx.Exec(ctx, `
			INSERT INTO settings (key, value, updated_by, updated_at)
			VALUES ('actions_paused', jsonb_build_object('paused', $1::boolean), $2, now())
			ON CONFLICT (key) DO UPDATE SET value = EXCLUDED.value, updated_by = EXCLUDED.updated_by, updated_at = now()`,
			paused, by); err != nil {
			return err
		}
		tool, result := "resume_actions", "Autonomous actions resumed."
		if paused {
			tool, result = "pause_actions", "Autonomous actions paused."
		}
		_, err = tx.Exec(ctx, `
			INSERT INTO actions (actor, tool, input, status, result, finished_at)
			VALUES ($1, $2, '{}', 'succeeded', $3, now())`, by, tool, result)
		return err
	})
	return changed, err
}

// ClaudeBlock is a failure that stops every run until it clears: the token
// is refused (FailureAuth) or the subscription's limit is reached
// (FailureUsageLimit). Until is when the dispatcher next tries; the block
// itself stands until a run succeeds.
type ClaudeBlock struct {
	Reason string
	Until  time.Time
	Since  time.Time
}

// GetClaudeBlock returns the block, if one stands.
func (s *Store) GetClaudeBlock(ctx context.Context) (ClaudeBlock, bool, error) {
	var b ClaudeBlock
	err := s.pool.QueryRow(ctx, `
		SELECT value->>'reason', (value->>'until')::timestamptz, (value->>'since')::timestamptz
		FROM settings WHERE key = 'claude_blocked'`).Scan(&b.Reason, &b.Until, &b.Since)
	if errors.Is(err, pgx.ErrNoRows) {
		return ClaudeBlock{}, false, nil
	}
	return b, err == nil, err
}

// SetClaudeBlock records a block, or moves an existing one's retry time.
// started is true when no block of this reason stood before: the moment to
// tell a person, once.
func (s *Store) SetClaudeBlock(ctx context.Context, reason string, until time.Time) (started bool, err error) {
	err = s.pool.QueryRow(ctx, `
		WITH prev AS (SELECT value->>'reason' AS reason, value->>'since' AS since FROM settings WHERE key = 'claude_blocked')
		INSERT INTO settings (key, value, updated_by, updated_at)
		VALUES ('claude_blocked', jsonb_build_object(
			'reason', $1::text, 'until', $2::timestamptz,
			'since', COALESCE((SELECT since FROM prev WHERE reason = $1), now()::text)), 'controller', now())
		ON CONFLICT (key) DO UPDATE SET value = EXCLUDED.value, updated_by = EXCLUDED.updated_by, updated_at = now()
		RETURNING NOT EXISTS (SELECT 1 FROM prev WHERE reason = $1)`, reason, until).Scan(&started)
	return started, err
}

// ClearClaudeBlock removes the block. cleared is false if none stood.
func (s *Store) ClearClaudeBlock(ctx context.Context) (cleared bool, err error) {
	tag, err := s.pool.Exec(ctx, `DELETE FROM settings WHERE key = 'claude_blocked'`)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() == 1, nil
}
