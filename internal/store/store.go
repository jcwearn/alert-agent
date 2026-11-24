// Package store is the controller's Postgres-backed record of incidents and
// everything that happens to them.
package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/jcwearn/alert-agent/internal/alertmanager"
)

// Store wraps a connection pool.
type Store struct {
	pool *pgxpool.Pool
}

// Open connects to Postgres at url and checks the connection.
func Open(ctx context.Context, url string) (*Store, error) {
	cfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		return nil, err
	}
	return openConfig(ctx, cfg)
}

func openConfig(ctx context.Context, cfg *pgxpool.Config) (*Store, error) {
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, err
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("ping: %w", err)
	}
	return &Store{pool: pool}, nil
}

// Close releases the pool.
func (s *Store) Close() { s.pool.Close() }

// Ping reports whether the database is reachable.
func (s *Store) Ping(ctx context.Context) error { return s.pool.Ping(ctx) }

// Incident is one row of the incident list.
type Incident struct {
	ID          int64
	AlertName   string
	Namespace   string
	Severity    string
	Summary     string
	AlertStatus string
	OpenedAt    time.Time
	UpdatedAt   time.Time
	ResolvedAt  *time.Time
	AlertCount  int
}

// IncidentAlert is one alert attached to an incident.
type IncidentAlert struct {
	Fingerprint  string
	Status       string
	Labels       map[string]string
	Annotations  map[string]string
	StartsAt     time.Time
	EndsAt       *time.Time
	GeneratorURL string
	UpdatedAt    time.Time
}

// RecordResult says what a webhook delivery did.
type RecordResult struct {
	// IncidentID is 0 when a resolve matched no firing incident.
	IncidentID int64
	Created    bool
	Resolved   bool
}

// ErrNotFound is returned for an incident ID that does not exist.
var ErrNotFound = errors.New("not found")

// RecordWebhook stores one Alertmanager delivery and folds it into the
// incident for its group, in a single transaction.
func (s *Store) RecordWebhook(ctx context.Context, raw []byte, p alertmanager.Payload) (RecordResult, error) {
	var res RecordResult
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		res = RecordResult{}

		switch p.Status {
		case alertmanager.StatusFiring:
			// xmax = 0 only on a freshly inserted row, which is how an upsert
			// says whether it inserted or updated.
			err := tx.QueryRow(ctx, `
				INSERT INTO incidents (group_key, alertname, namespace, severity, summary, alert_status)
				VALUES ($1, $2, $3, $4, $5, 'firing')
				ON CONFLICT (group_key) WHERE alert_status = 'firing'
				DO UPDATE SET summary = EXCLUDED.summary, updated_at = now()
				RETURNING id, (xmax = 0)`,
				p.GroupKey, p.Label("alertname"), p.Label("namespace"), p.Label("severity"), p.Summary(),
			).Scan(&res.IncidentID, &res.Created)
			if err != nil {
				return fmt.Errorf("upsert incident: %w", err)
			}

		case alertmanager.StatusResolved:
			err := tx.QueryRow(ctx, `
				UPDATE incidents
				SET alert_status = 'resolved', resolved_at = now(), updated_at = now()
				WHERE group_key = $1 AND alert_status = 'firing'
				RETURNING id`,
				p.GroupKey,
			).Scan(&res.IncidentID)
			switch {
			case errors.Is(err, pgx.ErrNoRows):
				// Nothing firing to resolve. Still record the delivery below.
			case err != nil:
				return fmt.Errorf("resolve incident: %w", err)
			default:
				res.Resolved = true
			}
		}

		if res.IncidentID != 0 {
			for _, a := range p.Alerts {
				if err := upsertAlert(ctx, tx, res.IncidentID, a); err != nil {
					return err
				}
			}
		}

		var incidentID *int64
		if res.IncidentID != 0 {
			incidentID = &res.IncidentID
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO webhook_deliveries (group_key, status, payload, incident_id)
			VALUES ($1, $2, $3, $4)`,
			p.GroupKey, p.Status, raw, incidentID,
		); err != nil {
			return fmt.Errorf("insert delivery: %w", err)
		}
		return nil
	})
	return res, err
}

func upsertAlert(ctx context.Context, tx pgx.Tx, incidentID int64, a alertmanager.Alert) error {
	labels, err := json.Marshal(nonNil(a.Labels))
	if err != nil {
		return err
	}
	annotations, err := json.Marshal(nonNil(a.Annotations))
	if err != nil {
		return err
	}
	// Alertmanager sends the zero time for an alert with no end yet.
	var endsAt *time.Time
	if !a.EndsAt.IsZero() && a.EndsAt.Year() > 1 {
		endsAt = &a.EndsAt
	}
	_, err = tx.Exec(ctx, `
		INSERT INTO incident_alerts
			(incident_id, fingerprint, status, labels, annotations, starts_at, ends_at, generator_url)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
		ON CONFLICT (incident_id, fingerprint) DO UPDATE SET
			status = EXCLUDED.status,
			labels = EXCLUDED.labels,
			annotations = EXCLUDED.annotations,
			ends_at = EXCLUDED.ends_at,
			updated_at = now()`,
		incidentID, a.Fingerprint, a.Status, labels, annotations, a.StartsAt, endsAt, a.GeneratorURL,
	)
	if err != nil {
		return fmt.Errorf("upsert alert %s: %w", a.Fingerprint, err)
	}
	return nil
}

func nonNil(m map[string]string) map[string]string {
	if m == nil {
		return map[string]string{}
	}
	return m
}

const incidentColumns = `
	i.id, i.alertname, i.namespace, i.severity, i.summary, i.alert_status,
	i.opened_at, i.updated_at, i.resolved_at,
	(SELECT count(*) FROM incident_alerts a WHERE a.incident_id = i.id)`

func scanIncident(row pgx.Row) (Incident, error) {
	var in Incident
	err := row.Scan(&in.ID, &in.AlertName, &in.Namespace, &in.Severity, &in.Summary, &in.AlertStatus,
		&in.OpenedAt, &in.UpdatedAt, &in.ResolvedAt, &in.AlertCount)
	return in, err
}

// ListIncidents returns the most recently opened incidents first.
func (s *Store) ListIncidents(ctx context.Context, limit int) ([]Incident, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT `+incidentColumns+` FROM incidents i ORDER BY i.opened_at DESC, i.id DESC LIMIT $1`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Incident
	for rows.Next() {
		in, err := scanIncident(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, in)
	}
	return out, rows.Err()
}

// GetIncident returns one incident and its alerts.
func (s *Store) GetIncident(ctx context.Context, id int64) (Incident, []IncidentAlert, error) {
	in, err := scanIncident(s.pool.QueryRow(ctx,
		`SELECT `+incidentColumns+` FROM incidents i WHERE i.id = $1`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return Incident{}, nil, ErrNotFound
	}
	if err != nil {
		return Incident{}, nil, err
	}

	rows, err := s.pool.Query(ctx, `
		SELECT fingerprint, status, labels, annotations, starts_at, ends_at, generator_url, updated_at
		FROM incident_alerts WHERE incident_id = $1
		ORDER BY status, starts_at`, id)
	if err != nil {
		return Incident{}, nil, err
	}
	defer rows.Close()

	var alerts []IncidentAlert
	for rows.Next() {
		var a IncidentAlert
		if err := rows.Scan(&a.Fingerprint, &a.Status, &a.Labels, &a.Annotations,
			&a.StartsAt, &a.EndsAt, &a.GeneratorURL, &a.UpdatedAt); err != nil {
			return Incident{}, nil, err
		}
		alerts = append(alerts, a)
	}
	return in, alerts, rows.Err()
}
