package controller

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/jcwearn/alert-agent/internal/runnerapi"
	"github.com/jcwearn/alert-agent/internal/store"
)

// The fakeStore's consolidation methods: just enough model to check what
// the controller asks for.

func (f *fakeStore) RecordAction(_ context.Context, a store.ActionStart) (int64, error) {
	p := f.pr()
	p.mu.Lock()
	defer p.mu.Unlock()
	p.actions = append(p.actions, fakeAction{runID: a.RunID, actor: a.Actor, incident: a.IncidentID, target: a.TargetIncidentID,
		tool: a.Tool, status: store.ActionStarted, input: string(a.Input)})
	return int64(len(p.actions)), nil
}

func (f *fakeStore) IncidentActions(_ context.Context, id int64) ([]store.Action, error) {
	p := f.pr()
	p.mu.Lock()
	defer p.mu.Unlock()
	var out []store.Action
	for i, a := range p.actions {
		if a.incident == id || a.target == id {
			out = append(out, store.Action{ID: int64(i + 1), Actor: a.actor, IncidentID: a.incident, Tool: a.tool,
				Input: json.RawMessage(a.input), Status: a.status, Result: a.result})
		}
	}
	return out, nil
}

func (f *fakeStore) incident(id int64) *store.Incident {
	for i := range f.incidents {
		if f.incidents[i].ID == id {
			return &f.incidents[i]
		}
	}
	return nil
}

func (f *fakeStore) MergeIncident(_ context.Context, child, into int64, by, reason string) (int64, error) {
	c, t := f.incident(child), f.incident(into)
	if c == nil || t == nil {
		return 0, store.ErrNotFound
	}
	root := into
	if t.ParentID != nil {
		root = *t.ParentID
	}
	if root == child {
		return 0, store.ErrConflict
	}
	now := time.Now()
	c.ParentID, c.MergedBy, c.MergeReason, c.MergedAt = &root, by, reason, &now
	return root, nil
}

func (f *fakeStore) UnmergeIncident(_ context.Context, id int64) (int64, error) {
	c := f.incident(id)
	if c == nil || c.ParentID == nil {
		return 0, store.ErrNotFound
	}
	former := *c.ParentID
	c.ParentID, c.MergedBy, c.MergeReason, c.MergedAt = nil, "", "", nil
	return former, nil
}

func linkKey(x, y int64) [2]int64 { return [2]int64{min(x, y), max(x, y)} }

func (f *fakeStore) LinkIncidents(_ context.Context, x, y int64, reason, _ string, _ int64) (bool, error) {
	if f.links == nil {
		f.links = map[[2]int64]string{}
	}
	if _, ok := f.links[linkKey(x, y)]; ok {
		return false, nil
	}
	f.links[linkKey(x, y)] = reason
	return true, nil
}

func (f *fakeStore) UnlinkIncidents(_ context.Context, x, y int64) (bool, error) {
	_, ok := f.links[linkKey(x, y)]
	delete(f.links, linkKey(x, y))
	return ok, nil
}

func (f *fakeStore) IncidentLinks(context.Context, int64) ([]store.Link, error) { return nil, nil }
func (f *fakeStore) IncidentChildren(context.Context, int64) ([]store.Incident, error) {
	return nil, nil
}

func (f *fakeStore) RecentIncidents(context.Context, time.Time, int) ([]store.RecentIncident, error) {
	var out []store.RecentIncident
	for _, in := range f.incidents {
		out = append(out, store.RecentIncident{Incident: in, Labels: []map[string]string{{"pod": "cache-0"}},
			Report: "## Summary\nOOMKilled.\n\n## Evidence\nlots"})
	}
	return out, nil
}

func (f *fakeStore) RunIncidents(_ context.Context, runID int64) ([]int64, error) {
	if m, ok := f.members[runID]; ok {
		return m, nil
	}
	return []int64{f.runs[runID].IncidentID}, nil
}

func (f *fakeStore) LeftoverIncidents(_ context.Context, runID int64) ([]int64, error) {
	return f.leftover[runID], nil
}

func hasTool(tools []*mcp.Tool, name string) bool {
	return slices.ContainsFunc(tools, func(t *mcp.Tool) bool { return t.Name == name })
}

// consolidationFixture is run 7 on incident 3 (ebooks), batched with 4,
// plus an older incident 1 in ebooks and 2 in another namespace.
func consolidationFixture() *fakeStore {
	fs := runnerFixture()
	opened := fs.incidents[0].OpenedAt
	fs.incidents = append(fs.incidents,
		store.Incident{ID: 1, AlertName: "KubernetesContainerOomKiller", Namespace: "ebooks", AlertStatus: "firing", OpenedAt: opened.Add(-5 * time.Minute)},
		store.Incident{ID: 2, AlertName: "NodeDown", Namespace: "monitoring", AlertStatus: "firing", OpenedAt: opened},
		store.Incident{ID: 4, AlertName: "KubePodNotReady", Namespace: "ebooks", AlertStatus: "firing", OpenedAt: opened},
		store.Incident{ID: 5, AlertName: "Old", Namespace: "ebooks", AlertStatus: "resolved", OpenedAt: opened.Add(-3 * time.Hour)},
	)
	fs.members = map[int64][]int64{7: {3, 4}}
	return fs
}

func consolidationServer(t *testing.T) (*Server, *fakeStore, *mcp.ClientSession) {
	t.Helper()
	fs := consolidationFixture()
	s := New(fs, Options{RecentWindow: 30 * time.Minute}, nil, nil)
	s.now = func() time.Time { return fs.incidents[0].OpenedAt.Add(time.Minute) }
	cs, err := mcpSession(t, s, "good")
	if err != nil {
		t.Fatal(err)
	}
	return s, fs, cs
}

func call(t *testing.T, cs *mcp.ClientSession, tool string, args any) (string, bool) {
	t.Helper()
	res, err := cs.CallTool(t.Context(), &mcp.CallToolParams{Name: tool, Arguments: args})
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

func TestConsolidationToolsWithoutPRPath(t *testing.T) {
	_, _, cs := consolidationServer(t)
	tools, err := cs.ListTools(t.Context(), nil)
	if err != nil || len(tools.Tools) != 2 || !hasTool(tools.Tools, "merge_incident") || !hasTool(tools.Tools, "link_incident") {
		t.Fatalf("tools: %+v %v", tools, err)
	}
}

func TestMergeGuards(t *testing.T) {
	cases := []struct {
		name string
		in   MergeInput
		want string
	}{
		{"medium confidence", MergeInput{IntoID: 1, Reason: "same pod", Confidence: "medium"}, "link_incident instead"},
		{"other namespace", MergeInput{IntoID: 2, Reason: "same", Confidence: "high"}, "only within one namespace"},
		{"no reason", MergeInput{IntoID: 1, Confidence: "high"}, "reason is required"},
		{"not a member", MergeInput{IntoID: 1, Reason: "x", Confidence: "high", IncidentID: 1}, "not one of this run's incidents"},
		{"stale target", MergeInput{IntoID: 5, Reason: "x", Confidence: "high"}, "older than"},
		{"primary into own batch", MergeInput{IntoID: 4, Reason: "x", Confidence: "high"}, "merge the batch members into the primary"},
		{"missing target", MergeInput{IntoID: 99, Reason: "x", Confidence: "high"}, "does not exist"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, fs, cs := consolidationServer(t)
			out, isErr := call(t, cs, "merge_incident", tc.in)
			if !isErr || !strings.Contains(out, tc.want) {
				t.Fatalf("got %q (error=%v), want refusal containing %q", out, isErr, tc.want)
			}
			if fs.incident(3).Merged() {
				t.Fatal("refused merge changed the incident")
			}
			acts := fs.pr().actions
			if len(acts) != 1 || acts[0].status != store.ActionRejected || acts[0].tool != "merge_incident" || acts[0].actor != store.ActorAgent {
				t.Fatalf("refusal not audited: %+v", acts)
			}
		})
	}
}

func TestMergeAndLink(t *testing.T) {
	_, fs, cs := consolidationServer(t)

	// A batch member folds into the primary.
	out, isErr := call(t, cs, "merge_incident", MergeInput{IntoID: 3, IncidentID: 4, Reason: "same pod OOMKilled", Confidence: "high"})
	if isErr || !strings.Contains(out, "Merged #4 into #3") || strings.Contains(out, "Stop investigating") {
		t.Fatalf("member merge: %s", out)
	}
	if p := fs.incident(4).ParentID; p == nil || *p != 3 || fs.incident(4).MergedBy != "run:7" {
		t.Fatalf("incident 4 = %+v", fs.incident(4))
	}

	// The primary folds into the older incident that owns the problem.
	out, isErr = call(t, cs, "merge_incident", MergeInput{IntoID: 1, Reason: "same pod", Confidence: "high"})
	if isErr || !strings.Contains(out, "Merged #3 into #1") || !strings.Contains(out, "## Merged") {
		t.Fatalf("primary merge: %s", out)
	}

	out, isErr = call(t, cs, "link_incident", LinkInput{OtherID: 2, Reason: "the node went down first"})
	if isErr || !strings.Contains(out, "Linked #3 and #2") {
		t.Fatalf("link: %s", out)
	}
	out, isErr = call(t, cs, "link_incident", LinkInput{OtherID: 2, Reason: "again"})
	if isErr || !strings.Contains(out, "already linked") {
		t.Fatalf("relink: %s", out)
	}

	acts := fs.pr().actions
	if len(acts) != 4 {
		t.Fatalf("want 4 audited calls, got %+v", acts)
	}
	if acts[0].incident != 4 || acts[0].target != 3 || acts[2].target != 2 {
		t.Errorf("audit targets wrong: %+v", acts)
	}
	for _, a := range acts {
		if a.status != store.ActionSucceeded || a.runID != 7 {
			t.Errorf("action %+v", a)
		}
	}
}

func TestConsolidationCallCap(t *testing.T) {
	_, _, cs := consolidationServer(t)
	for range maxConsolidationCalls {
		call(t, cs, "link_incident", LinkInput{OtherID: 2, Reason: "x"})
	}
	out, isErr := call(t, cs, "link_incident", LinkInput{OtherID: 2, Reason: "x"})
	if !isErr || !strings.Contains(out, "already called") {
		t.Fatalf("cap: %s", out)
	}
}

func TestProposeRefusedForMergedIncident(t *testing.T) {
	s, fs, _, _ := prServer(t, nil)
	fs.incidents = append(fs.incidents, store.Incident{ID: 1, Namespace: "ebooks", AlertStatus: "firing"})
	parent := int64(1)
	fs.incidents[0].ParentID = &parent
	cs, err := mcpSession(t, s, "good")
	if err != nil {
		t.Fatal(err)
	}
	out, isErr := callPropose(t, cs, goodProposal)
	if !isErr || !strings.Contains(out, "merged into #1") {
		t.Fatalf("merged incident proposed: %s", out)
	}
}

func TestProposeRefusesOverlappingFiles(t *testing.T) {
	s, fs, gh, _ := prServer(t, nil)
	fs.pr().prs = []store.PullRequest{{ID: 1, IncidentID: 1, Number: 1143, URL: "https://github.com/o/r/pull/1143",
		State: store.PROpen, Files: []string{"apps/ebooks/deployment.yaml"}}}
	cs, err := mcpSession(t, s, "good")
	if err != nil {
		t.Fatal(err)
	}
	out, isErr := callPropose(t, cs, goodProposal)
	if !isErr || !strings.Contains(out, "open agent PR #1143") || !strings.Contains(out, "apps/ebooks/deployment.yaml") {
		t.Fatalf("overlap not refused: %s", out)
	}
	if len(gh.created) != 0 {
		t.Fatal("a PR was opened anyway")
	}
}

func TestProposeStoresFiles(t *testing.T) {
	s, fs, _, _ := prServer(t, nil)
	cs, err := mcpSession(t, s, "good")
	if err != nil {
		t.Fatal(err)
	}
	if out, isErr := callPropose(t, cs, goodProposal); isErr {
		t.Fatal(out)
	}
	if got := fs.pr().prs[0].Files; len(got) != 1 || got[0] != "apps/ebooks/deployment.yaml" {
		t.Fatalf("files = %v", got)
	}
}

func TestRunnerContextListsRelated(t *testing.T) {
	s, fs, _, _ := prServer(t, nil)
	more := consolidationFixture()
	fs.incidents, fs.members = more.incidents, more.members
	fs.pr().prs = []store.PullRequest{{IncidentID: 1, Number: 1143, Title: "fix", State: store.PROpen, Files: []string{"a.yaml"}}}
	s.opts.RecentWindow = 30 * time.Minute

	status, body := runnerReq(t, s.RunnerHandler(), http.MethodGet, runnerapi.PathContext, "good", "")
	if status != http.StatusOK {
		t.Fatalf("status %d: %s", status, body)
	}
	var rc runnerapi.Context
	if err := json.Unmarshal([]byte(body), &rc); err != nil {
		t.Fatal(err)
	}
	if !rc.Consolidate || len(rc.Batch) != 1 || rc.Batch[0].Incident.ID != 4 {
		t.Errorf("batch: %+v", rc.Batch)
	}
	var ids []int64
	for _, r := range rc.Recent {
		ids = append(ids, r.ID)
		if r.Summary != "OOMKilled." || len(r.Objects) != 1 || r.Objects[0] != "pod=cache-0" {
			t.Errorf("recent entry: %+v", r)
		}
	}
	if slices.Contains(ids, 3) || slices.Contains(ids, 4) || !slices.Contains(ids, 1) {
		t.Errorf("recent should exclude the run's own incidents: %v", ids)
	}
	if len(rc.OpenPRs) != 1 || rc.OpenPRs[0].Number != 1143 || rc.OpenPRs[0].Files[0] != "a.yaml" {
		t.Errorf("open PRs: %+v", rc.OpenPRs)
	}
}

func TestReportSummary(t *testing.T) {
	cases := map[string]string{
		"## Summary\nIt broke.\n\n## Evidence\nx": "It broke.",
		"## Merged\nInto #3: same pod.":           "Into #3: same pod.",
		"no sections":                             "",
	}
	for in, want := range cases {
		if got := reportSummary(in); got != want {
			t.Errorf("reportSummary(%q) = %q, want %q", in, got, want)
		}
	}
	long := reportSummary("## Summary\n" + strings.Repeat("é", 400))
	if len(long) > maxSummary+3 || !strings.HasSuffix(long, "...") || !strings.HasPrefix(long, "é") {
		t.Errorf("long summary cut badly: %d bytes", len(long))
	}
}

func TestHoldAndLeftovers(t *testing.T) {
	fs := consolidationFixture()
	waker := &countingWaker{}
	notifier := &recordingNotifier{done: make(chan struct{})}
	s := New(fs, Options{AgentEnabled: true, RunHold: time.Minute}, waker, notifier)

	fs.result = store.RecordResult{IncidentID: 4, Created: true}
	if status, _ := do(t, s.WebhookHandler(), http.MethodPost, "/webhook/alertmanager", validPayload); status != http.StatusNoContent {
		t.Fatalf("webhook status %d", status)
	}
	if len(fs.holds) != 1 || fs.holds[0] != time.Minute {
		t.Fatalf("new incident should be held: %v", fs.holds)
	}

	// Run 8 finished with incident 2 left over: it gets its own run, unheld.
	fs.runs[8] = store.Run{ID: 8, IncidentID: 3, Status: store.RunSucceeded}
	fs.leftover = map[int64][]int64{8: {2}}
	s.RunFinished(8)
	<-notifier.done
	if len(fs.enqueued) != 2 || fs.enqueued[1] != 2 || fs.holds[1] != 0 {
		t.Fatalf("leftover not queued without a hold: %v %v", fs.enqueued, fs.holds)
	}

	// A failed run leaves its leftovers alone.
	fs.runs[9] = store.Run{ID: 9, IncidentID: 2, Status: store.RunFailed}
	fs.leftover[9] = []int64{1}
	notifier.done = make(chan struct{})
	s.RunFinished(9)
	<-notifier.done
	if len(fs.enqueued) != 2 {
		t.Fatalf("failed run re-queued leftovers: %v", fs.enqueued)
	}
}

func TestNoSlackForARunThatMergedItsIncident(t *testing.T) {
	fs := consolidationFixture()
	notifier := &recordingNotifier{done: make(chan struct{})}
	s := New(fs, Options{}, nil, notifier)
	parent := int64(1)
	fs.incidents[0].ParentID, fs.incidents[0].MergedBy = &parent, "run:8"
	s.RunFinished(8)
	select {
	case <-notifier.done:
		t.Fatal("a run that merged its own incident was posted to Slack")
	case <-time.After(100 * time.Millisecond):
	}

	// Merged by someone else (here, a human): the run's diagnosis still goes out.
	fs.incidents[0].MergedBy = "ui"
	s.RunFinished(8)
	select {
	case <-notifier.done:
	case <-time.After(2 * time.Second):
		t.Fatal("diagnosis not posted")
	}
}

// uiPost submits a form to the UI and returns the status and redirect target.
func uiPost(t *testing.T, s *Server, path string, form url.Values, crossSite bool) (status int, location string) {
	t.Helper()
	srv := httptest.NewServer(s.UIHandler())
	t.Cleanup(srv.Close)
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, srv.URL+path, strings.NewReader(form.Encode()))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if crossSite {
		req.Header.Set("Sec-Fetch-Site", "cross-site")
	} else {
		req.Header.Set("Sec-Fetch-Site", "same-origin")
	}
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	return resp.StatusCode, resp.Header.Get("Location")
}

func TestUIMergeLinkAndUnmerge(t *testing.T) {
	fs := consolidationFixture()
	s := New(fs, Options{}, nil, nil)

	if status, _ := uiPost(t, s, "/incidents/3/merge", url.Values{"into": {"2"}, "reason": {"same outage"}}, true); status != http.StatusForbidden {
		t.Fatalf("cross-site post: %d", status)
	}
	if fs.incident(3).Merged() || len(fs.pr().actions) != 0 {
		t.Fatal("cross-site post had an effect")
	}

	// A human may merge across namespaces.
	status, loc := uiPost(t, s, "/incidents/3/merge", url.Values{"into": {"2"}, "reason": {"same outage"}}, false)
	if status != http.StatusSeeOther || !strings.Contains(loc, "msg=") {
		t.Fatalf("merge: %d %s", status, loc)
	}
	if in := fs.incident(3); in.ParentID == nil || *in.ParentID != 2 || in.MergedBy != "ui" {
		t.Fatalf("incident 3 = %+v", in)
	}

	status, _ = uiPost(t, s, "/incidents/3/unmerge", nil, false)
	if status != http.StatusSeeOther || fs.incident(3).Merged() {
		t.Fatalf("unmerge: %d %+v", status, fs.incident(3))
	}
	if _, loc = uiPost(t, s, "/incidents/3/unmerge", nil, false); !strings.Contains(loc, "error=") {
		t.Fatalf("second unmerge should report an error: %s", loc)
	}

	uiPost(t, s, "/incidents/3/link", url.Values{"other": {"1"}}, false)
	if _, ok := fs.links[linkKey(1, 3)]; !ok || fs.links[linkKey(1, 3)] != "(no reason given)" {
		t.Fatalf("link: %v", fs.links)
	}
	uiPost(t, s, "/incidents/3/unlink", url.Values{"other": {"1"}}, false)
	if len(fs.links) != 0 {
		t.Fatalf("unlink: %v", fs.links)
	}

	acts := fs.pr().actions
	var tools []string
	for _, a := range acts {
		if a.actor != store.ActorUI || a.runID != 0 {
			t.Errorf("UI action not attributed to the UI: %+v", a)
		}
		tools = append(tools, a.tool+":"+a.status)
	}
	want := []string{"merge_incident:succeeded", "unmerge_incident:succeeded", "unmerge_incident:rejected",
		"link_incident:succeeded", "unlink_incident:succeeded"}
	if !slices.Equal(tools, want) {
		t.Fatalf("audit = %v, want %v", tools, want)
	}
	if acts[1].target != 2 {
		t.Errorf("unmerge should name the former parent: %+v", acts[1])
	}
}

func TestIncidentPageShowsConsolidation(t *testing.T) {
	fs := consolidationFixture()
	parent := int64(1)
	fs.incidents[0].ParentID, fs.incidents[0].MergedBy, fs.incidents[0].MergeReason = &parent, "run:7", "same pod OOMKilled"
	fs.runs[7] = store.Run{ID: 7, IncidentID: 3, Status: store.RunSucceeded}
	s := New(fs, Options{}, nil, nil)
	s.now = func() time.Time { return fs.incidents[0].OpenedAt.Add(time.Hour) }
	if _, err := s.store.RecordAction(t.Context(), store.ActionStart{Actor: store.ActorAgent, RunID: 7, IncidentID: 3,
		TargetIncidentID: 1, Tool: "merge_incident", Input: json.RawMessage(`{"reason":"same pod OOMKilled","confidence":"high"}`)}); err != nil {
		t.Fatal(err)
	}
	status, body := do(t, s.UIHandler(), http.MethodGet, "/incidents/3", "")
	if status != http.StatusOK {
		t.Fatalf("status %d", status)
	}
	for _, want := range []string{"Merged into", "/incidents/1", "the agent (run #7)", "same pod OOMKilled", "Unmerge",
		"Activity (1)", "merge_incident", "confidence high", "Link to…"} {
		if !strings.Contains(body, want) {
			t.Errorf("page lacks %q", want)
		}
	}
	// A merged incident offers no "Merge into…" of its own: unmerge first.
	if strings.Contains(body, "Merge into…") {
		t.Error("merged incident offers another merge")
	}

	status, body = do(t, s.UIHandler(), http.MethodGet, "/", "")
	if status != http.StatusOK || !strings.Contains(body, "↳ merged") || !strings.Contains(body, "+1 merged") {
		t.Errorf("home page should fold #3 under #1 (status %d)", status)
	}
}
