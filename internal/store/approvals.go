package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// Approval statuses.
const (
	ApprovalPending    = "pending"
	ApprovalApproved   = "approved"
	ApprovalRejected   = "rejected"
	ApprovalExpired    = "expired"
	ApprovalSuperseded = "superseded"
)

// Approval is a cluster action the agent asked a human to approve.
type Approval struct {
	ID int64
	// IncidentID is the root incident the request is about.
	IncidentID int64
	RunID      int64
	Tool       string
	Input      json.RawMessage
	Reason     string
	// Target and Object as the request's dry run resolved them.
	TargetKind, TargetNamespace, TargetName string
	Object                                  string
	Status                                  string
	CreatedAt                               time.Time
	ExpiresAt                               time.Time
	DecidedBy                               string
	DecidedAt                               *time.Time
	// ActionID is the action an approval ran, with its outcome.
	ActionID     *int64
	ActionStatus string
	ActionResult string
}

// Target names the workload, as "Kind ns/name".
func (a Approval) Target() string {
	if a.TargetKind == "" {
		return ""
	}
	return a.TargetKind + " " + a.TargetNamespace + "/" + a.TargetName
}

const approvalColumns = `p.id, p.incident_id, p.run_id, p.tool, p.input, p.reason, p.target_kind, p.target_namespace,
	p.target_name, p.object, p.status, p.created_at, p.expires_at, p.decided_by, p.decided_at, p.action_id,
	COALESCE(a.status, ''), COALESCE(a.result, '')`

const approvalFrom = ` FROM approvals p LEFT JOIN actions a ON a.id = p.action_id`

func scanApproval(row pgx.Row) (Approval, error) {
	var p Approval
	var input []byte
	err := row.Scan(&p.ID, &p.IncidentID, &p.RunID, &p.Tool, &input, &p.Reason, &p.TargetKind, &p.TargetNamespace,
		&p.TargetName, &p.Object, &p.Status, &p.CreatedAt, &p.ExpiresAt, &p.DecidedBy, &p.DecidedAt, &p.ActionID,
		&p.ActionStatus, &p.ActionResult)
	p.Input = input
	return p, err
}

func collectApprovals(rows pgx.Rows, err error) ([]Approval, error) {
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Approval
	for rows.Next() {
		p, err := scanApproval(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// RequestApproval records a pending request for p.IncidentID (the root
// incident), superseding any request still pending for it, and returns the
// new id and the ids it superseded.
func (s *Store) RequestApproval(ctx context.Context, p Approval) (id int64, superseded []int64, err error) {
	err = pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			UPDATE approvals SET status = 'superseded', decided_at = now(), decided_by = 'run:' || $2::bigint::text
			WHERE incident_id = $1 AND status = 'pending' RETURNING id`, p.IncidentID, p.RunID)
		if err != nil {
			return err
		}
		superseded, err = pgx.CollectRows(rows, pgx.RowTo[int64])
		if err != nil {
			return err
		}
		return tx.QueryRow(ctx, `
			INSERT INTO approvals (incident_id, run_id, tool, input, reason, target_kind, target_namespace,
				target_name, object, expires_at)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10) RETURNING id`,
			p.IncidentID, p.RunID, p.Tool, []byte(p.Input), p.Reason, p.TargetKind, p.TargetNamespace,
			p.TargetName, p.Object, p.ExpiresAt).Scan(&id)
	})
	return id, superseded, err
}

// GetApproval returns one approval, with the outcome of the action it ran.
func (s *Store) GetApproval(ctx context.Context, id int64) (Approval, error) {
	p, err := scanApproval(s.pool.QueryRow(ctx, `SELECT `+approvalColumns+approvalFrom+` WHERE p.id = $1`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return p, ErrNotFound
	}
	return p, err
}

// DecideApproval approves or rejects a pending, unexpired request. A
// request decided already (or expired, or superseded) is ErrConflict, so two
// clicks can never run an action twice.
func (s *Store) DecideApproval(ctx context.Context, id int64, approve bool, by string) (Approval, error) {
	status := ApprovalRejected
	if approve {
		status = ApprovalApproved
	}
	tag, err := s.pool.Exec(ctx, `
		UPDATE approvals SET status = $2, decided_by = $3, decided_at = now()
		WHERE id = $1 AND status = 'pending' AND expires_at > now()`, id, status, by)
	if err != nil {
		return Approval{}, err
	}
	p, err := s.GetApproval(ctx, id)
	if err != nil {
		return p, err
	}
	if tag.RowsAffected() == 0 {
		st := p.Status
		if st == ApprovalPending {
			st = "past its expiry"
		}
		return p, fmt.Errorf("%w: approval #%d is %s", ErrConflict, id, st)
	}
	return p, nil
}

// SetApprovalAction links an approved request to the action that ran it.
func (s *Store) SetApprovalAction(ctx context.Context, id, actionID int64) error {
	_, err := s.pool.Exec(ctx, `UPDATE approvals SET action_id = $2 WHERE id = $1`, id, actionID)
	return err
}

// ExpireApprovals marks pending requests expired once they pass their
// expiry, or once their incident and every incident merged into it have
// resolved: an approval is for a problem happening now.
func (s *Store) ExpireApprovals(ctx context.Context) ([]Approval, error) {
	rows, err := s.pool.Query(ctx, `
		UPDATE approvals p SET status = 'expired', decided_at = now(), decided_by = 'expiry'
		WHERE p.status = 'pending' AND (p.expires_at <= now() OR NOT EXISTS (
			SELECT 1 FROM incidents i
			WHERE (i.id = p.incident_id OR i.parent_id = p.incident_id) AND i.alert_status = 'firing'))
		RETURNING p.id`)
	if err != nil {
		return nil, err
	}
	ids, err := pgx.CollectRows(rows, pgx.RowTo[int64])
	if err != nil || len(ids) == 0 {
		return nil, err
	}
	return collectApprovals(s.pool.Query(ctx, `SELECT `+approvalColumns+approvalFrom+` WHERE p.id = ANY($1) ORDER BY p.id`, ids))
}

// PendingApprovals lists every request waiting for a human, oldest first.
func (s *Store) PendingApprovals(ctx context.Context) ([]Approval, error) {
	return collectApprovals(s.pool.Query(ctx, `SELECT `+approvalColumns+approvalFrom+
		` WHERE p.status = 'pending' AND p.expires_at > now() ORDER BY p.id`))
}

// IncidentApprovals lists the requests about an incident's merge family
// (they are filed on the root), oldest first.
func (s *Store) IncidentApprovals(ctx context.Context, incidentID int64) ([]Approval, error) {
	return collectApprovals(s.pool.Query(ctx, `SELECT `+approvalColumns+approvalFrom+`
		WHERE p.incident_id = (SELECT COALESCE(parent_id, id) FROM incidents WHERE id = $1) ORDER BY p.id`, incidentID))
}
