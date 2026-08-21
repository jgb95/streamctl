package handlers

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"html/template"
	"log"
	"net/http"
	"net/url"
	"strings"
	"time"

	"streamctl/internal/btcppoauth"
	"streamctl/internal/db"
)

const (
	sessionCookieName = "streamctl_session"
	stateCookieName   = "streamctl_oauth_state"
	loginStateTTL     = 10 * time.Minute
	sessionTTL        = 30 * 24 * time.Hour
	roleCheckInterval = 5 * time.Minute
)

var errGlobalAdminRequired = errors.New("Bitcoin++ global-admin role is required")

type requestAuth struct {
	PersonID    string
	DisplayName string
	CSRFToken   string
	SessionHash []byte
	Session     *db.AuthSession
	BreakGlass  bool
}

type requestAuthKey struct{}

func (h *Handler) auth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		principal, err := h.authenticateRequest(r)
		if err != nil {
			logHandlerError("authenticate request", err)
		}
		if principal == nil {
			target := "/login"
			if r.Method == http.MethodGet && r.URL.RequestURI() != "/" {
				target += "?next=" + url.QueryEscape(safeLocalNext(r.URL.RequestURI()))
			}
			http.Redirect(w, r, target, http.StatusSeeOther)
			return
		}
		if isUnsafeMethod(r.Method) {
			if err := r.ParseForm(); err != nil || subtle.ConstantTimeCompare([]byte(principal.CSRFToken), []byte(r.FormValue("csrf_token"))) != 1 {
				http.Error(w, "Invalid or expired request. Reload the page and try again.", http.StatusForbidden)
				return
			}
		}
		r = r.WithContext(context.WithValue(r.Context(), requestAuthKey{}, principal))
		next.ServeHTTP(w, r)
	})
}

func (h *Handler) mutation(next http.Handler) http.Handler {
	return h.auth(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.Header().Set("Allow", http.MethodPost)
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}
		next.ServeHTTP(w, r)
	}))
}

func (h *Handler) authenticateRequest(r *http.Request) (*requestAuth, error) {
	cookie, err := r.Cookie(sessionCookieName)
	if err != nil || strings.TrimSpace(cookie.Value) == "" {
		return nil, nil
	}
	if h.Secret != "" && subtle.ConstantTimeCompare([]byte(cookie.Value), []byte(h.Secret)) == 1 {
		return &requestAuth{DisplayName: "break-glass admin", CSRFToken: h.breakGlassCSRF(), BreakGlass: true}, nil
	}
	if h.OAuth == nil || h.DB == nil {
		return nil, nil
	}
	hash := sha256.Sum256([]byte(cookie.Value))
	now := time.Now()
	session, err := h.DB.FindAuthSession(hash[:], now)
	if err != nil || session == nil {
		return nil, err
	}
	if now.Sub(session.RoleCheckedAt) >= roleCheckInterval {
		if err := h.revalidateOAuthSession(r.Context(), hash[:], session, now); err != nil {
			if errors.Is(err, errGlobalAdminRequired) {
				_ = h.DB.DeleteAuthSession(hash[:])
			}
			return nil, err
		}
	} else if now.Sub(session.LastSeenAt) >= roleCheckInterval {
		session.LastSeenAt = now
		if err := h.DB.UpdateAuthSession(hash[:], *session); err != nil {
			return nil, err
		}
	}
	return &requestAuth{
		PersonID: session.PersonID, DisplayName: session.DisplayName, CSRFToken: session.CSRFToken,
		SessionHash: hash[:], Session: session,
	}, nil
}

func (h *Handler) revalidateOAuthSession(ctx context.Context, sessionHash []byte, session *db.AuthSession, now time.Time) error {
	if session.AccessExpiresAt.Before(now.Add(30 * time.Second)) {
		if strings.TrimSpace(session.RefreshToken) == "" {
			return fmt.Errorf("bitcoin++ OAuth session cannot be refreshed")
		}
		tokens, err := h.OAuth.Refresh(ctx, session.RefreshToken)
		if err != nil {
			return err
		}
		session.AccessToken = tokens.AccessToken
		if tokens.RefreshToken != "" {
			session.RefreshToken = tokens.RefreshToken
		}
		session.AccessExpiresAt = tokens.ExpiresAt
		session.ExpiresAt = now.Add(sessionTTL)
	}
	identity, err := h.OAuth.Identity(ctx, session.AccessToken)
	if err != nil {
		return err
	}
	if !hasGlobalAdminRole(identity.Roles) {
		return fmt.Errorf("%w: account %s is no longer a global admin", errGlobalAdminRequired, identity.ID)
	}
	session.PersonID = identity.ID
	session.DisplayName = identity.Name
	session.RoleCheckedAt = now
	session.LastSeenAt = now
	return h.DB.UpdateAuthSession(sessionHash, *session)
}

func (h *Handler) login(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet {
		h.renderLogin(w, r, r.URL.Query().Get("error"))
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if h.Secret == "" {
		h.renderLogin(w, r, "Break-glass login is disabled.")
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if subtle.ConstantTimeCompare([]byte(r.FormValue("secret")), []byte(h.Secret)) != 1 {
		h.renderLogin(w, r, "Incorrect break-glass secret.")
		return
	}
	h.setSessionCookie(w, r, h.Secret, int(sessionTTL.Seconds()))
	http.Redirect(w, r, safeLocalNext(r.FormValue("next")), http.StatusSeeOther)
}

func (h *Handler) oauthStart(w http.ResponseWriter, r *http.Request) {
	if h.OAuth == nil {
		http.Redirect(w, r, "/login?error="+url.QueryEscape("Bitcoin++ login is not configured."), http.StatusSeeOther)
		return
	}
	state, err := btcppoauth.RandomToken(32)
	if err != nil {
		http.Error(w, "Unable to begin login.", http.StatusInternalServerError)
		return
	}
	verifier, err := btcppoauth.RandomToken(48)
	if err != nil {
		http.Error(w, "Unable to begin login.", http.StatusInternalServerError)
		return
	}
	hash := sha256.Sum256([]byte(state))
	now := time.Now()
	if err := h.DB.CleanupAuthState(now); err != nil {
		logHandlerError("cleanup OAuth login state", err)
	}
	if err := h.DB.CreateOAuthLoginState(hash[:], verifier, now, now.Add(loginStateTTL)); err != nil {
		http.Error(w, "Unable to begin login.", http.StatusInternalServerError)
		return
	}
	target, err := h.OAuth.AuthorizationURL(state, verifier)
	if err != nil {
		http.Error(w, "Unable to begin login.", http.StatusInternalServerError)
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name: stateCookieName, Value: state, Path: "/oauth/callback", HttpOnly: true,
		Secure: requestIsHTTPS(r), SameSite: http.SameSiteLaxMode, MaxAge: int(loginStateTTL.Seconds()),
	})
	http.Redirect(w, r, target, http.StatusSeeOther)
}

func (h *Handler) oauthCallback(w http.ResponseWriter, r *http.Request) {
	if h.OAuth == nil {
		http.Redirect(w, r, "/login?error="+url.QueryEscape("Bitcoin++ login is not configured."), http.StatusSeeOther)
		return
	}
	stateCookie, err := r.Cookie(stateCookieName)
	state := strings.TrimSpace(r.URL.Query().Get("state"))
	if err != nil || state == "" || subtle.ConstantTimeCompare([]byte(stateCookie.Value), []byte(state)) != 1 {
		http.Redirect(w, r, "/login?error="+url.QueryEscape("That Bitcoin++ login request is invalid or expired."), http.StatusSeeOther)
		return
	}
	h.clearCookie(w, r, stateCookieName, "/oauth/callback")
	stateHash := sha256.Sum256([]byte(state))
	verifier, err := h.DB.ConsumeOAuthLoginState(stateHash[:], time.Now())
	if err != nil || verifier == "" {
		http.Redirect(w, r, "/login?error="+url.QueryEscape("That Bitcoin++ login request is invalid or expired."), http.StatusSeeOther)
		return
	}
	if oauthError := strings.TrimSpace(r.URL.Query().Get("error")); oauthError != "" {
		http.Redirect(w, r, "/login?error="+url.QueryEscape("Bitcoin++ authorization was not completed."), http.StatusSeeOther)
		return
	}
	if issuer := strings.TrimRight(strings.TrimSpace(r.URL.Query().Get("iss")), "/"); issuer != "" && issuer != strings.TrimRight(strings.TrimSpace(h.OAuth.BaseURL), "/") {
		http.Redirect(w, r, "/login?error="+url.QueryEscape("Bitcoin++ returned an unexpected authorization issuer."), http.StatusSeeOther)
		return
	}
	code := strings.TrimSpace(r.URL.Query().Get("code"))
	if code == "" {
		http.Redirect(w, r, "/login?error="+url.QueryEscape("Bitcoin++ did not return an authorization code."), http.StatusSeeOther)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	tokens, err := h.OAuth.Exchange(ctx, code, verifier)
	if err != nil {
		logHandlerError("exchange OAuth code", err)
		http.Redirect(w, r, "/login?error="+url.QueryEscape("Bitcoin++ login could not be completed."), http.StatusSeeOther)
		return
	}
	identity, err := h.OAuth.Identity(ctx, tokens.AccessToken)
	if err != nil {
		logHandlerError("load OAuth identity", err)
		http.Redirect(w, r, "/login?error="+url.QueryEscape("Your Bitcoin++ account details could not be loaded."), http.StatusSeeOther)
		return
	}
	if !hasGlobalAdminRole(identity.Roles) {
		_ = h.OAuth.Revoke(ctx, tokens.AccessToken)
		_ = h.OAuth.Revoke(ctx, tokens.RefreshToken)
		http.Redirect(w, r, "/login?error="+url.QueryEscape("Only Bitcoin++ global administrators can access streamctl."), http.StatusSeeOther)
		return
	}
	sessionToken, err := btcppoauth.RandomToken(32)
	if err != nil {
		http.Error(w, "Unable to create session.", http.StatusInternalServerError)
		return
	}
	csrfToken, err := btcppoauth.RandomToken(32)
	if err != nil {
		http.Error(w, "Unable to create session.", http.StatusInternalServerError)
		return
	}
	now := time.Now()
	sessionHash := sha256.Sum256([]byte(sessionToken))
	if err := h.DB.CreateAuthSession(sessionHash[:], db.AuthSession{
		PersonID: identity.ID, DisplayName: identity.Name, AccessToken: tokens.AccessToken,
		RefreshToken: tokens.RefreshToken, AccessExpiresAt: tokens.ExpiresAt, RoleCheckedAt: now,
		CSRFToken: csrfToken, CreatedAt: now, ExpiresAt: now.Add(sessionTTL), LastSeenAt: now,
	}); err != nil {
		http.Error(w, "Unable to create session.", http.StatusInternalServerError)
		return
	}
	h.setSessionCookie(w, r, sessionToken, int(sessionTTL.Seconds()))
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func (h *Handler) logout(w http.ResponseWriter, r *http.Request) {
	principal := requestAuthentication(r)
	if principal != nil && principal.Session != nil {
		_ = h.DB.DeleteAuthSession(principal.SessionHash)
		if h.OAuth != nil {
			ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
			defer cancel()
			_ = h.OAuth.Revoke(ctx, principal.Session.AccessToken)
			_ = h.OAuth.Revoke(ctx, principal.Session.RefreshToken)
		}
	}
	h.clearCookie(w, r, sessionCookieName, "/")
	http.Redirect(w, r, "/login", http.StatusSeeOther)
}

func (h *Handler) renderLogin(w http.ResponseWriter, r *http.Request, errorMessage string) {
	h.renderStatus(w, r, http.StatusOK, "login.html", map[string]any{
		"Error": errorMessage, "OAuthEnabled": h.OAuth != nil, "BreakGlassEnabled": h.Secret != "",
	})
}

func (h *Handler) setSessionCookie(w http.ResponseWriter, r *http.Request, value string, maxAge int) {
	http.SetCookie(w, &http.Cookie{
		Name: sessionCookieName, Value: value, Path: "/", HttpOnly: true, Secure: requestIsHTTPS(r),
		SameSite: http.SameSiteLaxMode, MaxAge: maxAge,
	})
}

func (h *Handler) clearCookie(w http.ResponseWriter, r *http.Request, name, path string) {
	http.SetCookie(w, &http.Cookie{
		Name: name, Value: "", Path: path, HttpOnly: true, Secure: requestIsHTTPS(r),
		SameSite: http.SameSiteLaxMode, MaxAge: -1,
	})
}

func (h *Handler) breakGlassCSRF() string {
	digest := sha256.Sum256([]byte("streamctl-break-glass-csrf\x00" + h.Secret))
	return base64.RawURLEncoding.EncodeToString(digest[:])
}

func requestAuthentication(r *http.Request) *requestAuth {
	principal, _ := r.Context().Value(requestAuthKey{}).(*requestAuth)
	return principal
}

func csrfField(r *http.Request) template.HTML {
	principal := requestAuthentication(r)
	if principal == nil || principal.CSRFToken == "" {
		return ""
	}
	return template.HTML(`<input type="hidden" name="csrf_token" value="` + template.HTMLEscapeString(principal.CSRFToken) + `">`)
}

func hasGlobalAdminRole(roles []string) bool {
	for _, role := range roles {
		if role == "global-admin" {
			return true
		}
	}
	return false
}

func safeLocalNext(next string) string {
	next = strings.TrimSpace(next)
	if next == "" || !strings.HasPrefix(next, "/") || strings.HasPrefix(next, "//") {
		return "/"
	}
	return next
}

func isUnsafeMethod(method string) bool {
	return method != http.MethodGet && method != http.MethodHead && method != http.MethodOptions
}

func requestIsHTTPS(r *http.Request) bool {
	return r.TLS != nil || strings.EqualFold(strings.TrimSpace(r.Header.Get("X-Forwarded-Proto")), "https")
}

func logHandlerError(action string, err error) {
	if err != nil {
		log.Printf("streamctl %s: %v", action, err)
	}
}
