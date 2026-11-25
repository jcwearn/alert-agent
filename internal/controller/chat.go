package controller

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/jcwearn/alert-agent/internal/proposal"
	"github.com/jcwearn/alert-agent/internal/runnerapi"
	"github.com/jcwearn/alert-agent/internal/store"
	"github.com/jcwearn/alert-agent/internal/ui"
)

// UpdateInput is what the agent passes to update_pr.
type UpdateInput struct {
	Title string `json:"title" jsonschema:"conventional commit subject for the new commit, at most 72 characters, e.g. fix(ebooks): use 512Mi instead. It is published: no hostnames, addresses or tailnet names"`
	Patch string `json:"patch" jsonschema:"the exact output of the git_diff tool, unchanged"`
}

// maxUpdateAttempts bounds update_pr calls per chat turn, refusals included.
const maxUpdateAttempts = 3

// maxChatMessage bounds one message typed in the UI.
const maxChatMessage = 8000

// chatContext is a chat turn's runner context: the PR it is about and the
// reviewer's messages. The rest of the conversation is in the session.
func (s *Server) chatContext(ctx context.Context, run store.Run, in store.Incident) (runnerapi.Context, error) {
	rc := runnerapi.Context{
		RunID: run.ID, SessionID: run.SessionID, Model: run.Model, MaxTurns: s.opts.MaxTurns,
		UIURL: s.incidentURL(in.ID), Kind: runnerapi.KindChat, UpdatePR: s.prs != nil,
		Incident: apiIncident(in),
	}
	if run.PRID == nil {
		return rc, errors.New("chat run without a PR")
	}
	pr, err := s.store.GetPullRequest(ctx, *run.PRID)
	if err != nil {
		return rc, err
	}
	rc.PR = &runnerapi.PR{Number: pr.Number, URL: pr.URL, Title: pr.Title, Branch: pr.Branch, HeadSHA: pr.HeadSHA,
		Files: pr.Files, State: pr.State, Checks: pr.Checks}
	msgs, err := s.store.TurnMessages(ctx, run.ID)
	if err != nil {
		return rc, err
	}
	for _, m := range msgs {
		rc.Messages = append(rc.Messages, runnerapi.ChatMessage{Source: m.Source, Author: m.Author, Body: m.Body, At: m.CreatedAt,
			Kind: m.GHKind, Path: m.Path, Line: m.Line, DiffHunk: m.DiffHunk})
	}
	return rc, nil
}

// runnerResumeSession gives a chat turn the newest session file of its
// conversation.
func (s *Server) runnerResumeSession(w http.ResponseWriter, r *http.Request) {
	run := runFrom(r)
	if run.Kind != store.KindChat {
		http.Error(w, "only a chat turn resumes a session", http.StatusBadRequest)
		return
	}
	gz, err := s.store.LatestSession(r.Context(), run.SessionID, run.ID)
	if errors.Is(err, store.ErrNotFound) {
		http.Error(w, "no stored session for this conversation", http.StatusNotFound)
		return
	}
	if err != nil {
		slog.Error("loading session", "run", run.ID, "err", err)
		http.Error(w, "could not load session", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/gzip")
	_, _ = w.Write(gz)
}

// addUpdateTool serves update_pr to a chat turn.
func (s *Server) addUpdateTool(srv *mcp.Server, run store.Run) {
	mcp.AddTool(srv, &mcp.Tool{
		Name: "update_pr",
		Description: "Add a commit to the pull request this conversation is about, with your edits. " +
			"Edit the files in the checkout (it is the PR branch), call git_diff, and pass its output unchanged as patch. " +
			"The controller applies it on the PR branch's head, refuses anything outside apps/ and infrastructure/, " +
			"encrypted secrets and the agent's own manifests, runs the repository's commit hooks, and pushes the commit " +
			"to the same branch (never forcing). On a refusal, fix the cause and call again; at most 3 calls per turn. " +
			"The agent can never merge, approve or close a PR.",
		Annotations: &mcp.ToolAnnotations{DestructiveHint: ptr(false), OpenWorldHint: ptr(true)},
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in UpdateInput) (*mcp.CallToolResult, any, error) {
		return s.updateTool(ctx, run, in)
	})
}

// pushedPrefix starts update_pr's success result, which the reply footer
// reads back: "Pushed <sha> to <branch>".
const pushedPrefix = "Pushed "

func (s *Server) updateTool(ctx context.Context, run store.Run, in UpdateInput) (*mcp.CallToolResult, any, error) {
	input, _ := json.Marshal(in)
	actionID, err := s.store.StartAction(ctx, run.ID, run.IncidentID, "update_pr", input)
	if err != nil {
		slog.Error("recording action", "run", run.ID, "err", err)
		return toolError("the controller could not record this call, so it did nothing; try once more later"), nil, nil
	}
	pr, sha, err := s.updatePR(ctx, run, in)
	var rejected *proposal.Rejected
	switch {
	case err == nil:
		msg := fmt.Sprintf("%s%s to %s (PR #%d, %s).\nThe PR now changes: %s\nSay in your reply what this commit changed.",
			pushedPrefix, sha, pr.Branch, pr.Number, pr.URL, strings.Join(pr.Files, ", "))
		s.finishAction(ctx, actionID, store.ActionSucceeded, msg)
		return toolText(msg), nil, nil
	case errors.As(err, &rejected):
		s.metrics.prs.WithLabelValues("update_rejected").Inc()
		s.finishAction(ctx, actionID, store.ActionRejected, rejected.Reason)
		slog.Info("update rejected", "run", run.ID, "reason", firstLine(rejected.Reason))
		return toolError("Refused: " + rejected.Reason), nil, nil
	default:
		s.metrics.prs.WithLabelValues("update_failed").Inc()
		s.finishAction(ctx, actionID, store.ActionFailed, err.Error())
		slog.Error("update failed", "run", run.ID, "err", err)
		return toolError("The controller failed to update the PR (its error is logged). Do not retry; describe the change in your reply."), nil, nil
	}
}

// updatePR is update_pr: validate, commit on the PR branch through the hooks,
// fast-forward the branch, and record the new head and file list.
func (s *Server) updatePR(ctx context.Context, run store.Run, in UpdateInput) (store.PullRequest, string, error) {
	p := s.prs
	if p == nil || run.Kind != store.KindChat || run.PRID == nil {
		return store.PullRequest{}, "", &proposal.Rejected{Reason: "update_pr is only for a chat turn on an agent PR"}
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	prepare, push := p.PrepareUpdate, p.Push
	if prepare == nil {
		prepare = proposal.PrepareUpdate
	}
	if push == nil {
		push = func(ctx context.Context, c *proposal.Commit) error { return c.Push(ctx) }
	}

	n, err := s.store.CountActions(ctx, run.ID, "update_pr")
	if err != nil {
		return store.PullRequest{}, "", err
	}
	if n > maxUpdateAttempts {
		return store.PullRequest{}, "", &proposal.Rejected{Reason: fmt.Sprintf(
			"update_pr was already called %d times in this turn; stop and describe the change in your reply instead", maxUpdateAttempts)}
	}
	pr, err := s.store.GetPullRequest(ctx, *run.PRID)
	if err != nil {
		return pr, "", err
	}
	if pr.State != store.PROpen {
		return pr, "", &proposal.Rejected{Reason: fmt.Sprintf("PR #%d is %s; it can no longer change", pr.Number, pr.State)}
	}
	if err := proposal.CheckTitle(in.Title); err != nil {
		return pr, "", err
	}
	if err := proposal.CheckPatch(in.Patch); err != nil {
		return pr, "", err
	}

	write, err := p.GitHub.InstallationToken(ctx, []string{p.repoName()}, map[string]string{"contents": "write"})
	if err != nil {
		return pr, "", fmt.Errorf("github token: %w", err)
	}
	commit, err := prepare(ctx, p.Proposal, write.Token, proposal.UpdateRequest{
		IncidentID: pr.IncidentID, Branch: pr.Branch, HeadSHA: pr.HeadSHA, BaseSHA: pr.BaseSHA,
		Files: pr.Files, Title: in.Title, Patch: in.Patch,
	})
	if err != nil {
		return pr, "", err
	}
	defer commit.Close()
	if err := s.checkOverlap(ctx, commit.Files, pr.ID); err != nil {
		return pr, "", err
	}
	if err := push(ctx, commit); err != nil {
		return pr, "", fmt.Errorf("push %s: %w", pr.Branch, err)
	}
	if _, err := s.store.UpdatePullRequestHead(ctx, pr.ID, commit.ParentSHA, commit.HeadSHA, commit.Files); err != nil {
		return pr, commit.HeadSHA, fmt.Errorf("pushed %s but not recorded: %w", commit.HeadSHA, err)
	}
	pr.HeadSHA, pr.Files = commit.HeadSHA, commit.Files
	s.metrics.prs.WithLabelValues("updated").Inc()
	slog.Info("PR updated", "incident", pr.IncidentID, "run", run.ID, "pr", pr.Number, "head", commit.HeadSHA)
	return pr, commit.HeadSHA, nil
}

// chatLimits are the caps a new turn is checked against.
func (s *Server) chatLimits() store.ChatLimits {
	return store.ChatLimits{PerPRHour: s.opts.ChatMaxPerPRHour, GlobalHour: s.opts.MaxRunsPerHour}
}

// chatTurnFinished records the agent's reply to the turn's messages, with a
// line per commit the turn pushed.
func (s *Server) chatTurnFinished(ctx context.Context, run store.Run) {
	reply := strings.TrimSpace(run.Report)
	answered := run.Status == store.RunSucceeded && reply != ""
	if !answered {
		reason := firstLine(run.Error)
		if reason == "" {
			reason = "the turn ended without a reply"
		}
		reply = "I could not finish this turn: " + reason
	}
	if footer := s.pushedFooter(ctx, run); footer != "" {
		reply += "\n\n" + footer
	}
	msg, first, err := s.store.FinishChatTurn(ctx, run.ID, answered, reply)
	if err != nil {
		slog.Error("recording chat reply", "run", run.ID, "err", err)
		return
	}
	if first {
		slog.Info("chat turn finished", "run", run.ID, "pr_id", *run.PRID, "answered", answered)
		s.replyOnGitHub(ctx, run, msg)
	}
}

// pushedFooter lists the commits a turn pushed, from its update_pr audit
// entries, so the reply never depends on the agent mentioning them.
func (s *Server) pushedFooter(ctx context.Context, run store.Run) string {
	actions, err := s.store.IncidentActions(ctx, run.IncidentID)
	if err != nil {
		slog.Error("loading turn actions", "run", run.ID, "err", err)
		return ""
	}
	var lines []string
	for _, a := range actions {
		if a.RunID == nil || *a.RunID != run.ID || a.Tool != "update_pr" || a.Status != store.ActionSucceeded {
			continue
		}
		rest, ok := strings.CutPrefix(a.Result, pushedPrefix)
		if !ok {
			continue
		}
		sha, _, _ := strings.Cut(rest, " ")
		var in UpdateInput
		_ = json.Unmarshal(a.Input, &in)
		lines = append(lines, fmt.Sprintf("Pushed %#q: %s", shortSHA(sha), in.Title))
	}
	return strings.Join(lines, "\n")
}

func shortSHA(sha string) string {
	if len(sha) > 7 {
		return sha[:7]
	}
	return sha
}

// uiChat takes a message typed in the incident page's chat panel.
func (s *Server) uiChat(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	back := func(key, val string) {
		q := url.Values{key: {val}}
		http.Redirect(w, r, "/incidents/"+strconv.FormatInt(id, 10)+"?"+q.Encode()+"#chat", http.StatusSeeOther)
	}
	body := strings.TrimSpace(r.PostFormValue("message"))
	switch {
	case s.prs == nil:
		back("error", "The PR path is off, so there is no one to chat with.")
		return
	case body == "":
		back("error", "Type a message first.")
		return
	case len(body) > maxChatMessage:
		back("error", fmt.Sprintf("That message is over %d characters.", maxChatMessage))
		return
	}
	ctx := r.Context()
	pr, err := s.store.PullRequestForIncident(ctx, id)
	if errors.Is(err, store.ErrNotFound) {
		back("error", "This incident has no PR to talk about.")
		return
	}
	if err != nil {
		slog.Error("loading PR for chat", "incident", id, "err", err)
		back("error", "The controller failed to load the PR; its error is logged.")
		return
	}
	res, err := s.store.EnqueueChat(ctx, store.ChatMessage{PRID: pr.ID, Source: store.SourceUI, Author: "you", Body: body}, s.chatLimits())
	switch {
	case errors.Is(err, store.ErrPRNotOpen):
		back("error", fmt.Sprintf("PR #%d is %s; the conversation is over.", pr.Number, pr.State))
		return
	case err != nil:
		slog.Error("queueing chat", "incident", id, "err", err)
		back("error", "The controller failed to queue that; its error is logged.")
		return
	case res.Capped != "":
		back("error", cappedText(res, s.now()))
		return
	}
	slog.Info("chat message queued", "incident", id, "pr", pr.Number, "run", res.RunID, "new_turn", res.NewRun)
	if res.NewRun {
		s.metrics.runs.WithLabelValues(store.RunQueued).Inc()
	}
	if s.waker != nil {
		s.waker.Wake()
	}
	back("msg", "Sent. The agent's reply appears below.")
}

// cappedText says why a message got no turn, and when to try again.
func cappedText(res store.ChatEnqueue, now time.Time) string {
	msg := "Not processed: " + res.Capped + "."
	if !res.RetryAt.IsZero() && res.RetryAt.After(now) {
		msg += " Try again after " + res.RetryAt.UTC().Format("15:04 UTC") + "."
	}
	return msg
}

// streamItem is one transcript step sent to the chat panel.
type streamItem struct {
	Kind    string `json:"kind"`
	Title   string `json:"title,omitempty"`
	Body    string `json:"body,omitempty"`
	IsError bool   `json:"is_error,omitempty"`
}

// Stream pacing and bounds.
const (
	streamPoll = time.Second
	streamMax  = 30 * time.Minute
	streamBody = 2000
)

// runStream sends a run's transcript as it is recorded, over SSE, until the
// run finishes. Each step carries its seq as the event id, so a browser that
// reconnects (a proxy timeout, say) resumes where it stopped.
func (s *Server) runStream(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	ctx := r.Context()
	if _, err := s.store.GetRun(ctx, id); err != nil {
		http.NotFound(w, r)
		return
	}
	seq := -1
	if last, err := strconv.Atoi(r.Header.Get("Last-Event-ID")); err == nil {
		seq = last
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")

	send := func(event, id string, data any) error {
		b, _ := json.Marshal(data)
		if id != "" {
			if _, err := fmt.Fprintf(w, "id: %s\n", id); err != nil {
				return err
			}
		}
		_, err := fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, b)
		return err
	}
	tick := time.NewTicker(streamPoll)
	defer tick.Stop()
	deadline := time.After(streamMax)
	status := ""
	for {
		run, err := s.store.GetRun(ctx, id)
		if err != nil {
			return
		}
		events, err := s.store.RunEventsAfter(ctx, id, seq)
		if err != nil {
			slog.Error("streaming run events", "run", id, "err", err)
			return
		}
		for _, e := range events {
			seq = e.Seq
			for _, it := range ui.Transcript([]store.RunEvent{e}) {
				item := streamItem{Kind: it.Kind, Title: it.Title, Body: clipRunes(it.Body, streamBody), IsError: it.IsError}
				if it.Kind == ui.ItemResult {
					item.Body = ""
				}
				if err := send("item", strconv.Itoa(e.Seq), item); err != nil {
					return
				}
			}
		}
		if run.Status != status {
			status = run.Status
			if err := send("status", "", status); err != nil {
				return
			}
		}
		flusher.Flush()
		if run.FinishedAt != nil && len(events) == 0 {
			_ = send("done", "", status)
			flusher.Flush()
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-deadline:
			return
		case <-tick.C:
		}
	}
}
