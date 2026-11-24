package main

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

func TestRunnerImageFromOwnPod(t *testing.T) {
	kube := fake.NewClientset(&corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "alert-agent-abc", Namespace: "alert-agent"},
		Spec: corev1.PodSpec{Containers: []corev1.Container{
			{Name: "controller", Image: "ghcr.io/jcwearn/alert-agent:0.3.0@sha256:abc"},
		}},
	})
	t.Setenv("RUNNER_IMAGE", "")
	t.Setenv("POD_NAME", "alert-agent-abc")
	img, err := runnerImage(kube, "alert-agent")
	if err != nil || img != "ghcr.io/jcwearn/alert-agent:0.3.0@sha256:abc" {
		t.Fatalf("image = %q, err = %v", img, err)
	}

	t.Setenv("RUNNER_IMAGE", "override:1")
	if img, _ := runnerImage(kube, "alert-agent"); img != "override:1" {
		t.Errorf("RUNNER_IMAGE should win, got %q", img)
	}

	t.Setenv("RUNNER_IMAGE", "")
	t.Setenv("POD_NAME", "")
	if _, err := runnerImage(kube, "alert-agent"); err == nil {
		t.Error("no RUNNER_IMAGE and no POD_NAME should fail")
	}
}
