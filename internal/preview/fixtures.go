package preview

import (
	"cmp"
	"context"
	"encoding/json"
	"slices"
	"strconv"
	"sync"
	"time"

	"github.com/jcwearn/alert-agent/internal/controller"
	"github.com/jcwearn/alert-agent/internal/store"
)

// fixtures is an in-memory controller.Store holding a small, made-up
// cluster's incidents and conversations. Only what the UI reads is
// implemented; the embedded interface panics on anything else, which in a
// preview is a bug worth seeing.
type fixtures struct {
	controller.Store

	mu        sync.Mutex
	incidents []store.Incident
	alerts    map[int64][]store.IncidentAlert
	prs       []store.PullRequest
	runs      []store.Run
	threads   []store.Thread
	messages  []store.ChatMessage
	actions   []store.Action
	approvals []store.Approval
	links     map[int64][]store.Link
	// scripted turns: steps revealed over time, then a reply.
	turns map[int64]*turn
}

type turn struct {
	steps []step
	// done is when the turn finishes; zero never does (a screenshot of a
	// turn in progress).
	done  time.Duration
	reply string
}

type step struct {
	at    time.Duration
	event store.RunEvent
}

func ptr[T any](v T) *T { return &v }

func newFixtures() *fixtures {
	now := time.Now()
	ago := func(d time.Duration) time.Time { return now.Add(-d) }
	f := &fixtures{alerts: map[int64][]store.IncidentAlert{}, links: map[int64][]store.Link{}, turns: map[int64]*turn{}}

	f.incidents = []store.Incident{
		{ID: 27, AlertName: "EnvoyHighLatency", Namespace: "gateway", Severity: "warning", AlertStatus: "firing",
			Summary: "p99 latency over 2s on the main gateway for 10 minutes", OpenedAt: ago(12 * time.Minute), AlertCount: 1},
		{ID: 26, AlertName: "KubePodNotReady", Namespace: "photos", Severity: "warning", AlertStatus: "resolved",
			Summary: "photos/ml-worker-7d9f not ready for 5 minutes", OpenedAt: ago(3 * time.Hour), AlertCount: 1,
			ParentID: ptr(int64(25)), MergedAt: ptr(ago(170 * time.Minute)), MergedBy: "run:29",
			MergeReason: "The pod was not ready because it was restarting after the OOM kill in #25."},
		{ID: 25, AlertName: "KubeContainerOOMKilled", Namespace: "photos", Severity: "critical", AlertStatus: "firing",
			Summary: "ml-worker was OOMKilled 3 times in the last hour", OpenedAt: ago(3*time.Hour + 2*time.Minute), AlertCount: 1},
		{ID: 24, AlertName: "TargetDown", Namespace: "monitoring", Severity: "warning", AlertStatus: "resolved",
			Summary: "1 of 3 node-exporter targets down", OpenedAt: ago(26 * time.Hour), ResolvedAt: ptr(ago(25 * time.Hour)), AlertCount: 1},
		{ID: 23, AlertName: "NodeFilesystemAlmostFull", Namespace: "monitoring", Severity: "warning", AlertStatus: "resolved",
			Summary: "node-b /var at 91%", OpenedAt: ago(30 * time.Hour), ResolvedAt: ptr(ago(28 * time.Hour)), AlertCount: 1, Links: []int64{22}},
		{ID: 22, AlertName: "KubePersistentVolumeFillingUp", Namespace: "media", Severity: "warning", AlertStatus: "resolved",
			Summary: "media/library is 92% full", OpenedAt: ago(31 * time.Hour), ResolvedAt: ptr(ago(28 * time.Hour)), AlertCount: 1, Links: []int64{23}},
	}
	f.incidents = append(f.incidents, history(now)...)
	for i := range f.incidents {
		in := &f.incidents[i]
		in.UpdatedAt = in.OpenedAt
		status := "firing"
		if in.AlertStatus == "resolved" {
			status = "resolved"
		}
		f.alerts[in.ID] = []store.IncidentAlert{{
			Fingerprint: "fp" + strconv.FormatInt(in.ID, 10), Status: status, StartsAt: in.OpenedAt,
			Labels:       map[string]string{"alertname": in.AlertName, "namespace": in.Namespace, "severity": in.Severity},
			Annotations:  map[string]string{"description": in.Summary},
			GeneratorURL: "https://prometheus.medic.example/graph",
		}}
	}
	f.links[23] = []store.Link{{Other: f.incidents[5], Reason: "Both are the media library's volume: it lives on node-b's /var.", Actor: "run:38", RunID: ptr(int64(38)), CreatedAt: ago(40 * time.Minute)}}
	f.links[22] = []store.Link{{Other: f.incidents[4], Reason: "Both are the media library's volume: it lives on node-b's /var.", Actor: "run:38", RunID: ptr(int64(38)), CreatedAt: ago(40 * time.Minute)}}

	f.prs = []store.PullRequest{
		{ID: 1, IncidentID: 25, RunID: 29, Number: 212, URL: "https://github.com/example/homelab/pull/212",
			Title: "photos: give the ml worker 2.75Gi", Branch: "agent/25", State: store.PROpen, Checks: "success",
			CreatedAt: ago(170 * time.Minute), UpdatedAt: ago(28 * time.Minute), AlertName: "KubeContainerOOMKilled", Namespace: "photos",
			Files: []string{"apps/photos/ml-deployment.yaml"}},
		{ID: 2, IncidentID: 24, RunID: 20, Number: 208, URL: "https://github.com/example/homelab/pull/208",
			Title: "monitoring: tolerate node-exporter on tainted nodes", Branch: "agent/24", State: store.PRMerged, Checks: "success",
			CreatedAt: ago(25 * time.Hour), UpdatedAt: ago(24 * time.Hour), ClosedAt: ptr(ago(24 * time.Hour)), AlertName: "TargetDown", Namespace: "monitoring"},
	}

	done := func(r store.Run, took time.Duration, report string) store.Run {
		r.StartedAt = ptr(r.CreatedAt.Add(5 * time.Second))
		r.FinishedAt = ptr(r.StartedAt.Add(took))
		r.DurationMS = ptr(took.Milliseconds())
		r.NumTurns = ptr(int(took / (8 * time.Second)))
		r.Status, r.Report, r.Model = store.RunSucceeded, report, "claude-opus-5-5"
		if r.Kind == "" {
			r.Kind = store.KindDiagnose
		}
		return r
	}
	chat := func(id, incident, thread int64, at time.Time, took time.Duration, reply string) store.Run {
		return done(store.Run{ID: id, IncidentID: incident, ThreadID: ptr(thread), Kind: store.KindChat, CreatedAt: at}, took, reply)
	}
	f.runs = []store.Run{
		done(store.Run{ID: 20, IncidentID: 24, CreatedAt: ago(25 * time.Hour)}, 3*time.Minute,
			"node-exporter on **node-c** was not scheduled after the node was tainted for maintenance. Proposed PR #208 adds the toleration."),
		done(store.Run{ID: 27, IncidentID: 27, CreatedAt: ago(11 * time.Minute)}, 2*time.Minute,
			"Latency rose with traffic to `photos` while the ml worker was restarting; no change proposed yet."),
		done(store.Run{ID: 29, IncidentID: 25, CreatedAt: ago(3 * time.Hour)}, 4*time.Minute,
			"`ml-worker` hits its 2Gi limit during the nightly face-detection job (peak 2.71Gi). Proposed PR #212: 3Gi."),
		chat(31, 25, 1, ago(49*time.Minute), 70*time.Second, ""),
		chat(33, 25, 1, ago(29*time.Minute), 95*time.Second, ""),
		chat(35, 25, 1, ago(4*time.Minute), 40*time.Second, ""),
		chat(36, 0, 2, ago(2*time.Hour), 55*time.Second, ""),
		chat(38, 0, 3, ago(42*time.Minute), 60*time.Second, ""),
		chat(21, 24, 5, ago(24*time.Hour), 30*time.Second, ""),
		{ID: 40, ThreadID: ptr(int64(4)), Kind: store.KindChat, Status: store.RunRunning, CreatedAt: ago(40 * time.Second),
			StartedAt: ptr(ago(30 * time.Second)), Model: "claude-opus-5-5"},
	}
	f.turns[40] = &turn{steps: script(now.Add(-30*time.Second), 0,
		text("I'll look at the restarts first, then at what the pods were doing before each one."),
		tool("mcp__kubernetes__pods_list", map[string]any{"namespace": "photos"}),
		tool("mcp__kubernetes__events_list", map[string]any{"namespace": "photos"}),
		text("Three restarts, all `ml-worker`, all `OOMKilled` between 02:10 and 02:40. That is incident #25's window."),
		tool("mcp__prometheus__query_range", map[string]any{"query": `max(container_memory_working_set_bytes{namespace="photos",container="ml-worker"})`}),
	)}

	// Run 29's transcript, so the run page shows each kind of step.
	f.turns[29] = &turn{steps: script(ago(3*time.Hour), 0,
		text("ml-worker in photos was OOMKilled three times. I'll check its limit against what it actually uses."),
		tool("Read", map[string]any{"file_path": "/work/repo/apps/photos/ml-deployment.yaml"}),
		result("resources:\n  requests:\n    memory: 2Gi\n  limits:\n    memory: 2Gi"),
		tool("mcp__prometheus__query_range", map[string]any{"query": `max(container_memory_working_set_bytes{namespace="photos",container="ml-worker"})`}),
		result("peak 2.71Gi at 02:14, during the nightly face-detection job"),
		text("The limit is 2Gi and the 7-day peak is **2.71Gi**. A 3Gi limit leaves about 10% headroom; proposing that."),
	)}

	f.threads = []store.Thread{
		{ID: 1, Kind: store.ThreadIncident, IncidentID: ptr(int64(25)), AlertName: "KubeContainerOOMKilled", Namespace: "photos", CreatedAt: ago(50 * time.Minute)},
		{ID: 2, Kind: store.ThreadGeneral, Title: "How is the cluster?", CreatedAt: ago(2 * time.Hour)},
		{ID: 3, Kind: store.ThreadGeneral, Title: "Are #22 and #23 the same problem?", CreatedAt: ago(43 * time.Minute)},
		{ID: 4, Kind: store.ThreadGeneral, Title: "Why did the photos pods restart overnight?", CreatedAt: ago(41 * time.Second)},
		{ID: 5, Kind: store.ThreadIncident, IncidentID: ptr(int64(24)), AlertName: "TargetDown", Namespace: "monitoring", CreatedAt: ago(24 * time.Hour)},
	}

	ui := func(thread int64, at time.Time, page, body, status string) store.ChatMessage {
		return store.ChatMessage{ThreadID: thread, Source: store.SourceUI, Author: "you", Page: page, Body: body, Status: status, CreatedAt: at}
	}
	agent := func(thread, run int64, at time.Time, body string) store.ChatMessage {
		return store.ChatMessage{ThreadID: thread, RunID: ptr(run), Source: store.SourceAgent, Author: "agent", Body: body, Status: store.ChatReply, CreatedAt: at}
	}
	f.messages = []store.ChatMessage{
		ui(2, ago(2*time.Hour), "/", "How is the cluster?", store.ChatAnswered),
		agent(2, 36, ago(119*time.Minute), "Mostly healthy. Three things worth knowing:\n\n"+
			"- **#25 `KubeContainerOOMKilled`** (photos) is firing: the ml worker runs out of memory during the nightly job. PR #212 is waiting for your review, CI green.\n"+
			"- **#24 `TargetDown`** resolved yesterday after PR #208 merged.\n"+
			"- Nodes are at 41–63% memory and 18–35% CPU; nothing is pending or evicted.\n\n"+
			"No other alerts fired in the last 24 hours."),
		ui(3, ago(43*time.Minute), "/incidents/23", "Are #22 and #23 the same problem? They resolved at the same minute.", store.ChatAnswered),
		agent(3, 38, ago(41*time.Minute), "Yes, related but not duplicates, so I **linked** them rather than merging:\n\n"+
			"- #22 is the `media/library` volume filling up.\n"+
			"- #23 is node-b's `/var` filling up, and that volume is a hostPath on node-b's `/var`.\n\n"+
			"Both cleared at 14:02 when the transcode cache was pruned. Linked #23 ↔ #22; the reason is in each incident's Activity."),
		ui(1, ago(50*time.Minute), "/incidents/25", "Why 3Gi and not 2Gi? The other workers are fine at 1Gi.", store.ChatAnswered),
		agent(1, 31, ago(48*time.Minute), "The ml worker peaked at **2.71Gi** in the last 7 days, during the nightly face-detection job "+
			"(`max_over_time(container_memory_working_set_bytes[5m])`):\n\n| window | peak |\n|---|---|\n| last 24 hours | 2.68Gi |\n| last 7 days | 2.71Gi |\n\n"+
			"2Gi would OOM again at the next run; 3Gi leaves about 10% headroom. The other workers never load the model, which is why 1Gi is enough for them."),
		{ThreadID: 1, PRID: 1, Source: store.SourceGitHub, Author: "owner", Body: "Can we cap it at 2.75Gi instead? node-b is tight on memory.",
			Status: store.ChatAnswered, Path: "apps/photos/ml-deployment.yaml", Line: ptr(48),
			HTMLURL: "https://github.com/example/homelab/pull/212#discussion_r1", CreatedAt: ago(30 * time.Minute)},
		agent(1, 33, ago(27*time.Minute), "Done: the limit is now `2816Mi` (2.75Gi) and the request stays at `2Gi`. That is about 4% over the 7-day peak, "+
			"so a heavier night could still OOM; I said so in the PR description.\n\nPushed `3f9c2ab`: photos: cap the ml worker at 2.75Gi\n\n"+
			"Retitled the PR: photos: give the ml worker 2.75Gi"),
		ui(1, ago(5*time.Minute), "/incidents/26", "Does this explain #26 too?", store.ChatAnswered),
		agent(1, 35, ago(4*time.Minute), "Yes. #26 (`KubePodNotReady`) fired 40 seconds after the second OOM kill: the pod was restarting, so it was not ready. "+
			"I merged it into this incident when it opened (run #29); nothing else to do for it."),
		ui(4, ago(41*time.Second), "/", "Why did the photos pods restart overnight?", store.ChatQueued),
		ui(5, ago(24*time.Hour), "/incidents/24", "Will this happen again on the next maintenance?", store.ChatAnswered),
		agent(5, 21, ago(24*time.Hour-time.Minute), "Not for node-exporter: the toleration in #208 covers the `maintenance` taint. "+
			"Other DaemonSets without it would still leave that node, though; `alloy` is the only other one, and it already tolerates everything."),
		ui(5, ago(3*time.Minute), "/incidents/24", "And the alert rule itself, should it wait longer?", store.ChatCapped),
	}
	for i := range f.messages {
		f.messages[i].ID = int64(i + 1)
	}
	for i := range f.threads {
		f.touch(&f.threads[i])
	}
	f.actions = []store.Action{
		{ID: 1, Actor: "run:29", RunID: ptr(int64(29)), IncidentID: 26, TargetIncidentID: ptr(int64(25)), Tool: "merge_incident",
			Status: store.ActionSucceeded, Result: "merged #26 into #25", CreatedAt: ago(170 * time.Minute)},
		{ID: 2, Actor: "run:29", RunID: ptr(int64(29)), IncidentID: 25, Tool: "propose_pr",
			Status: store.ActionSucceeded, Result: "opened PR #212", CreatedAt: ago(169 * time.Minute)},
		{ID: 3, Actor: "run:33", RunID: ptr(int64(33)), IncidentID: 25, Tool: "update_pr",
			Status: store.ActionSucceeded, Result: "Pushed 3f9c2ab", CreatedAt: ago(28 * time.Minute)},
	}
	f.addActions(now)
	return f
}

// script turns steps into run events spaced every gap from start.
func script(start time.Time, gap time.Duration, payloads ...json.RawMessage) []step {
	out := make([]step, len(payloads))
	for i, p := range payloads {
		at := time.Duration(i) * gap
		out[i] = step{at: at, event: store.RunEvent{Seq: i + 1, Type: "assistant", Payload: p, ReceivedAt: start.Add(at)}}
	}
	return out
}

func text(s string) json.RawMessage {
	b, _ := json.Marshal(map[string]any{"message": map[string]any{"content": []any{map[string]any{"type": "text", "text": s}}}})
	return b
}

func result(s string) json.RawMessage {
	b, _ := json.Marshal(map[string]any{"message": map[string]any{"content": []any{map[string]any{"type": "tool_result", "content": s}}}})
	return b
}

func tool(name string, input map[string]any) json.RawMessage {
	b, _ := json.Marshal(map[string]any{"message": map[string]any{"content": []any{map[string]any{"type": "tool_use", "name": name, "input": input}}}})
	return b
}

// touch sets a thread's activity time from its newest message.
func (f *fixtures) touch(t *store.Thread) {
	t.UpdatedAt, t.Messages = t.CreatedAt, 0
	for _, m := range f.messages {
		if m.ThreadID == t.ID {
			t.Messages++
			if m.CreatedAt.After(t.UpdatedAt) {
				t.UpdatedAt = m.CreatedAt
			}
		}
	}
}

// advance finishes scripted turns whose time has come, recording their
// replies. Callers hold mu.
func (f *fixtures) advance() {
	now := time.Now()
	for i := range f.runs {
		r := &f.runs[i]
		tn := f.turns[r.ID]
		if tn == nil || tn.done == 0 || r.FinishedAt != nil || r.StartedAt == nil || now.Sub(*r.StartedAt) < tn.done {
			continue
		}
		r.Status, r.FinishedAt, r.Report = store.RunSucceeded, ptr(now), tn.reply
		for j := range f.messages {
			if f.messages[j].ThreadID == *r.ThreadID && f.messages[j].Status == store.ChatQueued {
				f.messages[j].Status = store.ChatAnswered
			}
		}
		f.messages = append(f.messages, store.ChatMessage{ID: int64(len(f.messages) + 1), ThreadID: *r.ThreadID, RunID: ptr(r.ID),
			Source: store.SourceAgent, Author: "agent", Body: tn.reply, Status: store.ChatReply, CreatedAt: now})
		f.touch(f.thread(*r.ThreadID))
	}
}

func (f *fixtures) thread(id int64) *store.Thread {
	for i := range f.threads {
		if f.threads[i].ID == id {
			return &f.threads[i]
		}
	}
	return nil
}

func (f *fixtures) incident(id int64) (store.Incident, bool) {
	for _, in := range f.incidents {
		if in.ID == id {
			return in, true
		}
	}
	return store.Incident{}, false
}

// ---- controller.Store, the parts the UI reads ----

func (f *fixtures) Ping(context.Context) error { return nil }

func (f *fixtures) ListIncidents(context.Context, int) ([]store.Incident, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.incidents), nil
}

func (f *fixtures) GetIncident(_ context.Context, id int64) (store.Incident, []store.IncidentAlert, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	in, ok := f.incident(id)
	if !ok {
		return store.Incident{}, nil, store.ErrNotFound
	}
	return in, f.alerts[id], nil
}

func (f *fixtures) IncidentChildren(_ context.Context, id int64) ([]store.Incident, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []store.Incident
	for _, in := range f.incidents {
		if in.ParentID != nil && *in.ParentID == id {
			out = append(out, in)
		}
	}
	return out, nil
}

func (f *fixtures) IncidentLinks(_ context.Context, id int64) ([]store.Link, error) {
	return f.links[id], nil
}

func (f *fixtures) IncidentActions(_ context.Context, id int64) ([]store.Action, error) {
	var out []store.Action
	for _, a := range f.actions {
		if a.IncidentID == id || (a.TargetIncidentID != nil && *a.TargetIncidentID == id) {
			out = append(out, a)
		}
	}
	return out, nil
}

func (f *fixtures) ListRuns(_ context.Context, incidentID int64) ([]store.Run, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []store.Run
	for _, r := range f.runs {
		if r.IncidentID == incidentID {
			out = append(out, r)
		}
	}
	slices.SortFunc(out, func(a, b store.Run) int { return cmp.Compare(b.ID, a.ID) })
	return out, nil
}

func (f *fixtures) GetRun(_ context.Context, id int64) (store.Run, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.advance()
	for _, r := range f.runs {
		if r.ID == id {
			return r, nil
		}
	}
	return store.Run{}, store.ErrNotFound
}

func (f *fixtures) RunIncidents(_ context.Context, id int64) ([]int64, error) {
	r, err := f.GetRun(context.Background(), id)
	if err != nil || r.IncidentID == 0 {
		return nil, err
	}
	return []int64{r.IncidentID}, nil
}

func (f *fixtures) RunEvents(ctx context.Context, id int64) ([]store.RunEvent, error) {
	return f.RunEventsAfter(ctx, id, 0)
}

func (f *fixtures) RunEventsAfter(_ context.Context, id int64, seq int) ([]store.RunEvent, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	tn := f.turns[id]
	if tn == nil {
		return nil, nil
	}
	var started time.Time
	for _, r := range f.runs {
		if r.ID == id && r.StartedAt != nil {
			started = *r.StartedAt
		}
	}
	var out []store.RunEvent
	for _, s := range tn.steps {
		if s.event.Seq > seq && time.Since(started) >= s.at {
			out = append(out, s.event)
		}
	}
	return out, nil
}

func (f *fixtures) PullRequestForIncident(_ context.Context, id int64) (store.PullRequest, error) {
	for _, p := range f.prs {
		if p.IncidentID == id {
			return p, nil
		}
	}
	return store.PullRequest{}, store.ErrNotFound
}

func (f *fixtures) OpenPullRequests(context.Context) ([]store.PullRequest, error) {
	var out []store.PullRequest
	for _, p := range f.prs {
		if p.State == store.PROpen {
			out = append(out, p)
		}
	}
	return out, nil
}

func (f *fixtures) GetThread(_ context.Context, id int64) (store.Thread, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.advance()
	if t := f.thread(id); t != nil {
		return *t, nil
	}
	return store.Thread{}, store.ErrNotFound
}

func (f *fixtures) IncidentThread(_ context.Context, incidentID int64, create bool) (store.Thread, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, t := range f.threads {
		if t.IncidentID != nil && *t.IncidentID == incidentID {
			return t, true, nil
		}
	}
	if !create {
		return store.Thread{}, false, store.ErrNotFound
	}
	in, _ := f.incident(incidentID)
	t := store.Thread{ID: int64(len(f.threads) + 1), Kind: store.ThreadIncident, IncidentID: ptr(incidentID),
		AlertName: in.AlertName, Namespace: in.Namespace, CreatedAt: time.Now(), UpdatedAt: time.Now()}
	f.threads = append(f.threads, t)
	return t, false, nil
}

func (f *fixtures) CreateGeneralThread(context.Context) (store.Thread, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	t := store.Thread{ID: int64(len(f.threads) + 1), Kind: store.ThreadGeneral, CreatedAt: time.Now(), UpdatedAt: time.Now()}
	f.threads = append(f.threads, t)
	return t, nil
}

func (f *fixtures) RecentThreads(_ context.Context, limit int) ([]store.Thread, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.advance()
	var out []store.Thread
	for _, t := range f.threads {
		if t.Kind == store.ThreadGeneral || t.Messages > 0 {
			out = append(out, t)
		}
	}
	slices.SortFunc(out, func(a, b store.Thread) int { return b.UpdatedAt.Compare(a.UpdatedAt) })
	return out[:min(limit, len(out))], nil
}

func (f *fixtures) ChatMessages(_ context.Context, threadID int64) ([]store.ChatMessage, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.advance()
	var out []store.ChatMessage
	for _, m := range f.messages {
		if m.ThreadID == threadID {
			out = append(out, m)
		}
	}
	return out, nil
}

func (f *fixtures) ActiveChatRun(_ context.Context, threadID int64) (store.Run, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.advance()
	for _, r := range f.runs {
		if r.ThreadID != nil && *r.ThreadID == threadID && r.FinishedAt == nil {
			return r, true, nil
		}
	}
	return store.Run{}, false, nil
}

// EnqueueChat records the message and plays a short scripted turn, so the
// whole loop (send, watch the steps, read the reply) can be tried.
func (f *fixtures) EnqueueChat(_ context.Context, m store.ChatMessage, _ store.ChatLimits) (store.ChatEnqueue, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.advance()
	t := f.thread(m.ThreadID)
	if t == nil {
		return store.ChatEnqueue{}, store.ErrNotFound
	}
	now := time.Now()
	m.ID, m.Status, m.CreatedAt = int64(len(f.messages)+1), store.ChatQueued, now
	f.messages = append(f.messages, m)
	if t.Kind == store.ThreadGeneral && t.Title == "" {
		t.Title = m.Body[:min(80, len(m.Body))]
	}
	f.touch(t)
	for _, r := range f.runs {
		if r.ThreadID != nil && *r.ThreadID == t.ID && r.FinishedAt == nil {
			return store.ChatEnqueue{MessageID: m.ID, RunID: r.ID}, nil
		}
	}
	var id int64
	for _, r := range f.runs {
		id = max(id, r.ID+1)
	}
	run := store.Run{ID: id, ThreadID: ptr(t.ID), Kind: store.KindChat, Status: store.RunRunning, CreatedAt: now,
		StartedAt: ptr(now.Add(time.Second)), Model: "claude-opus-5-5"}
	if t.IncidentID != nil {
		run.IncidentID = *t.IncidentID
	}
	f.runs = append(f.runs, run)
	f.turns[id] = &turn{
		steps: script(now.Add(time.Second), 1500*time.Millisecond,
			text("Let me check the cluster first."),
			tool("mcp__kubernetes__pods_list", map[string]any{"namespace": cmp.Or(t.Namespace, "all")}),
			tool("mcp__controller__list_incidents", map[string]any{"since_hours": 24}),
		),
		done: 6 * time.Second,
		reply: "This is the **preview server**: no agent runs here, so every turn ends with this reply.\n\n" +
			"In the cluster, the agent would answer from the cluster's state and its incidents, and could merge or link incidents (or change the PR) when you ask.",
	}
	return store.ChatEnqueue{MessageID: m.ID, RunID: id, NewRun: true}, nil
}
