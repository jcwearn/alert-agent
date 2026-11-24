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
	ProposePR bool     `json:"propose_pr"`
	Incident  Incident `json:"incident"`
	Alerts    []Alert  `json:"alerts"`
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
