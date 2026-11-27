package controller

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jcwearn/alert-agent/internal/alertmanager"
	"github.com/jcwearn/alert-agent/internal/store"
)

type fakeStore struct {
	pingErr   error
	recordErr error
	result    store.RecordResult
	recorded  []alertmanager.Payload
	incidents []store.Incident
	alerts    []store.IncidentAlert
	// the last home page query
	listFilter            store.IncidentFilter
	listBefore, listAfter *store.Cursor

	enqueued  []int64
	holds     []time.Duration
	batchRun  int64 // when set, EnqueueRun with a hold joins this run
	members   map[int64][]int64
	leftover  map[int64][]int64
	links     map[[2]int64]string
	runs      map[int64]store.Run
	tokenHash map[string]int64
	events    map[int64][]store.RunEvent
	finished  map[int64]store.RunResult

	prState   *prState
	chatState *chatState
	actState  *actState

	block    *store.ClaudeBlock
	deferred map[int64]time.Time // failed run -> the successor's hold
	queued   map[string]int
}

func (f *fakeStore) DeferRun(_ context.Context, runID int64, until time.Time) (next int64, deferred bool, err error) {
	if r, ok := f.runs[runID]; !ok || r.Kind == store.KindChat {
		return 0, false, nil
	}
	if f.deferred == nil {
		f.deferred = map[int64]time.Time{}
	}
	f.deferred[runID] = until
	return runID + 1000, true, nil
}

func (f *fakeStore) QueueDepth(context.Context) (map[string]int, time.Duration, error) {
	return f.queued, 0, nil
}

func (f *fakeStore) GetClaudeBlock(context.Context) (store.ClaudeBlock, bool, error) {
	if f.block == nil {
		return store.ClaudeBlock{}, false, nil
	}
	return *f.block, true, nil
}

func (f *fakeStore) SetClaudeBlock(_ context.Context, reason string, until time.Time) (bool, error) {
	started := f.block == nil || f.block.Reason != reason
	f.block = &store.ClaudeBlock{Reason: reason, Until: until}
	return started, nil
}

func (f *fakeStore) ClearClaudeBlock(context.Context) (bool, error) {
	had := f.block != nil
	f.block = nil
	return had, nil
}

func (f *fakeStore) EnqueueRun(_ context.Context, incidentID int64, hold time.Duration) (runID int64, batched bool, err error) {
	f.enqueued = append(f.enqueued, incidentID)
	f.holds = append(f.holds, hold)
	if hold > 0 && f.batchRun != 0 {
		return f.batchRun, true, nil
	}
	return int64(len(f.enqueued)), false, nil
}

func (f *fakeStore) RunByTokenHash(_ context.Context, hash []byte) (store.Run, error) {
	id, ok := f.tokenHash[string(hash)]
	if !ok || f.runs[id].Status != store.RunRunning {
		return store.Run{}, store.ErrNotFound
	}
	return f.runs[id], nil
}

func (f *fakeStore) AppendRunEvents(_ context.Context, runID int64, events []store.RunEvent) error {
	if f.events == nil {
		f.events = map[int64][]store.RunEvent{}
	}
	f.events[runID] = append(f.events[runID], events...)
	return nil
}

func (f *fakeStore) FinishRun(_ context.Context, runID int64, res store.RunResult) (bool, error) {
	r, ok := f.runs[runID]
	if !ok || r.Status != store.RunRunning {
		return false, nil
	}
	if f.finished == nil {
		f.finished = map[int64]store.RunResult{}
	}
	f.finished[runID] = res
	r.Status, r.Report, r.Error, r.FailureReason = res.Status, res.Report, res.Error, res.Reason
	f.runs[runID] = r
	return true, nil
}

func (f *fakeStore) GetRun(_ context.Context, id int64) (store.Run, error) {
	r, ok := f.runs[id]
	if !ok {
		return store.Run{}, store.ErrNotFound
	}
	return r, nil
}

func (f *fakeStore) ListRuns(_ context.Context, incidentID int64) ([]store.Run, error) {
	var out []store.Run
	for _, r := range f.runs {
		if r.IncidentID == incidentID {
			out = append(out, r)
		}
	}
	return out, nil
}

func (f *fakeStore) RunEvents(_ context.Context, runID int64) ([]store.RunEvent, error) {
	return f.events[runID], nil
}

func (f *fakeStore) Ping(context.Context) error { return f.pingErr }

func (f *fakeStore) RecordWebhook(_ context.Context, _ []byte, p alertmanager.Payload) (store.RecordResult, error) {
	f.recorded = append(f.recorded, p)
	return f.result, f.recordErr
}

func (f *fakeStore) ListIncidents(context.Context, int) ([]store.Incident, error) {
	return f.incidents, nil
}

func (f *fakeStore) ListIncidentPage(_ context.Context, flt store.IncidentFilter, before, after *store.Cursor, _ int) (store.IncidentListPage, error) {
	f.listFilter, f.listBefore, f.listAfter = flt, before, after
	return store.IncidentListPage{Incidents: f.incidents}, nil
}

func (f *fakeStore) IncidentFacets(context.Context) (namespaces, severities []string, err error) {
	return []string{"ebooks"}, []string{"warning"}, nil
}

func (f *fakeStore) GetIncident(_ context.Context, id int64) (store.Incident, []store.IncidentAlert, error) {
	for _, in := range f.incidents {
		if in.ID == id {
			return in, f.alerts, nil
		}
	}
	return store.Incident{}, nil, store.ErrNotFound
}

const validPayload = `{"version":"4","groupKey":"g1","status":"firing",
  "groupLabels":{"alertname":"KubePodCrashLooping","namespace":"ebooks"},
  "alerts":[{"status":"firing","fingerprint":"f1","labels":{"pod":"a"},"startsAt":"2026-11-24T10:00:00Z"}]}`

// do makes one request and returns the status and body, with the body read
// and closed.
func do(t *testing.T, h http.Handler, method, path, body string) (status int, respBody string) {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	req, err := http.NewRequestWithContext(t.Context(), method, srv.URL+path, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode, string(b)
}

func counter(t *testing.T, s *Server, name, label string) float64 {
	t.Helper()
	families, err := s.metrics.registry.Gather()
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range families {
		if f.GetName() != name {
			continue
		}
		for _, m := range f.GetMetric() {
			for _, l := range m.GetLabel() {
				if l.GetValue() == label {
					return m.GetCounter().GetValue()
				}
			}
		}
	}
	t.Fatalf("no series %s{%s}", name, label)
	return 0
}

func TestWebhookRecords(t *testing.T) {
	fs := &fakeStore{result: store.RecordResult{IncidentID: 7, Created: true}}
	s := New(fs, Options{}, nil, nil)

	status, _ := do(t, s.WebhookHandler(), http.MethodPost, "/webhook/alertmanager", validPayload)
	if status != http.StatusNoContent {
		t.Fatalf("status = %d", status)
	}
	if len(fs.recorded) != 1 || fs.recorded[0].GroupKey != "g1" {
		t.Fatalf("payload not passed to store: %+v", fs.recorded)
	}
	if got := counter(t, s, "alert_agent_incidents_total", "opened"); got != 1 {
		t.Errorf("opened counter = %v", got)
	}
	if got := counter(t, s, "alert_agent_webhooks_total", "ok"); got != 1 {
		t.Errorf("ok counter = %v", got)
	}
}

func TestWebhookRejectsBadPayload(t *testing.T) {
	fs := &fakeStore{}
	s := New(fs, Options{}, nil, nil)

	status, _ := do(t, s.WebhookHandler(), http.MethodPost, "/webhook/alertmanager", `{"version":"3"}`)
	if status != http.StatusBadRequest {
		t.Fatalf("status = %d", status)
	}
	if len(fs.recorded) != 0 {
		t.Error("bad payload reached the store")
	}
	if got := counter(t, s, "alert_agent_webhooks_total", "bad_request"); got != 1 {
		t.Errorf("bad_request counter = %v", got)
	}
}

// A store failure must be a 5xx, so Alertmanager retries the delivery.
func TestWebhookStoreErrorIs5xx(t *testing.T) {
	s := New(&fakeStore{recordErr: errors.New("db down")}, Options{}, nil, nil)

	status, _ := do(t, s.WebhookHandler(), http.MethodPost, "/webhook/alertmanager", validPayload)
	if status != http.StatusInternalServerError {
		t.Fatalf("status = %d", status)
	}
}

func TestWebhookNotOnUIPort(t *testing.T) {
	s := New(&fakeStore{}, Options{}, nil, nil)
	if status, _ := do(t, s.UIHandler(), http.MethodPost, "/webhook/alertmanager", validPayload); status == http.StatusNoContent {
		t.Fatal("webhook must only be served on the webhook port")
	}
}

func TestReadyzFollowsDatabase(t *testing.T) {
	fs := &fakeStore{}
	s := New(fs, Options{}, nil, nil)
	if status, _ := do(t, s.UIHandler(), http.MethodGet, "/readyz", ""); status != http.StatusOK {
		t.Fatal("not ready")
	}
	fs.pingErr = errors.New("down")
	if status, _ := do(t, s.UIHandler(), http.MethodGet, "/readyz", ""); status != http.StatusServiceUnavailable {
		t.Fatal("still ready")
	}
	// Liveness ignores the database.
	if status, _ := do(t, s.UIHandler(), http.MethodGet, "/healthz", ""); status != http.StatusOK {
		t.Fatal("healthz failed")
	}
}

func TestIncidentPages(t *testing.T) {
	opened := time.Date(2026, 11, 24, 10, 0, 0, 0, time.UTC)
	fs := &fakeStore{
		incidents: []store.Incident{{
			ID: 42, AlertName: "KubePodCrashLooping", Namespace: "ebooks", Severity: "warning",
			Summary: "Pod <b>a</b> is crash looping", AlertStatus: "firing", OpenedAt: opened, UpdatedAt: opened, AlertCount: 1,
		}},
		alerts: []store.IncidentAlert{{
			Fingerprint: "f1", Status: "firing", Labels: map[string]string{"pod": "a"},
			Annotations: map[string]string{"description": "restarting"}, StartsAt: opened,
			GeneratorURL: "javascript:alert(1)",
		}},
	}
	s := New(fs, Options{}, nil, nil)
	s.now = func() time.Time { return opened.Add(90 * time.Minute) }

	_, body := do(t, s.UIHandler(), http.MethodGet, "/", "")
	for _, want := range []string{`href="/incidents/42"`, "#42", "Kube<wbr>Pod<wbr>Crash<wbr>Looping", "1h ago", "Pod &lt;b&gt;a&lt;/b&gt;"} {
		if !strings.Contains(body, want) {
			t.Errorf("list page missing %q", want)
		}
	}

	status, body := do(t, s.UIHandler(), http.MethodGet, "/incidents/42", "")
	if status != http.StatusOK || !strings.Contains(body, "pod=a") || !strings.Contains(body, "restarting") {
		t.Errorf("detail page wrong (status %d)", status)
	}
	if strings.Contains(body, "javascript:") {
		t.Error("unsafe generator URL rendered as a link")
	}

	for _, path := range []string{"/incidents/9", "/incidents/abc"} {
		if status, _ := do(t, s.UIHandler(), http.MethodGet, path, ""); status != http.StatusNotFound {
			t.Errorf("%s status = %d, want 404", path, status)
		}
	}
}
