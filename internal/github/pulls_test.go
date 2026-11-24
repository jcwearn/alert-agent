package github

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestCreatePullRequest(t *testing.T) {
	var got NewPullRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/repos/o/r/pulls" || r.Header.Get("Authorization") != "Bearer tok" {
			http.Error(w, "wrong request", http.StatusBadRequest)
			return
		}
		_ = json.NewDecoder(r.Body).Decode(&got)
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"number":7,"html_url":"https://github.com/o/r/pull/7","state":"open","head":{"sha":"abc","ref":"agent/3"}}`))
	}))
	t.Cleanup(srv.Close)

	app := &App{BaseURL: srv.URL}
	pr, err := app.CreatePullRequest(t.Context(), "tok", "o/r", NewPullRequest{Title: "t", Body: "b", Head: "agent/3", Base: "main"})
	if err != nil {
		t.Fatal(err)
	}
	if pr.Number != 7 || pr.Head.SHA != "abc" || got.Head != "agent/3" || got.Base != "main" {
		t.Errorf("pr = %+v, sent %+v", pr, got)
	}
}

func TestCreatePullRequestError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, `{"message":"Validation Failed"}`, http.StatusUnprocessableEntity)
	}))
	t.Cleanup(srv.Close)
	app := &App{BaseURL: srv.URL}
	if _, err := app.CreatePullRequest(t.Context(), "tok", "o/r", NewPullRequest{}); err == nil {
		t.Fatal("422 should be an error")
	}
}

func TestChecksRollup(t *testing.T) {
	cases := []struct {
		name, body, want string
	}{
		{"none yet", `{"total_count":0,"check_runs":[]}`, ChecksPending},
		{"running", `{"check_runs":[{"status":"completed","conclusion":"success"},{"status":"in_progress"}]}`, ChecksPending},
		{"all green", `{"check_runs":[{"status":"completed","conclusion":"success"},{"status":"completed","conclusion":"skipped"}]}`, ChecksSuccess},
		{"one failed while another runs", `{"check_runs":[{"status":"completed","conclusion":"failure"},{"status":"queued"}]}`, ChecksFailure},
		{"cancelled", `{"check_runs":[{"status":"completed","conclusion":"cancelled"}]}`, ChecksFailure},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/repos/o/r/commits/abc/check-runs" {
					http.NotFound(w, r)
					return
				}
				_, _ = w.Write([]byte(c.body))
			}))
			t.Cleanup(srv.Close)
			got, err := (&App{BaseURL: srv.URL}).ChecksRollup(t.Context(), "tok", "o/r", "abc")
			if err != nil || got != c.want {
				t.Errorf("got %q, %v; want %q", got, err, c.want)
			}
		})
	}
}
