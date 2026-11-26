package ui

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/jcwearn/alert-agent/internal/store"
)

// The home page names every incident by number: parents, merged children
// and the incident behind a PR awaiting review.
func TestIncidentListShowsNumbers(t *testing.T) {
	now := time.Now()
	parent := int64(41)
	list := []store.Incident{
		{ID: 41, AlertName: "KubePodCrashLooping", AlertStatus: "firing", OpenedAt: now},
		{ID: 42, AlertName: "KubePodNotReady", AlertStatus: "resolved", OpenedAt: now, ParentID: &parent},
	}
	prs := []store.PullRequest{{Number: 212, IncidentID: 25, AlertName: "KubeContainerOOMKilled", Checks: "success", CreatedAt: now}}
	var b strings.Builder
	if err := IncidentList(IncidentListView{Rows: GroupIncidents(list)}, prs, nil, now).Render(context.Background(), &b); err != nil {
		t.Fatal(err)
	}
	page := b.String()
	for _, want := range []string{
		`<span class="num">#41</span>`,
		`<span class="num">#42</span>`,
		`<a href="/incidents/25">#25<span class="hide-narrow"> KubeContainerOOMKilled</span>`,
	} {
		if !strings.Contains(page, want) {
			t.Errorf("home page lacks %s", want)
		}
	}
}

// The pager keeps the filters, and the presets swap the range but keep the
// rest.
func TestIncidentListPager(t *testing.T) {
	q := ListQuery{From: "2026-11-01", Status: "firing", Namespace: "books"}
	v := IncidentListView{
		Rows:  GroupIncidents([]store.Incident{{ID: 7, AlertName: "A", AlertStatus: "firing"}}),
		Query: q, Newer: "100-8", Older: "50-6", Paged: true,
	}
	var b strings.Builder
	if err := IncidentList(v, nil, nil, time.Now()).Render(context.Background(), &b); err != nil {
		t.Fatal(err)
	}
	page := b.String()
	for _, want := range []string{
		`href="/?after=100-8&amp;from=2026-11-01&amp;ns=books&amp;status=firing" rel="prev"`,
		`href="/?before=50-6&amp;from=2026-11-01&amp;ns=books&amp;status=firing" rel="next"`,
		`href="/?from=2026-11-01&amp;ns=books&amp;status=firing">« Newest`,
		`<a class="preset" href="/?ns=books&amp;range=7d&amp;status=firing">7 days</a>`,
		`<a class="preset" href="/?ns=books&amp;status=firing">Any time</a>`,
		`<input type="date" name="from" value="2026-11-01">`,
	} {
		if !strings.Contains(page, want) {
			t.Errorf("page lacks %s", want)
		}
	}
	if strings.Contains(page, `name="range"`) {
		t.Error("a hidden range field alongside a date")
	}
}
