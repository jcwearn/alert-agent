package controller

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
)

type metrics struct {
	registry  *prometheus.Registry
	webhooks  *prometheus.CounterVec
	incidents *prometheus.CounterVec
	runs      *prometheus.CounterVec
}

func newMetrics() *metrics {
	m := &metrics{
		registry: prometheus.NewRegistry(),
		webhooks: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "alert_agent_webhooks_total",
			Help: "Alertmanager webhook deliveries, by outcome.",
		}, []string{"outcome"}),
		incidents: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "alert_agent_incidents_total",
			Help: "Incident transitions recorded from webhooks.",
		}, []string{"event"}),
		runs: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "alert_agent_runs_total",
			Help: "Agent runs queued, and finished by outcome.",
		}, []string{"status"}),
	}
	// Pre-create every series so a rate() over them works from the first
	// scrape rather than from the first occurrence.
	for _, o := range []string{"ok", "bad_request", "error"} {
		m.webhooks.WithLabelValues(o)
	}
	for _, e := range []string{"opened", "resolved"} {
		m.incidents.WithLabelValues(e)
	}
	for _, st := range []string{"queued", "succeeded", "failed"} {
		m.runs.WithLabelValues(st)
	}
	m.registry.MustRegister(
		m.webhooks, m.incidents, m.runs,
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
	)
	return m
}
