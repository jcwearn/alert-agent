package store

import (
	"encoding/json"
	"errors"
	"testing"
)

func runningRun(t *testing.T, s *Store) Run {
	t.Helper()
	ctx := t.Context()
	incident := firingIncident(t, s)
	if _, _, err := s.EnqueueRun(ctx, incident, 0); err != nil {
		t.Fatal(err)
	}
	run, ok, err := s.ClaimNextRun(ctx, []byte("hash"), "alert-agent-run-", "opus")
	if err != nil || !ok {
		t.Fatalf("claim: %v %v", ok, err)
	}
	return run
}

func TestActions(t *testing.T) {
	s := openTestStore(t)
	ctx := t.Context()
	run := runningRun(t, s)

	id, err := s.StartAction(ctx, run.ID, run.IncidentID, "propose_pr", json.RawMessage(`{"title":"x"}`))
	if err != nil {
		t.Fatal(err)
	}
	if err := s.FinishAction(ctx, id, ActionRejected, "path refused"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.StartAction(ctx, run.ID, run.IncidentID, "propose_pr", json.RawMessage(`{}`)); err != nil {
		t.Fatal(err)
	}
	n, err := s.CountActions(ctx, run.ID, "propose_pr")
	if err != nil || n != 2 {
		t.Fatalf("count = %d, %v; want 2", n, err)
	}
	if n, _ := s.CountActions(ctx, run.ID, "other"); n != 0 {
		t.Errorf("other tool count = %d", n)
	}
}

func TestPullRequestLifecycle(t *testing.T) {
	s := openTestStore(t)
	ctx := t.Context()
	run := runningRun(t, s)

	if _, err := s.PullRequestForIncident(ctx, run.IncidentID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
	pr := PullRequest{IncidentID: run.IncidentID, RunID: run.ID, Number: 42, URL: "https://github.com/o/r/pull/42",
		Title: "fix(x): y", Branch: "agent/1", BaseSHA: "aaa", HeadSHA: "bbb",
		Files: []string{"apps/x/deployment.yaml"}}
	id, err := s.InsertPullRequest(ctx, pr)
	if err != nil {
		t.Fatal(err)
	}
	// One PR per incident.
	if _, err := s.InsertPullRequest(ctx, pr); err == nil {
		t.Fatal("second PR for the same incident was accepted")
	}

	got, err := s.PullRequestForIncident(ctx, run.IncidentID)
	if err != nil || got.ID != id || got.State != PROpen || got.Checks != ChecksPending || got.AlertName == "" ||
		len(got.Files) != 1 || got.Files[0] != "apps/x/deployment.yaml" {
		t.Fatalf("got %+v, %v", got, err)
	}
	open, err := s.OpenPullRequests(ctx)
	if err != nil || len(open) != 1 {
		t.Fatalf("open = %d, %v", len(open), err)
	}

	first, err := s.MarkPullRequestNotified(ctx, id)
	if err != nil || !first {
		t.Fatalf("first notify: %v %v", first, err)
	}
	again, err := s.MarkPullRequestNotified(ctx, id)
	if err != nil || again {
		t.Fatalf("second notify must be refused: %v %v", again, err)
	}

	if err := s.UpdatePullRequest(ctx, id, PRStatus{State: PRMerged, Checks: ChecksSuccess, HeadSHA: "ccc"}); err != nil {
		t.Fatal(err)
	}
	got, _ = s.PullRequestForIncident(ctx, run.IncidentID)
	if got.State != PRMerged || got.Checks != ChecksSuccess || got.HeadSHA != "ccc" || got.ClosedAt == nil {
		t.Fatalf("after merge: %+v", got)
	}
	if open, _ := s.OpenPullRequests(ctx); len(open) != 0 {
		t.Errorf("merged PR still listed as open")
	}
}

func TestSaveRunSession(t *testing.T) {
	s := openTestStore(t)
	ctx := t.Context()
	run := runningRun(t, s)
	if err := s.SaveRunSession(ctx, run.ID, []byte{1, 2}, 10); err != nil {
		t.Fatal(err)
	}
	if err := s.SaveRunSession(ctx, run.ID, []byte{3}, 20); err != nil {
		t.Fatalf("re-upload: %v", err)
	}
}
