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

	"github.com/OMG-Software/omg-review-bot/internal/netguard"
	"github.com/OMG-Software/omg-review-bot/internal/textutil"
)

type Message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
	// ToolCalls are the calls an assistant message made (see Response).
	ToolCalls []ToolCall `json:"-"`
	// ToolCallID and ToolName say which call a "tool" message answers.
	ToolCallID string `json:"-"`
	ToolName   string `json:"-"`
	// Reasoning is the reasoning an assistant message with tool calls came
	// with, sent back on the next request (some servers require it).
	Reasoning string `json:"-"`
}

// Tool is a function the model may call. Parameters is its JSON schema.
type Tool struct {
	Name        string
	Description string
	Parameters  map[string]any
}

// ToolCall is one call the model asked for. Arguments is a JSON object.
type ToolCall struct {
	ID        string
	Name      string
	Arguments string
}

type Request struct {
	Messages    []Message
	Schema      map[string]any // when set (and structured output is on), constrain output to this JSON schema
	SchemaName  string
	Tools       []Tool // offered to the model; its calls come back in Response.ToolCalls
	Temperature float64
	MaxTokens   int
}

type Response struct {
	Content          string
	ToolCalls        []ToolCall
	Reasoning        string // reasoning the server returned apart from Content
	PromptTokens     int
	CompletionTokens int
	Truncated        bool // the model hit its output token limit
	Duration         time.Duration
}

// ErrToolsUnsupported means the server or model refused a request because it
// offered tools. The caller can ask again without them.
var ErrToolsUnsupported = errors.New("the model or server does not support tool calls")

// toolsRefused wraps err as ErrToolsUnsupported when a request that offered
// tools was rejected as a bad request that mentions them.
func toolsRefused(req Request, err error) error {
	var he *httpError
	// Rate limits, sign-in and timeouts are ordinary failures, whatever the
	// message says (a model may be named "...tool...").
	if len(req.Tools) > 0 && errors.As(err, &he) && he.status >= 400 && he.status < 500 &&
		he.status != http.StatusUnauthorized && he.status != http.StatusForbidden &&
		he.status != http.StatusRequestTimeout && he.status != http.StatusTooManyRequests &&
		mentionsTools(he.body) {
		return fmt.Errorf("%w: %v", ErrToolsUnsupported, err)
	}
	return err
}

// toolWords are how servers name tools when they refuse them ("does not
// support tools", "function calling is not supported", "functions are not
// enabled for this model").
var toolWords = regexp.MustCompile(`(?i)\btools?\b|tool_calls?|tool_choice|function[ _-]?call|\bfunctions\b`)

func mentionsTools(body string) bool { return toolWords.MatchString(body) }

type httpError struct {
	method, url string
	status      int
	body        string
}

func (e *httpError) Error() string {
	return fmt.Sprintf("%s %s: HTTP %d: %s", e.method, e.url, e.status, e.body)
}

func toolDefs(tools []Tool) []map[string]any {
	out := make([]map[string]any, 0, len(tools))
	for _, t := range tools {
		out = append(out, map[string]any{"type": "function", "function": map[string]any{
			"name": t.Name, "description": t.Description, "parameters": t.Parameters,
		}})
	}
	return out
}

// rawArgs reads a call's arguments, which servers send either as a JSON
// object or as a string holding one.
// None at all (missing, null or "") is an empty object, as for a tool
// without parameters.
func rawArgs(r json.RawMessage) string {
	s := strings.TrimSpace(string(r))
	var inner string
	if json.Unmarshal(r, &inner) == nil {
		s = strings.TrimSpace(inner)
	}
	if s == "" || s == "null" {
		return "{}"
	}
	return s
}

// argsObject is a call's arguments as a JSON value to send back. Arguments
// that aren't a JSON object are sent as an empty one.
func argsObject(s string) json.RawMessage {
	var m map[string]any
	if json.Unmarshal([]byte(s), &m) != nil || m == nil {
		return json.RawMessage("{}")
	}
	return json.RawMessage(s)
}

type Config struct {
	Provider      string
	BaseURL       string
	APIKey        string
	Model         string
	ContextTokens int
	Structured    bool
	Timeout       time.Duration
	// PublicOnly limits the client to https and public addresses, checked
	// when connecting: for servers an organization chose (its own models),
	// which mustn't reach the bot's own machine or network. No proxy is used,
	// since the check has to see the server's own address.
	PublicOnly bool
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
	bu, err := url.ParseRequestURI(base)
	if err != nil || bu.Host == "" || (bu.Scheme != "http" && bu.Scheme != "https") {
		return nil, fmt.Errorf("LLM base URL %q is not a valid http(s) URL", base)
	}
	if cfg.PublicOnly {
		if err := netguard.CheckURL(base); err != nil {
			return nil, err
		}
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = 10 * time.Minute
	}
	// Prompts carry the PR's code and the API key, so they go only to the
	// configured server: a redirect elsewhere is refused (Go would otherwise
	// resend the body on a 307/308).
	hc := &http.Client{Timeout: cfg.Timeout, CheckRedirect: func(req *http.Request, via []*http.Request) error {
		if len(via) >= 5 {
			return errors.New("stopped after 5 redirects")
		}
		if !strings.EqualFold(req.URL.Scheme, bu.Scheme) || !strings.EqualFold(req.URL.Host, bu.Host) {
			return fmt.Errorf("refusing redirect to %s: not the configured LLM server", req.URL.Host)
		}
		return nil
	}}
	if cfg.PublicOnly {
		hc.Transport = &http.Transport{
			DialContext:           netguard.Dialer(),
			ForceAttemptHTTP2:     true,
			MaxIdleConns:          10,
			IdleConnTimeout:       90 * time.Second,
			TLSHandshakeTimeout:   10 * time.Second,
			ExpectContinueTimeout: time.Second,
		}
	}
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
		return &httpError{method: method, url: u, status: resp.StatusCode, body: textutil.Truncate(strings.TrimSpace(string(data)), 500)}
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
		"messages":    openAIMessages(req.Messages),
		"stream":      false,
		"temperature": req.Temperature,
	}
	if req.MaxTokens > 0 {
		body["max_tokens"] = req.MaxTokens
	}
	if len(req.Tools) > 0 {
		body["tools"] = toolDefs(req.Tools)
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
				ToolCalls        []struct {
					ID       string `json:"id"`
					Function struct {
						Name      string          `json:"name"`
						Arguments json.RawMessage `json:"arguments"`
					} `json:"function"`
				} `json:"tool_calls"`
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
		return nil, toolsRefused(req, err)
	}
	if len(out.Choices) == 0 {
		return nil, errors.New("model returned no choices")
	}
	c := out.Choices[0]
	var calls []ToolCall
	for i, tc := range c.Message.ToolCalls {
		id := tc.ID
		if id == "" {
			id = fmt.Sprintf("call_%d", i)
		}
		calls = append(calls, ToolCall{ID: id, Name: tc.Function.Name, Arguments: rawArgs(tc.Function.Arguments)})
	}
	if len(calls) == 0 {
		if err := reasoningExhausted(c.Message.Content, c.Message.ReasoningContent, c.FinishReason == "length", req.MaxTokens); err != nil {
			return nil, err
		}
	}
	return &Response{
		Content:          c.Message.Content,
		ToolCalls:        calls,
		Reasoning:        c.Message.ReasoningContent,
		PromptTokens:     out.Usage.PromptTokens,
		CompletionTokens: out.Usage.CompletionTokens,
		Truncated:        c.FinishReason == "length",
		Duration:         time.Since(start),
	}, nil
}

// openAIMessages encodes messages for /v1/chat/completions, with tool calls
// and their results in OpenAI's shape.
func openAIMessages(msgs []Message) []map[string]any {
	out := make([]map[string]any, 0, len(msgs))
	for _, m := range msgs {
		e := map[string]any{"role": m.Role, "content": m.Content}
		if len(m.ToolCalls) > 0 {
			var tcs []map[string]any
			for _, tc := range m.ToolCalls {
				tcs = append(tcs, map[string]any{"id": tc.ID, "type": "function",
					"function": map[string]any{"name": tc.Name, "arguments": string(argsObject(tc.Arguments))}})
			}
			e["tool_calls"] = tcs
			if m.Reasoning != "" {
				e["reasoning_content"] = m.Reasoning
			}
		}
		if m.Role == "tool" {
			e["tool_call_id"] = m.ToolCallID
		}
		out = append(out, e)
	}
	return out
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
		"messages": ollamaMessages(req.Messages),
		"stream":   false,
		"options":  opts,
	}
	if req.Schema != nil && o.cfg.Structured {
		body["format"] = req.Schema
	}
	if len(req.Tools) > 0 {
		body["tools"] = toolDefs(req.Tools)
	}
	var out struct {
		Message struct {
			Content   string `json:"content"`
			Thinking  string `json:"thinking"`
			ToolCalls []struct {
				ID       string `json:"id"`
				Function struct {
					Name      string          `json:"name"`
					Arguments json.RawMessage `json:"arguments"`
				} `json:"function"`
			} `json:"tool_calls"`
		} `json:"message"`
		DoneReason      string `json:"done_reason"`
		PromptEvalCount int    `json:"prompt_eval_count"`
		EvalCount       int    `json:"eval_count"`
	}
	start := time.Now()
	if err := doJSON(ctx, o.hc, http.MethodPost, o.base+"/api/chat", o.cfg.APIKey, body, &out); err != nil {
		return nil, toolsRefused(req, err)
	}
	var calls []ToolCall
	for i, tc := range out.Message.ToolCalls {
		id := tc.ID
		if id == "" {
			id = fmt.Sprintf("call_%d", i)
		}
		calls = append(calls, ToolCall{ID: id, Name: tc.Function.Name, Arguments: rawArgs(tc.Function.Arguments)})
	}
	if len(calls) == 0 {
		if err := reasoningExhausted(out.Message.Content, out.Message.Thinking, out.DoneReason == "length", req.MaxTokens); err != nil {
			return nil, err
		}
	}
	return &Response{
		Content:          out.Message.Content,
		Reasoning:        out.Message.Thinking,
		ToolCalls:        calls,
		PromptTokens:     out.PromptEvalCount,
		CompletionTokens: out.EvalCount,
		Truncated:        out.DoneReason == "length",
		Duration:         time.Since(start),
	}, nil
}

// ollamaMessages encodes messages for /api/chat, where a call's arguments
// are an object and a tool result names its tool.
func ollamaMessages(msgs []Message) []map[string]any {
	out := make([]map[string]any, 0, len(msgs))
	for _, m := range msgs {
		e := map[string]any{"role": m.Role, "content": m.Content}
		if len(m.ToolCalls) > 0 {
			var tcs []map[string]any
			for _, tc := range m.ToolCalls {
				tcs = append(tcs, map[string]any{"function": map[string]any{"name": tc.Name, "arguments": argsObject(tc.Arguments)}})
			}
			e["tool_calls"] = tcs
			if m.Reasoning != "" {
				e["thinking"] = m.Reasoning
			}
		}
		if m.Role == "tool" && m.ToolName != "" {
			e["tool_name"] = m.ToolName
		}
		out = append(out, e)
	}
	return out
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

// ---------- model limits ----------

// Limits are a model's token limits as its server reports them; 0 means
// the server didn't say.
type Limits struct {
	Context int // the context window: prompt and output together
	Output  int // the most output tokens one answer can have
}

// LimitReader is a Client that can ask its server for the model's limits.
type LimitReader interface {
	Limits(ctx context.Context) (Limits, error)
}

// errNoLimits: the server answered but didn't report the model's limits.
var errNoLimits = errors.New("the server doesn't report the model's token limits")

// Limits asks the server for the model's limits. LM Studio reports them on
// its own REST API; other OpenAI-compatible servers on their model list, by
// whichever name they use (OpenRouter, vLLM, LiteLLM, ...), and llama.cpp's
// server on /props.
func (o *openAI) Limits(ctx context.Context) (Limits, error) {
	if o.cfg.Model == "" {
		return Limits{}, errors.New("no model selected")
	}
	root := strings.TrimSuffix(o.base, "/v1")
	var firstErr error
	note := func(err error) {
		if firstErr == nil {
			firstErr = err
		}
	}
	if o.cfg.Provider == "lmstudio" {
		// The v1 REST API (LM Studio 0.4) lists each loaded instance's
		// window; v0 gives the model's own entry.
		var v1 struct {
			Models []map[string]any `json:"models"`
		}
		if err := doJSON(ctx, o.hc, http.MethodGet, root+"/api/v1/models", o.cfg.APIKey, nil, &v1); err == nil {
			for _, m := range v1.Models {
				if key, _ := m["key"].(string); key == o.cfg.Model {
					if l := lmStudioLimits(m); l.Context > 0 {
						return l, nil
					}
				}
			}
		}
		var m map[string]any
		err := doJSON(ctx, o.hc, http.MethodGet, root+"/api/v0/models/"+strings.ReplaceAll(url.PathEscape(o.cfg.Model), "%2F", "/"), o.cfg.APIKey, nil, &m)
		if err == nil {
			if l := lmStudioLimits(m); l.Context > 0 {
				return l, nil
			}
		}
		note(err)
	}
	var list struct {
		Data []map[string]any `json:"data"`
	}
	err := doJSON(ctx, o.hc, http.MethodGet, o.base+"/models", o.cfg.APIKey, nil, &list)
	listed := false
	if err == nil {
		for _, m := range list.Data {
			if id, _ := m["id"].(string); id == o.cfg.Model {
				listed = true
				if l := limitsFrom(m); l.Context > 0 || l.Output > 0 {
					return l, nil
				}
			}
		}
	}
	note(err)
	// A model list without this model most likely means a misspelt name;
	// saying so beats the defaults' "not reported". (LM Studio lists only
	// loaded models there, and its own API was asked about this one above.)
	if err == nil && !listed && o.cfg.Provider != "lmstudio" {
		return Limits{}, fmt.Errorf("the server's model list has no model named %q; check the name", o.cfg.Model)
	}
	// llama.cpp's /props takes no model: it describes the model the server
	// has loaded. That's this one only when it's the one model listed;
	// otherwise another model's window would be taken for this one's.
	if !listed || len(list.Data) != 1 {
		if firstErr != nil {
			return Limits{}, firstErr
		}
		return Limits{}, errNoLimits
	}
	var props struct {
		Settings struct {
			NCtx int `json:"n_ctx"`
		} `json:"default_generation_settings"`
	}
	if err := doJSON(ctx, o.hc, http.MethodGet, root+"/props", o.cfg.APIKey, nil, &props); err == nil && props.Settings.NCtx > 0 {
		return Limits{Context: props.Settings.NCtx}, nil
	}
	if firstErr != nil {
		return Limits{}, firstErr
	}
	return Limits{}, errNoLimits
}

// limitsFrom reads the limits from one model's entry in a server's model
// list, under the names servers use for them.
func limitsFrom(m map[string]any) Limits {
	num := func(m map[string]any, keys ...string) int {
		for _, k := range keys {
			if f, ok := m[k].(float64); ok && f > 0 {
				return int(f)
			}
		}
		return 0
	}
	l := Limits{
		Context: num(m, "max_context_length", "context_length", "max_model_len", "context_window"),
		Output:  num(m, "max_completion_tokens", "max_output_tokens"),
	}
	if tp, ok := m["top_provider"].(map[string]any); ok { // OpenRouter
		l.Context = max(l.Context, num(tp, "context_length"))
		if l.Output == 0 {
			l.Output = num(tp, "max_completion_tokens")
		}
	}
	return l
}

// lmStudioLimits reads an LM Studio model entry. A loaded model answers in
// the window it was loaded with, which can be smaller than the model's
// maximum, so the context is capped at the smallest loaded window (v1's
// loaded_instances, v0's loaded_context_length).
func lmStudioLimits(m map[string]any) Limits {
	l := limitsFrom(m)
	loaded := func(n float64) {
		if n > 0 && (l.Context == 0 || int(n) < l.Context) {
			l.Context = int(n)
		}
	}
	if n, ok := m["loaded_context_length"].(float64); ok {
		loaded(n)
	}
	insts, _ := m["loaded_instances"].([]any)
	for _, in := range insts {
		inst, _ := in.(map[string]any)
		cfg, _ := inst["config"].(map[string]any)
		if n, ok := cfg["context_length"].(float64); ok {
			loaded(n)
		}
	}
	return l
}

// Limits asks Ollama for the model's context window, which /api/show gives
// in its model_info as "<architecture>.context_length". Ollama has no
// separate output cap: an answer can use what the prompt leaves.
func (o *ollama) Limits(ctx context.Context) (Limits, error) {
	if o.cfg.Model == "" {
		return Limits{}, errors.New("no model selected")
	}
	var out struct {
		ModelInfo map[string]any `json:"model_info"`
	}
	if err := doJSON(ctx, o.hc, http.MethodPost, o.base+"/api/show", o.cfg.APIKey, map[string]any{"model": o.cfg.Model}, &out); err != nil {
		return Limits{}, err
	}
	return ollamaLimits(out.ModelInfo)
}

// ollamaLimits reads the context window from /api/show's model_info. A
// model with more than one part (a vision model's projector, say) has more
// than one "*.context_length", so the main architecture's is read first,
// and failing that the first by name, never whichever a map gives first.
func ollamaLimits(info map[string]any) (Limits, error) {
	num := func(k string) int {
		if f, ok := info[k].(float64); ok && f > 0 {
			return int(f)
		}
		return 0
	}
	if arch, ok := info["general.architecture"].(string); ok && arch != "" {
		if n := num(arch + ".context_length"); n > 0 {
			return Limits{Context: n}, nil
		}
	}
	var keys []string
	for k := range info {
		if strings.HasSuffix(k, ".context_length") && num(k) > 0 {
			keys = append(keys, k)
		}
	}
	if len(keys) == 0 {
		return Limits{}, errNoLimits
	}
	sort.Strings(keys)
	return Limits{Context: num(keys[0])}, nil
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
// reasoning blocks, markdown fences and chatter around it) into v. An object
// that is JSON but doesn't fit v is skipped whole: the objects nested in it
// aren't tried, or {"comments": {...one finding...}} would decode as the
// finding, read as an answer with no comments.
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
		dec := json.NewDecoder(strings.NewReader(s[i:]))
		if err := dec.Decode(&raw); err != nil {
			lastErr = err
			continue
		}
		if err := json.Unmarshal(raw, v); err != nil {
			lastErr = err
			i += int(dec.InputOffset()) - 1
			continue
		}
		return nil
	}
	return lastErr
}
