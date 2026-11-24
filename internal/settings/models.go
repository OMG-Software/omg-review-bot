package settings

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"

	"github.com/OMG-Software/omg-review-bot/internal/netguard"
	"github.com/OMG-Software/omg-review-bot/internal/textutil"
)

// How a set of models is used for each job.
const (
	// StrategySingle uses the active model; the others are kept for later.
	StrategySingle = "single"
	// StrategyFailover uses the models in order: when one fails, the next
	// takes over, for the rest of the job.
	StrategyFailover = "failover"
	// StrategyRoundRobin gives each job the next model in turn.
	StrategyRoundRobin = "roundrobin"
	// StrategyCollate reviews each PR with every model and merges the findings.
	StrategyCollate = "collate"
)

// Strategy describes a strategy for the UI.
type Strategy struct {
	ID, Label, Help string
}

// Strategies lists the strategies in the order the UI offers them.
var Strategies = []Strategy{
	{StrategySingle, "One model", "Every review and reply uses the selected model. The others are kept, ready to switch to."},
	{StrategyFailover, "Failover", "Use the first model. If it fails or times out, the next one in the list takes over."},
	{StrategyRoundRobin, "Round-robin", "Each review or reply uses the next model in turn, spreading the load."},
	{StrategyCollate, "Collate", "Each review runs on every model in the list and their findings are merged, with duplicates removed. Replies use the first model, failing over to the next."},
}

// MaxModels is the most models one set can hold.
const MaxModels = 8

// Models is a set of model configurations and how they're used: the
// instance's, or an organization's own (bring your own key).
type Models struct {
	Strategy string `json:"strategy"`
	Active   string `json:"active"` // the ID StrategySingle uses
	Configs  []LLM  `json:"configs"`
}

// Get returns the configuration with id.
func (m Models) Get(id string) (LLM, bool) {
	for _, c := range m.Configs {
		if c.ID == id {
			return c, true
		}
	}
	return LLM{}, false
}

// Primary is the model that stands for the set where one is needed: the
// active one for StrategySingle, otherwise the first.
func (m Models) Primary() LLM {
	if m.Strategy == StrategySingle {
		if c, ok := m.Get(m.Active); ok {
			return c
		}
	}
	if len(m.Configs) > 0 {
		return m.Configs[0]
	}
	return LLM{}
}

// InUse lists the models jobs use, in order: only the active one for
// StrategySingle, otherwise all of them.
func (m Models) InUse() []LLM {
	if m.Strategy == StrategySingle {
		if c, ok := m.Get(m.Active); ok {
			return []LLM{c}
		}
	}
	return m.Configs
}

// StrategyInfo returns the set's strategy's description.
func (m Models) StrategyInfo() Strategy {
	for _, s := range Strategies {
		if s.ID == m.Strategy {
			return s
		}
	}
	return Strategies[0]
}

// Normalize gives every configuration a unique ID, normalizes each one and
// makes the strategy and active model valid.
func (m *Models) Normalize() {
	if len(m.Configs) > MaxModels {
		m.Configs = m.Configs[:MaxModels]
	}
	used := map[string]bool{}
	for i := range m.Configs {
		c := &m.Configs[i]
		c.ID = strings.TrimSpace(c.ID)
		if used[c.ID] {
			c.ID = "" // a copy: it gets an ID of its own below
		}
		if c.ID != "" {
			used[c.ID] = true
		}
	}
	n := 1
	for i := range m.Configs {
		c := &m.Configs[i]
		if c.ID == "" {
			for used[fmt.Sprintf("m%d", n)] {
				n++
			}
			c.ID = fmt.Sprintf("m%d", n)
			used[c.ID] = true
		}
		c.Normalize()
	}
	switch m.Strategy {
	case StrategySingle, StrategyFailover, StrategyRoundRobin, StrategyCollate:
	default:
		m.Strategy = StrategySingle
	}
	if _, ok := m.Get(m.Active); !ok {
		m.Active = ""
		if len(m.Configs) > 0 {
			m.Active = m.Configs[0].ID
		}
	}
}

// Issues lists what's missing from the models jobs use.
func (m Models) Issues() []string {
	inUse := m.InUse()
	if len(inUse) == 0 {
		return []string{"No model is configured"}
	}
	var out []string
	for _, c := range inUse {
		name := ""
		if len(inUse) > 1 {
			name = " (" + c.Label() + ")"
		}
		if c.BaseURL == "" {
			out = append(out, "LLM base URL is not set"+name)
		}
		if c.Model == "" {
			out = append(out, "LLM model is not set"+name)
		}
		if c.Provider == "ollama-cloud" && c.APIKey == "" {
			out = append(out, "Ollama Cloud needs an API key"+name)
		}
	}
	return out
}

// Label names the configuration for people: its name, else its model.
func (l LLM) Label() string {
	switch {
	case l.Name != "":
		return l.Name
	case l.Model != "":
		return l.Model
	}
	return "Model " + l.ID
}

// Normalize trims the configuration's values and clamps its numbers.
func (l *LLM) Normalize() {
	l.Name = textutil.Truncate(strings.TrimSpace(l.Name), 60)
	l.Provider = strings.TrimSpace(l.Provider)
	if DefaultBaseURL(l.Provider) == "" && l.Provider != "openai" {
		l.Provider = "lmstudio"
	}
	l.BaseURL = strings.TrimRight(strings.TrimSpace(l.BaseURL), "/")
	if l.BaseURL == "" {
		l.BaseURL = DefaultBaseURL(l.Provider)
	}
	l.APIKey = strings.TrimSpace(l.APIKey)
	l.Model = strings.TrimSpace(l.Model)
	l.Temperature = clampF(l.Temperature, 0, 2)
	l.ContextTokens = clamp(l.ContextTokens, MinContextTokens, MaxContextTokens)
	l.MaxOutputTokens = clamp(l.MaxOutputTokens, MinOutputTokens, l.ContextTokens/2)
	l.TimeoutSeconds = clamp(l.TimeoutSeconds, 30, 3600)
}

// NewLLM is a new configuration with the default limits, for the UI to fill in.
func NewLLM() LLM { return Defaults().LLM }

// A model's token limits default to its maximums, read from its server when
// it's added (see SetMaximums). These are used when the server doesn't say.
const (
	FallbackContextTokens = 131072
	FallbackOutputTokens  = 16384
)

// The limits a model can be given (Normalize). A window smaller than
// MinContextTokens can't hold a review's prompt, so a server reporting one
// still gets the minimum, and the bot says so rather than calling it the
// model's maximum.
const (
	MinContextTokens = 4096
	MaxContextTokens = 2_000_000
	MinOutputTokens  = 512
)

// SetMaximums sets the context window (setCtx) and output cap (setOut) to
// the model's maximums: ctxMax and outMax as its server reports them, 0 when
// it doesn't. A server that reports a window but no output cap (Ollama, LM
// Studio) lets an answer use what the prompt leaves, so the output cap is
// the most the bot allows: half the window, the other half kept for the
// prompt. Limits the server doesn't report fall back to the defaults above.
func (l *LLM) SetMaximums(ctxMax, outMax int, setCtx, setOut bool) {
	if setCtx {
		l.ContextTokens = FallbackContextTokens
		if ctxMax > 0 {
			l.ContextTokens = ctxMax
		}
	}
	if setOut {
		switch {
		case outMax > 0:
			l.MaxOutputTokens = outMax
		case ctxMax > 0:
			// Half the smaller window: a context kept as it was can be
			// larger than the server's.
			l.MaxOutputTokens = min(l.ContextTokens, ctxMax) / 2
		default:
			l.MaxOutputTokens = FallbackOutputTokens
		}
	}
	l.Normalize()
}

// CheckOwn reports what's wrong with an organization's own model
// configuration. Unlike the instance's models (set by its administrators),
// an organization's are reached at an address its owners chose, so it has
// to be a public https server: never the bot's own machine or network.
func CheckOwn(l LLM) error {
	if l.Model == "" {
		return errors.New("choose a model")
	}
	if l.BaseURL == "" {
		return errors.New("enter the model server's base URL")
	}
	if l.Provider == "ollama-cloud" && l.APIKey == "" {
		return errors.New("an Ollama Cloud model needs an API key") // as Models.Issues requires
	}
	if err := netguard.CheckURL(l.BaseURL); err != nil {
		return fmt.Errorf("the base URL must be a public https address: %w", err)
	}
	return nil
}

// CheckBaseURL reports what's wrong with raw as a model server's base URL:
// it must be an http or https address with a host. Organizations' own
// models must also pass CheckOwn.
func CheckBaseURL(raw string) error {
	if raw == "" {
		return errors.New("enter the model server's base URL")
	}
	u, err := url.Parse(raw)
	switch {
	case err != nil:
		return fmt.Errorf("%q isn't a valid address: %w", raw, err)
	case u.Scheme != "http" && u.Scheme != "https":
		return fmt.Errorf("%q must start with http:// or https://", raw)
	case u.Hostname() == "":
		return fmt.Errorf("%q has no host name", raw)
	}
	return nil
}

// WithOwnModels returns the settings with an organization's own models in
// place of the instance's.
func (s Settings) WithOwnModels(accountID int64, m Models) Settings {
	m.Normalize()
	s.Models, s.OwnModels = m, accountID
	s.LLM = m.Primary()
	return s
}

// Tightest returns the configuration whose input budget (context less output)
// is smallest among cfgs, so a prompt sized for it fits every one of them.
func Tightest(cfgs []LLM) LLM {
	var best LLM
	for i, c := range cfgs {
		if i == 0 || c.ContextTokens-c.MaxOutputTokens < best.ContextTokens-best.MaxOutputTokens {
			best = c
		}
	}
	return best
}

// ParseModels reads an organization's stored models ("" = none).
func ParseModels(raw []byte) (Models, error) {
	var m Models
	if len(raw) == 0 {
		return m, nil
	}
	if err := json.Unmarshal(raw, &m); err != nil {
		return Models{}, err
	}
	m.Normalize()
	return m, nil
}
