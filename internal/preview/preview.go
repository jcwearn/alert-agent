// Package preview serves the real UI against made-up data, for looking at
// it (and screenshotting it) without a database, a cluster or GitHub. It is
// a development tool: `alert-agent ui-preview`, on localhost.
//
// Query parameters set the chat window's state, so every screenshot is one
// URL: chat=open, full=1, thread=<id>, pending=general, menu=1, draft=<text>.
// chat=after loads the page with the window closed and then opens it, the
// way a person does (the window starts hidden).
// paused=1 shows autonomous actions paused.
package preview

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"html"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"

	"github.com/jcwearn/alert-agent/internal/controller"
)

// Handler is the UI over fixture data.
func Handler() http.Handler {
	s := controller.New(newFixtures(), controller.Options{PublicURL: "https://medic.example", ChatMaxPerThreadHour: 4, ChatMaxPerHour: 12, ListPageSize: pageSize}, nil, nil)
	// Turns the "awaiting review" list and PR abilities on; nothing here
	// ever reaches GitHub, since no runner calls the tools.
	s.EnablePRs(&controller.PRs{Repo: "example/homelab"}, nil)
	// Turns the approvals and the pause switch on; approving runs nothing.
	s.EnableActions(&controller.Actions{Actor: previewActor{}, Autonomous: map[string]bool{"books": true, "photos": true}})
	mux := http.NewServeMux()
	mux.Handle("/", withPaused(withChatState(s.UIHandler())))
	mux.HandleFunc("GET /__phone", phoneFrame)
	return mux
}

// phoneFrame shows a page in a 390×844 frame. A desktop Chrome window, even
// headless, will not lay out narrower than about 500px, so this is how the
// screenshots get a phone's width.
func phoneFrame(w http.ResponseWriter, r *http.Request) {
	src := r.URL.Query().Get("src")
	if !strings.HasPrefix(src, "/") || strings.HasPrefix(src, "//") {
		http.Error(w, "src must be a path on this server", http.StatusBadRequest)
		return
	}
	// h makes the frame taller, to capture a whole page.
	h, err := strconv.Atoi(r.URL.Query().Get("h"))
	if err != nil || h < 400 || h > 8000 {
		h = 844
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = fmt.Fprintf(w, `<!DOCTYPE html><html><head><meta charset="utf-8"><title>phone</title>
<style>html,body{margin:0;background:#888}iframe{display:block;border:0;width:390px;height:%dpx}</style></head>
<body><iframe src="%s"></iframe></body></html>`, h, html.EscapeString(src))
}

// withPaused shows the pages with autonomous actions paused when the URL
// says paused=1.
func withPaused(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("paused") == "1" {
			r = r.WithContext(context.WithValue(r.Context(), pausedKey{}, true))
		}
		next.ServeHTTP(w, r)
	})
}

// withChatState puts the chat window in the state the query asks for,
// before chat.js reads it.
func withChatState(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if r.Method != http.MethodGet || (q.Get("chat") == "" && q.Get("thread") == "" && q.Get("pending") == "") {
			next.ServeHTTP(w, r)
			return
		}
		rec := httptest.NewRecorder()
		next.ServeHTTP(rec, r)
		body := rec.Body.Bytes()
		if strings.HasPrefix(rec.Header().Get("Content-Type"), "text/html") {
			body = bytes.Replace(body, []byte("<body>"), []byte("<body>"+stateScript(r)), 1)
		}
		for k, v := range rec.Header() {
			w.Header()[k] = v
		}
		w.Header().Del("Content-Length")
		w.WriteHeader(rec.Code)
		_, _ = w.Write(body)
	})
}

func stateScript(r *http.Request) string {
	q := r.URL.Query()
	state := map[string]any{"open": q.Get("chat") == "open", "full": q.Get("full") == "1", "draft": q.Get("draft")}
	if id, err := strconv.ParseInt(q.Get("thread"), 10, 64); err == nil {
		state["thread"] = id
	}
	if q.Get("pending") == "general" {
		state["pending"] = map[string]string{"kind": "general"}
	}
	b, _ := json.Marshal(state)
	js := `try { sessionStorage.setItem("alert-agent.chat", ` + strconv.Quote(string(b)) + `); } catch (e) {}`
	if q.Get("chat") == "after" {
		js += `window.addEventListener("load", function () {
			setTimeout(function () {
				document.querySelector(".cw-launch").click();
				// A screenshot does not wait for the opening animation.
				document.getElementById("cw-panel").classList.remove("cw-anim");
			}, 0);
		});`
	}
	if q.Get("menu") == "1" {
		js += `document.addEventListener("chat:render", function f() {
			document.removeEventListener("chat:render", f);
			setTimeout(function () { document.querySelector(".cw-title").click(); }, 0);
		});`
	}
	// The script is ours and the values are JSON-quoted; escape only what
	// would end the element.
	return "<script>" + strings.ReplaceAll(js, "</", `<\/`) + "</script>"
}
