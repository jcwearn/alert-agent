package store

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/jcwearn/alert-agent/internal/alertmanager"
)

// openTestStore returns a migrated Store in a fresh schema, dropped after the
// test, so tests never see each other's rows.
func openTestStore(t *testing.T) *Store {
	t.Helper()
	url := testDatabaseURL
	ctx := t.Context()

	admin, err := pgx.Connect(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	schema := "test_" + time.Now().Format("150405_000000000")
	quoted := pgx.Identifier{schema}.Sanitize()
	if _, err := admin.Exec(ctx, "CREATE SCHEMA "+quoted); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = admin.Exec(context.Background(), "DROP SCHEMA "+quoted+" CASCADE")
		_ = admin.Close(context.Background())
	})

	cfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		t.Fatal(err)
	}
	cfg.ConnConfig.RuntimeParams["search_path"] = schema
	s, err := openConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	if err := s.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	return s
}

func payload(status string, alerts ...alertmanager.Alert) alertmanager.Payload {
	return alertmanager.Payload{
		Version:     "4",
		GroupKey:    `{}:{alertname="KubePodCrashLooping", namespace="ebooks"}`,
		Status:      status,
		GroupLabels: map[string]string{"alertname": "KubePodCrashLooping", "namespace": "ebooks"},
		CommonLabels: map[string]string{
			"alertname": "KubePodCrashLooping", "namespace": "ebooks", "severity": "warning",
		},
		CommonAnnotations: map[string]string{"summary": "Pod is crash looping"},
		Alerts:            alerts,
	}
}

func alert(fp, status string) alertmanager.Alert {
	return alertmanager.Alert{
		Status:      status,
		Fingerprint: fp,
		Labels:      map[string]string{"pod": fp},
		StartsAt:    time.Date(2026, 11, 24, 10, 0, 0, 0, time.UTC),
	}
}

func record(t *testing.T, s *Store, p alertmanager.Payload) RecordResult {
	t.Helper()
	raw, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	res, err := s.RecordWebhook(t.Context(), raw, p)
	if err != nil {
		t.Fatal(err)
	}
	return res
}

func TestIncidentLifecycle(t *testing.T) {
	s := openTestStore(t)
	ctx := t.Context()

	first := record(t, s, payload("firing", alert("a", "firing")))
	if !first.Created || first.IncidentID == 0 {
		t.Fatalf("first firing should create an incident: %+v", first)
	}

	// A repeat delivery for the same group, now with a second alert, folds
	// into the same incident.
	again := record(t, s, payload("firing", alert("a", "firing"), alert("b", "firing")))
	if again.Created || again.IncidentID != first.IncidentID {
		t.Fatalf("repeat should update incident %d: %+v", first.IncidentID, again)
	}

	resolved := record(t, s, payload("resolved", alert("a", "resolved"), alert("b", "resolved")))
	if !resolved.Resolved || resolved.IncidentID != first.IncidentID {
		t.Fatalf("resolve should close incident %d: %+v", first.IncidentID, resolved)
	}

	in, alerts, err := s.GetIncident(ctx, first.IncidentID)
	if err != nil {
		t.Fatal(err)
	}
	if in.AlertStatus != "resolved" || in.ResolvedAt == nil {
		t.Errorf("incident not resolved: %+v", in)
	}
	if in.AlertName != "KubePodCrashLooping" || in.Namespace != "ebooks" || in.Severity != "warning" {
		t.Errorf("incident labels wrong: %+v", in)
	}
	if len(alerts) != 2 || in.AlertCount != 2 {
		t.Fatalf("want 2 alerts, got %d (count %d)", len(alerts), in.AlertCount)
	}
	for _, a := range alerts {
		if a.Status != "resolved" {
			t.Errorf("alert %s still %s", a.Fingerprint, a.Status)
		}
	}

	// Firing again after resolution is a new incident.
	refire := record(t, s, payload("firing", alert("a", "firing")))
	if !refire.Created || refire.IncidentID == first.IncidentID {
		t.Fatalf("re-fire should open a new incident: %+v", refire)
	}

	list, err := s.ListIncidents(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 2 || list[0].ID != refire.IncidentID {
		t.Errorf("list should be newest first: %+v", list)
	}

	var deliveries int
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM webhook_deliveries`).Scan(&deliveries); err != nil {
		t.Fatal(err)
	}
	if deliveries != 4 {
		t.Errorf("every delivery is kept: got %d, want 4", deliveries)
	}
}

func TestResolveWithoutFiringIsRecordedOnly(t *testing.T) {
	s := openTestStore(t)

	res := record(t, s, payload("resolved", alert("a", "resolved")))
	if res.IncidentID != 0 || res.Resolved {
		t.Fatalf("orphan resolve should touch no incident: %+v", res)
	}

	var deliveries int
	if err := s.pool.QueryRow(t.Context(),
		`SELECT count(*) FROM webhook_deliveries WHERE incident_id IS NULL`).Scan(&deliveries); err != nil {
		t.Fatal(err)
	}
	if deliveries != 1 {
		t.Errorf("orphan delivery not kept: %d", deliveries)
	}
}

func TestMigrateIsIdempotent(t *testing.T) {
	s := openTestStore(t)
	if err := s.Migrate(t.Context()); err != nil {
		t.Fatalf("second migrate: %v", err)
	}
}

func TestGetIncidentNotFound(t *testing.T) {
	s := openTestStore(t)
	if _, _, err := s.GetIncident(t.Context(), 999); !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}
