// Command alert-agent is one binary with several roles: the long-running
// controller, its schema migration, and the runner that wraps `claude -p`
// inside a per-incident Job.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/discovery/cached/memory"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/restmapper"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/jcwearn/alert-agent/internal/controller"
	"github.com/jcwearn/alert-agent/internal/dispatch"
	"github.com/jcwearn/alert-agent/internal/github"
	"github.com/jcwearn/alert-agent/internal/mcptools"
	"github.com/jcwearn/alert-agent/internal/preview"
	"github.com/jcwearn/alert-agent/internal/proposal"
	"github.com/jcwearn/alert-agent/internal/runner"
	"github.com/jcwearn/alert-agent/internal/store"
)

const usage = "usage: alert-agent <controller|migrate|runner|mcp|ui-preview>"

func main() {
	os.Exit(run(os.Args[1:]))
}

func run(args []string) int {
	if len(args) < 1 {
		fmt.Fprintln(os.Stderr, usage)
		return 2
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	var err error
	switch args[0] {
	case "controller":
		err = runController(ctx)
	case "migrate":
		err = runMigrate(ctx)
	case "runner":
		err = runRunner(ctx)
	case "mcp":
		err = runMCP(ctx)
	case "ui-preview":
		err = runPreview(ctx)
	default:
		fmt.Fprintln(os.Stderr, usage)
		return 2
	}

	if err != nil {
		slog.Error("exiting", "err", err)
		return 1
	}
	return 0
}

func runController(ctx context.Context) error {
	st, err := openStore(ctx)
	if err != nil {
		return err
	}
	defer st.Close()

	opts := controller.Options{
		AgentEnabled:   os.Getenv("AGENT_ENABLED") == "true",
		PublicURL:      os.Getenv("PUBLIC_URL"),
		MaxTurns:       envInt("RUN_MAX_TURNS", 40),
		RunHold:        envDuration("RUN_HOLD", time.Minute),
		RecentWindow:   envDuration("RECENT_WINDOW", 30*time.Minute),
		ChatMaxPerHour: envInt("CHAT_MAX_PER_HOUR", 12),
		// CHAT_MAX_PER_PR_HOUR is the 0.10-0.12 name, still read during the move.
		ChatMaxPerThreadHour: envInt("CHAT_MAX_PER_THREAD_HOUR", envInt("CHAT_MAX_PER_PR_HOUR", 4)),
	}

	var notifier controller.Notifier
	if hook := os.Getenv("SLACK_WEBHOOK_URL"); hook != "" {
		notifier = &controller.SlackNotifier{WebhookURL: hook, PublicURL: opts.PublicURL}
	}

	addrs := []string{envOr("LISTEN_ADDR", ":8080"), envOr("WEBHOOK_LISTEN_ADDR", ":8081"), envOr("RUNNER_LISTEN_ADDR", ":8082")}
	if !opts.AgentEnabled {
		slog.Info("agent disabled: incidents are recorded, no runs are started")
		return controller.New(st, opts, nil, notifier).Run(ctx, addrs[0], addrs[1], addrs[2])
	}

	app, err := githubApp()
	if err != nil {
		return err
	}
	var srv *controller.Server
	d, err := newDispatcher(st, app, func(id int64) { srv.RunFinished(id) })
	if err != nil {
		return err
	}
	srv = controller.New(st, opts, d, notifier)
	if os.Getenv("PR_ENABLED") == "true" {
		srv.EnablePRs(newPRs(app), newReviewer(notifier))
		slog.Info("PR path enabled: runners may propose_pr")
		if c := newComments(app); c != nil {
			srv.EnableComments(c)
			slog.Info("PR comments enabled", "logins", c.AllowedLogins, "interval", c.Interval)
		}
	}
	go d.Run(ctx)
	return srv.Run(ctx, addrs[0], addrs[1], addrs[2])
}

func githubApp() (*github.App, error) {
	keyPEM, err := os.ReadFile(envOr("GITHUB_PRIVATE_KEY_FILE", "/etc/alert-agent/github/private-key"))
	if err != nil {
		return nil, fmt.Errorf("github app key: %w", err)
	}
	key, err := github.ParsePrivateKey(keyPEM)
	if err != nil {
		return nil, err
	}
	app := &github.App{AppID: os.Getenv("GITHUB_APP_ID"), InstallationID: os.Getenv("GITHUB_INSTALLATION_ID"), Key: key}
	if app.AppID == "" || app.InstallationID == "" {
		return nil, errors.New("GITHUB_APP_ID and GITHUB_INSTALLATION_ID must be set")
	}
	return app, nil
}

// newPRs configures the PR path. PUBLIC_GATE lists, comma-separated, the
// exact strings the repository's hooks must refuse (the real domain and
// tailnet); the Deployment fills it from the cluster-vars substitution, so
// the values never appear in a manifest.
func newPRs(app *github.App) *controller.PRs {
	var gate []string
	for _, v := range strings.Split(os.Getenv("PUBLIC_GATE"), ",") {
		if v = strings.TrimSpace(v); v != "" {
			gate = append(gate, v)
		}
	}
	if len(gate) == 0 {
		slog.Warn("PUBLIC_GATE is empty: the hooks only catch private values a regex can see")
	}
	repo := envOr("REPO", "jcwearn/k3s-cluster")
	return &controller.PRs{
		GitHub: app,
		Repo:   repo,
		Proposal: proposal.Config{
			Repo:        repo,
			WorkDir:     envOr("PROPOSAL_WORK_DIR", "/work/proposals"),
			Path:        envOr("PATH", "/usr/local/bin:/usr/bin:/bin"),
			PublicGate:  gate,
			AuthorName:  envOr("COMMIT_AUTHOR_NAME", "alert-agent[bot]"),
			AuthorEmail: envOr("COMMIT_AUTHOR_EMAIL", "alert-agent[bot]@users.noreply.github.com"),
		},
	}
}

// newComments turns on PR comments as a way into the chat when
// CHAT_ALLOWED_LOGINS names at least one GitHub login (comma-separated).
func newComments(app *github.App) *controller.Comments {
	var logins []string
	for _, v := range strings.Split(os.Getenv("CHAT_ALLOWED_LOGINS"), ",") {
		if v = strings.TrimSpace(v); v != "" {
			logins = append(logins, v)
		}
	}
	if len(logins) == 0 {
		return nil
	}
	return &controller.Comments{GitHub: app, AllowedLogins: logins, Interval: envDuration("COMMENT_POLL_INTERVAL", 30*time.Second)}
}

// newReviewer sends "needs your review" to ntfy and to Slack, whichever of
// them is configured, or is nil if neither is.
func newReviewer(slack controller.Notifier) controller.Reviewer {
	var rs controller.Reviewers
	base, token := os.Getenv("NTFY_URL"), os.Getenv("NTFY_TOKEN")
	if base != "" && token != "" {
		rs = append(rs, &controller.NtfyReviewer{BaseURL: base, Topic: envOr("NTFY_TOPIC", "medic"), Token: token})
	} else {
		slog.Warn("NTFY_URL or NTFY_TOKEN unset: PRs will not be announced on ntfy")
	}
	if r, ok := slack.(controller.Reviewer); ok {
		rs = append(rs, r)
	}
	if len(rs) == 0 {
		return nil
	}
	return rs
}

func newDispatcher(st *store.Store, app *github.App, onFinish func(int64)) (*dispatch.Dispatcher, error) {
	cfg, err := rest.InClusterConfig()
	if err != nil {
		return nil, fmt.Errorf("kubernetes config: %w", err)
	}
	kube, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return nil, err
	}

	namespace := envOr("POD_NAMESPACE", "alert-agent")
	image, err := runnerImage(kube, namespace)
	if err != nil {
		return nil, err
	}
	slog.Info("runner image", "image", image)
	return dispatch.New(dispatch.Config{
		Namespace:      namespace,
		Image:          image,
		ServiceAccount: envOr("RUNNER_SERVICE_ACCOUNT", "alert-agent-runner"),
		ClaudeSecret:   envOr("CLAUDE_SECRET", "alert-agent-claude"),
		ControllerURL:  envOr("RUNNER_API_URL", "http://alert-agent.alert-agent.svc.cluster.local:8082"),
		Repo:           envOr("REPO", "jcwearn/k3s-cluster"),
		Model:          os.Getenv("RUN_MODEL"),
		TZ:             envOr("TZ", "UTC"),
		Timeout:        envDuration("RUN_TIMEOUT", 20*time.Minute),
		MaxRunsPerHour: envInt("RUN_MAX_PER_HOUR", 6),
		Interval:       10 * time.Second,
	}, st, kube, app, onFinish), nil
}

// runnerImage is the image runner Jobs use: RUNNER_IMAGE if set, otherwise
// this pod's own controller image. Reading it from the pod means a Renovate
// bump of the Deployment's image moves the runners too, with no second pin to
// forget.
func runnerImage(kube kubernetes.Interface, namespace string) (string, error) {
	if img := os.Getenv("RUNNER_IMAGE"); img != "" {
		return img, nil
	}
	podName := os.Getenv("POD_NAME")
	if podName == "" {
		return "", errors.New("set RUNNER_IMAGE, or POD_NAME so the controller can use its own image")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	pod, err := kube.CoreV1().Pods(namespace).Get(ctx, podName, metav1.GetOptions{})
	if err != nil {
		return "", fmt.Errorf("read own pod for runner image: %w", err)
	}
	for _, c := range pod.Spec.Containers {
		if c.Name == envOr("CONTROLLER_CONTAINER", "controller") {
			return c.Image, nil
		}
	}
	return "", errors.New("own pod has no controller container")
}

// runMCP serves the read-only tools on stdio. claude starts it inside the
// runner pod; stdout is the protocol, so all logging goes to stderr.
func runMCP(ctx context.Context) error {
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, nil)))
	cfg, err := rest.InClusterConfig()
	if errors.Is(err, rest.ErrNotInCluster) {
		// Local development: the usual kubeconfig.
		cfg, err = clientcmd.NewNonInteractiveDeferredLoadingClientConfig(
			clientcmd.NewDefaultClientConfigLoadingRules(), nil).ClientConfig()
	}
	if err != nil {
		return fmt.Errorf("kubernetes config: %w", err)
	}
	core, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return err
	}
	dyn, err := dynamic.NewForConfig(cfg)
	if err != nil {
		return err
	}
	cached := memory.NewMemCacheClient(core.Discovery())
	mapper := restmapper.NewShortcutExpander(restmapper.NewDeferredDiscoveryRESTMapper(cached), cached, nil)

	return mcptools.Run(ctx, &mcptools.Deps{
		Dynamic:       dyn,
		Mapper:        mapper,
		Core:          core,
		PrometheusURL: envOr("PROMETHEUS_URL", "http://kube-prometheus-stack-prometheus.prometheus.svc.cluster.local:9090"),
		LokiURL:       envOr("LOKI_URL", "http://loki.loki.svc.cluster.local:3100"),
		RepoDir:       envOr("REPO_DIR", "/work/repo"),
	}, "alert-agent")
}

func runRunner(ctx context.Context) error {
	cfg, err := runner.ConfigFromEnv()
	if err != nil {
		return err
	}
	return runner.Run(ctx, cfg)
}

func runMigrate(ctx context.Context) error {
	st, err := openStore(ctx)
	if err != nil {
		return err
	}
	defer st.Close()
	if err := st.Migrate(ctx); err != nil {
		return err
	}
	slog.Info("migrations applied")
	return nil
}

// openStore builds the connection string from the pieces CloudNativePG puts
// in its generated app Secret, so no password ever sits in a URL in a manifest.
func openStore(ctx context.Context) (*store.Store, error) {
	host, name, user, pass := os.Getenv("DB_HOST"), os.Getenv("DB_NAME"), os.Getenv("DB_USER"), os.Getenv("DB_PASSWORD")
	if host == "" || name == "" || user == "" {
		return nil, errors.New("DB_HOST, DB_NAME and DB_USER must be set")
	}
	u := url.URL{
		Scheme:   "postgres",
		User:     url.UserPassword(user, pass),
		Host:     host,
		Path:     "/" + name,
		RawQuery: "sslmode=" + envOr("DB_SSLMODE", "require"),
	}
	return store.Open(ctx, u.String())
}

func envInt(key string, fallback int) int {
	if n, err := strconv.Atoi(os.Getenv(key)); err == nil && n > 0 {
		return n
	}
	return fallback
}

func envDuration(key string, fallback time.Duration) time.Duration {
	if d, err := time.ParseDuration(os.Getenv(key)); err == nil && d > 0 {
		return d
	}
	return fallback
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// runPreview serves the UI over made-up data on localhost, for looking at
// UI changes (and screenshotting them) without a cluster. See
// internal/preview.
func runPreview(ctx context.Context) error {
	addr := envOr("PREVIEW_ADDR", "127.0.0.1:8090")
	srv := &http.Server{Addr: addr, Handler: preview.Handler(), ReadHeaderTimeout: 5 * time.Second}
	go func() {
		<-ctx.Done()
		_ = srv.Close()
	}()
	slog.Info("ui preview", "url", "http://"+addr)
	if err := srv.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}
