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
	in, alerts, err := s.store.GetIncident(r.Context(), run.IncidentID)
	if err != nil {
		slog.Error("runner context", "run", run.ID, "err", err)
		http.Error(w, "could not load incident", http.StatusInternalServerError)
		return
	}
	rc := runnerapi.Context{
		RunID:     run.ID,
		SessionID: run.SessionID,
		Model:     run.Model,
		MaxTurns:  s.opts.MaxTurns,
		UIURL:     s.incidentURL(in.ID),
		ProposePR: s.prs != nil,
		Incident: runnerapi.Incident{
			ID: in.ID, AlertName: in.AlertName, Namespace: in.Namespace, Severity: in.Severity,
			Summary: in.Summary, AlertStatus: in.AlertStatus, OpenedAt: in.OpenedAt,
		},
	}
	for _, a := range alerts {
		rc.Alerts = append(rc.Alerts, runnerapi.Alert{
			Status: a.Status, Labels: a.Labels, Annotations: a.Annotations, StartsAt: a.StartsAt,
		})
	}
	writeJSON(w, rc)
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
// humans, and let the dispatcher start the next one.
func (s *Server) RunFinished(runID int64) {
	ctx := context.Background()
	run, err := s.store.GetRun(ctx, runID)
	if err != nil {
		slog.Error("loading finished run", "run", runID, "err", err)
		return
	}
	s.metrics.runs.WithLabelValues(run.Status).Inc()
	if s.notifier != nil {
		in, _, err := s.store.GetIncident(ctx, run.IncidentID)
		if err == nil {
			var pr *store.PullRequest
			if p, err := s.store.PullRequestForIncident(ctx, run.IncidentID); err == nil && p.RunID == run.ID {
				pr = &p
			}
			go s.notifier.RunFinished(context.Background(), in, run, pr)
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
