// Package actions makes the agent's Tier-A changes to the cluster: delete a
// pod a controller will replace, restart a rollout, re-run a CronJob, ask
// Flux to reconcile. It runs as the actor ServiceAccount (the controller
// impersonates it), so RBAC decides where it may act and the cluster's
// admission policy decides what it may change. This package adds the rules
// neither can express, and describes every call exactly for the audit trail.
//
// Nothing here decides whether an action may run without a human; that is
// the controller's guardrails.
package actions

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
)

// The Tier-A tools.
const (
	DeletePod      = "delete_pod"
	RolloutRestart = "rollout_restart"
	RerunCronJob   = "rerun_cronjob"
	FluxReconcile  = "flux_reconcile"
)

// Tools lists them in the order they are described to the agent.
var Tools = []string{DeletePod, RolloutRestart, RerunCronJob, FluxReconcile}

// IsTool reports whether name is a Tier-A tool.
func IsTool(name string) bool { return slices.Contains(Tools, name) }

// IsWorkloadTool reports whether a tool changes a workload, which an
// approval-only namespace allows only after a human approves. Asking Flux to
// reconcile is not one: Flux does it on its own within minutes.
func IsWorkloadTool(name string) bool { return name != FluxReconcile && IsTool(name) }

// Annotations the actions set. The cluster's admission policy allows the
// actor to change these and nothing else on an existing object.
const (
	RestartedAtAnnotation  = "kubectl.kubernetes.io/restartedAt"
	RequestedAtAnnotation  = "reconcile.fluxcd.io/requestedAt"
	instantiateAnnotation  = "cronjob.kubernetes.io/instantiate"
	rerunJobNameSuffixBase = "-medic-"
)

// Request is one action as the agent (or an approval) names it.
type Request struct {
	Tool      string `json:"tool"`
	Kind      string `json:"kind,omitempty"`
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
}

// Object names a Kubernetes object.
type Object struct {
	Kind      string `json:"kind"`
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
}

func (o Object) String() string { return o.Kind + " " + o.Namespace + "/" + o.Name }

// Result is what an action did, or would do on a dry run.
type Result struct {
	// Target is the workload the action is about, which the controller's
	// cooldown keys on: a pod's Deployment rather than the pod, whose name
	// changes when it is replaced.
	Target Object
	// Object is what the call acted on.
	Object Object
	// Call is the exact API call and its outcome, for the audit trail.
	Call string
}

// Refusal is an action this package or the cluster declined. Reason is
// written for the agent.
type Refusal struct {
	Reason string
	// Forbidden: the actor has no RBAC for it, so a human approval would be
	// refused too.
	Forbidden bool
}

func (r *Refusal) Error() string { return r.Reason }

func refuse(format string, args ...any) error { return &Refusal{Reason: fmt.Sprintf(format, args...)} }

// Options tune one execution.
type Options struct {
	// DryRun sends the call with dryRun=All: the API server checks RBAC,
	// existence and admission, and changes nothing.
	DryRun bool
	// IncidentID names a re-run Job, so a second attempt for the same
	// incident collides instead of starting another.
	IncidentID int64
}

// Executor performs actions with clients that act as the actor.
type Executor struct {
	Core    kubernetes.Interface
	Dynamic dynamic.Interface
	// Actor is the identity the clients act as, named in each call's record.
	Actor string
	Now   func() time.Time
}

// Workload kinds rollout_restart takes.
var restartKinds = []string{"Deployment", "StatefulSet", "DaemonSet"}

// Pod owners whose pods come back when deleted.
var replacingOwners = []string{"ReplicaSet", "StatefulSet", "DaemonSet", "Job"}

// fluxKinds are the objects flux_reconcile takes, at the versions this
// cluster serves.
var fluxKinds = map[string]schema.GroupVersionResource{
	"Kustomization":  {Group: "kustomize.toolkit.fluxcd.io", Version: "v1", Resource: "kustomizations"},
	"HelmRelease":    {Group: "helm.toolkit.fluxcd.io", Version: "v2", Resource: "helmreleases"},
	"GitRepository":  {Group: "source.toolkit.fluxcd.io", Version: "v1", Resource: "gitrepositories"},
	"HelmRepository": {Group: "source.toolkit.fluxcd.io", Version: "v1", Resource: "helmrepositories"},
	"OCIRepository":  {Group: "source.toolkit.fluxcd.io", Version: "v1", Resource: "ocirepositories"},
}

// FluxKinds lists the kinds flux_reconcile takes.
func FluxKinds() []string {
	out := make([]string, 0, len(fluxKinds))
	for k := range fluxKinds {
		out = append(out, k)
	}
	slices.Sort(out)
	return out
}

// RestartKinds lists the kinds rollout_restart takes.
func RestartKinds() []string { return slices.Clone(restartKinds) }

// Check validates a request's shape, before anything is read.
func Check(req Request) error {
	if !IsTool(req.Tool) {
		return refuse("unknown action %q; it must be one of %s", req.Tool, strings.Join(Tools, ", "))
	}
	if errs := validation.IsDNS1123Label(req.Namespace); len(errs) > 0 {
		return refuse("namespace %q is not valid", req.Namespace)
	}
	if errs := validation.IsDNS1123Subdomain(req.Name); len(errs) > 0 {
		return refuse("name %q is not valid", req.Name)
	}
	switch req.Tool {
	case RolloutRestart:
		if !slices.Contains(restartKinds, req.Kind) {
			return refuse("rollout_restart takes kind %s (got %q)", strings.Join(restartKinds, ", "), req.Kind)
		}
	case FluxReconcile:
		if _, ok := fluxKinds[req.Kind]; !ok {
			return refuse("flux_reconcile takes kind %s (got %q)", strings.Join(FluxKinds(), ", "), req.Kind)
		}
	}
	return nil
}

// Do performs req, or checks it with a server-side dry run.
func (e *Executor) Do(ctx context.Context, req Request, opts Options) (Result, error) {
	if err := Check(req); err != nil {
		return Result{}, err
	}
	switch req.Tool {
	case DeletePod:
		return e.deletePod(ctx, req, opts)
	case RolloutRestart:
		return e.rolloutRestart(ctx, req, opts)
	case RerunCronJob:
		return e.rerunCronJob(ctx, req, opts)
	default:
		return e.fluxReconcile(ctx, req, opts)
	}
}

func dryRun(opts Options) []string {
	if opts.DryRun {
		return []string{metav1.DryRunAll}
	}
	return nil
}

// suffix marks a dry run in the call record.
func (e *Executor) suffix(opts Options) string {
	s := " as " + e.Actor
	if opts.DryRun {
		s += " (dry run)"
	}
	return s
}

func (e *Executor) now() time.Time {
	if e.Now != nil {
		return e.Now()
	}
	return time.Now()
}

func (e *Executor) deletePod(ctx context.Context, req Request, opts Options) (Result, error) {
	obj := Object{Kind: "Pod", Namespace: req.Namespace, Name: req.Name}
	pods := e.Core.CoreV1().Pods(req.Namespace)
	pod, err := pods.Get(ctx, req.Name, metav1.GetOptions{})
	if err != nil {
		return Result{}, apiError(err, "read", obj)
	}
	owner := metav1.GetControllerOf(pod)
	if owner == nil {
		return Result{}, refuse("%s has no controller: deleting it would not bring it back", obj)
	}
	if !slices.Contains(replacingOwners, owner.Kind) {
		return Result{}, refuse("%s is managed by %s %s, which manages its own pods; delete_pod only takes pods owned by a %s",
			obj, owner.Kind, owner.Name, strings.Join(replacingOwners, ", "))
	}
	target, err := e.topOwner(ctx, req.Namespace, owner)
	if err != nil {
		return Result{}, err
	}

	uid := pod.UID
	err = pods.Delete(ctx, req.Name, metav1.DeleteOptions{
		Preconditions: &metav1.Preconditions{UID: &uid},
		DryRun:        dryRun(opts),
	})
	if err != nil {
		return Result{}, apiError(err, "delete", obj)
	}
	return Result{Target: target, Object: obj, Call: fmt.Sprintf("DELETE pods %s/%s uid=%s%s: deleted; owner %s",
		req.Namespace, req.Name, uid, e.suffix(opts), target)}, nil
}

// topOwner follows a pod's owner to the workload a human would name: a
// ReplicaSet's Deployment, a Job's CronJob.
func (e *Executor) topOwner(ctx context.Context, ns string, owner *metav1.OwnerReference) (Object, error) {
	obj := Object{Kind: owner.Kind, Namespace: ns, Name: owner.Name}
	var meta metav1.Object
	switch owner.Kind {
	case "ReplicaSet":
		rs, err := e.Core.AppsV1().ReplicaSets(ns).Get(ctx, owner.Name, metav1.GetOptions{})
		if err != nil {
			return obj, apiError(err, "read", obj)
		}
		meta = rs
	case "Job":
		job, err := e.Core.BatchV1().Jobs(ns).Get(ctx, owner.Name, metav1.GetOptions{})
		if err != nil {
			return obj, apiError(err, "read", obj)
		}
		meta = job
	default:
		return obj, nil
	}
	if up := metav1.GetControllerOf(meta); up != nil && (up.Kind == "Deployment" || up.Kind == "CronJob") {
		return Object{Kind: up.Kind, Namespace: ns, Name: up.Name}, nil
	}
	return obj, nil
}

func (e *Executor) rolloutRestart(ctx context.Context, req Request, opts Options) (Result, error) {
	obj := Object{Kind: req.Kind, Namespace: req.Namespace, Name: req.Name}
	at := e.now().UTC().Format(time.RFC3339)
	patch := fmt.Appendf(nil, `{"spec":{"template":{"metadata":{"annotations":{%q:%q}}}}}`, RestartedAtAnnotation, at)
	po := metav1.PatchOptions{DryRun: dryRun(opts)}
	apps := e.Core.AppsV1()

	var err error
	var resource string
	switch req.Kind {
	case "Deployment":
		resource = "deployments"
		_, err = apps.Deployments(req.Namespace).Patch(ctx, req.Name, types.MergePatchType, patch, po)
	case "StatefulSet":
		resource = "statefulsets"
		_, err = apps.StatefulSets(req.Namespace).Patch(ctx, req.Name, types.MergePatchType, patch, po)
	default:
		resource = "daemonsets"
		_, err = apps.DaemonSets(req.Namespace).Patch(ctx, req.Name, types.MergePatchType, patch, po)
	}
	if err != nil {
		return Result{}, apiError(err, "restart", obj)
	}
	return Result{Target: obj, Object: obj, Call: fmt.Sprintf("PATCH %s.apps %s/%s spec.template.metadata.annotations[%s]=%s%s: patched",
		resource, req.Namespace, req.Name, RestartedAtAnnotation, at, e.suffix(opts))}, nil
}

// RerunJobName is the Job rerun_cronjob creates for an incident: one per
// incident, so a repeat collides rather than starting a second.
func RerunJobName(cronJob string, incidentID int64) string {
	suffix := fmt.Sprintf("%s%d", rerunJobNameSuffixBase, incidentID)
	// Job names feed the job-name pod label, which is capped at 63.
	if limit := validation.DNS1123LabelMaxLength - len(suffix); len(cronJob) > limit {
		cronJob = strings.TrimRight(cronJob[:limit], "-.")
	}
	return cronJob + suffix
}

func (e *Executor) rerunCronJob(ctx context.Context, req Request, opts Options) (Result, error) {
	obj := Object{Kind: "CronJob", Namespace: req.Namespace, Name: req.Name}
	batch := e.Core.BatchV1()
	cj, err := batch.CronJobs(req.Namespace).Get(ctx, req.Name, metav1.GetOptions{})
	if err != nil {
		return Result{}, apiError(err, "read", obj)
	}
	jobs, err := batch.Jobs(req.Namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return Result{}, apiError(err, "list Jobs for", obj)
	}
	for i := range jobs.Items {
		j := &jobs.Items[i]
		if o := metav1.GetControllerOf(j); o != nil && o.UID == cj.UID && j.Status.Active > 0 {
			return Result{}, refuse("Job %s from %s is still running; re-running now would overlap it", j.Name, obj)
		}
	}

	name := RerunJobName(cj.Name, opts.IncidentID)
	annotations := map[string]string{instantiateAnnotation: "manual"}
	for k, v := range cj.Spec.JobTemplate.Annotations {
		annotations[k] = v
	}
	labels := map[string]string{}
	for k, v := range cj.Spec.JobTemplate.Labels {
		labels[k] = v
	}
	// The same owner reference `kubectl create job --from=cronjob/...` sets,
	// so the CronJob's history limits cover this Job too.
	controller := true
	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name: name, Namespace: req.Namespace, Labels: labels, Annotations: annotations,
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: batchv1.SchemeGroupVersion.String(), Kind: "CronJob",
				Name: cj.Name, UID: cj.UID, Controller: &controller,
			}},
		},
		Spec: *cj.Spec.JobTemplate.Spec.DeepCopy(),
	}
	_, err = batch.Jobs(req.Namespace).Create(ctx, job, metav1.CreateOptions{DryRun: dryRun(opts)})
	if apierrors.IsAlreadyExists(err) {
		return Result{}, refuse("%s was already re-run for this incident as Job %s", obj, name)
	}
	if err != nil {
		return Result{}, apiError(err, "re-run", obj)
	}
	return Result{Target: obj, Object: Object{Kind: "Job", Namespace: req.Namespace, Name: name},
		Call: fmt.Sprintf("CREATE jobs.batch %s/%s from %s (owner uid=%s)%s: created", req.Namespace, name, obj, cj.UID, e.suffix(opts))}, nil
}

func (e *Executor) fluxReconcile(ctx context.Context, req Request, opts Options) (Result, error) {
	obj := Object{Kind: req.Kind, Namespace: req.Namespace, Name: req.Name}
	gvr := fluxKinds[req.Kind]
	res := e.Dynamic.Resource(gvr).Namespace(req.Namespace)
	u, err := res.Get(ctx, req.Name, metav1.GetOptions{})
	if err != nil {
		return Result{}, apiError(err, "read", obj)
	}
	if suspended, _, _ := unstructured.NestedBool(u.Object, "spec", "suspend"); suspended {
		return Result{}, refuse("%s is suspended. Someone suspended it on purpose; resuming it is a human's call", obj)
	}
	at := e.now().UTC().Format(time.RFC3339Nano)
	patch := fmt.Appendf(nil, `{"metadata":{"annotations":{%q:%q}}}`, RequestedAtAnnotation, at)
	if _, err := res.Patch(ctx, req.Name, types.MergePatchType, patch, metav1.PatchOptions{DryRun: dryRun(opts)}); err != nil {
		return Result{}, apiError(err, "reconcile", obj)
	}
	return Result{Target: obj, Object: obj, Call: fmt.Sprintf("PATCH %s.%s %s/%s metadata.annotations[%s]=%s%s: patched",
		gvr.Resource, gvr.Group, req.Namespace, req.Name, RequestedAtAnnotation, at, e.suffix(opts))}, nil
}

// apiError turns the API server's answers the agent can act on into
// refusals; anything else is a failure the controller logs.
func apiError(err error, verb string, obj Object) error {
	var status apierrors.APIStatus
	msg := err.Error()
	if errors.As(err, &status) {
		msg = status.Status().Message
	}
	switch {
	case apierrors.IsNotFound(err):
		return refuse("%s does not exist", obj)
	// A policy denial is 422 Invalid for an update or delete and 403 for a
	// create; either way the message names the policy.
	case (apierrors.IsForbidden(err) || apierrors.IsInvalid(err)) && strings.Contains(msg, "ValidatingAdmissionPolicy"):
		return refuse("the cluster's admission policy refused to let the actor %s %s: %s", verb, obj, msg)
	case apierrors.IsForbidden(err):
		return &Refusal{Forbidden: true, Reason: fmt.Sprintf(
			"the actor is not permitted to %s %s: namespace %s is outside where the agent may act. A human has to do this, or propose a PR",
			verb, obj, obj.Namespace)}
	case apierrors.IsConflict(err):
		return refuse("%s changed while the action ran (it may already have been replaced); check it again before deciding", obj)
	}
	return fmt.Errorf("%s %s: %w", verb, obj, err)
}
