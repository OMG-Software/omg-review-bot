package llm

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/anthropics/anthropic-sdk-go/v2"
	"github.com/anthropics/anthropic-sdk-go/v2/option"

	"github.com/OMG-Software/omg-review-bot/internal/textutil"
)

// ---------- Anthropic Messages API (Claude), through its SDK ----------

// anthropicClient calls Claude's Messages API. Requests stream, so a large
// output cap (Claude's is up to 128K tokens) doesn't run into a timeout
// while the answer is written; the answer is read whole at the end.
type anthropicClient struct {
	cfg    Config
	client anthropic.Client
}

// defaultAnthropicMaxTokens is the output cap of a request that names none:
// the Messages API needs one.
const defaultAnthropicMaxTokens = 16384

func newAnthropic(cfg Config, base string, hc *http.Client) *anthropicClient {
	// The SDK adds /v1 itself.
	base = strings.TrimSuffix(base, "/v1")
	opts := []option.RequestOption{
		// Never the host's own credentials or address (ANTHROPIC_API_KEY,
		// profiles, ANTHROPIC_BASE_URL): a model without a key of its own,
		// an organization's above all, mustn't be billed to the instance's.
		option.WithoutEnvironmentDefaults(),
		option.WithBaseURL(base),
		option.WithHTTPClient(hc),
	}
	if cfg.APIKey != "" {
		opts = append(opts, option.WithAPIKey(cfg.APIKey))
	}
	return &anthropicClient{cfg: cfg, client: anthropic.NewClient(opts...)}
}

func (a *anthropicClient) Chat(ctx context.Context, req Request) (*Response, error) {
	if a.cfg.Model == "" {
		return nil, errors.New("no model selected")
	}
	system, msgs := anthropicMessages(req.Messages, a.cfg.Model)
	maxTokens := req.MaxTokens
	if maxTokens <= 0 {
		maxTokens = defaultAnthropicMaxTokens
	}
	// No temperature: current Claude models refuse one (thinking decides).
	params := anthropic.MessageNewParams{
		Model:     anthropic.Model(a.cfg.Model),
		MaxTokens: int64(maxTokens),
		Messages:  msgs,
	}
	if system != "" {
		// The system prompt is the same for every part of a review and every
		// turn of its tool conversation: cached, it's read at a tenth of the
		// price. Prompts too short to cache simply aren't.
		params.System = []anthropic.TextBlockParam{{Text: system, CacheControl: anthropic.NewCacheControlEphemeralParam()}}
	}
	if len(req.Tools) > 0 {
		params.Tools = anthropicTools(req.Tools)
		// A tool conversation resends everything so far on every turn: the
		// cache keeps each turn's prefix for the next.
		params.CacheControl = anthropic.NewCacheControlEphemeralParam()
	}
	if req.Schema != nil && a.cfg.Structured {
		params.OutputConfig = anthropic.OutputConfigParam{Format: anthropic.JSONOutputFormatParam{Schema: req.Schema}}
	}
	start := time.Now()
	stream := a.client.Messages.NewStreaming(ctx, params)
	defer stream.Close()
	var msg anthropic.Message
	stopped := false
	for stream.Next() {
		ev := stream.Current()
		if err := msg.Accumulate(ev); err != nil {
			return nil, fmt.Errorf("read Claude's answer: %w", err)
		}
		stopped = stopped || ev.Type == "message_stop"
	}
	if err := stream.Err(); err != nil {
		return nil, toolsRefused(req, anthropicError(err))
	}
	// A stream can end cleanly part-way through, with no error: what came
	// before message_stop isn't the whole answer.
	if !stopped {
		return nil, errors.New("claude's answer stream ended before it was complete")
	}
	if msg.StopReason == anthropic.StopReasonRefusal {
		why := string(msg.StopDetails.Category)
		if e := strings.TrimSpace(msg.StopDetails.Explanation); e != "" {
			why = strings.TrimSpace(why + ": " + e)
		}
		if why == "" {
			return nil, ErrRefused
		}
		return nil, fmt.Errorf("%w (%s)", ErrRefused, textutil.Truncate(why, 300))
	}
	var text, reasoning strings.Builder
	var calls []ToolCall
	for _, b := range msg.Content {
		switch b.Type {
		case "text":
			text.WriteString(b.Text)
		case "thinking":
			reasoning.WriteString(b.Thinking)
		case "tool_use":
			calls = append(calls, ToolCall{ID: b.ID, Name: b.Name, Arguments: rawArgs(b.Input)})
		}
	}
	truncated := msg.StopReason == anthropic.StopReasonMaxTokens || msg.StopReason == anthropic.StopReasonModelContextWindowExceeded
	if len(calls) == 0 {
		// Claude's thinking text is often left out of the answer (only a
		// signature is returned), so a cut-off answer with none is judged by
		// its thinking blocks being there at all.
		thought := reasoning.String()
		if thought == "" && hasThinking(msg) {
			thought = "(thinking)"
		}
		if err := reasoningExhausted(text.String(), thought, truncated, maxTokens); err != nil {
			return nil, err
		}
	}
	u := msg.Usage
	return &Response{
		Content:          text.String(),
		ToolCalls:        calls,
		Reasoning:        reasoning.String(),
		PromptTokens:     int(u.InputTokens + u.CacheReadInputTokens + u.CacheCreationInputTokens),
		CompletionTokens: int(u.OutputTokens),
		Truncated:        truncated,
		Duration:         time.Since(start),
		// Sent back as it came, so the next turn keeps its signed thinking.
		Native: anthropicTurn{model: a.cfg.Model, param: msg.ToParam()},
	}, nil
}

func hasThinking(msg anthropic.Message) bool {
	for _, b := range msg.Content {
		if b.Type == "thinking" || b.Type == "redacted_thinking" {
			return true
		}
	}
	return false
}

// anthropicTurn is an answer as Claude gave it, sent back as it is on the
// next turn of a tool conversation, with the signed thinking it holds. Only
// the model that wrote it gets it back: after a failover to another model,
// the turn is rebuilt from its text and tool calls.
type anthropicTurn struct {
	model string
	param anthropic.MessageParam
}

// anthropicMessages splits messages into the system prompt and the
// conversation in the Messages API's shape for model: tool results are
// blocks of a user turn, all of one turn's results in the same one.
func anthropicMessages(msgs []Message, model string) (string, []anthropic.MessageParam) {
	var system []string
	var out []anthropic.MessageParam
	var results []anthropic.ContentBlockParamUnion
	flush := func() {
		if len(results) > 0 {
			out = append(out, anthropic.NewUserMessage(results...))
			results = nil
		}
	}
	// The API refuses empty text blocks.
	nonEmpty := func(s string) string {
		if strings.TrimSpace(s) == "" {
			return "(empty)"
		}
		return s
	}
	for _, m := range msgs {
		switch m.Role {
		case "system":
			system = append(system, m.Content)
		case "tool":
			results = append(results, anthropic.NewToolResultBlock(m.ToolCallID, nonEmpty(m.Content), false))
		case "assistant":
			flush()
			if t, ok := m.Native.(anthropicTurn); ok && t.model == model {
				out = append(out, t.param)
				continue
			}
			var blocks []anthropic.ContentBlockParamUnion
			if strings.TrimSpace(m.Content) != "" {
				blocks = append(blocks, anthropic.NewTextBlock(m.Content))
			}
			for _, tc := range m.ToolCalls {
				blocks = append(blocks, anthropic.NewToolUseBlock(tc.ID, argsObject(tc.Arguments), tc.Name))
			}
			if len(blocks) == 0 {
				blocks = append(blocks, anthropic.NewTextBlock(nonEmpty("")))
			}
			out = append(out, anthropic.NewAssistantMessage(blocks...))
		default:
			if len(results) > 0 {
				// Text after tool results goes in the same user turn.
				results = append(results, anthropic.NewTextBlock(nonEmpty(m.Content)))
				flush()
				continue
			}
			out = append(out, anthropic.NewUserMessage(anthropic.NewTextBlock(nonEmpty(m.Content))))
		}
	}
	flush()
	return strings.Join(system, "\n\n"), out
}

func anthropicTools(tools []Tool) []anthropic.ToolUnionParam {
	out := make([]anthropic.ToolUnionParam, 0, len(tools))
	for _, t := range tools {
		schema := anthropic.ToolInputSchemaParam{Properties: t.Parameters["properties"]}
		if req, ok := t.Parameters["required"].([]string); ok {
			schema.Required = req
		}
		tp := anthropic.ToolParam{Name: t.Name, InputSchema: schema}
		if t.Description != "" {
			tp.Description = anthropic.String(t.Description)
		}
		out = append(out, anthropic.ToolUnionParam{OfTool: &tp})
	}
	return out
}

// anthropicError turns the SDK's API error into the package's own, so
// tool refusals, failover and the messages people see treat it like any
// other server's.
func anthropicError(err error) error {
	var ae *anthropic.Error
	if !errors.As(err, &ae) {
		return err
	}
	he := &httpError{status: ae.StatusCode, body: strings.TrimSpace(ae.Message)}
	if ae.Request != nil {
		he.method, he.url = ae.Request.Method, ae.Request.URL.String()
	}
	if t := string(ae.Type()); t != "" {
		he.body = strings.TrimSpace(t + ": " + he.body)
	}
	if ae.RequestID != "" {
		he.body += " (request " + ae.RequestID + ")"
	}
	he.body = textutil.Truncate(he.body, 500)
	return he
}

func (a *anthropicClient) Models(ctx context.Context) ([]string, error) {
	pager := a.client.Models.ListAutoPaging(ctx, anthropic.ModelListParams{})
	var ids []string
	for pager.Next() {
		ids = append(ids, pager.Current().ID)
	}
	if err := pager.Err(); err != nil {
		return nil, anthropicError(err)
	}
	sort.Strings(ids)
	return ids, nil
}

// Limits reads the model's context window and output cap from the Models
// API.
func (a *anthropicClient) Limits(ctx context.Context) (Limits, error) {
	if a.cfg.Model == "" {
		return Limits{}, errors.New("no model selected")
	}
	m, err := a.client.Models.Get(ctx, a.cfg.Model, anthropic.ModelGetParams{})
	if err != nil {
		var ae *anthropic.Error
		if errors.As(err, &ae) && ae.StatusCode == http.StatusNotFound {
			return Limits{}, fmt.Errorf("Anthropic has no model named %q; check the name", a.cfg.Model)
		}
		return Limits{}, anthropicError(err)
	}
	if m.MaxInputTokens <= 0 && m.MaxTokens <= 0 {
		return Limits{}, errNoLimits
	}
	return Limits{Context: int(m.MaxInputTokens), Output: int(m.MaxTokens)}, nil
}
