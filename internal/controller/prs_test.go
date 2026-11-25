package controller

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/jcwearn/alert-agent/internal/github"
	"github.com/jcwearn/alert-agent/internal/proposal"
	"github.com/jcwearn/alert-agent/internal/runnerapi"
	"github.com/jcwearn/alert-agent/internal/store"
)

// prState is the fakeStore's PR-path tables.
type prState struct {
	mu       sync.Mutex
	actions  []fakeAction
	prs      []store.PullRequest
	sessions map[int64]int
}

type fakeAction struct {
	runID         int64
	actor         string
	incident      int64
	target        int64
	tool, status  string
	input, result string
	finished      bool
}

func (f *fakeStore) pr() *prState {
	if f.prState == nil {
		f.prState = &prState{sessions: map[int64]int{}}
	}
	return f.prState
}

func (f *fakeStore) StartAction(_ context.Context, runID, incidentID int64, tool string, input json.RawMessage) (int64, error) {
	p := f.pr()
	p.mu.Lock()
	defer p.mu.Unlock()
	p.actions = append(p.actions, fakeAction{runID: runID, actor: store.ActorAgent, incident: incidentID, tool: tool,
		status: store.ActionStarted, input: string(input)})
	return int64(len(p.actions)), nil
}

func (f *fakeStore) FinishAction(_ context.Context, id int64, status, result string) error {
	p := f.pr()
	p.mu.Lock()
	defer p.mu.Unlock()
	a := &p.actions[id-1]
	a.status, a.result, a.finished = status, result, true
	return nil
}

func (f *fakeStore) CountActions(_ context.Context, runID int64, tool string) (int, error) {
	p := f.pr()
	p.mu.Lock()
	defer p.mu.Unlock()
	n := 0
	for _, a := range p.actions {
		if a.runID == runID && a.tool == tool {
			n++
		}
	}
	return n, nil
}

func (f *fakeStore) InsertPullRequest(_ context.Context, pr store.PullRequest) (int64, error) {
	p := f.pr()
	p.mu.Lock()
	defer p.mu.Unlock()
	pr.ID = int64(len(p.prs) + 1)
	if pr.CreatedAt.IsZero() {
		pr.CreatedAt = time.Now()
	}
	p.prs = append(p.prs, pr)
	return pr.ID, nil
}

func (f *fakeStore) PullRequestForIncident(_ context.Context, incidentID int64) (store.PullRequest, error) {
	p := f.pr()
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, pr := range p.prs {
		if pr.IncidentID == incidentID {
			return pr, nil
		}
	}
	return store.PullRequest{}, store.ErrNotFound
}

func (f *fakeStore) OpenPullRequests(context.Context) ([]store.PullRequest, error) {
	p := f.pr()
	p.mu.Lock()
	defer p.mu.Unlock()
	var out []store.PullRequest
	for _, pr := range p.prs {
		if pr.State == store.PROpen {
			out = append(out, pr)
		}
	}
	return out, nil
}

func (f *fakeStore) UpdatePullRequest(_ context.Context, id int64, st store.PRStatus) error {
	p := f.pr()
	p.mu.Lock()
	defer p.mu.Unlock()
	pr := &p.prs[id-1]
	pr.State, pr.Checks, pr.HeadSHA = st.State, st.Checks, st.HeadSHA
	return nil
}

func (f *fakeStore) MarkPullRequestNotified(_ context.Context, id int64) (bool, error) {
	p := f.pr()
	p.mu.Lock()
	defer p.mu.Unlock()
	pr := &p.prs[id-1]
	if pr.NotifiedAt != nil {
		return false, nil
	}
	now := time.Now()
	pr.NotifiedAt = &now
	return true, nil
}

func (f *fakeStore) SaveRunSession(_ context.Context, runID int64, _ []byte, size int) error {
	p := f.pr()
	p.mu.Lock()
	defer p.mu.Unlock()
	p.sessions[runID] = size
	return nil
}

type fakeGitHub struct {
	mu      sync.Mutex
	perms   []map[string]string
	created []github.NewPullRequest
	pr      github.PullRequest
	checks  string
}

func (g *fakeGitHub) InstallationToken(_ context.Context, repos []string, perms map[string]string) (github.Token, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if len(repos) != 1 || repos[0] != "k3s-cluster" {
		return github.Token{}, errors.New("token not narrowed to the repo")
	}
	g.perms = append(g.perms, perms)
	return github.Token{Token: "tok"}, nil
}

func (g *fakeGitHub) CreatePullRequest(_ context.Context, _, repo string, pr github.NewPullRequest) (github.PullRequest, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.created = append(g.created, pr)
	out := github.PullRequest{Number: 1200, HTMLURL: "https://github.com/" + repo + "/pull/1200", State: "open"}
	out.Head.SHA = "head1"
	return out, nil
}

func (g *fakeGitHub) GetPullRequest(context.Context, string, string, int) (github.PullRequest, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.pr, nil
}

func (g *fakeGitHub) ChecksRollup(context.Context, string, string, string) (string, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.checks, nil
}

type recordingReviewer struct {
	mu  sync.Mutex
	prs []store.PullRequest
}

func (r *recordingReviewer) NeedsReview(_ context.Context, pr store.PullRequest, _ string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.prs = append(r.prs, pr)
	return nil
}

// prServer is a controller with the PR path on, a fake GitHub, and a
// proposal step that either succeeds or returns prepareErr.
func prServer(t *testing.T, prepareErr error) (*Server, *fakeStore, *fakeGitHub, *recordingReviewer) {
	t.Helper()
	fs := runnerFixture()
	gh := &fakeGitHub{checks: store.ChecksPending}
	rev := &recordingReviewer{}
	s := New(fs, Options{PublicURL: "https://medic.example"}, nil, nil)
	s.EnablePRs(&PRs{
		GitHub: gh,
		Repo:   "jcwearn/k3s-cluster",
		Prepare: func(_ context.Context, _ proposal.Config, token string, req proposal.Request) (*proposal.Commit, error) {
			if token != "tok" {
				t.Errorf("prepare got token %q", token)
			}
			if prepareErr != nil {
				return nil, prepareErr
			}
			return &proposal.Commit{Branch: proposal.BranchName(req.IncidentID), BaseSHA: "base1", HeadSHA: "head1",
				Files: []string{"apps/ebooks/deployment.yaml"}}, nil
		},
		Push: func(context.Context, *proposal.Commit) error { return nil },
	}, rev)
	return s, fs, gh, rev
}

type bearer struct {
	token string
	base  http.RoundTripper
}

func (b bearer) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("Authorization", "Bearer "+b.token)
	return b.base.RoundTrip(r)
}

// mcpSession connects to the controller's tools as a runner would.
func mcpSession(t *testing.T, s *Server, token string) (*mcp.ClientSession, error) {
	t.Helper()
	srv := httptest.NewServer(s.RunnerHandler())
	t.Cleanup(srv.Close)
	tr := &mcp.StreamableClientTransport{
		Endpoint:   srv.URL + runnerapi.PathMCP,
		HTTPClient: &http.Client{Transport: bearer{token: token, base: http.DefaultTransport}},
	}
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "test"}, nil).Connect(t.Context(), tr, nil)
	if err == nil {
		t.Cleanup(func() { _ = cs.Close() })
	}
	return cs, err
}

func callPropose(t *testing.T, cs *mcp.ClientSession, in ProposeInput) (string, bool) {
	t.Helper()
	res, err := cs.CallTool(t.Context(), &mcp.CallToolParams{Name: "propose_pr", Arguments: in})
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

var goodProposal = ProposeInput{
	Title: "fix(ebooks): raise calibre memory limit to 1Gi",
	Body:  "## Why\nOOMKilled three times in an hour.",
	Patch: "diff --git a/apps/ebooks/deployment.yaml b/apps/ebooks/deployment.yaml\n",
}

func TestProposePR(t *testing.T) {
	s, fs, gh, _ := prServer(t, nil)
	cs, err := mcpSession(t, s, "good")
	if err != nil {
		t.Fatal(err)
	}
	tools, err := cs.ListTools(t.Context(), nil)
	if err != nil || len(tools.Tools) != 3 || !hasTool(tools.Tools, "propose_pr") {
		t.Fatalf("tools: %+v %v", tools, err)
	}

	out, isErr := callPropose(t, cs, goodProposal)
	if isErr || !strings.Contains(out, "Opened PR #1200") {
		t.Fatalf("propose: %s", out)
	}

	if len(gh.created) != 1 {
		t.Fatalf("created %d PRs", len(gh.created))
	}
	got := gh.created[0]
	if got.Head != "agent/3" || got.Base != "main" || got.Title != goodProposal.Title {
		t.Errorf("PR = %+v", got)
	}
	for _, want := range []string{"incident #3", "OOMKilled three times", "base1", "`apps/ebooks/deployment.yaml`",
		"https://medic.example/incidents/3", "https://medic.example/runs/7", "session=11111111-2222-3333-4444-555555555555"} {
		if !strings.Contains(got.Body, want) {
			t.Errorf("PR body lacks %q:\n%s", want, got.Body)
		}
	}
	// Tokens are minted per purpose: write contents to push; write PRs, and
	// read the refs (GitHub refuses with 422 otherwise), to open.
	if len(gh.perms) != 2 || gh.perms[0]["contents"] != "write" || len(gh.perms[0]) != 1 ||
		gh.perms[1]["pull_requests"] != "write" || gh.perms[1]["contents"] != "read" || len(gh.perms[1]) != 2 {
		t.Errorf("token permissions = %v", gh.perms)
	}

	p := fs.pr()
	if len(p.prs) != 1 || p.prs[0].Number != 1200 || p.prs[0].RunID != 7 || p.prs[0].IncidentID != 3 {
		t.Errorf("stored PRs = %+v", p.prs)
	}
	if len(p.actions) != 1 || p.actions[0].status != store.ActionSucceeded || !strings.Contains(p.actions[0].input, "raise calibre") {
		t.Errorf("actions = %+v", p.actions)
	}

	// One PR per incident.
	out, isErr = callPropose(t, cs, goodProposal)
	if !isErr || !strings.Contains(out, "already has PR #1200") {
		t.Errorf("second proposal: %s", out)
	}
	if p.actions[1].status != store.ActionRejected {
		t.Errorf("second action = %+v", p.actions[1])
	}
}

func TestProposePRRejections(t *testing.T) {
	s, fs, gh, _ := prServer(t, &proposal.Rejected{Reason: "scripts/render.sh: only files under apps/ or infrastructure/ may be changed"})
	cs, err := mcpSession(t, s, "good")
	if err != nil {
		t.Fatal(err)
	}
	out, isErr := callPropose(t, cs, goodProposal)
	if !isErr || !strings.Contains(out, "Refused: scripts/render.sh") {
		t.Fatalf("want refusal, got %s", out)
	}
	bad := goodProposal
	bad.Title = "Raise the limit"
	if out, isErr := callPropose(t, cs, bad); !isErr || !strings.Contains(out, "conventional commit") {
		t.Errorf("bad title: %s", out)
	}
	// Third attempt is still allowed; the fourth is refused before any work.
	callPropose(t, cs, goodProposal)
	if out, isErr := callPropose(t, cs, goodProposal); !isErr || !strings.Contains(out, "already called 3 times") {
		t.Errorf("fourth attempt: %s", out)
	}
	if len(gh.created) != 0 || len(fs.pr().prs) != 0 {
		t.Error("a rejected proposal opened a PR")
	}
	for i, a := range fs.pr().actions {
		if a.status != store.ActionRejected || !a.finished {
			t.Errorf("action %d = %+v", i, a)
		}
	}
}

func TestProposePRFailureIsNotShown(t *testing.T) {
	s, fs, _, _ := prServer(t, errors.New("clone: token for ghs_secret rejected"))
	cs, err := mcpSession(t, s, "good")
	if err != nil {
		t.Fatal(err)
	}
	out, isErr := callPropose(t, cs, goodProposal)
	if !isErr || strings.Contains(out, "ghs_secret") || !strings.Contains(out, "Do not retry") {
		t.Errorf("failure output: %s", out)
	}
	if a := fs.pr().actions[0]; a.status != store.ActionFailed || !strings.Contains(a.result, "ghs_secret") {
		t.Errorf("the action record should keep the real error: %+v", a)
	}
}

func TestMCPNeedsRunToken(t *testing.T) {
	s, _, _, _ := prServer(t, nil)
	for _, token := range []string{"", "nope", "finished"} {
		if _, err := mcpSession(t, s, token); err == nil {
			t.Errorf("token %q connected", token)
		}
	}
}

func TestRunnerContextAnnouncesPRs(t *testing.T) {
	s, _, _, _ := prServer(t, nil)
	_, body := runnerReq(t, s.RunnerHandler(), http.MethodGet, runnerapi.PathContext, "good", "")
	var rc runnerapi.Context
	if err := json.Unmarshal([]byte(body), &rc); err != nil || !rc.ProposePR {
		t.Errorf("propose_pr not announced: %s", body)
	}
	plain := New(runnerFixture(), Options{}, nil, nil)
	_, body = runnerReq(t, plain.RunnerHandler(), http.MethodGet, runnerapi.PathContext, "good", "")
	if strings.Contains(body, `"propose_pr":true`) {
		t.Errorf("propose_pr announced with the PR path off: %s", body)
	}
}

func TestPollPRs(t *testing.T) {
	s, fs, gh, rev := prServer(t, nil)
	if _, err := fs.InsertPullRequest(t.Context(), store.PullRequest{IncidentID: 3, RunID: 7, Number: 1200,
		URL: "https://github.com/jcwearn/k3s-cluster/pull/1200", State: store.PROpen, Checks: store.ChecksPending}); err != nil {
		t.Fatal(err)
	}
	gh.pr = github.PullRequest{Number: 1200, State: "open"}
	gh.pr.Head.SHA = "head1"

	// Checks still running and the PR is young: no push yet.
	if err := s.PollPRsOnce(t.Context()); err != nil {
		t.Fatal(err)
	}
	if len(rev.prs) != 0 {
		t.Fatal("notified before checks settled")
	}
	if p := gh.perms[len(gh.perms)-1]; p["pull_requests"] != "read" || p["checks"] != "read" || p["contents"] != "read" || len(p) != 3 {
		t.Errorf("poll token permissions = %v", p)
	}

	gh.checks = store.ChecksSuccess
	for range 2 {
		if err := s.PollPRsOnce(t.Context()); err != nil {
			t.Fatal(err)
		}
	}
	if len(rev.prs) != 1 || rev.prs[0].Checks != store.ChecksSuccess {
		t.Fatalf("want exactly one review push with checks=success, got %+v", rev.prs)
	}

	gh.pr.Merged, gh.pr.State = true, "closed"
	if err := s.PollPRsOnce(t.Context()); err != nil {
		t.Fatal(err)
	}
	if pr := fs.pr().prs[0]; pr.State != store.PRMerged {
		t.Errorf("state = %s", pr.State)
	}
	if open, _ := fs.OpenPullRequests(t.Context()); len(open) != 0 {
		t.Error("merged PR still polled")
	}
}

func TestPollPRsNotifiesStragglers(t *testing.T) {
	s, fs, gh, rev := prServer(t, nil)
	if _, err := fs.InsertPullRequest(t.Context(), store.PullRequest{IncidentID: 3, RunID: 7, Number: 1200,
		State: store.PROpen, Checks: store.ChecksPending, CreatedAt: time.Now().Add(-20 * time.Minute)}); err != nil {
		t.Fatal(err)
	}
	gh.pr = github.PullRequest{Number: 1200, State: "open"}
	if err := s.PollPRsOnce(t.Context()); err != nil {
		t.Fatal(err)
	}
	if len(rev.prs) != 1 || rev.prs[0].Checks != store.ChecksPending {
		t.Errorf("a PR whose checks never finish should still be announced: %+v", rev.prs)
	}
}

func TestNtfyReviewer(t *testing.T) {
	var got ntfyMessage
	var auth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth = r.Header.Get("Authorization")
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		_ = json.NewDecoder(r.Body).Decode(&got)
	}))
	t.Cleanup(srv.Close)
	n := &NtfyReviewer{BaseURL: srv.URL, Topic: "medic", Token: "tk_x"}
	pr := store.PullRequest{IncidentID: 3, Number: 1200, URL: "https://github.com/o/r/pull/1200", Title: "fix(ebooks): raise limit",
		Checks: store.ChecksFailure, AlertName: "KubeContainerOOMKilled", Namespace: "ebooks"}
	if err := n.NeedsReview(t.Context(), pr, "https://medic.example/incidents/3"); err != nil {
		t.Fatal(err)
	}
	if auth != "Bearer tk_x" || got.Topic != "medic" || got.Click != pr.URL || !strings.Contains(got.Title, "PR #1200") ||
		!strings.Contains(got.Message, "CI FAILED") || len(got.Actions) != 2 {
		t.Errorf("message = %+v (auth %q)", got, auth)
	}
}

func TestRunnerSession(t *testing.T) {
	s, fs, _, _ := prServer(t, nil)
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	_, _ = io.WriteString(zw, `{"type":"user"}`+"\n")
	_ = zw.Close()
	if status, body := runnerReq(t, s.RunnerHandler(), http.MethodPost, runnerapi.PathSession, "good", buf.String()); status != http.StatusNoContent {
		t.Fatalf("status = %d: %s", status, body)
	}
	if fs.pr().sessions[7] != 16 {
		t.Errorf("sessions = %v", fs.pr().sessions)
	}
	if status, _ := runnerReq(t, s.RunnerHandler(), http.MethodPost, runnerapi.PathSession, "good", "not gzip"); status != http.StatusBadRequest {
		t.Errorf("non-gzip status = %d", status)
	}
}

func TestSlackMessageLinksPR(t *testing.T) {
	pr := &store.PullRequest{Number: 1200, URL: "https://github.com/o/r/pull/1200", Title: "fix(x): <y>"}
	msg := slackMessage("https://ui.example", store.Incident{ID: 1, AlertName: "X"},
		store.Run{ID: 1, Status: store.RunSucceeded, Report: "## Summary\nok"}, pr)
	b, _ := json.Marshal(msg)
	if !strings.Contains(string(b), "pull/1200|PR #1200") || strings.Contains(string(b), "<y>") {
		t.Errorf("slack message: %s", b)
	}
}

func TestUIShowsAwaitingReview(t *testing.T) {
	s, fs, _, _ := prServer(t, nil)
	if _, err := fs.InsertPullRequest(t.Context(), store.PullRequest{IncidentID: 3, RunID: 7, Number: 1200, URL: "https://github.com/o/r/pull/1200",
		Title: "fix(ebooks): raise limit", Branch: "agent/3", State: store.PROpen, Checks: store.ChecksSuccess, AlertName: "KubePodCrashLooping"}); err != nil {
		t.Fatal(err)
	}
	_, home := do(t, s.UIHandler(), http.MethodGet, "/", "")
	if !strings.Contains(home, "Awaiting review (1)") || !strings.Contains(home, "fix(ebooks): raise limit") {
		t.Errorf("home page lacks the PR:\n%s", home)
	}
	_, detail := do(t, s.UIHandler(), http.MethodGet, "/incidents/3", "")
	if !strings.Contains(detail, "Pull request") || !strings.Contains(detail, "agent/3") {
		t.Errorf("incident page lacks the PR:\n%s", detail)
	}
}
