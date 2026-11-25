package ui

import (
	"encoding/json"
	"strconv"
	"strings"
	"time"

	"github.com/a-h/templ"

	"github.com/jcwearn/alert-agent/internal/store"
)

// IncidentPage is everything the incident page shows.
type IncidentPage struct {
	Incident store.Incident
	Alerts   []store.IncidentAlert
	Runs     []store.Run
	PR       *store.PullRequest
	// Parent is the incident this one is merged into.
	Parent     *store.Incident
	Children   []store.Incident
	Links      []store.Link
	Actions    []store.Action
	Candidates []store.Incident
	// Notice and Problem report the outcome of the last form post.
	Notice  string
	Problem string
	// Chat is the conversation on the PR; ChatRun the turn waiting or
	// running, if any; ChatEnabled whether a message can be sent (an open PR).
	Chat        []store.ChatMessage
	ChatRun     *store.Run
	ChatEnabled bool
}

// IncidentRow is one row of the home page: an incident and the incidents
// merged into it.
type IncidentRow struct {
	store.Incident
	Children []store.Incident
	// Status rolls up the family: firing while any member fires.
	Status string
}

// GroupIncidents folds merged incidents under their parent, keeping the
// list's order for the parents. A child whose parent is not in the list
// (it opened long before) is shown on its own.
func GroupIncidents(list []store.Incident) []IncidentRow {
	present := map[int64]bool{}
	for _, in := range list {
		present[in.ID] = true
	}
	children := map[int64][]store.Incident{}
	for _, in := range list {
		if in.ParentID != nil && present[*in.ParentID] {
			children[*in.ParentID] = append(children[*in.ParentID], in)
		}
	}
	var rows []IncidentRow
	for _, in := range list {
		if in.ParentID != nil && present[*in.ParentID] {
			continue
		}
		row := IncidentRow{Incident: in, Children: children[in.ID], Status: in.AlertStatus}
		for _, c := range row.Children {
			if c.AlertStatus == "firing" {
				row.Status = "firing"
			}
		}
		rows = append(rows, row)
	}
	return rows
}

// candidateWindow is how far back the merge and link pickers reach.
const candidateWindow = 24 * time.Hour

// MergeCandidates are the incidents the pickers offer: recent incidents that
// are not merged themselves, not this one, and not merged into this one.
func MergeCandidates(self store.Incident, recent []store.Incident, now time.Time) []store.Incident {
	var out []store.Incident
	for _, in := range recent {
		switch {
		case in.ID == self.ID, in.ParentID != nil, now.Sub(in.OpenedAt) > candidateWindow:
		default:
			out = append(out, in)
		}
	}
	return out
}

func idStr(id int64) string { return "#" + strconv.FormatInt(id, 10) }

// mergedByLabel says who merged an incident, for humans.
func mergedByLabel(by string) string {
	if run, ok := strings.CutPrefix(by, "run:"); ok {
		return "the agent (run #" + run + ")"
	}
	if by == store.ActorUI {
		return "a human in the UI"
	}
	return by
}

func mergedByRun(by string) (templ.SafeURL, bool) {
	if run, ok := strings.CutPrefix(by, "run:"); ok {
		return templ.SafeURL("/runs/" + run), true
	}
	return "", false
}

// actionSummary is the human part of an action's input: its reason, or a
// PR's title.
func actionSummary(a store.Action) string {
	var in map[string]any
	if json.Unmarshal(a.Input, &in) != nil {
		return ""
	}
	for _, k := range []string{"reason", "title"} {
		if v, ok := in[k].(string); ok && v != "" {
			if c, ok := in["confidence"].(string); ok && c != "" {
				return v + " (confidence " + c + ")"
			}
			return v
		}
	}
	return ""
}

func actorLabel(a store.Action) string {
	if a.Actor == store.ActorUI {
		return "human (UI)"
	}
	return "agent"
}

func candidateLabel(in store.Incident) string {
	s := idStr(in.ID) + " " + in.AlertName
	if in.Namespace != "" {
		s += " · " + in.Namespace
	}
	return s + " · " + in.AlertStatus
}
