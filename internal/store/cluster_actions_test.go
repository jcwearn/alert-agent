package store

import (
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

// runFor queues a run for an incident; cluster actions need a run to cite.
func runFor(t *testing.T, s *Store, incident int64) int64 {
	t.Helper()
	id, _, err := s.EnqueueRun(t.Context(), incident, 0)
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func autonomous(run, incident int64, target string) ClusterAction {
	return ClusterAction{
		Actor: ActorAgent, RunID: run, IncidentID: incident, Tool: "delete_pod", Input: []byte(`{}`),
		Mode: ModeAutonomous, TargetKind: "Deployment", TargetNamespace: "drill", TargetName: target,
		Object: "Pod drill/" + target + "-abc",
	}
}

var drillGuardrails = Guardrails{Cooldown: 6 * time.Hour, MaxPerHour: 3}

func start(t *testing.T, s *Store, a ClusterAction, g Guardrails) (id int64, refusal string) {
	t.Helper()
	id, refusal, err := s.StartClusterAction(t.Context(), a, g)
	if err != nil {
		t.Fatal(err)
	}
	return id, refusal
}

func TestClusterActionOnePerIncidentFamily(t *testing.T) {
	s := openTestStore(t)
	ctx := t.Context()
	parent := incidentIn(t, s, "KubePodCrashLooping", "drill")
	child := incidentIn(t, s, "KubePodNotReady", "drill")
	if _, err := s.MergeIncident(ctx, child, parent, "run:1", "same pod"); err != nil {
		t.Fatal(err)
	}
	run := runFor(t, s, parent)

	id, refusal := start(t, s, autonomous(run, parent, "web"), drillGuardrails)
	if refusal != "" {
		t.Fatalf("first action refused: %s", refusal)
	}
	if err := s.FinishAction(ctx, id, ActionSucceeded, "deleted"); err != nil {
		t.Fatal(err)
	}

	// A second action on the merged child is the same incident.
	_, refusal = start(t, s, autonomous(run, child, "other"), Guardrails{})
	if !strings.Contains(refusal, "already had its one action") || !strings.Contains(refusal, "request_approval") {
		t.Fatalf("second action in the family: %q", refusal)
	}

	// The refusal is in the audit trail, finished.
	acts, err := s.IncidentActions(ctx, child)
	if err != nil {
		t.Fatal(err)
	}
	last := acts[len(acts)-1]
	if last.Status != ActionRejected || last.Mode != ModeAutonomous || last.Target != "Deployment drill/other" || last.FinishedAt == nil {
		t.Errorf("refusal row = %+v", last)
	}
}

func TestClusterActionCooldownEscalatesARecurrence(t *testing.T) {
	s := openTestStore(t)
	ctx := t.Context()
	first := incidentIn(t, s, "KubePodCrashLooping", "drill")
	id, refusal := start(t, s, autonomous(runFor(t, s, first), first, "web"), drillGuardrails)
	if refusal != "" {
		t.Fatal(refusal)
	}
	_ = s.FinishAction(ctx, id, ActionSucceeded, "deleted")
	resolve(t, s, first)

	// The alert fires again: a new incident, same workload.
	again := incidentIn(t, s, "KubePodCrashLooping", "drill")
	_, refusal = start(t, s, autonomous(runFor(t, s, again), again, "web"), drillGuardrails)
	if !strings.Contains(refusal, "recurrence") || !strings.Contains(refusal, "6h0m0s cooldown") {
		t.Fatalf("recurrence: %q", refusal)
	}

	// A different alert on the same workload is caught by the target...
	other := incidentIn(t, s, "KubeDeploymentReplicasMismatch", "drill")
	if _, refusal = start(t, s, autonomous(runFor(t, s, other), other, "web"), drillGuardrails); refusal == "" {
		t.Fatal("same workload, other alert: not refused")
	}
	// ...and the same alert on another workload in the namespace by the alert.
	third := incidentIn(t, s, "KubePodCrashLooping", "drill2")
	if _, refusal = start(t, s, autonomous(runFor(t, s, third), third, "elsewhere"), drillGuardrails); refusal != "" {
		t.Fatalf("unrelated namespace and workload refused: %q", refusal)
	}

	// Outside the window the workload may be acted on again.
	if _, err := s.pool.Exec(ctx, `UPDATE actions SET created_at = now() - interval '7 hours'`); err != nil {
		t.Fatal(err)
	}
	fourth := incidentIn(t, s, "KubePodNotReady", "drill")
	if _, refusal = start(t, s, autonomous(runFor(t, s, fourth), fourth, "web"), drillGuardrails); refusal != "" {
		t.Fatalf("after the cooldown: %q", refusal)
	}
}

func resolve(t *testing.T, s *Store, id int64) {
	t.Helper()
	if _, err := s.pool.Exec(t.Context(), `UPDATE incidents SET alert_status = 'resolved', resolved_at = now() WHERE id = $1`, id); err != nil {
		t.Fatal(err)
	}
}

func TestClusterActionHourlyCapAndPause(t *testing.T) {
	s := openTestStore(t)
	ctx := t.Context()
	g := Guardrails{MaxPerHour: 2}
	for i, ns := range []string{"a", "b"} {
		in := incidentIn(t, s, "Crash", ns)
		a := autonomous(runFor(t, s, in), in, "w"+ns)
		a.TargetNamespace = ns
		if _, refusal := start(t, s, a, g); refusal != "" {
			t.Fatalf("action %d: %s", i, refusal)
		}
	}
	in := incidentIn(t, s, "Crash", "c")
	a := autonomous(runFor(t, s, in), in, "wc")
	if _, refusal := start(t, s, a, g); !strings.Contains(refusal, "hourly cap of 2") {
		t.Fatalf("cap: %q", refusal)
	}

	// Paused: refused before anything else, and a human's approval still runs.
	changed, err := s.SetActionsPaused(ctx, true, ActorUI)
	if err != nil || !changed {
		t.Fatalf("pause: %v %v", changed, err)
	}
	if again, _ := s.SetActionsPaused(ctx, true, ActorUI); again {
		t.Error("pausing twice changed it twice")
	}
	if _, refusal := start(t, s, a, Guardrails{}); !strings.Contains(refusal, "paused") {
		t.Fatalf("paused: %q", refusal)
	}
	approved := a
	approved.Actor, approved.RunID, approved.Mode = ActorUI, 0, ModeApproved
	if _, refusal := start(t, s, approved, g); refusal != "" {
		t.Fatalf("approved action refused: %q", refusal)
	}
	p, err := s.ActionsPause(ctx)
	if err != nil || !p.Paused || p.By != ActorUI {
		t.Fatalf("pause state: %+v %v", p, err)
	}
	if _, err := s.SetActionsPaused(ctx, false, ActorUI); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM actions WHERE tool IN ('pause_actions', 'resume_actions')`).Scan(&n); err != nil || n != 2 {
		t.Errorf("pause audit rows = %d %v", n, err)
	}
}

func TestClusterActionGuardrailsSerialise(t *testing.T) {
	s := openTestStore(t)
	in := incidentIn(t, s, "Crash", "drill")
	run := runFor(t, s, in)
	var wg sync.WaitGroup
	var mu sync.Mutex
	passed := 0
	for range 5 {
		wg.Go(func() {
			_, refusal, err := s.StartClusterAction(t.Context(), autonomous(run, in, "web"), drillGuardrails)
			if err != nil {
				t.Error(err)
				return
			}
			if refusal == "" {
				mu.Lock()
				passed++
				mu.Unlock()
			}
		})
	}
	wg.Wait()
	if passed != 1 {
		t.Errorf("%d concurrent actions passed one-per-incident", passed)
	}
}

func TestPriorClusterActions(t *testing.T) {
	s := openTestStore(t)
	ctx := t.Context()
	in := incidentIn(t, s, "KubePodCrashLooping", "drill")
	id, _ := start(t, s, autonomous(runFor(t, s, in), in, "web"), drillGuardrails)
	_ = s.FinishAction(ctx, id, ActionSucceeded, "DELETE pods drill/web-abc: deleted")
	// A refused one is not prior history.
	_, _ = start(t, s, autonomous(runFor(t, s, in), in, "web"), drillGuardrails)

	got, err := s.PriorClusterActions(ctx, "drill", "KubePodCrashLooping", time.Now().Add(-time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Target != "Deployment drill/web" || got[0].Status != ActionSucceeded || got[0].AlertName != "KubePodCrashLooping" {
		t.Fatalf("prior = %+v", got)
	}
	if none, _ := s.PriorClusterActions(ctx, "elsewhere", "Other", time.Now().Add(-time.Hour)); len(none) != 0 {
		t.Errorf("other namespace sees %+v", none)
	}
}

func TestApprovalLifecycle(t *testing.T) {
	s := openTestStore(t)
	ctx := t.Context()
	in := incidentIn(t, s, "KubePodCrashLooping", "drill")
	run := runFor(t, s, in)
	req := Approval{IncidentID: in, RunID: run, Tool: "delete_pod", Input: []byte(`{"tool":"delete_pod"}`), Reason: "recurred",
		TargetKind: "Deployment", TargetNamespace: "drill", TargetName: "web", Object: "Pod drill/web-abc",
		ExpiresAt: time.Now().Add(2 * time.Hour)}

	first, sup, err := s.RequestApproval(ctx, req)
	if err != nil || len(sup) != 0 {
		t.Fatalf("first: %v %v", sup, err)
	}
	second, sup, err := s.RequestApproval(ctx, req)
	if err != nil || len(sup) != 1 || sup[0] != first {
		t.Fatalf("second should supersede %d: %v %v", first, sup, err)
	}
	if _, err := s.DecideApproval(ctx, first, true, ActorUI); !errors.Is(err, ErrConflict) || !strings.Contains(err.Error(), "superseded") {
		t.Fatalf("approving a superseded request: %v", err)
	}

	pending, err := s.PendingApprovals(ctx)
	if err != nil || len(pending) != 1 || pending[0].ID != second || pending[0].Target() != "Deployment drill/web" {
		t.Fatalf("pending = %+v %v", pending, err)
	}

	got, err := s.DecideApproval(ctx, second, true, ActorUI)
	if err != nil || got.Status != ApprovalApproved || got.DecidedAt == nil {
		t.Fatalf("approve: %+v %v", got, err)
	}
	if _, err := s.DecideApproval(ctx, second, false, ActorUI); !errors.Is(err, ErrConflict) {
		t.Fatalf("deciding twice: %v", err)
	}
	actionID, refusal := start(t, s, ClusterAction{Actor: ActorUI, IncidentID: in, Tool: "delete_pod", Input: req.Input,
		Mode: ModeApproved, TargetKind: "Deployment", TargetNamespace: "drill", TargetName: "web", ApprovalID: second}, Guardrails{})
	if refusal != "" {
		t.Fatal(refusal)
	}
	if err := s.SetApprovalAction(ctx, second, actionID); err != nil {
		t.Fatal(err)
	}
	_ = s.FinishAction(ctx, actionID, ActionSucceeded, "deleted")
	all, err := s.IncidentApprovals(ctx, in)
	if err != nil || len(all) != 2 || all[1].ActionStatus != ActionSucceeded || all[1].ActionResult != "deleted" {
		t.Fatalf("incident approvals = %+v %v", all, err)
	}
}

func TestApprovalsExpire(t *testing.T) {
	s := openTestStore(t)
	ctx := t.Context()
	stale := incidentIn(t, s, "A", "drill")
	resolvedIn := incidentIn(t, s, "B", "drill")
	live := incidentIn(t, s, "C", "drill")
	mk := func(in int64, exp time.Duration) int64 {
		id, _, err := s.RequestApproval(ctx, Approval{IncidentID: in, RunID: runFor(t, s, in), Tool: "delete_pod",
			Input: []byte(`{}`), Reason: "r", ExpiresAt: time.Now().Add(exp)})
		if err != nil {
			t.Fatal(err)
		}
		return id
	}
	staleID := mk(stale, -time.Minute)
	resolvedID := mk(resolvedIn, time.Hour)
	liveID := mk(live, time.Hour)
	resolve(t, s, resolvedIn)

	expired, err := s.ExpireApprovals(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(expired) != 2 || expired[0].ID != staleID || expired[1].ID != resolvedID || expired[0].Status != ApprovalExpired {
		t.Fatalf("expired = %+v", expired)
	}
	if p, _ := s.GetApproval(ctx, liveID); p.Status != ApprovalPending {
		t.Errorf("live approval = %s", p.Status)
	}
}
