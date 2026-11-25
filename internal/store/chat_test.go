package store

import (
	"errors"
	"strings"
	"testing"
	"time"
)

// openPR finishes a run that opened a PR, with a stored session, and
// resolves its incident: PRs are usually reviewed after the alert cleared.
func openPR(t *testing.T, s *Store) (PullRequest, Run) {
	t.Helper()
	ctx := t.Context()
	run := runningRun(t, s)
	pr := PullRequest{IncidentID: run.IncidentID, RunID: run.ID, Number: 7, URL: "u", Title: "fix(x): y",
		Branch: "agent/1", BaseSHA: "base", HeadSHA: "head1", Files: []string{"apps/x/a.yaml"}}
	id, err := s.InsertPullRequest(ctx, pr)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SaveRunSession(ctx, run.ID, []byte("gz-1"), 4); err != nil {
		t.Fatal(err)
	}
	if _, err := s.FinishRun(ctx, run.ID, RunResult{Status: RunSucceeded}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.pool.Exec(ctx, `UPDATE incidents SET alert_status = 'resolved' WHERE id = $1`, run.IncidentID); err != nil {
		t.Fatal(err)
	}
	p, err := s.GetPullRequest(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	return p, run
}

func uiMessage(prID int64, body string) ChatMessage {
	return ChatMessage{PRID: prID, Source: SourceUI, Author: "ui", Body: body}
}

func TestChatTurnLifecycle(t *testing.T) {
	s := openTestStore(t)
	ctx := t.Context()
	pr, prRun := openPR(t, s)
	lim := ChatLimits{PerPRHour: 4, AllHour: 100}

	// A diagnosis queued before the chat turn, for another alert.
	other := payload("firing", alert("z", "firing"))
	other.GroupKey = `{}:{alertname="KubeJobFailed", namespace="ebooks"}`
	other.CommonLabels = map[string]string{"alertname": "KubeJobFailed", "namespace": "ebooks"}
	diag, _, err := s.EnqueueRun(ctx, record(t, s, other).IncidentID, 0)
	if err != nil {
		t.Fatal(err)
	}

	first, err := s.EnqueueChat(ctx, uiMessage(pr.ID, "why this limit?"), lim)
	if err != nil || !first.NewRun || first.Capped != "" {
		t.Fatalf("first enqueue: %+v %v", first, err)
	}
	// A second message while the turn waits joins it.
	second, err := s.EnqueueChat(ctx, uiMessage(pr.ID, "and make it 512Mi"), lim)
	if err != nil || second.NewRun || second.RunID != first.RunID {
		t.Fatalf("second enqueue: %+v %v", second, err)
	}

	// The incident resolved, but the chat turn is still claimed, before the
	// older diagnosis, and keeps its incident and the PR's session.
	if diag > first.RunID {
		t.Fatalf("diagnosis run %d should predate chat run %d", diag, first.RunID)
	}
	run, ok, err := s.ClaimNextRun(ctx, []byte("chat-hash"), "alert-agent-run-", "opus")
	if err != nil || !ok {
		t.Fatalf("claim: %v %v", ok, err)
	}
	if run.ID != first.RunID || run.Kind != KindChat || run.PRID == nil || *run.PRID != pr.ID ||
		run.IncidentID != pr.IncidentID || run.SessionID != prRun.SessionID {
		t.Fatalf("claimed %+v, want chat run %d on PR %d with session %s", run, first.RunID, pr.ID, prRun.SessionID)
	}

	msgs, err := s.TurnMessages(ctx, run.ID)
	if err != nil || len(msgs) != 2 || msgs[0].Body != "why this limit?" {
		t.Fatalf("turn messages: %+v %v", msgs, err)
	}
	// A message during the running turn queues the next one.
	third, err := s.EnqueueChat(ctx, uiMessage(pr.ID, "thanks"), lim)
	if err != nil || !third.NewRun || third.RunID == run.ID {
		t.Fatalf("third enqueue: %+v %v", third, err)
	}
	if active, ok, err := s.ActiveChatRun(ctx, pr.ID); err != nil || !ok || active.ID != run.ID {
		t.Fatalf("active chat run: %+v %v %v", active, ok, err)
	}

	gz, err := s.LatestSession(ctx, run.SessionID, run.ID)
	if err != nil || string(gz) != "gz-1" {
		t.Fatalf("session: %q %v", gz, err)
	}
	if err := s.SaveRunSession(ctx, run.ID, []byte("gz-2"), 4); err != nil {
		t.Fatal(err)
	}
	if gz, _ := s.LatestSession(ctx, run.SessionID, third.RunID); string(gz) != "gz-2" {
		t.Errorf("next turn resumes %q, want the chat turn's session", gz)
	}

	reply, firstFinish, err := s.FinishChatTurn(ctx, run.ID, true, "Because…")
	if err != nil || !firstFinish || reply.Source != SourceAgent || reply.Status != ChatReply {
		t.Fatalf("finish: %+v %v %v", reply, firstFinish, err)
	}
	if _, again, err := s.FinishChatTurn(ctx, run.ID, true, "dup"); err != nil || again {
		t.Fatalf("second finish: %v %v", again, err)
	}
	all, err := s.ChatMessages(ctx, pr.ID)
	if err != nil || len(all) != 4 {
		t.Fatalf("conversation: %+v %v", all, err)
	}
	if all[0].Status != ChatAnswered || all[1].Status != ChatAnswered || all[2].Status != ChatQueued || all[3].Body != "Because…" {
		t.Errorf("statuses: %s %s %s, reply %q", all[0].Status, all[1].Status, all[2].Status, all[3].Body)
	}

	moved, err := s.UpdatePullRequestHead(ctx, pr.ID, "head1", "head2", []string{"apps/x/a.yaml", "apps/x/b.yaml"})
	if err != nil || !moved {
		t.Fatalf("head update: %v %v", moved, err)
	}
	if stale, _ := s.UpdatePullRequestHead(ctx, pr.ID, "head1", "head3", nil); stale {
		t.Error("head update from a stale sha succeeded")
	}
	if p, _ := s.GetPullRequest(ctx, pr.ID); p.HeadSHA != "head2" || len(p.Files) != 2 || p.Checks != ChecksPending {
		t.Errorf("PR after update: %+v", p)
	}
}

func TestChatCapsAndClosedPR(t *testing.T) {
	s := openTestStore(t)
	ctx := t.Context()
	pr, _ := openPR(t, s)
	lim := ChatLimits{PerPRHour: 1, AllHour: 100}

	first, err := s.EnqueueChat(ctx, uiMessage(pr.ID, "one"), lim)
	if err != nil || !first.NewRun {
		t.Fatalf("first: %+v %v", first, err)
	}
	turn, ok, err := s.ClaimNextRun(ctx, []byte("h"), "alert-agent-run-", "opus")
	if err != nil || !ok {
		t.Fatalf("claim: %v %v", ok, err)
	}
	capped, err := s.EnqueueChat(ctx, uiMessage(pr.ID, "two"), lim)
	if err != nil || capped.Capped == "" || capped.NewRun || capped.RetryAt.IsZero() {
		t.Fatalf("per-PR cap: %+v %v", capped, err)
	}

	// The all-PR chat cap counts chat turns only: one so far. The diagnosis
	// that opened the PR does not count.
	global, err := s.EnqueueChat(ctx, uiMessage(pr.ID, "three"), ChatLimits{AllHour: 1})
	if err != nil || !strings.Contains(global.Capped, "across all PRs") {
		t.Fatalf("all-PR cap of 1 with one chat turn this hour: %+v %v", global, err)
	}
	if r, err := s.EnqueueChat(ctx, uiMessage(pr.ID, "four"), ChatLimits{AllHour: 2}); err != nil || r.Capped != "" || !r.NewRun {
		t.Fatalf("one chat turn is under an all-PR cap of 2: %+v %v", r, err)
	}

	// GitHub comments dedupe on their id. Real ids no longer fit in 32 bits.
	gh := ChatMessage{PRID: pr.ID, Source: SourceGitHub, Author: "me", Body: "hi", GHKind: GHReviewComment, GHID: 7171034603, GHThreadID: 4699221870}
	if r, err := s.EnqueueChat(ctx, gh, ChatLimits{}); err != nil || r.Duplicate {
		t.Fatalf("first comment: %+v %v", r, err)
	}
	if r, err := s.EnqueueChat(ctx, gh, ChatLimits{}); err != nil || !r.Duplicate {
		t.Fatalf("repeat comment: %+v %v", r, err)
	}

	// A waiting turn on a PR that closed is skipped, and its messages fail.
	if err := s.UpdatePullRequest(ctx, pr.ID, PRStatus{State: PRClosed, Checks: ChecksSuccess, HeadSHA: "head1"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.FinishRun(ctx, turn.ID, RunResult{Status: RunSucceeded, Report: "done"}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.FinishChatTurn(ctx, turn.ID, true, "done"); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := s.ClaimNextRun(ctx, []byte("h2"), "alert-agent-run-", "opus"); err != nil || ok {
		t.Fatalf("claimed a chat turn on a closed PR: %v %v", ok, err)
	}
	if _, err := s.EnqueueChat(ctx, uiMessage(pr.ID, "late"), lim); !errors.Is(err, ErrPRNotOpen) {
		t.Errorf("message on a closed PR: %v", err)
	}
	all, _ := s.ChatMessages(ctx, pr.ID)
	found := false
	for _, m := range all {
		found = found || (m.GHID == 7171034603 && m.GHThreadID == 4699221870)
	}
	if !found {
		t.Error("a 64-bit GitHub comment id did not round-trip")
	}
	for _, m := range all {
		if m.Status == ChatQueued {
			t.Errorf("message %q still queued after its PR closed", m.Body)
		}
	}
}

func TestCommentCursorAndReplyURL(t *testing.T) {
	s := openTestStore(t)
	ctx := t.Context()
	pr, _ := openPR(t, s)
	if pr.CommentsSince.IsZero() {
		t.Fatal("comments_since not initialised")
	}
	later := pr.CommentsSince.Add(time.Hour)
	if err := s.AdvanceCommentsSince(ctx, pr.ID, later); err != nil {
		t.Fatal(err)
	}
	if err := s.AdvanceCommentsSince(ctx, pr.ID, pr.CommentsSince); err != nil {
		t.Fatal(err)
	}
	if p, _ := s.GetPullRequest(ctx, pr.ID); !p.CommentsSince.Equal(later) {
		t.Errorf("cursor = %v, want %v (it must never move back)", p.CommentsSince, later)
	}

	res, err := s.EnqueueChat(ctx, ChatMessage{PRID: pr.ID, Source: SourceGitHub, Author: "me", Body: "hi", GHKind: GHIssueComment, GHID: 5}, ChatLimits{})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.MarkChatReacted(ctx, res.MessageID); err != nil {
		t.Fatal(err)
	}
	if err := s.SetChatMessageURL(ctx, res.MessageID, "https://example/c/1"); err != nil {
		t.Fatal(err)
	}
	all, _ := s.ChatMessages(ctx, pr.ID)
	if len(all) != 1 || all[0].ReactedAt == nil || all[0].HTMLURL != "https://example/c/1" || all[0].GHID != 5 || all[0].GHKind != GHIssueComment {
		t.Errorf("message = %+v", all)
	}
}
