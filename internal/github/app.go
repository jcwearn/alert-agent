// Package github mints short-lived installation tokens for the alert-agent
// GitHub App, narrowed per use: the runner gets contents:read on one repo,
// and only the controller ever holds the App's private key.
package github

import (
	"bytes"
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"time"
)

// App signs requests as a GitHub App installation.
type App struct {
	AppID          string
	InstallationID string
	Key            *rsa.PrivateKey
	BaseURL        string // https://api.github.com unless testing
	HTTP           *http.Client
	Now            func() time.Time
}

// ParsePrivateKey reads the PEM key GitHub hands out (PKCS#1, or PKCS#8 if
// it was converted).
func ParsePrivateKey(pemBytes []byte) (*rsa.PrivateKey, error) {
	block, _ := pem.Decode(pemBytes)
	if block == nil {
		return nil, errors.New("no PEM block in private key")
	}
	if k, err := x509.ParsePKCS1PrivateKey(block.Bytes); err == nil {
		return k, nil
	}
	k, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("parse private key: %w", err)
	}
	rk, ok := k.(*rsa.PrivateKey)
	if !ok {
		return nil, errors.New("private key is not RSA")
	}
	return rk, nil
}

// jwt returns the App-level token used to ask for an installation token.
// iat is backdated a minute for clock skew; GitHub caps exp at ten minutes.
func (a *App) jwt() (string, error) {
	now := a.now()
	enc := base64.RawURLEncoding
	header := enc.EncodeToString([]byte(`{"alg":"RS256","typ":"JWT"}`))
	claims, err := json.Marshal(map[string]any{
		"iat": now.Add(-time.Minute).Unix(),
		"exp": now.Add(9 * time.Minute).Unix(),
		"iss": a.AppID,
	})
	if err != nil {
		return "", err
	}
	signingInput := header + "." + enc.EncodeToString(claims)
	sum := sha256.Sum256([]byte(signingInput))
	sig, err := rsa.SignPKCS1v15(rand.Reader, a.Key, crypto.SHA256, sum[:])
	if err != nil {
		return "", err
	}
	return signingInput + "." + enc.EncodeToString(sig), nil
}

// Token is an installation access token.
type Token struct {
	Token     string    `json:"token"`
	ExpiresAt time.Time `json:"expires_at"`
}

// InstallationToken mints a token limited to repos and permissions. Both are
// required: an empty list would mean "everything the installation has".
func (a *App) InstallationToken(ctx context.Context, repos []string, permissions map[string]string) (Token, error) {
	if len(repos) == 0 || len(permissions) == 0 {
		return Token{}, errors.New("installation token must name repositories and permissions")
	}
	jwt, err := a.jwt()
	if err != nil {
		return Token{}, err
	}
	body, err := json.Marshal(map[string]any{"repositories": repos, "permissions": permissions})
	if err != nil {
		return Token{}, err
	}
	url := a.baseURL() + "/app/installations/" + a.InstallationID + "/access_tokens"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return Token{}, err
	}
	req.Header.Set("Authorization", "Bearer "+jwt)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")

	resp, err := a.client().Do(req)
	if err != nil {
		return Token{}, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusCreated {
		return Token{}, fmt.Errorf("installation token: GitHub answered %s", strconv.Itoa(resp.StatusCode))
	}
	var t Token
	if err := json.NewDecoder(resp.Body).Decode(&t); err != nil {
		return Token{}, err
	}
	if t.Token == "" {
		return Token{}, errors.New("installation token: empty token in response")
	}
	return t, nil
}

func (a *App) now() time.Time {
	if a.Now != nil {
		return a.Now()
	}
	return time.Now()
}

func (a *App) baseURL() string {
	if a.BaseURL != "" {
		return a.BaseURL
	}
	return "https://api.github.com"
}

func (a *App) client() *http.Client {
	if a.HTTP != nil {
		return a.HTTP
	}
	return &http.Client{Timeout: 30 * time.Second}
}
