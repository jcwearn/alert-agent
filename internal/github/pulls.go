package github

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
)

// PullRequest is the part of GitHub's pull request object the controller uses.
type PullRequest struct {
	Number  int    `json:"number"`
	HTMLURL string `json:"html_url"`
	State   string `json:"state"` // open or closed
	Merged  bool   `json:"merged"`
	Head    struct {
		SHA string `json:"sha"`
		Ref string `json:"ref"`
	} `json:"head"`
}

// NewPullRequest is a PR to open.
type NewPullRequest struct {
	Title string `json:"title"`
	Body  string `json:"body"`
	Head  string `json:"head"`
	Base  string `json:"base"`
}

// CreatePullRequest opens a PR. There is deliberately no merge counterpart
// anywhere in this package.
func (a *App) CreatePullRequest(ctx context.Context, token, repo string, pr NewPullRequest) (PullRequest, error) {
	var out PullRequest
	err := a.api(ctx, token, http.MethodPost, "/repos/"+repo+"/pulls", pr, http.StatusCreated, &out)
	return out, err
}

// GetPullRequest reads one PR.
func (a *App) GetPullRequest(ctx context.Context, token, repo string, number int) (PullRequest, error) {
	var out PullRequest
	err := a.api(ctx, token, http.MethodGet, "/repos/"+repo+"/pulls/"+strconv.Itoa(number), nil, http.StatusOK, &out)
	return out, err
}

// Check rollups.
const (
	ChecksPending = "pending"
	ChecksSuccess = "success"
	ChecksFailure = "failure"
)

// ChecksRollup reduces a commit's check runs to one word: failure if any
// completed run failed, pending while any is unfinished (or none has started
// yet), success otherwise.
func (a *App) ChecksRollup(ctx context.Context, token, repo, sha string) (string, error) {
	var out struct {
		TotalCount int `json:"total_count"`
		CheckRuns  []struct {
			Status     string `json:"status"`
			Conclusion string `json:"conclusion"`
		} `json:"check_runs"`
	}
	path := "/repos/" + repo + "/commits/" + sha + "/check-runs?per_page=100"
	if err := a.api(ctx, token, http.MethodGet, path, nil, http.StatusOK, &out); err != nil {
		return "", err
	}
	if len(out.CheckRuns) == 0 {
		return ChecksPending, nil
	}
	pending := false
	for _, c := range out.CheckRuns {
		if c.Status != "completed" {
			pending = true
			continue
		}
		switch c.Conclusion {
		case "success", "neutral", "skipped":
		default:
			return ChecksFailure, nil
		}
	}
	if pending {
		return ChecksPending, nil
	}
	return ChecksSuccess, nil
}

func (a *App) api(ctx context.Context, token, method, path string, in any, want int, out any) error {
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, a.baseURL()+path, body)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := a.client().Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != want {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return &StatusError{Method: method, Path: strings.SplitN(path, "?", 2)[0], Code: resp.StatusCode, Message: strings.TrimSpace(string(msg))}
	}
	if out == nil {
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

// StatusError is a GitHub answer with an unexpected status.
type StatusError struct {
	Method, Path string
	Code         int
	Message      string
}

func (e *StatusError) Error() string {
	return fmt.Sprintf("%s %s: GitHub answered %d: %s", e.Method, e.Path, e.Code, e.Message)
}

func isStatus(err error, code int) bool {
	var se *StatusError
	return errors.As(err, &se) && se.Code == code
}
