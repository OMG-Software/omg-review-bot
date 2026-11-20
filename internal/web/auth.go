package web

import (
	"context"
	"crypto/hmac"
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/OMG-Software/omg-review-bot/internal/store"
)

const (
	cookieName  = "prbot_session"
	sessionTTL  = 7 * 24 * time.Hour
	pwKey       = "admin_password"
	secretKey   = "session_secret"
	pbkdf2Iters = 600_000

	// localAdminID is the session user ID of the password administrator.
	localAdminID = 0
)

func hashPassword(pw string) (string, error) {
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		return "", err
	}
	key, err := pbkdf2.Key(sha256.New, pw, salt, pbkdf2Iters, 32)
	if err != nil {
		return "", err
	}
	enc := base64.RawStdEncoding
	return fmt.Sprintf("pbkdf2-sha256$%d$%s$%s", pbkdf2Iters, enc.EncodeToString(salt), enc.EncodeToString(key)), nil
}

func checkPassword(stored, pw string) bool {
	parts := strings.Split(stored, "$")
	if len(parts) != 4 || parts[0] != "pbkdf2-sha256" {
		return false
	}
	iters, err := strconv.Atoi(parts[1])
	if err != nil {
		return false
	}
	enc := base64.RawStdEncoding
	salt, err1 := enc.DecodeString(parts[2])
	want, err2 := enc.DecodeString(parts[3])
	if err1 != nil || err2 != nil {
		return false
	}
	got, err := pbkdf2.Key(sha256.New, pw, salt, iters, len(want))
	return err == nil && subtle.ConstantTimeCompare(got, want) == 1
}

func (s *Server) hasPassword() bool {
	_, ok, _ := s.store.Get(pwKey)
	return ok
}

func (s *Server) loadSecret() error {
	v, ok, err := s.store.Get(secretKey)
	if err != nil {
		return err
	}
	if !ok {
		return s.rotateSecret()
	}
	b, err := hex.DecodeString(v)
	if err != nil {
		return err
	}
	s.secret.Store(&b)
	return nil
}

// rotateSecret invalidates every existing session (used on password change).
func (s *Server) rotateSecret() error {
	s.secretMu.Lock()
	defer s.secretMu.Unlock()
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return err
	}
	if err := s.saveSecret(hex.EncodeToString(b)); err != nil {
		return err
	}
	s.secret.Store(&b) // requests signing now use either the old key or this one, whole
	return nil
}

func (s *Server) sign(payload string) string {
	m := hmac.New(sha256.New, *s.secret.Load())
	m.Write([]byte(payload))
	return base64.RawURLEncoding.EncodeToString(m.Sum(nil))
}

// signed returns payload.mac; unsign checks it and returns the payload.
func (s *Server) signed(payload string) string { return payload + "." + s.sign(payload) }

func (s *Server) unsign(v string) (string, bool) {
	i := strings.LastIndexByte(v, '.')
	if i < 0 {
		return "", false
	}
	payload, mac := v[:i], v[i+1:]
	return payload, hmac.Equal([]byte(mac), []byte(s.sign(payload)))
}

// secureRequest reports whether the browser reached the bot over HTTPS:
// either the bot terminated TLS itself, or the request came in on the
// configured https:// Public URL through a reverse proxy (which passes the
// original Host). A direct plain-HTTP visit, such as http://127.0.0.1:8080
// beside a proxy, isn't, so its cookies still work there.
func (s *Server) secureRequest(r *http.Request) bool {
	if r.TLS != nil {
		return true
	}
	u, err := url.Parse(s.bot.Settings().GitHub.PublicURL)
	return err == nil && strings.EqualFold(u.Scheme, "https") && strings.EqualFold(u.Host, r.Host)
}

// setCookie sets one of the bot's cookies. Every cookie is HttpOnly and
// SameSite=Lax, and Secure whenever the request came over HTTPS; it can't
// always be Secure, because browsers drop Secure cookies sent over plain
// HTTP, which the bot uses on a LAN (-listen 0.0.0.0:8080). maxAge < 0
// deletes the cookie.
func (s *Server) setCookie(w http.ResponseWriter, r *http.Request, name, value, path string, maxAge int) {
	http.SetCookie(w, &http.Cookie{
		Name: name, Value: value, Path: path, MaxAge: maxAge,
		HttpOnly: true, SameSite: http.SameSiteLaxMode, Secure: s.secureRequest(r),
	})
}

// issueSession signs a person in. The session names the user and their
// session epoch, so bumping the epoch (sign out everywhere, blocking the
// user) ends it.
func (s *Server) issueSession(w http.ResponseWriter, r *http.Request, userID, epoch int64) {
	exp := time.Now().Add(sessionTTL).Unix()
	s.setCookie(w, r, cookieName, s.signed(fmt.Sprintf("%d.%d.%d", userID, epoch, exp)), "/", int(sessionTTL.Seconds()))
}

func (s *Server) clearSession(w http.ResponseWriter, r *http.Request) {
	s.setCookie(w, r, cookieName, "", "/", -1)
}

// viewer is the signed-in person.
type viewer struct {
	ID     int64 // GitHub user ID; localAdminID for the password administrator
	Login  string
	Name   string
	Avatar string
	Admin  bool
	Local  bool           // signed in with the administrator password
	Orgs   []store.Member // organizations they have access to
}

// Display is how the viewer is named in the header.
func (v *viewer) Display() string {
	if v.Local {
		return "Administrator"
	}
	return "@" + v.Login
}

// Role returns the viewer's role in an organization: "admin" for
// administrators, "owner" or "member", or "" for no access.
func (v *viewer) Role(orgID int64) string {
	if v == nil {
		return ""
	}
	if v.Admin {
		return "admin"
	}
	for _, m := range v.Orgs {
		if m.OrgID == orgID {
			return m.Role
		}
	}
	return ""
}

// CanManage reports whether the viewer may change an organization's settings.
func (v *viewer) CanManage(orgID int64) bool {
	r := v.Role(orgID)
	return r == "admin" || r == store.RoleOwner
}

// sessionViewer reads and checks the session cookie; nil when not signed in.
func (s *Server) sessionViewer(r *http.Request) *viewer {
	c, err := r.Cookie(cookieName)
	if err != nil {
		return nil
	}
	payload, ok := s.unsign(c.Value)
	if !ok {
		return nil
	}
	parts := strings.Split(payload, ".")
	if len(parts) != 3 {
		return nil
	}
	uid, err1 := strconv.ParseInt(parts[0], 10, 64)
	epoch, err2 := strconv.ParseInt(parts[1], 10, 64)
	exp, err3 := strconv.ParseInt(parts[2], 10, 64)
	if err1 != nil || err2 != nil || err3 != nil || time.Now().Unix() >= exp {
		return nil
	}
	if uid == localAdminID {
		return &viewer{ID: localAdminID, Admin: true, Local: true}
	}
	u, err := s.store.GetUser(uid)
	if _, bad := s.unreconciled.Load(uid); bad {
		return nil // their access couldn't be updated at sign-in
	}
	if err != nil || u == nil || u.Disabled || u.Epoch != epoch {
		return nil
	}
	v := &viewer{ID: u.ID, Login: u.Login, Name: u.Name, Avatar: u.AvatarURL, Admin: u.Admin}
	v.Orgs, _ = s.store.UserMemberships(u.ID)
	return v
}

type viewerKey struct{}

// me returns the request's signed-in person (set by ServeHTTP).
func me(r *http.Request) *viewer {
	v, _ := r.Context().Value(viewerKey{}).(*viewer)
	return v
}

func withViewer(r *http.Request, v *viewer) *http.Request {
	return r.WithContext(context.WithValue(r.Context(), viewerKey{}, v))
}

// sameOrigin is the CSRF check for state-changing requests: browsers always
// send Origin (or at least Referer) on form posts and fetch() calls.
func sameOrigin(r *http.Request) bool {
	src := r.Header.Get("Origin")
	if src == "" {
		src = r.Header.Get("Referer")
	}
	if src == "" {
		return false
	}
	u, err := url.Parse(src)
	return err == nil && u.Host == r.Host
}

// safeNext keeps a post-sign-in redirect on this site. Browsers treat a
// backslash like a slash and drop tabs and newlines before parsing, so
// `/\evil.example` or `/./\evil.example` would leave the site: any of those
// characters is refused outright.
func safeNext(next string) string {
	if !strings.HasPrefix(next, "/") || strings.HasPrefix(next, "//") || strings.ContainsAny(next, "\\\t\r\n") {
		return "/"
	}
	return next
}

func randomToken() string {
	b := make([]byte, 24)
	_, _ = rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}
