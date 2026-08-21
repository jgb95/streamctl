package handlers

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"streamctl/internal/btcppoauth"
	"streamctl/internal/db"
)

func authTestHandler(t *testing.T, roles []string) (*Handler, *http.ServeMux) {
	t.Helper()
	oauthServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/oauth/token":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"access_token":"access","refresh_token":"refresh","expires_in":3600,"scope":"identity:self:read offline_access","token_type":"Bearer"}`))
		case "/api/v1/me/identity":
			_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"id": "person-1", "name": "Mara", "roles": roles}})
		case "/oauth/revoke":
			w.WriteHeader(http.StatusOK)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(oauthServer.Close)
	database, err := db.Open(filepath.Join(t.TempDir(), "streamctl.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	if err := database.Migrate(); err != nil {
		t.Fatal(err)
	}
	h := &Handler{DB: database, OAuth: &btcppoauth.Client{
		BaseURL: oauthServer.URL, ClientID: "client", ClientSecret: "secret",
		RedirectURL: "https://stream.example/oauth/callback",
	}}
	mux := http.NewServeMux()
	h.Register(mux)
	return h, mux
}

func TestOAuthLoginAcceptsGlobalAdmin(t *testing.T) {
	_, mux := authTestHandler(t, []string{"global-admin"})
	start := httptest.NewRecorder()
	mux.ServeHTTP(start, httptest.NewRequest(http.MethodGet, "/oauth/start", nil))
	if start.Code != http.StatusSeeOther {
		t.Fatalf("start status = %d", start.Code)
	}
	authorize, err := url.Parse(start.Header().Get("Location"))
	if err != nil {
		t.Fatal(err)
	}
	state := authorize.Query().Get("state")
	var stateCookie *http.Cookie
	for _, cookie := range start.Result().Cookies() {
		if cookie.Name == stateCookieName {
			stateCookie = cookie
		}
	}
	if state == "" || stateCookie == nil {
		t.Fatal("OAuth start omitted state")
	}
	callback := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/oauth/callback?code=code&state="+url.QueryEscape(state), nil)
	request.AddCookie(stateCookie)
	mux.ServeHTTP(callback, request)
	if callback.Code != http.StatusSeeOther || callback.Header().Get("Location") != "/" {
		t.Fatalf("callback = %d %q: %s", callback.Code, callback.Header().Get("Location"), callback.Body.String())
	}
	foundSession := false
	for _, cookie := range callback.Result().Cookies() {
		foundSession = foundSession || cookie.Name == sessionCookieName && cookie.Value != ""
	}
	if !foundSession {
		t.Fatal("OAuth callback did not issue a streamctl session")
	}
}

func TestOAuthLoginRejectsNonGlobalAdmin(t *testing.T) {
	_, mux := authTestHandler(t, []string{"conference-admin:dev26"})
	start := httptest.NewRecorder()
	mux.ServeHTTP(start, httptest.NewRequest(http.MethodGet, "/oauth/start", nil))
	authorize, _ := url.Parse(start.Header().Get("Location"))
	state := authorize.Query().Get("state")
	callback := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/oauth/callback?code=code&state="+url.QueryEscape(state), nil)
	request.AddCookie(start.Result().Cookies()[0])
	mux.ServeHTTP(callback, request)
	if callback.Code != http.StatusSeeOther || !strings.Contains(callback.Header().Get("Location"), "/login?error=") {
		t.Fatalf("callback = %d %q", callback.Code, callback.Header().Get("Location"))
	}
	for _, cookie := range callback.Result().Cookies() {
		if cookie.Name == sessionCookieName && cookie.Value != "" {
			t.Fatal("non-global-admin received a streamctl session")
		}
	}
}

func TestMutationRequiresPOSTAndCSRF(t *testing.T) {
	h, _ := authTestHandler(t, []string{"global-admin"})
	sessionToken := "session"
	hash := sha256.Sum256([]byte(sessionToken))
	now := time.Now()
	if err := h.DB.CreateAuthSession(hash[:], db.AuthSession{
		PersonID: "person-1", DisplayName: "Mara", AccessToken: "access", RefreshToken: "refresh",
		AccessExpiresAt: now.Add(time.Hour), RoleCheckedAt: now, CSRFToken: "csrf",
		CreatedAt: now, ExpiresAt: now.Add(time.Hour), LastSeenAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	called := false
	handler := h.mutation(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { called = true }))
	request := httptest.NewRequest(http.MethodPost, "/mutation", strings.NewReader("csrf_token=wrong"))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	request.AddCookie(&http.Cookie{Name: sessionCookieName, Value: sessionToken})
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusForbidden || called {
		t.Fatalf("bad CSRF = %d, called=%v", response.Code, called)
	}
	request = httptest.NewRequest(http.MethodGet, "/mutation", nil)
	request.AddCookie(&http.Cookie{Name: sessionCookieName, Value: sessionToken})
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusMethodNotAllowed || called {
		t.Fatalf("GET mutation = %d, called=%v", response.Code, called)
	}
}

func TestSessionIsRemovedWhenGlobalAdminRoleIsRemoved(t *testing.T) {
	h, _ := authTestHandler(t, []string{"conference-admin:dev26"})
	sessionToken := "stale-session"
	hash := sha256.Sum256([]byte(sessionToken))
	now := time.Now()
	if err := h.DB.CreateAuthSession(hash[:], db.AuthSession{
		PersonID: "person-1", DisplayName: "Mara", AccessToken: "access", RefreshToken: "refresh",
		AccessExpiresAt: now.Add(time.Hour), RoleCheckedAt: now.Add(-roleCheckInterval), CSRFToken: "csrf",
		CreatedAt: now.Add(-time.Hour), ExpiresAt: now.Add(time.Hour), LastSeenAt: now.Add(-time.Hour),
	}); err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodGet, "/", nil)
	request.AddCookie(&http.Cookie{Name: sessionCookieName, Value: sessionToken})
	principal, err := h.authenticateRequest(request)
	if err == nil || principal != nil {
		t.Fatalf("authentication = %#v, %v", principal, err)
	}
	session, findErr := h.DB.FindAuthSession(hash[:], now)
	if findErr != nil || session != nil {
		t.Fatalf("revoked session still present: %#v, %v", session, findErr)
	}
}

func TestAuthenticatedLayoutRendersIdentityAndCSRF(t *testing.T) {
	h, _ := authTestHandler(t, []string{"global-admin"})
	request := httptest.NewRequest(http.MethodGet, "/", nil)
	request = request.WithContext(context.WithValue(request.Context(), requestAuthKey{}, &requestAuth{
		DisplayName: "Mara", CSRFToken: "csrf-token",
	}))
	response := httptest.NewRecorder()
	h.render(response, request, "streams.html", map[string]any{"Streams": nil})
	if response.Code != http.StatusOK {
		t.Fatalf("render status = %d: %s", response.Code, response.Body.String())
	}
	body := response.Body.String()
	if !strings.Contains(body, "Mara") || !strings.Contains(body, `name="csrf_token" value="csrf-token"`) {
		t.Fatalf("layout omitted identity or CSRF field: %s", body)
	}
}
