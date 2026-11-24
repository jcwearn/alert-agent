// Package alertmanager holds the webhook payload Alertmanager posts to a
// webhook_configs receiver (payload version 4).
package alertmanager

import (
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// Status values Alertmanager uses for both the group and each alert.
const (
	StatusFiring   = "firing"
	StatusResolved = "resolved"
)

// Payload is one webhook delivery: every alert currently in one group.
type Payload struct {
	Version           string            `json:"version"`
	GroupKey          string            `json:"groupKey"`
	TruncatedAlerts   int               `json:"truncatedAlerts"`
	Status            string            `json:"status"`
	Receiver          string            `json:"receiver"`
	GroupLabels       map[string]string `json:"groupLabels"`
	CommonLabels      map[string]string `json:"commonLabels"`
	CommonAnnotations map[string]string `json:"commonAnnotations"`
	ExternalURL       string            `json:"externalURL"`
	Alerts            []Alert           `json:"alerts"`
}

// Alert is one alert within a group.
type Alert struct {
	Status       string            `json:"status"`
	Labels       map[string]string `json:"labels"`
	Annotations  map[string]string `json:"annotations"`
	StartsAt     time.Time         `json:"startsAt"`
	EndsAt       time.Time         `json:"endsAt"`
	GeneratorURL string            `json:"generatorURL"`
	Fingerprint  string            `json:"fingerprint"`
}

// Parse decodes and validates a webhook body.
func Parse(body []byte) (Payload, error) {
	var p Payload
	if err := json.Unmarshal(body, &p); err != nil {
		return Payload{}, fmt.Errorf("decode payload: %w", err)
	}
	if p.Version != "4" {
		return Payload{}, fmt.Errorf("unsupported payload version %q", p.Version)
	}
	if p.GroupKey == "" {
		return Payload{}, errors.New("payload has no groupKey")
	}
	if p.Status != StatusFiring && p.Status != StatusResolved {
		return Payload{}, fmt.Errorf("unknown group status %q", p.Status)
	}
	for i, a := range p.Alerts {
		if a.Fingerprint == "" {
			return Payload{}, fmt.Errorf("alert %d has no fingerprint", i)
		}
		if a.Status != StatusFiring && a.Status != StatusResolved {
			return Payload{}, fmt.Errorf("alert %d: unknown status %q", i, a.Status)
		}
	}
	return p, nil
}

// Label returns a label for the group as a whole: from the group labels when
// the route groups by it, else from the labels every alert shares.
func (p Payload) Label(name string) string {
	if v, ok := p.GroupLabels[name]; ok {
		return v
	}
	return p.CommonLabels[name]
}

// Summary is the group's one-line description: the shared summary annotation,
// or the first alert's when the alerts disagree.
func (p Payload) Summary() string {
	if s := p.CommonAnnotations["summary"]; s != "" {
		return s
	}
	for _, a := range p.Alerts {
		if s := a.Annotations["summary"]; s != "" {
			return s
		}
	}
	return ""
}
