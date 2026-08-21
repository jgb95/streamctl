package btcppoauth

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSecretFromFileRequiresPrivatePermissions(t *testing.T) {
	path := filepath.Join(t.TempDir(), "client-secret")
	if err := os.WriteFile(path, []byte("secret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	secret, err := SecretFromFile(path)
	if err != nil || secret != "secret" {
		t.Fatalf("secret = %q, %v", secret, err)
	}
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := SecretFromFile(path); err == nil {
		t.Fatal("expected group/world-readable client secret to be rejected")
	}
}

func TestAuthorizationURLUsesPKCEAndMinimalIdentityScope(t *testing.T) {
	client := &Client{BaseURL: "https://btcpp.dev", ClientID: "client-id", ClientSecret: "secret", RedirectURL: "https://stream.btcpp.dev/oauth/callback"}
	target, err := client.AuthorizationURL("state", "verifier")
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := url.Parse(target)
	if err != nil {
		t.Fatal(err)
	}
	query := parsed.Query()
	if parsed.Path != "/oauth/authorize" || query.Get("scope") != "identity:self:read offline_access" || query.Get("code_challenge_method") != "S256" || query.Get("code_challenge") != PKCEChallenge("verifier") {
		t.Fatalf("authorization URL = %s", target)
	}
}

func TestExchangeAndIdentity(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/oauth/token":
			clientID, secret, ok := r.BasicAuth()
			if !ok || clientID != "client-id" || secret != "secret" {
				t.Fatalf("basic auth = %q / %q / %v", clientID, secret, ok)
			}
			if err := r.ParseForm(); err != nil {
				t.Fatal(err)
			}
			if r.FormValue("code_verifier") != "verifier" || r.FormValue("redirect_uri") != "https://stream.example/oauth/callback" {
				t.Fatalf("token form = %v", r.Form)
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "access", "refresh_token": "refresh", "expires_in": 3600, "scope": "identity:self:read offline_access", "token_type": "Bearer"})
		case "/api/v1/me/identity":
			if r.Header.Get("Authorization") != "Bearer access" {
				t.Fatalf("Authorization = %q", r.Header.Get("Authorization"))
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"id": "person-1", "name": "Mara", "roles": []string{"global-admin"}}})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	client := &Client{BaseURL: server.URL, ClientID: "client-id", ClientSecret: "secret", RedirectURL: "https://stream.example/oauth/callback", HTTPClient: server.Client()}
	tokens, err := client.Exchange(context.Background(), "code", "verifier")
	if err != nil {
		t.Fatal(err)
	}
	if tokens.AccessToken != "access" || tokens.RefreshToken != "refresh" || !strings.Contains(strings.Join(tokens.Scopes, " "), "identity:self:read") {
		t.Fatalf("tokens = %#v", tokens)
	}
	identity, err := client.Identity(context.Background(), tokens.AccessToken)
	if err != nil {
		t.Fatal(err)
	}
	if identity.ID != "person-1" || identity.Name != "Mara" || len(identity.Roles) != 1 || identity.Roles[0] != "global-admin" {
		t.Fatalf("identity = %#v", identity)
	}
}
