package llm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// seen is one request a stand-in server got.
type seen struct {
	method, path string
	header       http.Header
	body         map[string]any
}

// sse writes events as a server-sent event stream, each named by its type.
func sse(w io.Writer, events ...map[string]any) {
	for _, e := range events {
		data, _ := json.Marshal(e)
		fmt.Fprintf(w, "event: %s\ndata: %s\n\n", e["type"], data)
	}
}

type obj = map[string]any

// claudeAnswer is a Messages API stream with blocks, ending for stop.
func claudeAnswer(stop string, details any, blocks ...obj) []obj {
	ev := []obj{{"type": "message_start", "message": obj{"id": "msg_1", "type": "message", "role": "assistant", "model": "claude-x",
		"content": []any{}, "stop_reason": nil, "usage": obj{"input_tokens": 100, "output_tokens": 1, "cache_read_input_tokens": 900, "cache_creation_input_tokens": 50}}}}
	for i, b := range blocks {
		switch b["type"] {
		case "text":
			ev = append(ev, obj{"type": "content_block_start", "index": i, "content_block": obj{"type": "text", "text": ""}},
				obj{"type": "content_block_delta", "index": i, "delta": obj{"type": "text_delta", "text": b["text"]}})
		case "thinking":
			ev = append(ev, obj{"type": "content_block_start", "index": i, "content_block": obj{"type": "thinking", "thinking": "", "signature": ""}},
				obj{"type": "content_block_delta", "index": i, "delta": obj{"type": "thinking_delta", "thinking": b["thinking"]}},
				obj{"type": "content_block_delta", "index": i, "delta": obj{"type": "signature_delta", "signature": b["signature"]}})
		case "tool_use":
			ev = append(ev, obj{"type": "content_block_start", "index": i, "content_block": obj{"type": "tool_use", "id": b["id"], "name": b["name"], "input": obj{}}},
				obj{"type": "content_block_delta", "index": i, "delta": obj{"type": "input_json_delta", "partial_json": b["input"]}})
		}
		ev = append(ev, obj{"type": "content_block_stop", "index": i})
	}
	return append(ev, obj{"type": "message_delta", "delta": obj{"stop_reason": stop, "stop_sequence": nil, "stop_details": details}, "usage": obj{"output_tokens": 42}},
		obj{"type": "message_stop"})
}

// claudeServer answers each Messages request with the next of answers (a
// stream, or an error status and body), and the Models API from models.
func claudeServer(t *testing.T, answers ...any) (*httptest.Server, <-chan seen) {
	got := make(chan seen, 8)
	n := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		got <- seen{r.Method, r.URL.Path, r.Header.Clone(), body}
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/v1/messages":
			if n >= len(answers) {
				http.Error(w, "no more answers", http.StatusTeapot)
				return
			}
			a := answers[n]
			n++
			if e, ok := a.(apiError); ok {
				w.Header().Set("Content-Type", "application/json")
				w.Header().Set("request-id", "req_1")
				w.WriteHeader(e.status)
				_, _ = io.WriteString(w, e.body)
				return
			}
			w.Header().Set("Content-Type", "text/event-stream")
			sse(w, a.([]obj)...)
		case r.URL.Path == "/v1/models":
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(obj{"data": []obj{
				{"type": "model", "id": "claude-z", "display_name": "Z", "created_at": "2026-01-01T00:00:00Z"},
				{"type": "model", "id": "claude-a", "display_name": "A", "created_at": "2026-01-01T00:00:00Z"},
			}, "has_more": false, "first_id": "claude-z", "last_id": "claude-a"})
		case r.URL.Path == "/v1/models/claude-x":
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(obj{"type": "model", "id": "claude-x", "display_name": "X", "created_at": "2026-01-01T00:00:00Z",
				"max_input_tokens": 1000000, "max_tokens": 128000})
		default:
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusNotFound)
			_, _ = io.WriteString(w, `{"type":"error","error":{"type":"not_found_error","message":"model: nope"}}`)
		}
	}))
	t.Cleanup(srv.Close)
	return srv, got
}

type apiError struct {
	status int
	body   string
}

func claude(t *testing.T, srv *httptest.Server, key string) Client {
	t.Helper()
	c, err := New(Config{Provider: "anthropic", BaseURL: srv.URL + "/v1", APIKey: key, Model: "claude-x", Structured: true})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// TestAnthropicChat: a request goes to the Messages API with the key, the
// system prompt cached, the schema as output_config and no temperature; the
// answer's text, thinking and usage (cache reads included) come back.
func TestAnthropicChat(t *testing.T) {
	srv, got := claudeServer(t, claudeAnswer("end_turn", nil,
		obj{"type": "thinking", "thinking": "Looks fine.", "signature": "sig-1"},
		obj{"type": "text", "text": `{"findings":[]}`}))
	resp, err := claude(t, srv, "sk-ant-test").Chat(context.Background(), Request{
		Messages:    []Message{{Role: "system", Content: "You review code."}, {Role: "user", Content: "Review this."}},
		Schema:      map[string]any{"type": "object", "properties": map[string]any{"findings": map[string]any{"type": "array"}}},
		Temperature: 0.3, MaxTokens: 2000,
	})
	if err != nil {
		t.Fatal(err)
	}
	if resp.Content != `{"findings":[]}` || resp.Reasoning != "Looks fine." || resp.Truncated {
		t.Errorf("response = %+v", resp)
	}
	if resp.PromptTokens != 1050 || resp.CompletionTokens != 42 {
		t.Errorf("tokens = %d prompt, %d completion; want 1050 (cache reads count), 42", resp.PromptTokens, resp.CompletionTokens)
	}
	r := <-got
	// The SDK sends the key as a bearer token.
	if r.path != "/v1/messages" || r.header.Get("Authorization") != "Bearer sk-ant-test" || r.header.Get("Anthropic-Version") == "" {
		t.Errorf("request %s, headers %v", r.path, r.header)
	}
	b := r.body
	if b["model"] != "claude-x" || b["max_tokens"] != float64(2000) || b["stream"] != true {
		t.Errorf("body = %v", b)
	}
	if _, ok := b["temperature"]; ok {
		t.Error("a temperature was sent: current Claude models refuse one")
	}
	sys, _ := b["system"].([]any)
	if len(sys) != 1 || sys[0].(map[string]any)["text"] != "You review code." || sys[0].(map[string]any)["cache_control"] == nil {
		t.Errorf("system = %v, want one cached block", b["system"])
	}
	if f, _ := b["output_config"].(map[string]any)["format"].(map[string]any); f["type"] != "json_schema" || f["schema"] == nil {
		t.Errorf("output_config = %v", b["output_config"])
	}
	if msgs, _ := b["messages"].([]any); len(msgs) != 1 {
		t.Errorf("messages = %v (the system prompt isn't one)", b["messages"])
	}
}

// TestAnthropicNoHostCredentials: a model without a key of its own sends
// none, even with the host's ANTHROPIC_API_KEY and ANTHROPIC_BASE_URL set:
// an organization's model mustn't be billed to the instance's account.
func TestAnthropicNoHostCredentials(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "sk-ant-host")
	t.Setenv("ANTHROPIC_AUTH_TOKEN", "host-token")
	t.Setenv("ANTHROPIC_BASE_URL", "http://127.0.0.1:1/never")
	srv, got := claudeServer(t, claudeAnswer("end_turn", nil, obj{"type": "text", "text": "OK"}))
	if _, err := claude(t, srv, "").Chat(context.Background(), Request{Messages: []Message{{Role: "user", Content: "Hi"}}}); err != nil {
		t.Fatal(err)
	}
	r := <-got
	if k, a := r.header.Get("X-Api-Key"), r.header.Get("Authorization"); k != "" || a != "" {
		t.Errorf("the host's credentials were sent: x-api-key %q, authorization %q", k, a)
	}
	if r.body["max_tokens"] != float64(defaultAnthropicMaxTokens) {
		t.Errorf("max_tokens = %v, want the default", r.body["max_tokens"])
	}
}

// TestAnthropicToolTurn: tools go as Claude's tools with the conversation
// cached; a tool call comes back with its turn as Claude gave it, sent back
// as it is (signed thinking included) to the same model, and rebuilt
// without the thinking for another model after a failover.
func TestAnthropicToolTurn(t *testing.T) {
	srv, got := claudeServer(t,
		claudeAnswer("tool_use", nil,
			obj{"type": "thinking", "thinking": "Read a.go.", "signature": "sig-1"},
			obj{"type": "tool_use", "id": "toolu_1", "name": "read_file", "input": `{"path":"a.go"}`}),
		claudeAnswer("end_turn", nil, obj{"type": "text", "text": "Done."}),
		claudeAnswer("end_turn", nil, obj{"type": "text", "text": "Done."}))
	c := claude(t, srv, "k")
	msgs := []Message{{Role: "system", Content: "Review."}, {Role: "user", Content: "Review this."}}
	resp, err := c.Chat(context.Background(), Request{Messages: msgs, Tools: []Tool{readTool}})
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.ToolCalls) != 1 || resp.ToolCalls[0].ID != "toolu_1" || resp.ToolCalls[0].Name != "read_file" || resp.ToolCalls[0].Arguments != `{"path":"a.go"}` {
		t.Fatalf("tool calls = %+v", resp.ToolCalls)
	}
	r := <-got
	tools, _ := r.body["tools"].([]any)
	if len(tools) != 1 || tools[0].(map[string]any)["name"] != "read_file" || tools[0].(map[string]any)["input_schema"] == nil || r.body["cache_control"] == nil {
		t.Errorf("tools = %v, cache_control = %v", r.body["tools"], r.body["cache_control"])
	}

	turn := append(msgs, Message{Role: "assistant", ToolCalls: resp.ToolCalls, Reasoning: resp.Reasoning, Native: resp.Native},
		Message{Role: "tool", Content: "package a", ToolCallID: "toolu_1", ToolName: "read_file"})
	if _, err := c.Chat(context.Background(), Request{Messages: turn, Tools: []Tool{readTool}}); err != nil {
		t.Fatal(err)
	}
	sent := (<-got).body["messages"].([]any)
	if len(sent) != 3 {
		t.Fatalf("messages = %v", sent)
	}
	blocks := sent[1].(map[string]any)["content"].([]any)
	if len(blocks) != 2 || blocks[0].(map[string]any)["type"] != "thinking" || blocks[0].(map[string]any)["signature"] != "sig-1" ||
		blocks[1].(map[string]any)["type"] != "tool_use" {
		t.Errorf("assistant turn sent back = %v, want its signed thinking and call", blocks)
	}
	res := sent[2].(map[string]any)
	if rb := res["content"].([]any)[0].(map[string]any); res["role"] != "user" || rb["type"] != "tool_result" || rb["tool_use_id"] != "toolu_1" {
		t.Errorf("tool result = %v", res)
	}

	other, _ := New(Config{Provider: "anthropic", BaseURL: srv.URL, APIKey: "k", Model: "claude-other"})
	if _, err := other.Chat(context.Background(), Request{Messages: turn, Tools: []Tool{readTool}}); err != nil {
		t.Fatal(err)
	}
	blocks = (<-got).body["messages"].([]any)[1].(map[string]any)["content"].([]any)
	if len(blocks) != 1 || blocks[0].(map[string]any)["type"] != "tool_use" || blocks[0].(map[string]any)["id"] != "toolu_1" {
		t.Errorf("another model got %v, want the call without the first model's thinking", blocks)
	}
}

// TestAnthropicRefusalsAndErrors: a refusal is ErrRefused (so a failover
// tries the next model); a 4xx about tools is ErrToolsUnsupported; other
// errors say what the API said.
func TestAnthropicRefusalsAndErrors(t *testing.T) {
	srv, _ := claudeServer(t,
		claudeAnswer("refusal", obj{"type": "refusal", "category": "cyber", "explanation": "Not this."}),
		apiError{400, `{"type":"error","error":{"type":"invalid_request_error","message":"tools.0.input_schema: invalid"}}`},
		apiError{401, `{"type":"error","error":{"type":"authentication_error","message":"invalid x-api-key"}}`},
		claudeAnswer("max_tokens", nil, obj{"type": "thinking", "thinking": "Hmm", "signature": "s"}))
	c := claude(t, srv, "k")
	hi := Request{Messages: []Message{{Role: "user", Content: "Hi"}}}
	if _, err := c.Chat(context.Background(), hi); !errors.Is(err, ErrRefused) || !strings.Contains(err.Error(), "cyber") {
		t.Errorf("refusal = %v", err)
	}
	if _, err := c.Chat(context.Background(), Request{Messages: hi.Messages, Tools: []Tool{readTool}}); !errors.Is(err, ErrToolsUnsupported) {
		t.Errorf("tool error = %v, want ErrToolsUnsupported", err)
	}
	_, err := c.Chat(context.Background(), hi)
	var he *httpError
	if !errors.As(err, &he) || he.status != 401 || !strings.Contains(err.Error(), "authentication_error") || !strings.Contains(err.Error(), "req_1") {
		t.Errorf("auth error = %v", err)
	}
	if _, err := c.Chat(context.Background(), Request{Messages: hi.Messages, MaxTokens: 600}); err == nil || !strings.Contains(err.Error(), "reasoning") {
		t.Errorf("all thinking, cut off = %v, want the reasoning-exhausted error", err)
	}
}

func TestAnthropicModelsAndLimits(t *testing.T) {
	srv, _ := claudeServer(t)
	c := claude(t, srv, "k")
	ms, err := c.Models(context.Background())
	if err != nil || strings.Join(ms, ",") != "claude-a,claude-z" {
		t.Errorf("models = %v, %v", ms, err)
	}
	lim, err := c.(LimitReader).Limits(context.Background())
	if err != nil || lim.Context != 1000000 || lim.Output != 128000 {
		t.Errorf("limits = %+v, %v", lim, err)
	}
	nope, _ := New(Config{Provider: "anthropic", BaseURL: srv.URL, APIKey: "k", Model: "nope"})
	if _, err := nope.(LimitReader).Limits(context.Background()); err == nil || !strings.Contains(err.Error(), `no model named "nope"`) {
		t.Errorf("unknown model's limits = %v", err)
	}
}
