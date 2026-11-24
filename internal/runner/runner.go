// Package runner is the Job side of a run: fetch the incident, check out the
// GitOps repo, run `claude -p` read-only, and stream everything it prints
// back to the controller.
package runner

import (
	"bufio"
	"bytes"
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
	ClaudeBin     string
	GitBin        string
	SelfBin       string // this binary, started by claude as the MCP tool server
	Getenv        func(string) string
	HTTP          *http.Client
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

	if err := clone(ctx, cfg); err != nil {
		return api.fail(ctx, "checkout failed: "+err.Error())
	}

	settings, err := settingsJSON()
	if err != nil {
		return api.fail(ctx, err.Error())
	}
	mcpConfig, err := mcpConfigJSON(cfg.SelfBin, cfg.WorkDir, cfg.Getenv)
	if err != nil {
		return api.fail(ctx, err.Error())
	}

	cmd := exec.CommandContext(ctx, cfg.ClaudeBin, claudeArgs(rc, settings, mcpConfig, userPrompt(rc))...)
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

	fin := runnerapi.Finish{Status: "succeeded"}
	if result != nil {
		fin.Report = result.Result
		fin.NumTurns = result.NumTurns
		fin.DurationMS = result.DurationMS
		fin.Usage = result.Usage
		if result.IsError || result.Subtype != "success" {
			fin.Status = "failed"
			fin.Error = "claude ended with " + result.Subtype
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

// clone checks out the repo's main branch. The token travels as an HTTP
// header through git's environment-supplied config, so it never appears in
// argv, in .git/config, or in the remote URL the agent can read later.
func clone(ctx context.Context, cfg Config) error {
	url := "https://github.com/" + cfg.Repo + ".git"
	cmd := exec.CommandContext(ctx, cfg.GitBin, "clone", "--quiet", "--depth", "50",
		"--single-branch", "--branch", "main", url, cfg.WorkDir)
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

		if head.Type == "result" {
			var res resultEvent
			if err := json.Unmarshal(line, &res); err == nil {
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
