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
