package ui

import (
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
