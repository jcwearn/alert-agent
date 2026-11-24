package runner

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"time"

	"github.com/jcwearn/alert-agent/internal/runnerapi"
)

// client talks to the controller's runner API.
type client struct {
	base  string
	token string
	http  *http.Client
}

func (c *client) context(ctx context.Context) (runnerapi.Context, error) {
	var rc runnerapi.Context
	err := c.do(ctx, http.MethodGet, runnerapi.PathContext, nil, &rc)
	return rc, err
}

func (c *client) events(ctx context.Context, events []runnerapi.Event) error {
	return c.do(ctx, http.MethodPost, runnerapi.PathEvents, events, nil)
}

func (c *client) finish(ctx context.Context, f runnerapi.Finish) error {
	return c.do(ctx, http.MethodPost, runnerapi.PathFinish, f, nil)
}

// fail reports a failed run. It returns an error only if the report itself
// could not be delivered; see Run for why.
func (c *client) fail(ctx context.Context, msg string) error {
	if err := c.finish(ctx, runnerapi.Finish{Status: "failed", Error: msg}); err != nil {
		return fmt.Errorf("%s (and reporting it failed: %w)", msg, err)
	}
	slog.Warn("run failed", "error", msg)
	return nil
}

// do retries transient failures: the controller restarting mid-run should
// not cost the run's transcript.
func (c *client) do(ctx context.Context, method, path string, body, out any) error {
	var payload []byte
	if body != nil {
		var err error
		if payload, err = json.Marshal(body); err != nil {
			return err
		}
	}

	var lastErr error
	for attempt := range 5 {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(time.Duration(attempt) * 2 * time.Second):
			}
		}
		retry, err := c.once(ctx, method, path, payload, out)
		if err == nil {
			return nil
		}
		lastErr = err
		if !retry {
			break
		}
	}
	return lastErr
}

func (c *client) once(ctx context.Context, method, path string, payload []byte, out any) (retry bool, err error) {
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, bytes.NewReader(payload))
	if err != nil {
		return false, err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return true, err
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode >= 300 {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		err := fmt.Errorf("%s %s: %s: %s", method, path, resp.Status, bytes.TrimSpace(msg))
		return resp.StatusCode >= 500, err
	}
	if out != nil {
		return false, json.NewDecoder(resp.Body).Decode(out)
	}
	return false, nil
}
