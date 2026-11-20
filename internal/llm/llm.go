// Package llm talks to local or hosted chat models: LM Studio (and any
// OpenAI-compatible server) via /v1/chat/completions, and Ollama (local or
// cloud) via its native /api/chat so the context window (num_ctx) can be set.
package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/OMG-Software/omg-review-bot/internal/textutil"
)

type Message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type Request struct {
	Messages    []Message
	Schema      map[string]any // when set (and structured output is on), constrain output to this JSON schema
	SchemaName  string
	Temperature float64
	MaxTokens   int
}

type Response struct {
	Content          string
	PromptTokens     int
	CompletionTokens int
	Truncated        bool // the model hit its output token limit
	Duration         time.Duration
}

type Config struct {
	Provider      string
	BaseURL       string
	APIKey        string
	Model         string
	ContextTokens int
	Structured    bool
	Timeout       time.Duration
}

type Client interface {
	Chat(ctx context.Context, req Request) (*Response, error)
	Models(ctx context.Context) ([]string, error)
}

func New(cfg Config) (Client, error) {
	base := strings.TrimRight(strings.TrimSpace(cfg.BaseURL), "/")
	if base == "" {
		return nil, errors.New("LLM base URL is not set")
	}
	if u, err := url.ParseRequestURI(base); err != nil || u.Host == "" {
		return nil, fmt.Errorf("LLM base URL %q is not a valid URL", base)
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = 10 * time.Minute
	}
	hc := &http.Client{Timeout: cfg.Timeout}
	switch cfg.Provider {
	case "ollama", "ollama-cloud":
		base = strings.TrimSuffix(strings.TrimSuffix(base, "/v1"), "/api")
		return &ollama{cfg: cfg, base: base, hc: hc}, nil
	default:
		return &openAI{cfg: cfg, base: base, hc: hc}, nil
	}
}

func doJSON(ctx context.Context, hc *http.Client, method, u, apiKey string, in, out any) error {
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, u, body)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+apiKey)
	}
	resp, err := hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode >= 300 {
		return fmt.Errorf("%s %s: HTTP %d: %s", method, u, resp.StatusCode, textutil.Truncate(strings.TrimSpace(string(data)), 500))
	}
	if out != nil {
		if err := json.Unmarshal(data, out); err != nil {
			return fmt.Errorf("decode response from %s: %w", u, err)
		}
	}
	return nil
}

// ---------- OpenAI-compatible (LM Studio, vLLM, llama.cpp server, ...) ----------

type openAI struct {
	cfg  Config
	base string
	hc   *http.Client
}

func (o *openAI) Chat(ctx context.Context, req Request) (*Response, error) {
	if o.cfg.Model == "" {
		return nil, errors.New("no model selected")
	}
	body := map[string]any{
		"model":       o.cfg.Model,
		"messages":    req.Messages,
		"stream":      false,
		"temperature": req.Temperature,
	}
	if req.MaxTokens > 0 {
		body["max_tokens"] = req.MaxTokens
	}
	if req.Schema != nil && o.cfg.Structured {
		name := req.SchemaName
		if name == "" {
			name = "response"
		}
		body["response_format"] = map[string]any{
			"type":        "json_schema",
			"json_schema": map[string]any{"name": name, "strict": true, "schema": req.Schema},
		}
	}
	var out struct {
		Choices []struct {
			Message struct {
				Content          string `json:"content"`
				ReasoningContent string `json:"reasoning_content"`
			} `json:"message"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
		Usage struct {
			PromptTokens     int `json:"prompt_tokens"`
			CompletionTokens int `json:"completion_tokens"`
		} `json:"usage"`
	}
	start := time.Now()
	if err := doJSON(ctx, o.hc, http.MethodPost, o.base+"/chat/completions", o.cfg.APIKey, body, &out); err != nil {
		return nil, err
	}
	if len(out.Choices) == 0 {
		return nil, errors.New("model returned no choices")
	}
	c := out.Choices[0]
	if err := reasoningExhausted(c.Message.Content, c.Message.ReasoningContent, c.FinishReason == "length", req.MaxTokens); err != nil {
		return nil, err
	}
	return &Response{
		Content:          out.Choices[0].Message.Content,
		PromptTokens:     out.Usage.PromptTokens,
		CompletionTokens: out.Usage.CompletionTokens,
		Truncated:        out.Choices[0].FinishReason == "length",
		Duration:         time.Since(start),
	}, nil
}

func (o *openAI) Models(ctx context.Context) ([]string, error) {
	var out struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := doJSON(ctx, o.hc, http.MethodGet, o.base+"/models", o.cfg.APIKey, nil, &out); err != nil {
		return nil, err
	}
	var ids []string
	for _, m := range out.Data {
		ids = append(ids, m.ID)
	}
	sort.Strings(ids)
	return ids, nil
}

// ---------- Ollama native API (local and ollama.com) ----------

type ollama struct {
	cfg  Config
	base string
	hc   *http.Client
}

func (o *ollama) Chat(ctx context.Context, req Request) (*Response, error) {
	if o.cfg.Model == "" {
		return nil, errors.New("no model selected")
	}
	opts := map[string]any{"temperature": req.Temperature}
	if req.MaxTokens > 0 {
		opts["num_predict"] = req.MaxTokens
	}
	// Ollama's default context is small and it silently drops the start of
	// longer prompts, so local models get an explicit window. Cloud models
	// manage their own context.
	if o.cfg.Provider != "ollama-cloud" && o.cfg.ContextTokens > 0 {
		opts["num_ctx"] = o.cfg.ContextTokens
	}
	body := map[string]any{
		"model":    o.cfg.Model,
		"messages": req.Messages,
		"stream":   false,
		"options":  opts,
	}
	if req.Schema != nil && o.cfg.Structured {
		body["format"] = req.Schema
	}
	var out struct {
		Message struct {
			Content  string `json:"content"`
			Thinking string `json:"thinking"`
		} `json:"message"`
		DoneReason      string `json:"done_reason"`
		PromptEvalCount int    `json:"prompt_eval_count"`
		EvalCount       int    `json:"eval_count"`
	}
	start := time.Now()
	if err := doJSON(ctx, o.hc, http.MethodPost, o.base+"/api/chat", o.cfg.APIKey, body, &out); err != nil {
		return nil, err
	}
	if err := reasoningExhausted(out.Message.Content, out.Message.Thinking, out.DoneReason == "length", req.MaxTokens); err != nil {
		return nil, err
	}
	return &Response{
		Content:          out.Message.Content,
		PromptTokens:     out.PromptEvalCount,
		CompletionTokens: out.EvalCount,
		Truncated:        out.DoneReason == "length",
		Duration:         time.Since(start),
	}, nil
}

func (o *ollama) Models(ctx context.Context) ([]string, error) {
	var out struct {
		Models []struct {
			Name string `json:"name"`
		} `json:"models"`
	}
	if err := doJSON(ctx, o.hc, http.MethodGet, o.base+"/api/tags", o.cfg.APIKey, nil, &out); err != nil {
		return nil, err
	}
	var ids []string
	for _, m := range out.Models {
		ids = append(ids, m.Name)
	}
	sort.Strings(ids)
	return ids, nil
}

// ---------- output helpers ----------

// ErrReasoningExhausted means a reasoning model spent its whole output budget
// thinking (servers return that separately) and never produced an answer.
var ErrReasoningExhausted = errors.New("the model used all its output tokens on reasoning and gave no answer")

func reasoningExhausted(content, reasoning string, truncated bool, maxTokens int) error {
	if strings.TrimSpace(StripThinking(content)) == "" && strings.TrimSpace(reasoning) != "" && truncated {
		return fmt.Errorf("%w (limit %d): raise 'Max output tokens' in Settings, or use a non-reasoning model", ErrReasoningExhausted, maxTokens)
	}
	return nil
}

var thinkRE = regexp.MustCompile(`(?s)<think>.*?</think>`)

// StripThinking removes <think>…</think> reasoning that some models (Qwen3,
// DeepSeek-R1, …) put in the content.
func StripThinking(s string) string {
	s = thinkRE.ReplaceAllString(s, "")
	// Some chat templates emit only the closing tag.
	if i := strings.Index(s, "</think>"); i >= 0 {
		s = s[i+len("</think>"):]
	}
	return strings.TrimSpace(s)
}

// DecodeJSON extracts the first JSON object from model output (tolerating
// reasoning blocks, markdown fences and chatter around it) into v.
func DecodeJSON(s string, v any) error {
	s = StripThinking(s)
	var lastErr error = errors.New("no JSON object found")
	tries := 0
	for i := 0; i < len(s) && tries < 25; i++ {
		if s[i] != '{' {
			continue
		}
		tries++
		var raw json.RawMessage
		if err := json.NewDecoder(strings.NewReader(s[i:])).Decode(&raw); err != nil {
			lastErr = err
			continue
		}
		if err := json.Unmarshal(raw, v); err != nil {
			lastErr = err
			continue
		}
		return nil
	}
	return lastErr
}
