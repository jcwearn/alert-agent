package controller

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/jcwearn/alert-agent/internal/dispatch"
	"github.com/jcwearn/alert-agent/internal/runnerapi"
	"github.com/jcwearn/alert-agent/internal/store"
)

// threadServer is prServer with no PR, plus:
//   - thread 1, general, and run 10, a running general turn (token "general");
//   - thread 2, incident 3's (no PR yet), and run 11, a running turn
//     (token "incident") that starts its session fresh;
//   - incident 4 in ebooks and 5 in another namespace, to merge and link.
func threadServer(t *testing.T) (*Server, *fakeStore) {
	t.Helper()
	s, fs, _, _ := prServer(t, nil)
	opened := fs.incidents[0].OpenedAt
	fs.incidents = append(fs.incidents,
		store.Incident{ID: 4, AlertName: "KubePodNotReady", Namespace: "ebooks", AlertStatus: "firing", OpenedAt: opened},
		store.Incident{ID: 5, AlertName: "NodeDiskPressure", Namespace: "kube-system", AlertStatus: "resolved", OpenedAt: opened})
	general, incThread, inc := int64(1), int64(2), int64(3)
	c := fs.chat()
	c.threads = []store.Thread{
		{ID: general, Kind: store.ThreadGeneral, Title: "How is the cluster?", SessionID: "general-session"},
		{ID: incThread, Kind: store.ThreadIncident, IncidentID: &inc, SessionID: "incident-session", AlertName: "KubePodCrashLooping"},
	}
	fs.runs[10] = store.Run{ID: 10, Status: store.RunRunning, Kind: store.KindChat, ThreadID: &general, SessionID: "general-session"}
	fs.runs[11] = store.Run{ID: 11, IncidentID: 3, Status: store.RunRunning, Kind: store.KindChat, ThreadID: &incThread, SessionID: "incident-session"}
	fs.tokenHash[string(dispatch.HashToken("general"))] = 10
	fs.tokenHash[string(dispatch.HashToken("incident"))] = 11
	r10, r11 := int64(10), int64(11)
	c.messages = []store.ChatMessage{
		{ID: 1, ThreadID: general, RunID: &r10, Source: store.SourceUI, Author: "you", Body: "how is the cluster?", Page: "/"},
		{ID: 2, ThreadID: incThread, RunID: &r11, Source: store.SourceUI, Author: "you", Body: "fix it", Page: "/incidents/3"},
	}
	return s, fs
}

func runnerContextFor(t *testing.T, s *Server, token string) runnerapi.Context {
	t.Helper()
	status, body := runnerReq(t, s.RunnerHandler(), http.MethodGet, runnerapi.PathContext, token, "")
	if status != http.StatusOK {
		t.Fatalf("context: %d %s", status, body)
	}
	var rc runnerapi.Context
	if err := json.Unmarshal([]byte(body), &rc); err != nil {
		t.Fatal(err)
	}
	return rc
}

func TestGeneralThreadTurn(t *testing.T) {
	s, _ := threadServer(t)
	rc := runnerContextFor(t, s, "general")
	if rc.Thread == nil || rc.Thread.Kind != runnerapi.ThreadGeneral || rc.Resume || rc.ProposePR || rc.UpdatePR ||
		!rc.Consolidate || !rc.IncidentTools || rc.Incident.ID != 0 || rc.PR != nil {
		t.Fatalf("general context: %+v", rc)
	}
	if len(rc.Messages) != 1 || !strings.HasPrefix(rc.Messages[0].Page, "the home page: 2 incident(s) firing") {
		t.Errorf("messages: %+v", rc.Messages)
	}
	cs, err := mcpSession(t, s, "general")
	if err != nil {
		t.Fatal(err)
	}
	tools, err := cs.ListTools(t.Context(), nil)
	if err != nil || len(tools.Tools) != 4 || hasTool(tools.Tools, "propose_pr") || hasTool(tools.Tools, "update_pr") {
		t.Fatalf("general tools: %+v %v", tools, err)
	}
	// Its run page renders without an incident.
	if status, body := do(t, s.UIHandler(), http.MethodGet, "/runs/10", ""); status != http.StatusOK || !strings.Contains(body, "Run #10") {
		t.Errorf("general run page: %d", status)
	}
}

func TestIncidentThreadWithoutPRCanPropose(t *testing.T) {
	s, fs := threadServer(t)
	rc := runnerContextFor(t, s, "incident")
	if !rc.ProposePR || rc.UpdatePR || rc.Resume || rc.Incident.ID != 3 || len(rc.Alerts) == 0 {
		t.Fatalf("no-PR incident context: %+v", rc)
	}
	cs, err := mcpSession(t, s, "incident")
	if err != nil {
		t.Fatal(err)
	}
	tools, _ := cs.ListTools(t.Context(), nil)
	if !hasTool(tools.Tools, "propose_pr") || hasTool(tools.Tools, "update_pr") {
		t.Fatalf("tools: %+v", tools)
	}
	out, isErr := callTool(t, cs, "propose_pr", goodProposal)
	if isErr || !strings.Contains(out, "Opened PR #1200") {
		t.Fatalf("propose from chat: %s", out)
	}
	if p := fs.pr().prs; len(p) != 1 || p[0].IncidentID != 3 || p[0].RunID != 11 {
		t.Errorf("PR = %+v", p)
	}
	// Next turn: the PR is open, so update_pr instead.
	if rc := runnerContextFor(t, s, "incident"); rc.ProposePR || !rc.UpdatePR || rc.PR == nil {
		t.Errorf("after the PR: %+v", rc)
	}
}

func TestChatMergeAndLinkAnyIncident(t *testing.T) {
	s, fs := threadServer(t)
	cs, err := mcpSession(t, s, "general")
	if err != nil {
		t.Fatal(err)
	}
	// Neither incident is the run's, and they are in different namespaces:
	// an alert run would be refused both; a chat directed by the owner is not.
	out, isErr := callTool(t, cs, "link_incident", ChatLinkInput{IncidentID: 3, OtherID: 5, Reason: "same node"})
	if isErr || !strings.Contains(out, "Linked #3 and #5") {
		t.Fatalf("link: %s", out)
	}
	out, isErr = callTool(t, cs, "merge_incident", ChatMergeInput{IncidentID: 4, IntoID: 3, Reason: "same pod"})
	if isErr || !strings.Contains(out, "Merged #4 into #3") {
		t.Fatalf("merge: %s", out)
	}
	if in := fs.incident(4); in.ParentID == nil || *in.ParentID != 3 || in.MergedBy != "run:10" || in.MergeReason != "same pod" {
		t.Errorf("incident 4 after merge: %+v", in)
	}
	if out, isErr := callTool(t, cs, "merge_incident", ChatMergeInput{IncidentID: 4, IntoID: 3}); !isErr || !strings.Contains(out, "reason is required") {
		t.Errorf("merge without a reason: %s", out)
	}
	if out, isErr := callTool(t, cs, "link_incident", ChatLinkInput{IncidentID: 99, OtherID: 3, Reason: "x"}); !isErr || !strings.Contains(out, "#99 does not exist") {
		t.Errorf("unknown incident: %s", out)
	}
	acts := fs.pr().actions
	if len(acts) < 2 || acts[0].runID != 10 || acts[0].tool != "link_incident" || acts[0].target != 5 {
		t.Errorf("audit = %+v", acts)
	}
}

func TestIncidentTools(t *testing.T) {
	s, fs := threadServer(t)
	fs.runs[20] = store.Run{ID: 20, IncidentID: 3, Kind: store.KindDiagnose, Status: store.RunSucceeded,
		Report: "## Summary\nCalibre is OOMKilled.\n\n## Cause\nlimit"}
	cs, err := mcpSession(t, s, "general")
	if err != nil {
		t.Fatal(err)
	}
	out, isErr := callTool(t, cs, "list_incidents", ListIncidentsInput{Status: "firing", SinceHours: 24 * 365 * 10})
	if isErr || !strings.Contains(out, "#3 KubePodCrashLooping") || !strings.Contains(out, "#4 KubePodNotReady") || strings.Contains(out, "#5") {
		t.Errorf("list_incidents: %s", out)
	}
	out, isErr = callTool(t, cs, "get_incident", GetIncidentInput{ID: 3})
	if isErr || !strings.Contains(out, "Incident #3: KubePodCrashLooping") || !strings.Contains(out, "pod=a") ||
		!strings.Contains(out, "Latest diagnosis (run #20)") {
		t.Errorf("get_incident: %s", out)
	}
	if out, isErr := callTool(t, cs, "get_incident", GetIncidentInput{ID: 99}); !isErr || !strings.Contains(out, "does not exist") {
		t.Errorf("get_incident unknown: %s", out)
	}
}

func TestPageContext(t *testing.T) {
	s, _ := threadServer(t)
	ctx := t.Context()
	for path, want := range map[string]string{
		"/":              "the home page: 2 incident(s) firing, 0 agent PR(s) awaiting review",
		"/incidents/3":   "incident #3 (KubePodCrashLooping in ebooks, firing)",
		"/runs/10":       "the transcript of run #10 (chat, running, about a general conversation)",
		"/incidents/99":  "",
		"/incidents/3/x": "",
		"/evil?x=<b>":    "",
		"":               "",
	} {
		if got := s.pageContext(ctx, path); got != want {
			t.Errorf("pageContext(%q) = %q, want %q", path, got, want)
		}
	}
	if pageLabel("/incidents/3") != "#3" || pageLabel("/") != "home" || pageLabel("/runs/7") != "run #7" || pageLabel("/x") != "" {
		t.Error("page labels")
	}
}

func apiJSON(t *testing.T, s *Server, method, path, body string, out any) int {
	t.Helper()
	status, resp := do(t, s.UIHandler(), method, path, body)
	if out != nil && status < 300 {
		if err := json.Unmarshal([]byte(resp), out); err != nil {
			t.Fatalf("%s %s: %v\n%s", method, path, err, resp)
		}
	}
	return status
}

func TestChatAPI(t *testing.T) {
	s, fs := threadServer(t)
	waker := &countingWaker{}
	s.waker = waker
	r10 := int64(10)
	fs.chat().messages = append(fs.chat().messages, store.ChatMessage{ID: 3, ThreadID: 1, RunID: &r10, Source: store.SourceAgent,
		Body: "All **green**. <script>x</script>", Status: store.ChatReply, CreatedAt: time.Now()})

	var list struct {
		Here struct {
			IncidentID int64      `json:"incident_id"`
			Title      string     `json:"title"`
			Thread     *apiThread `json:"thread"`
		} `json:"here"`
		Threads []apiThread `json:"threads"`
	}
	if status := apiJSON(t, s, http.MethodGet, "/api/chat/threads?page=/incidents/3", "", &list); status != http.StatusOK {
		t.Fatal(status)
	}
	if list.Here.IncidentID != 3 || list.Here.Thread == nil || list.Here.Thread.ID != 2 || len(list.Threads) != 2 ||
		list.Threads[1].Title != "#3 KubePodCrashLooping" {
		t.Errorf("threads = %+v", list)
	}

	var th struct {
		Thread   apiThread    `json:"thread"`
		Messages []apiMessage `json:"messages"`
		Can      []string     `json:"can"`
	}
	if status := apiJSON(t, s, http.MethodGet, "/api/chat/threads/1", "", &th); status != http.StatusOK {
		t.Fatal(status)
	}
	if len(th.Messages) != 2 || th.Messages[0].Text != "how is the cluster?" || th.Messages[0].From != "home" ||
		!strings.Contains(th.Messages[1].HTML, "<strong>green</strong>") || strings.Contains(th.Messages[1].HTML, "<script>") {
		t.Errorf("thread = %+v", th)
	}
	if status := apiJSON(t, s, http.MethodGet, "/api/chat/threads/99", "", nil); status != http.StatusNotFound {
		t.Errorf("unknown thread: %d", status)
	}

	var created apiThread
	if status := apiJSON(t, s, http.MethodPost, "/api/chat/threads", `{"kind":"general"}`, &created); status != http.StatusOK || created.Kind != "general" {
		t.Fatalf("create general: %d %+v", status, created)
	}
	var inc apiThread
	if status := apiJSON(t, s, http.MethodPost, "/api/chat/threads", `{"kind":"incident","incident_id":4}`, &inc); status != http.StatusOK ||
		inc.IncidentID == nil || *inc.IncidentID != 4 {
		t.Fatalf("create incident thread: %d %+v", status, inc)
	}

	var posted map[string]any
	if status := apiJSON(t, s, http.MethodPost, "/api/chat/threads/1/messages", `{"body":" what about #3? ","page":"/incidents/3"}`, &posted); status != http.StatusOK ||
		posted["new_turn"] != true {
		t.Fatalf("post: %d %v", status, posted)
	}
	last := fs.chat().enqueued[len(fs.chat().enqueued)-1]
	if last.ThreadID != 1 || last.Body != "what about #3?" || last.Page != "/incidents/3" || waker.n == 0 {
		t.Errorf("enqueued = %+v", last)
	}
	apiJSON(t, s, http.MethodPost, "/api/chat/threads/1/messages", `{"body":"hi","page":"https://evil.example/x"}`, nil)
	if last := fs.chat().enqueued[len(fs.chat().enqueued)-1]; last.Page != "" {
		t.Errorf("an unknown page was kept: %q", last.Page)
	}
	if status := apiJSON(t, s, http.MethodPost, "/api/chat/threads/1/messages", `{"body":"   "}`, nil); status != http.StatusBadRequest {
		t.Errorf("empty message: %d", status)
	}
	fs.chat().result = store.ChatEnqueue{Capped: "this conversation already had 4 agent turns in the last hour (the limit)",
		RetryAt: time.Now().Add(20 * time.Minute)}
	apiJSON(t, s, http.MethodPost, "/api/chat/threads/1/messages", `{"body":"again"}`, &posted)
	if c, _ := posted["capped"].(string); !strings.HasPrefix(c, "Not processed:") || !strings.Contains(c, "Try again after") {
		t.Errorf("capped: %v", posted)
	}

	// A form or fetch from another site cannot post.
	srv := newTestServer(t, s.UIHandler())
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodPost, srv+"/api/chat/threads/1/messages", strings.NewReader(`{"body":"x"}`))
	req.Header.Set("Sec-Fetch-Site", "cross-site")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("cross-site POST: %d", resp.StatusCode)
	}
}
