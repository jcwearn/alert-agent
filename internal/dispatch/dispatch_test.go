package dispatch

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/jcwearn/alert-agent/internal/github"
	"github.com/jcwearn/alert-agent/internal/store"
)

type fakeStore struct {
	queued    []store.Run
	running   *store.Run
	started   int
	claimHash []byte
	finished  map[int64]store.RunResult
}

func (f *fakeStore) ClaimNextRun(_ context.Context, hash []byte, prefix, model string) (store.Run, bool, error) {
	if f.running != nil || len(f.queued) == 0 {
		return store.Run{}, false, nil
	}
	r := f.queued[0]
	f.queued = f.queued[1:]
	now := time.Now()
	r.Status, r.JobName, r.Model, r.StartedAt = store.RunRunning, prefix+"7", model, &now
	f.running, f.claimHash = &r, hash
	return r, true, nil
}

func (f *fakeStore) RunningRun(context.Context) (store.Run, bool, error) {
	if f.running == nil {
		return store.Run{}, false, nil
	}
	return *f.running, true, nil
}

func (f *fakeStore) RunsStartedSince(context.Context, time.Time) (int, error) { return f.started, nil }

func (f *fakeStore) FinishRun(_ context.Context, id int64, res store.RunResult) (bool, error) {
	if f.running == nil || f.running.ID != id {
		return false, nil
	}
	if f.finished == nil {
		f.finished = map[int64]store.RunResult{}
	}
	f.finished[id] = res
	f.running = nil
	return true, nil
}

type fakeMinter struct {
	repos []string
	perms map[string]string
	err   error
}

func (m *fakeMinter) InstallationToken(_ context.Context, repos []string, perms map[string]string) (github.Token, error) {
	m.repos, m.perms = repos, perms
	return github.Token{Token: "ghs_read"}, m.err
}

func testConfig() Config {
	return Config{
		Namespace: "alert-agent", Image: "ghcr.io/jcwearn/alert-agent:test", ServiceAccount: "alert-agent-runner",
		ClaudeSecret: "alert-agent-claude", ControllerURL: "http://alert-agent:8082", Repo: "jcwearn/k3s-cluster",
		Model: "opus", TZ: "UTC", Timeout: 20 * time.Minute, MaxRunsPerHour: 6, Interval: time.Second,
	}
}

func TestTickStartsRunner(t *testing.T) {
	st := &fakeStore{queued: []store.Run{{ID: 7, IncidentID: 3}}}
	kube := fake.NewClientset()
	gh := &fakeMinter{}
	d := New(testConfig(), st, kube, gh, nil)

	if err := d.Tick(t.Context()); err != nil {
		t.Fatal(err)
	}

	job, err := kube.BatchV1().Jobs("alert-agent").Get(t.Context(), "alert-agent-run-7", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("job not created: %v", err)
	}
	pod := job.Spec.Template.Spec
	if pod.ServiceAccountName != "alert-agent-runner" || *job.Spec.BackoffLimit != 0 || *job.Spec.ActiveDeadlineSeconds != 1200 {
		t.Errorf("job spec wrong: sa=%s backoff=%d deadline=%d", pod.ServiceAccountName, *job.Spec.BackoffLimit, *job.Spec.ActiveDeadlineSeconds)
	}
	if job.Spec.Template.Labels[LabelComponent] != ComponentValue {
		t.Error("runner pod lacks the component label the NetworkPolicy keys on")
	}
	c := pod.Containers[0]
	if !*c.SecurityContext.ReadOnlyRootFilesystem || *c.SecurityContext.AllowPrivilegeEscalation {
		t.Error("runner container is not locked down")
	}
	env := map[string]corev1.EnvVar{}
	for _, e := range c.Env {
		env[e.Name] = e
	}
	if ref := env["RUN_TOKEN"].ValueFrom; ref == nil || ref.SecretKeyRef.Name != "alert-agent-run-7" {
		t.Error("run token must come from the per-run Secret, not a literal")
	}
	if ref := env["CLAUDE_CODE_OAUTH_TOKEN"].ValueFrom; ref == nil || ref.SecretKeyRef.Name != "alert-agent-claude" {
		t.Error("claude token must come from the claude Secret")
	}
	for name, e := range env {
		if e.Value != "" && (name == "RUN_TOKEN" || name == "GITHUB_TOKEN") {
			t.Errorf("%s set as a literal in the Job spec", name)
		}
	}

	sec, err := kube.CoreV1().Secrets("alert-agent").Get(t.Context(), "alert-agent-run-7", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("secret not created: %v", err)
	}
	runToken := string(sec.Data[secretRunToken])
	if !bytes.Equal(HashToken(runToken), st.claimHash) {
		t.Error("stored hash does not match the token handed to the runner")
	}
	if len(sec.OwnerReferences) != 1 || sec.OwnerReferences[0].Kind != "Job" {
		t.Error("secret is not owned by its Job")
	}

	if len(gh.repos) != 1 || gh.repos[0] != "k3s-cluster" || gh.perms["contents"] != "read" || len(gh.perms) != 1 {
		t.Errorf("github token not narrowed to contents:read on one repo: %v %v", gh.repos, gh.perms)
	}
}

func TestTickRespectsHourlyCap(t *testing.T) {
	st := &fakeStore{queued: []store.Run{{ID: 7}}, started: 6}
	kube := fake.NewClientset()
	d := New(testConfig(), st, kube, &fakeMinter{}, nil)

	if err := d.Tick(t.Context()); err != nil {
		t.Fatal(err)
	}
	if st.running != nil {
		t.Fatal("run started past the hourly cap")
	}
}

func TestTickFailsRunWhenTokenMintFails(t *testing.T) {
	st := &fakeStore{queued: []store.Run{{ID: 7}}}
	var notified int64
	d := New(testConfig(), st, fake.NewClientset(), &fakeMinter{err: errors.New("github down")}, func(id int64) { notified = id })

	if err := d.Tick(t.Context()); err == nil {
		t.Fatal("expected an error")
	}
	if res, ok := st.finished[7]; !ok || res.Status != store.RunFailed {
		t.Fatalf("run not failed: %+v", st.finished)
	}
	if notified != 7 {
		t.Error("failure not reported")
	}
}

func TestCheckRunningFailsDeadJob(t *testing.T) {
	cases := map[string]struct {
		job     *batchv1.Job
		started time.Duration
		failed  bool
	}{
		"job failed": {
			job:     jobWith(batchv1.JobCondition{Type: batchv1.JobFailed, Status: corev1.ConditionTrue, Reason: "DeadlineExceeded"}),
			started: time.Minute, failed: true,
		},
		"job complete, report overdue": {
			job: jobWith(batchv1.JobCondition{Type: batchv1.JobComplete, Status: corev1.ConditionTrue,
				LastTransitionTime: metav1.NewTime(time.Now().Add(-time.Minute))}),
			started: time.Minute, failed: true,
		},
		"job complete, report may still arrive": {
			job: jobWith(batchv1.JobCondition{Type: batchv1.JobComplete, Status: corev1.ConditionTrue,
				LastTransitionTime: metav1.NewTime(time.Now())}),
			started: time.Minute,
		},
		"job still running":   {job: jobWith(), started: time.Minute},
		"job missing":         {started: 2 * time.Minute, failed: true},
		"job not created yet": {started: 10 * time.Second},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			started := time.Now().Add(-tc.started)
			st := &fakeStore{running: &store.Run{ID: 7, JobName: "alert-agent-run-7", Status: store.RunRunning, StartedAt: &started}}
			kube := fake.NewClientset()
			if tc.job != nil {
				if _, err := kube.BatchV1().Jobs("alert-agent").Create(t.Context(), tc.job, metav1.CreateOptions{}); err != nil {
					t.Fatal(err)
				}
			}
			d := New(testConfig(), st, kube, &fakeMinter{}, nil)
			if err := d.Tick(t.Context()); err != nil {
				t.Fatal(err)
			}
			_, failed := st.finished[7]
			if failed != tc.failed {
				t.Errorf("failed = %v, want %v (%+v)", failed, tc.failed, st.finished)
			}
		})
	}
}

func jobWith(conds ...batchv1.JobCondition) *batchv1.Job {
	return &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{Name: "alert-agent-run-7", Namespace: "alert-agent"},
		Status:     batchv1.JobStatus{Conditions: conds},
	}
}
