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
	// PathSession takes the run's gzipped claude session file (POST), and
	// gives a chat turn the session it resumes (GET).
	PathSession = "/runner/v1/session"
)

// Run kinds.
const (
	KindDiagnose = "diagnose"
	// KindChat resumes the PR run's session on the PR branch to answer the
	// reviewer.
	KindChat = "chat"
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
	Consolidate bool `json:"consolidate"`
	// Kind is diagnose, or chat for a reviewer's turn on a PR.
	Kind string `json:"kind"`
	// UpdatePR says the controller serves update_pr (chat turns).
	UpdatePR bool `json:"update_pr"`
	// IncidentTools says the controller serves list_incidents and
	// get_incident (chat turns).
	IncidentTools bool `json:"incident_tools"`
	// Thread is a chat turn's conversation.
	Thread *Thread `json:"thread,omitempty"`
	// Resume says the thread's session has a stored file to continue; a chat
	// turn without one starts the session.
	Resume bool `json:"resume"`
	// PR is the PR a chat turn is about.
	PR *PR `json:"pr,omitempty"`
	// Messages are the reviewer's messages a chat turn answers.
	Messages []ChatMessage `json:"messages,omitempty"`
	Incident Incident      `json:"incident"`
	Alerts   []Alert       `json:"alerts"`
	// Batch is the run's other incidents: they opened in the same namespace
	// while the run was held, and this run decides what they are.
	Batch []BatchIncident `json:"batch,omitempty"`
	// Recent lists other incidents still firing or opened lately, so the
	// agent can recognise a problem that already has an owner.
	Recent []RecentIncident `json:"recent,omitempty"`
	// OpenPRs are the agent's PRs awaiting review, with the files they touch.
	OpenPRs []OpenPR `json:"open_prs,omitempty"`

	// Actions says the controller serves the cluster actions (delete_pod,
	// rollout_restart, rerun_cronjob, flux_reconcile) and request_approval:
	// an alert run with Phase 5 on.
	Actions bool `json:"actions"`
	// RequestApproval says the controller serves request_approval alone: a
	// chat turn on an incident.
	RequestApproval bool `json:"request_approval"`
	// AutonomousNamespaces are where a workload action may run without a
	// human; elsewhere it needs request_approval.
	AutonomousNamespaces []string `json:"autonomous_namespaces,omitempty"`
	// ActionsPaused says the owner has paused autonomous actions.
	ActionsPaused bool `json:"actions_paused,omitempty"`
	// PriorActions are cluster actions within the cooldown in this
	// namespace or for this alert: a recurrence must escalate, not retry.
	PriorActions []PriorAction `json:"prior_actions,omitempty"`
	// Approvals are the requests made for this incident, with their outcome.
	Approvals []Approval `json:"approvals,omitempty"`
}

// PriorAction is a recent cluster action the agent or a human took.
type PriorAction struct {
	ID         int64     `json:"id"`
	IncidentID int64     `json:"incident_id"`
	AlertName  string    `json:"alertname"`
	Tool       string    `json:"tool"`
	Mode       string    `json:"mode"` // autonomous or approved
	Target     string    `json:"target"`
	Status     string    `json:"status"`
	Result     string    `json:"result"`
	At         time.Time `json:"at"`
}

// Approval is a request for a human to approve a cluster action.
type Approval struct {
	ID     int64     `json:"id"`
	Tool   string    `json:"tool"`
	Target string    `json:"target"`
	Object string    `json:"object"`
	Reason string    `json:"reason"`
	Status string    `json:"status"`
	Result string    `json:"result,omitempty"`
	At     time.Time `json:"at"`
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

// Thread kinds.
const (
	ThreadIncident = "incident"
	ThreadGeneral  = "general"
)

// Thread is the conversation a chat turn belongs to.
type Thread struct {
	ID    int64  `json:"id"`
	Kind  string `json:"kind"`
	Title string `json:"title"`
}

// PR is the agent PR a chat turn works on. The runner checks out Branch,
// which is at HeadSHA.
type PR struct {
	Number  int      `json:"number"`
	URL     string   `json:"url"`
	Title   string   `json:"title"`
	Branch  string   `json:"branch"`
	HeadSHA string   `json:"head_sha"`
	Files   []string `json:"files"`
	State   string   `json:"state"`
	Checks  string   `json:"checks"`
}

// ChatMessage is one reviewer message, from the UI or a GitHub comment.
type ChatMessage struct {
	Source string    `json:"source"` // ui or github
	Author string    `json:"author"`
	Body   string    `json:"body"`
	At     time.Time `json:"at"`
	// Kind is the GitHub comment kind (issue_comment, review_comment, review).
	Kind string `json:"kind,omitempty"`
	// A line comment's file, line and the diff it was made on.
	Path     string `json:"path,omitempty"`
	Line     *int   `json:"line,omitempty"`
	DiffHunk string `json:"diff_hunk,omitempty"`
	// Page describes the UI page the message was sent from, written by the
	// controller from the page's path ("incident #25: …"); empty if none.
	Page string `json:"page,omitempty"`
}
