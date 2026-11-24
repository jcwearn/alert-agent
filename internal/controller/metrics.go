package controller

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
)

type metrics struct {
	registry  *prometheus.Registry
	webhooks  *prometheus.CounterVec
	incidents *prometheus.CounterVec
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
	}
	// Pre-create every series so a rate() over them works from the first
	// scrape rather than from the first occurrence.
	for _, o := range []string{"ok", "bad_request", "error"} {
		m.webhooks.WithLabelValues(o)
	}
	for _, e := range []string{"opened", "resolved"} {
		m.incidents.WithLabelValues(e)
	}
	m.registry.MustRegister(
		m.webhooks, m.incidents,
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
	)
	return m
}
