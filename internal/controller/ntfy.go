package controller

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/jcwearn/alert-agent/internal/store"
)

// Reviewer tells a human a PR is waiting for them.
type Reviewer interface {
	NeedsReview(ctx context.Context, pr store.PullRequest, incidentURL string) error
}

// NtfyReviewer publishes "needs your review" to an ntfy topic. ntfy is kept
// for exactly this: diagnoses go to Slack, so a push means there is
// something to do.
type NtfyReviewer struct {
	BaseURL string // e.g. http://ntfy-svc.ntfy.svc.cluster.local:4080
	Topic   string
	Token   string
	HTTP    *http.Client
}

type ntfyAction struct {
	Action string `json:"action"`
	Label  string `json:"label"`
	URL    string `json:"url"`
}

type ntfyMessage struct {
	Topic    string       `json:"topic"`
	Title    string       `json:"title"`
	Message  string       `json:"message"`
	Click    string       `json:"click,omitempty"`
	Tags     []string     `json:"tags,omitempty"`
	Priority int          `json:"priority,omitempty"`
	Actions  []ntfyAction `json:"actions,omitempty"`
}

func reviewMessage(topic string, pr store.PullRequest, incidentURL string) ntfyMessage {
	where := pr.AlertName
	if pr.Namespace != "" {
		where += " in " + pr.Namespace
	}
	checks := map[string]string{
		store.ChecksSuccess: "CI passed.",
		store.ChecksFailure: "CI FAILED.",
		store.ChecksPending: "CI still running.",
	}[pr.Checks]
	tags := []string{"wrench"}
	if pr.Checks == store.ChecksFailure {
		tags = append(tags, "x")
	}
	m := ntfyMessage{
		Topic:    topic,
		Title:    fmt.Sprintf("Review PR #%d: %s", pr.Number, pr.Title),
		Message:  fmt.Sprintf("Proposed for incident #%d (%s). %s", pr.IncidentID, where, checks),
		Click:    pr.URL,
		Tags:     tags,
		Priority: 3,
		Actions:  []ntfyAction{{Action: "view", Label: "Open PR", URL: pr.URL}},
	}
	if incidentURL != "" && !strings.HasPrefix(incidentURL, "/") {
		m.Actions = append(m.Actions, ntfyAction{Action: "view", Label: "Incident", URL: incidentURL})
	}
	return m
}

// NeedsReview publishes one message, using ntfy's JSON publishing (POST to
// the server root), which carries UTF-8 titles without header encoding.
func (n *NtfyReviewer) NeedsReview(ctx context.Context, pr store.PullRequest, incidentURL string) error {
	body, err := json.Marshal(reviewMessage(n.Topic, pr, incidentURL))
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(n.BaseURL, "/")+"/", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if n.Token != "" {
		req.Header.Set("Authorization", "Bearer "+n.Token)
	}
	client := n.HTTP
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("ntfy answered %s", resp.Status)
	}
	return nil
}
