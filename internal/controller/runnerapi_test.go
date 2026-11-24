package controller

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jcwearn/alert-agent/internal/dispatch"
	"github.com/jcwearn/alert-agent/internal/runnerapi"
	"github.com/jcwearn/alert-agent/internal/store"
)

type countingWaker struct {
	mu sync.Mutex
	n  int
}

func (w *countingWaker) Wake() { w.mu.Lock(); w.n++; w.mu.Unlock() }

type recordingNotifier struct {
	mu   sync.Mutex
	runs []store.Run
	done chan struct{}
}

func (n *recordingNotifier) RunFinished(_ context.Context, _ store.Incident, run store.Run, _ *store.PullRequest) {
	n.mu.Lock()
	n.runs = append(n.runs, run)
	n.mu.Unlock()
	close(n.done)
}

func runnerFixture() *fakeStore {
	opened := time.Date(2026, 11, 24, 10, 0, 0, 0, time.UTC)
	return &fakeStore{
		incidents: []store.Incident{{ID: 3, AlertName: "KubePodCrashLooping", Namespace: "ebooks", AlertStatus: "firing", OpenedAt: opened}},
		alerts:    []store.IncidentAlert{{Fingerprint: "f1", Status: "firing", Labels: map[string]string{"pod": "a"}, StartsAt: opened}},
		runs: map[int64]store.Run{
			7: {ID: 7, IncidentID: 3, Status: store.RunRunning, SessionID: "11111111-2222-3333-4444-555555555555", Model: "opus"},
			8: {ID: 8, IncidentID: 3, Status: store.RunSucceeded},
		},
		tokenHash: map[string]int64{
			string(dispatch.HashToken("good")):     7,
			string(dispatch.HashToken("finished")): 8,
		},
	}
}

func runnerReq(t *testing.T, h http.Handler, method, path, token, body string) (status int, respBody string) {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	req, err := http.NewRequestWithContext(t.Context(), method, srv.URL+path, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode, string(b)
}

func TestRunnerAPIRejectsBadTokens(t *testing.T) {
	s := New(runnerFixture(), Options{}, nil, nil)
	for name, token := range map[string]string{"none": "", "unknown": "nope", "finished run": "finished"} {
		t.Run(name, func(t *testing.T) {
			if status, _ := runnerReq(t, s.RunnerHandler(), http.MethodGet, runnerapi.PathContext, token, ""); status != http.StatusUnauthorized {
				t.Errorf("status = %d, want 401", status)
			}
		})
	}
}

func TestRunnerContext(t *testing.T) {
	s := New(runnerFixture(), Options{MaxTurns: 40, PublicURL: "https://alert-agent.example"}, nil, nil)
	status, body := runnerReq(t, s.RunnerHandler(), http.MethodGet, runnerapi.PathContext, "good", "")
	if status != http.StatusOK {
		t.Fatalf("status = %d: %s", status, body)
	}
	var rc runnerapi.Context
	if err := json.Unmarshal([]byte(body), &rc); err != nil {
		t.Fatal(err)
	}
	if rc.RunID != 7 || rc.Model != "opus" || rc.MaxTurns != 40 || rc.Incident.AlertName != "KubePodCrashLooping" || len(rc.Alerts) != 1 {
		t.Errorf("context wrong: %+v", rc)
	}
	if rc.UIURL != "https://alert-agent.example/incidents/3" {
		t.Errorf("ui url = %q", rc.UIURL)
	}
}

func TestRunnerEventsAndFinish(t *testing.T) {
	fs := runnerFixture()
	waker := &countingWaker{}
	notifier := &recordingNotifier{done: make(chan struct{})}
	s := New(fs, Options{}, waker, notifier)
	h := s.RunnerHandler()

	events := `[{"seq":0,"type":"system","payload":{"type":"system"}},{"seq":1,"type":"assistant","payload":{"type":"assistant"}}]`
	if status, body := runnerReq(t, h, http.MethodPost, runnerapi.PathEvents, "good", events); status != http.StatusNoContent {
		t.Fatalf("events status = %d: %s", status, body)
	}
	if len(fs.events[7]) != 2 {
		t.Fatalf("events not stored: %+v", fs.events)
	}

	if status, _ := runnerReq(t, h, http.MethodPost, runnerapi.PathFinish, "good", `{"status":"maybe"}`); status != http.StatusBadRequest {
		t.Errorf("bad status accepted: %d", status)
	}

	finish := `{"status":"succeeded","report":"## Summary\nOOMKilled","num_turns":4}`
	if status, body := runnerReq(t, h, http.MethodPost, runnerapi.PathFinish, "good", finish); status != http.StatusNoContent {
		t.Fatalf("finish status = %d: %s", status, body)
	}
	if res := fs.finished[7]; res.Status != store.RunSucceeded || res.NumTurns == nil || *res.NumTurns != 4 {
		t.Errorf("finish not recorded: %+v", res)
	}

	select {
	case <-notifier.done:
	case <-time.After(2 * time.Second):
		t.Fatal("notifier not called")
	}
	if waker.n == 0 {
		t.Error("dispatcher not woken after finish")
	}

	// The token is dead once the run is finished.
	if status, _ := runnerReq(t, h, http.MethodGet, runnerapi.PathContext, "good", ""); status != http.StatusUnauthorized {
		t.Errorf("finished run's token still accepted: %d", status)
	}
}

func TestWebhookQueuesRunOnlyWhenEnabled(t *testing.T) {
	for _, enabled := range []bool{true, false} {
		fs := &fakeStore{result: store.RecordResult{IncidentID: 7, Created: true}}
		waker := &countingWaker{}
		s := New(fs, Options{AgentEnabled: enabled}, waker, nil)
		if status, _ := do(t, s.WebhookHandler(), http.MethodPost, "/webhook/alertmanager", validPayload); status != http.StatusNoContent {
			t.Fatalf("status = %d", status)
		}
		if got := len(fs.enqueued) == 1; got != enabled {
			t.Errorf("enabled=%v: enqueued %v", enabled, fs.enqueued)
		}
		if enabled && waker.n == 0 {
			t.Error("dispatcher not woken")
		}
	}
}

func TestWebhookDoesNotRequeueExistingIncident(t *testing.T) {
	fs := &fakeStore{result: store.RecordResult{IncidentID: 7, Created: false}}
	s := New(fs, Options{AgentEnabled: true}, nil, nil)
	do(t, s.WebhookHandler(), http.MethodPost, "/webhook/alertmanager", validPayload)
	if len(fs.enqueued) != 0 {
		t.Error("a repeat delivery queued another run")
	}
}

const sampleReport = `## Summary
Calibre is **OOMKilled**: its 512Mi limit is below its working set.
Second summary line.

## Evidence
- kube_get shows restarts 7
- <!channel> & <http://evil|click>

## Cause
Memory limit too low.

## Recommended fix
pr: raise the limit in ` + "`apps/ebooks/deployment.yaml`" + ` to 1Gi. See [the docs](https://example.com/docs).

## Confidence
high, the OOMKilled reason is explicit.
`

func blockTexts(p slackPayload) string {
	var b strings.Builder
	for _, bl := range p.Blocks {
		if bl.Text != nil {
			b.WriteString(bl.Text.Text + "\n")
		}
		for _, e := range bl.Elements {
			b.WriteString(e.Text + "\n")
		}
	}
	return b.String()
}

func TestSlackMessageIsConcise(t *testing.T) {
	in := store.Incident{ID: 3, AlertName: "KubePodCrashLooping", Namespace: "ebooks"}
	msg := slackMessage("https://ui.example/", in, store.Run{ID: 7, Status: store.RunSucceeded, Report: sampleReport}, nil)
	all := blockTexts(msg)

	for _, want := range []string{
		"<https://ui.example/runs/7|incident #3>",
		"*Summary*\nCalibre is *OOMKilled*",
		"*Recommended fix*\npr: raise the limit",
		"<https://example.com/docs|the docs>",
		"*Confidence*\nhigh",
		"<https://ui.example/runs/7|Full report and transcript>",
	} {
		if !strings.Contains(all, want) {
			t.Errorf("message lacks %q:\n%s", want, all)
		}
	}
	for _, unwanted := range []string{"## ", "Evidence", "Memory limit too low", "<!channel>", "<http://evil"} {
		if strings.Contains(all, unwanted) {
			t.Errorf("message contains %q:\n%s", unwanted, all)
		}
	}
	if msg.Text != "Diagnosis: KubePodCrashLooping in ebooks: Calibre is OOMKilled: its 512Mi limit is below its working set." {
		t.Errorf("preview text = %q", msg.Text)
	}
}

func TestSlackMessageWithoutSectionsFallsBack(t *testing.T) {
	in := store.Incident{ID: 3, AlertName: "X"}
	msg := slackMessage("https://ui.example", in, store.Run{ID: 7, Status: store.RunSucceeded, Report: "No headings here, <!here>."}, nil)
	if all := blockTexts(msg); !strings.Contains(all, "No headings here") || strings.Contains(all, "<!here>") {
		t.Errorf("fallback wrong:\n%s", all)
	}
}

func TestSlackMessageForFailure(t *testing.T) {
	in := store.Incident{ID: 3, AlertName: "X", Namespace: "ebooks"}
	msg := slackMessage("https://ui.example", in, store.Run{ID: 7, Status: store.RunFailed, Error: "claude produced no result <boom>"}, nil)
	all := blockTexts(msg)
	if !strings.Contains(all, "*Agent run failed:*") || !strings.Contains(all, "```claude produced no result &lt;boom&gt;```") {
		t.Errorf("failure message wrong:\n%s", all)
	}
	if msg.Text != "Agent run failed: X in ebooks" {
		t.Errorf("preview = %q", msg.Text)
	}
}

func TestSlackSectionsStayUnderLimit(t *testing.T) {
	long := "## Summary\n" + strings.Repeat("é", 5000)
	msg := slackMessage("https://ui.example", store.Incident{ID: 1, AlertName: "X"}, store.Run{ID: 1, Status: store.RunSucceeded, Report: long}, nil)
	for _, bl := range msg.Blocks {
		if bl.Text != nil && len([]rune(bl.Text.Text)) > sectionLimit {
			t.Errorf("section has %d runes, limit %d", len([]rune(bl.Text.Text)), sectionLimit)
		}
	}
}

func TestRunPageRendersTranscript(t *testing.T) {
	fs := runnerFixture()
	fs.events = map[int64][]store.RunEvent{7: {
		{Seq: 0, Type: "assistant", Payload: json.RawMessage(`{"type":"assistant","message":{"content":[{"type":"tool_use","name":"Bash","input":{"command":"kubectl get pods -n ebooks"}}]}}`)},
		{Seq: 1, Type: "user", Payload: json.RawMessage(`{"type":"user","message":{"content":[{"type":"tool_result","content":"calibre-0 CrashLoopBackOff"}]}}`)},
	}}
	s := New(fs, Options{}, nil, nil)
	status, body := do(t, s.UIHandler(), http.MethodGet, "/runs/7", "")
	if status != http.StatusOK {
		t.Fatalf("status = %d", status)
	}
	for _, want := range []string{"$ kubectl get pods -n ebooks", "calibre-0 CrashLoopBackOff", `href="/incidents/3"`} {
		if !strings.Contains(body, want) {
			t.Errorf("run page missing %q", want)
		}
	}
	if status, _ := do(t, s.UIHandler(), http.MethodGet, "/runs/99", ""); status != http.StatusNotFound {
		t.Errorf("missing run status = %d", status)
	}
}
