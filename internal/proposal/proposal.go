package proposal

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
)

// BranchPrefix is the only ref namespace the controller ever pushes to.
const BranchPrefix = "agent/"

// Config describes the repository and the local tools.
type Config struct {
	// Repo is owner/name on GitHub.
	Repo string
	// RemoteURL overrides https://github.com/<Repo>.git (tests use a local path).
	RemoteURL string
	// WorkDir is where per-proposal clones are made. Each is removed after.
	WorkDir string
	GitBin  string
	// Path is PATH for git and the hooks: they need bash, yq, kustomize, flux.
	Path string
	// PublicGate are exact strings (the real domain, the real tailnet) that
	// must never be committed. They are written to .publicgate-local, which
	// the repository's hooks read and .gitignore keeps out of commits.
	PublicGate []string
	// Committer identity.
	AuthorName  string
	AuthorEmail string
}

// Request is one proposal.
type Request struct {
	IncidentID int64
	Title      string
	Patch      string
}

// Commit is a validated commit, ready to push.
type Commit struct {
	Branch  string
	BaseSHA string
	HeadSHA string
	Files   []string

	dir  string
	cfg  Config
	auth string
}

// Close removes the clone.
func (c *Commit) Close() { _ = os.RemoveAll(c.dir) }

// BranchName is agent/<incident-id>.
func BranchName(incidentID int64) string {
	return BranchPrefix + strconv.FormatInt(incidentID, 10)
}

// CommitMessage is written by the controller, never by the agent: commit
// messages are replayed onto the public mirror verbatim, so only the title
// (checked by CheckTitle and the commit-msg hook) comes from the agent.
func CommitMessage(title string, incidentID int64) string {
	return fmt.Sprintf("%s\n\nProposed by the alert agent for incident #%d.\n", title, incidentID)
}

// Prepare clones main, applies the patch, checks every staged path, and
// commits through the repository's own git hooks. Hook or apply failures
// come back as *Rejected with their output, for the agent to act on.
//
// The patch is applied to the current main, not to whatever commit the agent
// started from, so the hooks that judge it are always main's. --3way lets a
// patch made against a slightly older main still land when the lines it
// touches did not move.
func Prepare(ctx context.Context, cfg Config, token string, req Request) (*Commit, error) {
	if err := CheckTitle(req.Title); err != nil {
		return nil, err
	}
	if err := CheckPatch(req.Patch); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(cfg.WorkDir, 0o700); err != nil {
		return nil, err
	}
	dir, err := os.MkdirTemp(cfg.WorkDir, "proposal-")
	if err != nil {
		return nil, err
	}
	c := &Commit{dir: dir, cfg: cfg, Branch: BranchName(req.IncidentID)}
	if token != "" {
		c.auth = base64.StdEncoding.EncodeToString([]byte("x-access-token:" + token))
	}
	if err := c.prepare(ctx, req); err != nil {
		c.Close()
		return nil, err
	}
	return c, nil
}

func (c *Commit) prepare(ctx context.Context, req Request) error {
	repoDir := filepath.Join(c.dir, "repo")
	if _, err := c.git(ctx, c.dir, "clone", "--quiet", "--depth", "50", "--single-branch", "--branch", "main",
		"--no-tags", c.remote(), repoDir); err != nil {
		return fmt.Errorf("clone: %w", err)
	}
	base, err := c.git(ctx, repoDir, "rev-parse", "HEAD")
	if err != nil {
		return err
	}
	c.BaseSHA = strings.TrimSpace(base)
	if _, err := c.git(ctx, repoDir, "checkout", "--quiet", "-b", c.Branch); err != nil {
		return err
	}

	patchFile := filepath.Join(c.dir, "patch.diff")
	if err := os.WriteFile(patchFile, []byte(ensureNewline(req.Patch)), 0o600); err != nil {
		return err
	}
	if _, err := c.git(ctx, repoDir, "apply", "--index", "--3way", "--whitespace=nowarn", patchFile); err != nil {
		return reject("the patch does not apply to the current main (%s):\n%s\nRe-read the files, edit them again and pass a fresh git_diff.",
			short(c.BaseSHA), clip(err.Error(), 4000))
	}
	if out, _ := c.git(ctx, repoDir, "diff", "--cached", "--name-only", "--diff-filter=U"); strings.TrimSpace(out) != "" {
		return reject("the patch conflicts with the current main in: %s", strings.TrimSpace(out))
	}

	raw, err := c.git(ctx, repoDir, "diff", "--cached", "--raw", "-z", "--no-renames", "--no-abbrev")
	if err != nil {
		return err
	}
	changes, err := parseRaw(raw)
	if err != nil {
		return err
	}
	if err := checkChanges(changes); err != nil {
		return err
	}
	if numstat, err := c.git(ctx, repoDir, "diff", "--cached", "--numstat"); err == nil {
		for _, line := range strings.Split(numstat, "\n") {
			if strings.HasPrefix(line, "-\t-\t") {
				return reject("binary changes are not allowed: %s", strings.TrimPrefix(line, "-\t-\t"))
			}
		}
	}
	for _, ch := range changes {
		c.Files = append(c.Files, ch.Path)
	}

	if len(c.cfg.PublicGate) > 0 {
		gate := strings.Join(c.cfg.PublicGate, "\n") + "\n"
		if err := os.WriteFile(filepath.Join(repoDir, ".publicgate-local"), []byte(gate), 0o600); err != nil {
			return err
		}
	}
	msgFile := filepath.Join(c.dir, "message.txt")
	if err := os.WriteFile(msgFile, []byte(CommitMessage(req.Title, req.IncidentID)), 0o600); err != nil {
		return err
	}
	// The hooks are what a human committing here would run: render every
	// overlay the commit touches, refuse a plaintext Secret, refuse a literal
	// private address or tailnet name in the files and in the message.
	if _, err := c.git(ctx, repoDir, "-c", "core.hooksPath=.githooks",
		"-c", "user.name="+c.cfg.AuthorName, "-c", "user.email="+c.cfg.AuthorEmail,
		"commit", "--quiet", "--file", msgFile); err != nil {
		return reject("the repository's commit hooks refused the change:\n%s", clip(err.Error(), 6000))
	}
	head, err := c.git(ctx, repoDir, "rev-parse", "HEAD")
	if err != nil {
		return err
	}
	c.HeadSHA = strings.TrimSpace(head)
	return nil
}

// Push publishes the branch. Only refs under agent/ are ever pushed, and the
// destination is spelled out in full so no refspec trickery can widen it.
// It overwrites a branch of the same name: one left behind by an earlier
// attempt for this incident that never became a PR.
func (c *Commit) Push(ctx context.Context) error {
	if !strings.HasPrefix(c.Branch, BranchPrefix) || strings.ContainsAny(c.Branch, " :^~?*[\\") {
		return errors.New("refusing to push outside " + BranchPrefix)
	}
	refspec := "+" + c.HeadSHA + ":refs/heads/" + c.Branch
	if _, err := c.git(ctx, filepath.Join(c.dir, "repo"), "push", "--quiet", "--no-verify", c.remote(), refspec); err != nil {
		return fmt.Errorf("push: %w", err)
	}
	return nil
}

func (c *Commit) remote() string {
	if c.cfg.RemoteURL != "" {
		return c.cfg.RemoteURL
	}
	return "https://github.com/" + c.cfg.Repo + ".git"
}

// git runs git with a minimal environment. The token travels as an HTTP
// header through environment-supplied config, never in argv or .git/config.
// It returns combined output, and on failure the output in the error too.
func (c *Commit) git(ctx context.Context, dir string, args ...string) (string, error) {
	bin := c.cfg.GitBin
	if bin == "" {
		bin = "git"
	}
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Dir = dir
	tmp := filepath.Join(c.dir, "tmp")
	_ = os.MkdirAll(tmp, 0o700)
	cmd.Env = []string{
		"PATH=" + c.cfg.Path,
		"HOME=" + c.dir,
		"TMPDIR=" + tmp,
		"LANG=C.UTF-8",
		"GIT_TERMINAL_PROMPT=0",
		"GIT_CONFIG_NOSYSTEM=1",
	}
	if c.auth != "" {
		cmd.Env = append(cmd.Env,
			"GIT_CONFIG_COUNT=1",
			"GIT_CONFIG_KEY_0=http.https://github.com/.extraheader",
			"GIT_CONFIG_VALUE_0=AUTHORIZATION: basic "+c.auth,
		)
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		return string(out), fmt.Errorf("git %s: %w: %s", args[0], err, clip(strings.TrimSpace(string(out)), 6000))
	}
	return string(out), nil
}

func ensureNewline(s string) string {
	if strings.HasSuffix(s, "\n") {
		return s
	}
	return s + "\n"
}

func short(sha string) string {
	if len(sha) > 12 {
		return sha[:12]
	}
	return sha
}

func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "\n… (truncated)"
}
