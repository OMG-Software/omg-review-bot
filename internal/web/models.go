package web

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

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
}

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
		p.Err = "The organization's own models couldn't be read: " + err.Error()
	}
	// Keys never go into the page's data, only whether one is saved.
	shown := m
	shown.Configs = make([]settings.LLM, len(m.Configs))
	views := make([]modelView, len(m.Configs))
	for i, c := range m.Configs {
		key := c.APIKey != ""
		c.APIKey = ""
		shown.Configs[i] = c
		views[i] = modelView{LLM: c, HasKey: key}
	}
	d := map[string]any{
		"Base": sc.base, "Own": sc.own, "M": shown, "Configs": views, "ReadOnly": readOnly,
		"Strategies": settings.Strategies, "Providers": settings.Providers, "New": settings.NewLLM(),
		"Max": settings.MaxModels, "Full": len(m.Configs) >= settings.MaxModels, "Unreadable": err != nil,
	}
	if sc.org != nil {
		// Only the names of the instance's models: members can see this page.
		var names []string
		for _, c := range s.bot.Settings().Models.InUse() {
			names = append(names, c.Label())
		}
		d["InstanceInUse"] = names
		d["DailyLimit"] = sc.org.DailyLimit()
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
	v.TimeoutSeconds = num("timeout", v.TimeoutSeconds)
	v.Normalize()
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
// then redirects back with msg or the error.
func (s *Server) modelsChange(w http.ResponseWriter, r *http.Request, sc modelScope, change func(*settings.Models) (string, error)) {
	mu := s.modelsLock(sc)
	mu.Lock()
	defer mu.Unlock()
	m, err := s.loadModels(sc)
	if err != nil && sc.org != nil && r.PathValue("action") != "delete" {
		redirectErr(w, r, sc.base, err)
		return
	}
	msg, err := change(&m)
	if err == nil {
		err = s.saveModels(sc, m)
	}
	if err != nil {
		redirectErr(w, r, sc.base, err)
		return
	}
	s.logModels(sc, r, msg)
	redirectMsg(w, r, sc.base, upperFirst(msg)+".")
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
		s.modelsChange(w, r, sc, func(m *settings.Models) (string, error) {
			if len(m.Configs) >= settings.MaxModels {
				return "", fmt.Errorf("a set can hold at most %d models", settings.MaxModels)
			}
			c := parseLLM(r, settings.NewLLM())
			c.ID = ""
			if err := sc.check(c); err != nil {
				return "", err
			}
			m.Configs = append(m.Configs, c)
			msg := fmt.Sprintf("added %s to %s", c.Label(), where)
			if sc.org != nil && len(m.Configs) == 1 {
				msg += "; its reviews now use only its own models, with no daily review limit"
			}
			return msg, nil
		})
	case "update":
		s.modelsChange(w, r, sc, func(m *settings.Models) (string, error) {
			for i := range m.Configs {
				if m.Configs[i].ID == id {
					c := parseLLM(r, m.Configs[i])
					if err := sc.check(c); err != nil {
						return "", err
					}
					m.Configs[i] = c
					return fmt.Sprintf("changed %s in %s", c.Label(), where), nil
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
	case "test", "list":
		s.modelsProbe(w, r, sc)
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
	if sc.own && r.PathValue("action") == "test" {
		if err := settings.CheckOwn(c); err != nil {
			respond(w, "", err, nil)
			return
		}
	}
	if r.PathValue("action") == "list" {
		ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
		defer cancel()
		models, err := s.bot.ListModels(ctx, c, sc.own)
		msg := fmt.Sprintf("%d models available.", len(models))
		if err == nil && len(models) == 0 {
			msg = "The server returned no models. Load or pull one first."
		}
		respond(w, msg, err, models)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), time.Duration(c.TimeoutSeconds)*time.Second)
	defer cancel()
	msg, err := s.bot.TestModel(ctx, c, sc.own)
	respond(w, msg, err, nil)
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
