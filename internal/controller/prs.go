package controller

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/jcwearn/alert-agent/internal/github"
	"github.com/jcwearn/alert-agent/internal/proposal"
	"github.com/jcwearn/alert-agent/internal/store"
)

// GitHub is what the PR path needs from the GitHub App. It has no merge:
// merging is the human's.
type GitHub interface {
	InstallationToken(ctx context.Context, repos []string, permissions map[string]string) (github.Token, error)
	CreatePullRequest(ctx context.Context, token, repo string, pr github.NewPullRequest) (github.PullRequest, error)
	GetPullRequest(ctx context.Context, token, repo string, number int) (github.PullRequest, error)
	ChecksRollup(ctx context.Context, token, repo, sha string) (string, error)
}

// ProposeInput is what the agent passes to propose_pr.
type ProposeInput struct {
	Title string `json:"title" jsonschema:"conventional commit subject, at most 72 characters, e.g. fix(ebooks): raise calibre memory limit to 1Gi. It is published: no hostnames, addresses or tailnet names"`
	Body  string `json:"body" jsonschema:"Markdown PR description for the reviewer: what is wrong, the evidence, why this change fixes it, and any risk"`
	Patch string `json:"patch" jsonschema:"the exact output of the git_diff tool, unchanged"`
}

// PRs opens and tracks the agent's pull requests.
type PRs struct {
	GitHub   GitHub
	Repo     string // owner/name
	Proposal proposal.Config
	// Prepare defaults to proposal.Prepare; tests replace it.
	Prepare func(ctx context.Context, cfg proposal.Config, token string, req proposal.Request) (*proposal.Commit, error)
	// Push defaults to (*proposal.Commit).Push; tests replace it.
	Push func(ctx context.Context, c *proposal.Commit) error

	mu sync.Mutex // one proposal at a time
}

func (p *PRs) repoName() string { return p.Repo[strings.LastIndex(p.Repo, "/")+1:] }

// maxProposeAttempts bounds propose_pr calls per run, rejected ones included:
// each attempt clones the repo and runs the hooks.
const maxProposeAttempts = 3

// propose is propose_pr: validate, commit through the hooks, push agent/<id>,
// open the PR and record it. A *proposal.Rejected goes back to the agent;
// any other error is the controller's own failure.
func (s *Server) propose(ctx context.Context, run store.Run, in ProposeInput) (store.PullRequest, error) {
	p := s.prs
	p.mu.Lock()
	defer p.mu.Unlock()
	prepare, push := p.Prepare, p.Push
	if prepare == nil {
		prepare = proposal.Prepare
	}
	if push == nil {
		push = func(ctx context.Context, c *proposal.Commit) error { return c.Push(ctx) }
	}

	n, err := s.store.CountActions(ctx, run.ID, "propose_pr")
	if err != nil {
		return store.PullRequest{}, err
	}
	// This call is already recorded, so n includes it.
	if n > maxProposeAttempts {
		return store.PullRequest{}, &proposal.Rejected{Reason: fmt.Sprintf(
			"propose_pr was already called %d times in this run; stop and describe the change in your report instead", maxProposeAttempts)}
	}
	if existing, err := s.store.PullRequestForIncident(ctx, run.IncidentID); err == nil {
		return store.PullRequest{}, &proposal.Rejected{Reason: fmt.Sprintf(
			"incident #%d already has PR #%d (%s); only one PR per incident", run.IncidentID, existing.Number, existing.URL)}
	} else if !errors.Is(err, store.ErrNotFound) {
		return store.PullRequest{}, err
	}
	if err := proposal.CheckTitle(in.Title); err != nil {
		return store.PullRequest{}, err
	}
	if err := proposal.CheckBody(in.Body); err != nil {
		return store.PullRequest{}, err
	}
	if err := proposal.CheckPatch(in.Patch); err != nil {
		return store.PullRequest{}, err
	}

	incident, _, err := s.store.GetIncident(ctx, run.IncidentID)
	if err != nil {
		return store.PullRequest{}, err
	}

	write, err := p.GitHub.InstallationToken(ctx, []string{p.repoName()}, map[string]string{"contents": "write"})
	if err != nil {
		return store.PullRequest{}, fmt.Errorf("github token: %w", err)
	}
	commit, err := prepare(ctx, p.Proposal, write.Token, proposal.Request{IncidentID: run.IncidentID, Title: in.Title, Patch: in.Patch})
	if err != nil {
		return store.PullRequest{}, err
	}
	defer commit.Close()
	if err := push(ctx, commit); err != nil {
		return store.PullRequest{}, err
	}

	prToken, err := p.GitHub.InstallationToken(ctx, []string{p.repoName()}, map[string]string{"pull_requests": "write"})
	if err != nil {
		return store.PullRequest{}, fmt.Errorf("github token: %w", err)
	}
	gh, err := p.GitHub.CreatePullRequest(ctx, prToken.Token, p.Repo, github.NewPullRequest{
		Title: in.Title,
		Body:  s.prBody(incident, run, commit, in.Body),
		Head:  commit.Branch,
		Base:  "main",
	})
	if err != nil {
		return store.PullRequest{}, fmt.Errorf("open PR for %s: %w", commit.Branch, err)
	}
	pr := store.PullRequest{
		IncidentID: run.IncidentID, RunID: run.ID, Number: gh.Number, URL: gh.HTMLURL, Title: in.Title,
		Branch: commit.Branch, BaseSHA: commit.BaseSHA, HeadSHA: commit.HeadSHA,
		State: store.PROpen, Checks: store.ChecksPending, AlertName: incident.AlertName, Namespace: incident.Namespace,
	}
	if pr.ID, err = s.store.InsertPullRequest(ctx, pr); err != nil {
		return pr, fmt.Errorf("PR #%d opened but not recorded: %w", gh.Number, err)
	}
	s.metrics.prs.WithLabelValues("opened").Inc()
	slog.Info("PR opened", "incident", run.IncidentID, "run", run.ID, "pr", gh.Number, "branch", commit.Branch)
	return pr, nil
}

// prBody is the PR description. The repository is private and PR bodies
// are never replayed onto the public mirror (only commit messages and the
// title are), so it may link the UI by its real name.
func (s *Server) prBody(in store.Incident, run store.Run, c *proposal.Commit, agentBody string) string {
	var b strings.Builder
	where := in.AlertName
	if in.Namespace != "" {
		where += " in " + in.Namespace
	}
	fmt.Fprintf(&b, "> [!NOTE]\n> Proposed by the alert agent for incident #%d (%s). The agent cannot merge; review it like any other PR.\n\n", in.ID, where)
	b.WriteString(strings.TrimSpace(agentBody))
	b.WriteString("\n\n## Validation (by the controller)\n\n")
	fmt.Fprintf(&b, "- Applied to `main` at %s\n", c.BaseSHA)
	b.WriteString("- Paths: only `apps/` and `infrastructure/`; no encrypted secrets, nothing under `apps/alert-agent/`\n")
	b.WriteString("- `.githooks/pre-commit` (render, no plaintext Secret, no private literals) and `.githooks/commit-msg` passed\n")
	b.WriteString("- Files:\n")
	for _, f := range c.Files {
		fmt.Fprintf(&b, "  - `%s`\n", f)
	}
	if s.opts.PublicURL != "" {
		fmt.Fprintf(&b, "\n[Incident #%d](%s) · [run #%d transcript](%s/runs/%d)\n",
			in.ID, s.incidentURL(in.ID), run.ID, strings.TrimRight(s.opts.PublicURL, "/"), run.ID)
	}
	fmt.Fprintf(&b, "\n<!-- alert-agent incident=%d run=%d session=%s -->\n", in.ID, run.ID, run.SessionID)
	return b.String()
}

// Poll intervals for open PRs.
const (
	prPollInterval = 2 * time.Minute
	// notifyAfter sends the review push even if checks have not finished.
	notifyAfter = 15 * time.Minute
)

// pollPRs keeps open PRs' state and checks current, and sends the one
// "needs your review" push per PR once its checks settle.
func (s *Server) pollPRs(ctx context.Context) {
	t := time.NewTicker(prPollInterval)
	defer t.Stop()
	for {
		if err := s.PollPRsOnce(ctx); err != nil {
			slog.Error("polling PRs", "err", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// PollPRsOnce is one pass of the poller.
func (s *Server) PollPRsOnce(ctx context.Context) error {
	open, err := s.store.OpenPullRequests(ctx)
	if err != nil || len(open) == 0 {
		return err
	}
	p := s.prs
	tok, err := p.GitHub.InstallationToken(ctx, []string{p.repoName()},
		map[string]string{"pull_requests": "read", "checks": "read"})
	if err != nil {
		return fmt.Errorf("github token: %w", err)
	}
	for _, pr := range open {
		gh, err := p.GitHub.GetPullRequest(ctx, tok.Token, p.Repo, pr.Number)
		if err != nil {
			slog.Warn("reading PR", "pr", pr.Number, "err", err)
			continue
		}
		st := store.PRStatus{State: store.PROpen, Checks: pr.Checks, HeadSHA: gh.Head.SHA}
		switch {
		case gh.Merged:
			st.State = store.PRMerged
		case gh.State == "closed":
			st.State = store.PRClosed
		}
		if checks, err := p.GitHub.ChecksRollup(ctx, tok.Token, p.Repo, gh.Head.SHA); err == nil {
			st.Checks = checks
		} else {
			slog.Warn("reading checks", "pr", pr.Number, "err", err)
		}
		if err := s.store.UpdatePullRequest(ctx, pr.ID, st); err != nil {
			return err
		}
		if st.State != store.PROpen {
			s.metrics.prs.WithLabelValues(st.State).Inc()
			slog.Info("PR finished", "pr", pr.Number, "state", st.State)
			continue
		}
		if pr.NotifiedAt == nil && (st.Checks != store.ChecksPending || s.now().Sub(pr.CreatedAt) > notifyAfter) {
			s.notifyReview(ctx, pr, st.Checks)
		}
	}
	return nil
}

func (s *Server) notifyReview(ctx context.Context, pr store.PullRequest, checks string) {
	if s.reviewer == nil {
		return
	}
	first, err := s.store.MarkPullRequestNotified(ctx, pr.ID)
	if err != nil || !first {
		return
	}
	pr.Checks = checks
	if err := s.reviewer.NeedsReview(ctx, pr, s.incidentURL(pr.IncidentID)); err != nil {
		slog.Error("review notification", "pr", pr.Number, "err", err)
	}
}
