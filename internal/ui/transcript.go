package ui

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/jcwearn/alert-agent/internal/store"
)

// Item kinds a transcript renders as.
const (
	ItemSystem = "system"
	ItemText   = "text"
	ItemTool   = "tool"
	ItemResult = "result"
	ItemFinal  = "final"
	ItemRaw    = "raw"
)

// TranscriptItem is one rendered step of a run.
type TranscriptItem struct {
	Kind    string
	Title   string
	Body    string
	IsError bool
}

// resultLimit keeps a page with a few large kubectl outputs loadable. The
// full payload stays in run_events.
const resultLimit = 8000

// Transcript turns stream-json events into readable steps. Unknown shapes
// are shown raw rather than dropped: the transcript is an audit record.
func Transcript(events []store.RunEvent) []TranscriptItem {
	var items []TranscriptItem
	for _, e := range events {
		switch e.Type {
		case "system":
			var ev struct {
				Subtype string   `json:"subtype"`
				Model   string   `json:"model"`
				Tools   []string `json:"tools"`
			}
			_ = json.Unmarshal(e.Payload, &ev)
			if ev.Subtype == "init" {
				items = append(items, TranscriptItem{Kind: ItemSystem,
					Title: fmt.Sprintf("Session started · %s · %d tools", ev.Model, len(ev.Tools))})
			}
		case "assistant", "user":
			items = append(items, messageItems(e.Payload)...)
		case "result":
			var ev struct {
				Subtype  string `json:"subtype"`
				NumTurns int    `json:"num_turns"`
				IsError  bool   `json:"is_error"`
			}
			_ = json.Unmarshal(e.Payload, &ev)
			items = append(items, TranscriptItem{Kind: ItemFinal, IsError: ev.IsError,
				Title: fmt.Sprintf("Finished · %s · %d turns", ev.Subtype, ev.NumTurns)})
		default:
			items = append(items, TranscriptItem{Kind: ItemRaw, Title: e.Type, Body: truncate(string(e.Payload))})
		}
	}
	return items
}

type contentBlock struct {
	Type      string          `json:"type"`
	Text      string          `json:"text"`
	Name      string          `json:"name"`
	Input     json.RawMessage `json:"input"`
	Content   json.RawMessage `json:"content"`
	IsError   bool            `json:"is_error"`
	ToolUseID string          `json:"tool_use_id"`
}

func messageItems(payload json.RawMessage) []TranscriptItem {
	var ev struct {
		Message struct {
			Content json.RawMessage `json:"content"`
		} `json:"message"`
	}
	if err := json.Unmarshal(payload, &ev); err != nil {
		return []TranscriptItem{{Kind: ItemRaw, Body: truncate(string(payload))}}
	}
	// A user message's content may be a bare string.
	var text string
	if json.Unmarshal(ev.Message.Content, &text) == nil {
		return []TranscriptItem{{Kind: ItemText, Body: text}}
	}
	var blocks []contentBlock
	if err := json.Unmarshal(ev.Message.Content, &blocks); err != nil {
		return []TranscriptItem{{Kind: ItemRaw, Body: truncate(string(payload))}}
	}

	var items []TranscriptItem
	for _, b := range blocks {
		switch b.Type {
		case "text":
			if strings.TrimSpace(b.Text) != "" {
				items = append(items, TranscriptItem{Kind: ItemText, Body: b.Text})
			}
		case "tool_use":
			title, body := describeToolUse(b.Name, b.Input)
			items = append(items, TranscriptItem{Kind: ItemTool, Title: title, Body: body})
		case "tool_result":
			out := toolResultText(b.Content)
			items = append(items, TranscriptItem{Kind: ItemResult, IsError: b.IsError,
				Title: fmt.Sprintf("%d chars", len(out)), Body: truncate(out)})
		}
		// thinking blocks are intentionally not shown: they are empty by
		// default on current models, and the report is what matters.
	}
	return items
}

// describeToolUse gives a one-line title for the common tools and keeps the
// full input for the rest.
func describeToolUse(name string, input json.RawMessage) (title, body string) {
	var in map[string]any
	_ = json.Unmarshal(input, &in)
	str := func(k string) string { s, _ := in[k].(string); return s }
	switch name {
	case "Bash":
		return "$ " + str("command"), str("description")
	case "Read":
		return "Read " + str("file_path"), ""
	case "Grep":
		return "Grep " + str("pattern") + " " + str("path"), ""
	case "Glob":
		return "Glob " + str("pattern"), ""
	default:
		pretty, err := json.MarshalIndent(in, "", "  ")
		if err != nil {
			return name, string(input)
		}
		return name, string(pretty)
	}
}

func toolResultText(content json.RawMessage) string {
	var s string
	if json.Unmarshal(content, &s) == nil {
		return s
	}
	var parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if json.Unmarshal(content, &parts) == nil {
		var b strings.Builder
		for _, p := range parts {
			if p.Type == "text" {
				b.WriteString(p.Text)
			}
		}
		return b.String()
	}
	return string(content)
}

func truncate(s string) string {
	if len(s) <= resultLimit {
		return s
	}
	return s[:resultLimit] + fmt.Sprintf("\n… %d more characters not shown", len(s)-resultLimit)
}
