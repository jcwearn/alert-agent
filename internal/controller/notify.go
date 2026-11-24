package controller

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/jcwearn/alert-agent/internal/store"
)

// Notifier tells a human how a run went.
type Notifier interface {
	RunFinished(ctx context.Context, in store.Incident, run store.Run)
}

// SlackNotifier posts run results to a Slack incoming webhook. Diagnoses go
// to Slack only; ntfy is kept for "needs your review" from Phase 3 on, so a
// diagnosis does not double the pushes an alert already caused.
type SlackNotifier struct {
	WebhookURL string
	PublicURL  string
	HTTP       *http.Client
}

// reportLimit keeps a message readable; the full report is one click away.
const reportLimit = 2500

// RunFinished posts one message. Failures are logged, not retried: the run
// and its report are already safe in the database.
func (n *SlackNotifier) RunFinished(ctx context.Context, in store.Incident, run store.Run) {
	text := slackText(n.PublicURL, in, run)
	body, err := json.Marshal(map[string]string{"text": text})
	if err != nil {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, n.WebhookURL, bytes.NewReader(body))
	if err != nil {
		slog.Error("slack request", "err", err)
		return
	}
	req.Header.Set("Content-Type", "application/json")
	client := n.HTTP
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(req)
	if err != nil {
		slog.Error("slack notify", "run", run.ID, "err", err)
		return
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		slog.Error("slack notify", "run", run.ID, "status", resp.Status)
	}
}

func slackText(publicURL string, in store.Incident, run store.Run) string {
	link := fmt.Sprintf("<%s/runs/%d|incident #%d>", strings.TrimRight(publicURL, "/"), run.ID, in.ID)
	where := in.AlertName
	if in.Namespace != "" {
		where += " in " + in.Namespace
	}

	var b strings.Builder
	switch run.Status {
	case store.RunSucceeded:
		fmt.Fprintf(&b, ":mag: Diagnosis: %s (%s)\n", slackEscape(where), link)
		report := run.Report
		if len(report) > reportLimit {
			report = report[:reportLimit] + "\n… (truncated; full report at the link)"
		}
		b.WriteString(slackEscape(report))
	default:
		fmt.Fprintf(&b, ":warning: Agent run failed: %s (%s)\n", slackEscape(where), link)
		b.WriteString(slackEscape(run.Error))
	}
	return b.String()
}

// slackEscape escapes the three characters Slack treats as control
// sequences, so report text cannot forge links or mentions.
func slackEscape(s string) string {
	return strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;").Replace(s)
}
