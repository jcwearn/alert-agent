package controller

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/jcwearn/alert-agent/internal/store"
)

// How long a block holds runs back before the dispatcher tries again. A try
// while the limit still stands costs nothing: the API refuses it before any
// tokens are spent.
const (
	usageLimitRetry = time.Hour
	authRetry       = 30 * time.Minute
)

// BlockNotifier tells a human that Claude refuses every run until someone
// (auth) or the clock (usage limit) fixes it. It is sent once per block.
type BlockNotifier interface {
	ClaudeBlocked(ctx context.Context, b store.ClaudeBlock, queued int, runbookURL string) error
}

// EnableBlockNotices sets where block notices go.
func (s *Server) EnableBlockNotices(ns ...BlockNotifier) {
	s.blockNotify = ns
}

// claudeBlocked handles a run that failed on the token or the usage limit:
// it records the block (the dispatcher starts nothing while it stands),
// queues a held successor for an alert run's incidents, and tells a human
// the first time.
func (s *Server) claudeBlocked(ctx context.Context, run store.Run) {
	retry := usageLimitRetry
	if run.FailureReason == store.FailureAuth {
		retry = authRetry
	}
	until := s.now().Add(retry)
	started, err := s.store.SetClaudeBlock(ctx, run.FailureReason, until)
	if err != nil {
		slog.Error("recording claude block", "run", run.ID, "err", err)
	}
	if run.Kind != store.KindChat {
		if next, ok, err := s.store.DeferRun(ctx, run.ID, until); err != nil {
			slog.Error("deferring run", "run", run.ID, "err", err)
		} else if ok {
			slog.Info("run deferred", "run", run.ID, "next", next, "reason", run.FailureReason, "until", until)
		}
	}
	slog.Warn("claude blocked", "reason", run.FailureReason, "run", run.ID, "until", until, "new", started)
	if !started || err != nil {
		return
	}
	b := store.ClaudeBlock{Reason: run.FailureReason, Until: until, Since: s.now()}
	queued := 0
	if depth, _, err := s.store.QueueDepth(ctx); err == nil {
		for _, n := range depth {
			queued += n
		}
	}
	runbook := strings.TrimRight(s.opts.RunbookURL, "/")
	for _, n := range s.blockNotify {
		go func() {
			if err := n.ClaudeBlocked(context.Background(), b, queued, runbook); err != nil {
				slog.Error("block notification", "err", err)
			}
		}()
	}
}

// claudeUnblocked clears a standing block after a run succeeded.
func (s *Server) claudeUnblocked(ctx context.Context) {
	cleared, err := s.store.ClearClaudeBlock(ctx)
	if err != nil {
		slog.Error("clearing claude block", "err", err)
		return
	}
	if cleared {
		slog.Info("claude block cleared: a run succeeded")
	}
}

func blockSummary(b store.ClaudeBlock, queued int) (title, body string) {
	at := b.Until.Local().Format("15:04 MST")
	switch b.Reason {
	case store.FailureAuth:
		return "medic: Claude rejects the token",
			fmt.Sprintf("Runs are failing authentication. %d run(s) wait; the next try is at %s. Rotate the token (runbook: token rotation).", queued, at)
	default:
		return "medic: Claude usage limit reached",
			fmt.Sprintf("%d run(s) are deferred until %s, then retried. Nothing to do unless it lasts.", queued, at)
	}
}

func blockNtfyMessage(topic string, b store.ClaudeBlock, queued int, runbookURL string) ntfyMessage {
	title, body := blockSummary(b, queued)
	m := ntfyMessage{Topic: topic, Title: title, Message: body, Tags: []string{"hourglass"}, Priority: 3}
	if b.Reason == store.FailureAuth {
		m.Tags, m.Priority = []string{"key"}, 4
	}
	if strings.HasPrefix(runbookURL, "http") {
		m.Click = runbookURL
		m.Actions = []ntfyAction{{Action: "view", Label: "Runbook", URL: runbookURL}}
	}
	return m
}

// ClaudeBlocked publishes the block notice.
func (n *NtfyReviewer) ClaudeBlocked(ctx context.Context, b store.ClaudeBlock, queued int, runbookURL string) error {
	return n.publish(ctx, blockNtfyMessage(n.Topic, b, queued, runbookURL))
}

// ClaudeBlocked posts the block notice.
func (n *SlackNotifier) ClaudeBlocked(ctx context.Context, b store.ClaudeBlock, queued int, runbookURL string) error {
	title, body := blockSummary(b, queued)
	text := fmt.Sprintf(":hourglass: *%s*\n%s", slackEscape(title), slackEscape(body))
	if strings.HasPrefix(runbookURL, "http") {
		text += fmt.Sprintf("\n<%s|Runbook>", runbookURL)
	}
	return n.post(ctx, slackPayload{Text: title, Blocks: []slackBlock{mrkdwnSection(text)}})
}

// stateCollector reports the queue and the block from the database at
// scrape time, so the numbers are right after a restart too.
type stateCollector struct {
	store  Store
	queued *prometheus.Desc
	oldest *prometheus.Desc
	block  *prometheus.Desc
}

func newStateCollector(st Store) *stateCollector {
	return &stateCollector{
		store: st,
		queued: prometheus.NewDesc("alert_agent_runs_queued",
			"Runs waiting to start, by kind (diagnose, chat); held and deferred runs included.", []string{"kind"}, nil),
		oldest: prometheus.NewDesc("alert_agent_oldest_queued_run_age_seconds",
			"Age of the oldest queued run; 0 when none is queued.", nil, nil),
		block: prometheus.NewDesc("alert_agent_claude_blocked",
			"1 while Claude refuses every run, by reason (auth, usage_limit); cleared by the next successful run.", []string{"reason"}, nil),
	}
}

func (c *stateCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- c.queued
	ch <- c.oldest
	ch <- c.block
}

func (c *stateCollector) Collect(ch chan<- prometheus.Metric) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if depth, oldest, err := c.store.QueueDepth(ctx); err != nil {
		slog.Warn("metrics: queue depth", "err", err)
	} else {
		for kind, n := range depth {
			ch <- prometheus.MustNewConstMetric(c.queued, prometheus.GaugeValue, float64(n), kind)
		}
		ch <- prometheus.MustNewConstMetric(c.oldest, prometheus.GaugeValue, oldest.Seconds())
	}
	b, blocked, err := c.store.GetClaudeBlock(ctx)
	if err != nil {
		slog.Warn("metrics: claude block", "err", err)
		return
	}
	for _, reason := range []string{store.FailureAuth, store.FailureUsageLimit} {
		v := 0.0
		if blocked && b.Reason == reason {
			v = 1
		}
		ch <- prometheus.MustNewConstMetric(c.block, prometheus.GaugeValue, v, reason)
	}
}

// WatchTokenDate exports when the Claude token was created, read by get now
// and every interval after, so a rotation shows without a restart. The value
// is 0 when get fails (the annotation is missing or unreadable), which the
// TokenDateMissing alert catches. Registered only when called, so a
// controller that runs no agent reports nothing.
func (s *Server) WatchTokenDate(ctx context.Context, get func(context.Context) (time.Time, error), interval time.Duration) {
	g := prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "alert_agent_claude_token_created_timestamp_seconds",
		Help: "When the Claude OAuth token was created (the alert-agent/token-created annotation on its Secret); 0 if unknown.",
	})
	s.metrics.registry.MustRegister(g)
	refresh := func() {
		t, err := get(ctx)
		if err != nil {
			slog.Warn("reading the claude token's creation date", "err", err)
			g.Set(0)
			return
		}
		g.Set(float64(t.Unix()))
	}
	refresh()
	go func() {
		tick := time.NewTicker(interval)
		defer tick.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-tick.C:
				refresh()
			}
		}
	}()
}

// ParseTokenDate reads the token-created annotation: a date (2026-11-24) or
// an RFC 3339 time.
func ParseTokenDate(v string) (time.Time, error) {
	v = strings.TrimSpace(v)
	if t, err := time.Parse(time.DateOnly, v); err == nil {
		return t, nil
	}
	return time.Parse(time.RFC3339, v)
}
