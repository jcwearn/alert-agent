package github

import (
	"context"
	"net/http"
	"net/url"
	"strconv"
	"time"
)

// User is a comment's author. Type is "User" for a person and "Bot" for an
// App (including this one) or another bot.
type User struct {
	Login string `json:"login"`
	Type  string `json:"type"`
}

// IssueComment is a comment in a PR's conversation.
type IssueComment struct {
	ID        int64     `json:"id"`
	Body      string    `json:"body"`
	User      User      `json:"user"`
	HTMLURL   string    `json:"html_url"`
	CreatedAt time.Time `json:"created_at"`
}

// ReviewComment is a comment on a line of a PR's diff.
type ReviewComment struct {
	ID          int64     `json:"id"`
	Body        string    `json:"body"`
	User        User      `json:"user"`
	HTMLURL     string    `json:"html_url"`
	CreatedAt   time.Time `json:"created_at"`
	Path        string    `json:"path"`
	Line        *int      `json:"line"`
	DiffHunk    string    `json:"diff_hunk"`
	InReplyToID int64     `json:"in_reply_to_id"`
}

// Review is a submitted review; its body is a comment of its own.
type Review struct {
	ID          int64     `json:"id"`
	Body        string    `json:"body"`
	User        User      `json:"user"`
	HTMLURL     string    `json:"html_url"`
	State       string    `json:"state"`
	SubmittedAt time.Time `json:"submitted_at"`
}

// Comment is a comment the controller posted.
type Comment struct {
	ID      int64  `json:"id"`
	HTMLURL string `json:"html_url"`
}

// maxPages bounds a listing: a PR with more than 1000 new comments between
// two polls is not a conversation.
const maxPages = 10

// listAll reads every page of a listing of 100 per page.
func listAll[T any](ctx context.Context, a *App, token, path string, q url.Values) ([]T, error) {
	var out []T
	q.Set("per_page", "100")
	for page := 1; page <= maxPages; page++ {
		q.Set("page", strconv.Itoa(page))
		var batch []T
		if err := a.api(ctx, token, http.MethodGet, path+"?"+q.Encode(), nil, http.StatusOK, &batch); err != nil {
			return nil, err
		}
		out = append(out, batch...)
		if len(batch) < 100 {
			break
		}
	}
	return out, nil
}

// IssueComments lists a PR's conversation comments updated at or after since.
func (a *App) IssueComments(ctx context.Context, token, repo string, number int, since time.Time) ([]IssueComment, error) {
	return listAll[IssueComment](ctx, a, token, "/repos/"+repo+"/issues/"+strconv.Itoa(number)+"/comments",
		url.Values{"since": {since.UTC().Format(time.RFC3339)}})
}

// ReviewComments lists a PR's line comments updated at or after since.
func (a *App) ReviewComments(ctx context.Context, token, repo string, number int, since time.Time) ([]ReviewComment, error) {
	return listAll[ReviewComment](ctx, a, token, "/repos/"+repo+"/pulls/"+strconv.Itoa(number)+"/comments",
		url.Values{"since": {since.UTC().Format(time.RFC3339)}})
}

// Reviews lists a PR's reviews. GitHub has no since filter for them.
func (a *App) Reviews(ctx context.Context, token, repo string, number int) ([]Review, error) {
	return listAll[Review](ctx, a, token, "/repos/"+repo+"/pulls/"+strconv.Itoa(number)+"/reviews", url.Values{})
}

// ReactToIssueComment adds a reaction (e.g. "eyes") to a conversation comment.
func (a *App) ReactToIssueComment(ctx context.Context, token, repo string, commentID int64, content string) error {
	return a.react(ctx, token, "/repos/"+repo+"/issues/comments/"+strconv.FormatInt(commentID, 10)+"/reactions", content)
}

// ReactToReviewComment adds a reaction to a line comment. GitHub has no
// reactions for a review's own body.
func (a *App) ReactToReviewComment(ctx context.Context, token, repo string, commentID int64, content string) error {
	return a.react(ctx, token, "/repos/"+repo+"/pulls/comments/"+strconv.FormatInt(commentID, 10)+"/reactions", content)
}

func (a *App) react(ctx context.Context, token, path, content string) error {
	// 201 for a new reaction, 200 if this App already reacted the same way.
	err := a.api(ctx, token, http.MethodPost, path, map[string]string{"content": content}, http.StatusCreated, nil)
	if err != nil && isStatus(err, http.StatusOK) {
		return nil
	}
	return err
}

// CreateIssueComment posts a comment in a PR's conversation.
func (a *App) CreateIssueComment(ctx context.Context, token, repo string, number int, body string) (Comment, error) {
	var out Comment
	err := a.api(ctx, token, http.MethodPost, "/repos/"+repo+"/issues/"+strconv.Itoa(number)+"/comments",
		map[string]string{"body": body}, http.StatusCreated, &out)
	return out, err
}

// ReplyToReviewComment posts a reply in a line comment's thread. commentID
// must be the thread's first comment.
func (a *App) ReplyToReviewComment(ctx context.Context, token, repo string, number int, commentID int64, body string) (Comment, error) {
	var out Comment
	err := a.api(ctx, token, http.MethodPost,
		"/repos/"+repo+"/pulls/"+strconv.Itoa(number)+"/comments/"+strconv.FormatInt(commentID, 10)+"/replies",
		map[string]string{"body": body}, http.StatusCreated, &out)
	return out, err
}
