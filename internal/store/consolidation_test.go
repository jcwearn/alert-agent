package store

import (
	"errors"
	"testing"
	"time"

	"github.com/jcwearn/alert-agent/internal/alertmanager"
)

// incidentIn opens a firing incident for alertname in namespace.
func incidentIn(t *testing.T, s *Store, alertname, namespace string) int64 {
	t.Helper()
	p := alertmanager.Payload{
		Version:  "4",
		GroupKey: `{}:{alertname="` + alertname + `", namespace="` + namespace + `"}`,
		Status:   "firing",
		CommonLabels: map[string]string{
			"alertname": alertname, "namespace": namespace, "severity": "warning",
		},
		Alerts: []alertmanager.Alert{{
			Status: "firing", Fingerprint: alertname + namespace,
			Labels:   map[string]string{"alertname": alertname, "namespace": namespace, "pod": "web-1"},
			StartsAt: time.Date(2026, 11, 24, 10, 0, 0, 0, time.UTC),
		}},
	}
	res := record(t, s, p)
	if !res.Created {
		t.Fatalf("incident not created: %+v", res)
	}
	return res.IncidentID
}

func endHolds(t *testing.T, s *Store) {
	t.Helper()
	if _, err := s.pool.Exec(t.Context(),
		`UPDATE runs SET hold_until = now() - interval '1 second' WHERE hold_until IS NOT NULL`); err != nil {
		t.Fatal(err)
	}
}

func TestHoldBatchesSameNamespace(t *testing.T) {
	s := openTestStore(t)
	ctx := t.Context()
	oom := incidentIn(t, s, "OOM", "drill")
	crash := incidentIn(t, s, "CrashLoop", "drill")
	other := incidentIn(t, s, "CertExpiry", "web")

	run1, batched, err := s.EnqueueRun(ctx, oom, time.Minute)
	if err != nil || batched {
		t.Fatalf("first: %v %v", batched, err)
	}
	run2, batched, err := s.EnqueueRun(ctx, crash, time.Minute)
	if err != nil || !batched || run2 != run1 {
		t.Fatalf("same namespace should join run %d: got %d batched=%v err=%v", run1, run2, batched, err)
	}
	run3, batched, err := s.EnqueueRun(ctx, other, time.Minute)
	if err != nil || batched || run3 == run1 {
		t.Fatalf("other namespace should get its own run: %d %v %v", run3, batched, err)
	}

	// Held: nothing to claim yet.
	if _, ok, err := s.ClaimNextRun(ctx, []byte("h"), "j-", "m"); err != nil || ok {
		t.Fatalf("held run claimed: ok=%v err=%v", ok, err)
	}

	endHolds(t, s)
	// After the hold, a new incident starts a fresh run.
	late := incidentIn(t, s, "NotReady", "drill")
	run4, batched, err := s.EnqueueRun(ctx, late, time.Minute)
	if err != nil || batched || run4 == run1 {
		t.Fatalf("expired hold must not take new members: %d %v %v", run4, batched, err)
	}

	run, ok, err := s.ClaimNextRun(ctx, []byte("h"), "j-", "m")
	if err != nil || !ok || run.ID != run1 || run.IncidentID != oom {
		t.Fatalf("claim: %+v %v %v", run, ok, err)
	}
	members, err := s.RunIncidents(ctx, run1)
	if err != nil || len(members) != 2 || members[0] != oom || members[1] != crash {
		t.Fatalf("members = %v, %v", members, err)
	}
}

func TestClaimRepointsPrimaryAndSkipsDeadBatches(t *testing.T) {
	s := openTestStore(t)
	ctx := t.Context()
	a := incidentIn(t, s, "A", "drill")
	b := incidentIn(t, s, "B", "drill")
	c := incidentIn(t, s, "C", "web")
	runAB, _, _ := s.EnqueueRun(ctx, a, time.Minute)
	if _, _, err := s.EnqueueRun(ctx, b, time.Minute); err != nil {
		t.Fatal(err)
	}
	runC, _, _ := s.EnqueueRun(ctx, c, time.Minute)

	// A human merges a into c during the hold, and c is then merged into b.
	if _, err := s.MergeIncident(ctx, a, c, "ui", "same"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.MergeIncident(ctx, c, b, "ui", "same"); err != nil {
		t.Fatal(err)
	}
	endHolds(t, s)

	run, ok, err := s.ClaimNextRun(ctx, []byte("h"), "j-", "m")
	if err != nil || !ok || run.ID != runAB || run.IncidentID != b {
		t.Fatalf("run %d should be claimed with primary %d: %+v %v %v", runAB, b, run, ok, err)
	}
	skipped, err := s.GetRun(ctx, runC)
	if err != nil || skipped.Status != RunSkipped {
		t.Fatalf("run for merged incident should be skipped: %+v %v", skipped, err)
	}
	// a moved with c: merges stay one level deep.
	in, _, err := s.GetIncident(ctx, a)
	if err != nil || in.ParentID == nil || *in.ParentID != b {
		t.Fatalf("a should now be under b: %+v %v", in.ParentID, err)
	}
}

func TestMergeRules(t *testing.T) {
	s := openTestStore(t)
	ctx := t.Context()
	parent := incidentIn(t, s, "OOM", "drill")
	child := incidentIn(t, s, "CrashLoop", "drill")
	third := incidentIn(t, s, "NotReady", "drill")

	if _, err := s.MergeIncident(ctx, child, child, "ui", "x"); !errors.Is(err, ErrConflict) {
		t.Fatalf("self-merge: %v", err)
	}
	if _, err := s.MergeIncident(ctx, child, 9999, "ui", "x"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing target: %v", err)
	}
	root, err := s.MergeIncident(ctx, child, parent, "run:7", "same pod")
	if err != nil || root != parent {
		t.Fatalf("merge: %d %v", root, err)
	}
	// Into a child resolves to its root.
	if root, err := s.MergeIncident(ctx, third, child, "ui", "same"); err != nil || root != parent {
		t.Fatalf("merge into child should land on root %d: %d %v", parent, root, err)
	}
	// Merging the root into its own child would loop.
	if _, err := s.MergeIncident(ctx, parent, child, "ui", "x"); !errors.Is(err, ErrConflict) {
		t.Fatalf("cycle: %v", err)
	}

	in, _, err := s.GetIncident(ctx, child)
	if err != nil || in.MergedBy != "run:7" || in.MergeReason != "same pod" || in.MergedAt == nil || !in.Merged() {
		t.Fatalf("merge metadata: %+v %v", in, err)
	}
	kids, err := s.IncidentChildren(ctx, parent)
	if err != nil || len(kids) != 2 {
		t.Fatalf("children: %+v %v", kids, err)
	}

	former, err := s.UnmergeIncident(ctx, child)
	if err != nil || former != parent {
		t.Fatalf("unmerge: %d %v", former, err)
	}
	if _, err := s.UnmergeIncident(ctx, child); !errors.Is(err, ErrNotFound) {
		t.Fatalf("second unmerge: %v", err)
	}
	in, _, _ = s.GetIncident(ctx, child)
	if in.Merged() || in.MergedBy != "" || in.MergedAt != nil {
		t.Fatalf("unmerge left metadata: %+v", in)
	}
}

func TestLinksAndAudit(t *testing.T) {
	s := openTestStore(t)
	ctx := t.Context()
	a := incidentIn(t, s, "A", "drill")
	b := incidentIn(t, s, "B", "drill")

	created, err := s.LinkIncidents(ctx, b, a, "same node", ActorUI, 0)
	if err != nil || !created {
		t.Fatalf("link: %v %v", created, err)
	}
	created, err = s.LinkIncidents(ctx, a, b, "again", ActorUI, 0)
	if err != nil || created {
		t.Fatalf("relink should be a no-op: %v %v", created, err)
	}
	links, err := s.IncidentLinks(ctx, a)
	if err != nil || len(links) != 1 || links[0].Other.ID != b || links[0].Reason != "same node" {
		t.Fatalf("links of a: %+v %v", links, err)
	}
	in, _, _ := s.GetIncident(ctx, b)
	if len(in.Links) != 1 || in.Links[0] != a {
		t.Fatalf("b's link ids: %v", in.Links)
	}

	id, err := s.RecordAction(ctx, ActionStart{Actor: ActorUI, IncidentID: b, TargetIncidentID: a,
		Tool: "link_incident", Input: []byte(`{"reason":"same node"}`)})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.FinishAction(ctx, id, ActionSucceeded, "linked"); err != nil {
		t.Fatal(err)
	}
	for _, who := range []int64{a, b} {
		acts, err := s.IncidentActions(ctx, who)
		if err != nil || len(acts) != 1 || acts[0].Actor != ActorUI || acts[0].RunID != nil || acts[0].Status != ActionSucceeded {
			t.Fatalf("audit of #%d: %+v %v", who, acts, err)
		}
	}
	// An agent entry must name its run.
	if _, err := s.RecordAction(ctx, ActionStart{Actor: ActorAgent, IncidentID: a, Tool: "x", Input: []byte(`{}`)}); err == nil {
		t.Fatal("agent action without a run was accepted")
	}

	if ok, err := s.UnlinkIncidents(ctx, a, b); err != nil || !ok {
		t.Fatalf("unlink: %v %v", ok, err)
	}
	if ok, _ := s.UnlinkIncidents(ctx, a, b); ok {
		t.Fatal("second unlink reported a removal")
	}
}

func TestLeftoverIncidentsAndRecent(t *testing.T) {
	s := openTestStore(t)
	ctx := t.Context()
	primary := incidentIn(t, s, "OOM", "drill")
	dup := incidentIn(t, s, "CrashLoop", "drill")
	unrelated := incidentIn(t, s, "JobFailed", "drill")
	runID, _, _ := s.EnqueueRun(ctx, primary, time.Minute)
	for _, id := range []int64{dup, unrelated} {
		if _, _, err := s.EnqueueRun(ctx, id, time.Minute); err != nil {
			t.Fatal(err)
		}
	}
	endHolds(t, s)
	if _, ok, err := s.ClaimNextRun(ctx, []byte("h"), "j-", "m"); err != nil || !ok {
		t.Fatalf("claim: %v %v", ok, err)
	}
	if _, err := s.MergeIncident(ctx, dup, primary, "run:1", "same pod"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.FinishRun(ctx, runID, RunResult{Status: RunSucceeded, Report: "## Summary\nOOM"}); err != nil {
		t.Fatal(err)
	}

	left, err := s.LeftoverIncidents(ctx, runID)
	if err != nil || len(left) != 1 || left[0] != unrelated {
		t.Fatalf("leftovers = %v, %v", left, err)
	}
	// Once it has its own run, it is no longer left over.
	if _, _, err := s.EnqueueRun(ctx, unrelated, 0); err != nil {
		t.Fatal(err)
	}
	if left, _ := s.LeftoverIncidents(ctx, runID); len(left) != 0 {
		t.Fatalf("leftover with a queued run: %v", left)
	}

	recent, err := s.RecentIncidents(ctx, time.Now().Add(-30*time.Minute), 20)
	if err != nil || len(recent) != 3 {
		t.Fatalf("recent: %d %v", len(recent), err)
	}
	for _, r := range recent {
		if r.ID == primary && (r.Report != "## Summary\nOOM" || len(r.Labels) != 1 || r.Labels[0]["pod"] != "web-1") {
			t.Fatalf("primary context: %+v", r)
		}
	}
}
