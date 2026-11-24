package github

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestInstallationToken(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Unix(1_800_000_000, 0)

	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/app/installations/99/access_tokens" || r.Method != http.MethodPost {
			http.Error(w, "wrong endpoint", http.StatusNotFound)
			return
		}
		verifyJWT(t, strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "), &key.PublicKey, now)
		if err := json.NewDecoder(r.Body).Decode(&gotBody); err != nil {
			t.Error(err)
		}
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"token":"ghs_test","expires_at":"2027-01-15T08:00:00Z"}`))
	}))
	t.Cleanup(srv.Close)

	app := &App{AppID: "12345", InstallationID: "99", Key: key, BaseURL: srv.URL, Now: func() time.Time { return now }}
	tok, err := app.InstallationToken(t.Context(), []string{"k3s-cluster"}, map[string]string{"contents": "read"})
	if err != nil {
		t.Fatal(err)
	}
	if tok.Token != "ghs_test" {
		t.Errorf("token = %q", tok.Token)
	}
	if repos, _ := gotBody["repositories"].([]any); len(repos) != 1 || repos[0] != "k3s-cluster" {
		t.Errorf("repositories not narrowed: %v", gotBody)
	}
	if perms, _ := gotBody["permissions"].(map[string]any); perms["contents"] != "read" || len(perms) != 1 {
		t.Errorf("permissions not narrowed: %v", gotBody)
	}
}

func TestInstallationTokenRefusesUnscoped(t *testing.T) {
	app := &App{AppID: "1", InstallationID: "2"}
	if _, err := app.InstallationToken(t.Context(), nil, map[string]string{"contents": "read"}); err == nil {
		t.Error("no repositories should be refused")
	}
	if _, err := app.InstallationToken(t.Context(), []string{"r"}, nil); err == nil {
		t.Error("no permissions should be refused")
	}
}

func TestParsePrivateKey(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	pkcs1 := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})
	if _, err := ParsePrivateKey(pkcs1); err != nil {
		t.Errorf("PKCS#1: %v", err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ParsePrivateKey(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})); err != nil {
		t.Errorf("PKCS#8: %v", err)
	}
	if _, err := ParsePrivateKey([]byte("nope")); err == nil {
		t.Error("garbage should fail")
	}
}

func verifyJWT(t *testing.T, token string, pub *rsa.PublicKey, now time.Time) {
	t.Helper()
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		t.Fatalf("jwt has %d parts", len(parts))
	}
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if err := rsa.VerifyPKCS1v15(pub, crypto.SHA256, sum[:], sig); err != nil {
		t.Fatalf("signature: %v", err)
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		t.Fatal(err)
	}
	var claims struct {
		Iat, Exp int64
		Iss      string
	}
	if err := json.Unmarshal(raw, &claims); err != nil {
		t.Fatal(err)
	}
	if claims.Iss != "12345" || claims.Iat >= now.Unix() || claims.Exp-now.Unix() > 600 {
		t.Errorf("claims wrong: %+v", claims)
	}
}
