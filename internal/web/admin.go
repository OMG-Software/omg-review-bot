package web

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"time"

	"github.com/OMG-Software/omg-review-bot/internal/github"
	"github.com/OMG-Software/omg-review-bot/internal/store"
)

// ---------- users ----------

func (s *Server) usersPage(w http.ResponseWriter, r *http.Request) {
	users, err := s.store.ListUsers()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	orgs, err := s.store.MembershipsByUser()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	s.render(w, r, "users.html", page{Title: "Users", Nav: "users", D: map[string]any{
		"Users": users, "Orgs": orgs, "Me": me(r).ID,
	}})
}

// userFor loads the user in the URL; an administrator can't change their own
// account here, so nobody locks themselves out.
func (s *Server) userFor(w http.ResponseWriter, r *http.Request) *store.User {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	u, err := s.store.GetUser(id)
	if err != nil || u == nil {
		s.notFound(w, r)
		return nil
	}
	if u.ID == me(r).ID {
		redirectErr(w, r, "/admin/users", errors.New("you can't change your own account; ask another administrator"))
		return nil
	}
	return u
}

func (s *Server) userSetAdmin(w http.ResponseWriter, r *http.Request) {
	u := s.userFor(w, r)
	if u == nil {
		return
	}
	on := r.FormValue("on") == "1"
	if err := s.store.SetUserAdmin(u.ID, on); err != nil {
		redirectErr(w, r, "/admin/users", err)
		return
	}
	verb := map[bool]string{true: "made @%s an administrator", false: "removed @%s as an administrator"}[on]
	s.store.Log("info", "", 0, me(r).Display()+" "+fmt.Sprintf(verb, u.Login))
	redirectMsg(w, r, "/admin/users", "Done: "+fmt.Sprintf(verb, u.Login)+".")
}

func (s *Server) userSetDisabled(w http.ResponseWriter, r *http.Request) {
	u := s.userFor(w, r)
	if u == nil {
		return
	}
	on := r.FormValue("on") == "1"
	if err := s.store.SetUserDisabled(u.ID, on); err != nil {
		redirectErr(w, r, "/admin/users", err)
		return
	}
	verb := map[bool]string{true: "blocked @%s (signed out everywhere)", false: "unblocked @%s"}[on]
	s.store.Log("info", "", 0, me(r).Display()+" "+fmt.Sprintf(verb, u.Login))
	redirectMsg(w, r, "/admin/users", "Done: "+fmt.Sprintf(verb, u.Login)+".")
}

// ---------- webhooks ----------

func (s *Server) webhooksPage(w http.ResponseWriter, r *http.Request) {
	ds, err := s.store.RecentDeliveries(200)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	names := map[int64]string{}
	if orgs, err := s.store.ListOrgs(); err == nil {
		for _, o := range orgs {
			names[o.ID] = o.Login
		}
	}
	st := s.bot.Settings()
	s.render(w, r, "webhooks.html", page{Title: "Webhooks", Nav: "settings", D: map[string]any{
		"Deliveries": ds, "Names": names, "On": st.GitHub.WebhookSecret != "", "URL": s.publicURL(r) + "/webhooks/github",
		"Interval": st.Poll.WebhookIntervalSeconds,
	}})
}

// webhook receives GitHub's deliveries. It answers quickly: the bot only
// records state and queues work, and GitHub gives up after 10 seconds.
// maxWebhookBody is the largest webhook payload the bot accepts.
const maxWebhookBody = 5 << 20

func (s *Server) webhook(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		http.Error(w, "webhooks are POSTed", http.StatusMethodNotAllowed)
		return
	}
	secret := s.bot.Settings().GitHub.WebhookSecret
	if secret == "" {
		http.Error(w, "webhooks are not enabled on this bot (no webhook secret is set)", http.StatusNotFound)
		return
	}
	// Read before the signature can be checked, so kept small: the events the
	// bot subscribes to carry no diffs and are far below this.
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxWebhookBody))
	if err != nil {
		http.Error(w, "payload too large", http.StatusRequestEntityTooLarge)
		return
	}
	if !github.VerifySignature(secret, body, r.Header.Get("X-Hub-Signature-256")) {
		s.log.Warn("webhook with a bad signature", "remote", r.RemoteAddr, "event", r.Header.Get("X-GitHub-Event"))
		http.Error(w, "bad signature", http.StatusUnauthorized)
		return
	}
	event := r.Header.Get("X-GitHub-Event")
	id := r.Header.Get("X-GitHub-Delivery")
	if id == "" {
		id = fmt.Sprintf("local-%d", time.Now().UnixNano())
	}
	d := store.Delivery{ID: id, Event: event}
	var probe github.WebhookEvent
	if json.Unmarshal(body, &probe) == nil {
		d.Action = probe.Action
		if probe.Repository != nil {
			d.Repo = probe.Repository.FullName
		}
	}
	fresh, err := s.store.RecordDelivery(d)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if !fresh {
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "result": "duplicate delivery ignored"})
		return
	}
	account, result, err := s.bot.HandleWebhook(event, body)
	if err != nil {
		result = "error: " + err.Error()
	}
	_ = s.store.SetDeliveryResult(id, account, result)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "result": result})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "result": result})
}
