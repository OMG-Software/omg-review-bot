// Package chatgpt signs a ChatGPT account in so the bot can use its plan's
// models: Sign in with ChatGPT's plan usage for open-source apps. It's an
// OAuth authorization code with PKCE, a public client OpenAI registers at the
// first sign-in, an ID token checked against OpenAI's published keys, and
// access tokens renewed with a rotating refresh token.
//
// The sign-in ends on a 127.0.0.1 address: OpenAI accepts only a loopback
// callback for this flow. The bot usually runs elsewhere, so the browser
// can't load that page, and the person pastes its address back into the bot,
// which exchanges the code it holds the PKCE verifier for.
//
// See https://developers.openai.com/siwc/token-sharing-open-source.
package chatgpt

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync"
	"time"
)

const (
	// AppName is the name the bot registers under, shown to the person
	// approving the sign-in (they may change it).
	AppName = "OMG Review Bot"
	// RedirectURI is where the sign-in ends: a loopback address, as the
	// flow requires.
	RedirectURI = "http://127.0.0.1:1455/auth/callback"
	// Resource is the API the tokens are for.
	Resource = "https://api.openai.com/v1"
	// PlanScope is the permission to use the account's ChatGPT plan.
	PlanScope = "chatgpt.tokens.use.direct"
	// dynamicClient starts a first sign-in, which registers a client.
	dynamicClient = "dynamic_agent_client"
	scopes        = "openid profile email offline_access resource.invoke " + PlanScope
)

// Endpoints are OpenAI's sign-in addresses (tests use their own).
type Endpoints struct {
	Issuer, Authorize, Token, Revoke, JWKS string
}

// Production are the endpoints of OpenAI's discovery document,
// https://auth.openai.com/.well-known/openid-configuration.
var Production = Endpoints{
	Issuer:    "https://auth.openai.com",
	Authorize: "https://auth.openai.com/api/accounts/authorize",
	Token:     "https://auth.openai.com/api/accounts/oauth/token",
	Revoke:    "https://auth.openai.com/api/accounts/oauth/revoke",
	JWKS:      "https://auth.openai.com/.well-known/jwks.json",
}

var (
	// ErrDenied: the person declined the sign-in or the plan's use.
	ErrDenied = errors.New("the ChatGPT sign-in was declined")
	// ErrNoPlanUsage: signed in, but without the permission to use the
	// ChatGPT plan, which is what the bot signs in for.
	ErrNoPlanUsage = errors.New("signed in, but use of the ChatGPT plan wasn't allowed: sign in again and allow it")
	// ErrSignInEnded: the refresh token can't be used any more (expired,
	// revoked or replaced): the account has to be signed in again.
	ErrSignInEnded = errors.New("the ChatGPT sign-in has expired or was revoked: sign in again on the Models page")
	// ErrOtherAccount: a returning client's sign-in came back for another
	// account than the one it was registered for.
	ErrOtherAccount = errors.New("that's another ChatGPT account than the one the model was signed in with")
)

// Client talks to OpenAI's sign-in endpoints.
type Client struct {
	E   Endpoints
	HC  *http.Client
	Now func() time.Time

	mu     sync.Mutex
	keys   map[string]*rsa.PublicKey // OpenAI's signing keys, by key ID
	keysAt time.Time                 // when they were last fetched
}

// New returns a Client for OpenAI's production endpoints.
func New() *Client {
	return &Client{E: Production, HC: &http.Client{
		Timeout: 30 * time.Second,
		// Codes and tokens go to OpenAI only.
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}}
}

func (c *Client) now() time.Time {
	if c.Now != nil {
		return c.Now()
	}
	return time.Now()
}

// Attempt is one sign-in in progress: what its callback is checked against.
// Keep it until the callback comes (only on the server: it holds the PKCE
// verifier).
type Attempt struct {
	State, Nonce, Verifier string
	// ClientID is the client issued at the account's first sign-in, to sign
	// it in again; "" registers a new one.
	ClientID  string
	LoginHint string // the account's email, to sign the same one in again
	// Subject is the account (the ID token's sub) ClientID was registered
	// for: a sign-in for any other is refused. "" checks nothing.
	Subject string
	HostID  string // this bot's host ID (see NewHostID)
	// Consent asks for consent again: the plan's use was declined last time.
	Consent bool
	Created time.Time
}

// NewAttempt starts a sign-in on the host hostID. clientID and loginHint
// are a returning account's, or "".
func NewAttempt(hostID, clientID, loginHint string) (Attempt, error) {
	a := Attempt{ClientID: clientID, LoginHint: loginHint, HostID: hostID, Created: time.Now()}
	for _, p := range []*string{&a.State, &a.Nonce, &a.Verifier} {
		v, err := random(32)
		if err != nil {
			return Attempt{}, err
		}
		*p = v
	}
	return a, nil
}

// NewHostID makes a host ID: one per bot installation, kept for good.
func NewHostID() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	b[6] = b[6]&0x0f | 0x40 // a version 4 UUID
	b[8] = b[8]&0x3f | 0x80
	return fmt.Sprintf("urn:uuid:%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:]), nil
}

func random(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// AuthorizeURL is the address that starts the attempt's sign-in in the
// browser.
func (c *Client) AuthorizeURL(a Attempt) string {
	q := url.Values{}
	if a.ClientID == "" {
		q.Set("client_id", dynamicClient)
		q.Set("agent_name_hint", AppName)
	} else {
		q.Set("client_id", a.ClientID)
		if a.LoginHint != "" {
			q.Set("login_hint", a.LoginHint)
		}
		if a.Consent {
			q.Set("prompt", "consent")
		}
	}
	q.Set("ext_agent_host_id", a.HostID)
	q.Set("response_type", "code")
	q.Set("redirect_uri", RedirectURI)
	q.Set("scope", scopes)
	q.Set("resource", Resource)
	q.Set("state", a.State)
	q.Set("nonce", a.Nonce)
	q.Set("code_challenge_method", "S256")
	sum := sha256.Sum256([]byte(a.Verifier))
	q.Set("code_challenge", base64.RawURLEncoding.EncodeToString(sum[:]))
	// Spaces as %20: a '+' in a value is already encoded as %2B.
	return c.E.Authorize + "?" + strings.ReplaceAll(q.Encode(), "+", "%20")
}

// Callback is what the sign-in's callback address carries.
type Callback struct {
	Code, State, ClientID, Error, ErrorDescription string
}

// ParseCallback reads the callback address the person pasted (or just its
// query).
func ParseCallback(raw string) (Callback, error) {
	raw = strings.TrimSpace(raw)
	if i := strings.IndexByte(raw, '#'); i >= 0 {
		raw = raw[:i]
	}
	if i := strings.IndexByte(raw, '?'); i >= 0 {
		raw = raw[i+1:]
	}
	q, err := url.ParseQuery(raw)
	if err != nil {
		return Callback{}, fmt.Errorf("that isn't the address the sign-in ended on: %w", err)
	}
	cb := Callback{Code: q.Get("code"), State: q.Get("state"), ClientID: q.Get("client_id"),
		Error: q.Get("error"), ErrorDescription: q.Get("error_description")}
	if cb.State == "" || (cb.Code == "" && cb.Error == "") {
		return Callback{}, errors.New("that isn't the address the sign-in ended on: copy the whole address of the 127.0.0.1 page, starting http://127.0.0.1")
	}
	return cb, nil
}

// Credentials are a signed-in account's tokens and who it is.
type Credentials struct {
	Email    string `json:"email"`
	Subject  string `json:"subject"`   // the ID token's sub: the account
	ClientID string `json:"client_id"` // issued at the first sign-in
	HostID   string `json:"host_id"`

	IDToken      string    `json:"id_token"`
	AccessToken  string    `json:"access_token"`
	RefreshToken string    `json:"refresh_token"`
	Scopes       []string  `json:"scopes"`
	Expiry       time.Time `json:"expiry"`   // the access token's
	SavedAt      time.Time `json:"saved_at"` // when the last token response came
}

// PlanUsage reports whether the sign-in may use the ChatGPT plan.
func (c Credentials) PlanUsage() bool { return slices.Contains(c.Scopes, PlanScope) }

// Finish checks the callback against its attempt, exchanges the code and
// checks the ID token. The credentials are returned with ErrNoPlanUsage if
// the plan's use wasn't granted, or ErrOtherAccount if they're for another
// account than a.Subject: either way, the caller should revoke them.
func (c *Client) Finish(ctx context.Context, a Attempt, cb Callback) (Credentials, error) {
	if cb.State != a.State {
		return Credentials{}, errors.New("that address is from another sign-in: start again")
	}
	if cb.Error != "" {
		if cb.Error == "access_denied" {
			return Credentials{}, ErrDenied
		}
		return Credentials{}, fmt.Errorf("the ChatGPT sign-in failed: %s %s", cb.Error, cb.ErrorDescription)
	}
	clientID := a.ClientID
	switch {
	case clientID == "" && (cb.ClientID == "" || cb.ClientID == dynamicClient):
		return Credentials{}, errors.New("the ChatGPT sign-in didn't register the app (no client ID came back): start again")
	case clientID == "":
		clientID = cb.ClientID
	case cb.ClientID != "" && cb.ClientID != clientID:
		return Credentials{}, errors.New("the ChatGPT sign-in came back for another app registration: start again")
	}
	tr, err := c.token(ctx, url.Values{
		"grant_type":    {"authorization_code"},
		"client_id":     {clientID},
		"code":          {cb.Code},
		"code_verifier": {a.Verifier},
		"redirect_uri":  {RedirectURI},
		"resource":      {Resource},
	})
	if err != nil {
		return Credentials{}, fmt.Errorf("couldn't finish the ChatGPT sign-in: %w", err)
	}
	id, err := c.verifyIDToken(ctx, tr.IDToken, clientID, a.Nonce)
	if err != nil {
		return Credentials{}, fmt.Errorf("the ChatGPT sign-in's ID token isn't valid: %w", err)
	}
	cred := Credentials{Email: id.Email, Subject: id.Subject, ClientID: clientID, HostID: a.HostID}
	c.apply(&cred, tr)
	if cred.RefreshToken == "" {
		return Credentials{}, errors.New("the ChatGPT sign-in returned no refresh token, so it couldn't be renewed: start again")
	}
	// A client is registered for one account: signing that client in again
	// must sign the same account in, not replace it with another.
	if a.Subject != "" && cred.Subject != a.Subject {
		return cred, ErrOtherAccount
	}
	if !cred.PlanUsage() {
		return cred, ErrNoPlanUsage
	}
	return cred, nil
}

// Refresh renews cred's access token. Each renewal replaces the refresh
// token too, so the caller must keep the new cred (and never refresh one
// session twice at once).
//
// A renewal that comes back without the replacement has still used up the
// old refresh token, so cred is left with none: its new access token works
// until it expires, and then the sign-in has ended.
func (c *Client) Refresh(ctx context.Context, cred *Credentials) error {
	tr, err := c.token(ctx, url.Values{
		"grant_type":    {"refresh_token"},
		"client_id":     {cred.ClientID},
		"refresh_token": {cred.RefreshToken},
		"resource":      {Resource},
	})
	if err != nil {
		var oe *OAuthError
		if errors.As(err, &oe) && oe.ended() {
			return fmt.Errorf("%w (%s)", ErrSignInEnded, oe.Code)
		}
		return fmt.Errorf("couldn't renew the ChatGPT sign-in: %w", err)
	}
	c.apply(cred, tr)
	if tr.RefreshToken == "" {
		cred.RefreshToken = ""
	}
	return nil
}

// Temporary reports whether a failed call to OpenAI's sign-in may work if
// tried again: the network or OpenAI failed, rather than the request.
func Temporary(err error) bool {
	var oe *OAuthError
	if !errors.As(err, &oe) {
		return !errors.Is(err, context.Canceled)
	}
	return oe.Status >= 500 || oe.Status == http.StatusTooManyRequests
}

// Revoke ends cred's renewable session at OpenAI.
func (c *Client) Revoke(ctx context.Context, cred Credentials) error {
	if cred.RefreshToken == "" {
		return nil
	}
	form := url.Values{"token": {cred.RefreshToken}, "token_type_hint": {"refresh_token"}, "client_id": {cred.ClientID}}
	resp, err := c.post(ctx, c.E.Revoke, form)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return oauthError(resp)
	}
	return nil
}

type tokenResponse struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	IDToken      string `json:"id_token"`
	ExpiresIn    int64  `json:"expires_in"`
	Scope        string `json:"scope"`
}

// apply puts a token response's tokens in cred. A refresh may leave some
// out (no new ID token, the same scopes): those are kept.
func (c *Client) apply(cred *Credentials, tr tokenResponse) {
	now := c.now()
	cred.AccessToken = tr.AccessToken
	if tr.RefreshToken != "" {
		cred.RefreshToken = tr.RefreshToken
	}
	if tr.IDToken != "" {
		cred.IDToken = tr.IDToken
	}
	if s := strings.Fields(tr.Scope); len(s) > 0 {
		cred.Scopes = s
	}
	exp := tr.ExpiresIn
	if exp <= 0 {
		exp = 3600
	}
	cred.Expiry = now.Add(time.Duration(exp) * time.Second)
	cred.SavedAt = now
}

// OAuthError is an error answer from the token or revocation endpoint.
type OAuthError struct {
	Status      int
	Code        string
	Description string
}

func (e *OAuthError) Error() string {
	s := fmt.Sprintf("HTTP %d", e.Status)
	if e.Code != "" {
		s += ": " + e.Code
	}
	if e.Description != "" {
		s += ": " + e.Description
	}
	return s
}

// ended reports whether the error means the refresh token is done with.
func (e *OAuthError) ended() bool {
	switch e.Code {
	case "invalid_grant", "invalid_refresh_token", "token_expired", "refresh_token_expired",
		"refresh_token_invalidated", "refresh_token_reused", "invalid_client":
		return true
	}
	return false
}

func oauthError(resp *http.Response) error {
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	var e struct {
		Error            any    `json:"error"`
		ErrorDescription string `json:"error_description"`
		Code             string `json:"code"`
		Message          string `json:"message"`
	}
	oe := &OAuthError{Status: resp.StatusCode}
	if json.Unmarshal(data, &e) == nil {
		switch v := e.Error.(type) {
		case string:
			oe.Code = v
		case map[string]any: // {"error": {"code": ..., "message": ...}}
			oe.Code, _ = v["code"].(string)
			oe.Description, _ = v["message"].(string)
		}
		if oe.Code == "" {
			oe.Code = e.Code
		}
		if oe.Description == "" {
			oe.Description = e.ErrorDescription
		}
		if oe.Description == "" {
			oe.Description = e.Message
		}
	} else {
		oe.Description = strings.TrimSpace(string(data))
		if len(oe.Description) > 300 {
			oe.Description = oe.Description[:300]
		}
	}
	return oe
}

func (c *Client) post(ctx context.Context, u string, form url.Values) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	return c.HC.Do(req)
}

func (c *Client) token(ctx context.Context, form url.Values) (tokenResponse, error) {
	resp, err := c.post(ctx, c.E.Token, form)
	if err != nil {
		return tokenResponse{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return tokenResponse{}, oauthError(resp)
	}
	var tr tokenResponse
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&tr); err != nil {
		return tokenResponse{}, fmt.Errorf("decode the token response: %w", err)
	}
	if tr.AccessToken == "" {
		return tokenResponse{}, errors.New("the token response has no access token")
	}
	return tr, nil
}

// ---------- the ID token ----------

type identity struct{ Subject, Email string }

// clockSkew is how far OpenAI's clock and the bot's may differ.
const clockSkew = 2 * time.Minute

// verifyIDToken checks the ID token's RS256 signature against OpenAI's keys,
// its issuer, audience (the client), expiry and nonce, and returns who it
// names.
func (c *Client) verifyIDToken(ctx context.Context, tok, clientID, nonce string) (identity, error) {
	parts := strings.Split(tok, ".")
	if len(parts) != 3 {
		return identity{}, errors.New("it isn't a JWT")
	}
	var hdr struct {
		Alg string `json:"alg"`
		Kid string `json:"kid"`
	}
	if err := decodePart(parts[0], &hdr); err != nil {
		return identity{}, err
	}
	if hdr.Alg != "RS256" {
		return identity{}, fmt.Errorf("it's signed with %q, not RS256", hdr.Alg)
	}
	key, err := c.key(ctx, hdr.Kid)
	if err != nil {
		return identity{}, err
	}
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return identity{}, fmt.Errorf("its signature isn't base64url: %w", err)
	}
	// RS256 is RSASSA-PKCS1-v1_5 with SHA-256 (RFC 7518, section 3.3), the
	// only signature the header check above lets through. Sonar's rule on
	// PKCS #1 v1.5 is about encryption padding, which this isn't: PSS would
	// be another algorithm (PS256), and OpenAI doesn't sign with it.
	sum := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if err := rsa.VerifyPKCS1v15(key, crypto.SHA256, sum[:], sig); err != nil { // NOSONAR: an RS256 signature, not encryption
		return identity{}, errors.New("its signature doesn't match OpenAI's key")
	}
	var cl struct {
		Iss   string          `json:"iss"`
		Sub   string          `json:"sub"`
		Aud   json.RawMessage `json:"aud"`
		Exp   int64           `json:"exp"`
		Nonce string          `json:"nonce"`
		Email string          `json:"email"`
	}
	if err := decodePart(parts[1], &cl); err != nil {
		return identity{}, err
	}
	var auds []string
	var one string
	if json.Unmarshal(cl.Aud, &one) == nil {
		auds = []string{one}
	} else if json.Unmarshal(cl.Aud, &auds) != nil {
		return identity{}, errors.New("its audience can't be read")
	}
	now := c.now()
	switch {
	case cl.Iss != c.E.Issuer:
		return identity{}, fmt.Errorf("it was issued by %q, not %q", cl.Iss, c.E.Issuer)
	case !slices.Contains(auds, clientID):
		return identity{}, errors.New("it was issued for another client")
	case cl.Exp == 0 || now.After(time.Unix(cl.Exp, 0).Add(clockSkew)):
		return identity{}, errors.New("it has expired")
	case cl.Nonce != nonce:
		return identity{}, errors.New("its nonce isn't this sign-in's")
	case cl.Sub == "":
		return identity{}, errors.New("it names no account")
	}
	return identity{Subject: cl.Sub, Email: cl.Email}, nil
}

func decodePart(part string, v any) error {
	b, err := base64.RawURLEncoding.DecodeString(part)
	if err != nil {
		return fmt.Errorf("a part of it isn't base64url: %w", err)
	}
	if err := json.Unmarshal(b, v); err != nil {
		return fmt.Errorf("a part of it isn't JSON: %w", err)
	}
	return nil
}

// key returns OpenAI's signing key kid, fetching the keys again if it's one
// not seen yet (keys rotate), at most once a minute.
func (c *Client) key(ctx context.Context, kid string) (*rsa.PublicKey, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if k := c.keys[kid]; k != nil {
		return k, nil
	}
	if c.keys != nil && c.now().Sub(c.keysAt) < time.Minute {
		return nil, fmt.Errorf("OpenAI has no signing key %q", kid)
	}
	keys, err := c.fetchKeys(ctx)
	if err != nil {
		return nil, fmt.Errorf("couldn't get OpenAI's signing keys: %w", err)
	}
	c.keys, c.keysAt = keys, c.now()
	if k := keys[kid]; k != nil {
		return k, nil
	}
	return nil, fmt.Errorf("OpenAI has no signing key %q", kid)
}

func (c *Client) fetchKeys(ctx context.Context) (map[string]*rsa.PublicKey, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.E.JWKS, nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.HC.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	var set struct {
		Keys []struct {
			Kty, Kid, N, E string
		} `json:"keys"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&set); err != nil {
		return nil, err
	}
	out := map[string]*rsa.PublicKey{}
	for _, k := range set.Keys {
		if k.Kty != "RSA" {
			continue
		}
		n, err1 := base64.RawURLEncoding.DecodeString(k.N)
		e, err2 := base64.RawURLEncoding.DecodeString(k.E)
		if err1 != nil || err2 != nil || len(e) == 0 || len(e) > 4 {
			continue
		}
		exp := 0
		for _, b := range e {
			exp = exp<<8 | int(b)
		}
		out[k.Kid] = &rsa.PublicKey{N: new(big.Int).SetBytes(n), E: exp}
	}
	return out, nil
}
