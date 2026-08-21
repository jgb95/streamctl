package btcppoauth

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

const maxResponseBytes = 1 << 20

type Client struct {
	BaseURL      string
	ClientID     string
	ClientSecret string
	RedirectURL  string
	HTTPClient   *http.Client
}

type TokenSet struct {
	AccessToken  string
	RefreshToken string
	ExpiresAt    time.Time
	Scopes       []string
}

type Identity struct {
	ID    string   `json:"id"`
	Name  string   `json:"name"`
	Roles []string `json:"roles"`
}

func SecretFromFile(path string) (string, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return "", fmt.Errorf("Bitcoin++ OAuth client secret file is required")
	}
	info, err := os.Stat(path)
	if err != nil {
		return "", fmt.Errorf("stat Bitcoin++ OAuth client secret file: %w", err)
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0o077 != 0 {
		return "", fmt.Errorf("Bitcoin++ OAuth client secret must be a regular file inaccessible by group and other users")
	}
	contents, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("read Bitcoin++ OAuth client secret file: %w", err)
	}
	secret := strings.TrimSpace(string(contents))
	if secret == "" {
		return "", fmt.Errorf("Bitcoin++ OAuth client secret file is empty")
	}
	return secret, nil
}

func (c *Client) Validate() error {
	_, err := c.endpoint("/oauth/authorize")
	return err
}

func RandomToken(bytes int) (string, error) {
	value := make([]byte, bytes)
	if _, err := rand.Read(value); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(value), nil
}

func PKCEChallenge(verifier string) string {
	digest := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(digest[:])
}

func (c *Client) AuthorizationURL(state, verifier string) (string, error) {
	endpoint, err := c.endpoint("/oauth/authorize")
	if err != nil {
		return "", err
	}
	query := endpoint.Query()
	query.Set("client_id", c.ClientID)
	query.Set("redirect_uri", c.RedirectURL)
	query.Set("response_type", "code")
	query.Set("scope", "identity:self:read offline_access")
	query.Set("state", state)
	query.Set("code_challenge", PKCEChallenge(verifier))
	query.Set("code_challenge_method", "S256")
	endpoint.RawQuery = query.Encode()
	return endpoint.String(), nil
}

func (c *Client) Exchange(ctx context.Context, code, verifier string) (*TokenSet, error) {
	return c.token(ctx, url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {code},
		"redirect_uri":  {c.RedirectURL},
		"code_verifier": {verifier},
	})
}

func (c *Client) Refresh(ctx context.Context, refreshToken string) (*TokenSet, error) {
	return c.token(ctx, url.Values{
		"grant_type":    {"refresh_token"},
		"refresh_token": {refreshToken},
	})
}

func (c *Client) Identity(ctx context.Context, accessToken string) (*Identity, error) {
	endpoint, err := c.endpoint("/api/v1/me/identity")
	if err != nil {
		return nil, err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint.String(), nil)
	if err != nil {
		return nil, err
	}
	request.Header.Set("Accept", "application/json")
	request.Header.Set("Authorization", "Bearer "+accessToken)
	response, err := c.httpClient().Do(request)
	if err != nil {
		return nil, fmt.Errorf("load bitcoin++ identity: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nil, responseError("load bitcoin++ identity", response)
	}
	var envelope struct {
		Data Identity `json:"data"`
	}
	if err := decodeLimited(response.Body, &envelope); err != nil {
		return nil, fmt.Errorf("decode bitcoin++ identity: %w", err)
	}
	if strings.TrimSpace(envelope.Data.ID) == "" {
		return nil, fmt.Errorf("bitcoin++ identity omitted id")
	}
	return &envelope.Data, nil
}

func (c *Client) Revoke(ctx context.Context, token string) error {
	if strings.TrimSpace(token) == "" {
		return nil
	}
	endpoint, err := c.endpoint("/oauth/revoke")
	if err != nil {
		return err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint.String(), strings.NewReader(url.Values{"token": {token}}.Encode()))
	if err != nil {
		return err
	}
	request.SetBasicAuth(c.ClientID, c.ClientSecret)
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	response, err := c.httpClient().Do(request)
	if err != nil {
		return fmt.Errorf("revoke bitcoin++ OAuth token: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return responseError("revoke bitcoin++ OAuth token", response)
	}
	return nil
}

func (c *Client) token(ctx context.Context, form url.Values) (*TokenSet, error) {
	endpoint, err := c.endpoint("/oauth/token")
	if err != nil {
		return nil, err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint.String(), strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	request.SetBasicAuth(c.ClientID, c.ClientSecret)
	request.Header.Set("Accept", "application/json")
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	response, err := c.httpClient().Do(request)
	if err != nil {
		return nil, fmt.Errorf("exchange bitcoin++ OAuth token: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nil, responseError("exchange bitcoin++ OAuth token", response)
	}
	var body struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		ExpiresIn    int    `json:"expires_in"`
		Scope        string `json:"scope"`
		TokenType    string `json:"token_type"`
	}
	if err := decodeLimited(response.Body, &body); err != nil {
		return nil, fmt.Errorf("decode bitcoin++ OAuth token: %w", err)
	}
	if body.AccessToken == "" || !strings.EqualFold(body.TokenType, "Bearer") || body.ExpiresIn <= 0 {
		return nil, fmt.Errorf("bitcoin++ OAuth token response is incomplete")
	}
	return &TokenSet{
		AccessToken: body.AccessToken, RefreshToken: body.RefreshToken,
		ExpiresAt: time.Now().Add(time.Duration(body.ExpiresIn) * time.Second), Scopes: strings.Fields(body.Scope),
	}, nil
}

func (c *Client) endpoint(path string) (*url.URL, error) {
	base, err := url.Parse(strings.TrimRight(strings.TrimSpace(c.BaseURL), "/"))
	if err != nil || (base.Scheme != "https" && base.Scheme != "http") || base.Host == "" {
		return nil, fmt.Errorf("invalid bitcoin++ OAuth base URL")
	}
	if strings.TrimSpace(c.ClientID) == "" || strings.TrimSpace(c.ClientSecret) == "" {
		return nil, fmt.Errorf("bitcoin++ OAuth client credentials are required")
	}
	redirect, err := url.Parse(c.RedirectURL)
	if err != nil || (redirect.Scheme != "https" && redirect.Scheme != "http") || redirect.Host == "" {
		return nil, fmt.Errorf("invalid streamctl OAuth redirect URL")
	}
	base.Path = strings.TrimRight(base.Path, "/") + path
	base.RawQuery = ""
	base.Fragment = ""
	return base, nil
}

func (c *Client) httpClient() *http.Client {
	if c.HTTPClient != nil {
		return c.HTTPClient
	}
	return &http.Client{Timeout: 15 * time.Second}
}

func decodeLimited(reader io.Reader, value any) error {
	return json.NewDecoder(io.LimitReader(reader, maxResponseBytes)).Decode(value)
}

func responseError(action string, response *http.Response) error {
	data, _ := io.ReadAll(io.LimitReader(response.Body, 4096))
	data = bytes.TrimSpace(data)
	if len(data) == 0 {
		return fmt.Errorf("%s: HTTP %d", action, response.StatusCode)
	}
	return fmt.Errorf("%s: HTTP %d: %s", action, response.StatusCode, data)
}
