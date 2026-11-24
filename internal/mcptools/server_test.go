package mcptools

import (
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	"k8s.io/client-go/kubernetes/fake"
)

var now = time.Date(2026, 11, 24, 12, 0, 0, 0, time.UTC)

func pod(ns, name string, ready bool, restarts int64, waiting string) *unstructured.Unstructured {
	state := map[string]any{"running": map[string]any{}}
	if waiting != "" {
		state = map[string]any{"waiting": map[string]any{"reason": waiting}}
	}
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "v1", "kind": "Pod",
		"metadata": map[string]any{
			"name": name, "namespace": ns, "labels": map[string]any{"app": "calibre"},
			"creationTimestamp": now.Add(-3 * time.Hour).Format(time.RFC3339),
			"managedFields":     []any{map[string]any{"manager": "kubelet"}},
		},
		"spec": map[string]any{"nodeName": "k3s-02"},
		"status": map[string]any{"phase": "Running", "containerStatuses": []any{
			map[string]any{"name": "calibre", "ready": ready, "restartCount": restarts, "state": state},
		}},
	}}
}

func kustomization() *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "kustomize.toolkit.fluxcd.io/v1", "kind": "Kustomization",
		"metadata": map[string]any{"name": "apps", "namespace": "flux-system", "creationTimestamp": now.Add(-48 * time.Hour).Format(time.RFC3339)},
		"status": map[string]any{"conditions": []any{
			map[string]any{"type": "Ready", "status": "False", "reason": "HealthCheckFailed", "message": "deployment alert-agent not ready"},
		}},
	}}
}

func newDeps(t *testing.T, promAndLoki http.Handler) *Deps {
	t.Helper()
	podGVR := schema.GroupVersionResource{Version: "v1", Resource: "pods"}
	secretGVR := schema.GroupVersionResource{Version: "v1", Resource: "secrets"}
	nodeGVR := schema.GroupVersionResource{Version: "v1", Resource: "nodes"}
	ksGVR := schema.GroupVersionResource{Group: "kustomize.toolkit.fluxcd.io", Version: "v1", Resource: "kustomizations"}

	mapper := meta.NewDefaultRESTMapper(nil)
	mapper.Add(schema.GroupVersionKind{Version: "v1", Kind: "Pod"}, meta.RESTScopeNamespace)
	mapper.Add(schema.GroupVersionKind{Version: "v1", Kind: "Secret"}, meta.RESTScopeNamespace)
	mapper.Add(schema.GroupVersionKind{Version: "v1", Kind: "Node"}, meta.RESTScopeRoot)
	mapper.Add(schema.GroupVersionKind{Group: "kustomize.toolkit.fluxcd.io", Version: "v1", Kind: "Kustomization"}, meta.RESTScopeNamespace)

	dyn := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), map[schema.GroupVersionResource]string{
		podGVR: "PodList", secretGVR: "SecretList", nodeGVR: "NodeList", ksGVR: "KustomizationList",
	}, pod("ebooks", "calibre-0", false, 7, "CrashLoopBackOff"), pod("immich", "server-0", true, 0, ""), kustomization())

	core := fake.NewClientset(
		&corev1.Event{ObjectMeta: metav1.ObjectMeta{Name: "e1", Namespace: "ebooks"}, Type: "Warning", Reason: "BackOff",
			Message: "Back-off restarting failed container", Count: 12, LastTimestamp: metav1.NewTime(now.Add(-time.Minute)),
			InvolvedObject: corev1.ObjectReference{Kind: "Pod", Name: "calibre-0"}},
		&corev1.Event{ObjectMeta: metav1.ObjectMeta{Name: "e0", Namespace: "ebooks"}, Type: "Normal", Reason: "Pulled",
			Message: "Container image pulled", LastTimestamp: metav1.NewTime(now.Add(-time.Hour)),
			InvolvedObject: corev1.ObjectReference{Kind: "Pod", Name: "calibre-0"}},
	)

	srv := httptest.NewServer(promAndLoki)
	t.Cleanup(srv.Close)
	return &Deps{
		Dynamic: dyn, Mapper: mapper, Core: core,
		PrometheusURL: srv.URL, LokiURL: srv.URL, RepoDir: gitRepo(t), HTTP: srv.Client(),
		Now: func() time.Time { return now },
	}
}

func gitRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	run := func(args ...string) {
		cmd := exec.Command("git", append([]string{"-C", dir, "-c", "user.email=t@t", "-c", "user.name=t"}, args...)...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
	}
	run("init", "-q", "-b", "main")
	if err := os.MkdirAll(filepath.Join(dir, "apps", "ebooks"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "apps", "ebooks", "deployment.yaml"), []byte("memory: 512Mi\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run("add", ".")
	run("commit", "-q", "-m", "feat(ebooks): lower calibre memory limit")
	return dir
}

func session(t *testing.T, d *Deps) *mcp.ClientSession {
	t.Helper()
	ct, st := mcp.NewInMemoryTransports()
	server := NewServer(d, "test")
	ss, err := server.Connect(t.Context(), st, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ss.Close() })
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "test"}, nil).Connect(t.Context(), ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cs.Close() })
	return cs
}

func call(t *testing.T, cs *mcp.ClientSession, tool string, args map[string]any) (string, bool) {
	t.Helper()
	res, err := cs.CallTool(t.Context(), &mcp.CallToolParams{Name: tool, Arguments: args})
	if err != nil {
		t.Fatalf("%s: %v", tool, err)
	}
	var b strings.Builder
	for _, c := range res.Content {
		if tc, ok := c.(*mcp.TextContent); ok {
			b.WriteString(tc.Text)
		}
	}
	return b.String(), res.IsError
}

func TestToolsAreAllReadOnly(t *testing.T) {
	cs := session(t, newDeps(t, http.NotFoundHandler()))
	res, err := cs.ListTools(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Tools) != 8 {
		t.Errorf("want 8 tools, got %d", len(res.Tools))
	}
	for _, tool := range res.Tools {
		if tool.Annotations == nil || !tool.Annotations.ReadOnlyHint {
			t.Errorf("%s is not marked read-only", tool.Name)
		}
	}
}

func TestKubeGet(t *testing.T) {
	cs := session(t, newDeps(t, http.NotFoundHandler()))

	out, isErr := call(t, cs, "kube_get", map[string]any{"resource": "pods"})
	if isErr {
		t.Fatal(out)
	}
	for _, want := range []string{"ebooks\tcalibre-0\t3h", "ready 0/1", "restarts 7", "CrashLoopBackOff", "node k3s-02", "immich\tserver-0"} {
		if !strings.Contains(out, want) {
			t.Errorf("pod list missing %q:\n%s", want, out)
		}
	}

	out, _ = call(t, cs, "kube_get", map[string]any{"resource": "pod", "namespace": "ebooks"})
	if strings.Contains(out, "immich") {
		t.Errorf("namespace filter ignored:\n%s", out)
	}

	out, _ = call(t, cs, "kube_get", map[string]any{"resource": "pods", "namespace": "ebooks", "name": "calibre-0"})
	if !strings.Contains(out, "nodeName: k3s-02") || strings.Contains(out, "managedFields") {
		t.Errorf("yaml wrong (managedFields must be stripped):\n%s", out)
	}

	out, _ = call(t, cs, "kube_get", map[string]any{"resource": "kustomizations.kustomize.toolkit.fluxcd.io"})
	if !strings.Contains(out, "Ready=False (HealthCheckFailed): deployment alert-agent not ready") {
		t.Errorf("CRD condition not summarised:\n%s", out)
	}
}

func TestKubeGetRefusals(t *testing.T) {
	cs := session(t, newDeps(t, http.NotFoundHandler()))
	for name, args := range map[string]map[string]any{
		"secrets":                 {"resource": "secrets", "namespace": "alert-agent"},
		"secret singular":         {"resource": "secret"},
		"unknown resource":        {"resource": "widgets"},
		"named without namespace": {"resource": "pods", "name": "calibre-0"},
	} {
		t.Run(name, func(t *testing.T) {
			if out, isErr := call(t, cs, "kube_get", args); !isErr {
				t.Errorf("expected a tool error, got:\n%s", out)
			}
		})
	}
}

func TestKubeEvents(t *testing.T) {
	cs := session(t, newDeps(t, http.NotFoundHandler()))
	out, isErr := call(t, cs, "kube_events", map[string]any{"namespace": "ebooks"})
	if isErr {
		t.Fatal(out)
	}
	if !strings.Contains(out, "Warning BackOff pod/calibre-0: Back-off restarting failed container (x12)") {
		t.Errorf("event line wrong:\n%s", out)
	}
	if strings.Index(out, "BackOff") > strings.Index(out, "Pulled") {
		t.Error("events must be newest first")
	}
}

func TestKubeLogs(t *testing.T) {
	cs := session(t, newDeps(t, http.NotFoundHandler()))
	out, isErr := call(t, cs, "kube_logs", map[string]any{"namespace": "ebooks", "pod": "calibre-0", "previous": true})
	if isErr || out == "" {
		t.Fatalf("logs: %q %v", out, isErr)
	}
}

func TestPrometheusAndLoki(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/query", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("query") != `up{job="calibre"}` {
			http.Error(w, "bad query", http.StatusBadRequest)
			return
		}
		_, _ = w.Write([]byte(`{"status":"success","data":{"resultType":"vector","result":[{"metric":{"job":"calibre","instance":"a"},"value":[1800000000,"0"]}]}}`))
	})
	mux.HandleFunc("/api/v1/query_range", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"status":"success","data":{"resultType":"matrix","result":[{"metric":{"pod":"calibre-0"},"values":[[1800000000,"1"],[1800000060,"5"],[1800000120,"3"]]}]}}`))
	})
	mux.HandleFunc("/loki/api/v1/query_range", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("direction") != "backward" {
			http.Error(w, "want backward", http.StatusBadRequest)
			return
		}
		_, _ = w.Write([]byte(`{"status":"success","data":{"result":[{"stream":{"namespace":"ebooks","pod":"calibre-0","filename":"/x"},"values":[["1800000000000000000","old line"],["1800000060000000000","OOMKilled"]]}]}}`))
	})
	cs := session(t, newDeps(t, mux))

	out, isErr := call(t, cs, "prometheus_query", map[string]any{"query": `up{job="calibre"}`})
	if isErr || !strings.Contains(out, `{instance="a", job="calibre"} => 0`) {
		t.Errorf("instant query:\n%s", out)
	}
	out, isErr = call(t, cs, "prometheus_query_range", map[string]any{"query": "x", "start": "-1h"})
	if isErr || !strings.Contains(out, "points 3, min 1, max 5, last 3") {
		t.Errorf("range query:\n%s", out)
	}
	out, isErr = call(t, cs, "loki_query", map[string]any{"query": `{namespace="ebooks"}`, "since": "30m"})
	if isErr || strings.Index(out, "OOMKilled") > strings.Index(out, "old line") || strings.Contains(out, "filename") {
		t.Errorf("loki should be newest first with trimmed labels:\n%s", out)
	}
}

func TestGitTools(t *testing.T) {
	cs := session(t, newDeps(t, http.NotFoundHandler()))
	out, isErr := call(t, cs, "git_log", map[string]any{"path": "apps/ebooks"})
	if isErr || !strings.Contains(out, "lower calibre memory limit") {
		t.Fatalf("git_log:\n%s", out)
	}
	out, isErr = call(t, cs, "git_show", map[string]any{"rev": "HEAD"})
	if isErr || !strings.Contains(out, "+memory: 512Mi") {
		t.Errorf("git_show:\n%s", out)
	}
	for _, bad := range []map[string]any{
		{"rev": "--output=/tmp/x"},
		{"rev": "HEAD", "path": "../../etc/passwd"},
		{"rev": "HEAD; rm -rf /"},
	} {
		if out, isErr := call(t, cs, "git_show", bad); !isErr {
			t.Errorf("%v should be refused:\n%s", bad, out)
		}
	}
}
