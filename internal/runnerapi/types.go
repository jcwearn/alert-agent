// Package runnerapi is the contract between the controller and a runner Job:
// the runner fetches its incident, streams its transcript and reports a
// result, all authenticated by the run's bearer token.
package runnerapi

import (
	"encoding/json"
	"time"
)

// Paths, relative to the controller's runner API base URL.
const (
	PathContext = "/runner/v1/context"
	PathEvents  = "/runner/v1/events"
	PathFinish  = "/runner/v1/finish"
	// PathMCP serves the controller's MCP tools (propose_pr) over streamable
	// HTTP, authenticated by the same run token.
	PathMCP = "/runner/v1/mcp"
	// PathSession takes the run's gzipped claude session file.
	PathSession = "/runner/v1/session"
)

// Context is everything a runner needs to start.
type Context struct {
	RunID     int64  `json:"run_id"`
	SessionID string `json:"session_id"`
	Model     string `json:"model"`
	MaxTurns  int    `json:"max_turns"`
	UIURL     string `json:"ui_url"`
	// ProposePR says the controller serves propose_pr for this run: the
	// runner then adds the controller's MCP server and allows file edits.
	ProposePR bool `json:"propose_pr"`
	// Consolidate says the controller serves link_incident and
	// merge_incident for this run.
	Consolidate bool     `json:"consolidate"`
	Incident    Incident `json:"incident"`
	Alerts      []Alert  `json:"alerts"`
	// Batch is the run's other incidents: they opened in the same namespace
	// while the run was held, and this run decides what they are.
	Batch []BatchIncident `json:"batch,omitempty"`
	// Recent lists other incidents still firing or opened lately, so the
	// agent can recognise a problem that already has an owner.
	Recent []RecentIncident `json:"recent,omitempty"`
	// OpenPRs are the agent's PRs awaiting review, with the files they touch.
	OpenPRs []OpenPR `json:"open_prs,omitempty"`
}

// BatchIncident is another incident this run covers, with its alerts.
type BatchIncident struct {
	Incident Incident `json:"incident"`
	Alerts   []Alert  `json:"alerts"`
}

// RecentIncident is a brief of an incident outside this run.
type RecentIncident struct {
	ID          int64     `json:"id"`
	AlertName   string    `json:"alertname"`
	Namespace   string    `json:"namespace"`
	AlertStatus string    `json:"alert_status"`
	OpenedAt    time.Time `json:"opened_at"`
	// ParentID is set when the incident is merged into another.
	ParentID *int64 `json:"parent_id,omitempty"`
	// Objects are the identifying labels of its alerts (pod=..., deployment=...).
	Objects []string `json:"objects,omitempty"`
	// Summary is the summary of its latest diagnosis, if one finished.
	Summary  string `json:"summary,omitempty"`
	PRNumber *int   `json:"pr_number,omitempty"`
}

// OpenPR is an agent PR awaiting review.
type OpenPR struct {
	Number     int      `json:"number"`
	Title      string   `json:"title"`
	IncidentID int64    `json:"incident_id"`
	Files      []string `json:"files"`
}

// Incident is the alert group being triaged.
type Incident struct {
	ID          int64     `json:"id"`
	AlertName   string    `json:"alertname"`
	Namespace   string    `json:"namespace"`
	Severity    string    `json:"severity"`
	Summary     string    `json:"summary"`
	AlertStatus string    `json:"alert_status"`
	OpenedAt    time.Time `json:"opened_at"`
}

// Alert is one alert in the group.
type Alert struct {
	Status      string            `json:"status"`
	Labels      map[string]string `json:"labels"`
	Annotations map[string]string `json:"annotations"`
	StartsAt    time.Time         `json:"starts_at"`
}

// Event is one stream-json line from `claude -p`.
type Event struct {
	Seq     int             `json:"seq"`
	Type    string          `json:"type"`
	Payload json.RawMessage `json:"payload"`
}

// Finish is the runner's final report.
type Finish struct {
	Status     string          `json:"status"` // succeeded or failed
	Report     string          `json:"report"`
	Error      string          `json:"error,omitempty"`
	NumTurns   *int            `json:"num_turns,omitempty"`
	DurationMS *int64          `json:"duration_ms,omitempty"`
	Usage      json.RawMessage `json:"usage,omitempty"`
}
