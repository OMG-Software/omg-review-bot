// Package settings defines the runtime configuration edited from the web UI.
package settings

import (
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
}

type LLM struct {
	Provider         string  `json:"provider"` // lmstudio | ollama | ollama-cloud | openai
	BaseURL          string  `json:"base_url"`
	APIKey           string  `json:"api_key"`
	Model            string  `json:"model"`
	Temperature      float64 `json:"temperature"`
	ContextTokens    int     `json:"context_tokens"`
	MaxOutputTokens  int     `json:"max_output_tokens"`
	StructuredOutput bool    `json:"structured_output"`
	TimeoutSeconds   int     `json:"timeout_seconds"`
}

type Review struct {
	Enabled         bool     `json:"enabled"`
	SkillDir        string   `json:"skill_dir"` // empty = <data>/skill
	SkipDrafts      bool     `json:"skip_drafts"`
	SkipLabels      []string `json:"skip_labels"`
	IgnorePatterns  []string `json:"ignore_patterns"`
	DebounceSeconds int      `json:"debounce_seconds"`
	MaxComments     int      `json:"max_comments"`
	MinSeverity     string   `json:"min_severity"`
	MaxFiles        int      `json:"max_files"`
	ReviewExisting  bool     `json:"review_existing"`
}

type Replies struct {
	Enabled      bool `json:"enabled"`
	Mentions     bool `json:"mentions"`
	MaxPerThread int  `json:"max_per_thread"`
}

type Poll struct {
	IntervalSeconds    int `json:"interval_seconds"`
	RepoRefreshMinutes int `json:"repo_refresh_minutes"`
}

type Settings struct {
	GitHub  GitHub  `json:"github"`
	LLM     LLM     `json:"llm"`
	Review  Review  `json:"review"`
	Replies Replies `json:"replies"`
	Poll    Poll    `json:"poll"`
}

// Provider describes an LLM backend choice shown in the UI.
type Provider struct {
	ID, Label, Base string
}

var Providers = []Provider{
	{"lmstudio", "LM Studio", "http://localhost:1234/v1"},
	{"ollama", "Ollama (local)", "http://localhost:11434"},
	{"ollama-cloud", "Ollama Cloud", "https://ollama.com"},
	{"openai", "Other OpenAI-compatible", ""},
}

func DefaultBaseURL(provider string) string {
	for _, p := range Providers {
		if p.ID == provider {
			return p.Base
		}
	}
	return ""
}

var Severities = []string{"nit", "minor", "major", "critical"}

func Defaults() Settings {
	return Settings{
		GitHub: GitHub{APIBaseURL: "https://api.github.com", NewReposEnabled: true},
		LLM: LLM{
			Provider:         "lmstudio",
			BaseURL:          DefaultBaseURL("lmstudio"),
			Temperature:      0.2,
			ContextTokens:    32768,
			MaxOutputTokens:  4096,
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
			DebounceSeconds: 60,
			MaxComments:     20,
			MinSeverity:     "nit",
			MaxFiles:        150,
		},
		Replies: Replies{Enabled: true, Mentions: true, MaxPerThread: 5},
		Poll:    Poll{IntervalSeconds: 60, RepoRefreshMinutes: 15},
	}
}

// Normalize trims values and clamps numbers into sane ranges.
func (s *Settings) Normalize() {
	s.GitHub.PrivateKey = strings.TrimSpace(s.GitHub.PrivateKey)
	s.GitHub.APIBaseURL = strings.TrimRight(strings.TrimSpace(s.GitHub.APIBaseURL), "/")
	if s.GitHub.APIBaseURL == "" {
		s.GitHub.APIBaseURL = "https://api.github.com"
	}
	s.LLM.Provider = strings.TrimSpace(s.LLM.Provider)
	if DefaultBaseURL(s.LLM.Provider) == "" && s.LLM.Provider != "openai" {
		s.LLM.Provider = "lmstudio"
	}
	s.LLM.BaseURL = strings.TrimRight(strings.TrimSpace(s.LLM.BaseURL), "/")
	if s.LLM.BaseURL == "" {
		s.LLM.BaseURL = DefaultBaseURL(s.LLM.Provider)
	}
	s.LLM.APIKey = strings.TrimSpace(s.LLM.APIKey)
	s.LLM.Model = strings.TrimSpace(s.LLM.Model)
	s.LLM.Temperature = clampF(s.LLM.Temperature, 0, 2)
	s.LLM.ContextTokens = clamp(s.LLM.ContextTokens, 4096, 2_000_000)
	s.LLM.MaxOutputTokens = clamp(s.LLM.MaxOutputTokens, 512, s.LLM.ContextTokens/2)
	s.LLM.TimeoutSeconds = clamp(s.LLM.TimeoutSeconds, 30, 3600)
	s.Review.SkillDir = strings.TrimSpace(s.Review.SkillDir)
	s.Review.DebounceSeconds = clamp(s.Review.DebounceSeconds, 0, 3600)
	s.Review.MaxComments = clamp(s.Review.MaxComments, 1, 100)
	s.Review.MaxFiles = clamp(s.Review.MaxFiles, 1, 3000)
	if severityIndex(s.Review.MinSeverity) < 0 {
		s.Review.MinSeverity = "nit"
	}
	s.Replies.MaxPerThread = clamp(s.Replies.MaxPerThread, 1, 50)
	s.Poll.IntervalSeconds = clamp(s.Poll.IntervalSeconds, 15, 3600)
	s.Poll.RepoRefreshMinutes = clamp(s.Poll.RepoRefreshMinutes, 1, 1440)
}

// Issues lists what still has to be configured before the bot can run.
func (s Settings) Issues() []string {
	var out []string
	if s.GitHub.AppID == 0 {
		out = append(out, "GitHub App ID is not set")
	}
	if s.GitHub.PrivateKey == "" {
		out = append(out, "GitHub App private key is not set")
	}
	if s.LLM.BaseURL == "" {
		out = append(out, "LLM base URL is not set")
	}
	if s.LLM.Model == "" {
		out = append(out, "LLM model is not set")
	}
	if s.LLM.Provider == "ollama-cloud" && s.LLM.APIKey == "" {
		out = append(out, "Ollama Cloud needs an API key")
	}
	return out
}

func severityIndex(s string) int {
	for i, v := range Severities {
		if v == s {
			return i
		}
	}
	return -1
}

func Load(st *store.Store) (Settings, error) {
	v := Defaults()
	if _, err := st.GetJSON(storeKey, &v); err != nil {
		return Defaults(), err
	}
	v.Normalize()
	return v, nil
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
