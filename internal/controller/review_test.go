package controller

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/jcwearn/alert-agent/internal/store"
)

var reviewPR = store.PullRequest{Number: 1149, URL: "https://github.com/o/r/pull/1149", Title: "fix(x): <raise> limit",
	IncidentID: 16, AlertName: "MedicDrillOOM", Namespace: "medic-drill", Checks: store.ChecksSuccess}

func TestSlackReviewNotice(t *testing.T) {
	var got slackPayload
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(b, &got); err != nil {
			t.Error(err)
		}
	}))
	t.Cleanup(srv.Close)

	n := &SlackNotifier{WebhookURL: srv.URL}
	if err := n.NeedsReview(t.Context(), reviewPR, "https://medic.example/incidents/16"); err != nil {
		t.Fatal(err)
	}
	if got.Text != "Review PR #1149: fix(x): <raise> limit" {
		t.Errorf("preview = %q", got.Text)
	}
	body := got.Blocks[0].Text.Text
	for _, want := range []string{"<https://github.com/o/r/pull/1149|PR #1149>", "fix(x): &lt;raise&gt; limit",
		"<https://medic.example/incidents/16|incident #16>", "MedicDrillOOM in medic-drill", "CI passed"} {
		if !strings.Contains(body, want) {
			t.Errorf("notice lacks %q: %s", want, body)
		}
	}
}

func TestSlackReviewNoticeReportsFailure(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	}))
	t.Cleanup(srv.Close)
	if err := (&SlackNotifier{WebhookURL: srv.URL}).NeedsReview(t.Context(), reviewPR, ""); err == nil {
		t.Fatal("a 403 from Slack should be an error")
	}
}

type failingReviewer struct{}

func (failingReviewer) NeedsReview(context.Context, store.PullRequest, string) error {
	return errors.New("ntfy down")
}

// One channel failing must not stop the other.
func TestReviewersNotifyAll(t *testing.T) {
	rec := &recordingReviewer{}
	err := Reviewers{failingReviewer{}, rec}.NeedsReview(t.Context(), reviewPR, "")
	if err == nil || !strings.Contains(err.Error(), "ntfy down") {
		t.Errorf("error = %v", err)
	}
	if len(rec.prs) != 1 {
		t.Fatal("the second reviewer was skipped")
	}
}
