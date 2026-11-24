package controller

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/jcwearn/alert-agent/internal/store"
)

// uiAction runs one form post from the incident page: it records the action
// before doing anything, runs do, records how it ended, and redirects back
// to the incident with the outcome. Every human merge and link is therefore
// in the same audit trail as the agent's.
func (s *Server) uiAction(w http.ResponseWriter, r *http.Request, tool string, target int64, input any,
	do func(ctx context.Context, id int64) (string, error),
) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	ctx := r.Context()
	raw, _ := json.Marshal(input)
	actionID, err := s.store.RecordAction(ctx, store.ActionStart{
		Actor: store.ActorUI, IncidentID: id, TargetIncidentID: target, Tool: tool, Input: raw,
	})
	if err != nil {
		slog.Error("recording UI action", "incident", id, "err", err)
		http.Error(w, "could not record the action, so nothing was done", http.StatusInternalServerError)
		return
	}

	msg, err := do(ctx, id)
	q := url.Values{}
	var r2 *refusal
	switch {
	case err == nil:
		s.finishAction(ctx, actionID, store.ActionSucceeded, msg)
		q.Set("msg", msg)
	case errors.As(err, &r2):
		s.finishAction(ctx, actionID, store.ActionRejected, r2.reason)
		q.Set("error", r2.reason)
	default:
		s.finishAction(ctx, actionID, store.ActionFailed, err.Error())
		slog.Error("UI action failed", "tool", tool, "incident", id, "err", err)
		q.Set("error", "The controller failed to do that; its error is logged.")
	}
	http.Redirect(w, r, "/incidents/"+strconv.FormatInt(id, 10)+"?"+q.Encode(), http.StatusSeeOther)
}

// uiReason is the optional reason a human typed.
func uiReason(r *http.Request) string {
	reason := strings.TrimSpace(r.PostFormValue("reason"))
	if reason == "" {
		return "(no reason given)"
	}
	return reason
}

func formID(r *http.Request, key string) int64 {
	id, _ := strconv.ParseInt(r.PostFormValue(key), 10, 64)
	return id
}

type uiInput struct {
	Other  int64  `json:"other_id,omitempty"`
	Into   int64  `json:"into_id,omitempty"`
	Reason string `json:"reason,omitempty"`
}

func (s *Server) uiMerge(w http.ResponseWriter, r *http.Request) {
	into, reason := formID(r, "into"), uiReason(r)
	s.uiAction(w, r, "merge_incident", into, uiInput{Into: into, Reason: reason},
		func(ctx context.Context, id int64) (string, error) {
			if into == 0 {
				return "", refuse("choose an incident to merge into")
			}
			if err := checkReason(reason); err != nil {
				return "", err
			}
			return s.merge(ctx, uiActor, id, into, reason)
		})
}

func (s *Server) uiUnmerge(w http.ResponseWriter, r *http.Request) {
	var parent int64
	if id, err := strconv.ParseInt(r.PathValue("id"), 10, 64); err == nil {
		if in, _, err := s.store.GetIncident(r.Context(), id); err == nil && in.ParentID != nil {
			parent = *in.ParentID
		}
	}
	s.uiAction(w, r, "unmerge_incident", parent, uiInput{Into: parent},
		func(ctx context.Context, id int64) (string, error) {
			former, err := s.store.UnmergeIncident(ctx, id)
			if errors.Is(err, store.ErrNotFound) {
				return "", refuse("incident #%d is not merged", id)
			}
			if err != nil {
				return "", err
			}
			s.metrics.consolidations.WithLabelValues("unmerge", store.ActorUI).Inc()
			slog.Info("incident unmerged", "incident", id, "from", former)
			return "Unmerged from #" + strconv.FormatInt(former, 10) + ".", nil
		})
}

func (s *Server) uiLink(w http.ResponseWriter, r *http.Request) {
	other, reason := formID(r, "other"), uiReason(r)
	s.uiAction(w, r, "link_incident", other, uiInput{Other: other, Reason: reason},
		func(ctx context.Context, id int64) (string, error) {
			if other == 0 {
				return "", refuse("choose an incident to link")
			}
			if err := checkReason(reason); err != nil {
				return "", err
			}
			return s.link(ctx, uiActor, id, other, reason)
		})
}

func (s *Server) uiUnlink(w http.ResponseWriter, r *http.Request) {
	other := formID(r, "other")
	s.uiAction(w, r, "unlink_incident", other, uiInput{Other: other},
		func(ctx context.Context, id int64) (string, error) {
			removed, err := s.store.UnlinkIncidents(ctx, id, other)
			if err != nil {
				return "", err
			}
			if !removed {
				return "", refuse("#%d and #%d are not linked", id, other)
			}
			s.metrics.consolidations.WithLabelValues("unlink", store.ActorUI).Inc()
			return "Unlinked #" + strconv.FormatInt(other, 10) + ".", nil
		})
}
