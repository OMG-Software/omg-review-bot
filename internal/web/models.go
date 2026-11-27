package web

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/OMG-Software/omg-review-bot/internal/bot"
	"github.com/OMG-Software/omg-review-bot/internal/llm"
	"github.com/OMG-Software/omg-review-bot/internal/settings"
	"github.com/OMG-Software/omg-review-bot/internal/store"
)

// A set of models is edited on its own page: the instance's (administrators,
// /settings/models) or an organization's own (its owners, /orgs/{org}/models).
// An organization with no models of its own uses the instance's.

// modelScope is the set of models a request edits.
type modelScope struct {
	base string     // the models page's path
	org  *store.Org // nil for the instance's models
	// own: an organization's own models, which must be reached at a public
	// https address (see settings.CheckOwn).
	own bool
}

// set is whose models these are, as the bot names a set: 0 for the
// instance's, else the organization's ID.
func (sc modelScope) set() int64 {
	if sc.org != nil {
		return sc.org.ID
	}
	return 0
}

// instanceModels is the scope of the instance's models (routes are adminOnly).
func instanceModels() modelScope { return modelScope{base: "/settings/models"} }

// orgModelsScope is the scope of the organization in the URL's own models.
// manage: the request changes them, which only its owners (and
// administrators) may.
func (s *Server) orgModelsScope(w http.ResponseWriter, r *http.Request, manage bool) (modelScope, bool) {
	org := s.orgFor(w, r, manage)
	if org == nil {
		return modelScope{}, false
	}
	return modelScope{base: orgBase(org) + "/models", org: org, own: true}, true
}

func (s *Server) loadModels(sc modelScope) (settings.Models, error) {
	if sc.org != nil {
		return s.bot.OrgModels(sc.org.ID)
	}
	return s.bot.Settings().Models, nil
}

func (s *Server) saveModels(sc modelScope, m settings.Models) error {
	if sc.org != nil {
		return s.bot.SetOrgModels(sc.org.ID, m)
	}
	if len(m.Configs) == 0 {
		return errors.New("the instance needs at least one model")
	}
	st := s.bot.Settings()
	st.Models = m
	if err := settings.Save(s.store, st); err != nil {
		return err
	}
	s.bot.Wake()
	return nil
}

// logModels records a change to an organization's models in its activity.
func (s *Server) logModels(sc modelScope, r *http.Request, what string) {
	if sc.org != nil {
		s.store.LogFor(sc.org.ID, "info", "", 0, fmt.Sprintf("%s %s", me(r).Display(), what))
	}
}

// modelView is one configuration as the page shows it: never its key.
type modelView struct {
	settings.LLM
	HasKey bool
	Usage  store.ConfigUsage // its use over the last usageDays days
	// SignInURL starts the ChatGPT sign-in the viewer began for it, waiting
	// for the address it ended on ("" when there's none).
	SignInURL string
}

// usageDays is the period the models page counts each model's use over.
const usageDays = 30

func (s *Server) instanceModelsPage(w http.ResponseWriter, r *http.Request) {
	s.modelsPage(w, r, instanceModels(), page{Title: "Models", Nav: "settings"}, false)
}

func (s *Server) orgModelsPage(w http.ResponseWriter, r *http.Request) {
	sc, ok := s.orgModelsScope(w, r, false)
	if !ok {
		return
	}
	p := page{Title: sc.org.Login + " · Models", Nav: "orgs", Org: sc.org, Tab: "models"}
	s.modelsPage(w, r, sc, p, !me(r).CanManage(sc.org.ID))
}

func (s *Server) modelsPage(w http.ResponseWriter, r *http.Request, sc modelScope, p page, readOnly bool) {
	m, err := s.loadModels(sc)
	if err != nil {
		// Shown, not hidden: the owners can still remove what's stored.
		p.Err = upperFirst(err.Error())
	}
	// Keys never go into the page's data, only whether one is saved.
	shown := m
	shown.Configs = make([]settings.LLM, len(m.Configs))
	views := make([]modelView, len(m.Configs))
	// Whose models' use to count: the instance's, or the organization's own.
	usage, uerr := s.store.ConfigUsageSince(sc.set(), store.DayWindowStart(usageDays, time.Now()))
	if uerr != nil {
		s.log.Warn("model usage", "err", uerr)
	}
	for i, c := range m.Configs {
		key := c.APIKey != ""
		c.APIKey = ""
		shown.Configs[i] = c
		views[i] = modelView{LLM: c, HasKey: key, Usage: usage[c.ID]}
		if p, ok := s.pendingSignIn(sc, c.ID, me(r)); ok && !readOnly {
			views[i].SignInURL = p.url
		}
	}
	d := map[string]any{
		"Base": sc.base, "Own": sc.own, "M": shown, "Configs": views, "ReadOnly": readOnly,
		"Strategies": settings.Strategies, "Providers": settings.Providers, "New": settings.NewLLM(),
		"Max": settings.MaxModels, "Full": len(m.Configs) >= settings.MaxModels, "Unreadable": err != nil,
		"UsageDays": usageDays, "UsageKnown": uerr == nil, "ChatGPT": settings.ProviderChatGPT,
	}
	if sc.org != nil {
		// Only the names of the instance's models: members can see this page.
		var names []string
		for _, c := range s.bot.Settings().Models.InUse() {
			names = append(names, c.Label())
		}
		d["InstanceInUse"] = names
		d["DailyLimit"] = sc.org.DailyLimit()
		// Unset: its reviews wait (see bot.modelsUnset); no model is named.
		d["InstanceUnset"] = len(s.bot.Settings().Models.Issues()) > 0
	} else {
		d["SlotPercent"] = s.bot.Settings().Limits.OrgSlotPercent
	}
	p.D = d
	s.render(w, r, "models.html", p)
}

// parseLLM reads one configuration's form fields onto cur. A blank API key
// keeps cur's.
func parseLLM(r *http.Request, cur settings.LLM) settings.LLM {
	v := cur
	f := func(k string) string { return strings.TrimSpace(r.FormValue(k)) }
	num := func(k string, def int) int {
		if n, err := strconv.Atoi(f(k)); err == nil {
			return n
		}
		return def
	}
	v.Name = f("name")
	v.Provider = f("provider")
	v.BaseURL = f("base_url")
	if k := f("api_key"); k != "" {
		v.APIKey = k
	}
	if r.FormValue("api_key_clear") != "" {
		v.APIKey = ""
	}
	v.Model = f("model")
	if t, err := strconv.ParseFloat(f("temperature"), 64); err == nil {
		v.Temperature = t
	}
	v.ContextTokens = num("context", v.ContextTokens)
	v.MaxOutputTokens = num("max_output", v.MaxOutputTokens)
	v.StructuredOutput = r.FormValue("structured") != ""
	v.NoTools = r.FormValue("tools") == ""
	v.TimeoutSeconds = num("timeout", v.TimeoutSeconds)
	v.Normalize()
	// A saved key is only kept for the server it was entered for: with the
	// address or provider changed, it has to be entered again, so whoever
	// edits a model can't send a key they never saw somewhere else (saving,
	// testing or listing models alike).
	if f("api_key") == "" && (v.BaseURL != cur.BaseURL || v.Provider != cur.Provider) {
		v.APIKey = ""
	}
	return v
}

// modelsLock returns the lock a change to sc's models holds from reading
// them to saving them, so two changes can't undo each other's. The
// instance's models are part of its settings, so they share settingsMu.
func (s *Server) modelsLock(sc modelScope) *sync.Mutex {
	if sc.org == nil {
		return &s.settingsMu
	}
	mu, _ := s.modelLocks.LoadOrStore(sc.org.ID, new(sync.Mutex))
	return mu.(*sync.Mutex)
}

// modelsChange loads the scope's models, applies change and saves them,
// then redirects back with msg or the error, which it returns.
func (s *Server) modelsChange(w http.ResponseWriter, r *http.Request, sc modelScope, change func(*settings.Models) (string, error)) error {
	msg, err := s.applyModels(r, sc, change)
	if err != nil {
		redirectErr(w, r, sc.base, err)
		return err
	}
	redirectMsg(w, r, sc.base, upperFirst(msg)+".")
	return nil
}

// applyModels is modelsChange without the redirect. The ChatGPT sign-ins
// of models the change removed (or signed in again, or moved to another
// provider) are ended once it's saved.
func (s *Server) applyModels(r *http.Request, sc modelScope, change func(*settings.Models) (string, error)) (string, error) {
	msg, gone, err := s.saveModelsChange(r, sc, change)
	if err != nil {
		return "", err
	}
	// Ended after the models lock is let go: ending one waits for a renewal
	// of it in progress, which other saves needn't. With the models saved,
	// nothing reaches these sign-ins any more. The change is made, so one
	// that couldn't be deleted (or read) is told in its message, not as a
	// failure.
	for _, id := range gone {
		if err := s.bot.DropChatGPT(id, sc.set()); err != nil {
			s.log.Warn("end ChatGPT sign-in", "set", sc.set(), "err", err)
			msg += ", but " + err.Error()
		}
	}
	return msg, nil
}

// saveModelsChange makes and saves the change under the models lock. It
// returns the change's message and the sign-ins no saved model names now.
func (s *Server) saveModelsChange(r *http.Request, sc modelScope, change func(*settings.Models) (string, error)) (string, []string, error) {
	mu := s.modelsLock(sc)
	mu.Lock()
	defer mu.Unlock()
	m, loadErr := s.loadModels(sc)
	if loadErr != nil && sc.org != nil && r.PathValue("action") != "delete" {
		return "", nil, loadErr
	}
	before := settings.Models{Configs: slices.Clone(m.Configs)}
	msg, err := change(&m)
	if err == nil {
		err = s.saveModels(sc, m)
	}
	if err != nil {
		return "", nil, err
	}
	s.signInChanged(sc, before, m)
	gone := bot.ForgottenChatGPT(before, m)
	if loadErr != nil {
		// An unreadable set was removed: which sign-ins its models named
		// can't be told, so all of the set's go, and so do the sign-ins
		// started for them.
		gone = s.bot.ChatGPTSignIns(sc.set())
		s.forgetSignIns(sc)
	}
	s.logModels(sc, r, msg)
	return msg, gone, nil
}

// modelsAction handles the page's forms: the strategy, adding, changing,
// moving and removing a configuration.
func (s *Server) modelsAction(w http.ResponseWriter, r *http.Request, sc modelScope) {
	id := r.PathValue("id")
	where := "the instance's models"
	if sc.org != nil {
		where = sc.org.Login + "'s own models"
	}
	switch r.PathValue("action") {
	case "strategy":
		s.modelsChange(w, r, sc, func(m *settings.Models) (string, error) {
			m.Strategy, m.Active = r.FormValue("strategy"), r.FormValue("active")
			m.Normalize()
			return fmt.Sprintf("set %s to %q", where, m.StrategyInfo().Label), nil
		})
	case "add":
		full := fmt.Errorf("a set can hold at most %d models", settings.MaxModels)
		// A full set fails at once, without waiting on the model's server
		// for its limits; the change checks again under the lock.
		if cur, err := s.loadModels(sc); err == nil && len(cur.Configs) >= settings.MaxModels {
			redirectErr(w, r, sc.base, full)
			return
		}
		mx := s.formMaximums(r, sc, settings.NewLLM())
		s.modelsChange(w, r, sc, func(m *settings.Models) (string, error) {
			if len(m.Configs) >= settings.MaxModels {
				return "", full
			}
			c := parseLLM(r, settings.NewLLM())
			c.ID = ""
			how := mx.apply(&c)
			if err := sc.check(c); err != nil {
				return "", err
			}
			m.Configs = append(m.Configs, c)
			msg := fmt.Sprintf("added %s to %s%s", c.Label(), where, how)
			if sc.org != nil && len(m.Configs) == 1 {
				msg += "; its reviews now use only its own models, with no daily review limit"
			}
			return msg, nil
		})
	case "update":
		var mx maximums
		if cur, err := s.loadModels(sc); err == nil {
			if c, ok := cur.Get(id); ok {
				mx = s.formMaximums(r, sc, c)
			}
		}
		s.modelsChange(w, r, sc, func(m *settings.Models) (string, error) {
			for i := range m.Configs {
				if m.Configs[i].ID == id {
					c := parseLLM(r, m.Configs[i])
					how := mx.apply(&c)
					if err := sc.check(c); err != nil {
						return "", err
					}
					m.Configs[i] = c
					return fmt.Sprintf("changed %s in %s%s", c.Label(), where, how), nil
				}
			}
			return "", errors.New("that model is no longer there")
		})
	case "move":
		s.modelsChange(w, r, sc, func(m *settings.Models) (string, error) {
			step := 1
			if r.FormValue("dir") == "up" {
				step = -1
			}
			for i := range m.Configs {
				if m.Configs[i].ID == id {
					j := i + step
					if j < 0 || j >= len(m.Configs) {
						return "", errors.New("it can't move further")
					}
					m.Configs[i], m.Configs[j] = m.Configs[j], m.Configs[i]
					return fmt.Sprintf("moved %s %s in %s", m.Configs[j].Label(), r.FormValue("dir"), where), nil
				}
			}
			return "", errors.New("that model is no longer there")
		})
	case "delete":
		s.modelsChange(w, r, sc, func(m *settings.Models) (string, error) {
			if r.FormValue("all") == "1" && sc.org != nil {
				// Also the way out when the stored set can't be read.
				*m = settings.Models{}
				return fmt.Sprintf("removed all of %s; %s uses the instance's models and daily review limit again", where, sc.org.Login), nil
			}
			for i := range m.Configs {
				if m.Configs[i].ID == id {
					c := m.Configs[i]
					m.Configs = append(m.Configs[:i], m.Configs[i+1:]...)
					msg := fmt.Sprintf("removed %s from %s", c.Label(), where)
					if sc.org != nil && len(m.Configs) == 0 {
						msg += fmt.Sprintf("; %s uses the instance's models and daily review limit again", sc.org.Login)
					}
					return msg, nil
				}
			}
			return "", errors.New("that model is no longer there")
		})
	case "test", "list", "limits":
		s.modelsProbe(w, r, sc)
	case "chatgpt-start", "chatgpt-finish", "chatgpt-cancel", "chatgpt-signout":
		s.chatgptAction(w, r, sc, id, where)
	default:
		s.notFound(w, r)
	}
}

// modelsProbe tests one configuration from its (unsaved) form, or lists its
// server's models. A blank key uses the saved one of the configuration named
// by the form's id.
func (s *Server) modelsProbe(w http.ResponseWriter, r *http.Request, sc modelScope) {
	cur := settings.NewLLM()
	if id := r.FormValue("id"); id != "" {
		if m, err := s.loadModels(sc); err == nil {
			if c, ok := m.Get(id); ok {
				cur = c
			}
		}
	}
	c := parseLLM(r, cur)
	if sc.own && r.PathValue("action") != "list" {
		if err := settings.CheckOwn(c); err != nil {
			respond(w, "", err, nil)
			return
		}
	}
	if r.PathValue("action") == "list" {
		ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
		defer cancel()
		models, err := s.bot.ListModels(ctx, c, sc.set())
		msg := fmt.Sprintf("%d models available.", len(models))
		if err == nil && len(models) == 0 {
			msg = "The server returned no models. Load or pull one first."
		}
		respond(w, msg, err, models)
		return
	}
	if r.PathValue("action") == "limits" {
		ctx, cancel := context.WithTimeout(r.Context(), limitsTimeout)
		defer cancel()
		lim, err := s.bot.ModelLimits(ctx, c, sc.set())
		if err != nil {
			respond(w, "", fmt.Errorf("couldn't read %s's token limits: %w", c.Model, err), nil)
			return
		}
		c.SetMaximums(lim.Context, lim.Output, true, true)
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "message": limitsNote(lim, c, true, true) + " Save to keep them.",
			"limits": map[string]int{"context": c.ContextTokens, "max_output": c.MaxOutputTokens}})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), time.Duration(c.TimeoutSeconds)*time.Second)
	defer cancel()
	msg, err := s.bot.TestModel(ctx, c, sc.set())
	respond(w, msg, err, nil)
}

// limitsTimeout bounds asking a model's server for its token limits.
const limitsTimeout = 15 * time.Second

// maximums are the token limits a model form left blank, which are set to
// the model's maximums, and those maximums as its server reported them.
type maximums struct {
	ctx, out bool // the context / output limit was left blank
	typedOut int  // the output limit typed in, if any
	lim      llm.Limits
	err      error // the server couldn't be asked, or didn't say
}

// formMaximums asks the model's server for its limits when the form leaves
// either blank. It's a network call, so it's made before the models' lock
// is taken. cur is the saved configuration the form edits (NewLLM to add
// one), whose key a form with none uses.
func (s *Server) formMaximums(r *http.Request, sc modelScope, cur settings.LLM) maximums {
	mx := maximums{ctx: strings.TrimSpace(r.FormValue("context")) == "", out: strings.TrimSpace(r.FormValue("max_output")) == ""}
	if n, err := strconv.Atoi(strings.TrimSpace(r.FormValue("max_output"))); err == nil {
		mx.typedOut = n
	}
	if !mx.ctx && !mx.out {
		return mx
	}
	c := parseLLM(r, cur)
	if err := sc.check(c); err != nil {
		mx.err = err // the change itself reports it
		return mx
	}
	if c.Provider == settings.ProviderChatGPT && !c.SignedIn() {
		mx.err = errors.New(`sign in with ChatGPT first, then press "Use model maximums"`)
		return mx
	}
	ctx, cancel := context.WithTimeout(r.Context(), limitsTimeout)
	defer cancel()
	mx.lim, mx.err = s.bot.ModelLimits(ctx, c, sc.set())
	return mx
}

// apply sets c's blank limits and says how, for the change's message.
func (mx maximums) apply(c *settings.LLM) string {
	if !mx.ctx && !mx.out {
		return ""
	}
	// parseLLM capped a typed output at half the context the form kept;
	// with the context now set from the server, the typed output is capped
	// at half of that instead (SetMaximums normalizes).
	if mx.ctx && !mx.out && mx.typedOut > 0 {
		c.MaxOutputTokens = mx.typedOut
	}
	c.SetMaximums(mx.lim.Context, mx.lim.Output, mx.ctx, mx.out)
	msg := ". " + strings.TrimSuffix(limitsNote(mx.lim, *c, mx.ctx, mx.out), ".")
	if typed := mx.typedOut; mx.ctx && !mx.out && typed > 0 && c.MaxOutputTokens != typed {
		why := fmt.Sprintf("half the %d-token context window", c.ContextTokens)
		if c.MaxOutputTokens > typed {
			why = "the least the bot allows"
		}
		msg += fmt.Sprintf("; the output limit typed, %d, was changed to %d, %s", typed, c.MaxOutputTokens, why)
	}
	if mx.err != nil {
		msg += fmt.Sprintf(" (its server couldn't be asked: %v); set them by hand if they're wrong", mx.err)
	}
	return msg
}

// limitsNote says where the limits that were set (ctx, out) came from: the
// model's maximums as its server reported them, half its window for the
// output when it sets no separate cap, or the defaults when it didn't say.
// A limit that wasn't set isn't mentioned.
func limitsNote(lim llm.Limits, c settings.LLM, ctx, out bool) string {
	var parts []string
	if ctx {
		switch {
		case lim.Context > 0 && c.ContextTokens > lim.Context:
			parts = append(parts, fmt.Sprintf("%d context tokens, the least the bot allows: the server reports a window of only %d, too small for a review, so load the model with a larger one", c.ContextTokens, lim.Context))
		case lim.Context > 0 && c.ContextTokens < lim.Context:
			parts = append(parts, fmt.Sprintf("%d context tokens, the most the bot allows (the model allows %d)", c.ContextTokens, lim.Context))
		case lim.Context > 0:
			parts = append(parts, fmt.Sprintf("%d context tokens, the model's maximum", c.ContextTokens))
		default:
			parts = append(parts, fmt.Sprintf("%d context tokens, the default (the server didn't report the model's window)", c.ContextTokens))
		}
	}
	if out {
		switch {
		case lim.Output > 0 && c.MaxOutputTokens > lim.Output:
			parts = append(parts, fmt.Sprintf("%d output tokens, the least the bot allows (the model reports a cap of only %d)", c.MaxOutputTokens, lim.Output))
		case lim.Output > 0 && c.MaxOutputTokens == lim.Output:
			parts = append(parts, fmt.Sprintf("%d output tokens, the model's maximum", c.MaxOutputTokens))
		case lim.Output > 0:
			parts = append(parts, fmt.Sprintf("%d output tokens, half the context window (the model allows %d, but the other half is kept for the prompt)", c.MaxOutputTokens, lim.Output))
		case lim.Context > 0:
			parts = append(parts, fmt.Sprintf("%d output tokens, half the context window (the server sets no separate output cap, so the other half is kept for the prompt)", c.MaxOutputTokens))
		case c.MaxOutputTokens < settings.FallbackOutputTokens:
			// The default didn't fit: Normalize cut it to half the window.
			parts = append(parts, fmt.Sprintf("%d output tokens, half the %d-token context window (the server didn't report a cap, and the default, %d, is more than half the window)", c.MaxOutputTokens, c.ContextTokens, settings.FallbackOutputTokens))
		default:
			parts = append(parts, fmt.Sprintf("%d output tokens, the default (the server didn't report a cap)", c.MaxOutputTokens))
		}
	}
	if len(parts) == 0 {
		return ""
	}
	return "Token limits set: " + strings.Join(parts, "; ") + "."
}

func (s *Server) instanceModelsAction(w http.ResponseWriter, r *http.Request) {
	s.modelsAction(w, r, instanceModels())
}

func (s *Server) orgModelsAction(w http.ResponseWriter, r *http.Request) {
	sc, ok := s.orgModelsScope(w, r, true)
	if !ok {
		return
	}
	s.modelsAction(w, r, sc)
}

func upperFirst(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}

// check reports what's wrong with c as one of sc's models: an organization's
// own must be at a public https address (settings.CheckOwn); the instance's
// need a well-formed one, as a malformed address would fail every job.
func (sc modelScope) check(c settings.LLM) error {
	if sc.own {
		return settings.CheckOwn(c)
	}
	return settings.CheckBaseURL(c.BaseURL)
}
