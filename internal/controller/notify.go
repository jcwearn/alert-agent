package controller

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/jcwearn/alert-agent/internal/store"
)

// Notifier tells a human how a run went.
type Notifier interface {
	// pr is the PR this run opened, if any.
	RunFinished(ctx context.Context, in store.Incident, run store.Run, pr *store.PullRequest)
}

// SlackNotifier posts run results to a Slack incoming webhook, and the
// "needs your review" notice for a PR. Diagnoses go to Slack only, so a
// diagnosis does not double the pushes an alert already caused; review
// notices go to both Slack and ntfy.
type SlackNotifier struct {
	WebhookURL string
	PublicURL  string
	HTTP       *http.Client
}

// RunFinished posts one message. Failures are logged, not retried: the run
// and its report are already safe in the database.
func (n *SlackNotifier) RunFinished(ctx context.Context, in store.Incident, run store.Run, pr *store.PullRequest) {
	if err := n.post(ctx, slackMessage(n.PublicURL, in, run, pr)); err != nil {
		slog.Error("slack notify", "run", run.ID, "err", err)
	}
}

// NeedsReview posts the "needs your review" notice for a PR whose checks
// have settled, alongside the ntfy push.
func (n *SlackNotifier) NeedsReview(ctx context.Context, pr store.PullRequest, incidentURL string) error {
	return n.post(ctx, slackReviewMessage(pr, incidentURL))
}

func (n *SlackNotifier) post(ctx context.Context, msg slackPayload) error {
	body, err := json.Marshal(msg)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, n.WebhookURL, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
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
		return fmt.Errorf("slack answered %s", resp.Status)
	}
	return nil
}

func slackReviewMessage(pr store.PullRequest, incidentURL string) slackPayload {
	where := pr.AlertName
	if pr.Namespace != "" {
		where += " in " + pr.Namespace
	}
	incident := fmt.Sprintf("incident #%d", pr.IncidentID)
	if strings.HasPrefix(incidentURL, "http") {
		incident = fmt.Sprintf("<%s|%s>", incidentURL, incident)
	}
	checks := map[string]string{
		store.ChecksSuccess: ":white_check_mark: CI passed.",
		store.ChecksFailure: ":x: CI failed.",
		store.ChecksPending: ":hourglass_flowing_sand: CI still running.",
	}[pr.Checks]
	return slackPayload{
		Text: fmt.Sprintf("Review PR #%d: %s", pr.Number, pr.Title),
		Blocks: []slackBlock{mrkdwnSection(fmt.Sprintf(":wrench: *Review* <%s|PR #%d> %s\nProposed for %s (%s). %s",
			pr.URL, pr.Number, slackEscape(pr.Title), incident, slackEscape(where), checks))},
	}
}

// Reviewers sends a review notice to every reviewer, so one failing channel
// does not silence the others.
type Reviewers []Reviewer

// NeedsReview notifies each reviewer and returns their errors joined.
func (rs Reviewers) NeedsReview(ctx context.Context, pr store.PullRequest, incidentURL string) error {
	var errs []error
	for _, r := range rs {
		if err := r.NeedsReview(ctx, pr, incidentURL); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// slackPayload is an incoming-webhook message: Block Kit blocks for the
// channel, and plain text for the push notification preview, which shows
// `text` and not the blocks.
type slackPayload struct {
	Text   string       `json:"text"`
	Blocks []slackBlock `json:"blocks"`
}

type slackBlock struct {
	Type     string       `json:"type"`
	Text     *slackText   `json:"text,omitempty"`
	Elements []*slackText `json:"elements,omitempty"`
}

type slackText struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

func mrkdwnSection(s string) slackBlock {
	return slackBlock{Type: "section", Text: &slackText{Type: "mrkdwn", Text: clipRunes(s, sectionLimit)}}
}

// sectionLimit is Slack's cap on a section block's text.
const sectionLimit = 3000

// notifySections are the parts of a report worth a notification. Evidence,
// cause and asides stay on the run page, one click away.
var notifySections = []string{"Summary", "Recommended fix", "Confidence"}

func slackMessage(publicURL string, in store.Incident, run store.Run, pr *store.PullRequest) slackPayload {
	runURL := fmt.Sprintf("%s/runs/%d", strings.TrimRight(publicURL, "/"), run.ID)
	where := in.AlertName
	if in.Namespace != "" {
		where += " in " + in.Namespace
	}
	link := fmt.Sprintf("<%s|incident #%d>", runURL, in.ID)

	if run.Status != store.RunSucceeded {
		return slackPayload{
			Text: "Agent run failed: " + where,
			Blocks: []slackBlock{
				mrkdwnSection(fmt.Sprintf(":warning: *Agent run failed:* %s (%s)", slackEscape(where), link)),
				mrkdwnSection("```" + slackEscape(clipRunes(run.Error, 2000)) + "```"),
			},
		}
	}

	sections := reportSections(run.Report)
	blocks := []slackBlock{mrkdwnSection(fmt.Sprintf(":mag: *Diagnosis:* %s (%s)", slackEscape(where), link))}
	found := false
	for _, name := range notifySections {
		body, ok := sections[strings.ToLower(name)]
		if !ok || strings.TrimSpace(body) == "" {
			continue
		}
		found = true
		blocks = append(blocks, mrkdwnSection("*"+name+"*\n"+toMrkdwn(body)))
	}
	if !found {
		// A report that ignored the requested shape: send it whole, clipped.
		blocks = append(blocks, mrkdwnSection(toMrkdwn(run.Report)))
	}
	if pr != nil {
		blocks = append(blocks, mrkdwnSection(fmt.Sprintf(":wrench: *Proposed fix:* <%s|PR #%d> %s",
			pr.URL, pr.Number, slackEscape(pr.Title))))
	}
	blocks = append(blocks, slackBlock{Type: "context", Elements: []*slackText{
		{Type: "mrkdwn", Text: fmt.Sprintf("<%s|Full report and transcript>", runURL)},
	}})

	preview := "Diagnosis: " + where
	if s := firstLine(sections["summary"]); s != "" {
		preview += ": " + clipRunes(plainText(s), 150)
	}
	return slackPayload{Text: preview, Blocks: blocks}
}

// reportSections splits a Markdown report on its "## " headings, keyed by
// lower-cased heading.
func reportSections(report string) map[string]string {
	out := map[string]string{}
	var name string
	var body strings.Builder
	flush := func() {
		if name != "" {
			out[name] = strings.TrimSpace(body.String())
		}
		body.Reset()
	}
	for _, line := range strings.Split(report, "\n") {
		if h, ok := strings.CutPrefix(line, "## "); ok {
			flush()
			name = strings.ToLower(strings.TrimSpace(h))
			continue
		}
		body.WriteString(line)
		body.WriteString("\n")
	}
	flush()
	return out
}

var (
	mdBold    = regexp.MustCompile(`\*\*([^*\n]+)\*\*`)
	mdLink    = regexp.MustCompile(`\[([^\]\n]+)\]\((https?://[^)\s]+)\)`)
	mdHeading = regexp.MustCompile(`(?m)^#{1,6}\s+(.+)$`)
	mdBullet  = regexp.MustCompile(`(?m)^(\s*)[-*]\s+`)
)

// toMrkdwn converts the Markdown the agent writes into Slack's mrkdwn:
// headings and **bold** become *bold*, [text](url) becomes <url|text>, and
// list markers become bullets. Text is escaped first, so report content can
// never forge a mention or a link of its own.
func toMrkdwn(md string) string {
	s := slackEscape(md)
	s = mdHeading.ReplaceAllString(s, "*$1*")
	s = mdBullet.ReplaceAllString(s, "$1• ")
	s = mdBold.ReplaceAllString(s, "*$1*")
	s = mdLink.ReplaceAllString(s, "<$2|$1>")
	return s
}

// slackEscape escapes the three characters Slack treats as control
// sequences, so report text cannot forge links or mentions.
func slackEscape(s string) string {
	return strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;").Replace(s)
}

// plainText strips Markdown emphasis and code markers for the push preview,
// which shows them literally.
func plainText(s string) string {
	return strings.NewReplacer("**", "", "__", "", "`", "").Replace(s)
}

func firstLine(s string) string {
	line, _, _ := strings.Cut(strings.TrimSpace(s), "\n")
	return strings.TrimSpace(line)
}

// clipRunes shortens s to at most n runes, never splitting a character.
func clipRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n-1]) + "…"
}
