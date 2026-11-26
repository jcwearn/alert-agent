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
	if err := IncidentList(GroupIncidents(list), prs, nil, now).Render(context.Background(), &b); err != nil {
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
