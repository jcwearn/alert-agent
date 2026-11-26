package ui

import (
	"context"
	"strings"
	"testing"

	"github.com/jcwearn/alert-agent/internal/store"
)

func TestLatestReport(t *testing.T) {
	runs := []store.Run{
		{ID: 5, Kind: store.KindChat, Status: store.RunSucceeded, Report: "chat"},
		{ID: 4, Kind: store.KindDiagnose, Status: store.RunFailed},
		{ID: 3, Kind: store.KindDiagnose, Status: store.RunSucceeded, Report: "newest"},
		{ID: 2, Kind: store.KindDiagnose, Status: store.RunSucceeded, Report: "older"},
	}
	if r, ok := latestReport(runs); !ok || r.ID != 3 {
		t.Errorf("got %d %v", r.ID, ok)
	}
	if _, ok := latestReport(runs[:2]); ok {
		t.Error("found a report where there is none")
	}
}

func TestCamelParts(t *testing.T) {
	for in, want := range map[string]string{
		"KubePodCrashLooping":    "Kube|Pod|Crash|Looping",
		"KubeContainerOOMKilled": "Kube|Container|OOMKilled",
		"Watchdog":               "Watchdog",
		"":                       "",
		"Http5xxRate":            "Http5xx|Rate",
	} {
		if got := strings.Join(camelParts(in), "|"); got != want {
			t.Errorf("camelParts(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestAlertNameEscapes(t *testing.T) {
	var b strings.Builder
	if err := alertName("kubeBad<script>").Render(context.Background(), &b); err != nil {
		t.Fatal(err)
	}
	if got, want := b.String(), "kube<wbr>Bad&lt;script&gt;"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}
