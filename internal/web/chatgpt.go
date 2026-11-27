package web

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/OMG-Software/omg-review-bot/internal/chatgpt"
	"github.com/OMG-Software/omg-review-bot/internal/settings"
)

// A ChatGPT plan model (settings.ProviderChatGPT) is signed in from its card
// on the models page. "Sign in with ChatGPT" starts an attempt, kept here
// (it holds the PKCE verifier, which goes only to OpenAI's token endpoint,
// never through the browser), and shows
// the link that opens OpenAI's sign-in. The sign-in ends on a 127.0.0.1
// page, which the browser can't load unless the bot runs on the same
// computer, so the person pastes that page's address back, and the bot
// exchanges its code and saves the sign-in on the model.

// signInTTL is how long a started sign-in can be finished.
const signInTTL = 15 * time.Minute

type pendingSignIn struct {
	attempt chatgpt.Attempt
	url     string // where the sign-in starts
	// gen and account are the model's sign-in generation and sign-in when
	// the attempt started: if either has changed by the time it finishes,
	// someone else signed the model in or out, changed its provider or
	// removed it meanwhile, and the attempt is out of date.
	gen     uint64
	account string
	expires time.Time
}

var errStaleSignIn = errors.New("the model's ChatGPT sign-in changed while you were signing in: start again")

// stale reports whether the model c, whose sign-in generation is now gen,
// changed since p started. The generation catches a change that was undone
// (signed in and out again, or removed and another model added with its
// ID), which c's values alone can't.
func (p pendingSignIn) stale(c *settings.LLM, gen uint64) bool {
	return gen != p.gen || c.Account != p.account || c.ChatGPTClient != p.attempt.ClientID
}

// modelKey names the model id of sc among every set's.
func modelKey(sc modelScope, id string) string {
	return fmt.Sprintf("%d/%s", sc.set(), id)
}

// signInKey: an attempt belongs to the person who started it, for one model
// of one set.
func signInKey(sc modelScope, id string, v *viewer) string {
	return fmt.Sprintf("%s/%d", modelKey(sc, id), v.ID)
}

// signInGen is the model id of sc's sign-in generation: how many times,
// since the bot started, it's been signed in or out, moved to another
// provider or removed, or its whole set removed unread. Both counts only
// go up, so their sum changes whenever either does. Attempts live only as
// long as the bot, so the counts needn't outlive it either.
func (s *Server) signInGen(sc modelScope, id string) uint64 {
	return s.signInCount(modelKey(sc, id)) + s.signInCount(setKey(sc))
}

// setKey names sc's set-wide count among signInGens' model keys.
func setKey(sc modelScope) string { return strconv.FormatInt(sc.set(), 10) }

func (s *Server) signInCount(key string) uint64 {
	x, _ := s.signInGens.Load(key)
	n, _ := x.(uint64)
	return n
}

// The counts move on only under the models lock of their set, which a
// finishing attempt holds to check them.
func (s *Server) moveOn(key string) { s.signInGens.Store(key, s.signInCount(key)+1) }

// signInChanged moves on the sign-in generation of each of before's models
// whose sign-in after changes, or that after removes.
func (s *Server) signInChanged(sc modelScope, before, after settings.Models) {
	for _, c := range before.Configs {
		if a, ok := after.Get(c.ID); !ok || a.Provider != c.Provider || a.Account != c.Account || a.ChatGPTClient != c.ChatGPTClient {
			s.moveOn(modelKey(sc, c.ID))
		}
	}
}

// setSignInsChanged moves on the sign-in generation of all of sc's models,
// as when the set is removed without being read (which models it held
// can't be told), and drops the attempts waiting for them. One already
// finishing, no longer waiting, is refused by the generation.
func (s *Server) setSignInsChanged(sc modelScope) {
	s.moveOn(setKey(sc))
	prefix := modelKey(sc, "")
	s.signIns.Range(func(k, _ any) bool {
		if strings.HasPrefix(k.(string), prefix) {
			s.signIns.Delete(k)
		}
		return true
	})
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
		gen := s.signInGen(sc, id) // before the model is read: a change after counts
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
		a, u, err := s.bot.StartChatGPT(sc.set(), c)
		if err != nil {
			fail(fmt.Errorf("couldn't start the ChatGPT sign-in: %w", err))
			return
		}
		s.sweepSignIns()
		s.signIns.Store(key, pendingSignIn{attempt: a, url: u, gen: gen, account: c.Account, expires: time.Now().Add(signInTTL)})
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
		gone := fmt.Errorf("that sign-in has expired (they last %d minutes) or was already finished: start again", int(signInTTL/time.Minute))
		x, ok := s.signIns.Load(key)
		if !ok || time.Now().After(x.(pendingSignIn).expires) {
			fail(gone)
			return
		}
		p := x.(pendingSignIn)
		if cb.State != p.attempt.State {
			// Left waiting for its own address.
			fail(errors.New("that address is from another sign-in: paste the address the latest one ended on, or start again"))
			return
		}
		// Used once: only if it's still the attempt just read, not one
		// finished or started again meanwhile.
		if !s.signIns.CompareAndDelete(key, x) {
			fail(gone)
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), time.Minute)
		defer cancel()
		acct, who, err := s.bot.FinishChatGPT(ctx, sc.set(), p.attempt, cb)
		switch {
		case errors.Is(err, chatgpt.ErrNoPlanUsage):
			// Kept: the next sign-in asks for the plan's use again.
			_, _ = s.applyModels(r, sc, func(m *settings.Models) (string, error) {
				return "kept the ChatGPT app registration of a sign-in that didn't allow the plan's use",
					setChatGPT(m, id, func(c *settings.LLM) error {
						if p.stale(c, s.signInGen(sc, id)) {
							return errStaleSignIn
						}
						c.ChatGPTClient, c.AccountEmail, c.AccountSubject = who.ClientID, who.Email, who.Subject
						return nil
					})
			})
		case errors.Is(err, chatgpt.ErrOtherAccount):
			err = fmt.Errorf("you signed in as %s, which isn't the ChatGPT account this model uses (%s), so the model keeps its sign-in: to use another account, sign the model out first",
				orUnknown(who.Email), orUnknown(p.attempt.LoginHint))
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
			err := setChatGPT(m, id, func(c *settings.LLM) error {
				if p.stale(c, s.signInGen(sc, id)) {
					return errStaleSignIn
				}
				c.Account, c.AccountEmail, c.AccountSubject, c.ChatGPTClient = acct, who.Email, who.Subject, who.ClientID
				label = c.Label()
				return nil
			})
			return fmt.Sprintf("signed %s (%s) in to ChatGPT as %s", label, where, email), err
		})
		if err != nil {
			if derr := s.bot.DropChatGPT(acct, sc.set()); derr != nil { // no model holds it
				s.log.Warn("end ChatGPT sign-in", "set", sc.set(), "err", derr)
			}
			fail(err)
			return
		}
		toModel(w, r, sc, id, "msg", upperFirst(msg)+". Its reviews use that account's ChatGPT plan.")

	case "chatgpt-signout":
		msg, err := s.applyModels(r, sc, func(m *settings.Models) (string, error) {
			var label string
			err := setChatGPT(m, id, func(c *settings.LLM) error {
				// The sign-in is ended once saved (applyModels).
				c.Account, c.AccountEmail, c.AccountSubject, c.ChatGPTClient = "", "", "", ""
				label = c.Label()
				return nil
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

func orUnknown(email string) string {
	if email == "" {
		return "an account with no email"
	}
	return email
}

// setChatGPT applies set to the model id of m, which must still be a
// ChatGPT plan model. It runs under the models' lock (applyModels), so set
// sees the model as saved.
func setChatGPT(m *settings.Models, id string, set func(*settings.LLM) error) error {
	for i := range m.Configs {
		c := &m.Configs[i]
		if c.ID != id {
			continue
		}
		if c.Provider != settings.ProviderChatGPT {
			return errors.New("that model no longer uses the ChatGPT plan")
		}
		return set(c)
	}
	return errors.New("that model is no longer there")
}
