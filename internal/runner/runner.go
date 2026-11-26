// Package runner is the Job side of a run: fetch the incident, check out the
// GitOps repo, run `claude -p` read-only, and stream everything it prints
// back to the controller.
package runner

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/jcwearn/alert-agent/internal/runnerapi"
)

// Config comes from the Job's environment.
type Config struct {
	ControllerURL string
	RunToken      string
	GitHubToken   string
	Repo          string // owner/name
	WorkDir       string
	// TmpDir holds files claude reads but the agent must not: the MCP config
	// carries the run token. It is outside WorkDir, where Read cannot reach.
	TmpDir    string
	ClaudeBin string
	GitBin    string
	SelfBin   string // this binary, started by claude as the MCP tool server
	Getenv    func(string) string
	HTTP      *http.Client
}

// ConfigFromEnv reads the variables the dispatcher sets on the Job.
func ConfigFromEnv() (Config, error) {
	self, err := os.Executable()
	if err != nil {
		return Config{}, err
	}
	c := Config{
		SelfBin:       self,
		ControllerURL: os.Getenv("CONTROLLER_URL"),
		RunToken:      os.Getenv("RUN_TOKEN"),
		GitHubToken:   os.Getenv("GITHUB_TOKEN"),
		Repo:          os.Getenv("REPO"),
		WorkDir:       "/work/repo",
		TmpDir:        os.TempDir(),
		ClaudeBin:     "claude",
		GitBin:        "git",
		Getenv:        os.Getenv,
		HTTP:          &http.Client{Timeout: 30 * time.Second},
	}
	if c.ControllerURL == "" || c.RunToken == "" || c.Repo == "" {
		return c, errors.New("CONTROLLER_URL, RUN_TOKEN and REPO must be set")
	}
	return c, nil
}

// Run carries out one run end to end. It always tries to report a result:
// a runner that dies silently leaves the dispatcher to time it out.
//
// It returns nil once a result -- success or failure -- has been reported.
// A failed run is the controller's to surface (UI, Slack); exiting non-zero
// would also fail the Job and page through KubeJobFailed and
// KubernetesPodNotHealthy for the same event. A non-nil error means the
// runner could not report at all, which is exactly when the Job should fail.
func Run(ctx context.Context, cfg Config) error {
	api := &client{base: strings.TrimRight(cfg.ControllerURL, "/"), token: cfg.RunToken, http: cfg.HTTP}

	rc, err := api.context(ctx)
	if err != nil {
		return fmt.Errorf("fetch context: %w", err)
	}
	slog.Info("run starting", "run", rc.RunID, "incident", rc.Incident.ID, "alert", rc.Incident.AlertName)

	branch, prompt := "main", userPrompt(rc)
	if rc.Kind == runnerapi.KindChat {
		if rc.Thread == nil {
			return api.fail(ctx, "chat turn without a thread")
		}
		// A thread with a stored session continues it; without its file there
		// is nothing to continue, so that is a failure, not a fresh start.
		if rc.Resume {
			if err := restoreSession(ctx, api, cfg.Getenv("HOME"), cfg.WorkDir, rc.SessionID); err != nil {
				return api.fail(ctx, "could not restore the conversation's session: "+err.Error())
			}
		}
		prompt = chatPrompt(rc)
		if rc.PR != nil && rc.PR.State == "open" {
			branch = rc.PR.Branch
		}
	}

	if err := clone(ctx, cfg, branch); err != nil {
		return api.fail(ctx, "checkout failed: "+err.Error())
	}

	settings, err := settingsJSON(rc.ProposePR, rc.Consolidate, rc.UpdatePR, rc.IncidentTools, rc.Actions, rc.RequestApproval)
	if err != nil {
		return api.fail(ctx, err.Error())
	}
	mcpConfig, err := mcpConfigJSON(cfg.SelfBin, cfg.WorkDir, cfg.Getenv, cfg.ControllerURL, cfg.RunToken,
		rc.ProposePR || rc.Consolidate || rc.UpdatePR || rc.IncidentTools || rc.Actions || rc.RequestApproval)
	if err != nil {
		return api.fail(ctx, err.Error())
	}
	mcpFile, err := writePrivate(cfg.TmpDir, "mcp-config-*.json", mcpConfig)
	if err != nil {
		return api.fail(ctx, "write mcp config: "+err.Error())
	}
	defer func() { _ = os.Remove(mcpFile) }()

	cmd := exec.CommandContext(ctx, cfg.ClaudeBin, claudeArgs(rc, settings, mcpFile, prompt)...)
	cmd.Dir = cfg.WorkDir
	cmd.Env = claudeEnv(cfg.Getenv)
	var stderr bytes.Buffer
	cmd.Stderr = &limitedWriter{w: &stderr, n: 64 << 10}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return api.fail(ctx, err.Error())
	}
	if err := cmd.Start(); err != nil {
		return api.fail(ctx, "start claude: "+err.Error())
	}

	result, streamErr := stream(ctx, stdout, api)
	waitErr := cmd.Wait()

	// The session file lets a later run resume this conversation. Losing it
	// costs only that, so a failure is logged and the run carries on.
	if err := uploadSession(ctx, api, cfg.Getenv("HOME"), rc.SessionID); err != nil {
		slog.Warn("session upload failed", "err", err)
	}

	fin := runnerapi.Finish{Status: "succeeded"}
	if result != nil {
		fin.Report = result.Result
		fin.NumTurns = result.NumTurns
		fin.DurationMS = result.DurationMS
		fin.Usage = result.Usage
		if result.IsError || result.Subtype != "success" {
			fin.Status = "failed"
			fin.Error = "claude ended with " + result.Subtype
			if result.IsError && result.Result != "" {
				fin.Error = "claude: " + result.Result
			}
			fin.Reason = failureReason(result)
		}
	}
	switch {
	case result == nil:
		fin.Status = "failed"
		fin.Error = "claude produced no result"
		if waitErr != nil {
			fin.Error += ": " + waitErr.Error()
		}
		if s := strings.TrimSpace(stderr.String()); s != "" {
			fin.Error += "\n" + s
		}
	case streamErr != nil:
		fin.Error = strings.TrimSpace(fin.Error + "\ntranscript upload incomplete: " + streamErr.Error())
	}

	if err := api.finish(ctx, fin); err != nil {
		return fmt.Errorf("report result: %w", err)
	}
	if fin.Status != "succeeded" {
		slog.Warn("run finished", "status", fin.Status, "error", fin.Error)
		return nil
	}
	slog.Info("run finished", "status", fin.Status)
	return nil
}

// writePrivate writes content to a new 0600 file in dir.
func writePrivate(dir, pattern, content string) (string, error) {
	f, err := os.CreateTemp(dir, pattern)
	if err != nil {
		return "", err
	}
	if _, err := f.WriteString(content); err != nil {
		_ = f.Close()
		_ = os.Remove(f.Name())
		return "", err
	}
	return f.Name(), f.Close()
}

// maxSession bounds the gzipped session upload; the controller enforces the
// same limit.
const maxSession = 32 << 20

// uploadSession sends claude's transcript file for this session, found
// under $HOME/.claude/projects/<cwd, sanitised>/<session-id>.jsonl.
func uploadSession(ctx context.Context, api *client, home, sessionID string) error {
	if home == "" || sessionID == "" {
		return errors.New("no HOME or session id")
	}
	matches, err := filepath.Glob(filepath.Join(home, ".claude", "projects", "*", sessionID+".jsonl"))
	if err != nil || len(matches) == 0 {
		return fmt.Errorf("session file for %s not found", sessionID)
	}
	raw, err := os.ReadFile(matches[0])
	if err != nil {
		return err
	}
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	if _, err := zw.Write(raw); err != nil {
		return err
	}
	if err := zw.Close(); err != nil {
		return err
	}
	if buf.Len() > maxSession {
		return fmt.Errorf("session is %d bytes gzipped, over the %d limit", buf.Len(), maxSession)
	}
	return api.session(ctx, buf.Bytes())
}

// projectDir is the directory claude keeps a working directory's sessions
// in, under $HOME/.claude/projects: the path with every '/', '_' and '.'
// replaced by '-' (so /work/repo is -work-repo).
func projectDir(workDir string) string {
	return strings.NewReplacer("/", "-", "_", "-", ".", "-").Replace(workDir)
}

// restoreSession writes the PR's session file where claude looks for it, so
// `claude --resume <id>` in the same working directory continues it.
func restoreSession(ctx context.Context, api *client, home, workDir, sessionID string) error {
	if home == "" || sessionID == "" || strings.ContainsAny(sessionID, "/.") {
		return errors.New("no HOME or a bad session id")
	}
	gz, err := api.resumeSession(ctx)
	if err != nil {
		return err
	}
	zr, err := gzip.NewReader(bytes.NewReader(gz))
	if err != nil {
		return fmt.Errorf("session is not gzip: %w", err)
	}
	dir := filepath.Join(home, ".claude", "projects", projectDir(workDir))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	f, err := os.OpenFile(filepath.Join(dir, sessionID+".jsonl"), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	n, err := io.Copy(f, io.LimitReader(zr, maxSessionRaw+1))
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil && n > maxSessionRaw {
		err = fmt.Errorf("session is over %d bytes uncompressed", maxSessionRaw)
	}
	return err
}

// maxSessionRaw bounds a restored session, uncompressed, as the controller
// does on upload.
const maxSessionRaw = 256 << 20

// clone checks out one branch of the repo: main, or a chat turn's PR branch.
// The token travels as an HTTP header through git's environment-supplied
// config, so it never appears in argv, in .git/config, or in the remote URL
// the agent can read later.
func clone(ctx context.Context, cfg Config, branch string) error {
	url := "https://github.com/" + cfg.Repo + ".git"
	cmd := exec.CommandContext(ctx, cfg.GitBin, "clone", "--quiet", "--depth", "50",
		"--single-branch", "--branch", branch, url, cfg.WorkDir)
	cmd.Env = []string{
		"PATH=" + cfg.Getenv("PATH"),
		"HOME=" + cfg.Getenv("HOME"),
		"GIT_TERMINAL_PROMPT=0",
	}
	if cfg.GitHubToken != "" {
		basic := base64.StdEncoding.EncodeToString([]byte("x-access-token:" + cfg.GitHubToken))
		cmd.Env = append(cmd.Env,
			"GIT_CONFIG_COUNT=1",
			"GIT_CONFIG_KEY_0=http.https://github.com/.extraheader",
			"GIT_CONFIG_VALUE_0=AUTHORIZATION: basic "+basic,
		)
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%w: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// resultEvent is the final stream-json line.
type resultEvent struct {
	Type       string          `json:"type"`
	Subtype    string          `json:"subtype"`
	IsError    bool            `json:"is_error"`
	Result     string          `json:"result"`
	NumTurns   *int            `json:"num_turns"`
	DurationMS *int64          `json:"duration_ms"`
	Usage      json.RawMessage `json:"usage"`
	// APIErrorStatus is the HTTP status of the API error that ended the
	// session, when one did.
	APIErrorStatus int `json:"api_error_status"`
	// apiError is the last assistant message's error kind (stream-json puts
	// it on the event, e.g. "authentication_failed"); stream fills it in.
	apiError string
}

// failureReason sorts a failed result into the failures that will fail every
// run the same way until a person or the clock fixes them -- the token
// (auth) and the subscription's limit (usage_limit) -- and everything else
// (""). Captured from claude 2.1.332 with an invalid token: the assistant
// event carries error "authentication_failed", the result
// api_error_status 401.
func failureReason(r *resultEvent) string {
	switch r.apiError {
	case "authentication_failed", "billing_error":
		return runnerapi.ReasonAuth
	case "rate_limit":
		return runnerapi.ReasonUsageLimit
	}
	switch r.APIErrorStatus {
	case 401, 403:
		return runnerapi.ReasonAuth
	case 429:
		return runnerapi.ReasonUsageLimit
	}
	if lower := strings.ToLower(r.Result); strings.Contains(lower, "usage limit") || strings.Contains(lower, "hit your limit") {
		return runnerapi.ReasonUsageLimit
	}
	return ""
}

const (
	batchSize     = 50
	batchInterval = 2 * time.Second
	maxLine       = 16 << 20
)

// stream forwards every stream-json line to the controller in batches and
// returns the result event. Upload failures are retried by the client; if a
// batch still fails, streaming carries on so the run itself is not lost.
func stream(ctx context.Context, r io.Reader, api *client) (*resultEvent, error) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64<<10), maxLine)

	var (
		result    *resultEvent
		apiError  string
		batch     []runnerapi.Event
		seq       int
		lastFlush = time.Now()
		uploadErr error
	)
	flush := func() {
		if len(batch) == 0 {
			return
		}
		if err := api.events(ctx, batch); err != nil && uploadErr == nil {
			uploadErr = err
		}
		batch = batch[:0]
		lastFlush = time.Now()
	}

	for sc.Scan() {
		line := bytes.TrimSpace(sc.Bytes())
		if len(line) == 0 {
			continue
		}
		var head struct {
			Type string `json:"type"`
		}
		if err := json.Unmarshal(line, &head); err != nil {
			// Not JSON; keep it anyway, wrapped, so the transcript is complete.
			wrapped, _ := json.Marshal(map[string]string{"type": "raw", "text": string(line)})
			line, head.Type = wrapped, "raw"
		}
		batch = append(batch, runnerapi.Event{Seq: seq, Type: head.Type, Payload: append(json.RawMessage(nil), line...)})
		seq++

		switch head.Type {
		case "assistant":
			var a struct {
				Error string `json:"error"`
			}
			if err := json.Unmarshal(line, &a); err == nil && a.Error != "" {
				apiError = a.Error
			}
		case "result":
			var res resultEvent
			if err := json.Unmarshal(line, &res); err == nil {
				res.apiError = apiError
				result = &res
			}
		}
		if len(batch) >= batchSize || time.Since(lastFlush) > batchInterval {
			flush()
		}
	}
	flush()
	if err := sc.Err(); err != nil {
		return result, err
	}
	return result, uploadErr
}

type limitedWriter struct {
	w io.Writer
	n int
}

func (l *limitedWriter) Write(p []byte) (int, error) {
	if l.n <= 0 {
		return len(p), nil
	}
	q := p
	if len(q) > l.n {
		q = q[:l.n]
	}
	l.n -= len(q)
	if _, err := l.w.Write(q); err != nil {
		return 0, err
	}
	return len(p), nil
}
