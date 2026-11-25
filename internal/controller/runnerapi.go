package controller

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/jcwearn/alert-agent/internal/dispatch"
	"github.com/jcwearn/alert-agent/internal/runnerapi"
	"github.com/jcwearn/alert-agent/internal/store"
)

type runKey struct{}

// RunnerHandler serves the runner API. It is a third port, admitted by the
// NetworkPolicy only from runner pods, and every request must carry the
// bearer token of a run that is currently running.
func (s *Server) RunnerHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET "+runnerapi.PathContext, s.runnerAuth(s.runnerContext))
	mux.HandleFunc("POST "+runnerapi.PathEvents, s.runnerAuth(s.runnerEvents))
	mux.HandleFunc("POST "+runnerapi.PathFinish, s.runnerAuth(s.runnerFinish))
	mux.HandleFunc("POST "+runnerapi.PathSession, s.runnerAuth(s.runnerSession))
	mux.HandleFunc("GET "+runnerapi.PathSession, s.runnerAuth(s.runnerResumeSession))
	mcpHandler := s.mcpHandler()
	mux.Handle(runnerapi.PathMCP, s.runnerAuth(mcpHandler.ServeHTTP))
	return mux
}

func (s *Server) runnerAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		token, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		if !ok || token == "" {
			http.Error(w, "missing bearer token", http.StatusUnauthorized)
			return
		}
		run, err := s.store.RunByTokenHash(r.Context(), dispatch.HashToken(token))
		if errors.Is(err, store.ErrNotFound) {
			http.Error(w, "unknown or finished run", http.StatusUnauthorized)
			return
		}
		if err != nil {
			slog.Error("runner auth", "err", err)
			http.Error(w, "lookup failed", http.StatusInternalServerError)
			return
		}
		next(w, r.WithContext(context.WithValue(r.Context(), runKey{}, run)))
	}
}

func runFrom(r *http.Request) store.Run { return r.Context().Value(runKey{}).(store.Run) }

func (s *Server) runnerContext(w http.ResponseWriter, r *http.Request) {
	run := runFrom(r)
	rc, err := s.buildContext(r.Context(), run)
	if err != nil {
		slog.Error("runner context", "run", run.ID, "err", err)
		http.Error(w, "could not load incident", http.StatusInternalServerError)
		return
	}
	writeJSON(w, rc)
}

// maxRecent bounds how many other incidents a run's context lists.
const maxRecent = 25

func (s *Server) buildContext(ctx context.Context, run store.Run) (runnerapi.Context, error) {
	in, alerts, err := s.store.GetIncident(ctx, run.IncidentID)
	if err != nil {
		return runnerapi.Context{}, err
	}
	if run.Kind == store.KindChat {
		return s.chatContext(ctx, run, in)
	}
	rc := runnerapi.Context{
		Kind:        runnerapi.KindDiagnose,
		RunID:       run.ID,
		SessionID:   run.SessionID,
		Model:       run.Model,
		MaxTurns:    s.opts.MaxTurns,
		UIURL:       s.incidentURL(in.ID),
		ProposePR:   s.prs != nil,
		Consolidate: true,
		Incident:    apiIncident(in),
		Alerts:      apiAlerts(alerts),
	}

	members, err := s.store.RunIncidents(ctx, run.ID)
	if err != nil {
		return rc, err
	}
	own := map[int64]bool{in.ID: true}
	for _, id := range members {
		if own[id] {
			continue
		}
		own[id] = true
		m, malerts, err := s.store.GetIncident(ctx, id)
		if err != nil {
			return rc, err
		}
		if m.Merged() || m.AlertStatus != "firing" {
			continue
		}
		rc.Batch = append(rc.Batch, runnerapi.BatchIncident{Incident: apiIncident(m), Alerts: apiAlerts(malerts)})
	}

	recent, err := s.store.RecentIncidents(ctx, s.now().Add(-s.opts.RecentWindow), maxRecent+len(own))
	if err != nil {
		return rc, err
	}
	for _, ri := range recent {
		if own[ri.ID] || len(rc.Recent) >= maxRecent {
			continue
		}
		rc.Recent = append(rc.Recent, runnerapi.RecentIncident{
			ID: ri.ID, AlertName: ri.AlertName, Namespace: ri.Namespace, AlertStatus: ri.AlertStatus,
			OpenedAt: ri.OpenedAt, ParentID: ri.ParentID, Objects: identifyingLabels(ri.Labels),
			Summary: reportSummary(ri.Report), PRNumber: ri.PRNumber,
		})
	}

	if s.prs != nil {
		open, err := s.store.OpenPullRequests(ctx)
		if err != nil {
			return rc, err
		}
		for _, pr := range open {
			rc.OpenPRs = append(rc.OpenPRs, runnerapi.OpenPR{
				Number: pr.Number, Title: pr.Title, IncidentID: pr.IncidentID, Files: pr.Files,
			})
		}
	}
	return rc, nil
}

func apiIncident(in store.Incident) runnerapi.Incident {
	return runnerapi.Incident{
		ID: in.ID, AlertName: in.AlertName, Namespace: in.Namespace, Severity: in.Severity,
		Summary: in.Summary, AlertStatus: in.AlertStatus, OpenedAt: in.OpenedAt,
	}
}

func apiAlerts(alerts []store.IncidentAlert) []runnerapi.Alert {
	out := make([]runnerapi.Alert, 0, len(alerts))
	for _, a := range alerts {
		out = append(out, runnerapi.Alert{
			Status: a.Status, Labels: a.Labels, Annotations: a.Annotations, StartsAt: a.StartsAt,
		})
	}
	return out
}

// objectLabels name the thing an alert is about; two incidents sharing one
// are candidates for the same problem.
var objectLabels = []string{
	"pod", "container", "deployment", "statefulset", "daemonset", "replicaset", "job_name", "job", "cronjob",
	"persistentvolumeclaim", "node", "instance", "service", "certificate", "name",
}

// identifyingLabels returns the distinct object labels across alerts, as
// key=value, in a stable order.
func identifyingLabels(alerts []map[string]string) []string {
	seen := map[string]bool{}
	var out []string
	for _, key := range objectLabels {
		for _, labels := range alerts {
			if v := labels[key]; v != "" && !seen[key+"="+v] {
				seen[key+"="+v] = true
				out = append(out, key+"="+v)
			}
		}
	}
	if len(out) > 12 {
		out = append(out[:12], "...")
	}
	return out
}

// maxSummary bounds the diagnosis summary each recent incident carries.
const maxSummary = 500

// reportSummary is the Summary (or Merged) section of a run report, which is
// all another run needs to recognise the problem.
func reportSummary(report string) string {
	for _, h := range []string{"## Summary", "## Merged"} {
		i := strings.Index(report, h)
		if i < 0 {
			continue
		}
		body := report[i+len(h):]
		if j := strings.Index(body, "\n## "); j >= 0 {
			body = body[:j]
		}
		body = strings.TrimSpace(body)
		if len(body) > maxSummary {
			cut := maxSummary
			for cut > 0 && !utf8.RuneStart(body[cut]) {
				cut--
			}
			body = body[:cut] + "..."
		}
		return body
	}
	return ""
}

// maxEventBatch bounds one upload. A single stream-json line can carry a
// large tool result, so this is generous.
const maxEventBatch = 32 << 20

func (s *Server) runnerEvents(w http.ResponseWriter, r *http.Request) {
	run := runFrom(r)
	var batch []runnerapi.Event
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxEventBatch)).Decode(&batch); err != nil {
		http.Error(w, "bad event batch: "+err.Error(), http.StatusBadRequest)
		return
	}
	events := make([]store.RunEvent, 0, len(batch))
	for _, e := range batch {
		if !json.Valid(e.Payload) {
			http.Error(w, "event "+strconv.Itoa(e.Seq)+" payload is not JSON", http.StatusBadRequest)
			return
		}
		events = append(events, store.RunEvent{Seq: e.Seq, Type: e.Type, Payload: e.Payload})
	}
	if err := s.store.AppendRunEvents(r.Context(), run.ID, events); err != nil {
		slog.Error("storing run events", "run", run.ID, "err", err)
		http.Error(w, "could not store events", http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) runnerFinish(w http.ResponseWriter, r *http.Request) {
	run := runFrom(r)
	var fin runnerapi.Finish
	if err := json.NewDecoder(io.LimitReader(r.Body, 4<<20)).Decode(&fin); err != nil {
		http.Error(w, "bad finish body: "+err.Error(), http.StatusBadRequest)
		return
	}
	if fin.Status != store.RunSucceeded && fin.Status != store.RunFailed {
		http.Error(w, "status must be succeeded or failed", http.StatusBadRequest)
		return
	}
	finished, err := s.store.FinishRun(r.Context(), run.ID, store.RunResult{
		Status: fin.Status, Report: fin.Report, Error: fin.Error,
		NumTurns: fin.NumTurns, DurationMS: fin.DurationMS, Usage: fin.Usage,
	})
	if err != nil {
		slog.Error("finishing run", "run", run.ID, "err", err)
		http.Error(w, "could not record result", http.StatusInternalServerError)
		return
	}
	if finished {
		slog.Info("run finished", "run", run.ID, "incident", run.IncidentID, "status", fin.Status)
		s.RunFinished(run.ID)
	}
	w.WriteHeader(http.StatusNoContent)
}

// RunFinished is called once per run, however it ended: count it, tell the
// humans, give batch members the run left alone a run of their own, and let
// the dispatcher start the next one.
func (s *Server) RunFinished(runID int64) {
	ctx := context.Background()
	run, err := s.store.GetRun(ctx, runID)
	if err != nil {
		slog.Error("loading finished run", "run", runID, "err", err)
		return
	}
	s.metrics.runs.WithLabelValues(run.Status).Inc()
	// A chat turn's outcome is its reply in the conversation, not a diagnosis.
	if run.Kind == store.KindChat {
		s.chatTurnFinished(ctx, run)
		if s.waker != nil {
			s.waker.Wake()
		}
		return
	}
	if s.notifier != nil {
		in, _, err := s.store.GetIncident(ctx, run.IncidentID)
		switch {
		case err != nil:
		case in.Merged() && in.MergedBy == agentActor(run.ID).mergedBy():
			// The run folded its own incident into another, whose diagnosis
			// covers it; the merge is in the UI's audit trail.
			slog.Info("not notifying: run merged its incident", "run", run.ID, "into", *in.ParentID)
		default:
			var pr *store.PullRequest
			if p, err := s.store.PullRequestForIncident(ctx, run.IncidentID); err == nil && p.RunID == run.ID {
				pr = &p
			}
			go s.notifier.RunFinished(context.Background(), in, run, pr)
		}
	}
	// Only after a run that did its job: a failed run (a usage limit, say)
	// would fail the same way for each of them.
	if run.Status == store.RunSucceeded {
		left, err := s.store.LeftoverIncidents(ctx, run.ID)
		if err != nil {
			slog.Error("finding leftover incidents", "run", run.ID, "err", err)
		}
		for _, id := range left {
			slog.Info("batch member left apart; queueing its own run", "run", run.ID, "incident", id)
			s.enqueue(ctx, id, 0)
		}
	}
	if s.waker != nil {
		s.waker.Wake()
	}
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(v); err != nil {
		slog.Error("encode response", "err", err)
	}
}
