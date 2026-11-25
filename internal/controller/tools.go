package controller

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/jcwearn/alert-agent/internal/proposal"
	"github.com/jcwearn/alert-agent/internal/store"
)

// ToolServerName is the controller's MCP server name; claude exposes its
// tools as mcp__controller__<tool>.
const ToolServerName = "controller"

// mcpHandler serves the controller's tools (link_incident, merge_incident,
// and propose_pr with the PR path on) to runners. It sits behind
// runnerAuth, and is stateless: each request builds a server bound to the
// run its token belongs to, so a tool can never act for another run.
func (s *Server) mcpHandler() http.Handler {
	return mcp.NewStreamableHTTPHandler(func(r *http.Request) *mcp.Server {
		return s.toolServer(r.Context(), runFrom(r))
	}, &mcp.StreamableHTTPOptions{Stateless: true, JSONResponse: true, MaxRequestBodyBytes: 1 << 20})
}

func (s *Server) toolServer(ctx context.Context, run store.Run) *mcp.Server {
	srv := mcp.NewServer(&mcp.Implementation{Name: ToolServerName, Version: "v1"}, nil)
	// A chat turn's tools depend on its thread (see chatScope).
	if run.Kind == store.KindChat {
		s.chatTools(ctx, srv, run)
		return srv
	}
	s.addConsolidationTools(srv, run)
	if s.prs != nil {
		s.addProposeTool(srv, run)
	}
	return srv
}

// addProposeTool serves propose_pr: to alert runs, and to a chat turn on an
// incident that has no PR yet.
func (s *Server) addProposeTool(srv *mcp.Server, run store.Run) {
	mcp.AddTool(srv, &mcp.Tool{
		Name: "propose_pr",
		Description: "Open a pull request against the GitOps repository with your edits, for a human to review. " +
			"Edit the files first, call git_diff, and pass its output unchanged as patch. " +
			"The controller applies it to the current main, refuses anything outside apps/ and infrastructure/, " +
			"encrypted secrets and the agent's own manifests, and runs the repository's commit hooks (render, " +
			"no plaintext Secret, no private addresses or names). On a refusal, fix the cause and call again; " +
			"at most 3 calls per run, and one PR per incident. A merged incident cannot propose (its parent owns the fix), " +
			"and a change touching a file an open agent PR already changes is refused. The agent can never merge a PR.",
		Annotations: &mcp.ToolAnnotations{DestructiveHint: ptr(false), OpenWorldHint: ptr(true)},
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in ProposeInput) (*mcp.CallToolResult, any, error) {
		return s.proposeTool(ctx, run, in)
	})
}

// proposeTool records the call before acting, then records how it ended.
// The agent gets the refusal reason, or a generic failure: controller-side
// errors (GitHub, the database) are logged, not shown.
func (s *Server) proposeTool(ctx context.Context, run store.Run, in ProposeInput) (*mcp.CallToolResult, any, error) {
	input, _ := json.Marshal(in)
	actionID, err := s.store.StartAction(ctx, run.ID, run.IncidentID, "propose_pr", input)
	if err != nil {
		slog.Error("recording action", "run", run.ID, "err", err)
		return toolError("the controller could not record this call, so it did nothing; try once more later"), nil, nil
	}

	pr, err := s.propose(ctx, run, in)
	var rejected *proposal.Rejected
	switch {
	case err == nil:
		msg := fmt.Sprintf("Opened PR #%d: %s\nBranch %s, commit %s on main %s.\nA human reviews and merges it; mention the PR in your report.",
			pr.Number, pr.URL, pr.Branch, pr.HeadSHA, pr.BaseSHA)
		s.finishAction(ctx, actionID, store.ActionSucceeded, msg)
		return toolText(msg), nil, nil
	case errors.As(err, &rejected):
		s.metrics.prs.WithLabelValues("rejected").Inc()
		s.finishAction(ctx, actionID, store.ActionRejected, rejected.Reason)
		slog.Info("proposal rejected", "run", run.ID, "reason", firstLine(rejected.Reason))
		return toolError("Refused: " + rejected.Reason), nil, nil
	default:
		s.metrics.prs.WithLabelValues("failed").Inc()
		s.finishAction(ctx, actionID, store.ActionFailed, err.Error())
		slog.Error("proposal failed", "run", run.ID, "err", err)
		return toolError("The controller failed to open the PR (its error is logged). Do not retry; describe the change in your report."), nil, nil
	}
}

func (s *Server) finishAction(ctx context.Context, id int64, status, result string) {
	// The request may be gone by now; the record still has to land.
	if err := s.store.FinishAction(context.WithoutCancel(ctx), id, status, result); err != nil {
		slog.Error("recording action result", "action", id, "err", err)
	}
}

func toolText(s string) *mcp.CallToolResult {
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: s}}}
}

func toolError(s string) *mcp.CallToolResult {
	r := toolText(s)
	r.IsError = true
	return r
}

func ptr[T any](v T) *T { return &v }

// Session upload limits: compressed on the wire, and decompressed to check
// it is gzip and measure it.
const (
	maxSessionGzip = 32 << 20
	maxSessionRaw  = 256 << 20
)

// runnerSession stores the run's claude session file, for Phase 4 to resume.
func (s *Server) runnerSession(w http.ResponseWriter, r *http.Request) {
	run := runFrom(r)
	gz, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxSessionGzip))
	if err != nil {
		http.Error(w, "session too large", http.StatusRequestEntityTooLarge)
		return
	}
	zr, err := gzip.NewReader(bytes.NewReader(gz))
	if err != nil {
		http.Error(w, "body is not gzip", http.StatusBadRequest)
		return
	}
	n, err := io.Copy(io.Discard, io.LimitReader(zr, maxSessionRaw+1))
	if err != nil || n > maxSessionRaw {
		http.Error(w, "bad or oversized session", http.StatusBadRequest)
		return
	}
	if err := s.store.SaveRunSession(r.Context(), run.ID, gz, int(n)); err != nil {
		slog.Error("storing session", "run", run.ID, "err", err)
		http.Error(w, "could not store session", http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
