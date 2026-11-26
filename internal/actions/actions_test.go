package actions

import (
	"errors"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	dynfake "k8s.io/client-go/dynamic/fake"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

var fixed = time.Date(2026, 11, 25, 20, 0, 0, 0, time.UTC)

func ctrlRef(kind, name string, uid types.UID) []metav1.OwnerReference {
	t := true
	return []metav1.OwnerReference{{Kind: kind, Name: name, UID: uid, Controller: &t}}
}

func newExecutor(objs ...runtime.Object) (*Executor, *fake.Clientset) {
	core := fake.NewClientset(objs...)
	return &Executor{Core: core, Actor: "alert-agent-actor", Now: func() time.Time { return fixed }}, core
}

func writes(cs *fake.Clientset) []k8stesting.Action {
	var out []k8stesting.Action
	for _, a := range cs.Actions() {
		switch a.GetVerb() {
		case "get", "list", "watch":
		default:
			out = append(out, a)
		}
	}
	return out
}

func refusal(t *testing.T, err error) *Refusal {
	t.Helper()
	var r *Refusal
	if !errors.As(err, &r) {
		t.Fatalf("want a refusal, got %v", err)
	}
	return r
}

func TestDeletePodResolvesDeployment(t *testing.T) {
	rs := &appsv1.ReplicaSet{ObjectMeta: metav1.ObjectMeta{Name: "web-7c9f", Namespace: "drill", OwnerReferences: ctrlRef("Deployment", "web", "d1")}}
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "web-7c9f-abcde", Namespace: "drill", UID: "p1", OwnerReferences: ctrlRef("ReplicaSet", "web-7c9f", "r1")}}
	e, cs := newExecutor(rs, pod)

	res, err := e.Do(t.Context(), Request{Tool: DeletePod, Namespace: "drill", Name: "web-7c9f-abcde"}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if res.Target != (Object{Kind: "Deployment", Namespace: "drill", Name: "web"}) {
		t.Errorf("target = %v", res.Target)
	}
	if !strings.Contains(res.Call, "DELETE pods drill/web-7c9f-abcde uid=p1 as alert-agent-actor: deleted; owner Deployment drill/web") {
		t.Errorf("call = %q", res.Call)
	}
	w := writes(cs)
	if len(w) != 1 {
		t.Fatalf("writes = %v", w)
	}
	del := w[0].(k8stesting.DeleteActionImpl)
	if uid := del.DeleteOptions.Preconditions.UID; uid == nil || *uid != "p1" {
		t.Errorf("delete has no UID precondition: %+v", del.DeleteOptions)
	}
	if len(del.DeleteOptions.DryRun) != 0 {
		t.Errorf("a real delete was sent as a dry run")
	}
}

func TestDeletePodDryRunAndCronJobOwner(t *testing.T) {
	job := &batchv1.Job{ObjectMeta: metav1.ObjectMeta{Name: "sync-29001", Namespace: "drill", OwnerReferences: ctrlRef("CronJob", "sync", "c1")}}
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "sync-29001-x", Namespace: "drill", UID: "p2", OwnerReferences: ctrlRef("Job", "sync-29001", "j1")}}
	e, cs := newExecutor(job, pod)

	res, err := e.Do(t.Context(), Request{Tool: DeletePod, Namespace: "drill", Name: "sync-29001-x"}, Options{DryRun: true})
	if err != nil {
		t.Fatal(err)
	}
	if res.Target.Kind != "CronJob" || res.Target.Name != "sync" {
		t.Errorf("target = %v", res.Target)
	}
	if !strings.HasSuffix(strings.SplitN(res.Call, ":", 2)[0], "(dry run)") {
		t.Errorf("call does not say dry run: %q", res.Call)
	}
	del := writes(cs)[0].(k8stesting.DeleteActionImpl)
	if len(del.DeleteOptions.DryRun) != 1 || del.DeleteOptions.DryRun[0] != metav1.DryRunAll {
		t.Errorf("dry run not sent: %+v", del.DeleteOptions)
	}
}

func TestDeletePodRefusesPodsNothingReplaces(t *testing.T) {
	bare := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "bare", Namespace: "drill"}}
	cnpg := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "db-1", Namespace: "drill", OwnerReferences: ctrlRef("Cluster", "db", "x")}}
	e, cs := newExecutor(bare, cnpg)
	for name, want := range map[string]string{"bare": "has no controller", "db-1": "managed by Cluster db"} {
		_, err := e.Do(t.Context(), Request{Tool: DeletePod, Namespace: "drill", Name: name}, Options{})
		if r := refusal(t, err); !strings.Contains(r.Reason, want) {
			t.Errorf("%s: %q", name, r.Reason)
		}
	}
	if w := writes(cs); len(w) != 0 {
		t.Errorf("refused deletes still wrote: %v", w)
	}
}

func TestRolloutRestartPatchesOnlyTheAnnotation(t *testing.T) {
	d := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: "drill"}}
	e, cs := newExecutor(d)
	res, err := e.Do(t.Context(), Request{Tool: RolloutRestart, Kind: "Deployment", Namespace: "drill", Name: "web"}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	p := writes(cs)[0].(k8stesting.PatchActionImpl)
	want := `{"spec":{"template":{"metadata":{"annotations":{"kubectl.kubernetes.io/restartedAt":"2026-11-25T20:00:00Z"}}}}}`
	if p.GetPatchType() != types.MergePatchType || string(p.GetPatch()) != want {
		t.Errorf("patch = %s %s", p.GetPatchType(), p.GetPatch())
	}
	if res.Target != (Object{Kind: "Deployment", Namespace: "drill", Name: "web"}) || !strings.Contains(res.Call, "PATCH deployments.apps drill/web") {
		t.Errorf("result = %+v", res)
	}
}

func TestRerunCronJob(t *testing.T) {
	cj := &batchv1.CronJob{
		ObjectMeta: metav1.ObjectMeta{Name: "report-sync", Namespace: "drill", UID: "c1"},
		Spec: batchv1.CronJobSpec{JobTemplate: batchv1.JobTemplateSpec{
			ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": "report-sync"}},
			Spec:       batchv1.JobSpec{Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "c", Image: "i"}}}}},
		}},
	}
	e, cs := newExecutor(cj)
	res, err := e.Do(t.Context(), Request{Tool: RerunCronJob, Namespace: "drill", Name: "report-sync"}, Options{IncidentID: 42})
	if err != nil {
		t.Fatal(err)
	}
	created := writes(cs)[0].(k8stesting.CreateActionImpl).GetObject().(*batchv1.Job)
	if created.Name != "report-sync-medic-42" || created.Annotations[instantiateAnnotation] != "manual" || created.Labels["app"] != "report-sync" {
		t.Errorf("job = %+v", created.ObjectMeta)
	}
	if o := metav1.GetControllerOf(created); o == nil || o.Kind != "CronJob" || o.UID != "c1" {
		t.Errorf("owner = %+v", created.OwnerReferences)
	}
	if res.Object.Name != "report-sync-medic-42" || res.Target.Kind != "CronJob" {
		t.Errorf("result = %+v", res)
	}

	// The same incident again collides with its own Job.
	_, err = e.Do(t.Context(), Request{Tool: RerunCronJob, Namespace: "drill", Name: "report-sync"}, Options{IncidentID: 42})
	if r := refusal(t, err); !strings.Contains(r.Reason, "already re-run") {
		t.Errorf("repeat: %q", r.Reason)
	}
}

func TestRerunCronJobRefusesWhileOneRuns(t *testing.T) {
	cj := &batchv1.CronJob{ObjectMeta: metav1.ObjectMeta{Name: "sync", Namespace: "drill", UID: "c1"}}
	running := &batchv1.Job{ObjectMeta: metav1.ObjectMeta{Name: "sync-1", Namespace: "drill", OwnerReferences: ctrlRef("CronJob", "sync", "c1")},
		Status: batchv1.JobStatus{Active: 1}}
	e, cs := newExecutor(cj, running)
	_, err := e.Do(t.Context(), Request{Tool: RerunCronJob, Namespace: "drill", Name: "sync"}, Options{IncidentID: 1})
	if r := refusal(t, err); !strings.Contains(r.Reason, "still running") {
		t.Errorf("%q", r.Reason)
	}
	if w := writes(cs); len(w) != 0 {
		t.Errorf("wrote: %v", w)
	}
}

func TestRerunJobNameFits(t *testing.T) {
	long := strings.Repeat("a", 70)
	if n := RerunJobName(long, 123456); len(n) > 63 || !strings.HasSuffix(n, "-medic-123456") {
		t.Errorf("%q (%d)", n, len(n))
	}
}

func fluxExecutor(objs ...runtime.Object) (*Executor, *dynfake.FakeDynamicClient) {
	scheme := runtime.NewScheme()
	kinds := map[schema.GroupVersionResource]string{}
	for kind, gvr := range fluxKinds {
		kinds[gvr] = kind + "List"
	}
	dyn := dynfake.NewSimpleDynamicClientWithCustomListKinds(scheme, kinds, objs...)
	return &Executor{Dynamic: dyn, Actor: "alert-agent-actor", Now: func() time.Time { return fixed }}, dyn
}

func helmRelease(suspend bool) *unstructured.Unstructured {
	u := &unstructured.Unstructured{}
	u.SetAPIVersion("helm.toolkit.fluxcd.io/v2")
	u.SetKind("HelmRelease")
	u.SetNamespace("loki")
	u.SetName("loki")
	_ = unstructured.SetNestedField(u.Object, suspend, "spec", "suspend")
	return u
}

func TestFluxReconcile(t *testing.T) {
	e, dyn := fluxExecutor(helmRelease(false))
	res, err := e.Do(t.Context(), Request{Tool: FluxReconcile, Kind: "HelmRelease", Namespace: "loki", Name: "loki"}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	var patch k8stesting.PatchActionImpl
	for _, a := range dyn.Actions() {
		if p, ok := a.(k8stesting.PatchActionImpl); ok {
			patch = p
		}
	}
	want := `{"metadata":{"annotations":{"reconcile.fluxcd.io/requestedAt":"2026-11-25T20:00:00Z"}}}`
	if string(patch.GetPatch()) != want {
		t.Errorf("patch = %s", patch.GetPatch())
	}
	if !strings.Contains(res.Call, "PATCH helmreleases.helm.toolkit.fluxcd.io loki/loki") {
		t.Errorf("call = %q", res.Call)
	}
}

func TestFluxReconcileRefusesSuspended(t *testing.T) {
	e, _ := fluxExecutor(helmRelease(true))
	_, err := e.Do(t.Context(), Request{Tool: FluxReconcile, Kind: "HelmRelease", Namespace: "loki", Name: "loki"}, Options{})
	if r := refusal(t, err); !strings.Contains(r.Reason, "suspended") {
		t.Errorf("%q", r.Reason)
	}
}

func TestCheck(t *testing.T) {
	for _, req := range []Request{
		{Tool: "scale", Namespace: "a", Name: "b"},
		{Tool: DeletePod, Namespace: "Bad_NS", Name: "b"},
		{Tool: DeletePod, Namespace: "a", Name: ""},
		{Tool: RolloutRestart, Kind: "ReplicaSet", Namespace: "a", Name: "b"},
		{Tool: FluxReconcile, Kind: "Bucket", Namespace: "a", Name: "b"},
	} {
		if err := Check(req); err == nil {
			t.Errorf("%+v accepted", req)
		}
	}
}

func TestAPIErrors(t *testing.T) {
	obj := Object{Kind: "Deployment", Namespace: "kube-system", Name: "coredns"}
	gr := schema.GroupResource{Group: "apps", Resource: "deployments"}

	r := refusal(t, apiError(apierrors.NewForbidden(gr, "coredns", errors.New("no RBAC")), "restart", obj))
	if !r.Forbidden || !strings.Contains(r.Reason, "outside where the agent may act") {
		t.Errorf("forbidden: %+v", r)
	}
	vap := apierrors.NewForbidden(gr, "coredns", errors.New(`ValidatingAdmissionPolicy 'alert-agent-actor' with binding 'alert-agent-actor' denied request: nope`))
	if r := refusal(t, apiError(vap, "restart", obj)); r.Forbidden || !strings.Contains(r.Reason, "admission policy") {
		t.Errorf("policy: %+v", r)
	}
	if r := refusal(t, apiError(apierrors.NewNotFound(gr, "coredns"), "restart", obj)); !strings.Contains(r.Reason, "does not exist") {
		t.Errorf("not found: %+v", r)
	}
	if err := apiError(errors.New("connection refused"), "restart", obj); errors.As(err, new(*Refusal)) {
		t.Errorf("a transport error became a refusal")
	}
}

func TestFluxReconcileWithoutSpec(t *testing.T) {
	u := &unstructured.Unstructured{}
	u.SetAPIVersion("kustomize.toolkit.fluxcd.io/v1")
	u.SetKind("Kustomization")
	u.SetNamespace("flux-system")
	u.SetName("apps")
	e, _ := fluxExecutor(u)
	if _, err := e.Do(t.Context(), Request{Tool: FluxReconcile, Kind: "Kustomization", Namespace: "flux-system", Name: "apps"}, Options{}); err != nil {
		t.Fatal(err)
	}
}
