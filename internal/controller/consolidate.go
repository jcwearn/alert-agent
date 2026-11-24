package controller

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/jcwearn/alert-agent/internal/store"
)

// LinkInput is what the agent passes to link_incident.
type LinkInput struct {
	OtherID    int64  `json:"other_id" jsonschema:"the incident this one is related to"`
	Reason     string `json:"reason" jsonschema:"one or two sentences: how the two are related, and why they are not the same problem"`
	IncidentID int64  `json:"incident_id,omitempty" jsonschema:"which of this run's incidents to link; defaults to the run's primary incident"`
}

// MergeInput is what the agent passes to merge_incident.
type MergeInput struct {
	IntoID     int64  `json:"into_id" jsonschema:"the incident that owns the problem; this incident is folded into it"`
	Reason     string `json:"reason" jsonschema:"the evidence that both incidents have the same failing object and the same root cause"`
	Confidence string `json:"confidence" jsonschema:"high, medium or low; only high is accepted"`
	IncidentID int64  `json:"incident_id,omitempty" jsonschema:"which of this run's incidents to merge; defaults to the run's primary incident"`
}

// Consolidation limits.
const (
	maxConsolidationCalls = 10
	maxReason             = 1024
)

// refusal is a merge or link the controller declines; its text goes back to
// whoever asked.
type refusal struct{ reason string }

func (r *refusal) Error() string { return r.reason }

func refuse(format string, args ...any) error { return &refusal{fmt.Sprintf(format, args...)} }

// actor is who asked for a consolidation: an agent run, or a human in the UI.
type actor struct {
	kind  string
	runID int64
}

func agentActor(runID int64) actor { return actor{kind: store.ActorAgent, runID: runID} }

var uiActor = actor{kind: store.ActorUI}

// mergedBy is how incidents.merged_by names the actor.
func (a actor) mergedBy() string {
	if a.kind == store.ActorAgent {
		return "run:" + strconv.FormatInt(a.runID, 10)
	}
	return store.ActorUI
}

func (s *Server) addConsolidationTools(srv *mcp.Server, run store.Run) {
	mcp.AddTool(srv, &mcp.Tool{
		Name: "link_incident",
		Description: "Record that one of this run's incidents is RELATED to another incident (for example, the same " +
			"node or an upstream dependency) without being the same problem. Both stay open and each keeps its own diagnosis.",
		Annotations: &mcp.ToolAnnotations{DestructiveHint: ptr(false), IdempotentHint: true},
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in LinkInput) (*mcp.CallToolResult, any, error) {
		return s.agentConsolidation(ctx, run, "link_incident", in, in.IncidentID, in.OtherID,
			func(ctx context.Context, child int64) (string, error) {
				if err := checkReason(in.Reason); err != nil {
					return "", err
				}
				return s.link(ctx, agentActor(run.ID), child, in.OtherID, in.Reason)
			})
	})
	mcp.AddTool(srv, &mcp.Tool{
		Name: "merge_incident",
		Description: "Fold one of this run's incidents into another incident that has the SAME failing object and the SAME " +
			"root cause, so one diagnosis and at most one PR cover both. Accepted only with confidence high and within one " +
			"namespace; otherwise use link_incident. A human can undo a merge.",
		Annotations: &mcp.ToolAnnotations{DestructiveHint: ptr(false)},
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in MergeInput) (*mcp.CallToolResult, any, error) {
		return s.agentConsolidation(ctx, run, "merge_incident", in, in.IncidentID, in.IntoID,
			func(ctx context.Context, child int64) (string, error) {
				return s.agentMerge(ctx, run, child, in)
			})
	})
}

// agentConsolidation records the call, applies the limits every
// consolidation tool shares, and runs do on the incident it names.
func (s *Server) agentConsolidation(ctx context.Context, run store.Run, tool string, in any, incidentID, target int64,
	do func(ctx context.Context, child int64) (string, error),
) (*mcp.CallToolResult, any, error) {
	if incidentID == 0 {
		incidentID = run.IncidentID
	}
	input, _ := json.Marshal(in)
	actionID, err := s.store.RecordAction(ctx, store.ActionStart{
		Actor: store.ActorAgent, RunID: run.ID, IncidentID: incidentID, TargetIncidentID: target,
		Tool: tool, Input: input,
	})
	if err != nil {
		slog.Error("recording action", "run", run.ID, "err", err)
		return toolError("the controller could not record this call, so it did nothing; try once more later"), nil, nil
	}

	msg, err := func() (string, error) {
		links, err := s.store.CountActions(ctx, run.ID, "link_incident")
		if err != nil {
			return "", err
		}
		merges, err := s.store.CountActions(ctx, run.ID, "merge_incident")
		if err != nil {
			return "", err
		}
		if links+merges > maxConsolidationCalls {
			return "", refuse("link_incident and merge_incident were already called %d times in this run; stop and report", maxConsolidationCalls)
		}
		members, err := s.store.RunIncidents(ctx, run.ID)
		if err != nil {
			return "", err
		}
		if !slices.Contains(members, incidentID) {
			return "", refuse("incident #%d is not one of this run's incidents (%s); incident_id must be one of them", incidentID, idList(members))
		}
		return do(ctx, incidentID)
	}()
	return s.finishConsolidation(ctx, actionID, run.ID, msg, err)
}

func (s *Server) finishConsolidation(ctx context.Context, actionID, runID int64, msg string, err error) (*mcp.CallToolResult, any, error) {
	var r *refusal
	switch {
	case err == nil:
		s.finishAction(ctx, actionID, store.ActionSucceeded, msg)
		return toolText(msg), nil, nil
	case errors.As(err, &r):
		s.finishAction(ctx, actionID, store.ActionRejected, r.reason)
		slog.Info("consolidation refused", "run", runID, "reason", r.reason)
		return toolError("Refused: " + r.reason), nil, nil
	default:
		s.finishAction(ctx, actionID, store.ActionFailed, err.Error())
		slog.Error("consolidation failed", "run", runID, "err", err)
		return toolError("The controller failed to record this (its error is logged). Do not retry; mention it in your report."), nil, nil
	}
}

// agentMerge applies the rules an agent's merge must pass on top of the
// data model's: high confidence, one namespace, a live target.
func (s *Server) agentMerge(ctx context.Context, run store.Run, childID int64, in MergeInput) (string, error) {
	if in.Confidence != "high" {
		return "", refuse("merge needs confidence high (got %q); if the incidents are related but not certainly the same problem, call link_incident instead", in.Confidence)
	}
	if err := checkReason(in.Reason); err != nil {
		return "", err
	}
	child, _, err := s.loadIncident(ctx, childID)
	if err != nil {
		return "", err
	}
	if child.Merged() {
		return "", refuse("incident #%d is already merged into #%d", child.ID, *child.ParentID)
	}
	into, _, err := s.loadIncident(ctx, in.IntoID)
	if err != nil {
		return "", err
	}
	root := into
	if into.ParentID != nil {
		if root, _, err = s.loadIncident(ctx, *into.ParentID); err != nil {
			return "", err
		}
	}
	if root.ID == child.ID {
		return "", refuse("#%d is merged into #%d already; merging back would make a loop", into.ID, child.ID)
	}
	if root.Namespace != child.Namespace {
		return "", refuse("#%d is in namespace %q and #%d in %q; the agent merges only within one namespace, so call link_incident instead",
			child.ID, child.Namespace, root.ID, root.Namespace)
	}
	if root.AlertStatus != "firing" && s.now().Sub(root.OpenedAt) > s.opts.RecentWindow {
		return "", refuse("#%d is resolved and older than %s; merge only into a current incident", root.ID, s.opts.RecentWindow)
	}
	if child.ID == run.IncidentID {
		members, err := s.store.RunIncidents(ctx, run.ID)
		if err != nil {
			return "", err
		}
		if slices.Contains(members, root.ID) {
			return "", refuse("#%d is in this run's own batch; merge the batch members into the primary incident #%d instead", root.ID, run.IncidentID)
		}
	}

	got, err := s.merge(ctx, agentActor(run.ID), child.ID, in.IntoID, in.Reason)
	if err != nil {
		return "", err
	}
	if child.ID == run.IncidentID {
		got += fmt.Sprintf("\nThis run's incident is now owned by #%d. Stop investigating, and end with only:\n## Merged\nInto #%d: <reason>.", root.ID, root.ID)
	}
	return got, nil
}

// merge folds child into into for any actor, and returns what happened.
func (s *Server) merge(ctx context.Context, a actor, childID, intoID int64, reason string) (string, error) {
	root, err := s.store.MergeIncident(ctx, childID, intoID, a.mergedBy(), reason)
	switch {
	case errors.Is(err, store.ErrNotFound):
		return "", refuse("incident #%d or #%d does not exist", childID, intoID)
	case errors.Is(err, store.ErrConflict):
		return "", refuse("%s", strings.TrimPrefix(err.Error(), store.ErrConflict.Error()+": "))
	case err != nil:
		return "", err
	}
	s.metrics.consolidations.WithLabelValues("merge", a.kind).Inc()
	slog.Info("incident merged", "incident", childID, "into", root, "by", a.mergedBy())
	msg := fmt.Sprintf("Merged #%d into #%d.", childID, root)
	if root != intoID {
		msg += fmt.Sprintf(" (#%d is itself merged into #%d, so #%d went there.)", intoID, root, childID)
	}
	return msg, nil
}

// link records a related link for any actor.
func (s *Server) link(ctx context.Context, a actor, x, y int64, reason string) (string, error) {
	if x == y {
		return "", refuse("an incident cannot be linked to itself")
	}
	if _, _, err := s.loadIncident(ctx, y); err != nil {
		return "", err
	}
	created, err := s.store.LinkIncidents(ctx, x, y, reason, a.kind, a.runID)
	if err != nil {
		return "", err
	}
	if !created {
		return fmt.Sprintf("#%d and #%d were already linked; nothing changed.", x, y), nil
	}
	s.metrics.consolidations.WithLabelValues("link", a.kind).Inc()
	slog.Info("incidents linked", "incident", x, "other", y, "by", a.mergedBy())
	return fmt.Sprintf("Linked #%d and #%d as related.", x, y), nil
}

func (s *Server) loadIncident(ctx context.Context, id int64) (store.Incident, []store.IncidentAlert, error) {
	in, alerts, err := s.store.GetIncident(ctx, id)
	if errors.Is(err, store.ErrNotFound) {
		return in, nil, refuse("incident #%d does not exist", id)
	}
	return in, alerts, err
}

func checkReason(reason string) error {
	reason = strings.TrimSpace(reason)
	if reason == "" {
		return refuse("reason is required")
	}
	if len(reason) > maxReason || !utf8.ValidString(reason) {
		return refuse("reason must be valid text of at most %d bytes", maxReason)
	}
	return nil
}

func idList(ids []int64) string {
	parts := make([]string, len(ids))
	for i, id := range ids {
		parts[i] = "#" + strconv.FormatInt(id, 10)
	}
	return strings.Join(parts, ", ")
}
