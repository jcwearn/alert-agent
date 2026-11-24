package mcptools

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/yaml"
)

// KubeGetInput selects objects.
type KubeGetInput struct {
	Resource      string `json:"resource" jsonschema:"resource type: plural, singular, short name or resource.group, e.g. pods, deploy, kustomizations.kustomize.toolkit.fluxcd.io"`
	Namespace     string `json:"namespace,omitempty" jsonschema:"namespace; empty lists across all namespaces (ignored for cluster-scoped resources)"`
	Name          string `json:"name,omitempty" jsonschema:"object name; when set, the object is returned as YAML"`
	LabelSelector string `json:"label_selector,omitempty" jsonschema:"label selector, e.g. app=calibre"`
	Output        string `json:"output,omitempty" jsonschema:"summary (default for lists) or yaml"`
}

const maxListItems = 200

func (d *Deps) resolve(resource string) (schema.GroupVersionResource, bool, error) {
	var gvr schema.GroupVersionResource
	full, gr := schema.ParseResourceArg(strings.ToLower(strings.TrimSpace(resource)))
	var err error
	if full != nil {
		gvr, err = d.Mapper.ResourceFor(*full)
	}
	if full == nil || err != nil {
		gvr, err = d.Mapper.ResourceFor(gr.WithVersion(""))
	}
	if err != nil {
		return gvr, false, fmt.Errorf("unknown resource %q: %w", resource, err)
	}
	if gvr.Group == "" && gvr.Resource == "secrets" {
		return gvr, false, errors.New("secrets are not readable by this agent")
	}
	kind, err := d.Mapper.KindFor(gvr)
	if err != nil {
		return gvr, false, err
	}
	mapping, err := d.Mapper.RESTMapping(kind.GroupKind(), kind.Version)
	if err != nil {
		return gvr, false, err
	}
	return gvr, mapping.Scope.Name() == "namespace", nil
}

func (d *Deps) kubeGet(ctx context.Context, _ *mcp.CallToolRequest, in KubeGetInput) (*mcp.CallToolResult, any, error) {
	gvr, namespaced, err := d.resolve(in.Resource)
	if err != nil {
		return nil, nil, err
	}
	// An empty namespace means all namespaces for a list, and is what a
	// cluster-scoped resource needs anyway.
	ns := ""
	if namespaced {
		ns = in.Namespace
	}
	client := d.Dynamic.Resource(gvr).Namespace(ns)

	if in.Name != "" {
		if namespaced && in.Namespace == "" {
			return nil, nil, errors.New("namespace is required to get a namespaced object by name")
		}
		obj, err := client.Get(ctx, in.Name, metav1.GetOptions{})
		if err != nil {
			return nil, nil, err
		}
		if in.Output == "summary" {
			return text(summaryHeader(namespaced) + summaryLine(obj, namespaced, d.Now())), nil, nil
		}
		out, err := toYAML(obj)
		if err != nil {
			return nil, nil, err
		}
		return text(out), nil, nil
	}

	list, err := client.List(ctx, metav1.ListOptions{LabelSelector: in.LabelSelector, Limit: maxListItems})
	if err != nil {
		return nil, nil, err
	}
	if in.Output == "yaml" {
		var b strings.Builder
		for i := range list.Items {
			y, err := toYAML(&list.Items[i])
			if err != nil {
				return nil, nil, err
			}
			b.WriteString("---\n")
			b.WriteString(y)
		}
		return text(b.String()), nil, nil
	}
	var b strings.Builder
	b.WriteString(summaryHeader(namespaced))
	for i := range list.Items {
		b.WriteString(summaryLine(&list.Items[i], namespaced, d.Now()))
	}
	if list.GetContinue() != "" {
		fmt.Fprintf(&b, "… more than %d objects; narrow with namespace or label_selector\n", maxListItems)
	}
	if len(list.Items) == 0 {
		b.WriteString("(none)\n")
	}
	return text(b.String()), nil, nil
}

// toYAML drops the fields that are noise for a diagnosis.
func toYAML(obj *unstructured.Unstructured) (string, error) {
	o := obj.DeepCopy()
	unstructured.RemoveNestedField(o.Object, "metadata", "managedFields")
	if ann := o.GetAnnotations(); ann != nil {
		delete(ann, "kubectl.kubernetes.io/last-applied-configuration")
		o.SetAnnotations(ann)
	}
	b, err := yaml.Marshal(o.Object)
	return string(b), err
}

func summaryHeader(namespaced bool) string {
	if namespaced {
		return "NAMESPACE\tNAME\tAGE\tSTATUS\n"
	}
	return "NAME\tAGE\tSTATUS\n"
}

func summaryLine(o *unstructured.Unstructured, namespaced bool, now time.Time) string {
	age := shortDuration(now.Sub(o.GetCreationTimestamp().Time))
	status := statusOf(o)
	if namespaced {
		return fmt.Sprintf("%s\t%s\t%s\t%s\n", o.GetNamespace(), o.GetName(), age, status)
	}
	return fmt.Sprintf("%s\t%s\t%s\n", o.GetName(), age, status)
}

// statusOf picks the few status fields that say whether an object is healthy.
func statusOf(o *unstructured.Unstructured) string {
	var parts []string
	if o.GetKind() == "Pod" {
		phase, _, _ := unstructured.NestedString(o.Object, "status", "phase")
		parts = append(parts, phase)
		statuses, _, _ := unstructured.NestedSlice(o.Object, "status", "containerStatuses")
		ready, restarts := 0, int64(0)
		var waiting []string
		for _, s := range statuses {
			m, _ := s.(map[string]any)
			if r, _ := m["ready"].(bool); r {
				ready++
			}
			if n, ok := m["restartCount"].(int64); ok {
				restarts += n
			}
			if reason, _, _ := unstructured.NestedString(m, "state", "waiting", "reason"); reason != "" {
				waiting = append(waiting, reason)
			}
		}
		parts = append(parts, fmt.Sprintf("ready %d/%d", ready, len(statuses)), fmt.Sprintf("restarts %d", restarts))
		parts = append(parts, waiting...)
		if node, _, _ := unstructured.NestedString(o.Object, "spec", "nodeName"); node != "" {
			parts = append(parts, "node "+node)
		}
		return strings.Join(parts, ", ")
	}

	if replicas, ok, _ := unstructured.NestedInt64(o.Object, "spec", "replicas"); ok {
		ready, _, _ := unstructured.NestedInt64(o.Object, "status", "readyReplicas")
		parts = append(parts, fmt.Sprintf("ready %d/%d", ready, replicas))
	}
	conds, _, _ := unstructured.NestedSlice(o.Object, "status", "conditions")
	for _, want := range []string{"Ready", "Available", "Healthy", "Stalled"} {
		for _, c := range conds {
			m, _ := c.(map[string]any)
			if m["type"] != want {
				continue
			}
			s := fmt.Sprintf("%s=%v", want, m["status"])
			if r, _ := m["reason"].(string); r != "" {
				s += " (" + r + ")"
			}
			if msg, _ := m["message"].(string); msg != "" && m["status"] != "True" {
				s += ": " + clip(msg, 160)
			}
			parts = append(parts, s)
		}
	}
	if len(parts) == 0 {
		if phase, ok, _ := unstructured.NestedString(o.Object, "status", "phase"); ok {
			parts = append(parts, phase)
		}
	}
	return strings.Join(parts, ", ")
}

// KubeEventsInput selects events.
type KubeEventsInput struct {
	Namespace string `json:"namespace" jsonschema:"namespace to read events from"`
	Name      string `json:"name,omitempty" jsonschema:"only events about the object with this name"`
	Kind      string `json:"kind,omitempty" jsonschema:"only events about objects of this kind, e.g. Pod"`
	Limit     int    `json:"limit,omitempty" jsonschema:"maximum events to return (default 50, max 200)"`
}

func (d *Deps) kubeEvents(ctx context.Context, _ *mcp.CallToolRequest, in KubeEventsInput) (*mcp.CallToolResult, any, error) {
	sel := fields.Set{}
	if in.Name != "" {
		sel["involvedObject.name"] = in.Name
	}
	if in.Kind != "" {
		sel["involvedObject.kind"] = in.Kind
	}
	list, err := d.Core.CoreV1().Events(in.Namespace).List(ctx, metav1.ListOptions{FieldSelector: sel.AsSelector().String()})
	if err != nil {
		return nil, nil, err
	}
	events := list.Items
	sort.Slice(events, func(i, j int) bool { return eventTime(events[i]).After(eventTime(events[j])) })
	limit := clamp(in.Limit, 50, 200)
	if len(events) > limit {
		events = events[:limit]
	}
	var b strings.Builder
	for _, e := range events {
		fmt.Fprintf(&b, "%s %s %s %s/%s: %s", eventTime(e).UTC().Format(time.RFC3339), e.Type, e.Reason,
			strings.ToLower(e.InvolvedObject.Kind), e.InvolvedObject.Name, strings.TrimSpace(e.Message))
		if e.Count > 1 {
			fmt.Fprintf(&b, " (x%d)", e.Count)
		}
		b.WriteString("\n")
	}
	if len(events) == 0 {
		b.WriteString("(no events)\n")
	}
	return text(b.String()), nil, nil
}

func eventTime(e corev1.Event) time.Time {
	switch {
	case !e.EventTime.IsZero():
		return e.EventTime.Time
	case !e.LastTimestamp.IsZero():
		return e.LastTimestamp.Time
	default:
		return e.CreationTimestamp.Time
	}
}

// KubeLogsInput selects a container's logs.
type KubeLogsInput struct {
	Namespace    string `json:"namespace" jsonschema:"pod namespace"`
	Pod          string `json:"pod" jsonschema:"pod name"`
	Container    string `json:"container,omitempty" jsonschema:"container name; required when the pod has more than one"`
	Previous     bool   `json:"previous,omitempty" jsonschema:"logs of the previous, crashed instance"`
	TailLines    int64  `json:"tail_lines,omitempty" jsonschema:"lines from the end (default 200, max 2000)"`
	SinceSeconds int64  `json:"since_seconds,omitempty" jsonschema:"only lines newer than this many seconds"`
}

func (d *Deps) kubeLogs(ctx context.Context, _ *mcp.CallToolRequest, in KubeLogsInput) (*mcp.CallToolResult, any, error) {
	tail := int64(clamp(int(in.TailLines), 200, 2000))
	opts := &corev1.PodLogOptions{Container: in.Container, Previous: in.Previous, TailLines: &tail}
	if in.SinceSeconds > 0 {
		opts.SinceSeconds = &in.SinceSeconds
	}
	raw, err := d.Core.CoreV1().Pods(in.Namespace).GetLogs(in.Pod, opts).DoRaw(ctx)
	if err != nil {
		return nil, nil, err
	}
	return text(string(raw)), nil, nil
}

func clamp(v, def, maxV int) int {
	if v <= 0 {
		return def
	}
	if v > maxV {
		return maxV
	}
	return v
}

func clip(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > n {
		return s[:n] + "…"
	}
	return s
}

func shortDuration(d time.Duration) string {
	switch {
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd", int(d.Hours()/24))
	}
}
