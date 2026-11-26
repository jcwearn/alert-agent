package preview

import (
	"cmp"
	"context"
	"slices"
	"strings"
	"time"

	"github.com/jcwearn/alert-agent/internal/store"
)

// pageSize is the preview's home page size: small, so the fixtures fill
// several pages and families land on page boundaries.
const pageSize = 10

// history is six weeks of older incidents (#1-#21), for the home page's
// pages and filters: several namespaces and severities, three merged
// families (one child opened before its parent, one child in another
// namespace than its parent), and one still firing.
func history(now time.Time) []store.Incident {
	day := 24 * time.Hour
	at := func(d time.Duration) time.Time { return now.Add(-d) }
	in := func(id int64, name, ns, sev, summary string, age time.Duration, parent int64) store.Incident {
		x := store.Incident{ID: id, AlertName: name, Namespace: ns, Severity: sev, Summary: summary,
			AlertStatus: "resolved", OpenedAt: at(age), AlertCount: 1}
		x.ResolvedAt = ptr(x.OpenedAt.Add(40 * time.Minute))
		if parent != 0 {
			x.ParentID, x.MergedAt, x.MergedBy = ptr(parent), ptr(x.OpenedAt.Add(2*time.Minute)), "run:1"
			x.MergeReason = "A symptom of the same outage."
		}
		return x
	}
	list := []store.Incident{
		in(21, "KubeJobFailed", "ansible", "warning", "ansible/k3s-patch-29371 failed", 34*time.Hour, 0),
		in(20, "EnvoyUpstreamErrors", "gateway", "warning", "5xx from the books route while calibre-web had no replicas", 2*day, 19),
		in(19, "KubeDeploymentReplicasMismatch", "books", "warning", "books/calibre-web has 0 of 1 replicas", 2*day+5*time.Minute, 0),
		in(18, "CertificateExpiringSoon", "cert-manager", "warning", "wildcard-tls expires in 13 days", 3*day, 0),
		in(17, "KubeContainerOOMKilled", "photos", "critical", "ml-worker was OOMKilled twice", 4*day, 0),
		in(16, "LokiRequestErrors", "loki", "warning", "5% of push requests failing", 5*day, 0),
		in(15, "NodeHighCPU", "monitoring", "warning", "node-a CPU over 90% for 15 minutes", 6*day, 0),
		in(14, "KubePodCrashLooping", "paperless", "warning", "paperless/consumer is crash looping", 8*day, 0),
		in(13, "KubePodNotReady", "paperless", "warning", "paperless/consumer not ready", 8*day+10*time.Minute, 14),
		in(12, "TargetDown", "monitoring", "warning", "1 of 3 kubelet targets down", 10*day, 0),
		in(11, "KubePersistentVolumeFillingUp", "immich", "critical", "immich/library is 96% full", 12*day, 0),
		in(10, "EnvoyHighLatency", "gateway", "warning", "p99 latency over 2s for 10 minutes", 14*day, 0),
		in(9, "KubePodCrashLooping", "jellyfin", "warning", "jellyfin/jellyfin is crash looping", 17*day, 0),
		in(8, "CPUThrottlingHigh", "jellyfin", "info", "jellyfin throttled 40% of the time", 18*day, 0),
		in(7, "KubeJobFailed", "renovate", "warning", "renovate/renovate-29360 failed", 21*day, 0),
		in(6, "FluxReconciliationFailure", "flux-system", "critical", "Kustomization apps failed to reconcile", 24*day, 0),
		in(5, "KubeDeploymentReplicasMismatch", "n8n", "warning", "n8n/n8n has 0 of 1 replicas", 27*day, 0),
		in(4, "KubePodNotReady", "n8n", "warning", "n8n/n8n-5b7f not ready", 27*day+3*time.Minute, 5),
		in(3, "NodeFilesystemAlmostFull", "monitoring", "warning", "node-c /var at 90%", 33*day, 0),
		in(2, "KubeContainerOOMKilled", "immich", "critical", "immich-server was OOMKilled", 39*day, 0),
		in(1, "TargetDown", "monitoring", "critical", "all node-exporter targets down", 45*day, 0),
	}
	// Still filling up: a family the "firing" filter finds on page 2.
	list[10].AlertStatus, list[10].ResolvedAt = "firing", nil
	return list
}

func (f *fixtures) ListIncidentPage(_ context.Context, flt store.IncidentFilter, before, after *store.Cursor, limit int) (store.IncidentListPage, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	children := map[int64][]store.Incident{}
	var roots []store.Incident
	for _, in := range f.incidents {
		if in.ParentID != nil {
			children[*in.ParentID] = append(children[*in.ParentID], in)
		} else {
			roots = append(roots, in)
		}
	}
	newest := func(a, b store.Incident) int {
		return cmp.Or(b.OpenedAt.Compare(a.OpenedAt), cmp.Compare(b.ID, a.ID))
	}
	slices.SortFunc(roots, newest)
	roots = slices.DeleteFunc(roots, func(r store.Incident) bool { return !familyMatches(r, children[r.ID], flt) })

	// Positions are indexes into the filtered, newest-first roots.
	firstWhere := func(ok func(store.Incident) bool) int {
		if i := slices.IndexFunc(roots, ok); i >= 0 {
			return i
		}
		return len(roots)
	}
	lo, hi := 0, min(limit, len(roots))
	switch {
	case before != nil:
		lo = firstWhere(func(r store.Incident) bool { return olderThan(r, *before) })
		hi = min(lo+limit, len(roots))
	case after != nil:
		hi = firstWhere(func(r store.Incident) bool { return !newerThan(r, *after) })
		lo = max(hi-limit, 0)
	}
	var page store.IncidentListPage
	if lo >= hi {
		return page, nil
	}
	if lo > 0 {
		page.Newer = &store.Cursor{OpenedAt: roots[lo].OpenedAt, ID: roots[lo].ID}
	}
	if hi < len(roots) {
		page.Older = &store.Cursor{OpenedAt: roots[hi-1].OpenedAt, ID: roots[hi-1].ID}
	}
	page.Incidents = slices.Clone(roots[lo:hi])
	for _, r := range roots[lo:hi] {
		kids := slices.Clone(children[r.ID])
		slices.SortFunc(kids, newest)
		page.Incidents = append(page.Incidents, kids...)
	}
	return page, nil
}

func olderThan(r store.Incident, c store.Cursor) bool {
	return r.OpenedAt.Before(c.OpenedAt) || (r.OpenedAt.Equal(c.OpenedAt) && r.ID < c.ID)
}

func newerThan(r store.Incident, c store.Cursor) bool {
	return r.OpenedAt.After(c.OpenedAt) || (r.OpenedAt.Equal(c.OpenedAt) && r.ID > c.ID)
}

// familyMatches is the store's filter semantics: status rolls up the
// family, and the member filters must hold together on one member.
func familyMatches(root store.Incident, kids []store.Incident, f store.IncidentFilter) bool {
	if !f.From.IsZero() && root.OpenedAt.Before(f.From) || !f.To.IsZero() && !root.OpenedAt.Before(f.To) {
		return false
	}
	members := append([]store.Incident{root}, kids...)
	firing := slices.ContainsFunc(members, func(m store.Incident) bool { return m.AlertStatus == "firing" })
	if f.Status == "firing" && !firing || f.Status == "resolved" && firing {
		return false
	}
	return slices.ContainsFunc(members, func(m store.Incident) bool {
		return (f.Namespace == "" || m.Namespace == f.Namespace) && (f.Severity == "" || m.Severity == f.Severity) &&
			(f.Search == "" || strings.Contains(strings.ToLower(m.AlertName), strings.ToLower(f.Search)))
	})
}

func (f *fixtures) IncidentFacets(context.Context) (namespaces, severities []string, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var ns, sev []string
	for _, in := range f.incidents {
		if in.Namespace != "" && !slices.Contains(ns, in.Namespace) {
			ns = append(ns, in.Namespace)
		}
		if in.Severity != "" && !slices.Contains(sev, in.Severity) {
			sev = append(sev, in.Severity)
		}
	}
	slices.Sort(ns)
	slices.Sort(sev)
	return ns, sev, nil
}
