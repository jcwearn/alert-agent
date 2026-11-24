package ui

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/jcwearn/alert-agent/internal/store"
)

func TestTranscript(t *testing.T) {
	ev := func(typ, payload string) store.RunEvent {
		return store.RunEvent{Type: typ, Payload: json.RawMessage(payload)}
	}
	items := Transcript([]store.RunEvent{
		ev("system", `{"type":"system","subtype":"init","model":"claude-opus-5-5","tools":["Bash","Read"]}`),
		ev("assistant", `{"type":"assistant","message":{"content":[{"type":"thinking","thinking":""},{"type":"text","text":"Checking pods."},{"type":"tool_use","name":"Bash","input":{"command":"kubectl get pods -n ebooks","description":"List pods"}}]}}`),
		ev("user", `{"type":"user","message":{"content":[{"type":"tool_result","content":[{"type":"text","text":"calibre-0 0/1 CrashLoopBackOff"}],"is_error":false}]}}`),
		ev("user", `{"type":"user","message":{"content":[{"type":"tool_result","content":"Forbidden","is_error":true}]}}`),
		ev("raw", `{"type":"raw","text":"oops"}`),
		ev("result", `{"type":"result","subtype":"success","num_turns":3}`),
	})

	kinds := make([]string, len(items))
	for i, it := range items {
		kinds[i] = it.Kind
	}
	want := []string{ItemSystem, ItemText, ItemTool, ItemResult, ItemResult, ItemRaw, ItemFinal}
	if strings.Join(kinds, ",") != strings.Join(want, ",") {
		t.Fatalf("kinds = %v, want %v", kinds, want)
	}
	if !strings.Contains(items[0].Title, "claude-opus-5-5") {
		t.Errorf("system title = %q", items[0].Title)
	}
	if items[2].Title != "$ kubectl get pods -n ebooks" || items[2].Body != "List pods" {
		t.Errorf("bash step = %+v", items[2])
	}
	if items[3].Body != "calibre-0 0/1 CrashLoopBackOff" || items[3].IsError {
		t.Errorf("result = %+v", items[3])
	}
	if !items[4].IsError {
		t.Error("error result not flagged")
	}
}

func TestTruncate(t *testing.T) {
	long := strings.Repeat("x", resultLimit+10)
	if got := truncate(long); !strings.Contains(got, "10 more characters") || len(got) > resultLimit+60 {
		t.Errorf("truncate wrong: %d chars", len(got))
	}
}
