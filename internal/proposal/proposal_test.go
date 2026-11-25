package proposal

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// The fixture's hooks stand in for the real repository's: pre-commit refuses
// a staged file containing FORBIDDEN or a value from .publicgate-local, and
// commit-msg refuses a 192.168 address.
const preCommit = `#!/bin/sh
root=$(git rev-parse --show-toplevel)
for f in $(git diff --cached --name-only --diff-filter=ACMR); do
  if git show ":$f" | grep -q FORBIDDEN; then echo "pre-commit: forbidden content in $f" >&2; exit 1; fi
  if [ -f "$root/.publicgate-local" ]; then
    while read -r n; do
      [ -n "$n" ] || continue
      if git show ":$f" | grep -qF -- "$n"; then echo "pre-commit: literal value from .publicgate-local in $f" >&2; exit 1; fi
    done < "$root/.publicgate-local"
  fi
done
`

const commitMsg = `#!/bin/sh
if grep -qE '192\.168\.[0-9]+\.[0-9]+' "$1"; then echo "commit-msg: literal private value" >&2; exit 1; fi
`

func run(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1", "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t",
		"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return string(out)
}

func write(t *testing.T, path, content string, mode os.FileMode) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), mode); err != nil {
		t.Fatal(err)
	}
}

// fixture makes a bare "GitHub" repository with a main branch, and a working
// clone the test edits to produce patches, as the runner's checkout would.
func fixture(t *testing.T) (cfg Config, remote, work string) {
	t.Helper()
	root := t.TempDir()
	remote = filepath.Join(root, "remote.git")
	run(t, root, "init", "--quiet", "--bare", "--initial-branch=main", remote)

	seed := filepath.Join(root, "seed")
	run(t, root, "clone", "--quiet", remote, seed)
	run(t, seed, "checkout", "--quiet", "-b", "main")
	write(t, filepath.Join(seed, filepath.FromSlash("apps/web/deployment.yaml")), "memory: 128Mi\ncpu: 100m\n", 0o644)
	write(t, filepath.Join(seed, filepath.FromSlash("apps/web/run.sh")), "#!/bin/sh\necho hi\n", 0o755)
	write(t, filepath.Join(seed, filepath.FromSlash("apps/alert-agent/rbac.yaml")), "rules: []\n", 0o644)
	write(t, filepath.Join(seed, filepath.FromSlash("apps/web/secrets.sops.yaml")), "data: x\n", 0o644)
	write(t, filepath.Join(seed, filepath.FromSlash("scripts/render.sh")), "#!/bin/sh\n", 0o755)
	write(t, filepath.Join(seed, filepath.FromSlash(".githooks/pre-commit")), preCommit, 0o755)
	write(t, filepath.Join(seed, filepath.FromSlash(".githooks/commit-msg")), commitMsg, 0o755)
	write(t, filepath.Join(seed, ".gitignore"), ".publicgate-local\n", 0o644)
	run(t, seed, "add", "-A")
	run(t, seed, "commit", "--quiet", "-m", "init")
	run(t, seed, "push", "--quiet", "origin", "main")

	work = filepath.Join(root, "work")
	run(t, root, "clone", "--quiet", remote, work)

	cfg = Config{
		Repo: "o/r", RemoteURL: remote, WorkDir: filepath.Join(root, "proposals"),
		Path: os.Getenv("PATH"), PublicGate: []string{"secret-domain.example"},
		AuthorName: "alert-agent[bot]", AuthorEmail: "alert-agent[bot]@users.noreply.github.com",
	}
	return cfg, remote, work
}

// diff returns the working clone's changes as git_diff would, then resets it.
func diff(t *testing.T, work string) string {
	t.Helper()
	run(t, work, "add", "-A", "--intent-to-add")
	d := run(t, work, "diff", "--full-index", "--binary")
	run(t, work, "reset", "--quiet", "--hard")
	run(t, work, "clean", "--quiet", "-fd")
	return d
}

func edit(t *testing.T, work, path, content string) string {
	t.Helper()
	write(t, filepath.Join(work, path), content, 0o644)
	return diff(t, work)
}

func TestPrepareAndPush(t *testing.T) {
	cfg, remote, work := fixture(t)
	patch := edit(t, work, "apps/web/deployment.yaml", "memory: 256Mi\ncpu: 100m\n")

	c, err := Prepare(t.Context(), cfg, "", Request{IncidentID: 5, Title: "fix(web): raise memory limit", Patch: patch})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(c.Close)
	if c.Branch != "agent/5" || c.HeadSHA == "" || c.BaseSHA == "" || len(c.Files) != 1 || c.Files[0] != "apps/web/deployment.yaml" {
		t.Fatalf("commit = %+v", c)
	}
	if err := c.Push(t.Context()); err != nil {
		t.Fatal(err)
	}

	msg := run(t, remote, "log", "-1", "--format=%an%n%B", "agent/5")
	if !strings.Contains(msg, "alert-agent[bot]\nfix(web): raise memory limit\n\nProposed by the alert agent for incident #5.") {
		t.Errorf("commit message = %q", msg)
	}
	files := run(t, remote, "show", "--name-only", "--format=", "agent/5")
	if strings.TrimSpace(files) != "apps/web/deployment.yaml" {
		t.Errorf(".publicgate-local or other files leaked into the commit: %q", files)
	}
	if got := run(t, remote, "show", "agent/5:apps/web/deployment.yaml"); got != "memory: 256Mi\ncpu: 100m\n" {
		t.Errorf("content = %q", got)
	}
	if main := strings.TrimSpace(run(t, remote, "rev-parse", "main")); main != c.BaseSHA {
		t.Errorf("main moved: %s, base %s", main, c.BaseSHA)
	}
}

func TestPrepareAppliesOnMovedMain(t *testing.T) {
	cfg, _, work := fixture(t)
	patch := edit(t, work, "apps/web/deployment.yaml", "memory: 256Mi\ncpu: 100m\n")

	// main gains an unrelated commit after the agent's checkout.
	write(t, filepath.Join(work, filepath.FromSlash("apps/other/x.yaml")), "a: 1\n", 0o644)
	run(t, work, "add", "-A")
	run(t, work, "commit", "--quiet", "-m", "unrelated")
	run(t, work, "push", "--quiet", "origin", "main")

	c, err := Prepare(t.Context(), cfg, "", Request{IncidentID: 1, Title: "fix(web): raise memory limit", Patch: patch})
	if err != nil {
		t.Fatal(err)
	}
	c.Close()
}

func TestPrepareRejects(t *testing.T) {
	cfg, _, work := fixture(t)

	cases := []struct {
		name  string
		title string
		patch func() string
		want  string
	}{
		{"scripts", "fix(x): y", func() string { return edit(t, work, "scripts/render.sh", "#!/bin/sh\nexit 0\n") }, "only files under"},
		{"hooks", "fix(x): y", func() string { return edit(t, work, ".githooks/pre-commit", "exit 0\n") }, "only files under"},
		{"sops", "fix(x): y", func() string { return edit(t, work, "apps/web/secrets.sops.yaml", "data: y\n") }, "encrypted secrets"},
		{"own deployment", "fix(x): y", func() string { return edit(t, work, "apps/alert-agent/rbac.yaml", "rules: [all]\n") }, "its own deployment"},
		{"symlink", "fix(x): y", func() string {
			if err := os.Symlink("/etc/passwd", filepath.Join(work, filepath.FromSlash("apps/web/link"))); err != nil {
				t.Fatal(err)
			}
			return diff(t, work)
		}, "regular"},
		{"executable", "fix(x): y", func() string {
			write(t, filepath.Join(work, filepath.FromSlash("apps/web/new.sh")), "echo\n", 0o755)
			return diff(t, work)
		}, "non-executable"},
		{"mode change", "fix(x): y", func() string {
			if err := os.Chmod(filepath.Join(work, filepath.FromSlash("apps/web/run.sh")), 0o644); err != nil {
				t.Fatal(err)
			}
			return diff(t, work)
		}, "modes may not change"},
		{"pre-commit hook", "fix(x): y", func() string { return edit(t, work, "apps/web/deployment.yaml", "FORBIDDEN\n") }, "commit hooks refused"},
		{"public gate", "fix(x): y", func() string {
			return edit(t, work, "apps/web/deployment.yaml", "host: app.secret-domain.example\n")
		}, ".publicgate-local"},
		{"commit-msg hook", "fix(x): point at 192.168.1.10", func() string {
			return edit(t, work, "apps/web/deployment.yaml", "memory: 1Gi\n")
		}, "commit hooks refused"},
		{"does not apply", "fix(x): y", func() string {
			return "diff --git a/apps/web/deployment.yaml b/apps/web/deployment.yaml\n--- a/apps/web/deployment.yaml\n+++ b/apps/web/deployment.yaml\n@@ -1 +1 @@\n-memory: 999Mi\n+memory: 1Gi\n"
		}, "does not apply"},
		{"outside the repo", "fix(x): y", func() string {
			return "diff --git a/../escape b/../escape\nnew file mode 100644\n--- /dev/null\n+++ b/../escape\n@@ -0,0 +1 @@\n+x\n"
		}, "does not apply"},
		{"bad title", "Fix the thing", func() string { return edit(t, work, "apps/web/deployment.yaml", "memory: 1Gi\n") }, "conventional commit"},
		{"empty", "fix(x): y", func() string { return "" }, "empty"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			commit, err := Prepare(t.Context(), cfg, "", Request{IncidentID: 9, Title: c.title, Patch: c.patch()})
			if err == nil {
				commit.Close()
				t.Fatal("accepted")
			}
			var r *Rejected
			if !errors.As(err, &r) {
				t.Fatalf("want *Rejected, got %T: %v", err, err)
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Errorf("error %q does not mention %q", err, c.want)
			}
		})
	}
	// Every failed attempt cleans up its clone.
	if entries, _ := os.ReadDir(cfg.WorkDir); len(entries) != 0 {
		t.Errorf("%d clones left behind", len(entries))
	}
}

func TestPushRefusesOtherRefs(t *testing.T) {
	c := &Commit{Branch: "main", HeadSHA: "abc"}
	if err := c.Push(t.Context()); err == nil {
		t.Fatal("push to main was attempted")
	}
}

func TestCheckPath(t *testing.T) {
	ok := []string{"apps/web/deployment.yaml", "infrastructure/prometheus/helm.yaml", "apps/mkdocs/data/docs/apps/web.md"}
	bad := []string{"clusters/prod/apps.yaml", ".github/workflows/x.yaml", ".githooks/pre-commit", "scripts/render.sh",
		".claude/rules/x.md", "docs/plans/x/plan.md", ".sops.yaml", "apps/x/secrets.sops.yaml",
		"apps/alert-agent/rbac.yaml", "apps/../clusters/x", "/etc/passwd", "README.md", "renovate.json"}
	for _, p := range ok {
		if err := checkPath(p); err != nil {
			t.Errorf("%s refused: %v", p, err)
		}
	}
	for _, p := range bad {
		if err := checkPath(p); err == nil {
			t.Errorf("%s allowed", p)
		}
	}
}

// openedPR pushes agent/5 as propose_pr would, and checks the working clone
// out on it, as a chat turn's runner would.
func openedPR(t *testing.T) (cfg Config, remote, work string, pr *Commit) {
	t.Helper()
	cfg, remote, work = fixture(t)
	patch := edit(t, work, "apps/web/deployment.yaml", "memory: 256Mi\ncpu: 100m\n")
	pr, err := Prepare(t.Context(), cfg, "", Request{IncidentID: 5, Title: "fix(web): raise memory limit", Patch: patch})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pr.Close)
	if err := pr.Push(t.Context()); err != nil {
		t.Fatal(err)
	}
	run(t, work, "fetch", "--quiet", "origin", "agent/5")
	run(t, work, "checkout", "--quiet", "-b", "agent/5", "FETCH_HEAD")
	return cfg, remote, work, pr
}

func updateReq(pr *Commit, title, patch string) UpdateRequest {
	return UpdateRequest{IncidentID: 5, Branch: "agent/5", HeadSHA: pr.HeadSHA, BaseSHA: pr.BaseSHA,
		Files: pr.Files, Title: title, Patch: patch}
}

func TestPrepareUpdateFastForwards(t *testing.T) {
	cfg, remote, work, pr := openedPR(t)
	write(t, filepath.Join(work, filepath.FromSlash("apps/web/extra.yaml")), "a: b\n", 0o644)
	patch := edit(t, work, "apps/web/deployment.yaml", "memory: 512Mi\ncpu: 100m\n")

	c, err := PrepareUpdate(t.Context(), cfg, "", updateReq(pr, "fix(web): use 512Mi instead", patch))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(c.Close)
	if c.ParentSHA != pr.HeadSHA || c.HeadSHA == pr.HeadSHA {
		t.Fatalf("parent %s head %s, PR head %s", c.ParentSHA, c.HeadSHA, pr.HeadSHA)
	}
	if strings.Join(c.Files, ",") != "apps/web/deployment.yaml,apps/web/extra.yaml" {
		t.Errorf("PR files = %v, want both files against the base", c.Files)
	}
	if err := c.Push(t.Context()); err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(run(t, remote, "rev-parse", "agent/5")); got != c.HeadSHA {
		t.Errorf("agent/5 at %s, want %s", got, c.HeadSHA)
	}
	if got := strings.TrimSpace(run(t, remote, "rev-parse", "agent/5^")); got != pr.HeadSHA {
		t.Errorf("new commit's parent is %s, want the old head %s", got, pr.HeadSHA)
	}
	if got := run(t, remote, "show", "agent/5:apps/web/deployment.yaml"); got != "memory: 512Mi\ncpu: 100m\n" {
		t.Errorf("content = %q", got)
	}
	msg := run(t, remote, "log", "-1", "--format=%B", "agent/5")
	if !strings.HasPrefix(msg, "fix(web): use 512Mi instead\n\nProposed by the alert agent for incident #5.") {
		t.Errorf("commit message = %q", msg)
	}
}

func TestPrepareUpdateRefusals(t *testing.T) {
	cfg, remote, work, pr := openedPR(t)
	ctx := t.Context()

	// The same checks as a new PR: a path outside apps/ and infrastructure/,
	// and the hooks.
	bad := edit(t, work, "scripts/render.sh", "#!/bin/sh\nexit 0\n")
	var rej *Rejected
	if _, err := PrepareUpdate(ctx, cfg, "", updateReq(pr, "fix(web): x", bad)); !errors.As(err, &rej) {
		t.Errorf("scripts/ change: %v, want a refusal", err)
	}
	forbidden := edit(t, work, "apps/web/deployment.yaml", "memory: FORBIDDEN\n")
	if _, err := PrepareUpdate(ctx, cfg, "", updateReq(pr, "fix(web): x", forbidden)); !errors.As(err, &rej) || !strings.Contains(rej.Reason, "hooks") {
		t.Errorf("hook failure: %v", err)
	}
	if _, err := PrepareUpdate(ctx, cfg, "", UpdateRequest{IncidentID: 5, Branch: "main", Title: "fix(web): x", Patch: forbidden}); err == nil {
		t.Error("an update outside the incident's agent branch was accepted")
	}

	// Someone pushes to the branch: the recorded head is stale.
	other := filepath.Join(t.TempDir(), "other")
	run(t, filepath.Dir(other), "clone", "--quiet", "--branch", "agent/5", remote, other)
	write(t, filepath.Join(other, filepath.FromSlash("apps/web/deployment.yaml")), "memory: 300Mi\ncpu: 100m\n", 0o644)
	run(t, other, "commit", "--quiet", "-am", "by hand")
	run(t, other, "push", "--quiet", "origin", "agent/5")
	patch := edit(t, work, "apps/web/deployment.yaml", "memory: 512Mi\ncpu: 100m\n")
	if _, err := PrepareUpdate(ctx, cfg, "", updateReq(pr, "fix(web): 512Mi", patch)); !errors.As(err, &rej) || !strings.Contains(rej.Reason, "moved") {
		t.Errorf("moved branch: %v", err)
	}
}

func TestUpdatePushNeverForces(t *testing.T) {
	cfg, remote, work, pr := openedPR(t)
	patch := edit(t, work, "apps/web/deployment.yaml", "memory: 512Mi\ncpu: 100m\n")
	c, err := PrepareUpdate(t.Context(), cfg, "", updateReq(pr, "fix(web): 512Mi", patch))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(c.Close)

	// The branch moves between the clone and the push.
	other := filepath.Join(t.TempDir(), "other")
	run(t, filepath.Dir(other), "clone", "--quiet", "--branch", "agent/5", remote, other)
	write(t, filepath.Join(other, filepath.FromSlash("apps/web/extra.yaml")), "x: y\n", 0o644)
	run(t, other, "add", "-A")
	run(t, other, "commit", "--quiet", "-m", "by hand")
	run(t, other, "push", "--quiet", "origin", "agent/5")
	moved := strings.TrimSpace(run(t, remote, "rev-parse", "agent/5"))

	if err := c.Push(t.Context()); err == nil {
		t.Fatal("a non-fast-forward update was pushed")
	}
	if got := strings.TrimSpace(run(t, remote, "rev-parse", "agent/5")); got != moved {
		t.Errorf("agent/5 overwritten: %s, want %s", got, moved)
	}
}
