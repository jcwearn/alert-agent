package github

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestCommentListingsPaginateAndFilter(t *testing.T) {
	var mu sync.Mutex
	var queries []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		queries = append(queries, r.URL.Path+"?"+r.URL.RawQuery)
		mu.Unlock()
		if r.Header.Get("Authorization") != "Bearer tok" {
			http.Error(w, "auth", http.StatusUnauthorized)
			return
		}
		n := 1
		if r.URL.Query().Get("page") == "1" {
			n = 100
		}
		var items []string
		for i := range n {
			items = append(items, fmt.Sprintf(`{"id":%d,"body":"b","user":{"login":"me","type":"User"},"created_at":"2026-11-25T10:00:00Z","in_reply_to_id":7,"line":3}`, i+1))
		}
		_, _ = w.Write([]byte("[" + strings.Join(items, ",") + "]"))
	}))
	t.Cleanup(srv.Close)
	a := &App{BaseURL: srv.URL}
	since := time.Date(2026, 11, 25, 9, 0, 0, 0, time.FixedZone("EST", -5*3600))

	ic, err := a.IssueComments(t.Context(), "tok", "o/r", 12, since)
	if err != nil || len(ic) != 101 || ic[0].User.Type != "User" {
		t.Fatalf("issue comments: %d %v", len(ic), err)
	}
	rc, err := a.ReviewComments(t.Context(), "tok", "o/r", 12, since)
	if err != nil || len(rc) != 101 || rc[0].InReplyToID != 7 || rc[0].Line == nil || *rc[0].Line != 3 {
		t.Fatalf("review comments: %d %+v %v", len(rc), rc[0], err)
	}
	if _, err := a.Reviews(t.Context(), "tok", "o/r", 12); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	for _, want := range []string{
		"/repos/o/r/issues/12/comments?page=1&per_page=100&since=2026-11-25T14%3A00%3A00Z",
		"/repos/o/r/issues/12/comments?page=2&per_page=100&since=2026-11-25T14%3A00%3A00Z",
		"/repos/o/r/pulls/12/comments?page=1&per_page=100&since=2026-11-25T14%3A00%3A00Z",
		"/repos/o/r/pulls/12/reviews?page=1&per_page=100",
	} {
		found := false
		for _, q := range queries {
			found = found || q == want
		}
		if !found {
			t.Errorf("no request %s in %v", want, queries)
		}
	}
}

func TestReactAndReply(t *testing.T) {
	type call struct{ method, path, body string }
	var mu sync.Mutex
	var calls []call
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]string
		_ = json.NewDecoder(r.Body).Decode(&body)
		mu.Lock()
		calls = append(calls, call{r.Method, r.URL.Path, body["content"] + body["body"]})
		mu.Unlock()
		switch {
		case strings.HasSuffix(r.URL.Path, "/issues/comments/5/reactions"):
			w.WriteHeader(http.StatusOK) // already reacted
		case strings.HasSuffix(r.URL.Path, "/reactions"):
			w.WriteHeader(http.StatusCreated)
		default:
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"id":99,"html_url":"https://github.com/o/r/pull/12#c99"}`))
		}
	}))
	t.Cleanup(srv.Close)
	a := &App{BaseURL: srv.URL}
	ctx := t.Context()

	if err := a.ReactToIssueComment(ctx, "tok", "o/r", 5, "eyes"); err != nil {
		t.Errorf("a repeated reaction (200) is not an error: %v", err)
	}
	if err := a.ReactToReviewComment(ctx, "tok", "o/r", 6, "eyes"); err != nil {
		t.Error(err)
	}
	c, err := a.CreateIssueComment(ctx, "tok", "o/r", 12, "hello")
	if err != nil || c.HTMLURL == "" {
		t.Fatalf("comment: %+v %v", c, err)
	}
	if _, err := a.ReplyToReviewComment(ctx, "tok", "o/r", 12, 7, "in thread"); err != nil {
		t.Fatal(err)
	}
	want := []call{
		{"POST", "/repos/o/r/issues/comments/5/reactions", "eyes"},
		{"POST", "/repos/o/r/pulls/comments/6/reactions", "eyes"},
		{"POST", "/repos/o/r/issues/12/comments", "hello"},
		{"POST", "/repos/o/r/pulls/12/comments/7/replies", "in thread"},
	}
	mu.Lock()
	defer mu.Unlock()
	if fmt.Sprint(calls) != fmt.Sprint(want) {
		t.Errorf("calls = %v\nwant %v", calls, want)
	}
}
