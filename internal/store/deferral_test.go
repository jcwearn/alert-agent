package store

import (
	"testing"
	"time"
)

func TestDeferRunQueuesAHeldSuccessor(t *testing.T) {
	s := openTestStore(t)
	ctx := t.Context()
	incident := firingIncident(t, s)

	runID, _, err := s.EnqueueRun(ctx, incident, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok, err := s.ClaimNextRun(ctx, []byte("h1"), "alert-agent-run-", "opus"); err != nil || !ok {
		t.Fatalf("claim: %v %v", ok, err)
	}
	if _, err := s.FinishRun(ctx, runID, RunResult{Status: RunFailed, Error: "claude: 401", Reason: FailureAuth}); err != nil {
		t.Fatal(err)
	}
	failed, err := s.GetRun(ctx, runID)
	if err != nil {
		t.Fatal(err)
	}
	var reason string
	if err := s.pool.QueryRow(ctx, `SELECT failure_reason FROM runs WHERE id = $1`, runID).Scan(&reason); err != nil || reason != FailureAuth {
		t.Fatalf("failure_reason = %q, %v", reason, err)
	}

	until := time.Now().Add(30 * time.Minute)
	next, ok, err := s.DeferRun(ctx, runID, until)
	if err != nil || !ok || next == runID {
		t.Fatalf("defer: id=%d ok=%v err=%v", next, ok, err)
	}
	successor, err := s.GetRun(ctx, next)
	if err != nil || successor.Status != RunQueued || successor.IncidentID != incident || successor.SessionID == failed.SessionID {
		t.Fatalf("successor = %+v, %v", successor, err)
	}
	members, err := s.RunIncidents(ctx, next)
	if err != nil || len(members) != 1 || members[0] != incident {
		t.Fatalf("members = %v, %v", members, err)
	}
	// Held: nothing to claim yet.
	if _, ok, err := s.ClaimNextRun(ctx, []byte("h2"), "alert-agent-run-", "opus"); err != nil || ok {
		t.Fatalf("held run claimed: %v %v", ok, err)
	}
	depth, oldest, err := s.QueueDepth(ctx)
	if err != nil || depth[KindDiagnose] != 1 || depth[KindChat] != 0 || oldest < 0 || oldest > time.Minute {
		t.Fatalf("depth = %v oldest = %v err = %v", depth, oldest, err)
	}
}

func TestDeferRunSkipsResolvedIncidents(t *testing.T) {
	s := openTestStore(t)
	ctx := t.Context()
	incident := firingIncident(t, s)
	runID, _, err := s.EnqueueRun(ctx, incident, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.ClaimNextRun(ctx, []byte("h1"), "alert-agent-run-", "opus"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.FinishRun(ctx, runID, RunResult{Status: RunFailed, Reason: FailureUsageLimit}); err != nil {
		t.Fatal(err)
	}
	record(t, s, payload("resolved", alert("a", "resolved")))
	if _, ok, err := s.DeferRun(ctx, runID, time.Now().Add(time.Hour)); err != nil || ok {
		t.Fatalf("a resolved incident needs no deferred run: ok=%v err=%v", ok, err)
	}
	depth, oldest, err := s.QueueDepth(ctx)
	if err != nil || depth[KindDiagnose] != 0 || oldest != 0 {
		t.Fatalf("depth = %v oldest = %v err = %v", depth, oldest, err)
	}
}

func TestClaudeBlock(t *testing.T) {
	s := openTestStore(t)
	ctx := t.Context()

	if _, ok, err := s.GetClaudeBlock(ctx); err != nil || ok {
		t.Fatalf("no block expected: %v %v", ok, err)
	}
	first := time.Now().Add(time.Hour).Truncate(time.Second)
	started, err := s.SetClaudeBlock(ctx, FailureUsageLimit, first)
	if err != nil || !started {
		t.Fatalf("first block: started=%v err=%v", started, err)
	}
	b, ok, err := s.GetClaudeBlock(ctx)
	if err != nil || !ok || b.Reason != FailureUsageLimit || !b.Until.Equal(first) || b.Since.IsZero() {
		t.Fatalf("block = %+v ok=%v err=%v", b, ok, err)
	}

	// The same reason again only moves the retry time: no second notice.
	later := first.Add(time.Hour)
	started, err = s.SetClaudeBlock(ctx, FailureUsageLimit, later)
	if err != nil || started {
		t.Fatalf("repeat block: started=%v err=%v", started, err)
	}
	b2, _, err := s.GetClaudeBlock(ctx)
	if err != nil || !b2.Until.Equal(later) || !b2.Since.Equal(b.Since) {
		t.Fatalf("repeat kept since? %+v vs %+v (%v)", b2, b, err)
	}

	// A different reason is news.
	if started, err = s.SetClaudeBlock(ctx, FailureAuth, later); err != nil || !started {
		t.Fatalf("auth after usage limit: started=%v err=%v", started, err)
	}

	cleared, err := s.ClearClaudeBlock(ctx)
	if err != nil || !cleared {
		t.Fatalf("clear: %v %v", cleared, err)
	}
	if cleared, err = s.ClearClaudeBlock(ctx); err != nil || cleared {
		t.Fatalf("second clear: %v %v", cleared, err)
	}
}
