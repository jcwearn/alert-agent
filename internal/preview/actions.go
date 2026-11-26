package preview

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strconv"
	"time"

	"github.com/jcwearn/alert-agent/internal/actions"
	"github.com/jcwearn/alert-agent/internal/store"
)

// Phase 5's made-up history: a crash loop the agent fixed on its own (#28),
// its recurrence (#29), where the agent was refused a second attempt and
// asked instead (one request replaced, one approved), a pending request in
// an approval-only namespace (#27), and one each expired (#24) and rejected
// (#23).
func (f *fixtures) addActions(now time.Time) {
	ago := func(d time.Duration) time.Time { return now.Add(-d) }

	f.incidents = append(f.incidents,
		store.Incident{ID: 29, AlertName: "KubePodCrashLooping", Namespace: "books", Severity: "warning", AlertStatus: "firing",
			Summary: "books/calibre-web is crash looping", OpenedAt: ago(40 * time.Minute), UpdatedAt: ago(40 * time.Minute), AlertCount: 1},
		store.Incident{ID: 28, AlertName: "KubePodCrashLooping", Namespace: "books", Severity: "warning", AlertStatus: "resolved",
			Summary: "books/calibre-web is crash looping", OpenedAt: ago(95 * time.Minute), UpdatedAt: ago(80 * time.Minute),
			ResolvedAt: ptr(ago(80 * time.Minute)), AlertCount: 1},
	)
	slices.SortStableFunc(f.incidents, func(a, b store.Incident) int { return b.OpenedAt.Compare(a.OpenedAt) })
	for _, id := range []int64{28, 29} {
		in, _ := f.incident(id)
		f.alerts[id] = []store.IncidentAlert{{
			Fingerprint: "fp" + strconv.FormatInt(id, 10), Status: in.AlertStatus, StartsAt: in.OpenedAt,
			Labels:       map[string]string{"alertname": in.AlertName, "namespace": in.Namespace, "pod": "calibre-web-5f7c-x9k2p"},
			Annotations:  map[string]string{"description": in.Summary},
			GeneratorURL: "https://prometheus.medic.example/graph",
		}}
	}

	report := func(summary, action string) string {
		return "## Summary\n" + summary + "\n\n## Recommended fix\nautonomous: delete the pod.\n\n## Action\n" + action + "\n\n## Confidence\nhigh"
	}
	finished := func(id, incident int64, at time.Time, took time.Duration, rep string) store.Run {
		return store.Run{ID: id, IncidentID: incident, Kind: store.KindDiagnose, Status: store.RunSucceeded, Model: "claude-opus-5-5",
			CreatedAt: at, StartedAt: ptr(at.Add(5 * time.Second)), FinishedAt: ptr(at.Add(took)), DurationMS: ptr(took.Milliseconds()),
			NumTurns: ptr(9), Report: rep}
	}
	f.runs = append(f.runs,
		finished(41, 28, ago(93*time.Minute), 75*time.Second, report(
			"calibre-web crash loops on a stale lock file left in its emptyDir by an unclean exit.",
			"Deleted Pod books/calibre-web-5f7c-x9k2p; the replacement is Running and Ready.")),
		finished(43, 29, ago(38*time.Minute), 60*time.Second, report(
			"The same stale-lock crash loop as #28, 55 minutes after the pod was replaced.",
			"delete_pod was refused (a recurrence within the cooldown). Asked for approval to restart the rollout; the real fix is to clear the lock on start.")),
		store.Run{ID: 45, IncidentID: 29, ThreadID: ptr(int64(6)), Kind: store.KindChat, Status: store.RunSucceeded, CreatedAt: ago(20 * time.Minute),
			StartedAt: ptr(ago(20 * time.Minute)), FinishedAt: ptr(ago(19 * time.Minute)), Model: "claude-opus-5-5"},
		store.Run{ID: 47, IncidentID: 27, ThreadID: ptr(int64(7)), Kind: store.KindChat, Status: store.RunSucceeded, CreatedAt: ago(6 * time.Minute),
			StartedAt: ptr(ago(6 * time.Minute)), FinishedAt: ptr(ago(5 * time.Minute)), Model: "claude-opus-5-5"},
	)

	f.threads = append(f.threads,
		store.Thread{ID: 6, Kind: store.ThreadIncident, IncidentID: ptr(int64(29)), AlertName: "KubePodCrashLooping", Namespace: "books", CreatedAt: ago(21 * time.Minute)},
		store.Thread{ID: 7, Kind: store.ThreadIncident, IncidentID: ptr(int64(27)), AlertName: "EnvoyHighLatency", Namespace: "gateway", CreatedAt: ago(7 * time.Minute)},
	)
	msg := func(thread int64, run *int64, source, body string, at time.Time) store.ChatMessage {
		m := store.ChatMessage{ThreadID: thread, RunID: run, Source: source, Author: "you", Body: body, Status: store.ChatAnswered, CreatedAt: at, Page: "/incidents/29"}
		if source == store.SourceAgent {
			m.Author, m.Status, m.Page = "agent", store.ChatReply, ""
		}
		return m
	}
	f.messages = append(f.messages,
		msg(6, nil, store.SourceUI, "Why restart the whole rollout and not just delete the pod again?", ago(21*time.Minute)),
		msg(6, ptr(int64(45)), store.SourceAgent, "Deleting the pod is what I did for #28, and it came back 55 minutes later. The guardrails will not let me "+
			"repeat an action on a recurrence, so I asked you instead.\n\nI first asked to delete the pod again, then replaced that with a rollout restart: "+
			"calibre-web runs one replica, so either clears the lock, but the restart also picks up the config change that landed since.\n\n"+
			"Neither fixes the cause. The entrypoint should remove `/state/lock` when no other instance holds it; I can open a PR for that.", ago(19*time.Minute)),
		store.ChatMessage{ThreadID: 7, Source: store.SourceUI, Author: "you", Body: "Why does envoy need a restart?", Status: store.ChatAnswered,
			CreatedAt: ago(6 * time.Minute), Page: "/incidents/27"},
		msg(7, ptr(int64(47)), store.SourceAgent, "p99 latency has been over 2s since envoy-main reloaded its config at 14:02, and every upstream it "+
			"proxies to answers in under 50ms directly. The last time this happened (#19), a restart cleared it.\n\n"+
			"`gateway` is approval-only, so the request is waiting for you above.", ago(5*time.Minute)),
	)
	for i := range f.messages {
		f.messages[i].ID = int64(i + 1)
	}
	for _, id := range []int64{6, 7} {
		f.touch(f.thread(id))
	}

	input := func(v any) json.RawMessage { b, _ := json.Marshal(v); return b }
	add := func(a store.Action) { a.ID = int64(len(f.actions) + 1); f.actions = append(f.actions, a) }
	deleted := "DELETE pods books/calibre-web-5f7c-x9k2p uid=6f1c2a90-7b1e-4c55-9d1a-2b8e0f4c7a13 as alert-agent-actor: deleted; owner Deployment books/calibre-web"
	restarted := "PATCH deployments.apps books/calibre-web spec.template.metadata.annotations[kubectl.kubernetes.io/restartedAt]=" +
		ago(14*time.Minute).UTC().Format(time.RFC3339) + " as alert-agent-actor: patched"
	add(store.Action{Actor: store.ActorAgent, RunID: ptr(int64(41)), IncidentID: 28, Tool: actions.DeletePod,
		Mode: store.ModeAutonomous, Object: "Pod books/calibre-web-5f7c-x9k2p", Target: "Deployment books/calibre-web",
		Input:  input(map[string]string{"namespace": "books", "name": "calibre-web-5f7c-x9k2p", "reason": "Crash loop on a stale lock file in the pod's emptyDir; a fresh pod starts clean."}),
		Status: store.ActionSucceeded, Result: deleted, CreatedAt: ago(92 * time.Minute)})
	add(store.Action{Actor: store.ActorAgent, RunID: ptr(int64(43)), IncidentID: 29, Tool: actions.DeletePod,
		Mode: store.ModeAutonomous, Object: "Pod books/calibre-web-5f7c-m2q8d", Target: "Deployment books/calibre-web",
		Input:  input(map[string]string{"namespace": "books", "name": "calibre-web-5f7c-m2q8d", "reason": "Same stale lock as #28."}),
		Status: store.ActionRejected, CreatedAt: ago(37 * time.Minute),
		Result: fmt.Sprintf("this looks like a recurrence: action #%d delete_pod on Pod books/calibre-web-5f7c-x9k2p (succeeded) at %s for incident #28, "+
			"within the 6h0m0s cooldown on Deployment books/calibre-web and on KubePodCrashLooping in books. Do not retry. "+
			"Escalate: call request_approval to ask the owner, or propose_pr for a fix to the cause.", len(f.actions), ago(92*time.Minute).UTC().Format("15:04 UTC"))})
	add(store.Action{Actor: store.ActorAgent, RunID: ptr(int64(43)), IncidentID: 29, Tool: "request_approval",
		Input:  input(map[string]string{"action": "delete_pod", "namespace": "books", "name": "calibre-web-5f7c-m2q8d", "reason": "Recurred after the delete in #28."}),
		Status: store.ActionSucceeded, Result: "Asked the owner to approve delete_pod on Pod books/calibre-web-5f7c-m2q8d (approval #2 on incident #29).", CreatedAt: ago(37 * time.Minute)})
	add(store.Action{Actor: store.ActorAgent, RunID: ptr(int64(43)), IncidentID: 29, Tool: "request_approval",
		Input:  input(map[string]string{"action": "rollout_restart", "kind": "Deployment", "namespace": "books", "name": "calibre-web", "reason": "A restart also picks up the config change."}),
		Status: store.ActionSucceeded, Result: "Asked the owner to approve rollout_restart on Deployment books/calibre-web (approval #4 on incident #29). It replaces the pending request #2.", CreatedAt: ago(36 * time.Minute)})
	add(store.Action{Actor: store.ActorUI, IncidentID: 29, Tool: actions.RolloutRestart, Mode: store.ModeApproved,
		Object: "Deployment books/calibre-web", Target: "Deployment books/calibre-web",
		Input:  input(map[string]string{"tool": "rollout_restart", "kind": "Deployment", "namespace": "books", "name": "calibre-web"}),
		Status: store.ActionSucceeded, Result: restarted, CreatedAt: ago(14 * time.Minute)})
	restartAction := f.actions[len(f.actions)-1].ID
	add(store.Action{Actor: store.ActorAgent, RunID: ptr(int64(27)), IncidentID: 27, Tool: "request_approval",
		Input:  input(map[string]string{"action": "rollout_restart", "kind": "Deployment", "namespace": "gateway", "name": "envoy-main", "reason": "Latency since the 14:02 reload."}),
		Status: store.ActionSucceeded, Result: "Asked the owner to approve rollout_restart on Deployment gateway/envoy-main (approval #3 on incident #27).", CreatedAt: ago(10 * time.Minute)})

	ap := func(id, incident, run int64, tool, object, target, reason, status string, at time.Time) store.Approval {
		a := store.Approval{ID: id, IncidentID: incident, RunID: run, Tool: tool, Object: object, Reason: reason, Status: status,
			CreatedAt: at, ExpiresAt: at.Add(2 * time.Hour)}
		var kind, ns, name string
		_, _ = fmt.Sscanf(target, "%s %s", &kind, &ns)
		for i := range ns {
			if ns[i] == '/' {
				ns, name = ns[:i], ns[i+1:]
				break
			}
		}
		a.TargetKind, a.TargetNamespace, a.TargetName = kind, ns, name
		return a
	}
	f.approvals = []store.Approval{
		ap(1, 24, 20, actions.RolloutRestart, "DaemonSet monitoring/node-exporter", "DaemonSet monitoring/node-exporter",
			"node-exporter on node-c stopped answering scrapes after the maintenance taint; a restart reschedules it. monitoring is approval-only.",
			store.ApprovalExpired, ago(26*time.Hour)),
		ap(2, 29, 43, actions.DeletePod, "Pod books/calibre-web-5f7c-m2q8d", "Deployment books/calibre-web",
			"The crash loop from #28 is back: the same stale lock in /state. A fresh pod cleared it last time; I may not repeat that myself.",
			store.ApprovalSuperseded, ago(37*time.Minute)),
		ap(3, 27, 27, actions.RolloutRestart, "Deployment gateway/envoy-main", "Deployment gateway/envoy-main",
			"p99 latency has been over 2s since envoy-main reloaded its config at 14:02, while every upstream answers in under 50ms. A restart cleared the same symptom in #19. gateway is approval-only.",
			store.ApprovalPending, ago(10*time.Minute)),
		ap(4, 29, 43, actions.RolloutRestart, "Deployment books/calibre-web", "Deployment books/calibre-web",
			"Same stale lock as #28. One replica, so a restart clears it like a delete would, and it also picks up the config change since. Replaces #2.",
			store.ApprovalApproved, ago(36*time.Minute)),
		ap(5, 23, 38, actions.RerunCronJob, "CronJob monitoring/log-pruner", "CronJob monitoring/log-pruner",
			"The 03:00 prune failed while /var was full; with space back it should run now rather than tomorrow.",
			store.ApprovalRejected, ago(29*time.Hour)),
		ap(6, 25, 29, actions.RerunCronJob, "CronJob photos/face-index", "CronJob photos/face-index",
			"Last night's face-index run died when ml-worker was OOMKilled at 02:14. With PR #212's limit live it can run now instead of waiting a day.",
			store.ApprovalPending, ago(25*time.Minute)),
	}
	decide := func(i int, at time.Time, by string) { f.approvals[i].DecidedAt, f.approvals[i].DecidedBy = ptr(at), by }
	decide(0, ago(24*time.Hour), "expiry")
	decide(1, ago(36*time.Minute), "run:43")
	decide(3, ago(14*time.Minute), store.ActorUI)
	decide(4, ago(28*time.Hour), store.ActorUI)
	f.approvals[3].ActionID = ptr(restartAction)
	f.approvals[3].ActionStatus, f.approvals[3].ActionResult = store.ActionSucceeded, restarted
}

// ---- controller.Store: Phase 5 ----

type pausedKey struct{}

// ActionsPause is "paused" when the preview URL asks for it (?paused=1),
// so both states are one screenshot each.
func (f *fixtures) ActionsPause(ctx context.Context) (store.Pause, error) {
	if paused, _ := ctx.Value(pausedKey{}).(bool); paused {
		return store.Pause{Paused: true, By: store.ActorUI, At: time.Now().Add(-18 * time.Minute)}, nil
	}
	return store.Pause{}, nil
}

func (f *fixtures) SetActionsPaused(context.Context, bool, string) (bool, error) { return true, nil }

func (f *fixtures) PendingApprovals(context.Context) ([]store.Approval, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []store.Approval
	for _, ap := range f.approvals {
		if ap.Status == store.ApprovalPending {
			out = append(out, ap)
		}
	}
	return out, nil
}

func (f *fixtures) IncidentApprovals(_ context.Context, id int64) ([]store.Approval, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if in, ok := f.incident(id); ok && in.ParentID != nil {
		id = *in.ParentID
	}
	var out []store.Approval
	for _, ap := range f.approvals {
		if ap.IncidentID == id {
			out = append(out, ap)
		}
	}
	slices.SortFunc(out, func(a, b store.Approval) int { return cmp.Compare(a.ID, b.ID) })
	return out, nil
}

// DecideApproval works in the preview, so the buttons can be clicked; the
// "action" it runs does nothing.
func (f *fixtures) DecideApproval(_ context.Context, id int64, approve bool, by string) (store.Approval, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i := range f.approvals {
		ap := &f.approvals[i]
		if ap.ID != id {
			continue
		}
		if ap.Status != store.ApprovalPending {
			return *ap, fmt.Errorf("%w: approval #%d is %s", store.ErrConflict, id, ap.Status)
		}
		ap.Status, ap.DecidedBy, ap.DecidedAt = store.ApprovalRejected, by, ptr(time.Now())
		if approve {
			ap.Status = store.ApprovalApproved
		}
		return *ap, nil
	}
	return store.Approval{}, store.ErrNotFound
}

func (f *fixtures) StartClusterAction(_ context.Context, a store.ClusterAction, _ store.Guardrails) (id int64, refusal string, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	id = int64(len(f.actions) + 1)
	f.actions = append(f.actions, store.Action{ID: id, Actor: a.Actor, IncidentID: a.IncidentID, Tool: a.Tool, Input: a.Input,
		Mode: a.Mode, Object: a.Object, Status: store.ActionStarted, CreatedAt: time.Now()})
	return id, "", nil
}

func (f *fixtures) FinishAction(_ context.Context, id int64, status, result string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.actions[id-1].Status, f.actions[id-1].Result = status, result
	return nil
}

func (f *fixtures) SetApprovalAction(_ context.Context, id, actionID int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i := range f.approvals {
		if f.approvals[i].ID == id {
			f.approvals[i].ActionID = &actionID
		}
	}
	return nil
}

// previewActor stands in for the cluster: approving in the preview runs
// nothing.
type previewActor struct{}

func (previewActor) Do(_ context.Context, req actions.Request, _ actions.Options) (actions.Result, error) {
	obj := actions.Object{Kind: req.Kind, Namespace: req.Namespace, Name: req.Name}
	return actions.Result{Target: obj, Object: obj, Call: "(preview) " + req.Tool + " " + obj.String() + ": nothing was done"}, nil
}

// ---- controller.Store: Phase 6 (read by /metrics) ----

func (f *fixtures) QueueDepth(context.Context) (map[string]int, time.Duration, error) {
	return map[string]int{store.KindDiagnose: 0, store.KindChat: 0}, 0, nil
}

func (f *fixtures) GetClaudeBlock(context.Context) (store.ClaudeBlock, bool, error) {
	return store.ClaudeBlock{}, false, nil
}
