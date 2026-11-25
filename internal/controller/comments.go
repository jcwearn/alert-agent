package controller

import (
	"context"
	"fmt"
	"log/slog"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/jcwearn/alert-agent/internal/github"
	"github.com/jcwearn/alert-agent/internal/store"
)

// CommentsGitHub is what the PR-comment path needs from the GitHub App.
type CommentsGitHub interface {
	IssueComments(ctx context.Context, token, repo string, number int, since time.Time) ([]github.IssueComment, error)
	ReviewComments(ctx context.Context, token, repo string, number int, since time.Time) ([]github.ReviewComment, error)
	Reviews(ctx context.Context, token, repo string, number int) ([]github.Review, error)
	ReactToIssueComment(ctx context.Context, token, repo string, commentID int64, content string) error
	ReactToReviewComment(ctx context.Context, token, repo string, commentID int64, content string) error
	CreateIssueComment(ctx context.Context, token, repo string, number int, body string) (github.Comment, error)
	ReplyToReviewComment(ctx context.Context, token, repo string, number int, commentID int64, body string) (github.Comment, error)
}

// Comments turns comments on open agent PRs into chat turns. There is no
// webhook: GitHub cannot reach the cluster, so the controller polls.
type Comments struct {
	GitHub CommentsGitHub
	// AllowedLogins are the GitHub logins whose comments count (the
	// cluster's owner). Everyone else, every bot and the App itself are
	// ignored, so the agent never answers itself.
	AllowedLogins []string
	Interval      time.Duration
}

// EnableComments turns on the PR-comment path. It needs the PR path.
func (s *Server) EnableComments(c *Comments) { s.comments = c }

func (c *Comments) allowed(u github.User) bool {
	if u.Type != "User" {
		return false
	}
	return slices.ContainsFunc(c.AllowedLogins, func(l string) bool { return strings.EqualFold(l, u.Login) })
}

func (s *Server) pollComments(ctx context.Context) {
	t := time.NewTicker(s.comments.Interval)
	defer t.Stop()
	for {
		if err := s.PollCommentsOnce(ctx); err != nil {
			slog.Error("polling PR comments", "err", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// Token permissions per purpose. Conversation comments and their reactions
// are issue endpoints; line comments, replies and reviews are pull request
// endpoints.
var (
	commentReadPerms  = map[string]string{"issues": "read", "pull_requests": "read"}
	commentWritePerms = map[string]string{"issues": "write", "pull_requests": "write"}
)

// incoming is a comment from any of the three listings, normalised.
type incoming struct {
	msg store.ChatMessage
	at  time.Time
}

// PollCommentsOnce is one pass over the open agent PRs.
func (s *Server) PollCommentsOnce(ctx context.Context) error {
	if s.comments == nil || s.prs == nil {
		return nil
	}
	open, err := s.store.OpenPullRequests(ctx)
	if err != nil || len(open) == 0 {
		return err
	}
	p := s.prs
	read, err := p.GitHub.InstallationToken(ctx, []string{p.repoName()}, commentReadPerms)
	if err != nil {
		return fmt.Errorf("github token: %w", err)
	}
	var write string // minted on first use
	for _, pr := range open {
		in, newest, err := s.fetchComments(ctx, read.Token, pr)
		if err != nil {
			slog.Warn("reading PR comments", "pr", pr.Number, "err", err)
			continue
		}
		queued := false
		for _, c := range in {
			if write == "" {
				tok, err := p.GitHub.InstallationToken(ctx, []string{p.repoName()}, commentWritePerms)
				if err != nil {
					return fmt.Errorf("github token: %w", err)
				}
				write = tok.Token
			}
			took, err := s.takeComment(ctx, write, pr, c.msg)
			if err != nil {
				// Not recorded: keep the cursor at this comment, so the next
				// pass reads it again (GitHub's since is inclusive, and the
				// dedupe key absorbs the ones already recorded).
				slog.Error("queueing PR comment", "pr", pr.Number, "comment", c.msg.GHID, "err", err)
				if c.at.Before(newest) {
					newest = c.at
				}
			}
			queued = queued || took
		}
		// Only after every comment is recorded: a crash before this re-reads
		// them, and the dedupe key makes that harmless.
		if !newest.IsZero() {
			if err := s.store.AdvanceCommentsSince(ctx, pr.ID, newest); err != nil {
				return err
			}
		}
		if queued && s.waker != nil {
			s.waker.Wake()
		}
	}
	return nil
}

// fetchComments returns a PR's new comments from allowlisted people, oldest
// first, and the newest timestamp seen (allowlisted or not) for the cursor.
// GitHub's since filters on updated_at, so an old comment that was edited
// comes back too; created_at before the cursor drops it.
func (s *Server) fetchComments(ctx context.Context, token string, pr store.PullRequest) ([]incoming, time.Time, error) {
	gh, repo, since := s.comments.GitHub, s.prs.Repo, pr.CommentsSince
	var (
		out    []incoming
		newest time.Time
	)
	consider := func(u github.User, at time.Time, m store.ChatMessage) {
		if at.Before(since) {
			return
		}
		if at.After(newest) {
			newest = at
		}
		if !s.comments.allowed(u) || strings.TrimSpace(m.Body) == "" {
			// GitHub's since is inclusive, so the comment at the cursor comes
			// back every pass; count it once.
			if at.After(since) {
				s.metrics.comments.WithLabelValues("ignored").Inc()
			}
			slog.Debug("ignoring PR comment", "pr", pr.Number, "login", u.Login, "type", u.Type, "kind", m.GHKind)
			return
		}
		m.PRID, m.Source, m.Author = pr.ID, store.SourceGitHub, u.Login
		out = append(out, incoming{msg: m, at: at})
	}

	issue, err := gh.IssueComments(ctx, token, repo, pr.Number, since)
	if err != nil {
		return nil, newest, err
	}
	for _, c := range issue {
		consider(c.User, c.CreatedAt, store.ChatMessage{Body: c.Body, GHKind: store.GHIssueComment, GHID: c.ID, HTMLURL: c.HTMLURL})
	}
	review, err := gh.ReviewComments(ctx, token, repo, pr.Number, since)
	if err != nil {
		return nil, newest, err
	}
	for _, c := range review {
		thread := c.ID
		if c.InReplyToID != 0 {
			thread = c.InReplyToID
		}
		consider(c.User, c.CreatedAt, store.ChatMessage{Body: c.Body, GHKind: store.GHReviewComment, GHID: c.ID, GHThreadID: thread,
			Path: c.Path, Line: c.Line, DiffHunk: clipRunes(c.DiffHunk, 2000), HTMLURL: c.HTMLURL})
	}
	reviews, err := gh.Reviews(ctx, token, repo, pr.Number)
	if err != nil {
		return nil, newest, err
	}
	for _, r := range reviews {
		// A review with only line comments has an empty body; its comments
		// arrive on their own.
		if strings.TrimSpace(r.Body) == "" {
			continue
		}
		consider(r.User, r.SubmittedAt, store.ChatMessage{Body: r.Body, GHKind: store.GHReview, GHID: r.ID, HTMLURL: r.HTMLURL})
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].at.Before(out[j].at) })
	return out, newest, nil
}

// takeComment records one comment and acknowledges it: 👀 when it is queued,
// or a short reply when a limit refused it. It reports whether a turn now
// has it, and an error only when the comment could not be recorded.
func (s *Server) takeComment(ctx context.Context, token string, pr store.PullRequest, m store.ChatMessage) (bool, error) {
	res, err := s.store.EnqueueChat(ctx, m, s.chatLimits())
	switch {
	case err != nil:
		return false, err
	case res.Duplicate:
		return false, nil
	case res.Capped != "":
		s.metrics.comments.WithLabelValues("capped").Inc()
		slog.Info("PR comment capped", "pr", pr.Number, "comment", m.GHID, "reason", res.Capped)
		if _, err := s.postReply(ctx, token, pr, []store.ChatMessage{m}, cappedText(res, s.now())); err != nil {
			slog.Error("replying to a capped comment", "pr", pr.Number, "err", err)
		}
		return false, nil
	}
	if res.NewRun {
		s.metrics.comments.WithLabelValues("queued").Inc()
		s.metrics.runs.WithLabelValues(store.RunQueued).Inc()
	} else {
		s.metrics.comments.WithLabelValues("joined").Inc()
	}
	slog.Info("PR comment queued", "pr", pr.Number, "comment", m.GHID, "kind", m.GHKind, "login", m.Author, "run", res.RunID)
	if err := s.react(ctx, token, m); err != nil {
		slog.Warn("reacting to PR comment", "pr", pr.Number, "comment", m.GHID, "err", err)
	} else if m.GHKind != store.GHReview {
		if err := s.store.MarkChatReacted(ctx, res.MessageID); err != nil {
			slog.Error("recording reaction", "comment", m.GHID, "err", err)
		}
	}
	return true, nil
}

// react adds 👀. A review's own body cannot take a reaction.
func (s *Server) react(ctx context.Context, token string, m store.ChatMessage) error {
	gh, repo := s.comments.GitHub, s.prs.Repo
	switch m.GHKind {
	case store.GHIssueComment:
		return gh.ReactToIssueComment(ctx, token, repo, m.GHID, "eyes")
	case store.GHReviewComment:
		return gh.ReactToReviewComment(ctx, token, repo, m.GHID, "eyes")
	}
	return nil
}

// maxCommentBody stays under GitHub's 65536-character comment limit.
const maxCommentBody = 60000

// postReply answers GitHub comments: in the thread when they are all one
// line comment's thread, otherwise in the PR's conversation, quoting each.
// msgs may include UI messages; only the GitHub ones are quoted.
func (s *Server) postReply(ctx context.Context, token string, pr store.PullRequest, msgs []store.ChatMessage, body string) (github.Comment, error) {
	var gh []store.ChatMessage
	for _, m := range msgs {
		if m.Source == store.SourceGitHub {
			gh = append(gh, m)
		}
	}
	thread := int64(0)
	if len(gh) > 0 {
		thread = gh[0].GHThreadID
		for _, m := range gh {
			if m.GHKind != store.GHReviewComment || m.GHThreadID != thread {
				thread = 0
				break
			}
		}
	}
	text := clipRunes(body, maxCommentBody)
	if thread != 0 {
		return s.comments.GitHub.ReplyToReviewComment(ctx, token, s.prs.Repo, pr.Number, thread, text)
	}
	var b strings.Builder
	for _, m := range gh {
		line := clipRunes(firstLine(m.Body), 120)
		if m.HTMLURL != "" {
			fmt.Fprintf(&b, "> [@%s](%s): %s\n", m.Author, m.HTMLURL, line)
		} else {
			fmt.Fprintf(&b, "> @%s: %s\n", m.Author, line)
		}
	}
	if b.Len() > 0 {
		b.WriteString("\n")
	}
	b.WriteString(text)
	return s.comments.GitHub.CreateIssueComment(ctx, token, s.prs.Repo, pr.Number, b.String())
}

// replyOnGitHub posts a chat turn's reply when any of its messages came from
// a PR comment. UI-only turns stay in the UI.
func (s *Server) replyOnGitHub(ctx context.Context, run store.Run, reply store.ChatMessage) {
	if s.comments == nil || run.PRID == nil {
		return
	}
	msgs, err := s.store.TurnMessages(ctx, run.ID)
	if err != nil {
		slog.Error("loading turn messages", "run", run.ID, "err", err)
		return
	}
	if !slices.ContainsFunc(msgs, func(m store.ChatMessage) bool { return m.Source == store.SourceGitHub }) {
		return
	}
	pr, err := s.store.GetPullRequest(ctx, *run.PRID)
	if err != nil {
		slog.Error("loading PR for reply", "run", run.ID, "err", err)
		return
	}
	tok, err := s.prs.GitHub.InstallationToken(ctx, []string{s.prs.repoName()}, commentWritePerms)
	if err != nil {
		slog.Error("github token for reply", "run", run.ID, "err", err)
		return
	}
	body := reply.Body
	if s.opts.PublicURL != "" {
		body += fmt.Sprintf("\n\n<sub>alert agent · [turn transcript](%s/runs/%s)</sub>",
			strings.TrimRight(s.opts.PublicURL, "/"), strconv.FormatInt(run.ID, 10))
	}
	c, err := s.postReply(ctx, tok.Token, pr, msgs, body)
	if err != nil {
		s.metrics.comments.WithLabelValues("reply_failed").Inc()
		slog.Error("posting reply on GitHub", "run", run.ID, "pr", pr.Number, "err", err)
		return
	}
	s.metrics.comments.WithLabelValues("replied").Inc()
	slog.Info("replied on GitHub", "run", run.ID, "pr", pr.Number, "url", c.HTMLURL)
	if err := s.store.SetChatMessageURL(ctx, reply.ID, c.HTMLURL); err != nil {
		slog.Error("recording reply URL", "run", run.ID, "err", err)
	}
}
