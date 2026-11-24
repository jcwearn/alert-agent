// Package mcptools is the agent's read-only toolbox, served over MCP on
// stdio by `alert-agent mcp` inside the runner pod. It replaces Bash: the
// agent can look at the cluster, its metrics, its logs and the GitOps repo's
// history, and can run nothing.
//
// Every tool here is read-only by construction, and the identity behind the
// Kubernetes calls (the runner ServiceAccount) cannot write or read Secrets
// anyway.
package mcptools

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
)

// ServerName is the MCP server's name; claude exposes its tools as
// mcp__<ServerName>__<tool>.
const ServerName = "cluster"

// Deps are the backends the tools read from.
type Deps struct {
	Dynamic       dynamic.Interface
	Mapper        meta.RESTMapper
	Core          kubernetes.Interface
	PrometheusURL string
	LokiURL       string
	RepoDir       string
	GitBin        string
	HTTP          *http.Client
	Now           func() time.Time
}

// maxOutput caps one tool result. The agent can always narrow a query; an
// unbounded result only burns its context.
const maxOutput = 60_000

// NewServer registers every tool.
func NewServer(d *Deps, version string) *mcp.Server {
	if d.HTTP == nil {
		d.HTTP = &http.Client{Timeout: 30 * time.Second}
	}
	if d.Now == nil {
		d.Now = time.Now
	}
	if d.GitBin == "" {
		d.GitBin = "git"
	}
	s := mcp.NewServer(&mcp.Implementation{Name: ServerName, Version: version}, nil)
	ro := &mcp.ToolAnnotations{ReadOnlyHint: true}

	mcp.AddTool(s, &mcp.Tool{Name: "kube_get", Annotations: ro, Description: "Get or list Kubernetes objects of any resource type, including CRDs " +
		"(e.g. pods, deployments, nodes, kustomizations.kustomize.toolkit.fluxcd.io, helmreleases, clusters.postgresql.cnpg.io). " +
		"Lists show a one-line status per object; a single named object is returned as YAML. Secrets are not readable."}, d.kubeGet)
	mcp.AddTool(s, &mcp.Tool{Name: "kube_events", Annotations: ro, Description: "Recent Kubernetes events in a namespace, newest first, optionally for one object."}, d.kubeEvents)
	mcp.AddTool(s, &mcp.Tool{Name: "kube_logs", Annotations: ro, Description: "Logs of a pod's container. Use previous=true for the last crashed instance."}, d.kubeLogs)
	mcp.AddTool(s, &mcp.Tool{Name: "prometheus_query", Annotations: ro, Description: "Instant PromQL query against the cluster's Prometheus."}, d.promQuery)
	mcp.AddTool(s, &mcp.Tool{Name: "prometheus_query_range", Annotations: ro, Description: "PromQL range query; each series is summarised (min, max, last) with sampled points."}, d.promQueryRange)
	mcp.AddTool(s, &mcp.Tool{Name: "loki_query", Annotations: ro, Description: "LogQL query against the cluster's Loki (pod logs, node journals, Kubernetes events), newest lines first. " +
		`Example: {namespace="ebooks"} |= "error"`}, d.lokiQuery)
	mcp.AddTool(s, &mcp.Tool{Name: "git_log", Annotations: ro, Description: "Recent commits in the GitOps repository checkout, optionally limited to a path."}, d.gitLog)
	mcp.AddTool(s, &mcp.Tool{Name: "git_show", Annotations: ro, Description: "Show one commit of the GitOps repository: message, stat and patch, optionally limited to a path."}, d.gitShow)
	return s
}

// text wraps a tool's output, truncating it to maxOutput.
func text(s string) *mcp.CallToolResult {
	if len(s) > maxOutput {
		s = s[:maxOutput] + fmt.Sprintf("\n… truncated: %d more characters. Narrow the query.", len(s)-maxOutput)
	}
	if strings.TrimSpace(s) == "" {
		s = "(no output)"
	}
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: s}}}
}

// Run serves the tools on stdio until the client disconnects.
func Run(ctx context.Context, d *Deps, version string) error {
	return NewServer(d, version).Run(ctx, &mcp.StdioTransport{})
}
