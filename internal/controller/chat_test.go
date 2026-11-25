package controller

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/jcwearn/alert-agent/internal/dispatch"
	"github.com/jcwearn/alert-agent/internal/proposal"
	"github.com/jcwearn/alert-agent/internal/runnerapi"
	"github.com/jcwearn/alert-agent/internal/store"
)

// chatState is the fakeStore's chat tables.
type chatState struct {
	messages   []store.ChatMessage
	enqueued   []store.ChatMessage
	result     store.ChatEnqueue
	sessions   map[string][]byte
	activeChat *store.Run
	failID     int64 // EnqueueChat fails for this GitHub comment id
	threads    []store.Thread
}

func (f *fakeStore) GetThread(_ context.Context, id int64) (store.Thread, error) {
	for _, t := range f.chat().threads {
		if t.ID == id {
			return t, nil
		}
	}
	return store.Thread{}, store.ErrNotFound
}

func (f *fakeStore) IncidentThread(_ context.Context, incidentID int64, create bool) (store.Thread, bool, error) {
	c := f.chat()
	for _, t := range c.threads {
		if t.IncidentID != nil && *t.IncidentID == incidentID {
			return t, true, nil
		}
	}
	if !create {
		return store.Thread{}, false, store.ErrNotFound
	}
	id := incidentID
	t := store.Thread{ID: int64(len(c.threads) + 1), Kind: store.ThreadIncident, IncidentID: &id, SessionID: "fresh-session"}
	c.threads = append(c.threads, t)
	return t, false, nil
}

func (f *fakeStore) CreateGeneralThread(context.Context) (store.Thread, error) {
	c := f.chat()
	t := store.Thread{ID: int64(len(c.threads) + 1), Kind: store.ThreadGeneral, SessionID: "general-session"}
	c.threads = append(c.threads, t)
	return t, nil
}

func (f *fakeStore) RecentThreads(context.Context, int) ([]store.Thread, error) {
	return f.chat().threads, nil
}

func (f *fakeStore) HasStoredSession(_ context.Context, sessionID string, _ int64) (bool, error) {
	_, ok := f.chat().sessions[sessionID]
	return ok, nil
}

func (f *fakeStore) chat() *chatState {
	if f.chatState == nil {
		f.chatState = &chatState{sessions: map[string][]byte{}, result: store.ChatEnqueue{RunID: 9, NewRun: true}}
	}
	return f.chatState
}

func (f *fakeStore) GetPullRequest(_ context.Context, id int64) (store.PullRequest, error) {
	p := f.pr()
	p.mu.Lock()
	defer p.mu.Unlock()
	if id < 1 || int(id) > len(p.prs) {
		return store.PullRequest{}, store.ErrNotFound
	}
	return p.prs[id-1], nil
}

func (f *fakeStore) UpdatePullRequestHead(_ context.Context, id int64, from, head string, files []string) (bool, error) {
	p := f.pr()
	p.mu.Lock()
	defer p.mu.Unlock()
	pr := &p.prs[id-1]
	if pr.HeadSHA != from {
		return false, nil
	}
	pr.HeadSHA, pr.Files = head, files
	return true, nil
}

func (f *fakeStore) SetPullRequestTitle(_ context.Context, id int64, title string) error {
	p := f.pr()
	p.mu.Lock()
	defer p.mu.Unlock()
	p.prs[id-1].Title = title
	return nil
}

func (f *fakeStore) EnqueueChat(_ context.Context, m store.ChatMessage, _ store.ChatLimits) (store.ChatEnqueue, error) {
	c := f.chat()
	if c.failID != 0 && m.GHID == c.failID {
		return store.ChatEnqueue{}, errors.New("encode failed")
	}
	c.enqueued = append(c.enqueued, m)
	res := c.result
	if !res.Duplicate {
		m.ID = int64(len(c.messages) + 1)
		c.messages = append(c.messages, m)
		res.MessageID = m.ID
	}
	return res, nil
}

func (f *fakeStore) ChatMessages(_ context.Context, threadID int64) ([]store.ChatMessage, error) {
	var out []store.ChatMessage
	for _, m := range f.chat().messages {
		if m.ThreadID == threadID {
			out = append(out, m)
		}
	}
	return out, nil
}

func (f *fakeStore) TurnMessages(_ context.Context, runID int64) ([]store.ChatMessage, error) {
	var out []store.ChatMessage
	for _, m := range f.chat().messages {
		if m.RunID != nil && *m.RunID == runID && m.Source != store.SourceAgent {
			out = append(out, m)
		}
	}
	return out, nil
}

func (f *fakeStore) FinishChatTurn(_ context.Context, runID int64, answered bool, reply string) (store.ChatMessage, bool, error) {
	c := f.chat()
	for _, m := range c.messages {
		if m.RunID != nil && *m.RunID == runID && m.Source == store.SourceAgent {
			return store.ChatMessage{}, false, nil
		}
	}
	status := store.ChatAnswered
	if !answered {
		status = store.ChatFailed
	}
	for i := range c.messages {
		if m := &c.messages[i]; m.RunID != nil && *m.RunID == runID {
			m.Status = status
		}
	}
	run := f.runs[runID]
	m := store.ChatMessage{ID: int64(len(c.messages) + 1), ThreadID: *run.ThreadID, RunID: &runID, Source: store.SourceAgent,
		Body: reply, Status: store.ChatReply}
	if run.PRID != nil {
		m.PRID = *run.PRID
	}
	c.messages = append(c.messages, m)
	return m, true, nil
}

func (f *fakeStore) LatestSession(_ context.Context, sessionID string, _ int64) ([]byte, error) {
	gz, ok := f.chat().sessions[sessionID]
	if !ok {
		return nil, store.ErrNotFound
	}
	return gz, nil
}

func (f *fakeStore) ActiveChatRun(context.Context, int64) (store.Run, bool, error) {
	if r := f.chat().activeChat; r != nil {
		return *r, true, nil
	}
	return store.Run{}, false, nil
}

func (f *fakeStore) RunEventsAfter(_ context.Context, runID int64, seq int) ([]store.RunEvent, error) {
	var out []store.RunEvent
	for _, e := range f.events[runID] {
		if e.Seq > seq {
			out = append(out, e)
		}
	}
	return out, nil
}

const chatSession = "11111111-2222-3333-4444-555555555555"

// chatServer is prServer with PR #1200 (row 1) open for incident 3, its
// thread 1, and run 9, a running chat turn in it (token "chat") with two
// messages.
func chatServer(t *testing.T, prepareErr error) (*Server, *fakeStore, *fakeGitHub, *[]proposal.UpdateRequest) {
	t.Helper()
	s, fs, gh, _ := prServer(t, nil)
	var updates []proposal.UpdateRequest
	s.prs.PrepareUpdate = func(_ context.Context, _ proposal.Config, token string, req proposal.UpdateRequest) (*proposal.Commit, error) {
		if token != "tok" {
			t.Errorf("prepare update got token %q", token)
		}
		updates = append(updates, req)
		if prepareErr != nil {
			return nil, prepareErr
		}
		return &proposal.Commit{Branch: req.Branch, BaseSHA: req.BaseSHA, ParentSHA: req.HeadSHA, HeadSHA: "head2",
			Files: []string{"apps/ebooks/deployment.yaml"}}, nil
	}
	if _, err := fs.InsertPullRequest(t.Context(), store.PullRequest{IncidentID: 3, RunID: 7, Number: 1200,
		URL: "https://github.com/jcwearn/k3s-cluster/pull/1200", Title: "fix(ebooks): raise memory", Branch: "agent/3",
		BaseSHA: "base1", HeadSHA: "head1", State: store.PROpen, Checks: store.ChecksSuccess,
		Files: []string{"apps/ebooks/deployment.yaml"}}); err != nil {
		t.Fatal(err)
	}
	prID, run, threadID, incident := int64(1), int64(9), int64(1), int64(3)
	fs.runs[run] = store.Run{ID: run, IncidentID: 3, Status: store.RunRunning, Kind: store.KindChat, PRID: &prID, ThreadID: &threadID,
		SessionID: chatSession, Model: "opus"}
	fs.tokenHash[string(dispatch.HashToken("chat"))] = run
	line := 12
	c := fs.chat()
	c.threads = []store.Thread{{ID: threadID, Kind: store.ThreadIncident, IncidentID: &incident, SessionID: chatSession,
		AlertName: "KubePodCrashLooping", Namespace: "ebooks"}}
	c.messages = []store.ChatMessage{
		{ID: 1, ThreadID: threadID, RunID: &run, Source: store.SourceUI, Author: "you", Body: "why this limit?", Status: store.ChatQueued,
			Page: "/incidents/3"},
		{ID: 2, ThreadID: threadID, PRID: 1, RunID: &run, Source: store.SourceGitHub, Author: "jcwearn", Body: "make it 512Mi",
			GHKind: store.GHReviewComment, Path: "apps/ebooks/deployment.yaml", Line: &line, Status: store.ChatQueued},
	}
	c.sessions[chatSession] = []byte("gzipped-session")
	return s, fs, gh, &updates
}

func TestChatTurnContextAndSession(t *testing.T) {
	s, _, _, _ := chatServer(t, nil)
	h := s.RunnerHandler()

	status, body := runnerReq(t, h, http.MethodGet, runnerapi.PathContext, "chat", "")
	if status != http.StatusOK {
		t.Fatalf("context: %d %s", status, body)
	}
	var rc runnerapi.Context
	if err := json.Unmarshal([]byte(body), &rc); err != nil {
		t.Fatal(err)
	}
	if rc.Kind != runnerapi.KindChat || !rc.UpdatePR || rc.ProposePR || !rc.Consolidate || !rc.IncidentTools || !rc.Resume ||
		rc.SessionID != chatSession || rc.Thread == nil || rc.Thread.Kind != runnerapi.ThreadIncident || rc.Thread.ID != 1 {
		t.Errorf("chat context flags: %+v", rc)
	}
	if !strings.HasPrefix(rc.Messages[0].Page, "incident #3 (KubePodCrashLooping in ebooks, firing, PR #1200 open") {
		t.Errorf("page context = %q", rc.Messages[0].Page)
	}
	if rc.PR == nil || rc.PR.Branch != "agent/3" || rc.PR.HeadSHA != "head1" || rc.PR.Number != 1200 {
		t.Errorf("chat context PR: %+v", rc.PR)
	}
	if len(rc.Messages) != 2 || rc.Messages[1].Path != "apps/ebooks/deployment.yaml" || rc.Messages[1].Kind != store.GHReviewComment {
		t.Errorf("chat context messages: %+v", rc.Messages)
	}

	if status, body := runnerReq(t, h, http.MethodGet, runnerapi.PathSession, "chat", ""); status != http.StatusOK || body != "gzipped-session" {
		t.Errorf("session: %d %q", status, body)
	}
	if status, _ := runnerReq(t, h, http.MethodGet, runnerapi.PathSession, "good", ""); status != http.StatusBadRequest {
		t.Errorf("a diagnosis run fetched a session: %d", status)
	}

	// A diagnosis run's context says so.
	_, body = runnerReq(t, h, http.MethodGet, runnerapi.PathContext, "good", "")
	if err := json.Unmarshal([]byte(body), &rc); err != nil || rc.Kind != runnerapi.KindDiagnose || rc.UpdatePR {
		t.Errorf("diagnosis context: %+v %v", rc, err)
	}
}

func callTool(t *testing.T, cs *mcp.ClientSession, name string, in any) (string, bool) {
	t.Helper()
	res, err := cs.CallTool(t.Context(), &mcp.CallToolParams{Name: name, Arguments: in})
	if err != nil {
		t.Fatal(err)
	}
	var b strings.Builder
	for _, c := range res.Content {
		if tc, ok := c.(*mcp.TextContent); ok {
			b.WriteString(tc.Text)
		}
	}
	return b.String(), res.IsError
}

var goodUpdate = UpdateInput{Title: "fix(ebooks): use 512Mi instead", Patch: "diff --git a/apps/ebooks/deployment.yaml b/apps/ebooks/deployment.yaml\n"}

func TestUpdatePR(t *testing.T) {
	s, fs, gh, updates := chatServer(t, nil)
	cs, err := mcpSession(t, s, "chat")
	if err != nil {
		t.Fatal(err)
	}
	tools, err := cs.ListTools(t.Context(), nil)
	if err != nil || len(tools.Tools) != 5 || hasTool(tools.Tools, "propose_pr") {
		t.Fatalf("tools: %+v %v", tools, err)
	}
	for _, want := range []string{"update_pr", "list_incidents", "get_incident", "merge_incident", "link_incident"} {
		if !hasTool(tools.Tools, want) {
			t.Errorf("a chat turn on an open PR lacks %s", want)
		}
	}

	out, isErr := callTool(t, cs, "update_pr", goodUpdate)
	if isErr || !strings.HasPrefix(out, "Pushed head2 to agent/3") {
		t.Fatalf("update: %s", out)
	}
	if len(*updates) != 1 {
		t.Fatalf("prepare called %d times", len(*updates))
	}
	u := (*updates)[0]
	if u.Branch != "agent/3" || u.HeadSHA != "head1" || u.BaseSHA != "base1" || u.IncidentID != 3 || u.Title != goodUpdate.Title {
		t.Errorf("update request = %+v", u)
	}
	if last := gh.perms[len(gh.perms)-1]; last["contents"] != "write" || len(last) != 1 {
		t.Errorf("push token permissions = %v", last)
	}
	if pr, _ := fs.GetPullRequest(t.Context(), 1); pr.HeadSHA != "head2" {
		t.Errorf("PR head not recorded: %+v", pr)
	}

	// A diagnosis run never gets update_pr.
	diag, err := mcpSession(t, s, "good")
	if err != nil {
		t.Fatal(err)
	}
	if tools, _ := diag.ListTools(t.Context(), nil); hasTool(tools.Tools, "update_pr") {
		t.Error("a diagnosis run was offered update_pr")
	}
}

func TestUpdatePRRefusals(t *testing.T) {
	s, fs, _, updates := chatServer(t, nil)
	cs, err := mcpSession(t, s, "chat")
	if err != nil {
		t.Fatal(err)
	}

	// Another open agent PR already changes the file.
	if _, err := fs.InsertPullRequest(t.Context(), store.PullRequest{IncidentID: 4, Number: 1201, URL: "u2", State: store.PROpen,
		Files: []string{"apps/ebooks/deployment.yaml"}}); err != nil {
		t.Fatal(err)
	}
	if out, isErr := callTool(t, cs, "update_pr", goodUpdate); !isErr || !strings.Contains(out, "#1201") {
		t.Errorf("overlap with another PR: %s", out)
	}
	fs.pr().prs[1].State = store.PRClosed

	if out, isErr := callTool(t, cs, "update_pr", UpdateInput{Title: "make it bigger", Patch: goodUpdate.Patch}); !isErr || !strings.Contains(out, "conventional commit") {
		t.Errorf("bad title: %s", out)
	}
	if out, isErr := callTool(t, cs, "update_pr", goodUpdate); isErr {
		t.Errorf("third call, no overlap left: %s", out)
	}
	n := len(*updates)
	if out, isErr := callTool(t, cs, "update_pr", goodUpdate); !isErr || !strings.Contains(out, "already called 3 times") {
		t.Errorf("fourth call: %s", out)
	}
	if len(*updates) != n {
		t.Error("the fourth call reached prepare")
	}

	fs.pr().prs[0].State = store.PRMerged
	run := fs.runs[9]
	run.ID = 10 // a fresh turn, under the per-turn cap
	if _, _, err := s.updatePR(t.Context(), run, goodUpdate); err == nil || !strings.Contains(err.Error(), "merged") {
		t.Errorf("update on a merged PR: %v", err)
	}
}

func TestChatTurnFinishRecordsReply(t *testing.T) {
	s, fs, _, _ := chatServer(t, nil)
	notifier := &recordingNotifier{done: make(chan struct{})}
	s.notifier = notifier
	cs, err := mcpSession(t, s, "chat")
	if err != nil {
		t.Fatal(err)
	}
	if out, isErr := callTool(t, cs, "update_pr", goodUpdate); isErr {
		t.Fatal(out)
	}
	status, body := runnerReq(t, s.RunnerHandler(), http.MethodPost, runnerapi.PathFinish, "chat",
		`{"status":"succeeded","report":"256Mi was the old peak. Now 512Mi."}`)
	if status != http.StatusNoContent {
		t.Fatalf("finish: %d %s", status, body)
	}
	msgs := fs.chat().messages
	reply := msgs[len(msgs)-1]
	if reply.Source != store.SourceAgent || !strings.Contains(reply.Body, "Now 512Mi.") ||
		!strings.Contains(reply.Body, "Pushed `head2`: fix(ebooks): use 512Mi instead") {
		t.Errorf("reply = %+v", reply)
	}
	if msgs[0].Status != store.ChatAnswered {
		t.Errorf("message status = %s", msgs[0].Status)
	}
	select {
	case <-notifier.done:
		t.Error("a chat turn sent a diagnosis notification")
	case <-time.After(50 * time.Millisecond):
	}
}

func TestChatTurnFailureReply(t *testing.T) {
	s, fs, _, _ := chatServer(t, nil)
	if status, _ := runnerReq(t, s.RunnerHandler(), http.MethodPost, runnerapi.PathFinish, "chat",
		`{"status":"failed","error":"could not restore the PR's session: 404\nmore"}`); status != http.StatusNoContent {
		t.Fatal(status)
	}
	msgs := fs.chat().messages
	reply := msgs[len(msgs)-1]
	if reply.Body != "I could not finish this turn: could not restore the PR's session: 404" || msgs[0].Status != store.ChatFailed {
		t.Errorf("failure reply = %+v, first message %s", reply, msgs[0].Status)
	}
}

func newTestServer(t *testing.T, h http.Handler) string {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return srv.URL
}

// The incident page no longer embeds the conversation: it offers the chat
// window (chat.js) on the incident's thread, and every page loads it.
func TestIncidentPageChatButton(t *testing.T) {
	s, _, _, _ := chatServer(t, nil)
	status, body := do(t, s.UIHandler(), http.MethodGet, "/incidents/3", "")
	if status != http.StatusOK {
		t.Fatalf("status %d", status)
	}
	for _, want := range []string{`data-chat-incident="3"`, `/static/chat.js?v=`, `/static/chat.css?v=`} {
		if !strings.Contains(body, want) {
			t.Errorf("page lacks %q", want)
		}
	}
	if strings.Contains(body, `action="/incidents/3/chat"`) {
		t.Error("the old chat form is still there")
	}
	if status, _ := do(t, s.UIHandler(), http.MethodPost, "/incidents/3/chat", "message=hi"); status != http.StatusMethodNotAllowed && status != http.StatusNotFound {
		t.Errorf("old chat route: %d", status)
	}
}

func TestStaticAssets(t *testing.T) {
	s, _, _, _ := chatServer(t, nil)
	h := s.UIHandler()
	_, page := do(t, h, http.MethodGet, "/incidents/3", "")
	i := strings.Index(page, "/static/chat.js?v=")
	if i < 0 {
		t.Fatal("no chat.js link")
	}
	link := page[i : i+strings.IndexByte(page[i:], '"')]
	srv := newTestServer(t, h)
	for _, c := range []struct{ path, cache, typ string }{
		{link, "immutable", "javascript"},
		{"/static/chat.css", "no-cache", "text/css"},
	} {
		req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, srv+c.path, http.NoBody)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusOK || !strings.Contains(resp.Header.Get("Cache-Control"), c.cache) ||
			!strings.Contains(resp.Header.Get("Content-Type"), c.typ) {
			t.Errorf("%s: %d %q %q", c.path, resp.StatusCode, resp.Header.Get("Cache-Control"), resp.Header.Get("Content-Type"))
		}
	}
}

func TestRunStream(t *testing.T) {
	s, fs, _, _ := chatServer(t, nil)
	now := time.Now()
	run := fs.runs[9]
	run.Status, run.FinishedAt = store.RunSucceeded, &now
	fs.runs[9] = run
	fs.events = map[int64][]store.RunEvent{9: {
		{Seq: 0, Type: "system", Payload: json.RawMessage(`{"type":"system","subtype":"init","model":"opus","tools":[]}`)},
		{Seq: 1, Type: "assistant", Payload: json.RawMessage(`{"type":"assistant","message":{"content":[{"type":"text","text":"Checking the pod."}]}}`)},
	}}
	status, body := do(t, s.UIHandler(), http.MethodGet, "/runs/9/stream", "")
	if status != http.StatusOK {
		t.Fatalf("status %d", status)
	}
	for _, want := range []string{"id: 0\nevent: item\n", "id: 1\nevent: item\n", `"body":"Checking the pod."`, "event: status\ndata: \"succeeded\"", "event: done"} {
		if !strings.Contains(body, want) {
			t.Errorf("stream lacks %q:\n%s", want, body)
		}
	}

	// A reconnect resumes after the last event it saw.
	srv := newTestServer(t, s.UIHandler())
	req, _ := http.NewRequestWithContext(t.Context(), http.MethodGet, srv+"/runs/9/stream", http.NoBody)
	req.Header.Set("Last-Event-ID", "0")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "id: 0\n") || !strings.Contains(string(b), "id: 1\n") {
		t.Errorf("reconnect replayed from the start:\n%s", b)
	}
}

func TestUpdatePRRetitles(t *testing.T) {
	s, fs, gh, updates := chatServer(t, nil)
	cs, err := mcpSession(t, s, "chat")
	if err != nil {
		t.Fatal(err)
	}

	// A bad PR title is refused before anything is committed or pushed.
	bad := goodUpdate
	bad.PRTitle = "Make it bigger"
	if out, isErr := callTool(t, cs, "update_pr", bad); !isErr || !strings.Contains(out, "conventional commit") {
		t.Fatalf("bad PR title: %s", out)
	}
	if len(*updates) != 0 {
		t.Fatal("a refused PR title still reached prepare")
	}

	in := goodUpdate
	in.PRTitle = "fix(ebooks): raise memory limit to 512Mi"
	out, isErr := callTool(t, cs, "update_pr", in)
	if isErr || !strings.Contains(out, "Retitled the PR: fix(ebooks): raise memory limit to 512Mi") {
		t.Fatalf("update with a title: %s", out)
	}
	if len(gh.retitled) != 1 || gh.retitled[0] != "#1200 fix(ebooks): raise memory limit to 512Mi" {
		t.Errorf("GitHub renames = %v", gh.retitled)
	}
	if last := gh.perms[len(gh.perms)-1]; last["pull_requests"] != "write" || last["contents"] != "read" || len(last) != 2 {
		t.Errorf("retitle token permissions = %v", last)
	}
	if pr, _ := fs.GetPullRequest(t.Context(), 1); pr.Title != in.PRTitle {
		t.Errorf("stored title = %q", pr.Title)
	}

	if status, _ := runnerReq(t, s.RunnerHandler(), http.MethodPost, runnerapi.PathFinish, "chat",
		`{"status":"succeeded","report":"Now 512Mi."}`); status != http.StatusNoContent {
		t.Fatal(status)
	}
	msgs := fs.chat().messages
	if reply := msgs[len(msgs)-1].Body; !strings.Contains(reply, "Pushed `head2`: fix(ebooks): use 512Mi instead\n\nRetitled the PR: fix(ebooks): raise memory limit to 512Mi") {
		t.Errorf("reply footer = %q", reply)
	}
}

// A rename that fails after the commit landed is not a failed update: the
// agent is told the title is unchanged, and the footer does not claim it.
func TestUpdatePRRetitleFailure(t *testing.T) {
	s, fs, gh, _ := chatServer(t, nil)
	gh.retitleErr = errors.New("GitHub answered 403")
	cs, err := mcpSession(t, s, "chat")
	if err != nil {
		t.Fatal(err)
	}
	in := goodUpdate
	in.PRTitle = "fix(ebooks): raise memory limit to 512Mi"
	out, isErr := callTool(t, cs, "update_pr", in)
	if isErr || !strings.HasPrefix(out, "Pushed head2") || !strings.Contains(out, "could NOT be changed") ||
		!strings.Contains(out, "fix(ebooks): raise memory") {
		t.Fatalf("update with a failed rename: %s", out)
	}
	if pr, _ := fs.GetPullRequest(t.Context(), 1); pr.Title != "fix(ebooks): raise memory" {
		t.Errorf("stored title changed to %q", pr.Title)
	}
	if footer := s.pushedFooter(t.Context(), fs.runs[9]); strings.Contains(footer, "Retitled") {
		t.Errorf("footer claims a rename: %q", footer)
	}
}
