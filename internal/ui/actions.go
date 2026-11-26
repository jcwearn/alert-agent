package ui

import (
	"context"
	"strconv"
	"time"

	"github.com/a-h/templ"

	"github.com/jcwearn/alert-agent/internal/store"
)

// ActionState is what every page needs to know about Phase 5: whether
// cluster actions are on, and whether the owner has paused them. It rides
// in the request context so the layout can show the pause banner on every
// page without each page passing it along.
type ActionState struct {
	Enabled bool
	Paused  bool
	At      time.Time
	// Path is the page being rendered, for the switch to return to.
	Path string
}

type actionStateKey struct{}

// WithActionState returns ctx carrying st for the layout.
func WithActionState(ctx context.Context, st ActionState) context.Context {
	return context.WithValue(ctx, actionStateKey{}, st)
}

func actionState(ctx context.Context) ActionState {
	st, _ := ctx.Value(actionStateKey{}).(ActionState)
	return st
}

func approvalURL(id int64, verb string) templ.SafeURL {
	return templ.SafeURL("/approvals/" + strconv.FormatInt(id, 10) + "/" + verb)
}

// ApprovalWhen is the line under an approval: when it expires, or who
// decided it and when.
func ApprovalWhen(ap store.Approval, now time.Time) string {
	switch {
	case ap.Status == store.ApprovalPending:
		left := ap.ExpiresAt.Sub(now).Round(time.Minute)
		if left <= 0 {
			return "expiring now"
		}
		return "expires in " + shortDuration(left)
	case ap.DecidedAt == nil:
		return ap.Status
	case ap.Status == store.ApprovalExpired:
		return "expired " + ago(*ap.DecidedAt, now)
	case ap.Status == store.ApprovalSuperseded:
		return "replaced by a newer request " + ago(*ap.DecidedAt, now)
	default:
		return ap.Status + " " + ago(*ap.DecidedAt, now) + " by " + deciderLabel(ap.DecidedBy)
	}
}

// shortDuration renders a positive duration as "1h 30m" or "45m".
func shortDuration(d time.Duration) string {
	h, m := int(d.Hours()), int(d.Minutes())%60
	switch {
	case h == 0:
		return strconv.Itoa(m) + "m"
	case m == 0:
		return strconv.Itoa(h) + "h"
	default:
		return strconv.Itoa(h) + "h " + strconv.Itoa(m) + "m"
	}
}

func deciderLabel(by string) string {
	if by == store.ActorUI {
		return "you (UI)"
	}
	return mergedByLabel(by)
}

// modeLabel names how a cluster action came to run.
func modeLabel(mode string) string {
	if mode == store.ModeApproved {
		return "approved"
	}
	return mode
}
