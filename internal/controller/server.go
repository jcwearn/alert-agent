// Package controller is the long-running half of alert-agent: it receives
// Alertmanager webhooks, records incidents, queues agent runs, serves the
// runner API and the UI, and (in later phases) hosts the MCP tools that are
// the agent's only write path.
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
	EnqueueRun(ctx context.Context, incidentID int64, hold time.Duration) (int64, bool, error)
	RunIncidents(ctx context.Context, runID int64) ([]int64, error)
	LeftoverIncidents(ctx context.Context, runID int64) ([]int64, error)
	RunByTokenHash(ctx context.Context, hash []byte) (store.Run, error)
	AppendRunEvents(ctx context.Context, runID int64, events []store.RunEvent) error
	FinishRun(ctx context.Context, runID int64, res store.RunResult) (bool, error)
	GetRun(ctx context.Context, id int64) (store.Run, error)
	ListRuns(ctx context.Context, incidentID int64) ([]store.Run, error)
	RunEvents(ctx context.Context, runID int64) ([]store.RunEvent, error)

	StartAction(ctx context.Context, runID, incidentID int64, tool string, input json.RawMessage) (int64, error)
	FinishAction(ctx context.Context, id int64, status, result string) error
	CountActions(ctx context.Context, runID int64, tool string) (int, error)
	InsertPullRequest(ctx context.Context, p store.PullRequest) (int64, error)
	PullRequestForIncident(ctx context.Context, incidentID int64) (store.PullRequest, error)
	OpenPullRequests(ctx context.Context) ([]store.PullRequest, error)
	UpdatePullRequest(ctx context.Context, id int64, st store.PRStatus) error
	MarkPullRequestNotified(ctx context.Context, id int64) (bool, error)
	SaveRunSession(ctx context.Context, runID int64, gz []byte, size int) error

	RecordAction(ctx context.Context, a store.ActionStart) (int64, error)
	IncidentActions(ctx context.Context, incidentID int64) ([]store.Action, error)
	MergeIncident(ctx context.Context, childID, intoID int64, mergedBy, reason string) (int64, error)
	UnmergeIncident(ctx context.Context, id int64) (int64, error)
	LinkIncidents(ctx context.Context, x, y int64, reason, actor string, runID int64) (bool, error)
	UnlinkIncidents(ctx context.Context, x, y int64) (bool, error)
	IncidentLinks(ctx context.Context, id int64) ([]store.Link, error)
	IncidentChildren(ctx context.Context, id int64) ([]store.Incident, error)
	RecentIncidents(ctx context.Context, since time.Time, limit int) ([]store.RecentIncident, error)

	GetPullRequest(ctx context.Context, id int64) (store.PullRequest, error)
	UpdatePullRequestHead(ctx context.Context, id int64, fromSHA, headSHA string, files []string) (bool, error)
	SetPullRequestTitle(ctx context.Context, id int64, title string) error
	EnqueueChat(ctx context.Context, m store.ChatMessage, lim store.ChatLimits) (store.ChatEnqueue, error)
	ChatMessages(ctx context.Context, threadID int64) ([]store.ChatMessage, error)
	TurnMessages(ctx context.Context, runID int64) ([]store.ChatMessage, error)
	FinishChatTurn(ctx context.Context, runID int64, answered bool, reply string) (store.ChatMessage, bool, error)
	LatestSession(ctx context.Context, sessionID string, beforeRunID int64) ([]byte, error)
	ActiveChatRun(ctx context.Context, threadID int64) (store.Run, bool, error)
	RunEventsAfter(ctx context.Context, runID int64, seq int) ([]store.RunEvent, error)
	AdvanceCommentsSince(ctx context.Context, prID int64, t time.Time) error
	MarkChatReacted(ctx context.Context, id int64) error
	SetChatMessageURL(ctx context.Context, id int64, url string) error

	GetThread(ctx context.Context, id int64) (store.Thread, error)
	IncidentThread(ctx context.Context, incidentID int64, create bool) (store.Thread, bool, error)
	CreateGeneralThread(ctx context.Context) (store.Thread, error)
	RecentThreads(ctx context.Context, limit int) ([]store.Thread, error)
	HasStoredSession(ctx context.Context, sessionID string, beforeRunID int64) (bool, error)
}

// Options configure the agent side of the controller.
type Options struct {
	// AgentEnabled queues a run for every new incident. Off, the controller
	// still records incidents but never starts the agent.
	AgentEnabled bool
	// PublicURL is the UI's address, for links in notifications.
	PublicURL string
	// MaxTurns caps each run's agent loop.
	MaxTurns int
	// RunHold is how long a new incident's run waits before it can start;
	// incidents opening in the same namespace meanwhile join that run.
	RunHold time.Duration
	// RecentWindow is how far back a run's context lists other incidents,
	// and how old a resolved incident may be to take a merge.
	RecentWindow time.Duration
	// ChatMaxPerThreadHour caps the chat turns one conversation can start in
	// an hour.
	ChatMaxPerThreadHour int
	// ChatMaxPerHour caps the chat turns all PRs together start in an hour.
	// Chat turns do not count against the alert runs' RUN_MAX_PER_HOUR.
	ChatMaxPerHour int
}

// Waker is told when there may be work for the dispatcher.
type Waker interface{ Wake() }

// maxWebhookBody bounds one delivery. Alertmanager sends every alert in the
// group, so a namespace-wide outage is the large case; this is generous.
const maxWebhookBody = 4 << 20

// Server holds the controller's HTTP surfaces.
type Server struct {
	store    Store
	opts     Options
	metrics  *metrics
	now      func() time.Time
	waker    Waker
	notifier Notifier
	prs      *PRs
	reviewer Reviewer
	comments *Comments
}

// New returns a Server backed by st. waker and notifier may be nil.
func New(st Store, opts Options, waker Waker, notifier Notifier) *Server {
	return &Server{store: st, opts: opts, metrics: newMetrics(), now: time.Now, waker: waker, notifier: notifier}
}

// EnablePRs turns on the PR path: propose_pr for runners, the PR poller,
// and review notifications through reviewer (which may be nil).
func (s *Server) EnablePRs(p *PRs, reviewer Reviewer) {
	s.prs, s.reviewer = p, reviewer
}

func (s *Server) incidentURL(id int64) string {
	return strings.TrimRight(s.opts.PublicURL, "/") + "/incidents/" + strconv.FormatInt(id, 10)
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
	mux.HandleFunc("GET /runs/{id}", s.runDetail)
	mux.HandleFunc("POST /incidents/{id}/merge", s.uiMerge)
	mux.HandleFunc("POST /incidents/{id}/unmerge", s.uiUnmerge)
	mux.HandleFunc("POST /incidents/{id}/link", s.uiLink)
	mux.HandleFunc("POST /incidents/{id}/unlink", s.uiUnlink)
	mux.HandleFunc("GET /runs/{id}/stream", s.runStream)
	mux.Handle("GET /static/", ui.StaticHandler())
	mux.HandleFunc("GET /api/chat/threads", s.apiThreads)
	mux.HandleFunc("POST /api/chat/threads", s.apiCreateThread)
	mux.HandleFunc("GET /api/chat/threads/{id}", s.apiThread)
	mux.HandleFunc("POST /api/chat/threads/{id}/messages", s.apiPostMessage)
	// The UI has no login (it is tailnet-only), so a form post from another
	// site in the same browser must not be able to merge incidents.
	return http.NewCrossOriginProtection().Handler(mux)
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
		if p.Status == alertmanager.StatusFiring && s.opts.AgentEnabled {
			s.enqueue(r.Context(), res.IncidentID, s.opts.RunHold)
		}
	}
	if res.Resolved {
		s.metrics.incidents.WithLabelValues("resolved").Inc()
	}
	slog.Info("webhook recorded",
		"status", p.Status, "alertname", p.Label("alertname"), "namespace", p.Label("namespace"),
		"alerts", len(p.Alerts), "incident", res.IncidentID, "created", res.Created, "resolved", res.Resolved)
	w.WriteHeader(http.StatusNoContent)
}

// enqueue queues a diagnosis, held for hold so a burst in one namespace
// becomes one run. A failure here is logged, not returned: the incident is
// recorded, and Alertmanager retrying the delivery would only fold into the
// same incident without queueing anything.
func (s *Server) enqueue(ctx context.Context, incidentID int64, hold time.Duration) {
	runID, batched, err := s.store.EnqueueRun(ctx, incidentID, hold)
	if err != nil {
		slog.Error("queueing run", "incident", incidentID, "err", err)
		return
	}
	if batched {
		slog.Info("incident joined a held run", "run", runID, "incident", incidentID)
		return
	}
	s.metrics.runs.WithLabelValues(store.RunQueued).Inc()
	slog.Info("run queued", "run", runID, "incident", incidentID, "hold", hold)
	if s.waker != nil {
		s.waker.Wake()
		if hold > 0 {
			time.AfterFunc(hold+time.Second, s.waker.Wake)
		}
	}
}

func (s *Server) incidentList(w http.ResponseWriter, r *http.Request) {
	incidents, err := s.store.ListIncidents(r.Context(), 200)
	if err != nil {
		slog.Error("listing incidents", "err", err)
		http.Error(w, "could not load incidents", http.StatusInternalServerError)
		return
	}
	var awaiting []store.PullRequest
	if s.prs != nil {
		if awaiting, err = s.store.OpenPullRequests(r.Context()); err != nil {
			slog.Error("listing open PRs", "err", err)
		}
	}
	render(w, r, ui.IncidentList(ui.GroupIncidents(incidents), awaiting, s.now()))
}

func (s *Server) incidentDetail(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	ctx := r.Context()
	in, alerts, err := s.store.GetIncident(ctx, id)
	if errors.Is(err, store.ErrNotFound) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		slog.Error("loading incident", "id", id, "err", err)
		http.Error(w, "could not load incident", http.StatusInternalServerError)
		return
	}
	page := ui.IncidentPage{
		Incident: in, Alerts: alerts,
		Notice: r.URL.Query().Get("msg"), Problem: r.URL.Query().Get("error"),
	}
	if page.Runs, err = s.store.ListRuns(ctx, id); err != nil {
		slog.Error("loading runs", "incident", id, "err", err)
		http.Error(w, "could not load runs", http.StatusInternalServerError)
		return
	}
	if p, err := s.store.PullRequestForIncident(ctx, id); err == nil {
		page.PR = &p
	} else if !errors.Is(err, store.ErrNotFound) {
		slog.Error("loading PR", "incident", id, "err", err)
	}
	if in.ParentID != nil {
		if parent, _, err := s.store.GetIncident(ctx, *in.ParentID); err == nil {
			page.Parent = &parent
		}
	}
	// The rest decorates the page; a failure is logged and the page still renders.
	if page.Children, err = s.store.IncidentChildren(ctx, id); err != nil {
		slog.Error("loading merged incidents", "incident", id, "err", err)
	}
	if page.Links, err = s.store.IncidentLinks(ctx, id); err != nil {
		slog.Error("loading links", "incident", id, "err", err)
	}
	if page.Actions, err = s.store.IncidentActions(ctx, id); err != nil {
		slog.Error("loading audit trail", "incident", id, "err", err)
	}
	if recent, err := s.store.ListIncidents(ctx, 200); err == nil {
		page.Candidates = ui.MergeCandidates(in, recent, s.now())
	} else {
		slog.Error("loading merge candidates", "incident", id, "err", err)
	}
	render(w, r, ui.IncidentDetail(page))
}

func (s *Server) runDetail(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	run, err := s.store.GetRun(r.Context(), id)
	if errors.Is(err, store.ErrNotFound) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		slog.Error("loading run", "id", id, "err", err)
		http.Error(w, "could not load run", http.StatusInternalServerError)
		return
	}
	// A general conversation's turn has no incident.
	var in store.Incident
	if run.IncidentID != 0 {
		if in, _, err = s.store.GetIncident(r.Context(), run.IncidentID); err != nil {
			slog.Error("loading run incident", "id", id, "err", err)
			http.Error(w, "could not load incident", http.StatusInternalServerError)
			return
		}
	}
	events, err := s.store.RunEvents(r.Context(), id)
	if err != nil {
		slog.Error("loading run events", "id", id, "err", err)
		http.Error(w, "could not load transcript", http.StatusInternalServerError)
		return
	}
	covers, err := s.store.RunIncidents(r.Context(), id)
	if err != nil {
		slog.Error("loading run incidents", "id", id, "err", err)
	}
	render(w, r, ui.RunDetail(in, run, covers, ui.Transcript(events)))
}

func render(w http.ResponseWriter, r *http.Request, c templ.Component) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := c.Render(r.Context(), w); err != nil {
		slog.Error("render", "path", r.URL.Path, "err", err)
	}
}

// Run serves all three handlers until ctx is cancelled, then drains
// in-flight requests for up to ten seconds.
func (s *Server) Run(ctx context.Context, uiAddr, webhookAddr, runnerAddr string) error {
	servers := []*http.Server{
		{Addr: uiAddr, Handler: s.UIHandler(), ReadHeaderTimeout: 10 * time.Second},
		{Addr: webhookAddr, Handler: s.WebhookHandler(), ReadHeaderTimeout: 10 * time.Second},
		{Addr: runnerAddr, Handler: s.RunnerHandler(), ReadHeaderTimeout: 10 * time.Second},
	}

	if s.prs != nil {
		go s.pollPRs(ctx)
		if s.comments != nil {
			go s.pollComments(ctx)
		}
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
