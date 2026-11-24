// Package controller is the long-running half of alert-agent: it receives
// Alertmanager webhooks, records incidents, serves the UI and (in later
// phases) hosts the MCP tools that are the agent's only write path.
package controller

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/a-h/templ"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/jcwearn/alert-agent/internal/alertmanager"
	"github.com/jcwearn/alert-agent/internal/store"
	"github.com/jcwearn/alert-agent/internal/ui"
)

// Store is what the controller needs from persistence.
type Store interface {
	Ping(ctx context.Context) error
	RecordWebhook(ctx context.Context, raw []byte, p alertmanager.Payload) (store.RecordResult, error)
	ListIncidents(ctx context.Context, limit int) ([]store.Incident, error)
	GetIncident(ctx context.Context, id int64) (store.Incident, []store.IncidentAlert, error)
}

// maxWebhookBody bounds one delivery. Alertmanager sends every alert in the
// group, so a namespace-wide outage is the large case; this is generous.
const maxWebhookBody = 4 << 20

// Server holds the controller's two HTTP surfaces.
type Server struct {
	store   Store
	metrics *metrics
	now     func() time.Time
}

// New returns a Server backed by st.
func New(st Store) *Server {
	return &Server{store: st, metrics: newMetrics(), now: time.Now}
}

// UIHandler serves the UI, health checks and metrics. It is the port the
// gateway and Prometheus reach.
func (s *Server) UIHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "ok\n")
	})
	mux.HandleFunc("GET /readyz", s.readyz)
	mux.Handle("GET /metrics", promhttp.HandlerFor(s.metrics.registry, promhttp.HandlerOpts{}))
	mux.HandleFunc("GET /{$}", s.incidentList)
	mux.HandleFunc("GET /incidents/{id}", s.incidentDetail)
	return mux
}

// WebhookHandler serves the Alertmanager receiver. It is a separate port so
// the NetworkPolicy can admit Alertmanager to it and nothing else.
func (s *Server) WebhookHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /webhook/alertmanager", s.alertmanagerWebhook)
	return mux
}

// readyz checks the database: an outage there takes the pod out of the
// Service, but liveness stays green so it is not restarted for it.
func (s *Server) readyz(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()
	if err := s.store.Ping(ctx); err != nil {
		http.Error(w, "database unavailable", http.StatusServiceUnavailable)
		return
	}
	_, _ = io.WriteString(w, "ok\n")
}

func (s *Server) alertmanagerWebhook(w http.ResponseWriter, r *http.Request) {
	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxWebhookBody))
	if err != nil {
		s.metrics.webhooks.WithLabelValues("bad_request").Inc()
		http.Error(w, "body too large or unreadable", http.StatusRequestEntityTooLarge)
		return
	}
	p, err := alertmanager.Parse(raw)
	if err != nil {
		s.metrics.webhooks.WithLabelValues("bad_request").Inc()
		slog.Warn("rejected webhook", "err", err)
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	res, err := s.store.RecordWebhook(r.Context(), raw, p)
	if err != nil {
		// 5xx makes Alertmanager retry the delivery, which is what we want:
		// a dropped webhook is a dropped incident.
		s.metrics.webhooks.WithLabelValues("error").Inc()
		slog.Error("recording webhook", "group", p.GroupKey, "err", err)
		http.Error(w, "could not record delivery", http.StatusInternalServerError)
		return
	}

	s.metrics.webhooks.WithLabelValues("ok").Inc()
	if res.Created {
		s.metrics.incidents.WithLabelValues("opened").Inc()
	}
	if res.Resolved {
		s.metrics.incidents.WithLabelValues("resolved").Inc()
	}
	slog.Info("webhook recorded",
		"status", p.Status, "alertname", p.Label("alertname"), "namespace", p.Label("namespace"),
		"alerts", len(p.Alerts), "incident", res.IncidentID, "created", res.Created, "resolved", res.Resolved)
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) incidentList(w http.ResponseWriter, r *http.Request) {
	incidents, err := s.store.ListIncidents(r.Context(), 200)
	if err != nil {
		slog.Error("listing incidents", "err", err)
		http.Error(w, "could not load incidents", http.StatusInternalServerError)
		return
	}
	render(w, r, ui.IncidentList(incidents, s.now()))
}

func (s *Server) incidentDetail(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	in, alerts, err := s.store.GetIncident(r.Context(), id)
	if errors.Is(err, store.ErrNotFound) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		slog.Error("loading incident", "id", id, "err", err)
		http.Error(w, "could not load incident", http.StatusInternalServerError)
		return
	}
	render(w, r, ui.IncidentDetail(in, alerts))
}

func render(w http.ResponseWriter, r *http.Request, c templ.Component) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := c.Render(r.Context(), w); err != nil {
		slog.Error("render", "path", r.URL.Path, "err", err)
	}
}

// Run serves both handlers until ctx is cancelled, then drains in-flight
// requests for up to ten seconds.
func (s *Server) Run(ctx context.Context, uiAddr, webhookAddr string) error {
	servers := []*http.Server{
		{Addr: uiAddr, Handler: s.UIHandler(), ReadHeaderTimeout: 10 * time.Second},
		{Addr: webhookAddr, Handler: s.WebhookHandler(), ReadHeaderTimeout: 10 * time.Second},
	}

	errCh := make(chan error, len(servers))
	for _, srv := range servers {
		go func() {
			slog.Info("listening", "addr", srv.Addr)
			errCh <- srv.ListenAndServe()
		}()
	}

	var runErr error
	select {
	case runErr = <-errCh:
	case <-ctx.Done():
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	for _, srv := range servers {
		if err := srv.Shutdown(shutdownCtx); err != nil && runErr == nil {
			runErr = err
		}
	}
	if errors.Is(runErr, http.ErrServerClosed) {
		return nil
	}
	return runErr
}
