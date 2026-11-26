package ui

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// Every page links the icons, and both they and /favicon.ico are served
// with the types a browser expects.
func TestIcons(t *testing.T) {
	var b strings.Builder
	if err := IncidentList(nil, nil, nil, time.Now()).Render(context.Background(), &b); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		`<link rel="icon" type="image/svg+xml" href="` + assetURL("favicon.svg") + `">`,
		`<link rel="apple-touch-icon" href="` + assetURL("apple-touch-icon.png") + `">`,
	} {
		if !strings.Contains(b.String(), want) {
			t.Errorf("layout lacks %s", want)
		}
	}

	for _, c := range []struct {
		h          http.Handler
		path, want string
	}{
		{StaticHandler(), assetURL("favicon.svg"), "image/svg+xml"},
		{StaticHandler(), assetURL("apple-touch-icon.png"), "image/png"},
		{FaviconHandler(), "/favicon.ico", "image/svg+xml"},
	} {
		rec := httptest.NewRecorder()
		c.h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, c.path, http.NoBody))
		if rec.Code != http.StatusOK || rec.Header().Get("Content-Type") != c.want || rec.Body.Len() == 0 {
			t.Errorf("GET %s: %d %q, %d bytes", c.path, rec.Code, rec.Header().Get("Content-Type"), rec.Body.Len())
		}
	}
}
