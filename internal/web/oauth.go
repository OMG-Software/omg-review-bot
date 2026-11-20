package web

import (
	"context"
	"crypto/subtle"
	"encoding/base64"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/OMG-Software/omg-review-bot/internal/github"
	"github.com/OMG-Software/omg-review-bot/internal/store"
)

const oauthCookie = "prbot_oauth"

// publicURL is the bot's address as people and GitHub see it.
func (s *Server) publicURL(r *http.Request) string {
	if u := s.bot.Settings().GitHub.PublicURL; u != "" {
		return u
	}
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	return scheme + "://" + r.Host
}

func (s *Server) callbackURL(r *http.Request) string { return s.publicURL(r) + "/auth/github/callback" }

// githubStart sends the browser to GitHub to sign in.
func (s *Server) githubStart(w http.ResponseWriter, r *http.Request) {
	st := s.bot.Settings()
	if !st.GitHub.SignInOn() {
		s.render(w, r, "login.html", page{Title: "Sign in", Err: "Sign in with GitHub isn't set up yet. An administrator has to add the App's client ID and secret in Settings.", D: s.loginData(r)})
		return
	}
	next := safeNext(r.URL.Query().Get("next"))
	if pub, err := url.Parse(st.GitHub.PublicURL); err == nil && pub.Host != "" && !strings.EqualFold(pub.Host, r.Host) {
		// GitHub returns to the Public URL, and the browser only sends the
		// state cookie back to the host that set it: start there.
		http.Redirect(w, r, strings.TrimRight(st.GitHub.PublicURL, "/")+"/auth/github?next="+url.QueryEscape(next), http.StatusSeeOther)
		return
	}
	state := randomToken()
	exp := time.Now().Add(10 * time.Minute).Unix()
	payload := fmt.Sprintf("%s|%d|%s", state, exp, base64.RawURLEncoding.EncodeToString([]byte(next)))
	s.setCookie(w, r, oauthCookie, s.signed(payload), "/auth/github", 600)
	web := github.WebBaseURL(st.GitHub.APIBaseURL)
	http.Redirect(w, r, github.AuthorizeURL(web, st.GitHub.ClientID, s.callbackURL(r), state), http.StatusSeeOther)
}

// githubCallback finishes sign-in: it checks the state, trades the code for a
// token, records the person, derives their organization access from GitHub,
// then discards the token.
func (s *Server) githubCallback(w http.ResponseWriter, r *http.Request) {
	fail := func(msg string) {
		s.render(w, r, "login.html", page{Title: "Sign in", Err: msg, D: s.loginData(r)})
	}
	q := r.URL.Query()
	c, err := r.Cookie(oauthCookie)
	s.setCookie(w, r, oauthCookie, "", "/auth/github", -1)
	if err != nil {
		fail("The sign-in took too long or started in another browser. Try again.")
		return
	}
	payload, ok := s.unsign(c.Value)
	parts := strings.Split(payload, "|")
	if !ok || len(parts) != 3 {
		fail("Sign-in failed. Try again.")
		return
	}
	exp, _ := strconv.ParseInt(parts[1], 10, 64)
	if subtle.ConstantTimeCompare([]byte(parts[0]), []byte(q.Get("state"))) != 1 || time.Now().Unix() > exp {
		fail("Sign-in failed (the request didn't match). Try again.")
		return
	}
	nextB, _ := base64.RawURLEncoding.DecodeString(parts[2])
	next := safeNext(string(nextB))
	if e := q.Get("error"); e != "" {
		fail("GitHub sign-in was cancelled: " + q.Get("error_description"))
		return
	}

	st := s.bot.Settings()
	ctx, cancel := context.WithTimeout(r.Context(), 45*time.Second)
	defer cancel()
	token, err := github.ExchangeCode(ctx, github.WebBaseURL(st.GitHub.APIBaseURL), st.GitHub.ClientID, st.GitHub.ClientSecret,
		q.Get("code"), s.callbackURL(r))
	if err != nil {
		s.log.Warn("GitHub sign-in", "err", err)
		fail(err.Error())
		return
	}
	uc := github.NewUserClient(st.GitHub.APIBaseURL, token)
	gu, err := uc.Me(ctx)
	if err != nil {
		fail("Couldn't read your GitHub profile: " + err.Error())
		return
	}
	if old, _ := s.store.GetUser(gu.ID); old != nil && old.Disabled {
		fail("Your account has been blocked on this bot. Ask an administrator.")
		return
	}
	u, err := s.store.RecordSignIn(gu.ID, gu.Login, gu.Name, gu.AvatarURL)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	notes, err := s.syncAccess(ctx, uc, u)
	if err != nil {
		// Don't sign them in with access that should have changed, and end
		// the sessions they already have, which could still use it: at once
		// here, and in the database so it lasts a restart (best effort, as the
		// database may be what failed). The next sign-in that gets through
		// updates the access and lets them back in.
		s.log.Error("update access at sign-in", "user", u.Login, "err", err)
		s.unreconciled.Store(u.ID, true)
		if err := s.endSessions(u.ID); err != nil {
			s.log.Error("end sessions after a failed access update", "user", u.Login, "err", err)
		}
		fail("Couldn't update your access, so you weren't signed in. Try again; if it keeps happening, tell an administrator.")
		return
	}
	s.unreconciled.Delete(u.ID)
	s.store.Log("info", "", 0, fmt.Sprintf("@%s signed in with GitHub", u.Login))
	s.issueSession(w, r, u.ID, u.Epoch)
	if len(notes) > 0 {
		redirectMsg(w, r, next, strings.Join(notes, " "))
		return
	}
	http.Redirect(w, r, next, http.StatusSeeOther)
}

// syncMemberships derives the person's organization access from GitHub:
// admins of a GitHub organization the App is installed on become its owners
// here, the account owner of a personal installation becomes its owner, and
// members join as members when the organization allows it. Access added by
// hand is left alone, whatever the GitHub role.
// Access that came from GitHub and no longer applies is removed, and so is
// access GitHub couldn't confirm this time: it's restored at the next sign-in
// that can. An error means the access couldn't be updated; the caller must
// not sign the person in.
func (s *Server) syncMemberships(ctx context.Context, uc *github.UserClient, u *store.User) ([]string, error) {
	st := s.bot.Settings()
	var notes []string
	insts, err := uc.Installations(ctx)
	if err != nil {
		s.log.Warn("list user installations", "user", u.Login, "err", err)
		notes = append(notes, "Couldn't read your organizations from GitHub ("+err.Error()+"), so access that comes from GitHub is paused until you sign in again.")
		insts = nil // the clean-up below removes GitHub-derived access
	}
	seen := map[int64]bool{}
	for _, in := range insts {
		acct := in.Account
		if acct.ID == 0 {
			continue
		}
		// An account the allow-list excludes is paused, not gone: GitHub-
		// derived access to it is still kept up to date (and removed when
		// GitHub withdraws it), but nobody gains new access to it.
		excluded := len(st.GitHub.Accounts) > 0 && !containsFold(st.GitHub.Accounts, acct.Login)
		org, _ := s.store.GetOrg(acct.ID)
		if org == nil && excluded {
			continue // never added, so nobody has access to keep
		}
		if org == nil {
			// Installed since the last repository sync: record it now.
			if err := s.store.UpsertOrg(acct.ID, acct.Login, acct.Type, in.ID); err != nil {
				continue
			}
			s.bot.Wake()
			org, _ = s.store.GetOrg(acct.ID)
			if org == nil {
				continue
			}
		}
		seen[org.ID] = true
		role := ""
		if acct.Type == "User" {
			if acct.ID == u.ID {
				role = store.RoleOwner
			}
		} else {
			m, err := uc.OrgMembership(ctx, acct.Login)
			switch {
			case err != nil && !github.IsStatus(err, http.StatusNotFound):
				// Unconfirmed: treat as no role, so access from GitHub is
				// paused (access added by hand stays).
				notes = append(notes, fmt.Sprintf("Couldn't read your role in %s (%v), so access that comes from GitHub is paused until you sign in again. The GitHub App needs the \"Members: Read-only\" organization permission.", acct.Login, err))
			case err != nil:
			case m.State == "active" && m.Role == "admin":
				role = store.RoleOwner
			case m.State == "active" && org.AutoJoin:
				role = store.RoleMember
			}
		}
		cur, err := s.store.GetMembership(org.ID, u.ID)
		if err != nil {
			return notes, err
		}
		// Access added by hand stays as an owner set it, whatever the GitHub
		// role: a role from GitHub is only stored as coming from GitHub, so it
		// can't outlast GitHub taking it back. The store checks the source
		// again as it writes, so access added by hand meanwhile isn't touched.
		var removed bool
		switch {
		case cur == nil && role != "" && !excluded:
			err = s.store.SetGitHubMembership(org.ID, u.ID, role)
			if err == nil {
				s.store.LogFor(org.ID, "info", "", 0, fmt.Sprintf("@%s joined %s as %s (from their GitHub role)", u.Login, org.Login, role))
			}
		case cur == nil:
		case cur.Source == store.SourceGitHub && role == "":
			removed, err = s.store.RemoveGitHubMembership(org.ID, u.ID)
			if removed {
				s.store.LogFor(org.ID, "info", "", 0, fmt.Sprintf("@%s lost access to %s (not confirmed as an admin or member on GitHub)", u.Login, org.Login))
			}
		case cur.Source == store.SourceGitHub && role != cur.Role:
			err = s.store.SetGitHubMembership(org.ID, u.ID, role)
		}
		if err != nil {
			return notes, fmt.Errorf("update access to %s: %w", org.Login, err)
		}
	}
	// Access derived from GitHub for installations the person can no longer see.
	ms, err := s.store.UserMemberships(u.ID)
	if err != nil {
		return notes, err
	}
	for _, m := range ms {
		if m.Source == store.SourceGitHub && !seen[m.OrgID] {
			removed, err := s.store.RemoveGitHubMembership(m.OrgID, u.ID)
			if err != nil {
				return notes, fmt.Errorf("remove access to %s: %w", m.OrgLogin, err)
			}
			if !removed {
				continue // added by hand meanwhile
			}
			s.store.LogFor(m.OrgID, "info", "", 0, fmt.Sprintf("@%s lost access to %s (the App's installation isn't visible to them on GitHub)", u.Login, m.OrgLogin))
		}
	}
	return notes, nil
}

func containsFold(list []string, s string) bool {
	for _, v := range list {
		if strings.EqualFold(strings.TrimSpace(v), s) {
			return true
		}
	}
	return false
}
