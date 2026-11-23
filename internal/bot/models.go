package bot

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/OMG-Software/omg-review-bot/internal/llm"
	"github.com/OMG-Software/omg-review-bot/internal/secret"
	"github.com/OMG-Software/omg-review-bot/internal/settings"
	"github.com/OMG-Software/omg-review-bot/internal/store"
)

// An organization can bring its own models (and API keys). Once it has one,
// its jobs use only its own, never the instance's, and the daily review
// limit no longer applies to it: the limit shares out the instance's models.
// Removing the last one puts it back on the instance's models and the limit.
//
// Its configurations are stored sealed (see package secret) and its servers
// are reached only over https at public addresses (llm.Config.PublicOnly).

// secretBox returns the box that seals organizations' own models, loading
// (or creating) its key on first use.
func (b *Bot) secretBox() (*secret.Box, error) {
	b.boxOnce.Do(func() {
		if b.box == nil {
			b.box, b.boxErr = secret.Load(b.dataDir)
		}
	})
	return b.box, b.boxErr
}

// modelsContext binds an organization's sealed models to it.
func modelsContext(accountID int64) string { return "org-models:" + strconv.FormatInt(accountID, 10) }

// OrgModels returns the organization's own models (an empty set when it uses
// the instance's).
func (b *Bot) OrgModels(accountID int64) (settings.Models, error) {
	sealed, err := b.store.OrgModels(accountID)
	if err != nil || sealed == "" {
		return settings.Models{}, err
	}
	box, err := b.secretBox()
	if err != nil {
		return settings.Models{}, err
	}
	raw, err := box.Open(sealed, modelsContext(accountID))
	if err != nil {
		return settings.Models{}, err
	}
	return settings.ParseModels(raw)
}

// SetOrgModels stores the organization's own models. With none left, it
// goes back to the instance's models and its daily review limit.
func (b *Bot) SetOrgModels(accountID int64, m settings.Models) error {
	m.Normalize()
	sealed := ""
	if len(m.Configs) > 0 {
		for _, c := range m.Configs {
			if err := settings.CheckOwn(c); err != nil {
				return fmt.Errorf("%s: %w", c.Label(), err)
			}
		}
		raw, err := json.Marshal(m)
		if err != nil {
			return err
		}
		box, err := b.secretBox()
		if err != nil {
			return err
		}
		if sealed, err = box.Seal(raw, modelsContext(accountID)); err != nil {
			return err
		}
	}
	if err := b.store.SetOrgModels(accountID, sealed); err != nil {
		return err
	}
	b.Wake() // PRs the daily limit held may go ahead now
	return nil
}

// modelSettings returns st with the models accountID's jobs use: its own if
// it has any, else the instance's. st.LLM becomes the in-use model with the
// least room for input, so prompts sized by it fit whichever model runs them.
// An organization's models that can't be read are an error: the instance's
// aren't used in their place.
func (b *Bot) modelSettings(st settings.Settings, accountID int64) (settings.Settings, error) {
	if accountID != 0 {
		own, err := b.OrgModels(accountID)
		if err != nil {
			return st, fmt.Errorf("couldn't read the organization's own models: %w", err)
		}
		if len(own.Configs) > 0 {
			st = st.WithOwnModels(accountID, own)
		}
	}
	st.LLM = settings.Tightest(st.Models.InUse())
	return st, nil
}

// modelUse is one model configuration and its client.
type modelUse struct {
	cfg settings.LLM
	lc  llm.Client
}

// modelClient is an llm.Client that can say which model answered.
type modelClient interface {
	llm.Client
	Label() string
}

// ownPublicOnly: organizations' own models are reached only at public https
// addresses. Tests turn it off to serve them from httptest.
var ownPublicOnly = true

// newClient returns the client for one configuration. Organizations' own
// models are reached only at public https addresses.
func newClient(cfg settings.LLM, own bool) (llm.Client, error) {
	return llm.New(llm.Config{
		Provider:      cfg.Provider,
		BaseURL:       cfg.BaseURL,
		APIKey:        cfg.APIKey,
		Model:         cfg.Model,
		ContextTokens: cfg.ContextTokens,
		Structured:    cfg.StructuredOutput,
		Timeout:       time.Duration(cfg.TimeoutSeconds) * time.Second,
		PublicOnly:    own && ownPublicOnly,
	})
}

// tuned calls one model with its own temperature, and no more output than
// it allows: callers size requests by st.LLM, which may be another model.
type tuned struct{ modelUse }

func (t tuned) Chat(ctx context.Context, req llm.Request) (*llm.Response, error) {
	req.Temperature = t.cfg.Temperature
	if req.MaxTokens <= 0 || req.MaxTokens > t.cfg.MaxOutputTokens {
		req.MaxTokens = t.cfg.MaxOutputTokens
	}
	return t.lc.Chat(ctx, req)
}

func (t tuned) Models(ctx context.Context) ([]string, error) { return t.lc.Models(ctx) }
func (t tuned) Label() string                                { return t.cfg.Label() }

// failover tries its models in order. Once one fails, the next takes over
// for the rest of the job: the failed one isn't tried again.
type failover struct {
	uses     []tuned
	onSwitch func(from, to string, err error)

	mu   sync.Mutex
	cur  int
	used []string // labels of the models that answered, in order
}

func (f *failover) Chat(ctx context.Context, req llm.Request) (*llm.Response, error) {
	f.mu.Lock()
	start := f.cur
	f.mu.Unlock()
	var errs []error
	for i := start; i < len(f.uses); i++ {
		resp, err := f.uses[i].Chat(ctx, req)
		if err == nil {
			f.mu.Lock()
			f.cur = i
			if n := len(f.used); n == 0 || f.used[n-1] != f.uses[i].Label() {
				f.used = append(f.used, f.uses[i].Label())
			}
			f.mu.Unlock()
			return resp, nil
		}
		if ctx.Err() != nil {
			return nil, err // the job is over, not the model
		}
		errs = append(errs, fmt.Errorf("%s: %w", f.uses[i].Label(), err))
		if i+1 < len(f.uses) && f.onSwitch != nil {
			f.onSwitch(f.uses[i].Label(), f.uses[i+1].Label(), err)
		}
	}
	if len(errs) == 1 {
		return nil, errors.Unwrap(errs[0])
	}
	return nil, errors.Join(errs...)
}

func (f *failover) Models(ctx context.Context) ([]string, error) {
	var errs []error
	for _, u := range f.uses {
		ms, err := u.Models(ctx)
		if err == nil {
			return ms, nil
		}
		errs = append(errs, err)
	}
	return nil, errors.Join(errs...)
}

// Label names the models that answered ("a → b" after a failover), or the
// first one before any has.
func (f *failover) Label() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.used) == 0 {
		return f.uses[f.cur].Label()
	}
	return strings.Join(f.used, " → ")
}

// modelUses builds the clients for st's models in use, in order.
func modelUses(st settings.Settings) ([]tuned, error) {
	cfgs := st.Models.InUse()
	if len(cfgs) == 0 {
		return nil, errors.New("no model is configured")
	}
	out := make([]tuned, 0, len(cfgs))
	for _, c := range cfgs {
		lc, err := newClient(c, st.OwnModels != 0)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", c.Label(), err)
		}
		out = append(out, tuned{modelUse{cfg: c, lc: lc}})
	}
	return out, nil
}

// nextRoundRobin returns the index of the next model in turn for the set
// st's models belong to (the instance's, or an organization's own).
func (b *Bot) nextRoundRobin(st settings.Settings, n int) int {
	b.mu.Lock()
	defer b.mu.Unlock()
	i := b.roundRobin[st.OwnModels]
	b.roundRobin[st.OwnModels] = i + 1
	return i % n
}

// chatClient returns the client one job's model calls go through, by the
// strategy of st's models: the active model, the next in turn, or each in
// order as the one before fails (failover and collate). pr, if not nil, is
// the PR the job is for, which a failover is logged against.
func (b *Bot) chatClient(st settings.Settings, pr *store.PR) (modelClient, error) {
	uses, err := modelUses(st)
	if err != nil {
		return nil, err
	}
	switch st.Models.Strategy {
	case settings.StrategyRoundRobin:
		return uses[b.nextRoundRobin(st, len(uses))], nil
	case settings.StrategyFailover, settings.StrategyCollate:
		if len(uses) == 1 {
			return uses[0], nil
		}
		return &failover{uses: uses, onSwitch: func(from, to string, err error) {
			if pr != nil {
				b.logf("warn", pr.Repo, pr.Number, "model %s failed (%v); failing over to %s", from, err, to)
			} else {
				b.log.Warn("model failed; failing over", "from", from, "to", to, "err", err)
			}
		}}, nil
	}
	return uses[0], nil
}

// reviewClients returns the clients a review runs its diff through: each
// model for StrategyCollate, otherwise the one chatClient gives.
func (b *Bot) reviewClients(st settings.Settings, pr *store.PR) ([]modelClient, error) {
	if st.Models.Strategy == settings.StrategyCollate && len(st.Models.InUse()) > 1 {
		uses, err := modelUses(st)
		if err != nil {
			return nil, err
		}
		out := make([]modelClient, len(uses))
		for i, u := range uses {
			out[i] = u
		}
		return out, nil
	}
	lc, err := b.chatClient(st, pr)
	if err != nil {
		return nil, err
	}
	return []modelClient{lc}, nil
}

// labels names the models of a review, for its notice and record.
func labels(lcs []modelClient) string {
	names := make([]string, len(lcs))
	for i, lc := range lcs {
		names[i] = lc.Label()
	}
	return strings.Join(names, " + ")
}

// collateFindings merges the findings of reviews of the same diff by
// different models. A finding another model already raised (same file, a
// line or so apart, a similar title) is dropped, keeping the more severe of
// the two levels. One list is returned as it is.
func collateFindings(p plan, lists [][]finding) []finding {
	if len(lists) == 1 {
		return lists[0]
	}
	var out []finding
	var from []int // which list each kept finding came from
	for li, l := range lists {
	next:
		for _, f := range l {
			f.Path = matchPath(p.files, f.Path)
			f.Severity = normSeverity(f.Severity)
			for k := range out {
				g := &out[k]
				if from[k] == li || g.Path != f.Path || abs(g.Line-f.Line) > 3 || similarity(g.Title, f.Title) < 0.5 {
					continue
				}
				if settings.SeverityRank(f.Severity) < settings.SeverityRank(g.Severity) {
					g.Severity = f.Severity
				}
				continue next
			}
			out = append(out, f)
			from = append(from, li)
		}
	}
	return out
}

func abs(n int) int {
	if n < 0 {
		return -n
	}
	return n
}

// TestModel sends a tiny prompt to one model configuration. own marks an
// organization's own model, reached only at public https addresses.
func (b *Bot) TestModel(ctx context.Context, cfg settings.LLM, own bool) (string, error) {
	lc, err := newClient(cfg, own)
	if err != nil {
		return "", err
	}
	resp, err := lc.Chat(ctx, llm.Request{
		Messages: []llm.Message{
			{Role: "system", Content: "You are a connectivity check. Follow instructions exactly."},
			{Role: "user", Content: "Reply with exactly the word: OK"},
		},
		MaxTokens: cfg.MaxOutputTokens, // same budget as reviews, so reasoning models are tested realistically
	})
	if err != nil {
		return "", err
	}
	reply := llm.StripThinking(resp.Content)
	if len(reply) > 80 {
		reply = reply[:80] + "…"
	}
	return fmt.Sprintf("%s replied %q in %s (%d prompt / %d completion tokens).",
		cfg.Model, reply, resp.Duration.Round(time.Millisecond), resp.PromptTokens, resp.CompletionTokens), nil
}

// ListModels lists the models one configuration's server offers.
func (b *Bot) ListModels(ctx context.Context, cfg settings.LLM, own bool) ([]string, error) {
	lc, err := newClient(cfg, own)
	if err != nil {
		return nil, err
	}
	return lc.Models(ctx)
}
