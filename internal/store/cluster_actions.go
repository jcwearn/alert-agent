package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// Cluster action modes.
const (
	ModeAutonomous = "autonomous"
	ModeApproved   = "approved"
)

// clusterActionLock serialises guardrail checks with the insert they guard,
// so two calls can never both pass "one per incident" or the hourly cap.
const clusterActionLock = 7_416_323_012

// ClusterAction is a cluster action about to run.
type ClusterAction struct {
	Actor      string // ActorAgent (autonomous) or ActorUI (approved)
	RunID      int64  // 0 for a human
	IncidentID int64
	Tool       string
	Input      json.RawMessage
	Mode       string
	// Target is the workload the action is about; Object what it touches.
	TargetKind, TargetNamespace, TargetName string
	Object                                  string
	ApprovalID                              int64
}

// Guardrails bound autonomous actions. Approved actions skip them: a human
// decided.
type Guardrails struct {
	// Cooldown refuses an autonomous action on a workload, or for an alert in
	// a namespace, that had any action within it. A re-fire opens a new
	// incident, so this is what turns a recurrence into an escalation.
	Cooldown time.Duration
	// MaxPerHour caps autonomous actions across the cluster.
	MaxPerHour int
}

// StartClusterAction records a cluster action before it runs. An autonomous
// one must first pass the guardrails, checked in the same transaction as the
// insert: the pause switch, one action per incident (its merge family
// included), the cooldown and the hourly cap. A refusal is recorded too, as
// rejected, and returned as text for the agent; the action must then not run.
func (s *Store) StartClusterAction(ctx context.Context, a ClusterAction, g Guardrails) (id int64, refusal string, err error) {
	err = pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`, clusterActionLock); err != nil {
			return err
		}
		if a.Mode == ModeAutonomous {
			var err error
			if refusal, err = checkGuardrails(ctx, tx, a, g); err != nil {
				return err
			}
		}
		status, result := ActionStarted, ""
		if refusal != "" {
			status, result = ActionRejected, refusal
		}
		return tx.QueryRow(ctx, `
			INSERT INTO actions (actor, run_id, incident_id, tool, input, mode, target_kind, target_namespace,
				target_name, object, approval_id, status, result, finished_at)
			VALUES ($1, NULLIF($2, 0), $3, $4, $5, $6, $7, $8, $9, $10, NULLIF($11, 0), $12, $13,
				CASE WHEN $12 = 'rejected' THEN now() END)
			RETURNING id`,
			a.Actor, a.RunID, a.IncidentID, a.Tool, []byte(a.Input), a.Mode, a.TargetKind, a.TargetNamespace,
			a.TargetName, a.Object, a.ApprovalID, status, result).Scan(&id)
	})
	return id, refusal, err
}

const escalate = "Do not retry. Escalate: call request_approval to ask the owner, or propose_pr for a fix to the cause."

func checkGuardrails(ctx context.Context, tx pgx.Tx, a ClusterAction, g Guardrails) (string, error) {
	var paused bool
	err := tx.QueryRow(ctx, `SELECT COALESCE((value->>'paused')::boolean, false) FROM settings WHERE key = 'actions_paused'`).Scan(&paused)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return "", err
	}
	if paused {
		return "autonomous actions are paused by the owner. " + escalate, nil
	}

	var root int64
	var alertname, namespace string
	if err := tx.QueryRow(ctx, `SELECT COALESCE(parent_id, id), alertname, namespace FROM incidents WHERE id = $1`,
		a.IncidentID).Scan(&root, &alertname, &namespace); err != nil {
		return "", fmt.Errorf("incident %d: %w", a.IncidentID, err)
	}

	var prior priorRow
	err = tx.QueryRow(ctx, `
		SELECT a.id, a.incident_id, a.tool, a.object, a.status, a.created_at
		FROM actions a JOIN incidents i ON i.id = a.incident_id
		WHERE a.mode IS NOT NULL AND a.status <> 'rejected' AND COALESCE(i.parent_id, i.id) = $1
		ORDER BY a.id LIMIT 1`, root).Scan(&prior.id, &prior.incident, &prior.tool, &prior.object, &prior.status, &prior.at)
	switch {
	case err == nil:
		return fmt.Sprintf("incident #%d already had its one action: %s. One attempt per incident. %s", root, prior, escalate), nil
	case !errors.Is(err, pgx.ErrNoRows):
		return "", err
	}

	if g.Cooldown > 0 {
		err = tx.QueryRow(ctx, `
			SELECT a.id, a.incident_id, a.tool, a.object, a.status, a.created_at
			FROM actions a JOIN incidents i ON i.id = a.incident_id
			WHERE a.mode IS NOT NULL AND a.status <> 'rejected' AND a.created_at > now() - make_interval(secs => $1)
			  AND ((a.target_kind, a.target_namespace, a.target_name) = ($2, $3, $4)
			       OR (i.alertname = $5 AND i.namespace = $6))
			ORDER BY a.id DESC LIMIT 1`,
			g.Cooldown.Seconds(), a.TargetKind, a.TargetNamespace, a.TargetName, alertname, namespace,
		).Scan(&prior.id, &prior.incident, &prior.tool, &prior.object, &prior.status, &prior.at)
		switch {
		case err == nil:
			return fmt.Sprintf("this looks like a recurrence: %s for incident #%d, within the %s cooldown on %s %s/%s and on %s in %s. %s",
				prior, prior.incident, g.Cooldown, a.TargetKind, a.TargetNamespace, a.TargetName, alertname, orDashSQL(namespace), escalate), nil
		case !errors.Is(err, pgx.ErrNoRows):
			return "", err
		}
	}

	if g.MaxPerHour > 0 {
		var n int
		if err := tx.QueryRow(ctx, `
			SELECT count(*) FROM actions
			WHERE mode = 'autonomous' AND status <> 'rejected' AND created_at > now() - interval '1 hour'`).Scan(&n); err != nil {
			return "", err
		}
		if n >= g.MaxPerHour {
			return fmt.Sprintf("the hourly cap of %d autonomous actions is reached. %s", g.MaxPerHour, escalate), nil
		}
	}
	return "", nil
}

type priorRow struct {
	id, incident         int64
	tool, object, status string
	at                   time.Time
}

func (p priorRow) String() string {
	return fmt.Sprintf("action #%d %s on %s (%s) at %s", p.id, p.tool, p.object, p.status, p.at.UTC().Format("15:04 UTC"))
}

func orDashSQL(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

// PriorAction is a recent cluster action, for a run's context.
type PriorAction struct {
	ID         int64
	IncidentID int64
	AlertName  string
	Tool       string
	Mode       string
	Target     string
	Object     string
	Status     string
	Result     string
	CreatedAt  time.Time
}

// PriorClusterActions lists cluster actions since t that touched namespace,
// or were taken for an alert with this alertname in it: what a run must know
// before acting again. Refused attempts are left out; nothing happened.
func (s *Store) PriorClusterActions(ctx context.Context, namespace, alertname string, since time.Time) ([]PriorAction, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT a.id, a.incident_id, i.alertname, a.tool, a.mode,
			CASE WHEN a.target_kind = '' THEN '' ELSE a.target_kind || ' ' || a.target_namespace || '/' || a.target_name END,
			a.object, a.status, a.result, a.created_at
		FROM actions a JOIN incidents i ON i.id = a.incident_id
		WHERE a.mode IS NOT NULL AND a.status <> 'rejected' AND a.created_at > $3
		  AND (a.target_namespace = $1 OR (i.alertname = $2 AND i.namespace = $1))
		ORDER BY a.id`, namespace, alertname, since)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []PriorAction
	for rows.Next() {
		var p PriorAction
		if err := rows.Scan(&p.ID, &p.IncidentID, &p.AlertName, &p.Tool, &p.Mode, &p.Target, &p.Object, &p.Status,
			&p.Result, &p.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}
