package store

import (
	"encoding/json"
	"errors"
	"strconv"
	"testing"
	"time"
)

func firingIncident(t *testing.T, s *Store) int64 {
	t.Helper()
	res := record(t, s, payload("firing", alert("a", "firing")))
	return res.IncidentID
}

func TestRunLifecycle(t *testing.T) {
	s := openTestStore(t)
	ctx := t.Context()
	incident := firingIncident(t, s)

	runID, _, err := s.EnqueueRun(ctx, incident, 0)
	if err != nil {
		t.Fatal(err)
	}

	run, ok, err := s.ClaimNextRun(ctx, []byte("hash-1"), "alert-agent-run-", "opus")
	if err != nil || !ok {
		t.Fatalf("claim: ok=%v err=%v", ok, err)
	}
	if run.ID != runID || run.Status != RunRunning || run.JobName == "" || run.Model != "opus" || run.SessionID == "" {
		t.Fatalf("claimed run wrong: %+v", run)
	}
	if want := "alert-agent-run-" + itoa(runID); run.JobName != want {
		t.Errorf("job name = %q, want %q", run.JobName, want)
	}

	byToken, err := s.RunByTokenHash(ctx, []byte("hash-1"))
	if err != nil || byToken.ID != runID {
		t.Fatalf("token lookup: %+v %v", byToken, err)
	}

	events := []RunEvent{
		{Seq: 0, Type: "system", Payload: json.RawMessage(`{"type":"system"}`)},
		{Seq: 1, Type: "assistant", Payload: json.RawMessage(`{"type":"assistant"}`)},
	}
	if err := s.AppendRunEvents(ctx, runID, events); err != nil {
		t.Fatal(err)
	}
	// A retried batch must not duplicate.
	if err := s.AppendRunEvents(ctx, runID, events); err != nil {
		t.Fatal(err)
	}
	got, err := s.RunEvents(ctx, runID)
	if err != nil || len(got) != 2 || got[1].Type != "assistant" {
		t.Fatalf("events: %+v %v", got, err)
	}

	turns := 7
	finished, err := s.FinishRun(ctx, runID, RunResult{Status: RunSucceeded, Report: "## Summary\nfine", NumTurns: &turns})
	if err != nil || !finished {
		t.Fatalf("finish: %v %v", finished, err)
	}
	again, err := s.FinishRun(ctx, runID, RunResult{Status: RunFailed})
	if err != nil || again {
		t.Fatalf("second finish should be refused: %v %v", again, err)
	}

	// The token stops working once the run is over.
	if _, err := s.RunByTokenHash(ctx, []byte("hash-1")); !errors.Is(err, ErrNotFound) {
		t.Fatalf("finished run's token still resolves: %v", err)
	}

	final, err := s.GetRun(ctx, runID)
	if err != nil || final.Status != RunSucceeded || final.Report == "" || final.NumTurns == nil || *final.NumTurns != 7 {
		t.Fatalf("final run: %+v %v", final, err)
	}
}

func TestOnlyOneRunAtATime(t *testing.T) {
	s := openTestStore(t)
	ctx := t.Context()
	incident := firingIncident(t, s)
	for range 2 {
		if _, _, err := s.EnqueueRun(ctx, incident, 0); err != nil {
			t.Fatal(err)
		}
	}

	if _, ok, err := s.ClaimNextRun(ctx, []byte("h1"), "j-", "m"); err != nil || !ok {
		t.Fatalf("first claim: %v %v", ok, err)
	}
	if _, ok, err := s.ClaimNextRun(ctx, []byte("h2"), "j-", "m"); err != nil || ok {
		t.Fatalf("second claim must wait for the first run: ok=%v err=%v", ok, err)
	}

	running, ok, err := s.RunningRun(ctx)
	if err != nil || !ok || running.Status != RunRunning {
		t.Fatalf("running run: %+v %v %v", running, ok, err)
	}
}

func TestResolvedIncidentRunIsSkipped(t *testing.T) {
	s := openTestStore(t)
	ctx := t.Context()
	incident := firingIncident(t, s)
	runID, _, err := s.EnqueueRun(ctx, incident, 0)
	if err != nil {
		t.Fatal(err)
	}
	record(t, s, payload("resolved", alert("a", "resolved")))

	if _, ok, err := s.ClaimNextRun(ctx, []byte("h"), "j-", "m"); err != nil || ok {
		t.Fatalf("resolved incident should not be claimed: ok=%v err=%v", ok, err)
	}
	run, err := s.GetRun(ctx, runID)
	if err != nil || run.Status != RunSkipped {
		t.Fatalf("run should be skipped: %+v %v", run, err)
	}
}

func TestRunsStartedSince(t *testing.T) {
	s := openTestStore(t)
	ctx := t.Context()
	incident := firingIncident(t, s)
	if _, _, err := s.EnqueueRun(ctx, incident, 0); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.ClaimNextRun(ctx, []byte("h"), "j-", "m"); err != nil {
		t.Fatal(err)
	}
	n, err := s.RunsStartedSince(ctx, time.Now().Add(-time.Hour))
	if err != nil || n != 1 {
		t.Fatalf("count = %d, err = %v", n, err)
	}
}

func itoa(n int64) string { return strconv.FormatInt(n, 10) }

// Chat turns have their own budget: they never count against the alert
// runs' cap, and the dispatcher can claim them alone when that cap is full.
func TestChatTurnsOutsideAlertCap(t *testing.T) {
	s := openTestStore(t)
	ctx := t.Context()
	_, _, th := openPRThread(t, s) // one diagnosis started
	if _, err := s.EnqueueChat(ctx, uiMessage(th.ID, "why?"), ChatLimits{}); err != nil {
		t.Fatal(err)
	}
	other := payload("firing", alert("z", "firing"))
	other.GroupKey = `{}:{alertname="KubeJobFailed", namespace="ebooks"}`
	diag, _, err := s.EnqueueRun(ctx, record(t, s, other).IncidentID, 0)
	if err != nil {
		t.Fatal(err)
	}

	turn, ok, err := s.ClaimNextChatRun(ctx, []byte("c"), "j-", "m")
	if err != nil || !ok || turn.Kind != KindChat {
		t.Fatalf("chat-only claim: %+v %v %v", turn, ok, err)
	}
	if n, err := s.RunsStartedSince(ctx, time.Now().Add(-time.Hour)); err != nil || n != 1 {
		t.Errorf("alert runs started = %d, %v; want 1 (the chat turn does not count)", n, err)
	}
	if _, err := s.FinishRun(ctx, turn.ID, RunResult{Status: RunSucceeded}); err != nil {
		t.Fatal(err)
	}
	if r, ok, err := s.ClaimNextChatRun(ctx, []byte("c2"), "j-", "m"); err != nil || ok {
		t.Fatalf("chat-only claim took a diagnosis: %+v %v %v", r, ok, err)
	}
	if r, ok, err := s.ClaimNextRun(ctx, []byte("d"), "j-", "m"); err != nil || !ok || r.ID != diag {
		t.Fatalf("claim: %+v %v %v, want diagnosis %d", r, ok, err, diag)
	}
}
