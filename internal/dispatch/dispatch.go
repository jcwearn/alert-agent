// Package dispatch turns queued runs into runner Jobs, one at a time, and
// notices when a runner dies without reporting.
package dispatch

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log/slog"
	"strings"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"

	"github.com/jcwearn/alert-agent/internal/github"
	"github.com/jcwearn/alert-agent/internal/store"
)

// JobPrefix names every runner Job and its Secret: alert-agent-run-<id>.
const JobPrefix = "alert-agent-run-"

// Config is how runner Jobs are built and paced.
type Config struct {
	Namespace      string
	Image          string
	ServiceAccount string
	ClaudeSecret   string
	ControllerURL  string
	Repo           string // owner/name of the GitOps repo
	Model          string
	TZ             string
	Timeout        time.Duration
	MaxRunsPerHour int
	Interval       time.Duration
}

// Store is what the dispatcher needs from persistence.
type Store interface {
	ClaimNextRun(ctx context.Context, tokenHash []byte, jobPrefix, model string) (store.Run, bool, error)
	ClaimNextChatRun(ctx context.Context, tokenHash []byte, jobPrefix, model string) (store.Run, bool, error)
	RunningRun(ctx context.Context) (store.Run, bool, error)
	RunsStartedSince(ctx context.Context, t time.Time) (int, error)
	FinishRun(ctx context.Context, runID int64, res store.RunResult) (bool, error)
	GetClaudeBlock(ctx context.Context) (store.ClaudeBlock, bool, error)
}

// TokenMinter issues narrowed GitHub installation tokens.
type TokenMinter interface {
	InstallationToken(ctx context.Context, repos []string, permissions map[string]string) (github.Token, error)
}

// Dispatcher owns the runner queue.
type Dispatcher struct {
	cfg      Config
	store    Store
	kube     kubernetes.Interface
	gh       TokenMinter
	wake     chan struct{}
	now      func() time.Time
	onFinish func(runID int64)
	capped   bool
	blocked  bool
}

// New returns a Dispatcher. onFinish is called after the dispatcher itself
// fails a run (the runner reports its own successes and failures through the
// runner API).
func New(cfg Config, st Store, kube kubernetes.Interface, gh TokenMinter, onFinish func(int64)) *Dispatcher {
	if onFinish == nil {
		onFinish = func(int64) {}
	}
	return &Dispatcher{
		cfg: cfg, store: st, kube: kube, gh: gh, onFinish: onFinish,
		wake: make(chan struct{}, 1), now: time.Now,
	}
}

// Wake asks for a pass soon, e.g. after a run is queued or finished.
func (d *Dispatcher) Wake() {
	select {
	case d.wake <- struct{}{}:
	default:
	}
}

// Run loops until ctx is cancelled.
func (d *Dispatcher) Run(ctx context.Context) {
	t := time.NewTicker(d.cfg.Interval)
	defer t.Stop()
	for {
		if err := d.Tick(ctx); err != nil {
			slog.Error("dispatch", "err", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		case <-d.wake:
		}
	}
}

// reportGrace is how long a finished Job may lag its runner's finish report
// before the run is failed for never reporting.
const reportGrace = 30 * time.Second

// Tick makes one pass: check on the running run, or start the next one.
func (d *Dispatcher) Tick(ctx context.Context) error {
	running, ok, err := d.store.RunningRun(ctx)
	if err != nil {
		return err
	}
	if ok {
		return d.checkRunning(ctx, running)
	}

	// Claude refuses every run (token or usage limit): start nothing,
	// chat turns included, until the retry time. The first run after it is
	// the probe; a success clears the block.
	block, isBlocked, err := d.store.GetClaudeBlock(ctx)
	if err != nil {
		return err
	}
	if isBlocked && d.now().Before(block.Until) {
		if !d.blocked {
			slog.Warn("claude blocked; queued runs wait", "reason", block.Reason, "until", block.Until)
			d.blocked = true
		}
		return nil
	}
	d.blocked = false

	// MaxRunsPerHour caps alert runs. Chat turns are budgeted when their
	// message arrives, so they still start when alert runs must wait.
	started, err := d.store.RunsStartedSince(ctx, d.now().Add(-time.Hour))
	if err != nil {
		return err
	}
	claim := d.store.ClaimNextRun
	if started >= d.cfg.MaxRunsPerHour {
		if !d.capped {
			slog.Warn("hourly alert run cap reached; queued alert runs wait", "cap", d.cfg.MaxRunsPerHour)
			d.capped = true
		}
		claim = d.store.ClaimNextChatRun
	} else {
		d.capped = false
	}

	token, hash, err := newRunToken()
	if err != nil {
		return err
	}
	run, ok, err := claim(ctx, hash, JobPrefix, d.cfg.Model)
	if err != nil || !ok {
		return err
	}
	if err := d.start(ctx, run, token); err != nil {
		d.fail(ctx, run.ID, "could not start runner: "+err.Error())
		return err
	}
	slog.Info("runner started", "run", run.ID, "incident", run.IncidentID, "job", run.JobName)
	return nil
}

func (d *Dispatcher) start(ctx context.Context, run store.Run, runToken string) error {
	repoName := d.cfg.Repo[strings.LastIndex(d.cfg.Repo, "/")+1:]
	gh, err := d.gh.InstallationToken(ctx, []string{repoName}, map[string]string{"contents": "read"})
	if err != nil {
		return fmt.Errorf("github token: %w", err)
	}

	secrets := d.kube.CoreV1().Secrets(d.cfg.Namespace)
	if _, err := secrets.Create(ctx, buildSecret(d.cfg, run, runToken, gh.Token), metav1.CreateOptions{}); err != nil {
		return fmt.Errorf("create secret: %w", err)
	}
	job, err := d.kube.BatchV1().Jobs(d.cfg.Namespace).Create(ctx, buildJob(d.cfg, run), metav1.CreateOptions{})
	if err != nil {
		_ = secrets.Delete(ctx, run.JobName, metav1.DeleteOptions{})
		return fmt.Errorf("create job: %w", err)
	}

	// Hand the Secret to the Job, so the Job's TTL garbage-collects both.
	sec, err := secrets.Get(ctx, run.JobName, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("get secret: %w", err)
	}
	sec.OwnerReferences = []metav1.OwnerReference{{
		APIVersion: "batch/v1", Kind: "Job", Name: job.Name, UID: job.UID,
	}}
	if _, err := secrets.Update(ctx, sec, metav1.UpdateOptions{}); err != nil {
		slog.Warn("could not set secret owner; it will outlive its job", "secret", sec.Name, "err", err)
	}
	return nil
}

func (d *Dispatcher) checkRunning(ctx context.Context, run store.Run) error {
	job, err := d.kube.BatchV1().Jobs(d.cfg.Namespace).Get(ctx, run.JobName, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		if run.StartedAt != nil && d.now().Sub(*run.StartedAt) > time.Minute {
			d.fail(ctx, run.ID, "runner Job disappeared before reporting")
		}
		return nil
	}
	if err != nil {
		return err
	}

	for _, c := range job.Status.Conditions {
		if c.Status != corev1.ConditionTrue {
			continue
		}
		switch c.Type {
		case batchv1.JobFailed:
			msg := "runner Job failed: " + c.Reason
			if c.Message != "" {
				msg += " (" + c.Message + ")"
			}
			d.fail(ctx, run.ID, msg)
		case batchv1.JobComplete:
			if d.now().Sub(c.LastTransitionTime.Time) > reportGrace {
				d.fail(ctx, run.ID, "runner exited without reporting a result")
			}
		}
	}
	return nil
}

func (d *Dispatcher) fail(ctx context.Context, runID int64, msg string) {
	finished, err := d.store.FinishRun(ctx, runID, store.RunResult{Status: store.RunFailed, Error: msg})
	if err != nil {
		slog.Error("failing run", "run", runID, "err", err)
		return
	}
	if finished {
		slog.Warn("run failed", "run", runID, "reason", msg)
		d.onFinish(runID)
		d.Wake()
	}
}

// newRunToken returns a random bearer token and the sha256 the store keeps.
func newRunToken() (token string, hash []byte, err error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", nil, err
	}
	token = hex.EncodeToString(b)
	return token, HashToken(token), nil
}

// HashToken is how runner tokens are stored and looked up.
func HashToken(token string) []byte {
	sum := sha256.Sum256([]byte(token))
	return sum[:]
}
