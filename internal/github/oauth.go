package github

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// WebBaseURL derives the GitHub website from the REST API base URL:
// https://api.github.com -> https://github.com, https://HOST/api/v3 -> https://HOST.
// Any other base (such as a test server) is used as is.
func WebBaseURL(apiBase string) string {
	apiBase = strings.TrimRight(strings.TrimSpace(apiBase), "/")
	switch {
	case apiBase == "" || apiBase == DefaultBaseURL:
		return "https://github.com"
	case strings.HasSuffix(apiBase, "/api/v3"):
		return strings.TrimSuffix(apiBase, "/api/v3")
	}
	return apiBase
}

// AuthorizeURL is where a person is sent to sign in with the GitHub App.
func AuthorizeURL(webBase, clientID, redirectURI, state string) string {
	q := url.Values{"client_id": {clientID}, "redirect_uri": {redirectURI}, "state": {state}, "allow_signup": {"false"}}
	return strings.TrimRight(webBase, "/") + "/login/oauth/authorize?" + q.Encode()
}

var oauthClient = &http.Client{Timeout: 30 * time.Second}

// ExchangeCode trades the code GitHub returned after sign-in for a user access token.
func ExchangeCode(ctx context.Context, webBase, clientID, clientSecret, code, redirectURI string) (string, error) {
	form := url.Values{"client_id": {clientID}, "client_secret": {clientSecret}, "code": {code}, "redirect_uri": {redirectURI}}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(webBase, "/")+"/login/oauth/access_token",
		strings.NewReader(form.Encode()))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "llm-pr-bot")
	resp, err := oauthClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	var r struct {
		AccessToken string `json:"access_token"`
		Error       string `json:"error"`
		Description string `json:"error_description"`
	}
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err := json.Unmarshal(data, &r); err != nil {
		return "", fmt.Errorf("GitHub sign-in: unexpected response (HTTP %d)", resp.StatusCode)
	}
	if r.Error != "" {
		return "", fmt.Errorf("GitHub sign-in: %s (%s)", r.Description, r.Error)
	}
	if r.AccessToken == "" {
		return "", fmt.Errorf("GitHub sign-in: no access token (HTTP %d)", resp.StatusCode)
	}
	return r.AccessToken, nil
}

// UserClient calls the API as a signed-in person, with a GitHub App user access token.
type UserClient struct {
	app   *App // only its HTTP plumbing and base URL are used
	token string
}

// AsUser returns a client that acts with a user access token.
func (a *App) AsUser(token string) *UserClient { return &UserClient{app: a, token: token} }

// NewUserClient builds a user client without App credentials (only REST base URL needed).
func NewUserClient(apiBase, token string) *UserClient {
	apiBase = strings.TrimRight(strings.TrimSpace(apiBase), "/")
	if apiBase == "" {
		apiBase = DefaultBaseURL
	}
	a := &App{base: apiBase, hc: apiHTTPClient(apiBase, 60*time.Second)}
	a.rate.Store(-1)
	return &UserClient{app: a, token: token}
}

func (u *UserClient) get(ctx context.Context, path string, out any) (*http.Response, error) {
	resp, data, err := u.app.send(ctx, http.MethodGet, path, "Bearer "+u.token, nil, nil)
	if err != nil {
		return resp, err
	}
	if out != nil {
		if err := json.Unmarshal(data, out); err != nil {
			return resp, fmt.Errorf("decode %s: %w", path, err)
		}
	}
	return resp, nil
}

// Me returns the signed-in person's profile.
func (u *UserClient) Me(ctx context.Context) (*User, error) {
	var me User
	if _, err := u.get(ctx, "/user", &me); err != nil {
		return nil, err
	}
	if me.ID == 0 || me.Login == "" {
		return nil, errors.New("GitHub returned an empty profile")
	}
	return &me, nil
}

// Installations lists the App's installations the person can access.
func (u *UserClient) Installations(ctx context.Context) ([]Installation, error) {
	var all []Installation
	next := "/user/installations?per_page=100"
	for page := 0; next != "" && page < 20; page++ {
		var r struct {
			Installations []Installation `json:"installations"`
		}
		resp, err := u.get(ctx, next, &r)
		if err != nil {
			return nil, err
		}
		all = append(all, r.Installations...)
		next = nextLink(resp.Header)
	}
	return all, nil
}

// Membership is the person's role in a GitHub organization.
type Membership struct {
	State string `json:"state"` // active | pending
	Role  string `json:"role"`  // admin | member | billing_manager
}

// OrgMembership returns the person's membership of org. It needs the App's
// "Members: Read-only" organization permission. A 404 means not a member.
func (u *UserClient) OrgMembership(ctx context.Context, org string) (*Membership, error) {
	var m Membership
	if _, err := u.get(ctx, "/user/memberships/orgs/"+url.PathEscape(org), &m); err != nil {
		return nil, err
	}
	return &m, nil
}

// VerifySignature checks a webhook's X-Hub-Signature-256 header against the secret.
func VerifySignature(secret string, body []byte, header string) bool {
	sig, ok := strings.CutPrefix(header, "sha256=")
	if !ok || secret == "" {
		return false
	}
	got, err := hex.DecodeString(sig)
	if err != nil {
		return false
	}
	m := hmac.New(sha256.New, []byte(secret))
	m.Write(body)
	return hmac.Equal(got, m.Sum(nil))
}

// Sign computes the X-Hub-Signature-256 header for a body (used by tests and tools).
func Sign(secret string, body []byte) string {
	m := hmac.New(sha256.New, []byte(secret))
	m.Write(body)
	return "sha256=" + hex.EncodeToString(m.Sum(nil))
}

// WebhookEvent is the subset of a webhook payload the bot reads. Which
// fields are set depends on the event.
type WebhookEvent struct {
	Action       string        `json:"action"`
	Installation *Installation `json:"installation"`
	Repository   *Repo         `json:"repository"`
	PullRequest  *PullRequest  `json:"pull_request"`
	Issue        *struct {
		Number      int       `json:"number"`
		PullRequest *struct{} `json:"pull_request"` // set when the issue is a PR
	} `json:"issue"`
	Comment *struct {
		ID   int64 `json:"id"`
		User User  `json:"user"`
	} `json:"comment"`
	Sender User `json:"sender"`
}
