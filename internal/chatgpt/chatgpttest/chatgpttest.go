// Package chatgpttest is a stand-in for OpenAI's sign-in, for tests: it
// registers clients, approves sign-ins, issues RS256 ID tokens and rotating
// refresh tokens, and records revocations.
package chatgpttest

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"math/big"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/OMG-Software/omg-review-bot/internal/chatgpt"
)

// Server is the stand-in. Change its fields before the sign-in they're for.
type Server struct {
	*httptest.Server
	Key *rsa.PrivateKey

	mu sync.Mutex
	// ClientID is the client a first sign-in registers.
	ClientID string
	// Scopes are those granted (all asked for, by default).
	Scopes string
	// Subject and Email are who signs in.
	Subject, Email string
	// ExpiresIn is each access token's lifetime, in seconds.
	ExpiresIn int
	// Nonce, if set, replaces the sign-in's nonce in the ID token.
	Nonce string
	// FailRevokes is how many revocations to come fail with a 503.
	FailRevokes int
	// NoReplacement leaves the replacement refresh token out of renewals
	// (still using up the old one).
	NoReplacement bool
	// OnRefresh, if set, runs as each renewal is answered.
	OnRefresh func()

	codes   map[string]grant
	live    map[string]string // refresh token → client
	used    map[string]bool   // refresh tokens replaced
	revoked []string
	n       int
	// Refreshes counts the refresh grants made.
	Refreshes int
	// LastAuthorize is the query of the last sign-in approved.
	LastAuthorize url.Values
}

type grant struct{ client, challenge, nonce, redirect string }

// New starts a stand-in, closed when the test ends.
func New(t testing.TB) *Server {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	s := &Server{Key: key, ClientID: "client-abc", Subject: "user-123", Email: "dev@example.com", ExpiresIn: 3600,
		Scopes: "openid profile email offline_access resource.invoke " + chatgpt.PlanScope,
		codes:  map[string]grant{}, live: map[string]string{}, used: map[string]bool{}}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /token", s.token)
	mux.HandleFunc("POST /revoke", s.revoke)
	mux.HandleFunc("GET /jwks", s.jwks)
	s.Server = httptest.NewServer(mux)
	t.Cleanup(s.Close)
	return s
}

// Endpoints are the stand-in's addresses.
func (s *Server) Endpoints() chatgpt.Endpoints {
	return chatgpt.Endpoints{Issuer: s.URL, Authorize: s.URL + "/authorize", Token: s.URL + "/token", Revoke: s.URL + "/revoke", JWKS: s.URL + "/jwks"}
}

// Client is a sign-in client that talks to the stand-in.
func (s *Server) Client() *chatgpt.Client {
	return &chatgpt.Client{E: s.Endpoints(), HC: s.Server.Client()}
}

// Approve is the person approving the sign-in that starts at authorize: it
// returns the address the browser ends on.
func (s *Server) Approve(t testing.TB, authorize string) string {
	t.Helper()
	u, err := url.Parse(authorize)
	if err != nil {
		t.Fatal(err)
	}
	q := u.Query()
	s.mu.Lock()
	defer s.mu.Unlock()
	s.LastAuthorize = q
	client := q.Get("client_id")
	if client == "dynamic_agent_client" {
		client = s.ClientID
	}
	s.n++
	code := fmt.Sprintf("code-%d", s.n)
	s.codes[code] = grant{client: client, challenge: q.Get("code_challenge"), nonce: q.Get("nonce"), redirect: q.Get("redirect_uri")}
	cb := url.Values{"code": {code}, "state": {q.Get("state")}, "client_id": {client}}
	return q.Get("redirect_uri") + "?" + cb.Encode()
}

// Decline is the person declining the sign-in that starts at authorize.
func Decline(authorize string) string {
	u, _ := url.Parse(authorize)
	return chatgpt.RedirectURI + "?" + url.Values{"error": {"access_denied"}, "state": {u.Query().Get("state")}}.Encode()
}

// Revoked lists the tokens revoked so far.
func (s *Server) Revoked() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.revoked...)
}

// Live reports whether refresh token rt can still be used.
func (s *Server) Live(rt string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.live[rt]
	return ok
}

func oauthErr(w http.ResponseWriter, code string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusBadRequest)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": code, "error_description": code + " (stand-in)"})
}

func (s *Server) token(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		oauthErr(w, "invalid_request")
		return
	}
	f := r.PostForm
	if f.Get("resource") != chatgpt.Resource {
		oauthErr(w, "invalid_target")
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	var client, nonce string
	switch f.Get("grant_type") {
	case "authorization_code":
		g, ok := s.codes[f.Get("code")]
		delete(s.codes, f.Get("code")) // used once
		sum := sha256.Sum256([]byte(f.Get("code_verifier")))
		switch {
		case !ok:
			oauthErr(w, "invalid_grant")
			return
		case g.client != f.Get("client_id"):
			oauthErr(w, "invalid_client")
			return
		case g.redirect != f.Get("redirect_uri"):
			oauthErr(w, "invalid_grant")
			return
		case base64.RawURLEncoding.EncodeToString(sum[:]) != g.challenge:
			oauthErr(w, "invalid_grant") // the PKCE verifier doesn't match
			return
		}
		client, nonce = g.client, g.nonce
		if s.Nonce != "" {
			nonce = s.Nonce
		}
	case "refresh_token":
		rt := f.Get("refresh_token")
		c, ok := s.live[rt]
		if !ok || c != f.Get("client_id") {
			if s.used[rt] {
				oauthErr(w, "refresh_token_reused")
			} else {
				oauthErr(w, "invalid_grant")
			}
			return
		}
		delete(s.live, rt) // rotated
		s.used[rt] = true
		s.Refreshes++
		client = c
		if s.OnRefresh != nil {
			s.OnRefresh()
		}
	default:
		oauthErr(w, "unsupported_grant_type")
		return
	}
	s.n++
	rt := fmt.Sprintf("rt-%d", s.n)
	s.live[rt] = client
	out := map[string]any{"access_token": fmt.Sprintf("at-%d", s.n), "refresh_token": rt,
		"expires_in": s.ExpiresIn, "scope": s.Scopes, "token_type": "Bearer"}
	if nonce != "" {
		out["id_token"] = s.idToken(client, nonce)
	}
	if s.NoReplacement && f.Get("grant_type") == "refresh_token" {
		delete(s.live, rt)
		delete(out, "refresh_token")
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(out)
}

// IDToken signs an ID token with the given claims over the defaults.
func (s *Server) IDToken(claims map[string]any) string {
	hdr, _ := json.Marshal(map[string]string{"alg": "RS256", "kid": "k1", "typ": "JWT"})
	body, _ := json.Marshal(claims)
	in := base64.RawURLEncoding.EncodeToString(hdr) + "." + base64.RawURLEncoding.EncodeToString(body)
	sum := sha256.Sum256([]byte(in))
	sig, _ := rsa.SignPKCS1v15(rand.Reader, s.Key, crypto.SHA256, sum[:]) // NOSONAR: signs an RS256 test token, not encryption
	return in + "." + base64.RawURLEncoding.EncodeToString(sig)
}

func (s *Server) idToken(client, nonce string) string {
	return s.IDToken(map[string]any{"iss": s.URL, "sub": s.Subject, "aud": []string{client}, "email": s.Email,
		"nonce": nonce, "iat": time.Now().Unix(), "exp": time.Now().Add(time.Hour).Unix()})
}

func (s *Server) revoke(w http.ResponseWriter, r *http.Request) {
	_ = r.ParseForm()
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.FailRevokes > 0 {
		s.FailRevokes--
		http.Error(w, `{"error":"server_error"}`, http.StatusServiceUnavailable)
		return
	}
	f := r.PostForm
	tok := f.Get("token")
	// A live token is revoked only by its own client, named as a refresh
	// token; one already invalid is answered 200 all the same.
	if client, ok := s.live[tok]; ok && (f.Get("client_id") != client || f.Get("token_type_hint") != "refresh_token") {
		oauthErr(w, "invalid_request")
		return
	}
	s.revoked = append(s.revoked, tok)
	delete(s.live, tok)
	w.WriteHeader(http.StatusOK)
}

func (s *Server) jwks(w http.ResponseWriter, _ *http.Request) {
	e := big.NewInt(int64(s.Key.E)).Bytes()
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"keys": []map[string]string{{
		"kty": "RSA", "kid": "k1", "alg": "RS256", "use": "sig",
		"n": base64.RawURLEncoding.EncodeToString(s.Key.N.Bytes()), "e": base64.RawURLEncoding.EncodeToString(e),
	}}})
}

// Callback returns the code of a callback address, for tests that change it.
func Callback(raw string) url.Values {
	_, q, _ := strings.Cut(raw, "?")
	v, _ := url.ParseQuery(q)
	return v
}
