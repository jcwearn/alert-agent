package controller

import (
	"context"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jcwearn/alert-agent/internal/github"
	"github.com/jcwearn/alert-agent/internal/runnerapi"
	"github.com/jcwearn/alert-agent/internal/store"
)

func (f *fakeStore) AdvanceCommentsSince(_ context.Context, prID int64, t time.Time) error {
	p := f.pr()
	p.mu.Lock()
	defer p.mu.Unlock()
	if pr := &p.prs[prID-1]; t.After(pr.CommentsSince) {
		pr.CommentsSince = t
	}
	return nil
}

func (f *fakeStore) MarkChatReacted(_ context.Context, id int64) error {
	now := time.Now()
	f.chat().messages[id-1].ReactedAt = &now
	return nil
}

func (f *fakeStore) SetChatMessageURL(_ context.Context, id int64, url string) error {
	f.chat().messages[id-1].HTMLURL = url
	return nil
}

// fakeComments serves canned listings and records what the controller did.
type fakeComments struct {
	mu        sync.Mutex
	issue     []github.IssueComment
	review    []github.ReviewComment
	reviews   []github.Review
	sinces    []time.Time
	reactions []string
	posted    []string
}

func (g *fakeComments) IssueComments(_ context.Context, _, _ string, _ int, since time.Time) ([]github.IssueComment, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.sinces = append(g.sinces, since)
	return g.issue, nil
}

func (g *fakeComments) ReviewComments(context.Context, string, string, int, time.Time) ([]github.ReviewComment, error) {
	return g.review, nil
}

func (g *fakeComments) Reviews(context.Context, string, string, int) ([]github.Review, error) {
	return g.reviews, nil
}

func (g *fakeComments) ReactToIssueComment(_ context.Context, _, _ string, id int64, content string) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.reactions = append(g.reactions, fmt.Sprintf("issue/%d:%s", id, content))
	return nil
}

func (g *fakeComments) ReactToReviewComment(_ context.Context, _, _ string, id int64, content string) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.reactions = append(g.reactions, fmt.Sprintf("review/%d:%s", id, content))
	return nil
}

func (g *fakeComments) CreateIssueComment(_ context.Context, _, _ string, n int, body string) (github.Comment, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.posted = append(g.posted, fmt.Sprintf("pr/%d: %s", n, body))
	return github.Comment{ID: 500, HTMLURL: "https://github.com/c/500"}, nil
}

func (g *fakeComments) ReplyToReviewComment(_ context.Context, _, _ string, n int, id int64, body string) (github.Comment, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.posted = append(g.posted, fmt.Sprintf("pr/%d thread/%d: %s", n, id, body))
	return github.Comment{ID: 501, HTMLURL: "https://github.com/c/501"}, nil
}

var (
	me      = github.User{Login: "jcwearn", Type: "User"}
	theApp  = github.User{Login: "alert-agent[bot]", Type: "Bot"}
	someone = github.User{Login: "stranger", Type: "User"}
)

// commentServer is chatServer (PR row 1 open, run 9 a running chat turn)
// with PR comments on, the PR's cursor at since, and an empty conversation.
func commentServer(t *testing.T) (*Server, *fakeStore, *fakeGitHub, *fakeComments, time.Time) {
	t.Helper()
	s, fs, gh, _ := chatServer(t, nil)
	gc := &fakeComments{}
	s.EnableComments(&Comments{GitHub: gc, AllowedLogins: []string{"JCWearn"}, Interval: time.Minute})
	since := time.Date(2026, 11, 25, 12, 0, 0, 0, time.UTC)
	fs.pr().prs[0].CommentsSince = since
	fs.chat().messages = nil
	return s, fs, gh, gc, since
}

func TestPollCommentsQueuesOnlyAllowlistedPeople(t *testing.T) {
	s, fs, gh, gc, since := commentServer(t)
	fs.chat().result = store.ChatEnqueue{RunID: 10, NewRun: true}
	line := 14
	gc.issue = []github.IssueComment{
		{ID: 1, Body: "why this limit?", User: me, CreatedAt: since.Add(time.Minute), HTMLURL: "https://github.com/i/1"},
		{ID: 2, Body: "Here is my reply", User: theApp, CreatedAt: since.Add(2 * time.Minute)},
		{ID: 3, Body: "please merge", User: someone, CreatedAt: since.Add(3 * time.Minute)},
		{ID: 4, Body: "edited old comment", User: me, CreatedAt: since.Add(-time.Hour)},
		{ID: 5, Body: "renovate says hi", User: github.User{Login: "renovate[bot]", Type: "Bot"}, CreatedAt: since.Add(4 * time.Minute)},
	}
	gc.review = []github.ReviewComment{
		{ID: 20, Body: "make it 512Mi", User: me, CreatedAt: since.Add(90 * time.Second), Path: "apps/ebooks/deployment.yaml", Line: &line, DiffHunk: "@@ x"},
		{ID: 21, Body: "and this too", User: me, CreatedAt: since.Add(100 * time.Second), InReplyToID: 20, Path: "apps/ebooks/deployment.yaml"},
	}
	gc.reviews = []github.Review{
		{ID: 30, Body: "Looks close; see comments", User: me, SubmittedAt: since.Add(95 * time.Second), State: "COMMENTED"},
		{ID: 31, Body: "", User: me, SubmittedAt: since.Add(96 * time.Second)},
	}
	waker := &countingWaker{}
	s.waker = waker

	if err := s.PollCommentsOnce(t.Context()); err != nil {
		t.Fatal(err)
	}
	got := fs.chat().enqueued
	var ids []string
	for _, m := range got {
		ids = append(ids, fmt.Sprintf("%s/%d", m.GHKind, m.GHID))
	}
	want := "issue_comment/1 review_comment/20 review/30 review_comment/21"
	if strings.Join(ids, " ") != want {
		t.Fatalf("queued %v, want %s (oldest first, allowlisted people only)", ids, want)
	}
	if m := got[1]; m.Author != "jcwearn" || m.Source != store.SourceGitHub || m.PRID != 1 || m.GHThreadID != 20 || m.Path == "" || m.Line == nil {
		t.Errorf("line comment = %+v", m)
	}
	if got[3].GHThreadID != 20 {
		t.Errorf("a reply in a thread belongs to the thread's first comment: %+v", got[3])
	}
	if strings.Join(gc.reactions, " ") != "issue/1:eyes review/20:eyes review/21:eyes" {
		t.Errorf("reactions = %v (a review body cannot take one)", gc.reactions)
	}
	if pr, _ := fs.GetPullRequest(t.Context(), 1); !pr.CommentsSince.Equal(since.Add(4 * time.Minute)) {
		t.Errorf("cursor = %v, want the newest comment seen", pr.CommentsSince)
	}
	if waker.n == 0 {
		t.Error("dispatcher not woken")
	}
	if len(gh.perms) == 0 || gh.perms[0]["issues"] != "read" || gh.perms[0]["pull_requests"] != "read" || len(gh.perms[0]) != 2 {
		t.Errorf("read token permissions = %v", gh.perms)
	}
	if len(gc.posted) != 0 {
		t.Errorf("posted %v before any turn ran", gc.posted)
	}
}

func TestPollCommentsDuplicatesAndCaps(t *testing.T) {
	s, fs, _, gc, since := commentServer(t)
	gc.issue = []github.IssueComment{{ID: 1, Body: "why?", User: me, CreatedAt: since.Add(time.Minute)}}

	fs.chat().result = store.ChatEnqueue{Duplicate: true}
	if err := s.PollCommentsOnce(t.Context()); err != nil {
		t.Fatal(err)
	}
	if len(gc.reactions) != 0 || len(gc.posted) != 0 {
		t.Errorf("a comment already recorded was acknowledged again: %v %v", gc.reactions, gc.posted)
	}

	line := 3
	gc.issue = nil
	gc.review = []github.ReviewComment{{ID: 40, Body: "bigger", User: me, CreatedAt: since.Add(2 * time.Minute), Path: "a.yaml", Line: &line}}
	fs.chat().result = store.ChatEnqueue{Capped: "this PR already had 4 agent turns in the last hour (the limit)", RetryAt: time.Now().Add(30 * time.Minute)}
	if err := s.PollCommentsOnce(t.Context()); err != nil {
		t.Fatal(err)
	}
	if len(gc.reactions) != 0 {
		t.Errorf("a capped comment got 👀: %v", gc.reactions)
	}
	if len(gc.posted) != 1 || !strings.HasPrefix(gc.posted[0], "pr/1200 thread/40: Not processed: this PR already had 4") ||
		!strings.Contains(gc.posted[0], "Try again after") {
		t.Errorf("capped reply = %v", gc.posted)
	}
}

func TestChatTurnRepliesOnGitHub(t *testing.T) {
	for _, tc := range []struct {
		name     string
		messages []store.ChatMessage
		want     string // prefix of the one post, or "" for none
	}{
		{"line comment thread", []store.ChatMessage{
			{Source: store.SourceGitHub, Author: "jcwearn", Body: "make it 512Mi", GHKind: store.GHReviewComment, GHID: 20, GHThreadID: 20},
			{Source: store.SourceGitHub, Author: "jcwearn", Body: "and here", GHKind: store.GHReviewComment, GHID: 21, GHThreadID: 20},
		}, "pr/1200 thread/20: Done."},
		{"conversation and line comments", []store.ChatMessage{
			{Source: store.SourceGitHub, Author: "jcwearn", Body: "why this limit?\nsecond line", GHKind: store.GHIssueComment, GHID: 1, HTMLURL: "https://github.com/i/1"},
			{Source: store.SourceGitHub, Author: "jcwearn", Body: "make it 512Mi", GHKind: store.GHReviewComment, GHID: 20, GHThreadID: 20},
		}, "pr/1200: > [@jcwearn](https://github.com/i/1): why this limit?\n> @jcwearn: make it 512Mi\n\nDone."},
		{"UI only", []store.ChatMessage{{Source: store.SourceUI, Author: "you", Body: "why?"}}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, fs, gh, gc, _ := commentServer(t)
			run := int64(9)
			for _, m := range tc.messages {
				m.PRID, m.RunID, m.Status = 1, &run, store.ChatQueued
				fs.chat().messages = append(fs.chat().messages, m)
			}
			if status, _ := runnerReq(t, s.RunnerHandler(), http.MethodPost, runnerapi.PathFinish, "chat",
				`{"status":"succeeded","report":"Done."}`); status != http.StatusNoContent {
				t.Fatal(status)
			}
			if tc.want == "" {
				if len(gc.posted) != 0 {
					t.Errorf("a UI-only turn posted on GitHub: %v", gc.posted)
				}
				return
			}
			if len(gc.posted) != 1 || !strings.HasPrefix(gc.posted[0], tc.want) {
				t.Fatalf("posted %q, want prefix %q", gc.posted, tc.want)
			}
			if !strings.Contains(gc.posted[0], "https://medic.example/runs/9") {
				t.Errorf("reply lacks the transcript link: %s", gc.posted[0])
			}
			msgs := fs.chat().messages
			if reply := msgs[len(msgs)-1]; reply.Source != store.SourceAgent || !strings.HasPrefix(reply.HTMLURL, "https://github.com/c/") {
				t.Errorf("reply not linked to its comment: %+v", reply)
			}
			if last := gh.perms[len(gh.perms)-1]; last["issues"] != "write" || last["pull_requests"] != "write" {
				t.Errorf("reply token permissions = %v", last)
			}
		})
	}
}

// A comment the store fails to record keeps the cursor at it, so the next
// pass reads it again; comments after it are not lost either.
func TestPollCommentsHoldsCursorAtAFailure(t *testing.T) {
	s, fs, _, gc, since := commentServer(t)
	fs.chat().result = store.ChatEnqueue{RunID: 10, NewRun: true}
	fs.chat().failID = 7171034603
	gc.issue = []github.IssueComment{
		{ID: 7171034603, Body: "why not 256Mi?", User: me, CreatedAt: since.Add(time.Minute)},
		{ID: 7171034604, Body: "and later", User: me, CreatedAt: since.Add(3 * time.Minute)},
	}
	if err := s.PollCommentsOnce(t.Context()); err != nil {
		t.Fatal(err)
	}
	if pr, _ := fs.GetPullRequest(t.Context(), 1); !pr.CommentsSince.Equal(since.Add(time.Minute)) {
		t.Errorf("cursor = %v, want it held at the failed comment (%v)", pr.CommentsSince, since.Add(time.Minute))
	}
	fs.chat().failID = 0
	if err := s.PollCommentsOnce(t.Context()); err != nil {
		t.Fatal(err)
	}
	var ids []int64
	for _, m := range fs.chat().messages {
		ids = append(ids, m.GHID)
	}
	if !slices.Contains(ids, int64(7171034603)) {
		t.Errorf("the failed comment was never recorded: %v", ids)
	}
	if pr, _ := fs.GetPullRequest(t.Context(), 1); !pr.CommentsSince.Equal(since.Add(3 * time.Minute)) {
		t.Errorf("cursor after recovery = %v", pr.CommentsSince)
	}
}
