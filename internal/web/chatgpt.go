package web

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/OMG-Software/omg-review-bot/internal/chatgpt"
	"github.com/OMG-Software/omg-review-bot/internal/settings"
)

// A ChatGPT plan model (settings.ProviderChatGPT) is signed in from its card
// on the models page. "Sign in with ChatGPT" starts an attempt, kept here
// (it holds the PKCE verifier, which never leaves the server), and shows
// the link that opens OpenAI's sign-in. The sign-in ends on a 127.0.0.1
// page, which the browser can't load unless the bot runs on the same
// computer, so the person pastes that page's address back, and the bot
// exchanges its code and saves the sign-in on the model.

// signInTTL is how long a started sign-in can be finished.
const signInTTL = 15 * time.Minute

type pendingSignIn struct {
	attempt chatgpt.Attempt
	url     string // where the sign-in starts
	expires time.Time
}

// signInKey: an attempt belongs to the person who started it, for one model
// of one set.
func signInKey(sc modelScope, id string, v *viewer) string {
	return fmt.Sprintf("%d/%s/%d", sc.set(), id, v.ID)
}

// pendingSignIn returns the sign-in v started for the model id of sc and
// hasn't finished, if it's still current.
func (s *Server) pendingSignIn(sc modelScope, id string, v *viewer) (pendingSignIn, bool) {
	key := signInKey(sc, id, v)
	x, ok := s.signIns.Load(key)
	if !ok {
		return pendingSignIn{}, false
	}
	p := x.(pendingSignIn)
	if time.Now().After(p.expires) {
		s.signIns.CompareAndDelete(key, x)
		return pendingSignIn{}, false
	}
	return p, true
}

// sweepSignIns forgets the sign-ins started too long ago.
func (s *Server) sweepSignIns() {
	now := time.Now()
	s.signIns.Range(func(k, x any) bool {
		if now.After(x.(pendingSignIn).expires) {
			s.signIns.CompareAndDelete(k, x)
		}
		return true
	})
}

// toModel redirects back to the model's card with a message (k "msg") or an
// error (k "err").
func toModel(w http.ResponseWriter, r *http.Request, sc modelScope, id, k, v string) {
	http.Redirect(w, r, withQuery(sc.base, k, v)+"#model-"+id, http.StatusSeeOther)
}

// chatgptAction signs the model id of sc in to ChatGPT, or out.
func (s *Server) chatgptAction(w http.ResponseWriter, r *http.Request, sc modelScope, id, where string) {
	key := signInKey(sc, id, me(r))
	fail := func(err error) { toModel(w, r, sc, id, "err", upperFirst(err.Error())) }
	switch r.PathValue("action") {
	case "chatgpt-start":
		m, err := s.loadModels(sc)
		if err != nil {
			fail(err)
			return
		}
		c, ok := m.Get(id)
		switch {
		case !ok:
			fail(errors.New("that model is no longer there"))
			return
		case c.Provider != settings.ProviderChatGPT:
			fail(errors.New("save the model with the ChatGPT plan as its provider first"))
			return
		}
		a, u, err := s.bot.StartChatGPT(c)
		if err != nil {
			fail(fmt.Errorf("couldn't start the ChatGPT sign-in: %w", err))
			return
		}
		s.sweepSignIns()
		s.signIns.Store(key, pendingSignIn{attempt: a, url: u, expires: time.Now().Add(signInTTL)})
		toModel(w, r, sc, id, "msg", "Open the ChatGPT sign-in below, then paste back the address it ends on.")

	case "chatgpt-cancel":
		s.signIns.Delete(key)
		toModel(w, r, sc, id, "msg", "The ChatGPT sign-in was cancelled.")

	case "chatgpt-finish":
		cb, err := chatgpt.ParseCallback(r.FormValue("callback"))
		if err != nil {
			fail(err) // the attempt stays: paste the right address
			return
		}
		x, ok := s.signIns.LoadAndDelete(key) // used once
		if !ok || time.Now().After(x.(pendingSignIn).expires) {
			fail(fmt.Errorf("that sign-in has expired (they last %d minutes) or was already finished: start again", int(signInTTL/time.Minute)))
			return
		}
		p := x.(pendingSignIn)
		if cb.State != p.attempt.State {
			s.signIns.Store(key, p) // still waiting for its own address
			fail(errors.New("that address is from another sign-in: paste the address the latest one ended on, or start again"))
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), time.Minute)
		defer cancel()
		acct, who, err := s.bot.FinishChatGPT(ctx, sc.set(), p.attempt, cb)
		if errors.Is(err, chatgpt.ErrNoPlanUsage) {
			// Kept: the next sign-in asks for the plan's use again.
			_, _ = s.applyModels(r, sc, func(m *settings.Models) (string, error) {
				return "kept the ChatGPT app registration of a sign-in that didn't allow the plan's use",
					setChatGPT(m, id, func(c *settings.LLM) { c.ChatGPTClient, c.AccountEmail = who.ClientID, who.Email })
			})
		}
		if err != nil {
			fail(err)
			return
		}
		email := who.Email
		if email == "" {
			email = "a ChatGPT account"
		}
		msg, err := s.applyModels(r, sc, func(m *settings.Models) (string, error) {
			var label string
			err := setChatGPT(m, id, func(c *settings.LLM) {
				c.Account, c.AccountEmail, c.AccountSubject, c.ChatGPTClient = acct, who.Email, who.Subject, who.ClientID
				label = c.Label()
			})
			return fmt.Sprintf("signed %s (%s) in to ChatGPT as %s", label, where, email), err
		})
		if err != nil {
			s.bot.DropChatGPT(acct, sc.set()) // no model holds it
			fail(err)
			return
		}
		toModel(w, r, sc, id, "msg", upperFirst(msg)+". Its reviews use that account's ChatGPT plan.")

	case "chatgpt-signout":
		msg, err := s.applyModels(r, sc, func(m *settings.Models) (string, error) {
			var label string
			err := setChatGPT(m, id, func(c *settings.LLM) {
				// The sign-in is ended once saved (applyModels).
				c.Account, c.AccountEmail, c.AccountSubject, c.ChatGPTClient = "", "", "", ""
				label = c.Label()
			})
			return fmt.Sprintf("signed %s (%s) out of ChatGPT", label, where), err
		})
		if err != nil {
			fail(err)
			return
		}
		toModel(w, r, sc, id, "msg", upperFirst(msg)+".")

	default:
		s.notFound(w, r)
	}
}

// setChatGPT applies set to the model id of m, which must still be a
// ChatGPT plan model.
func setChatGPT(m *settings.Models, id string, set func(*settings.LLM)) error {
	for i := range m.Configs {
		c := &m.Configs[i]
		if c.ID != id {
			continue
		}
		if c.Provider != settings.ProviderChatGPT {
			return errors.New("that model no longer uses the ChatGPT plan")
		}
		set(c)
		return nil
	}
	return errors.New("that model is no longer there")
}
