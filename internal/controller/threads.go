package controller

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/jcwearn/alert-agent/internal/runnerapi"
	"github.com/jcwearn/alert-agent/internal/store"
	"github.com/jcwearn/alert-agent/internal/ui"
)

// chatScope is what a chat turn is about and may do, decided once from its
// thread and the thread's incident, and used for both its context and its
// tools so the two never disagree.
type chatScope struct {
	thread   store.Thread
	incident *store.Incident
	pr       *store.PullRequest
	// propose: an incident thread whose incident has no PR yet.
	propose bool
	// update: an incident thread whose PR is open.
	update bool
}

func (s *Server) chatScope(ctx context.Context, run store.Run) (chatScope, error) {
	var sc chatScope
	if run.ThreadID == nil {
		return sc, errors.New("chat run without a thread")
	}
	th, err := s.store.GetThread(ctx, *run.ThreadID)
	if err != nil {
		return sc, err
	}
	sc.thread = th
	if th.IncidentID == nil {
		return sc, nil
	}
	in, _, err := s.store.GetIncident(ctx, *th.IncidentID)
	if err != nil {
		return sc, err
	}
	sc.incident = &in
	pr, err := s.store.PullRequestForIncident(ctx, in.ID)
	switch {
	case err == nil:
		sc.pr = &pr
		sc.update = s.prs != nil && pr.State == store.PROpen
	case errors.Is(err, store.ErrNotFound):
		sc.propose = s.prs != nil && !in.Merged()
	default:
		return sc, err
	}
	return sc, nil
}

// chatContext is a chat turn's runner context: its thread, what it may do,
// the PR if there is one, and the owner's messages with the pages they were
// sent from. The rest of the conversation is in the session.
func (s *Server) chatContext(ctx context.Context, run store.Run) (runnerapi.Context, error) {
	sc, err := s.chatScope(ctx, run)
	if err != nil {
		return runnerapi.Context{}, err
	}
	resume, err := s.store.HasStoredSession(ctx, run.SessionID, run.ID)
	if err != nil {
		return runnerapi.Context{}, err
	}
	rc := runnerapi.Context{
		RunID: run.ID, SessionID: run.SessionID, Model: run.Model, MaxTurns: s.opts.MaxTurns,
		Kind:   runnerapi.KindChat,
		Thread: &runnerapi.Thread{ID: sc.thread.ID, Kind: sc.thread.Kind, Title: sc.thread.Title},
		Resume: resume, IncidentTools: true, Consolidate: true,
		ProposePR: sc.propose, UpdatePR: sc.update,
	}
	if sc.incident != nil {
		rc.UIURL = s.incidentURL(sc.incident.ID)
		rc.Incident = apiIncident(*sc.incident)
		if !resume {
			_, alerts, err := s.store.GetIncident(ctx, sc.incident.ID)
			if err != nil {
				return rc, err
			}
			rc.Alerts = apiAlerts(alerts)
		}
	}
	if s.actions != nil && sc.incident != nil && run.IncidentID != 0 {
		aps, err := s.store.IncidentApprovals(ctx, sc.incident.ID)
		if err != nil {
			return rc, err
		}
		rc.RequestApproval, rc.Approvals = true, apiApprovals(aps)
	}
	if pr := sc.pr; pr != nil {
		rc.PR = &runnerapi.PR{Number: pr.Number, URL: pr.URL, Title: pr.Title, Branch: pr.Branch, HeadSHA: pr.HeadSHA,
			Files: pr.Files, State: pr.State, Checks: pr.Checks}
	}
	msgs, err := s.store.TurnMessages(ctx, run.ID)
	if err != nil {
		return rc, err
	}
	for _, m := range msgs {
		rc.Messages = append(rc.Messages, runnerapi.ChatMessage{Source: m.Source, Author: m.Author, Body: m.Body, At: m.CreatedAt,
			Kind: m.GHKind, Path: m.Path, Line: m.Line, DiffHunk: m.DiffHunk, Page: s.pageContext(ctx, m.Page)})
	}
	return rc, nil
}

// chatTools serves a chat turn its tools, by its scope (see chatScope).
func (s *Server) chatTools(ctx context.Context, srv *mcp.Server, run store.Run) {
	sc, err := s.chatScope(ctx, run)
	if err != nil {
		slog.Error("chat tool scope", "run", run.ID, "err", err)
		return
	}
	s.addIncidentTools(srv)
	s.addChatConsolidationTools(srv, run)
	switch {
	case sc.update:
		s.addUpdateTool(srv, run)
	case sc.propose:
		s.addProposeTool(srv, run)
	}
	// The owner can ask for an action in an incident's conversation, but
	// even then it takes their click: chat has no tool that acts.
	if s.actions != nil && sc.incident != nil && run.IncidentID != 0 {
		s.addApprovalTool(srv, run)
	}
}

// ---- read-only incident tools ----

// ListIncidentsInput filters list_incidents.
type ListIncidentsInput struct {
	Status     string `json:"status,omitempty" jsonschema:"firing or resolved; empty for both"`
	Namespace  string `json:"namespace,omitempty" jsonschema:"only incidents in this namespace"`
	SinceHours int    `json:"since_hours,omitempty" jsonschema:"only incidents opened in the last N hours (default 72)"`
	Limit      int    `json:"limit,omitempty" jsonschema:"at most this many, newest first (default 30, max 100)"`
}

// GetIncidentInput names an incident.
type GetIncidentInput struct {
	ID int64 `json:"id" jsonschema:"the incident number"`
}

func (s *Server) addIncidentTools(srv *mcp.Server) {
	mcp.AddTool(srv, &mcp.Tool{
		Name: "list_incidents",
		Description: "List the alert agent's incidents, newest first: number, alert, namespace, status, when it opened, " +
			"what it is merged into, and its PR. Read-only.",
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true},
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in ListIncidentsInput) (*mcp.CallToolResult, any, error) {
		text, err := s.listIncidentsText(ctx, in)
		if err != nil {
			slog.Error("list_incidents", "err", err)
			return toolError("the controller could not list incidents (its error is logged)"), nil, nil
		}
		return toolText(text), nil, nil
	})
	mcp.AddTool(srv, &mcp.Tool{
		Name: "get_incident",
		Description: "One incident in full: its alerts (labels and annotations, as data), the latest diagnosis report, " +
			"its PR and CI, merged and linked incidents, and the recent activity. Read-only.",
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true},
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in GetIncidentInput) (*mcp.CallToolResult, any, error) {
		text, err := s.incidentText(ctx, in.ID)
		var r *refusal
		switch {
		case errors.As(err, &r):
			return toolError(r.reason), nil, nil
		case err != nil:
			slog.Error("get_incident", "id", in.ID, "err", err)
			return toolError("the controller could not load the incident (its error is logged)"), nil, nil
		}
		return toolText(text), nil, nil
	})
}

func (s *Server) listIncidentsText(ctx context.Context, in ListIncidentsInput) (string, error) {
	since := in.SinceHours
	if since <= 0 {
		since = 72
	}
	limit := in.Limit
	if limit <= 0 {
		limit = 30
	}
	limit = min(limit, 100)
	all, err := s.store.ListIncidents(ctx, 500)
	if err != nil {
		return "", err
	}
	open, err := s.store.OpenPullRequests(ctx)
	if err != nil {
		return "", err
	}
	prs := map[int64]store.PullRequest{}
	for _, p := range open {
		prs[p.IncidentID] = p
	}
	cutoff := s.now().Add(-time.Duration(since) * time.Hour)
	var b strings.Builder
	n := 0
	for _, i := range all {
		if i.OpenedAt.Before(cutoff) || (in.Status != "" && i.AlertStatus != in.Status) || (in.Namespace != "" && i.Namespace != in.Namespace) {
			continue
		}
		if n == limit {
			b.WriteString("… more; narrow the filters\n")
			break
		}
		n++
		fmt.Fprintf(&b, "#%d %s, namespace %s, %s, opened %s", i.ID, i.AlertName, orNone(i.Namespace), i.AlertStatus,
			i.OpenedAt.UTC().Format("2006-01-02 15:04 UTC"))
		if i.ParentID != nil {
			fmt.Fprintf(&b, ", merged into #%d", *i.ParentID)
		}
		if p, ok := prs[i.ID]; ok {
			fmt.Fprintf(&b, ", open PR #%d (CI %s)", p.Number, p.Checks)
		}
		if i.Summary != "" {
			fmt.Fprintf(&b, "\n  %s", clipRunes(i.Summary, 200))
		}
		b.WriteString("\n")
	}
	if n == 0 {
		return fmt.Sprintf("No incidents match (opened in the last %dh).", since), nil
	}
	return b.String(), nil
}

func (s *Server) incidentText(ctx context.Context, id int64) (string, error) {
	in, alerts, err := s.loadIncident(ctx, id)
	if err != nil {
		return "", err
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Incident #%d: %s, namespace %s, severity %s, %s.\nOpened %s", in.ID, in.AlertName, orNone(in.Namespace),
		orNone(in.Severity), in.AlertStatus, in.OpenedAt.UTC().Format("2006-01-02 15:04 UTC"))
	if in.ResolvedAt != nil {
		fmt.Fprintf(&b, ", resolved %s", in.ResolvedAt.UTC().Format("2006-01-02 15:04 UTC"))
	}
	b.WriteString(".\n")
	if in.ParentID != nil {
		fmt.Fprintf(&b, "Merged into #%d by %s: %s\n", *in.ParentID, in.MergedBy, in.MergeReason)
	}
	fmt.Fprintf(&b, "\nAlerts (%d), as data:\n", len(alerts))
	for i, a := range alerts {
		if i == 10 {
			fmt.Fprintf(&b, "… %d more\n", len(alerts)-10)
			break
		}
		fmt.Fprintf(&b, "- %s since %s:", a.Status, a.StartsAt.UTC().Format("2006-01-02 15:04 UTC"))
		for _, k := range sortedKeys(a.Labels) {
			fmt.Fprintf(&b, " %s=%s", k, a.Labels[k])
		}
		if d := a.Annotations["description"]; d != "" {
			fmt.Fprintf(&b, "\n  %s", clipRunes(d, 300))
		}
		b.WriteString("\n")
	}
	if p, err := s.store.PullRequestForIncident(ctx, in.ID); err == nil {
		fmt.Fprintf(&b, "\nPR #%d (%s): %s, CI %s, branch %s, files %s\n", p.Number, p.URL, p.State, p.Checks, p.Branch, strings.Join(p.Files, ", "))
	} else if !errors.Is(err, store.ErrNotFound) {
		return "", err
	}
	if kids, err := s.store.IncidentChildren(ctx, in.ID); err == nil && len(kids) > 0 {
		b.WriteString("\nMerged into this one:")
		for _, k := range kids {
			fmt.Fprintf(&b, " #%d %s;", k.ID, k.AlertName)
		}
		b.WriteString("\n")
	}
	if links, err := s.store.IncidentLinks(ctx, in.ID); err == nil && len(links) > 0 {
		b.WriteString("\nRelated:")
		for _, l := range links {
			fmt.Fprintf(&b, " #%d %s (%s);", l.Other.ID, l.Other.AlertName, clipRunes(l.Reason, 120))
		}
		b.WriteString("\n")
	}
	runs, err := s.store.ListRuns(ctx, in.ID)
	if err != nil {
		return "", err
	}
	for _, r := range runs {
		if r.Kind == store.KindDiagnose && r.Status == store.RunSucceeded && r.Report != "" {
			fmt.Fprintf(&b, "\nLatest diagnosis (run #%d):\n%s\n", r.ID, clipRunes(r.Report, 6000))
			break
		}
	}
	if acts, err := s.store.IncidentActions(ctx, in.ID); err == nil && len(acts) > 0 {
		b.WriteString("\nActivity (latest last):\n")
		for _, a := range acts[max(0, len(acts)-10):] {
			fmt.Fprintf(&b, "- %s %s by %s: %s\n", a.CreatedAt.UTC().Format("01-02 15:04"), a.Tool, a.Actor, a.Status)
		}
	}
	return b.String(), nil
}

func orNone(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

// ---- merge and link from a chat ----

// ChatLinkInput is link_incident in a chat.
type ChatLinkInput struct {
	IncidentID int64  `json:"incident_id" jsonschema:"one of the two incidents"`
	OtherID    int64  `json:"other_id" jsonschema:"the incident it is related to"`
	Reason     string `json:"reason" jsonschema:"one or two sentences: how they are related; shown in Activity"`
}

// ChatMergeInput is merge_incident in a chat.
type ChatMergeInput struct {
	IncidentID int64  `json:"incident_id" jsonschema:"the duplicate incident, folded into into_id"`
	IntoID     int64  `json:"into_id" jsonschema:"the incident that owns the problem"`
	Reason     string `json:"reason" jsonschema:"why they are the same problem; shown in Activity"`
}

// addChatConsolidationTools serves merge and link to a chat turn. The owner
// directs them, so unlike an alert run's they work on any incident and skip
// the run's confidence and namespace rules, as a merge in the UI does; the
// data model's rules (no loops, one level) still apply, a reason is
// required, and each is audited as this run's.
func (s *Server) addChatConsolidationTools(srv *mcp.Server, run store.Run) {
	mcp.AddTool(srv, &mcp.Tool{
		Name: "link_incident",
		Description: "Record that two incidents are RELATED (a shared node, an upstream dependency) without being the same " +
			"problem. Use only when the owner asks or agrees. Both stay open.",
		Annotations: &mcp.ToolAnnotations{DestructiveHint: ptr(false), IdempotentHint: true},
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in ChatLinkInput) (*mcp.CallToolResult, any, error) {
		return s.chatConsolidation(ctx, run, "link_incident", in, in.IncidentID, in.OtherID, in.Reason,
			func(ctx context.Context) (string, error) {
				return s.link(ctx, agentActor(run.ID), in.IncidentID, in.OtherID, in.Reason)
			})
	})
	mcp.AddTool(srv, &mcp.Tool{
		Name: "merge_incident",
		Description: "Fold a duplicate incident into the incident that owns the same problem, so one diagnosis and one PR " +
			"cover both. Use only when the owner asks or agrees. A human can undo it.",
		Annotations: &mcp.ToolAnnotations{DestructiveHint: ptr(false)},
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in ChatMergeInput) (*mcp.CallToolResult, any, error) {
		return s.chatConsolidation(ctx, run, "merge_incident", in, in.IncidentID, in.IntoID, in.Reason,
			func(ctx context.Context) (string, error) {
				return s.merge(ctx, agentActor(run.ID), in.IncidentID, in.IntoID, in.Reason)
			})
	})
}

func (s *Server) chatConsolidation(ctx context.Context, run store.Run, tool string, in any, incidentID, target int64, reason string,
	do func(ctx context.Context) (string, error),
) (*mcp.CallToolResult, any, error) {
	if incidentID == 0 || target == 0 {
		return toolError("Refused: name both incidents (incident_id and the other)"), nil, nil
	}
	if _, _, err := s.store.GetIncident(ctx, incidentID); errors.Is(err, store.ErrNotFound) {
		return toolError(fmt.Sprintf("Refused: incident #%d does not exist", incidentID)), nil, nil
	}
	input, _ := json.Marshal(in)
	actionID, err := s.store.RecordAction(ctx, store.ActionStart{
		Actor: store.ActorAgent, RunID: run.ID, IncidentID: incidentID, TargetIncidentID: target, Tool: tool, Input: input,
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
			return "", refuse("link_incident and merge_incident were already called %d times in this turn", maxConsolidationCalls)
		}
		if err := checkReason(reason); err != nil {
			return "", err
		}
		return do(ctx)
	}()
	return s.finishConsolidation(ctx, actionID, run.ID, msg, err)
}

// ---- page context ----

var (
	incidentPath = regexp.MustCompile(`^/incidents/(\d{1,18})$`)
	runPath      = regexp.MustCompile(`^/runs/(\d{1,18})$`)
)

// pageContext describes the UI page a message was sent from, for the agent.
// Only the path comes from the browser, and only a known shape is used: the
// description is built from the controller's own records.
func (s *Server) pageContext(ctx context.Context, path string) string {
	switch {
	case path == "/":
		incs, err := s.store.ListIncidents(ctx, 200)
		if err != nil {
			return "the home page (incident list)"
		}
		firing := 0
		for _, i := range incs {
			if i.AlertStatus == "firing" && i.ParentID == nil {
				firing++
			}
		}
		open, _ := s.store.OpenPullRequests(ctx)
		return fmt.Sprintf("the home page: %d incident(s) firing, %d agent PR(s) awaiting review", firing, len(open))
	case incidentPath.MatchString(path):
		id, _ := strconv.ParseInt(incidentPath.FindStringSubmatch(path)[1], 10, 64)
		return s.incidentBrief(ctx, id)
	case runPath.MatchString(path):
		id, _ := strconv.ParseInt(runPath.FindStringSubmatch(path)[1], 10, 64)
		r, err := s.store.GetRun(ctx, id)
		if err != nil {
			return ""
		}
		about := "a general conversation"
		if r.IncidentID != 0 {
			about = fmt.Sprintf("incident #%d", r.IncidentID)
		}
		return fmt.Sprintf("the transcript of run #%d (%s, %s, about %s)", r.ID, r.Kind, r.Status, about)
	}
	return ""
}

// incidentBrief is one line about an incident for the page context.
func (s *Server) incidentBrief(ctx context.Context, id int64) string {
	in, _, err := s.store.GetIncident(ctx, id)
	if err != nil {
		return ""
	}
	var b strings.Builder
	fmt.Fprintf(&b, "incident #%d (%s in %s, %s", in.ID, in.AlertName, orNone(in.Namespace), in.AlertStatus)
	if in.ParentID != nil {
		fmt.Fprintf(&b, ", merged into #%d", *in.ParentID)
	}
	if p, err := s.store.PullRequestForIncident(ctx, in.ID); err == nil {
		fmt.Fprintf(&b, ", PR #%d %s with CI %s", p.Number, p.State, p.Checks)
	}
	b.WriteString(")")
	if runs, err := s.store.ListRuns(ctx, in.ID); err == nil {
		for _, r := range runs {
			if r.Kind == store.KindDiagnose && r.Status == store.RunSucceeded {
				if sum := reportSummary(r.Report); sum != "" {
					fmt.Fprintf(&b, ". Diagnosis: %s", strings.Join(strings.Fields(sum), " "))
				}
				break
			}
		}
	}
	return b.String()
}

// ---- threads ----

// threadForIncident returns the incident's thread, made if missing. A merged
// incident's conversation is its parent's.
func (s *Server) threadForIncident(ctx context.Context, incidentID int64, create bool) (store.Thread, error) {
	in, _, err := s.store.GetIncident(ctx, incidentID)
	if err != nil {
		return store.Thread{}, err
	}
	if in.ParentID != nil {
		incidentID = *in.ParentID
	}
	th, _, err := s.store.IncidentThread(ctx, incidentID, create)
	return th, err
}

// threadTitle is how a thread is named in the UI.
func threadTitle(t store.Thread) string {
	if t.Kind == store.ThreadIncident && t.IncidentID != nil {
		return fmt.Sprintf("#%d %s", *t.IncidentID, t.AlertName)
	}
	if t.Title == "" {
		return "New conversation"
	}
	return t.Title
}

// ---- chat API (UI port) ----

type apiThread struct {
	ID         int64     `json:"id"`
	Kind       string    `json:"kind"`
	Title      string    `json:"title"`
	IncidentID *int64    `json:"incident_id,omitempty"`
	Namespace  string    `json:"namespace,omitempty"`
	Messages   int       `json:"messages"`
	UpdatedAt  time.Time `json:"updated_at"`
}

func toAPIThread(t store.Thread) apiThread {
	return apiThread{ID: t.ID, Kind: t.Kind, Title: threadTitle(t), IncidentID: t.IncidentID, Namespace: t.Namespace,
		Messages: t.Messages, UpdatedAt: t.UpdatedAt}
}

type apiMessage struct {
	ID        int64     `json:"id"`
	Source    string    `json:"source"`
	Author    string    `json:"author"`
	Text      string    `json:"text,omitempty"` // a person's message, to insert as text
	HTML      string    `json:"html,omitempty"` // the agent's reply, rendered safely
	Status    string    `json:"status"`
	RunID     *int64    `json:"run_id,omitempty"`
	URL       string    `json:"url,omitempty"`
	Path      string    `json:"path,omitempty"`
	Line      *int      `json:"line,omitempty"`
	From      string    `json:"from,omitempty"` // "#25", "home", "run #18": where it was sent from
	CreatedAt time.Time `json:"created_at"`
}

func toAPIMessage(m store.ChatMessage) apiMessage {
	out := apiMessage{ID: m.ID, Source: m.Source, Author: m.Author, Status: m.Status, RunID: m.RunID, URL: m.HTMLURL,
		Path: m.Path, Line: m.Line, From: pageLabel(m.Page), CreatedAt: m.CreatedAt}
	if m.Source == store.SourceAgent {
		if h, err := ui.MarkdownHTML(m.Body); err == nil {
			out.HTML = h
		} else {
			out.Text = m.Body
		}
	} else {
		out.Text = m.Body
	}
	return out
}

// pageLabel is the short chip for where a message was sent from.
func pageLabel(path string) string {
	switch {
	case path == "/":
		return "home"
	case incidentPath.MatchString(path):
		return "#" + incidentPath.FindStringSubmatch(path)[1]
	case runPath.MatchString(path):
		return "run #" + runPath.FindStringSubmatch(path)[1]
	}
	return ""
}

func (s *Server) apiError(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": msg})
}

// maxThreadsListed bounds the switcher.
const maxThreadsListed = 30

// apiThreads lists conversations, and names the one for the page the panel
// is on: an incident page's thread (existing, or to be made on the first
// message).
func (s *Server) apiThreads(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	list, err := s.store.RecentThreads(ctx, maxThreadsListed)
	if err != nil {
		slog.Error("listing threads", "err", err)
		s.apiError(w, http.StatusInternalServerError, "could not list conversations")
		return
	}
	out := struct {
		Here    any         `json:"here"`
		Threads []apiThread `json:"threads"`
	}{Threads: []apiThread{}}
	for _, t := range list {
		out.Threads = append(out.Threads, toAPIThread(t))
	}
	if m := incidentPath.FindStringSubmatch(r.URL.Query().Get("page")); m != nil {
		id, _ := strconv.ParseInt(m[1], 10, 64)
		if in, _, err := s.store.GetIncident(ctx, id); err == nil {
			owner := in.ID
			if in.ParentID != nil {
				owner = *in.ParentID
			}
			here := map[string]any{"incident_id": owner, "title": fmt.Sprintf("#%d %s", in.ID, in.AlertName)}
			if owner != in.ID {
				here["merged_into"] = owner
			}
			if th, _, err := s.store.IncidentThread(ctx, owner, false); err == nil {
				here["thread"] = toAPIThread(th)
			}
			out.Here = here
		}
	}
	writeJSON(w, out)
}

// apiCreateThread starts a general conversation, or returns (making it if
// needed) an incident's.
func (s *Server) apiCreateThread(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Kind       string `json:"kind"`
		IncidentID int64  `json:"incident_id"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&in); err != nil {
		s.apiError(w, http.StatusBadRequest, "bad request")
		return
	}
	ctx := r.Context()
	var th store.Thread
	var err error
	switch in.Kind {
	case store.ThreadGeneral:
		th, err = s.store.CreateGeneralThread(ctx)
	case store.ThreadIncident:
		th, err = s.threadForIncident(ctx, in.IncidentID, true)
		if errors.Is(err, store.ErrNotFound) {
			s.apiError(w, http.StatusNotFound, "no such incident")
			return
		}
	default:
		s.apiError(w, http.StatusBadRequest, "kind must be general or incident")
		return
	}
	if err != nil {
		slog.Error("creating thread", "err", err)
		s.apiError(w, http.StatusInternalServerError, "could not start the conversation")
		return
	}
	writeJSON(w, toAPIThread(th))
}

// apiThread is one conversation: its messages, the turn in progress, and
// what the agent can do in it.
func (s *Server) apiThread(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		s.apiError(w, http.StatusNotFound, "no such conversation")
		return
	}
	ctx := r.Context()
	th, err := s.store.GetThread(ctx, id)
	if errors.Is(err, store.ErrNotFound) {
		s.apiError(w, http.StatusNotFound, "no such conversation")
		return
	}
	if err != nil {
		slog.Error("loading thread", "id", id, "err", err)
		s.apiError(w, http.StatusInternalServerError, "could not load the conversation")
		return
	}
	msgs, err := s.store.ChatMessages(ctx, id)
	if err != nil {
		slog.Error("loading messages", "thread", id, "err", err)
		s.apiError(w, http.StatusInternalServerError, "could not load the conversation")
		return
	}
	out := struct {
		Thread   apiThread    `json:"thread"`
		Messages []apiMessage `json:"messages"`
		Active   any          `json:"active"`
		PR       any          `json:"pr"`
		Can      []string     `json:"can"`
	}{Thread: toAPIThread(th), Messages: []apiMessage{}, Can: []string{"read the cluster", "read incidents", "merge and link incidents"}}
	for _, m := range msgs {
		out.Messages = append(out.Messages, toAPIMessage(m))
	}
	if run, ok, err := s.store.ActiveChatRun(ctx, id); err == nil && ok {
		out.Active = map[string]any{"run_id": run.ID, "status": run.Status}
	}
	if th.IncidentID != nil {
		if p, err := s.store.PullRequestForIncident(ctx, *th.IncidentID); err == nil {
			out.PR = map[string]any{"number": p.Number, "url": p.URL, "title": p.Title, "state": p.State, "checks": p.Checks}
			if p.State == store.PROpen && s.prs != nil {
				out.Can = append(out.Can, "add commits to the PR")
			}
		} else if s.prs != nil {
			if in, _, err := s.store.GetIncident(ctx, *th.IncidentID); err == nil && !in.Merged() {
				out.Can = append(out.Can, "open a PR")
			}
		}
	}
	writeJSON(w, out)
}

// apiPostMessage adds the owner's message to a conversation and queues a
// turn (or joins the waiting one).
func (s *Server) apiPostMessage(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		s.apiError(w, http.StatusNotFound, "no such conversation")
		return
	}
	var in struct {
		Body string `json:"body"`
		Page string `json:"page"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxChatMessage*4)).Decode(&in); err != nil {
		s.apiError(w, http.StatusBadRequest, "bad request")
		return
	}
	body := strings.TrimSpace(in.Body)
	switch {
	case body == "":
		s.apiError(w, http.StatusBadRequest, "type a message first")
		return
	case len(body) > maxChatMessage:
		s.apiError(w, http.StatusBadRequest, fmt.Sprintf("that message is over %d characters", maxChatMessage))
		return
	}
	page := in.Page
	if pageLabel(page) == "" {
		page = "" // only known shapes are kept
	}
	res, err := s.store.EnqueueChat(r.Context(), store.ChatMessage{ThreadID: id, Source: store.SourceUI, Author: "you", Body: body, Page: page},
		s.chatLimits())
	switch {
	case errors.Is(err, store.ErrNotFound):
		s.apiError(w, http.StatusNotFound, "no such conversation")
		return
	case errors.Is(err, store.ErrThreadArchived):
		s.apiError(w, http.StatusConflict, "this conversation is archived")
		return
	case err != nil:
		slog.Error("queueing chat", "thread", id, "err", err)
		s.apiError(w, http.StatusInternalServerError, "could not queue that; the error is logged")
		return
	}
	out := map[string]any{"message_id": res.MessageID}
	if res.Capped != "" {
		out["capped"] = cappedText(res, s.now())
		writeJSON(w, out)
		return
	}
	out["run_id"], out["new_turn"] = res.RunID, res.NewRun
	slog.Info("chat message queued", "thread", id, "run", res.RunID, "new_turn", res.NewRun, "page", page)
	if res.NewRun {
		s.metrics.runs.WithLabelValues(store.RunQueued).Inc()
	}
	if s.waker != nil {
		s.waker.Wake()
	}
	writeJSON(w, out)
}

// sortedKeys is map keys in order, for stable tool output.
func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}
