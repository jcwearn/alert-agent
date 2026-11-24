package mcptools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"os/exec"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// PromQueryInput is an instant query.
type PromQueryInput struct {
	Query string `json:"query" jsonschema:"PromQL expression"`
	Time  string `json:"time,omitempty" jsonschema:"evaluation time, RFC3339 or relative like -30m; default now"`
}

// PromRangeInput is a range query.
type PromRangeInput struct {
	Query string `json:"query" jsonschema:"PromQL expression"`
	Start string `json:"start,omitempty" jsonschema:"range start, RFC3339 or relative like -6h; default -1h"`
	End   string `json:"end,omitempty" jsonschema:"range end, RFC3339 or relative; default now"`
	Step  string `json:"step,omitempty" jsonschema:"resolution, e.g. 1m; default chosen for about 120 points"`
}

// LokiQueryInput is a log query.
type LokiQueryInput struct {
	Query string `json:"query" jsonschema:"LogQL log query, e.g. {namespace=\"ebooks\"} |= \"error\""`
	Since string `json:"since,omitempty" jsonschema:"how far back, e.g. 30m or 6h; default 1h, max 168h"`
	Limit int    `json:"limit,omitempty" jsonschema:"maximum lines (default 200, max 1000)"`
}

const (
	maxSeries       = 30
	pointsPerSeries = 30
)

// parseTime accepts RFC3339 or a negative duration relative to now.
func (d *Deps) parseTime(s string, def time.Time) (time.Time, error) {
	s = strings.TrimSpace(s)
	if s == "" || s == "now" {
		return def, nil
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t, nil
	}
	dur, err := time.ParseDuration(s)
	if err != nil {
		return time.Time{}, fmt.Errorf("time %q is neither RFC3339 nor a duration like -1h", s)
	}
	if dur > 0 {
		dur = -dur
	}
	return d.Now().Add(dur), nil
}

func (d *Deps) getJSON(ctx context.Context, u string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, http.NoBody)
	if err != nil {
		return err
	}
	resp, err := d.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%s: %s", resp.Status, clip(string(body), 500))
	}
	return json.Unmarshal(body, out)
}

type promResponse struct {
	Status string `json:"status"`
	Error  string `json:"error"`
	Data   struct {
		ResultType string            `json:"resultType"`
		Result     []json.RawMessage `json:"result"`
	} `json:"data"`
}

type promSample struct {
	Metric map[string]string `json:"metric"`
	Value  [2]any            `json:"value"`
	Values [][2]any          `json:"values"`
}

func (d *Deps) promQuery(ctx context.Context, _ *mcp.CallToolRequest, in PromQueryInput) (*mcp.CallToolResult, any, error) {
	at, err := d.parseTime(in.Time, d.Now())
	if err != nil {
		return nil, nil, err
	}
	q := url.Values{"query": {in.Query}, "time": {strconv.FormatInt(at.Unix(), 10)}}
	var resp promResponse
	if err := d.getJSON(ctx, d.PrometheusURL+"/api/v1/query?"+q.Encode(), &resp); err != nil {
		return nil, nil, err
	}
	if resp.Status != "success" {
		return nil, nil, errors.New(resp.Error)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%s, %d series\n", resp.Data.ResultType, len(resp.Data.Result))
	for i, raw := range resp.Data.Result {
		if i == maxSeries*4 {
			fmt.Fprintf(&b, "… %d more series\n", len(resp.Data.Result)-i)
			break
		}
		var s promSample
		if err := json.Unmarshal(raw, &s); err != nil {
			b.Write(raw)
			b.WriteString("\n")
			continue
		}
		fmt.Fprintf(&b, "%s => %v\n", labels(s.Metric), s.Value[1])
	}
	return text(b.String()), nil, nil
}

func (d *Deps) promQueryRange(ctx context.Context, _ *mcp.CallToolRequest, in PromRangeInput) (*mcp.CallToolResult, any, error) {
	end, err := d.parseTime(in.End, d.Now())
	if err != nil {
		return nil, nil, err
	}
	start, err := d.parseTime(in.Start, end.Add(-time.Hour))
	if err != nil {
		return nil, nil, err
	}
	if !start.Before(end) {
		return nil, nil, errors.New("start must be before end")
	}
	step := in.Step
	if step == "" {
		s := end.Sub(start) / 120
		if s < 15*time.Second {
			s = 15 * time.Second
		}
		step = strconv.Itoa(int(s.Seconds())) + "s"
	}
	q := url.Values{"query": {in.Query}, "start": {strconv.FormatInt(start.Unix(), 10)},
		"end": {strconv.FormatInt(end.Unix(), 10)}, "step": {step}}
	var resp promResponse
	if err := d.getJSON(ctx, d.PrometheusURL+"/api/v1/query_range?"+q.Encode(), &resp); err != nil {
		return nil, nil, err
	}
	if resp.Status != "success" {
		return nil, nil, errors.New(resp.Error)
	}

	var b strings.Builder
	fmt.Fprintf(&b, "%d series, %s to %s, step %s\n", len(resp.Data.Result),
		start.UTC().Format(time.RFC3339), end.UTC().Format(time.RFC3339), step)
	for i, raw := range resp.Data.Result {
		if i == maxSeries {
			fmt.Fprintf(&b, "… %d more series; narrow the query\n", len(resp.Data.Result)-i)
			break
		}
		var s promSample
		if err := json.Unmarshal(raw, &s); err != nil || len(s.Values) == 0 {
			continue
		}
		lo, hi := math.Inf(1), math.Inf(-1)
		for _, p := range s.Values {
			v := sampleValue(p)
			lo, hi = math.Min(lo, v), math.Max(hi, v)
		}
		last := s.Values[len(s.Values)-1]
		fmt.Fprintf(&b, "\n%s\n  points %d, min %g, max %g, last %v at %s\n  ", labels(s.Metric), len(s.Values), lo, hi,
			last[1], sampleTime(last).UTC().Format("15:04"))
		every := int(math.Ceil(float64(len(s.Values)) / pointsPerSeries))
		for j := 0; j < len(s.Values); j += every {
			fmt.Fprintf(&b, "%s=%v ", sampleTime(s.Values[j]).UTC().Format("15:04"), s.Values[j][1])
		}
		b.WriteString("\n")
	}
	return text(b.String()), nil, nil
}

func sampleValue(p [2]any) float64 {
	s, _ := p[1].(string)
	v, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return math.NaN()
	}
	return v
}

func sampleTime(p [2]any) time.Time {
	f, _ := p[0].(float64)
	return time.Unix(int64(f), 0)
}

func labels(m map[string]string) string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, k+"="+strconv.Quote(m[k]))
	}
	return "{" + strings.Join(parts, ", ") + "}"
}

type lokiResponse struct {
	Status string `json:"status"`
	Data   struct {
		Result []struct {
			Stream map[string]string `json:"stream"`
			Values [][2]string       `json:"values"`
		} `json:"result"`
	} `json:"data"`
}

func (d *Deps) lokiQuery(ctx context.Context, _ *mcp.CallToolRequest, in LokiQueryInput) (*mcp.CallToolResult, any, error) {
	since := time.Hour
	if in.Since != "" {
		dur, err := time.ParseDuration(strings.TrimPrefix(in.Since, "-"))
		if err != nil {
			return nil, nil, fmt.Errorf("since %q: %w", in.Since, err)
		}
		since = min(dur, 168*time.Hour)
	}
	limit := clamp(in.Limit, 200, 1000)
	end := d.Now()
	q := url.Values{
		"query": {in.Query}, "limit": {strconv.Itoa(limit)}, "direction": {"backward"},
		"start": {strconv.FormatInt(end.Add(-since).UnixNano(), 10)}, "end": {strconv.FormatInt(end.UnixNano(), 10)},
	}
	var resp lokiResponse
	if err := d.getJSON(ctx, d.LokiURL+"/loki/api/v1/query_range?"+q.Encode(), &resp); err != nil {
		return nil, nil, err
	}

	type line struct {
		ts     int64
		labels string
		text   string
	}
	var lines []line
	for _, st := range resp.Data.Result {
		lbl := labels(pick(st.Stream, "namespace", "pod", "container", "unit", "node", "app"))
		for _, v := range st.Values {
			ts, _ := strconv.ParseInt(v[0], 10, 64)
			lines = append(lines, line{ts, lbl, v[1]})
		}
	}
	sort.Slice(lines, func(i, j int) bool { return lines[i].ts > lines[j].ts })
	if len(lines) > limit {
		lines = lines[:limit]
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%d lines, newest first, last %s\n", len(lines), since)
	for _, l := range lines {
		fmt.Fprintf(&b, "%s %s %s\n", time.Unix(0, l.ts).UTC().Format(time.RFC3339), l.labels, strings.TrimRight(l.text, "\n"))
	}
	return text(b.String()), nil, nil
}

// pick keeps the stream labels that identify a source, so each line is not
// prefixed with a dozen labels.
func pick(m map[string]string, keys ...string) map[string]string {
	out := map[string]string{}
	for _, k := range keys {
		if v, ok := m[k]; ok {
			out[k] = v
		}
	}
	return out
}

// GitLogInput lists commits.
type GitLogInput struct {
	Path     string `json:"path,omitempty" jsonschema:"only commits touching this path, relative to the repo root"`
	MaxCount int    `json:"max_count,omitempty" jsonschema:"number of commits (default 20, max 100)"`
}

// GitShowInput shows a commit.
type GitShowInput struct {
	Rev  string `json:"rev" jsonschema:"commit hash or ref, e.g. HEAD~2 or a short hash from git_log"`
	Path string `json:"path,omitempty" jsonschema:"limit the patch to this path"`
}

var safeRev = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._/~^@{}-]*$`)

func safePath(p string) error {
	if p == "" {
		return nil
	}
	if strings.HasPrefix(p, "-") || strings.HasPrefix(p, "/") || strings.Contains(p, "..") {
		return fmt.Errorf("path %q must be relative to the repository root", p)
	}
	return nil
}

func (d *Deps) git(ctx context.Context, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, d.GitBin, append([]string{"-C", d.RepoDir, "--no-pager"}, args...)...)
	cmd.Env = []string{"PATH=/usr/local/bin:/usr/bin:/bin", "GIT_TERMINAL_PROMPT=0", "HOME=/tmp"}
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("git: %w: %s", err, clip(string(out), 500))
	}
	return string(out), nil
}

func (d *Deps) gitLog(ctx context.Context, _ *mcp.CallToolRequest, in GitLogInput) (*mcp.CallToolResult, any, error) {
	if err := safePath(in.Path); err != nil {
		return nil, nil, err
	}
	args := []string{"log", "--max-count=" + strconv.Itoa(clamp(in.MaxCount, 20, 100)), "--date=iso-strict", "--format=%h %ad %s"}
	if in.Path != "" {
		args = append(args, "--", in.Path)
	}
	out, err := d.git(ctx, args...)
	if err != nil {
		return nil, nil, err
	}
	return text(out), nil, nil
}

func (d *Deps) gitShow(ctx context.Context, _ *mcp.CallToolRequest, in GitShowInput) (*mcp.CallToolResult, any, error) {
	if !safeRev.MatchString(in.Rev) {
		return nil, nil, fmt.Errorf("rev %q is not a plain revision", in.Rev)
	}
	if err := safePath(in.Path); err != nil {
		return nil, nil, err
	}
	args := []string{"show", "--stat", "--patch", "--date=iso-strict", in.Rev, "--"}
	if in.Path != "" {
		args = append(args, in.Path)
	}
	out, err := d.git(ctx, args...)
	if err != nil {
		return nil, nil, err
	}
	return text(out), nil, nil
}

// GitDiffInput takes no arguments.
type GitDiffInput struct{}

// gitDiff is the agent's own edits as a patch, in the exact form propose_pr
// takes. New files are marked intent-to-add first so they appear too; that
// touches only the checkout's index.
func (d *Deps) gitDiff(ctx context.Context, _ *mcp.CallToolRequest, _ GitDiffInput) (*mcp.CallToolResult, any, error) {
	if _, err := d.git(ctx, "add", "--all", "--intent-to-add"); err != nil {
		return nil, nil, err
	}
	out, err := d.git(ctx, "diff", "--full-index", "--no-color", "--no-ext-diff")
	if err != nil {
		return nil, nil, err
	}
	if strings.TrimSpace(out) == "" {
		return text("(no changes: edit files in the checkout first)"), nil, nil
	}
	if len(out) > maxOutput {
		return nil, nil, fmt.Errorf("the diff is %d bytes, more than the %d a tool result can carry intact; make a smaller change", len(out), maxOutput)
	}
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: out}}}, nil, nil
}
