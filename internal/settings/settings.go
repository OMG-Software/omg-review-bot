// Package settings defines the runtime configuration edited from the web UI.
package settings

import (
	"encoding/json"
	"fmt"
	"net/url"
	"strings"

	"github.com/OMG-Software/omg-review-bot/internal/store"
)

const storeKey = "settings"

type GitHub struct {
	AppID           int64    `json:"app_id"`
	PrivateKey      string   `json:"private_key"`
	APIBaseURL      string   `json:"api_base_url"`
	Accounts        []string `json:"accounts"` // only installations on these orgs/users; empty = all
	NewReposEnabled bool     `json:"new_repos_enabled"`

	// ClientID and ClientSecret are the App's OAuth credentials, used for
	// "Sign in with GitHub". Empty = only the administrator password works.
	ClientID     string `json:"client_id"`
	ClientSecret string `json:"client_secret"`
	// WebhookSecret verifies webhook deliveries. Empty = webhooks are refused.
	WebhookSecret string `json:"webhook_secret"`
	// PublicURL is the address people and GitHub reach the bot at, e.g.
	// https://prbot.example.com. Empty = the address the browser used.
	PublicURL string `json:"public_url"`
}

// SignInOn reports whether "Sign in with GitHub" is configured.
func (g GitHub) SignInOn() bool { return g.ClientID != "" && g.ClientSecret != "" }

// LLM is one model configuration: where the model is served and how it's
// called. A set of them, and how they're used, is Models.
type LLM struct {
	ID               string  `json:"id,omitempty"`   // stable within its set ("m1", "m2", …)
	Name             string  `json:"name,omitempty"` // shown in the UI; empty = the model's name
	Provider         string  `json:"provider"`       // one of Providers' IDs
	BaseURL          string  `json:"base_url"`
	APIKey           string  `json:"api_key"`
	Model            string  `json:"model"`
	Temperature      float64 `json:"temperature"`
	ContextTokens    int     `json:"context_tokens"`
	MaxOutputTokens  int     `json:"max_output_tokens"`
	StructuredOutput bool    `json:"structured_output"`
	TimeoutSeconds   int     `json:"timeout_seconds"`
	// NoTools stops the review offering the model its read-only repository
	// tools. Off (tools offered) by default, including for settings saved
	// before the tools existed.
	NoTools bool `json:"no_tools,omitempty"`

	// A ProviderChatGPT model's sign-in. Account names its sealed tokens
	// (see bot.storeChatGPT), empty while it's signed out. The client OpenAI
	// issued at its first sign-in, and the account's subject and email, are
	// kept while it's signed in, to sign the same account in again, and
	// after a sign-in that declined the plan's use, to ask for it again. A
	// sign-out clears them all, so the next sign-in can be another account.
	// None of them is secret, and none is read from a form: only a sign-in
	// sets them.
	Account        string `json:"account,omitempty"`
	AccountEmail   string `json:"account_email,omitempty"`
	AccountSubject string `json:"account_subject,omitempty"`
	ChatGPTClient  string `json:"chatgpt_client,omitempty"`
}

// SignedIn reports whether a ProviderChatGPT model has a sign-in to use.
func (l LLM) SignedIn() bool { return l.Account != "" }

type Review struct {
	Enabled         bool     `json:"enabled"`
	SkillDir        string   `json:"skill_dir"` // empty = <data>/skill
	SkipDrafts      bool     `json:"skip_drafts"`
	SkipLabels      []string `json:"skip_labels"`
	IgnorePatterns  []string `json:"ignore_patterns"`
	DebounceSeconds int      `json:"debounce_seconds"`
	MaxComments     int      `json:"max_comments"`
	// CommentLevel is the least severe level ("P1".."P5") posted on the PR.
	CommentLevel string `json:"comment_level"`
	// ApprovalLevel ("P1".."P5", or ApprovalOff): the bot approves once no
	// finding at this level or above is unresolved.
	ApprovalLevel string `json:"approval_level"`
	// RequestChangesLevel ("P1".."P5", or LevelOff): the review requests
	// changes while a finding at this level or above is unresolved. It can't
	// be broader than the approval level: a finding that requests changes
	// must also hold back approval.
	RequestChangesLevel string `json:"request_changes_level"`
	// MaxRounds is how many reviews the bot posts on one PR automatically
	// (MinRounds..MaxRoundsLimit). A review asked for by hand goes past it.
	MaxRounds      int  `json:"max_rounds"`
	MaxFiles       int  `json:"max_files"`
	ReviewExisting bool `json:"review_existing"`
	// GuidelineFiles are gitignore-style patterns for the repository's own
	// rules and conventions docs, read from the PR's base branch. Empty = off.
	GuidelineFiles []string `json:"guideline_files"`

	// LegacyMinSeverity is the pre-P1..P5 setting; Normalize migrates it.
	LegacyMinSeverity string `json:"min_severity,omitempty"`
}

// ApprovalOn reports whether the bot approves pull requests at all.
func (r Review) ApprovalOn() bool { return SeverityRank(r.ApprovalLevel) > 0 }

// RequestChangesOn reports whether the bot requests changes at all.
func (r Review) RequestChangesOn() bool { return SeverityRank(r.RequestChangesLevel) > 0 }

type Replies struct {
	Enabled      bool `json:"enabled"`
	Mentions     bool `json:"mentions"`
	MaxPerThread int  `json:"max_per_thread"`
}

type Poll struct {
	IntervalSeconds    int `json:"interval_seconds"`
	RepoRefreshMinutes int `json:"repo_refresh_minutes"`
	// WebhookIntervalSeconds is the poll interval while webhooks are arriving:
	// polling is then only a safety net for missed deliveries.
	WebhookIntervalSeconds int `json:"webhook_interval_seconds"`
}

// Limits share the instance between organizations.
type Limits struct {
	// OrgSlotPercent is the largest share of the review slots (workers) one
	// organization's jobs may hold at once, so one busy organization can't
	// keep the others waiting. At least one slot is always allowed.
	OrgSlotPercent int `json:"org_slot_percent"`
}

// DefaultOrgSlotPercent is Limits.OrgSlotPercent unless an administrator changes it.
const DefaultOrgSlotPercent = 50

// OrgSlots is how many of slots one organization's jobs may hold at once.
func (l Limits) OrgSlots(slots int) int {
	return max(1, min(slots, slots*l.OrgSlotPercent/100))
}

type Settings struct {
	GitHub GitHub `json:"github"`
	// LLM is the primary model of Models (see Models.Primary). It's kept in
	// step on every Normalize, for the code that needs one model's limits
	// and for older versions reading the settings; edit Models, not this.
	LLM     LLM     `json:"llm"`
	Models  Models  `json:"models"`
	Review  Review  `json:"review"`
	Replies Replies `json:"replies"`
	Poll    Poll    `json:"poll"`
	Limits  Limits  `json:"limits"`

	// OwnModels is the GitHub account whose own models (bring your own key)
	// Models holds, or 0 when they're the instance's. Never stored: it's set
	// by WithOwnModels for the job at hand.
	OwnModels int64 `json:"-"`
}

// Policy is the part of the settings an organization can set for itself.
// The model, skill, GitHub App and polling stay instance-wide.
type Policy struct {
	Review  Review  `json:"review"`
	Replies Replies `json:"replies"`
}

// Policy returns the instance-wide review policy (what organizations inherit).
func (s Settings) Policy() Policy { return Policy{Review: s.Review, Replies: s.Replies} }

// WithPolicy returns the settings with an organization's policy applied.
func (s Settings) WithPolicy(p Policy) Settings {
	p.Review.SkillDir = s.Review.SkillDir
	if p.Review.RequestChangesLevel == "" {
		// Saved before the setting existed: inherit the instance's level.
		// Turning it off is saved as LevelOff, never "".
		p.Review.RequestChangesLevel = s.Review.RequestChangesLevel
	}
	if p.Review.MaxRounds == 0 {
		p.Review.MaxRounds = s.Review.MaxRounds // likewise; 0 is never a saved limit
	}
	s.Review, s.Replies = p.Review, p.Replies
	s.Normalize()
	return s
}

// ParsePolicy reads an organization's stored policy. ok is false when it has
// none (it inherits the instance settings) or it can't be read.
func ParsePolicy(raw string) (p Policy, ok bool) {
	if strings.TrimSpace(raw) == "" {
		return Policy{}, false
	}
	if err := json.Unmarshal([]byte(raw), &p); err != nil {
		return Policy{}, false
	}
	return p, true
}

// EncodePolicy stores a policy (normalized against the instance settings).
func EncodePolicy(base Settings, p Policy) (string, error) {
	n := base.WithPolicy(p).Policy()
	n.Review.SkillDir = ""
	b, err := json.Marshal(n)
	return string(b), err
}

// Provider describes an LLM backend choice shown in the UI.
type Provider struct {
	ID, Label, Base string
	// Key: the hosted API needs an API key.
	Key bool
}

// The providers whose IDs the code refers to.
const (
	ProviderOpenRouter = "openrouter"
	ProviderAnthropic  = "anthropic"
	ProviderOpenAI     = "openai-api" // OpenAI's own API ("openai" is any compatible server)
	// ProviderChatGPT uses a ChatGPT plan through Sign in with ChatGPT: no
	// API key, but a sign-in whose tokens the bot keeps and renews.
	ProviderChatGPT = "chatgpt"
)

// ChatGPTBaseURL is the only address a ChatGPT sign-in's tokens are sent to:
// a ProviderChatGPT model's base URL can't be changed.
const ChatGPTBaseURL = "https://api.openai.com/v1"

var Providers = []Provider{
	{ID: "lmstudio", Label: "LM Studio", Base: "http://localhost:1234/v1"},
	{ID: "ollama", Label: "Ollama (local)", Base: "http://localhost:11434"},
	{ID: "ollama-cloud", Label: "Ollama Cloud", Base: "https://ollama.com", Key: true},
	{ID: ProviderOpenRouter, Label: "OpenRouter", Base: "https://openrouter.ai/api/v1", Key: true},
	{ID: ProviderAnthropic, Label: "Anthropic API (Claude)", Base: "https://api.anthropic.com", Key: true},
	{ID: ProviderOpenAI, Label: "OpenAI API (ChatGPT models)", Base: "https://api.openai.com/v1", Key: true},
	{ID: ProviderChatGPT, Label: "ChatGPT plan (Sign in with ChatGPT)", Base: ChatGPTBaseURL},
	{ID: "openai", Label: "Other OpenAI-compatible", Base: ""},
}

// ProviderInfo returns the provider with id, and whether there is one.
func ProviderInfo(id string) (Provider, bool) {
	for _, p := range Providers {
		if p.ID == id {
			return p, true
		}
	}
	return Provider{}, false
}

func DefaultBaseURL(provider string) string {
	p, _ := ProviderInfo(provider)
	return p.Base
}

// Severity is one finding level. Rank 1 (P1) is the most severe.
type Severity struct {
	ID, Label, Icon, Meaning string
}

// Severities lists the levels from most to least severe.
var Severities = []Severity{
	{"P1", "[P1] CRITICAL", "🔴", "security hole, data loss or corruption, crash on a main path"},
	{"P2", "[P2] MAJOR", "🟠", "wrong behaviour people will hit, or a serious risk under realistic conditions"},
	{"P3", "[P3] MINOR (Defect)", "🟡", "a real defect with limited impact, such as an edge case or a missing timeout"},
	{"P4", "[P4] MINOR (Quality)", "🔵", "not a bug: maintainability, duplication, missing tests, clarity"},
	{"P5", "[P5] NIT", "⚪", "style or taste with no behavioural effect"},
}

// The range and default of Review.MaxRounds.
const (
	MinRounds      = 5
	MaxRoundsLimit = 30
	DefaultRounds  = 10
)

// LevelOff is the ApprovalLevel or RequestChangesLevel that turns it off.
const LevelOff = "off"

// ApprovalOff is the ApprovalLevel that disables approvals.
const ApprovalOff = LevelOff

// SeverityRank returns 1 (P1) .. 5 (P5), or 0 for an unknown level.
func SeverityRank(id string) int {
	for i, s := range Severities {
		if s.ID == id {
			return i + 1
		}
	}
	return 0
}

// SeverityInfo returns the level's metadata (P3's for an unknown ID).
func SeverityInfo(id string) Severity {
	if r := SeverityRank(id); r > 0 {
		return Severities[r-1]
	}
	return Severities[2]
}

// DefaultGuidelineFiles are the project docs read by default. A pattern with
// a slash is anchored at the repository root; CLAUDE.md and AGENTS.md match
// in any directory and apply to the files under it.
func DefaultGuidelineFiles() []string {
	return []string{
		"CLAUDE.md", "AGENTS.md",
		"/CONTRIBUTING.md", "/.github/CONTRIBUTING.md", "/docs/CONTRIBUTING.md",
		"/.github/copilot-instructions.md",
		"/STYLEGUIDE.md", "/STYLE_GUIDE.md", "/CODING_STANDARDS.md", "/CONVENTIONS.md",
		"/docs/STYLEGUIDE.md", "/docs/CODING_STANDARDS.md", "/docs/CONVENTIONS.md",
	}
}

// legacySeverity maps the old four-level min_severity values.
var legacySeverity = map[string]string{"critical": "P1", "major": "P2", "minor": "P4", "nit": "P5"}

func Defaults() Settings {
	return Settings{
		GitHub: GitHub{APIBaseURL: "https://api.github.com", NewReposEnabled: true},
		LLM: LLM{
			Provider:         "lmstudio",
			BaseURL:          DefaultBaseURL("lmstudio"),
			Temperature:      0.2,
			ContextTokens:    FallbackContextTokens,
			MaxOutputTokens:  FallbackOutputTokens,
			StructuredOutput: true,
			TimeoutSeconds:   600,
		},
		Review: Review{
			Enabled:    true,
			SkipDrafts: true,
			SkipLabels: []string{"no-bot-review", "skip-review"},
			IgnorePatterns: []string{
				"*.lock", "go.sum", "package-lock.json", "pnpm-lock.yaml", "yarn.lock", "Cargo.lock",
				"vendor/", "node_modules/", "dist/", "*.min.js", "*.min.css", "*.map", "*.pb.go", "*.snap", "*.svg",
			},
			DebounceSeconds:     60,
			MaxComments:         20,
			CommentLevel:        "P5",
			ApprovalLevel:       "P3",
			RequestChangesLevel: "P3",
			MaxRounds:           DefaultRounds,
			MaxFiles:            150,
			GuidelineFiles:      DefaultGuidelineFiles(),
		},
		Replies: Replies{Enabled: true, Mentions: true, MaxPerThread: 5},
		Poll:    Poll{IntervalSeconds: 60, RepoRefreshMinutes: 15, WebhookIntervalSeconds: 900},
		Limits:  Limits{OrgSlotPercent: DefaultOrgSlotPercent},
		// Models is left empty: Normalize makes LLM above the only model, and
		// settings saved before there were several keep theirs the same way.
	}
}

// Normalize trims values and clamps numbers into sane ranges.
func (s *Settings) Normalize() {
	s.GitHub.PrivateKey = strings.TrimSpace(s.GitHub.PrivateKey)
	s.GitHub.ClientID = strings.TrimSpace(s.GitHub.ClientID)
	s.GitHub.ClientSecret = strings.TrimSpace(s.GitHub.ClientSecret)
	s.GitHub.WebhookSecret = strings.TrimSpace(s.GitHub.WebhookSecret)
	s.GitHub.PublicURL = strings.TrimRight(strings.TrimSpace(s.GitHub.PublicURL), "/")
	if s.GitHub.PublicURL != "" && !strings.Contains(s.GitHub.PublicURL, "://") {
		s.GitHub.PublicURL = "https://" + s.GitHub.PublicURL // a bare host, as typed
	}
	s.GitHub.APIBaseURL = strings.TrimRight(strings.TrimSpace(s.GitHub.APIBaseURL), "/")
	if s.GitHub.APIBaseURL == "" {
		s.GitHub.APIBaseURL = "https://api.github.com"
	}
	if len(s.Models.Configs) == 0 {
		// Saved before there could be several models: the one becomes the set.
		s.Models.Configs = []LLM{s.LLM}
	}
	s.Models.Normalize()
	s.LLM = s.Models.Primary()
	if s.Limits.OrgSlotPercent == 0 {
		s.Limits.OrgSlotPercent = DefaultOrgSlotPercent // saved before the setting existed
	}
	s.Limits.OrgSlotPercent = clamp(s.Limits.OrgSlotPercent, 1, 100)
	s.Review.SkillDir = strings.TrimSpace(s.Review.SkillDir)
	s.Review.DebounceSeconds = clamp(s.Review.DebounceSeconds, 0, 3600)
	s.Review.MaxComments = clamp(s.Review.MaxComments, 1, 100)
	s.Review.MaxFiles = clamp(s.Review.MaxFiles, 1, 3000)
	s.Review.MaxRounds = clamp(s.Review.MaxRounds, MinRounds, MaxRoundsLimit)
	if p, ok := legacySeverity[s.Review.LegacyMinSeverity]; ok {
		s.Review.CommentLevel = p // old "minor" covered quality issues too, hence P4
	}
	s.Review.LegacyMinSeverity = ""
	s.Review.CommentLevel = strings.ToUpper(strings.TrimSpace(s.Review.CommentLevel))
	if SeverityRank(s.Review.CommentLevel) == 0 {
		s.Review.CommentLevel = "P5"
	}
	s.Review.ApprovalLevel = strings.ToUpper(strings.TrimSpace(s.Review.ApprovalLevel))
	switch r := SeverityRank(s.Review.ApprovalLevel); {
	case r == 0:
		s.Review.ApprovalLevel = ApprovalOff
	case r > SeverityRank(s.Review.CommentLevel):
		// Findings below the comment level are never posted, so they can't
		// be "unresolved"; an approval level broader than that means the same.
		s.Review.ApprovalLevel = s.Review.CommentLevel
	}
	s.Review.RequestChangesLevel = strings.ToUpper(strings.TrimSpace(s.Review.RequestChangesLevel))
	switch r := SeverityRank(s.Review.RequestChangesLevel); {
	case r == 0:
		s.Review.RequestChangesLevel = LevelOff
	case s.Review.ApprovalOn() && r > SeverityRank(s.Review.ApprovalLevel):
		// A finding that requests changes must hold back approval too, or one
		// review could ask for changes and approve at once.
		s.Review.RequestChangesLevel = s.Review.ApprovalLevel
	case r > SeverityRank(s.Review.CommentLevel):
		s.Review.RequestChangesLevel = s.Review.CommentLevel // unposted findings can't be acted on
	}
	s.Replies.MaxPerThread = clamp(s.Replies.MaxPerThread, 1, 50)
	s.Poll.IntervalSeconds = clamp(s.Poll.IntervalSeconds, 15, 3600)
	s.Poll.RepoRefreshMinutes = clamp(s.Poll.RepoRefreshMinutes, 1, 1440)
	if s.Poll.WebhookIntervalSeconds == 0 {
		s.Poll.WebhookIntervalSeconds = 900
	}
	s.Poll.WebhookIntervalSeconds = clamp(s.Poll.WebhookIntervalSeconds, s.Poll.IntervalSeconds, 86400)
}

// Issues lists what still has to be configured: GitHubIssues, then the
// instance models' issues.
func (s Settings) Issues() []string {
	return append(s.GitHubIssues(), s.Models.Issues()...)
}

// GitHubIssues lists what has to be configured before the bot can watch
// GitHub at all. The instance's models aren't among them: organizations with
// their own models don't need them (see Models.Issues for those).
func (s Settings) GitHubIssues() []string {
	var out []string
	if s.GitHub.AppID == 0 {
		out = append(out, "GitHub App ID is not set")
	}
	if s.GitHub.PrivateKey == "" {
		out = append(out, "GitHub App private key is not set")
	}
	return out
}

func Load(st *store.Store) (Settings, error) {
	v := Defaults()
	if _, err := st.GetJSON(storeKey, &v); err != nil {
		return Defaults(), err
	}
	v.Normalize()
	return v, nil
}

// CheckPublicURL reports what's wrong with a Public URL, if anything: it
// must be empty (use the address each request came in on) or a full http://
// or https:// address with no path, since the bot's pages, links and cookies
// all assume it's served at the root of its host. Sign-in's callback and the
// webhook URL are built from it, and secure cookies depend on its scheme.
func CheckPublicURL(raw string) error {
	if raw == "" {
		return nil
	}
	u, err := url.Parse(raw)
	switch {
	case err != nil:
		return fmt.Errorf("the Public URL %q isn't a valid address: %w", raw, err)
	case u.Scheme != "http" && u.Scheme != "https":
		return fmt.Errorf("the Public URL %q must start with https:// (or http://)", raw)
	case u.Host == "":
		return fmt.Errorf("the Public URL %q has no host name", raw)
	case u.RawQuery != "" || u.Fragment != "" || u.User != nil:
		return fmt.Errorf("the Public URL %q can't have a query, fragment or user name", raw)
	case u.Path != "" && u.Path != "/":
		return fmt.Errorf("the Public URL %q has a path; the bot must be served at the root of its host (give it a host name of its own, such as https://prbot.example.com)", raw)
	}
	return nil
}

func Save(st *store.Store, v Settings) error {
	v.Normalize()
	return st.SetJSON(storeKey, v)
}

func clamp(v, lo, hi int) int {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

func clampF(v, lo, hi float64) float64 {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}
