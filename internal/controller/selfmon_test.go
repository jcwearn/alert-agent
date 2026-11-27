package controller

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jcwearn/alert-agent/internal/runnerapi"
	"github.com/jcwearn/alert-agent/internal/store"
)

type recordingBlockNotifier struct {
	mu     sync.Mutex
	blocks []store.ClaudeBlock
	queued []int
	sent   chan struct{}
}

func (n *recordingBlockNotifier) ClaudeBlocked(_ context.Context, b store.ClaudeBlock, queued int, _ string) error {
	n.mu.Lock()
	n.blocks = append(n.blocks, b)
	n.queued = append(n.queued, queued)
	n.mu.Unlock()
	n.sent <- struct{}{}
	return nil
}

func TestAuthFailureDefersAndNotifiesOnce(t *testing.T) {
	fs := runnerFixture()
	fs.queued = map[string]int{store.KindDiagnose: 2}
	notifier := &recordingNotifier{done: make(chan struct{})}
	blocks := &recordingBlockNotifier{sent: make(chan struct{}, 4)}
	s := New(fs, Options{}, &countingWaker{}, notifier)
	s.EnableBlockNotices(blocks)
	now := time.Date(2026, 11, 26, 12, 0, 0, 0, time.UTC)
	s.now = func() time.Time { return now }

	finish := `{"status":"failed","error":"claude: Failed to authenticate.","reason":"auth"}`
	if status, body := runnerReq(t, s.RunnerHandler(), http.MethodPost, runnerapi.PathFinish, "good", finish); status != http.StatusNoContent {
		t.Fatalf("finish status = %d: %s", status, body)
	}
	if res := fs.finished[7]; res.Status != store.RunFailed || res.Reason != store.FailureAuth {
		t.Fatalf("finish = %+v", res)
	}
	if fs.block == nil || fs.block.Reason != store.FailureAuth || !fs.block.Until.Equal(now.Add(authRetry)) {
		t.Fatalf("block = %+v", fs.block)
	}
	if until, ok := fs.deferred[7]; !ok || !until.Equal(now.Add(authRetry)) {
		t.Fatalf("run not deferred to the retry time: %v", fs.deferred)
	}
	select {
	case <-blocks.sent:
	case <-time.After(2 * time.Second):
		t.Fatal("no block notice")
	}
	if blocks.queued[0] != 2 {
		t.Errorf("queued in notice = %d", blocks.queued[0])
	}
	select {
	case <-notifier.done:
		t.Error("a deferred run must not post a diagnosis")
	case <-time.After(100 * time.Millisecond):
	}

	// The successor fails the same way: the block moves, no second notice.
	fs.runs[9] = store.Run{ID: 9, IncidentID: 3, Status: store.RunFailed, FailureReason: store.FailureAuth}
	s.RunFinished(9)
	select {
	case <-blocks.sent:
		t.Error("second notice for the same block")
	case <-time.After(100 * time.Millisecond):
	}

	// A success clears it.
	fs.runs[10] = store.Run{ID: 10, IncidentID: 3, Status: store.RunSucceeded}
	s.RunFinished(10)
	if fs.block != nil {
		t.Errorf("block not cleared by a success: %+v", fs.block)
	}
}

func TestOtherFailureIsNotABlock(t *testing.T) {
	fs := runnerFixture()
	s := New(fs, Options{}, nil, nil)
	if status, _ := runnerReq(t, s.RunnerHandler(), http.MethodPost, runnerapi.PathFinish, "good",
		`{"status":"failed","error":"claude ended with error_max_turns","reason":"bogus"}`); status != http.StatusNoContent {
		t.Fatalf("status = %d", status)
	}
	if res := fs.finished[7]; res.Reason != "" {
		t.Errorf("an unknown reason must not be stored: %+v", res)
	}
	if fs.block != nil || len(fs.deferred) != 0 {
		t.Errorf("block = %+v deferred = %v", fs.block, fs.deferred)
	}
	_, metrics := do(t, s.UIHandler(), http.MethodGet, "/metrics", "")
	for _, want := range []string{
		`alert_agent_run_failures_total{reason="other"} 1`,
		`alert_agent_run_failures_total{reason="auth"} 0`,
		`alert_agent_claude_blocked{reason="usage_limit"} 0`,
		`alert_agent_oldest_queued_run_age_seconds 0`,
	} {
		if !strings.Contains(metrics, want) {
			t.Errorf("metrics missing %q", want)
		}
	}
	if strings.Contains(metrics, "alert_agent_claude_token_created_timestamp_seconds") {
		t.Error("token gauge registered without WatchTokenDate")
	}
}

func TestChatTurnBlockIsNotDeferred(t *testing.T) {
	fs := runnerFixture()
	s := New(fs, Options{}, nil, nil)
	fs.runs[11] = store.Run{ID: 11, Kind: store.KindChat, Status: store.RunFailed, FailureReason: store.FailureUsageLimit}
	s.claudeBlocked(t.Context(), fs.runs[11])
	if fs.block == nil || fs.block.Reason != store.FailureUsageLimit {
		t.Fatalf("block = %+v", fs.block)
	}
	if len(fs.deferred) != 0 {
		t.Errorf("chat turn deferred: %v", fs.deferred)
	}
}

func TestWatchTokenDate(t *testing.T) {
	s := New(runnerFixture(), Options{}, nil, nil)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	created, _ := ParseTokenDate("2026-11-24")
	s.WatchTokenDate(ctx, func(context.Context) (time.Time, error) { return created, nil }, time.Hour)
	_, metrics := do(t, s.UIHandler(), http.MethodGet, "/metrics", "")
	if !strings.Contains(metrics, "alert_agent_claude_token_created_timestamp_seconds 1.7954784e+09") {
		t.Errorf("token gauge wrong:\n%s", grepLines(metrics, "token_created"))
	}

	s2 := New(runnerFixture(), Options{}, nil, nil)
	s2.WatchTokenDate(ctx, func(context.Context) (time.Time, error) { return time.Time{}, errors.New("no annotation") }, time.Hour)
	_, metrics = do(t, s2.UIHandler(), http.MethodGet, "/metrics", "")
	if !strings.Contains(metrics, "alert_agent_claude_token_created_timestamp_seconds 0") {
		t.Errorf("unknown date should read 0:\n%s", grepLines(metrics, "token_created"))
	}
}

func TestParseTokenDate(t *testing.T) {
	if d, err := ParseTokenDate(" 2026-11-24 "); err != nil || d.Unix() != 1795478400 {
		t.Errorf("date: %v %v", d, err)
	}
	if d, err := ParseTokenDate("2026-11-24T15:00:00Z"); err != nil || d.Hour() != 15 {
		t.Errorf("rfc3339: %v %v", d, err)
	}
	if _, err := ParseTokenDate("next tuesday"); err == nil {
		t.Error("garbage parsed")
	}
}

func TestBlockMessages(t *testing.T) {
	b := store.ClaudeBlock{Reason: store.FailureAuth, Until: time.Date(2026, 11, 26, 12, 30, 0, 0, time.UTC)}
	m := blockNtfyMessage("medic", b, 3, "https://medic.example/runbook")
	if !strings.Contains(m.Title, "token") || !strings.Contains(m.Message, "3 run(s)") || m.Click != "https://medic.example/runbook" || m.Priority != 4 {
		t.Errorf("auth notice = %+v", m)
	}
	b.Reason = store.FailureUsageLimit
	m = blockNtfyMessage("medic", b, 1, "")
	if !strings.Contains(m.Title, "usage limit") || m.Click != "" || len(m.Actions) != 0 {
		t.Errorf("usage notice = %+v", m)
	}
}
