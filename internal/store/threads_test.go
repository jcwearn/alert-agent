package store

import (
	"errors"
	"testing"
)

// An incident thread continues the agent's own session: the PR run's, else
// the latest diagnosis that stored one, else a new one.
func TestIncidentThreadSession(t *testing.T) {
	s := openTestStore(t)
	ctx := t.Context()

	// Diagnosed, no PR: the latest run with a stored session.
	run := runningRun(t, s)
	if err := s.SaveRunSession(ctx, run.ID, []byte("gz"), 2); err != nil {
		t.Fatal(err)
	}
	if _, err := s.FinishRun(ctx, run.ID, RunResult{Status: RunSucceeded}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.IncidentThread(ctx, run.IncidentID, false); !errors.Is(err, ErrNotFound) {
		t.Fatalf("lookup without create: %v", err)
	}
	th, existed, err := s.IncidentThread(ctx, run.IncidentID, true)
	if err != nil || existed || th.SessionID != run.SessionID || th.AlertName == "" {
		t.Fatalf("thread %+v (existed %v, %v), want session %s", th, existed, err, run.SessionID)
	}
	again, existed, err := s.IncidentThread(ctx, run.IncidentID, true)
	if err != nil || !existed || again.ID != th.ID {
		t.Fatalf("second lookup made another thread: %+v %v", again, err)
	}
	if ok, _ := s.HasStoredSession(ctx, th.SessionID, run.ID+100); !ok {
		t.Error("the diagnosis session should be resumable")
	}

	// No stored session at all: a fresh one.
	other := payload("firing", alert("q", "firing"))
	other.GroupKey = `{}:{alertname="Other", namespace="ebooks"}`
	inc := record(t, s, other).IncidentID
	fresh, _, err := s.IncidentThread(ctx, inc, true)
	if err != nil || fresh.SessionID == "" || fresh.SessionID == run.SessionID {
		t.Fatalf("fresh thread: %+v %v", fresh, err)
	}
	if ok, _ := s.HasStoredSession(ctx, fresh.SessionID, 1<<40); ok {
		t.Error("a fresh session has nothing stored")
	}
}

// A general thread has no incident: its turns are chat runs without one,
// it is named after its first message, and it is listed while empty.
func TestGeneralThread(t *testing.T) {
	s := openTestStore(t)
	ctx := t.Context()
	g, err := s.CreateGeneralThread(ctx)
	if err != nil || g.Kind != ThreadGeneral || g.IncidentID != nil || g.Title != "" {
		t.Fatalf("general thread: %+v %v", g, err)
	}
	if list, _ := s.RecentThreads(ctx, 10); len(list) != 1 || list[0].ID != g.ID {
		t.Fatalf("an empty general thread should be listed: %+v", list)
	}
	res, err := s.EnqueueChat(ctx, ChatMessage{ThreadID: g.ID, Source: SourceUI, Author: "you", Body: "How is the cluster doing today?", Page: "/"},
		ChatLimits{})
	if err != nil || !res.NewRun {
		t.Fatalf("enqueue: %+v %v", res, err)
	}
	run, ok, err := s.ClaimNextRun(ctx, []byte("g"), "j-", "m")
	if err != nil || !ok || run.IncidentID != 0 || run.ThreadID == nil || *run.ThreadID != g.ID || run.SessionID != g.SessionID || run.PRID != nil {
		t.Fatalf("general turn: %+v %v %v", run, ok, err)
	}
	if got, _ := s.GetThread(ctx, g.ID); got.Title != "How is the cluster doing today?" || got.Messages != 1 {
		t.Errorf("thread after first message: %+v", got)
	}
	if _, err := s.FinishRun(ctx, run.ID, RunResult{Status: RunSucceeded, Report: "All good."}); err != nil {
		t.Fatal(err)
	}
	reply, first, err := s.FinishChatTurn(ctx, run.ID, true, "All good.")
	if err != nil || !first || reply.ThreadID != g.ID || reply.PRID != 0 {
		t.Fatalf("reply: %+v %v %v", reply, first, err)
	}
	// An incident thread with no messages is not listed.
	pr, _ := openPR(t, s)
	if _, _, err := s.IncidentThread(ctx, pr.IncidentID, true); err != nil {
		t.Fatal(err)
	}
	if list, _ := s.RecentThreads(ctx, 10); len(list) != 1 {
		t.Errorf("an incident thread with no messages was listed: %+v", list)
	}
}

// Migration 0006 turns each Phase 4 PR conversation into its incident's
// thread, with its chat runs and messages.
func TestThreadBackfill(t *testing.T) {
	s := openTestStoreThrough(t, "0005")
	ctx := t.Context()
	inc := record(t, s, payload("firing", alert("a", "firing"))).IncidentID
	var prRun, prID, chatRun int64
	var session string
	if err := s.pool.QueryRow(ctx, `INSERT INTO runs (incident_id, status) VALUES ($1, 'succeeded') RETURNING id, session_id::text`, inc).
		Scan(&prRun, &session); err != nil {
		t.Fatal(err)
	}
	if err := s.pool.QueryRow(ctx, `INSERT INTO pull_requests (incident_id, run_id, number, url, title, branch, base_sha, head_sha)
		VALUES ($1, $2, 7, 'u', 't', 'agent/1', 'b', 'h') RETURNING id`, inc, prRun).Scan(&prID); err != nil {
		t.Fatal(err)
	}
	if err := s.pool.QueryRow(ctx, `INSERT INTO runs (incident_id, status, kind, pr_id, session_id)
		VALUES ($1, 'succeeded', 'chat', $2, $3::uuid) RETURNING id`, inc, prID, session).Scan(&chatRun); err != nil {
		t.Fatal(err)
	}
	if _, err := s.pool.Exec(ctx, `INSERT INTO chat_messages (pr_id, run_id, source, author, body, status)
		VALUES ($1, $2, 'ui', 'you', 'why?', 'answered'), ($1, $2, 'agent', 'agent', 'because', 'reply')`, prID, chatRun); err != nil {
		t.Fatal(err)
	}

	if err := s.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	th, existed, err := s.IncidentThread(ctx, inc, false)
	if err != nil || !existed || th.SessionID != session {
		t.Fatalf("backfilled thread: %+v %v %v", th, existed, err)
	}
	msgs, err := s.ChatMessages(ctx, th.ID)
	if err != nil || len(msgs) != 2 || msgs[0].PRID != prID {
		t.Fatalf("backfilled messages: %+v %v", msgs, err)
	}
	run, err := s.GetRun(ctx, chatRun)
	if err != nil || run.ThreadID == nil || *run.ThreadID != th.ID {
		t.Fatalf("backfilled chat run: %+v %v", run, err)
	}
}
