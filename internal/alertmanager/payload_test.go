package alertmanager

import (
	"strings"
	"testing"
)

const firing = `{
  "version": "4",
  "groupKey": "{}/{}:{alertname=\"KubePodCrashLooping\", namespace=\"ebooks\"}",
  "status": "firing",
  "receiver": "alert-agent",
  "groupLabels": {"alertname": "KubePodCrashLooping", "namespace": "ebooks"},
  "commonLabels": {"alertname": "KubePodCrashLooping", "namespace": "ebooks", "severity": "warning"},
  "commonAnnotations": {},
  "externalURL": "http://alertmanager:9093",
  "alerts": [
    {"status": "firing", "labels": {"pod": "a"}, "annotations": {"summary": "Pod a is crash looping"},
     "startsAt": "2026-11-24T10:00:00Z", "endsAt": "0001-01-01T00:00:00Z", "fingerprint": "f1"},
    {"status": "firing", "labels": {"pod": "b"}, "annotations": {"summary": "Pod b is crash looping"},
     "startsAt": "2026-11-24T10:01:00Z", "endsAt": "0001-01-01T00:00:00Z", "fingerprint": "f2"}
  ]
}`

func TestParse(t *testing.T) {
	p, err := Parse([]byte(firing))
	if err != nil {
		t.Fatal(err)
	}
	if got := p.Label("alertname"); got != "KubePodCrashLooping" {
		t.Errorf("alertname = %q", got)
	}
	if got := p.Label("severity"); got != "warning" {
		t.Errorf("severity from commonLabels = %q", got)
	}
	if got := p.Summary(); got != "Pod a is crash looping" {
		t.Errorf("summary falls back to first alert, got %q", got)
	}
	if len(p.Alerts) != 2 || p.Alerts[1].Fingerprint != "f2" {
		t.Errorf("alerts not decoded: %+v", p.Alerts)
	}
}

func TestParseRejects(t *testing.T) {
	cases := map[string]string{
		"not json":       `{`,
		"wrong version":  strings.Replace(firing, `"version": "4"`, `"version": "3"`, 1),
		"no group key":   strings.Replace(firing, `"groupKey": "{}/{}:{alertname=\"KubePodCrashLooping\", namespace=\"ebooks\"}"`, `"groupKey": ""`, 1),
		"bad status":     strings.Replace(firing, `"status": "firing",`+"\n  \"receiver\"", `"status": "pending",`+"\n  \"receiver\"", 1),
		"no fingerprint": strings.Replace(firing, `"fingerprint": "f2"`, `"fingerprint": ""`, 1),
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			if body == firing {
				t.Fatal("test case did not change the payload")
			}
			if _, err := Parse([]byte(body)); err == nil {
				t.Error("expected an error")
			}
		})
	}
}
