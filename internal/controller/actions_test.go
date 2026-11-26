package controller

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jcwearn/alert-agent/internal/actions"
	"github.com/jcwearn/alert-agent/internal/store"
)

// actState is the fake store's Phase 5 model: enough to see what the
// controller records and asks for, with the guardrails' verdict scripted.
type actState struct {
	mu        sync.Mutex
	cluster   map[int64]store.ClusterAction // by action id
	refusal   string                        // what StartClusterAction's guardrails say
	prior     []store.PriorAction
	approvals []store.Approval
	paused    bool
}

func (f *fakeStore) act() *actState {
	if f.actState == nil {
		f.actState = &actState{cluster: map[int64]store.ClusterAction{}}
	}
	return f.actState
}

func (f *fakeStore) StartClusterAction(_ context.Context, a store.ClusterAction, _ store.Guardrails) (id int64, refusal string, err error) {
	st, p := f.act(), f.pr()
	st.mu.Lock()
	defer st.mu.Unlock()
	if a.Mode == store.ModeAutonomous {
		refusal = st.refusal
	}
	p.mu.Lock()
	fa := fakeAction{runID: a.RunID, actor: a.Actor, incident: a.IncidentID, tool: a.Tool, status: store.ActionStarted, input: string(a.Input)}
	if refusal != "" {
		fa.status, fa.result, fa.finished = store.ActionRejected, refusal, true
	}
	p.actions = append(p.actions, fa)
	id = int64(len(p.actions))
	p.mu.Unlock()
	st.cluster[id] = a
	return id, refusal, nil
}

func (f *fakeStore) PriorClusterActions(context.Context, string, string, time.Time) ([]store.PriorAction, error) {
	return f.act().prior, nil
}

func (f *fakeStore) RequestApproval(_ context.Context, ap store.Approval) (id int64, sup []int64, err error) {
	st := f.act()
	st.mu.Lock()
	defer st.mu.Unlock()
	for i := range st.approvals {
		if st.approvals[i].IncidentID == ap.IncidentID && st.approvals[i].Status == store.ApprovalPending {
			st.approvals[i].Status = store.ApprovalSuperseded
			sup = append(sup, st.approvals[i].ID)
		}
	}
	ap.ID, ap.Status = int64(len(st.approvals)+1), store.ApprovalPending
	st.approvals = append(st.approvals, ap)
	return ap.ID, sup, nil
}

func (f *fakeStore) DecideApproval(_ context.Context, id int64, approve bool, by string) (store.Approval, error) {
	st := f.act()
	st.mu.Lock()
	defer st.mu.Unlock()
	if id < 1 || int(id) > len(st.approvals) {
		return store.Approval{}, store.ErrNotFound
	}
	ap := &st.approvals[id-1]
	if ap.Status != store.ApprovalPending {
		return *ap, fmt.Errorf("%w: approval #%d is %s", store.ErrConflict, id, ap.Status)
	}
	ap.Status, ap.DecidedBy = store.ApprovalRejected, by
	if approve {
		ap.Status = store.ApprovalApproved
	}
	return *ap, nil
}

func (f *fakeStore) SetApprovalAction(_ context.Context, id, actionID int64) error {
	st := f.act()
	st.mu.Lock()
	defer st.mu.Unlock()
	st.approvals[id-1].ActionID = &actionID
	return nil
}

func (f *fakeStore) ExpireApprovals(context.Context) ([]store.Approval, error) { return nil, nil }

func (f *fakeStore) PendingApprovals(context.Context) ([]store.Approval, error) {
	st := f.act()
	st.mu.Lock()
	defer st.mu.Unlock()
	var out []store.Approval
	for _, ap := range st.approvals {
		if ap.Status == store.ApprovalPending {
			out = append(out, ap)
		}
	}
	return out, nil
}

func (f *fakeStore) IncidentApprovals(_ context.Context, id int64) ([]store.Approval, error) {
	st := f.act()
	st.mu.Lock()
	defer st.mu.Unlock()
	var out []store.Approval
	for _, ap := range st.approvals {
		if ap.IncidentID == id {
			out = append(out, ap)
		}
	}
	return out, nil
}

func (f *fakeStore) ActionsPause(context.Context) (store.Pause, error) {
	return store.Pause{Paused: f.act().paused}, nil
}

func (f *fakeStore) SetActionsPaused(_ context.Context, paused bool, _ string) (bool, error) {
	st := f.act()
	changed := st.paused != paused
	st.paused = paused
	return changed, nil
}

// fakeActor records every call; err (when set) answers the dry run or the
// real call.
type fakeActor struct {
	mu      sync.Mutex
	calls   []actorCall
	dryErr  error
	realErr error
}

type actorCall struct {
	req  actions.Request
	opts actions.Options
}

func (a *fakeActor) Do(_ context.Context, req actions.Request, opts actions.Options) (actions.Result, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.calls = append(a.calls, actorCall{req, opts})
	if opts.DryRun && a.dryErr != nil {
		return actions.Result{}, a.dryErr
	}
	if !opts.DryRun && a.realErr != nil {
		return actions.Result{}, a.realErr
	}
	obj := actions.Object{Kind: "Pod", Namespace: req.Namespace, Name: req.Name}
	call := "DELETE pods " + req.Namespace + "/" + req.Name + " as alert-agent-actor: deleted"
	if opts.DryRun {
		call += " (dry run)"
	}
	return actions.Result{Target: actions.Object{Kind: "Deployment", Namespace: req.Namespace, Name: "calibre"}, Object: obj, Call: call}, nil
}

func (a *fakeActor) realCalls() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	n := 0
	for _, c := range a.calls {
		if !c.opts.DryRun {
			n++
		}
	}
	return n
}

type fakeApprovalNotifier struct {
	got chan store.Approval
}

func (n *fakeApprovalNotifier) ApprovalRequested(_ context.Context, ap store.Approval, _ store.Incident, _ string) error {
	n.got <- ap
	return nil
}

// actionServer is run 7 on incident 3 in ebooks (autonomous), with loki
// approval-only.
func actionServer(t *testing.T) (*Server, *fakeStore, *fakeActor, *fakeApprovalNotifier) {
	t.Helper()
	fs := consolidationFixture()
	actor := &fakeActor{}
	notifier := &fakeApprovalNotifier{got: make(chan store.Approval, 4)}
	s := New(fs, Options{RecentWindow: 30 * time.Minute, PublicURL: "https://ui.example"}, nil, nil)
	s.now = func() time.Time { return fs.incidents[0].OpenedAt.Add(time.Minute) }
	s.EnableActions(&Actions{
		Actor: actor, Autonomous: map[string]bool{"ebooks": true},
		Guardrails:  store.Guardrails{Cooldown: 6 * time.Hour, MaxPerHour: 3},
		ApprovalTTL: 2 * time.Hour, Notify: []ApprovalNotifier{notifier},
	})
	return s, fs, actor, notifier
}

func TestActionToolsServedOnlyWhenEnabled(t *testing.T) {
	s, _, _, _ := actionServer(t)
	cs, err := mcpSession(t, s, "good")
	if err != nil {
		t.Fatal(err)
	}
	tools, err := cs.ListTools(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range append(append([]string{}, actions.Tools...), "request_approval") {
		if !hasTool(tools.Tools, name) {
			t.Errorf("%s not served", name)
		}
	}

	_, _, plain := consolidationServer(t)
	tools, err = plain.ListTools(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if hasTool(tools.Tools, "delete_pod") || hasTool(tools.Tools, "request_approval") {
		t.Error("actions served with ACTIONS_ENABLED off")
	}
}

func TestAutonomousActionRecordsThenActs(t *testing.T) {
	s, fs, actor, _ := actionServer(t)
	cs, _ := mcpSession(t, s, "good")
	out, isErr := call(t, cs, "delete_pod", map[string]any{"namespace": "ebooks", "name": "calibre-7c9f-abc", "reason": "stale lock in emptyDir"})
	if isErr || !strings.Contains(out, "Done: DELETE pods ebooks/calibre-7c9f-abc as alert-agent-actor: deleted") {
		t.Fatalf("delete_pod: %v %s", isErr, out)
	}
	if len(actor.calls) != 2 || !actor.calls[0].opts.DryRun || actor.calls[1].opts.DryRun || actor.calls[1].opts.IncidentID != 3 {
		t.Fatalf("actor calls = %+v", actor.calls)
	}
	ca := fs.act().cluster[1]
	if ca.Mode != store.ModeAutonomous || ca.TargetKind != "Deployment" || ca.TargetName != "calibre" || ca.Object != "Pod ebooks/calibre-7c9f-abc" || ca.RunID != 7 {
		t.Errorf("recorded = %+v", ca)
	}
	a := fs.pr().actions[0]
	if a.status != store.ActionSucceeded || !strings.HasPrefix(a.result, "DELETE pods") || !strings.Contains(a.input, "stale lock") {
		t.Errorf("audit row = %+v", a)
	}
}

func TestApprovalOnlyNamespaceNeverCallsTheActor(t *testing.T) {
	s, fs, actor, _ := actionServer(t)
	cs, _ := mcpSession(t, s, "good")
	out, isErr := call(t, cs, "rollout_restart", map[string]any{"kind": "Deployment", "namespace": "loki", "name": "loki", "reason": "wedged"})
	if !isErr || !strings.Contains(out, "approval-only") || !strings.Contains(out, "request_approval") {
		t.Fatalf("%v %s", isErr, out)
	}
	if len(actor.calls) != 0 {
		t.Errorf("actor called: %+v", actor.calls)
	}
	if a := fs.pr().actions[0]; a.status != store.ActionRejected || a.tool != "rollout_restart" {
		t.Errorf("refusal not audited: %+v", a)
	}

	// flux_reconcile is not a workload action: autonomous anywhere RBAC allows.
	out, isErr = call(t, cs, "flux_reconcile", map[string]any{"kind": "HelmRelease", "namespace": "loki", "name": "loki", "reason": "stalled"})
	if isErr {
		t.Fatalf("flux_reconcile in loki: %s", out)
	}
}

func TestGuardrailRefusalStopsAfterTheDryRun(t *testing.T) {
	s, fs, actor, _ := actionServer(t)
	fs.act().refusal = "this looks like a recurrence: action #1 delete_pod. Do not retry. Escalate: call request_approval"
	cs, _ := mcpSession(t, s, "good")
	out, isErr := call(t, cs, "delete_pod", map[string]any{"namespace": "ebooks", "name": "calibre-x", "reason": "again"})
	if !isErr || !strings.Contains(out, "recurrence") {
		t.Fatalf("%v %s", isErr, out)
	}
	if actor.realCalls() != 0 {
		t.Error("the action ran past a guardrail refusal")
	}
}

func TestForbiddenDryRunSaysApprovalWontHelp(t *testing.T) {
	s, _, actor, _ := actionServer(t)
	actor.dryErr = &actions.Refusal{Forbidden: true, Reason: "the actor is not permitted to delete Pod ebooks/x"}
	cs, _ := mcpSession(t, s, "good")
	out, isErr := call(t, cs, "delete_pod", map[string]any{"namespace": "ebooks", "name": "x", "reason": "r"})
	if !isErr || !strings.Contains(out, "request_approval would be refused too") {
		t.Fatalf("%v %s", isErr, out)
	}
}

func TestRequestApprovalThenApproveOnce(t *testing.T) {
	s, fs, actor, notifier := actionServer(t)
	cs, _ := mcpSession(t, s, "good")
	out, isErr := call(t, cs, "request_approval", map[string]any{
		"action": "rollout_restart", "kind": "Deployment", "namespace": "loki", "name": "loki", "reason": "recurred after a restart"})
	if isErr || !strings.Contains(out, "approval #1 on incident #3") {
		t.Fatalf("request_approval: %v %s", isErr, out)
	}
	if actor.realCalls() != 0 {
		t.Fatal("requesting approval acted")
	}
	select {
	case ap := <-notifier.got:
		if ap.Tool != "rollout_restart" || ap.IncidentID != 3 || ap.Reason != "recurred after a restart" {
			t.Errorf("notified = %+v", ap)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("owner not notified")
	}

	srv := httptest.NewServer(s.UIHandler())
	t.Cleanup(srv.Close)
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	post := func(path string) string {
		resp, err := client.PostForm(srv.URL+path, url.Values{})
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusSeeOther {
			t.Fatalf("%s: %s", path, resp.Status)
		}
		return resp.Header.Get("Location")
	}
	loc := post("/approvals/1/approve")
	if !strings.HasPrefix(loc, "/incidents/3?") || !strings.Contains(loc, "Approved+%231") {
		t.Fatalf("approve redirect = %s", loc)
	}
	if actor.realCalls() != 1 {
		t.Fatalf("approved action ran %d times", actor.realCalls())
	}
	var ca store.ClusterAction
	for _, c := range fs.act().cluster {
		ca = c
	}
	if ca.Mode != store.ModeApproved || ca.Actor != store.ActorUI || ca.ApprovalID != 1 {
		t.Errorf("approved action recorded as %+v", ca)
	}
	var req actions.Request
	_ = json.Unmarshal(ca.Input, &req)
	if req.Tool != "rollout_restart" || req.Kind != "Deployment" {
		t.Errorf("approved input = %s", ca.Input)
	}

	// A second click is refused without acting.
	loc = post("/approvals/1/approve")
	if !strings.Contains(loc, "error=") || actor.realCalls() != 1 {
		t.Errorf("second approve: %s, %d real calls", loc, actor.realCalls())
	}
}

func TestRejectAndPause(t *testing.T) {
	s, fs, actor, _ := actionServer(t)
	cs, _ := mcpSession(t, s, "good")
	if out, isErr := call(t, cs, "request_approval", map[string]any{"action": "delete_pod", "namespace": "ebooks", "name": "x", "reason": "r"}); isErr {
		t.Fatal(out)
	}
	srv := httptest.NewServer(s.UIHandler())
	t.Cleanup(srv.Close)
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}

	resp, err := client.PostForm(srv.URL+"/approvals/1/reject", url.Values{})
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if !strings.Contains(resp.Header.Get("Location"), "Rejected") || actor.realCalls() != 0 {
		t.Errorf("reject: %s", resp.Header.Get("Location"))
	}

	resp, err = client.PostForm(srv.URL+"/actions/pause", url.Values{"back": {"//evil.example/"}})
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.Header.Get("Location") != "/" || !fs.act().paused {
		t.Errorf("pause: %s paused=%v", resp.Header.Get("Location"), fs.act().paused)
	}
}

func TestRunContextCarriesPriorActions(t *testing.T) {
	s, fs, _, _ := actionServer(t)
	fs.act().prior = []store.PriorAction{{ID: 9, IncidentID: 1, AlertName: "KubePodCrashLooping", Tool: "delete_pod",
		Mode: store.ModeAutonomous, Target: "Deployment ebooks/calibre", Status: store.ActionSucceeded, Result: "DELETE pods\nmore"}}
	fs.act().paused = true
	rc, err := s.buildContext(t.Context(), fs.runs[7])
	if err != nil {
		t.Fatal(err)
	}
	if !rc.Actions || !rc.ActionsPaused || len(rc.AutonomousNamespaces) != 1 || len(rc.PriorActions) != 1 || rc.PriorActions[0].Result != "DELETE pods" {
		t.Errorf("context = actions %v paused %v ns %v prior %+v", rc.Actions, rc.ActionsPaused, rc.AutonomousNamespaces, rc.PriorActions)
	}
}

func TestLocalPath(t *testing.T) {
	for in, want := range map[string]string{"/incidents/3": "/incidents/3", "": "/", "https://x": "/", "//x": "/", `/\x`: "/"} {
		if got := localPath(in); got != want {
			t.Errorf("localPath(%q) = %q", in, got)
		}
	}
}

func TestChatRequestsApprovalButNeverActs(t *testing.T) {
	s, _ := threadServer(t)
	s.EnableActions(&Actions{Actor: &fakeActor{}, Autonomous: map[string]bool{"ebooks": true}, ApprovalTTL: time.Hour})

	for token, want := range map[string]bool{"incident": true, "general": false} {
		cs, err := mcpSession(t, s, token)
		if err != nil {
			t.Fatal(err)
		}
		tools, err := cs.ListTools(t.Context(), nil)
		if err != nil {
			t.Fatal(err)
		}
		if got := hasTool(tools.Tools, "request_approval"); got != want {
			t.Errorf("%s thread: request_approval served = %v", token, got)
		}
		for _, tool := range actions.Tools {
			if hasTool(tools.Tools, tool) {
				t.Errorf("%s thread serves %s", token, tool)
			}
		}
		if rc := runnerContextFor(t, s, token); rc.RequestApproval != want || rc.Actions {
			t.Errorf("%s context: request_approval %v actions %v", token, rc.RequestApproval, rc.Actions)
		}
	}
}
