package controller

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/jcwearn/alert-agent/internal/actions"
	"github.com/jcwearn/alert-agent/internal/runnerapi"
	"github.com/jcwearn/alert-agent/internal/store"
)

// Actor performs cluster actions as the actor ServiceAccount (see
// actions.Executor).
type Actor interface {
	Do(ctx context.Context, req actions.Request, opts actions.Options) (actions.Result, error)
}

// ApprovalNotifier tells a human an action is waiting for their approval.
type ApprovalNotifier interface {
	ApprovalRequested(ctx context.Context, ap store.Approval, in store.Incident, incidentURL string) error
}

// Actions configures Phase 5: the cluster actions alert runs may take, and
// the approvals they ask for when the guardrails hold them back.
type Actions struct {
	Actor Actor
	// Autonomous are the namespaces where a workload action may run without
	// a human. The actor's RBAC reaches further (approval-only namespaces);
	// the admission policy narrows what it may change anywhere.
	Autonomous map[string]bool
	Guardrails store.Guardrails
	// ApprovalTTL is how long a request waits for a human.
	ApprovalTTL time.Duration
	Notify      []ApprovalNotifier
}

// EnableActions turns on the cluster actions and approvals.
func (s *Server) EnableActions(a *Actions) { s.actions = a }

// ActionInput is what the agent passes to a cluster action.
type ActionInput struct {
	Kind      string `json:"kind,omitempty" jsonschema:"rollout_restart: Deployment, StatefulSet or DaemonSet; flux_reconcile: Kustomization, HelmRelease, GitRepository, HelmRepository or OCIRepository; ignored otherwise"`
	Namespace string `json:"namespace" jsonschema:"the object's namespace"`
	Name      string `json:"name" jsonschema:"the object's name"`
	Reason    string `json:"reason" jsonschema:"one or two sentences: the evidence that this action fixes the incident"`
}

// ApprovalInput is what the agent passes to request_approval.
type ApprovalInput struct {
	Action    string `json:"action" jsonschema:"delete_pod, rollout_restart, rerun_cronjob or flux_reconcile"`
	Kind      string `json:"kind,omitempty" jsonschema:"as for the action"`
	Namespace string `json:"namespace" jsonschema:"the object's namespace"`
	Name      string `json:"name" jsonschema:"the object's name"`
	Reason    string `json:"reason" jsonschema:"for the owner: what is wrong, why this action, and why you could not take it yourself"`
}

// actionRecord is what the audit trail keeps as a cluster action's input.
type actionRecord struct {
	actions.Request
	Reason string `json:"reason,omitempty"`
}

const actionRules = " One autonomous action per incident (merged incidents included), a cooldown per workload and per alert so a " +
	"recurrence escalates instead of retrying, an hourly cap, and the owner's pause switch; in an approval-only namespace a workload " +
	"action needs request_approval. A refusal says which rule held it back. Use it only for the autonomous fix class at high " +
	"confidence, and give the evidence as reason."

var actionDescriptions = map[string]string{
	actions.DeletePod: "Delete one pod so its controller replaces it with a fresh one: clears a crash loop caused by state inside the " +
		"pod (an emptyDir, a stale lock, a wedged process) or a pod stuck terminating. Only pods owned by a ReplicaSet, StatefulSet, " +
		"DaemonSet or Job; never a database or other operator-managed pod.",
	actions.RolloutRestart: "Restart every pod of a Deployment, StatefulSet or DaemonSet by setting the pod template's restartedAt " +
		"annotation, as kubectl rollout restart does. Prefer delete_pod when one pod is the problem.",
	actions.RerunCronJob: "Run a CronJob once now, as a Job from its template (kubectl create job --from=cronjob/NAME), for a run that " +
		"failed for a transient reason that has since cleared. Refused while one of its Jobs is still running.",
	actions.FluxReconcile: "Ask Flux to reconcile a Kustomization, HelmRelease, GitRepository, HelmRepository or OCIRepository now " +
		"(the requestedAt annotation), for one that is stuck or behind. Refused if it is suspended.",
}

// addActionTools serves the cluster actions and request_approval to an
// alert run.
func (s *Server) addActionTools(srv *mcp.Server, run store.Run) {
	for _, tool := range actions.Tools {
		mcp.AddTool(srv, &mcp.Tool{
			Name:        tool,
			Description: actionDescriptions[tool] + actionRules,
			Annotations: &mcp.ToolAnnotations{DestructiveHint: ptr(true), OpenWorldHint: ptr(true)},
		}, func(ctx context.Context, _ *mcp.CallToolRequest, in ActionInput) (*mcp.CallToolResult, any, error) {
			req := actions.Request{Tool: tool, Kind: in.Kind, Namespace: in.Namespace, Name: in.Name}
			if tool == actions.DeletePod || tool == actions.RerunCronJob {
				req.Kind = ""
			}
			return s.clusterActionTool(ctx, run, req, in.Reason)
		})
	}
	s.addApprovalTool(srv, run)
}

// clusterActionTool takes one autonomous action. It checks the request, the
// namespace's tier and (with a server-side dry run, as the actor) RBAC,
// existence and the admission policy; then the guardrails, in the same
// transaction that records the action; and only then acts, recording the
// exact call and its result. Every refusal is recorded too.
func (s *Server) clusterActionTool(ctx context.Context, run store.Run, req actions.Request, reason string) (*mcp.CallToolResult, any, error) {
	input, _ := json.Marshal(actionRecord{Request: req, Reason: reason})
	refuse := func(status, msg string) (*mcp.CallToolResult, any, error) {
		s.metrics.actions.WithLabelValues(req.Tool, store.ModeAutonomous, status).Inc()
		id, err := s.store.RecordAction(ctx, store.ActionStart{Actor: store.ActorAgent, RunID: run.ID, IncidentID: run.IncidentID,
			Tool: req.Tool, Input: input})
		if err != nil {
			slog.Error("recording action", "run", run.ID, "err", err)
		} else {
			s.finishAction(ctx, id, status, msg)
		}
		if status == store.ActionFailed {
			return toolError("The controller failed to check this action (its error is logged). Do not retry; describe the fix in your report."), nil, nil
		}
		return toolError("Refused: " + msg), nil, nil
	}

	if err := actions.Check(req); err != nil {
		return refuse(store.ActionRejected, err.Error())
	}
	if err := checkReason(reason); err != nil {
		return refuse(store.ActionRejected, err.Error())
	}
	if actions.IsWorkloadTool(req.Tool) && !s.actions.Autonomous[req.Namespace] {
		return refuse(store.ActionRejected, fmt.Sprintf(
			"namespace %s is approval-only: a workload action there needs the owner. Call request_approval with the same arguments, or propose_pr for a fix to the cause",
			req.Namespace))
	}
	dry, err := s.actions.Actor.Do(ctx, req, actions.Options{DryRun: true, IncidentID: run.IncidentID})
	if err != nil {
		return refuse(actionErrStatus(err), actionErrText(err, run.ID))
	}

	actionID, refusal, err := s.store.StartClusterAction(ctx, store.ClusterAction{
		Actor: store.ActorAgent, RunID: run.ID, IncidentID: run.IncidentID, Tool: req.Tool, Input: input,
		Mode: store.ModeAutonomous, TargetKind: dry.Target.Kind, TargetNamespace: dry.Target.Namespace,
		TargetName: dry.Target.Name, Object: dry.Object.String(),
	}, s.actions.Guardrails)
	if err != nil {
		slog.Error("recording action", "run", run.ID, "err", err)
		return toolError("the controller could not record this call, so it did nothing; try once more later"), nil, nil
	}
	if refusal != "" {
		s.metrics.actions.WithLabelValues(req.Tool, store.ModeAutonomous, store.ActionRejected).Inc()
		slog.Info("action refused by guardrails", "run", run.ID, "tool", req.Tool, "object", dry.Object, "reason", refusal)
		return toolError("Refused: " + refusal), nil, nil
	}

	res, err := s.actions.Actor.Do(context.WithoutCancel(ctx), req, actions.Options{IncidentID: run.IncidentID})
	if err != nil {
		status := actionErrStatus(err)
		s.finishAction(ctx, actionID, status, err.Error())
		s.metrics.actions.WithLabelValues(req.Tool, store.ModeAutonomous, status).Inc()
		slog.Warn("action did not complete", "run", run.ID, "tool", req.Tool, "object", dry.Object, "err", err)
		return toolError("The action did not complete: " + actionErrText(err, run.ID) +
			"\nThis was this incident's one autonomous attempt; escalate with request_approval or propose_pr, or describe it in your report."), nil, nil
	}
	s.finishAction(ctx, actionID, store.ActionSucceeded, res.Call)
	s.metrics.actions.WithLabelValues(req.Tool, store.ModeAutonomous, store.ActionSucceeded).Inc()
	slog.Info("action taken", "run", run.ID, "incident", run.IncidentID, "call", res.Call)
	return toolText("Done: " + res.Call + "\nCheck the outcome with kube_get (the replacement pod, the rollout, the Job). " +
		"This was this incident's one autonomous action: if it does not recover, escalate with request_approval or propose_pr, " +
		"and say what you did under ## Action in your report."), nil, nil
}

func actionErrStatus(err error) string {
	if errors.As(err, new(*actions.Refusal)) {
		return store.ActionRejected
	}
	return store.ActionFailed
}

func actionErrText(err error, runID int64) string {
	var r *actions.Refusal
	if errors.As(err, &r) {
		if r.Forbidden {
			return r.Reason + " (request_approval would be refused too)"
		}
		return r.Reason
	}
	slog.Error("cluster action failed", "run", runID, "err", err)
	return "the API call failed (its error is logged)"
}

// addApprovalTool serves request_approval.
func (s *Server) addApprovalTool(srv *mcp.Server, run store.Run) {
	mcp.AddTool(srv, &mcp.Tool{
		Name: "request_approval",
		Description: "Ask the owner to approve a cluster action you may not take yourself: a second attempt after a recurrence, " +
			"a workload in an approval-only namespace, or while actions are paused. Same arguments as the action, plus your " +
			"reasoning for the owner. The controller checks it with a dry run, notifies the owner on ntfy and Slack, and the " +
			"owner approves or rejects it in the UI; nothing you can call approves it. One pending request per incident: a new " +
			"one replaces the old. When the real fix is a manifest change, propose_pr is the better escalation.",
		Annotations: &mcp.ToolAnnotations{DestructiveHint: ptr(false), OpenWorldHint: ptr(true)},
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in ApprovalInput) (*mcp.CallToolResult, any, error) {
		return s.requestApprovalTool(ctx, run, in)
	})
}

func (s *Server) requestApprovalTool(ctx context.Context, run store.Run, in ApprovalInput) (*mcp.CallToolResult, any, error) {
	input, _ := json.Marshal(in)
	actionID, err := s.store.StartAction(ctx, run.ID, run.IncidentID, "request_approval", input)
	if err != nil {
		slog.Error("recording action", "run", run.ID, "err", err)
		return toolError("the controller could not record this call, so it did nothing; try once more later"), nil, nil
	}
	msg, err := s.requestApproval(ctx, run, in)
	var r *refusal
	switch {
	case err == nil:
		s.finishAction(ctx, actionID, store.ActionSucceeded, msg)
		return toolText(msg), nil, nil
	case errors.As(err, &r):
		s.finishAction(ctx, actionID, store.ActionRejected, r.reason)
		return toolError("Refused: " + r.reason), nil, nil
	default:
		s.finishAction(ctx, actionID, store.ActionFailed, err.Error())
		slog.Error("request_approval failed", "run", run.ID, "err", err)
		return toolError("The controller failed to record the request (its error is logged). Describe the action in your report instead."), nil, nil
	}
}

func (s *Server) requestApproval(ctx context.Context, run store.Run, in ApprovalInput) (string, error) {
	if run.IncidentID == 0 {
		return "", refuse("request_approval needs an incident; this conversation has none")
	}
	req := actions.Request{Tool: in.Action, Kind: in.Kind, Namespace: in.Namespace, Name: in.Name}
	if req.Tool == actions.DeletePod || req.Tool == actions.RerunCronJob {
		req.Kind = ""
	}
	if err := actions.Check(req); err != nil {
		return "", refuse("%s", err.Error())
	}
	if err := checkReason(in.Reason); err != nil {
		return "", err
	}
	incident, _, err := s.loadIncident(ctx, run.IncidentID)
	if err != nil {
		return "", err
	}
	root := incident
	if incident.ParentID != nil {
		if root, _, err = s.loadIncident(ctx, *incident.ParentID); err != nil {
			return "", err
		}
	}
	dry, err := s.actions.Actor.Do(ctx, req, actions.Options{DryRun: true, IncidentID: root.ID})
	if err != nil {
		var ar *actions.Refusal
		if errors.As(err, &ar) {
			return "", refuse("the action would be refused, so there is nothing to approve: %s", actionErrText(err, run.ID))
		}
		return "", err
	}
	raw, _ := json.Marshal(req)
	ap := store.Approval{
		IncidentID: root.ID, RunID: run.ID, Tool: req.Tool, Input: raw, Reason: strings.TrimSpace(in.Reason),
		TargetKind: dry.Target.Kind, TargetNamespace: dry.Target.Namespace, TargetName: dry.Target.Name,
		Object: dry.Object.String(), ExpiresAt: s.now().Add(s.actions.ApprovalTTL),
	}
	id, superseded, err := s.store.RequestApproval(ctx, ap)
	if err != nil {
		return "", err
	}
	ap.ID, ap.Status, ap.CreatedAt = id, store.ApprovalPending, s.now()
	s.metrics.approvals.WithLabelValues("requested").Inc()
	for range superseded {
		s.metrics.approvals.WithLabelValues("superseded").Inc()
	}
	slog.Info("approval requested", "approval", id, "run", run.ID, "incident", root.ID, "tool", req.Tool, "object", ap.Object)
	for _, n := range s.actions.Notify {
		go func() {
			if err := n.ApprovalRequested(context.Background(), ap, root, s.incidentURL(root.ID)); err != nil {
				slog.Error("approval notification", "approval", id, "err", err)
			}
		}()
	}

	msg := fmt.Sprintf("Asked the owner to approve %s on %s (approval #%d on incident #%d, expires %s). They were notified; "+
		"the outcome will show on the incident page. Say in your report that the action awaits approval.",
		req.Tool, ap.Object, id, root.ID, ap.ExpiresAt.UTC().Format("15:04 UTC"))
	if len(superseded) > 0 {
		msg += fmt.Sprintf(" It replaces the pending request %s.", idList(superseded))
	}
	return msg, nil
}

// ---- the UI side: approve, reject, pause ----

func (s *Server) uiApprove(w http.ResponseWriter, r *http.Request) { s.uiDecide(w, r, true) }
func (s *Server) uiReject(w http.ResponseWriter, r *http.Request)  { s.uiDecide(w, r, false) }

// uiDecide records a human's decision on a request and, on approval, runs
// the action under the same audit trail. The decision is a conditional
// update, so a second click (or a second tab) cannot run it twice.
func (s *Server) uiDecide(w http.ResponseWriter, r *http.Request, approve bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	ctx := r.Context()
	if approve && s.actions == nil {
		http.Error(w, "cluster actions are disabled on this controller (ACTIONS_ENABLED)", http.StatusConflict)
		return
	}
	ap, err := s.store.DecideApproval(ctx, id, approve, store.ActorUI)
	q := url.Values{}
	switch {
	case errors.Is(err, store.ErrNotFound):
		http.NotFound(w, r)
		return
	case errors.Is(err, store.ErrConflict):
		q.Set("error", strings.TrimPrefix(err.Error(), store.ErrConflict.Error()+": ")+"; nothing was done.")
	case err != nil:
		slog.Error("deciding approval", "approval", id, "err", err)
		http.Error(w, "could not record the decision, so nothing was done", http.StatusInternalServerError)
		return
	case !approve:
		s.metrics.approvals.WithLabelValues("rejected").Inc()
		slog.Info("approval rejected", "approval", id, "by", store.ActorUI)
		q.Set("msg", fmt.Sprintf("Rejected approval #%d; nothing was done.", id))
	default:
		s.metrics.approvals.WithLabelValues("approved").Inc()
		msg, err := s.runApproved(ctx, ap)
		if err != nil {
			q.Set("error", err.Error())
		} else {
			q.Set("msg", msg)
		}
	}
	http.Redirect(w, r, "/incidents/"+strconv.FormatInt(ap.IncidentID, 10)+"?"+q.Encode(), http.StatusSeeOther)
}

// runApproved performs an approved action. The guardrails are a human's to
// override, but RBAC, the admission policy and the action's own rules still
// apply, and the action is recorded before it runs.
func (s *Server) runApproved(ctx context.Context, ap store.Approval) (string, error) {
	var req actions.Request
	if err := json.Unmarshal(ap.Input, &req); err != nil {
		return "", fmt.Errorf("approval #%d has an unreadable action", ap.ID)
	}
	actionID, _, err := s.store.StartClusterAction(ctx, store.ClusterAction{
		Actor: store.ActorUI, IncidentID: ap.IncidentID, Tool: req.Tool, Input: ap.Input, Mode: store.ModeApproved,
		TargetKind: ap.TargetKind, TargetNamespace: ap.TargetNamespace, TargetName: ap.TargetName, Object: ap.Object,
		ApprovalID: ap.ID,
	}, store.Guardrails{})
	if err != nil {
		slog.Error("recording approved action", "approval", ap.ID, "err", err)
		return "", errors.New("approved, but the action could not be recorded, so it did not run")
	}
	if err := s.store.SetApprovalAction(ctx, ap.ID, actionID); err != nil {
		slog.Error("linking approval to action", "approval", ap.ID, "action", actionID, "err", err)
	}
	res, err := s.actions.Actor.Do(context.WithoutCancel(ctx), req, actions.Options{IncidentID: ap.IncidentID})
	if err != nil {
		status := actionErrStatus(err)
		s.finishAction(ctx, actionID, status, err.Error())
		s.metrics.actions.WithLabelValues(req.Tool, store.ModeApproved, status).Inc()
		slog.Warn("approved action did not complete", "approval", ap.ID, "err", err)
		var r *actions.Refusal
		if errors.As(err, &r) {
			return "", fmt.Errorf("approved, but the action was refused: %s", r.Reason)
		}
		return "", errors.New("approved, but the action failed; its error is logged")
	}
	s.finishAction(ctx, actionID, store.ActionSucceeded, res.Call)
	s.metrics.actions.WithLabelValues(req.Tool, store.ModeApproved, store.ActionSucceeded).Inc()
	slog.Info("approved action taken", "approval", ap.ID, "call", res.Call)
	return fmt.Sprintf("Approved #%d: %s", ap.ID, res.Call), nil
}

func (s *Server) uiPause(w http.ResponseWriter, r *http.Request)  { s.uiSetPause(w, r, true) }
func (s *Server) uiResume(w http.ResponseWriter, r *http.Request) { s.uiSetPause(w, r, false) }

func (s *Server) uiSetPause(w http.ResponseWriter, r *http.Request, paused bool) {
	changed, err := s.store.SetActionsPaused(r.Context(), paused, store.ActorUI)
	if err != nil {
		slog.Error("setting pause", "paused", paused, "err", err)
		http.Error(w, "could not change the switch", http.StatusInternalServerError)
		return
	}
	if changed {
		slog.Info("autonomous actions switched", "paused", paused, "by", store.ActorUI)
	}
	http.Redirect(w, r, localPath(r.PostFormValue("back")), http.StatusSeeOther)
}

// localPath keeps a redirect on this site.
func localPath(p string) string {
	if !strings.HasPrefix(p, "/") || strings.HasPrefix(p, "//") || strings.Contains(p, `\`) {
		return "/"
	}
	return p
}

// expireApprovals marks requests expired once they pass their TTL or their
// incident resolves, so nobody approves a fix for a problem that is gone.
func (s *Server) expireApprovals(ctx context.Context) {
	t := time.NewTicker(time.Minute)
	defer t.Stop()
	for {
		expired, err := s.store.ExpireApprovals(ctx)
		if err != nil && ctx.Err() == nil {
			slog.Error("expiring approvals", "err", err)
		}
		for _, ap := range expired {
			s.metrics.approvals.WithLabelValues("expired").Inc()
			slog.Info("approval expired", "approval", ap.ID, "incident", ap.IncidentID)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// ---- run context ----

// addActionContext tells an alert run what it may do and what was already
// done nearby: a recurrence must see the earlier action to escalate.
func (s *Server) addActionContext(ctx context.Context, rc *runnerapi.Context, in store.Incident) error {
	rc.Actions = true
	rc.AutonomousNamespaces = slices.Sorted(maps.Keys(s.actions.Autonomous))
	pause, err := s.store.ActionsPause(ctx)
	if err != nil {
		return err
	}
	rc.ActionsPaused = pause.Paused
	window := s.actions.Guardrails.Cooldown
	if window <= 0 {
		window = 24 * time.Hour
	}
	prior, err := s.store.PriorClusterActions(ctx, in.Namespace, in.AlertName, s.now().Add(-window))
	if err != nil {
		return err
	}
	for _, p := range prior {
		rc.PriorActions = append(rc.PriorActions, apiPriorAction(p))
	}
	return nil
}

func apiPriorAction(p store.PriorAction) runnerapi.PriorAction {
	return runnerapi.PriorAction{ID: p.ID, IncidentID: p.IncidentID, AlertName: p.AlertName, Tool: p.Tool, Mode: p.Mode,
		Target: p.Target, Status: p.Status, Result: firstLine(p.Result), At: p.CreatedAt}
}

func apiApprovals(aps []store.Approval) []runnerapi.Approval {
	out := make([]runnerapi.Approval, 0, len(aps))
	for _, ap := range aps {
		a := runnerapi.Approval{ID: ap.ID, Tool: ap.Tool, Target: ap.Target(), Object: ap.Object, Reason: ap.Reason,
			Status: ap.Status, At: ap.CreatedAt}
		if ap.ActionStatus != "" {
			a.Result = ap.ActionStatus + ": " + firstLine(ap.ActionResult)
		}
		out = append(out, a)
	}
	return out
}
