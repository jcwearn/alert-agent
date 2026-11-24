package proposal

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestRealRepositoryHooks runs proposals through the GitOps repository's own
// .githooks, which need bash, yq, kustomize and flux on PATH. It is skipped
// unless ALERT_AGENT_K3S_REMOTE names a clone of that repository (a bare
// clone works). ALERT_AGENT_PUBLIC_GATE, comma-separated, supplies the exact
// strings for .publicgate-local; it is never written into this file.
//
//	gh repo clone jcwearn/k3s-cluster /tmp/k3s.git -- --bare
//	ALERT_AGENT_K3S_REMOTE=/tmp/k3s.git go test ./internal/proposal -run RealRepository -v
func TestRealRepositoryHooks(t *testing.T) {
	remote := os.Getenv("ALERT_AGENT_K3S_REMOTE")
	if remote == "" {
		t.Skip("ALERT_AGENT_K3S_REMOTE not set")
	}
	var gate []string
	for _, v := range strings.Split(os.Getenv("ALERT_AGENT_PUBLIC_GATE"), ",") {
		if v = strings.TrimSpace(v); v != "" {
			gate = append(gate, v)
		}
	}
	root := t.TempDir()
	work := filepath.Join(root, "work")
	run(t, root, "clone", "--quiet", "--depth", "1", "--branch", "main", "file://"+remote, work)
	cfg := Config{
		Repo: "jcwearn/k3s-cluster", RemoteURL: "file://" + remote, WorkDir: filepath.Join(root, "proposals"),
		Path: os.Getenv("PATH"), PublicGate: gate,
		AuthorName: "alert-agent[bot]", AuthorEmail: "alert-agent[bot]@users.noreply.github.com",
	}
	dep := "apps/homepage/deployment.yaml"
	orig, err := os.ReadFile(filepath.Join(work, dep))
	if err != nil {
		t.Fatal(err)
	}
	change := func(old, new string) string {
		t.Helper()
		if !strings.Contains(string(orig), old) {
			t.Fatalf("%s no longer contains %q", dep, old)
		}
		return edit(t, work, dep, strings.Replace(string(orig), old, new, 1))
	}

	t.Run("valid change passes the hooks", func(t *testing.T) {
		c, err := Prepare(t.Context(), cfg, "", Request{IncidentID: 1, Title: "fix(homepage): raise memory limit to 384Mi",
			Patch: change(`memory: "256Mi"`, `memory: "384Mi"`)})
		if err != nil {
			t.Fatal(err)
		}
		c.Close()
	})

	refused := []struct {
		name, title, old, new, want string
	}{
		{"overlay that does not render", "fix(homepage): y", "resources:", "resources: [", "commit hooks refused"},
		{"literal LAN address", "fix(homepage): y", `memory: "256Mi"`, "memory: \"256Mi\"\n              # see 192.168.100.200", "literal private value"},
		{"tailnet name", "fix(homepage): y", `memory: "256Mi"`, "memory: \"256Mi\"\n              # k3s-gateway.tail1234.ts.net", "literal private value"},
		{"private address in the title", "fix(homepage): point at 192.168.100.20", `memory: "256Mi"`, `memory: "384Mi"`, "commit-msg"},
	}
	if len(gate) > 0 {
		refused = append(refused, struct{ name, title, old, new, want string }{
			"public gate value", "fix(homepage): y", `memory: "256Mi"`, "memory: \"256Mi\"\n              # " + gate[0], ".publicgate-local"})
	}
	for _, c := range refused {
		t.Run(c.name, func(t *testing.T) {
			commit, err := Prepare(t.Context(), cfg, "", Request{IncidentID: 1, Title: c.title, Patch: change(c.old, c.new)})
			if err == nil {
				commit.Close()
				t.Fatal("accepted")
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Errorf("error does not mention %q:\n%v", c.want, err)
			}
		})
	}
}
