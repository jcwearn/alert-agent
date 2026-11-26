package preview

import (
	"html"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
)

// Every page and API call the preview offers must work: the fixtures
// implement only part of the store, and a missing method panics.
func TestPreviewServes(t *testing.T) {
	srv := httptest.NewServer(Handler())
	defer srv.Close()
	get := func(path string) (int, string) {
		t.Helper()
		resp, err := http.Get(srv.URL + path) //nolint:noctx // a test against a local server
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = resp.Body.Close() }()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}
	for _, p := range []string{"/", "/incidents/22", "/incidents/23", "/incidents/24", "/incidents/25", "/incidents/26", "/incidents/27", "/incidents/28", "/incidents/29", "/?paused=1", "/runs/41", "/runs/43",
		"/runs/20", "/runs/29", "/runs/33", "/runs/36", "/runs/40",
		"/api/chat/threads?page=/incidents/26", "/api/chat/threads/1", "/api/chat/threads/2", "/api/chat/threads/3",
		"/api/chat/threads/4", "/api/chat/threads/5", "/api/chat/threads/6", "/api/chat/threads/7", "/static/chat.js", "/static/favicon.svg", "/static/apple-touch-icon.png", "/favicon.ico"} {
		if status, body := get(p); status != http.StatusOK {
			t.Errorf("GET %s: %d %s", p, status, body)
		}
	}

	_, page := get("/incidents/25?chat=open&thread=2&menu=1")
	if !strings.Contains(page, `<body><script>try { sessionStorage.setItem("alert-agent.chat"`) || !strings.Contains(page, `\"thread\":2`) {
		t.Errorf("chat state not injected: %.400s", page)
	}
	if _, page := get("/incidents/25"); strings.Contains(page, "sessionStorage") {
		t.Error("state injected without being asked for")
	}

	post := func(path, body string) string {
		t.Helper()
		req, _ := http.NewRequest(http.MethodPost, srv.URL+path, strings.NewReader(body)) //nolint:noctx // a test against a local server
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = resp.Body.Close() }()
		b, _ := io.ReadAll(resp.Body)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("POST %s: %d %s", path, resp.StatusCode, b)
		}
		return string(b)
	}
	if out := post("/api/chat/threads", `{"kind":"general"}`); !strings.Contains(out, `"id":8`) {
		t.Errorf("new thread: %s", out)
	}
	if out := post("/api/chat/threads/8/messages", `{"body":"hello","page":"/"}`); !strings.Contains(out, `"new_turn":true`) {
		t.Errorf("message: %s", out)
	}
	if _, body := get("/api/chat/threads/8"); !strings.Contains(body, `"active":{`) || !strings.Contains(body, `"title":"hello"`) {
		t.Errorf("thread after a message: %s", body)
	}
}

// The fixtures fill three home pages, and following the links (no
// JavaScript) walks them without losing or repeating a family.
func TestPreviewPages(t *testing.T) {
	srv := httptest.NewServer(Handler())
	defer srv.Close()
	get := func(path string) string {
		t.Helper()
		resp, err := http.Get(srv.URL + path) //nolint:noctx // a test against a local server
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = resp.Body.Close() }()
		b, _ := io.ReadAll(resp.Body)
		return string(b)
	}
	nums := regexp.MustCompile(`class="num">#(\d+)`)
	next := regexp.MustCompile(`href="([^"]*)" rel="next"`)
	var seen []string
	path, pages := "/", 0
	for path != "" {
		page := get(path)
		pages++
		for _, m := range nums.FindAllStringSubmatch(page, -1) {
			seen = append(seen, m[1])
		}
		path = ""
		if m := next.FindStringSubmatch(page); m != nil {
			path = html.UnescapeString(m[1])
		}
	}
	if pages != 3 || len(seen) != 29 {
		t.Errorf("walked %d pages and %d incidents, want 3 and 29: %v", pages, len(seen), seen)
	}
	if got := nums.FindAllStringSubmatch(get("/?status=firing&ns=immich"), -1); len(got) != 1 || got[0][1] != "11" {
		t.Errorf("firing in immich: %v", got)
	}
}
