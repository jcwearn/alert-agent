package dispatch

import (
	"strconv"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/jcwearn/alert-agent/internal/store"
)

// Labels on every runner pod. The controller's NetworkPolicy admits the
// runner API port from pods carrying the component label.
const (
	LabelName      = "app.kubernetes.io/name"
	LabelComponent = "app.kubernetes.io/component"
	LabelRunID     = "alert-agent/run-id"
	ComponentValue = "runner"
)

// Keys in the per-run Secret.
const (
	secretRunToken    = "RUN_TOKEN"
	secretGitHubToken = "GITHUB_TOKEN"
)

func runLabels(run store.Run) map[string]string {
	return map[string]string{
		LabelName:      "alert-agent",
		LabelComponent: ComponentValue,
		LabelRunID:     strconv.FormatInt(run.ID, 10),
	}
}

// buildSecret holds the two per-run credentials. They live in a Secret, not
// in the Job's env, because the runner's ServiceAccount can read Jobs
// cluster-wide but no Secrets at all.
func buildSecret(cfg Config, run store.Run, runToken, githubToken string) *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      run.JobName,
			Namespace: cfg.Namespace,
			Labels:    runLabels(run),
		},
		Type: corev1.SecretTypeOpaque,
		Data: map[string][]byte{
			secretRunToken:    []byte(runToken),
			secretGitHubToken: []byte(githubToken),
		},
	}
}

func buildJob(cfg Config, run store.Run) *batchv1.Job {
	labels := runLabels(run)
	deadline := int64(cfg.Timeout.Seconds())
	backoff := int32(0)
	ttl := int32(86400) // keep the finished pod's logs for a day
	nonRoot := true
	uid := int64(65532)
	noEscalation := false
	readOnly := true
	automount := true // kubectl inside the pod uses the runner ServiceAccount

	secretEnv := func(name, secret, key string) corev1.EnvVar {
		return corev1.EnvVar{Name: name, ValueFrom: &corev1.EnvVarSource{
			SecretKeyRef: &corev1.SecretKeySelector{
				LocalObjectReference: corev1.LocalObjectReference{Name: secret},
				Key:                  key,
			},
		}}
	}
	emptyDir := func(name string) corev1.Volume {
		return corev1.Volume{Name: name, VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}}
	}

	return &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      run.JobName,
			Namespace: cfg.Namespace,
			Labels:    labels,
		},
		Spec: batchv1.JobSpec{
			BackoffLimit:            &backoff,
			ActiveDeadlineSeconds:   &deadline,
			TTLSecondsAfterFinished: &ttl,
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: labels},
				Spec: corev1.PodSpec{
					RestartPolicy:                corev1.RestartPolicyNever,
					ServiceAccountName:           cfg.ServiceAccount,
					AutomountServiceAccountToken: &automount,
					SecurityContext: &corev1.PodSecurityContext{
						RunAsNonRoot:   &nonRoot,
						RunAsUser:      &uid,
						RunAsGroup:     &uid,
						FSGroup:        &uid,
						SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
					},
					Containers: []corev1.Container{{
						Name:  "runner",
						Image: cfg.Image,
						Args:  []string{"runner"},
						Env: []corev1.EnvVar{
							{Name: "CONTROLLER_URL", Value: cfg.ControllerURL},
							{Name: "REPO", Value: cfg.Repo},
							{Name: "TZ", Value: cfg.TZ},
							secretEnv("RUN_TOKEN", run.JobName, secretRunToken),
							secretEnv("GITHUB_TOKEN", run.JobName, secretGitHubToken),
							secretEnv("CLAUDE_CODE_OAUTH_TOKEN", cfg.ClaudeSecret, "CLAUDE_CODE_OAUTH_TOKEN"),
						},
						VolumeMounts: []corev1.VolumeMount{
							{Name: "home", MountPath: "/home/nonroot"},
							{Name: "tmp", MountPath: "/tmp"},
							{Name: "work", MountPath: "/work"},
						},
						SecurityContext: &corev1.SecurityContext{
							AllowPrivilegeEscalation: &noEscalation,
							ReadOnlyRootFilesystem:   &readOnly,
							Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
						},
						Resources: corev1.ResourceRequirements{
							Requests: corev1.ResourceList{
								corev1.ResourceCPU:    resource.MustParse("100m"),
								corev1.ResourceMemory: resource.MustParse("512Mi"),
							},
							Limits: corev1.ResourceList{
								corev1.ResourceCPU:    resource.MustParse("2"),
								corev1.ResourceMemory: resource.MustParse("2Gi"),
							},
						},
					}},
					Volumes: []corev1.Volume{emptyDir("home"), emptyDir("tmp"), emptyDir("work")},
				},
			},
		},
	}
}
