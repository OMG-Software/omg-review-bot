package llm

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

var readTool = Tool{Name: "read_file", Description: "Read a file.",
	Parameters: map[string]any{"type": "object", "properties": map[string]any{"path": map[string]any{"type": "string"}}}}

// A conversation with a tool call and its result, as the bot sends it back.
var toolConvo = []Message{
	{Role: "user", Content: "Review this."},
	{Role: "assistant", ToolCalls: []ToolCall{{ID: "c1", Name: "read_file", Arguments: `{"path":"a.go"}`}}, Reasoning: "I should read a.go."},
	{Role: "tool", Content: "package a", ToolCallID: "c1", ToolName: "read_file"},
}

// replyServer answers every request with reply and hands each request body
// to the test through the returned channel.
func replyServer(t *testing.T, reply string) (*httptest.Server, <-chan map[string]any) {
	got := make(chan map[string]any, 4)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		got <- body
		_, _ = io.WriteString(w, reply)
	}))
	t.Cleanup(srv.Close)
	return srv, got
}

func TestOpenAIToolCalls(t *testing.T) {
	// Arguments come as a string holding JSON, as OpenAI sends them.
	srv, got := replyServer(t, `{"choices":[{"message":{"content":null,"reasoning_content":"Now b.go.","tool_calls":[{"id":"c2","type":"function","function":{"name":"read_file","arguments":"{\"path\":\"b.go\"}"}}]},"finish_reason":"tool_calls"}]}`)
	c, _ := New(Config{Provider: "lmstudio", BaseURL: srv.URL + "/v1", Model: "m"})
	resp, err := c.Chat(context.Background(), Request{Messages: toolConvo, Tools: []Tool{readTool}})
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.ToolCalls) != 1 || resp.ToolCalls[0].ID != "c2" || resp.ToolCalls[0].Name != "read_file" || resp.ToolCalls[0].Arguments != `{"path":"b.go"}` {
		t.Fatalf("tool calls = %+v", resp.ToolCalls)
	}
	if resp.Reasoning != "Now b.go." {
		t.Errorf("reasoning = %q", resp.Reasoning)
	}
	body := <-got
	tools, _ := json.Marshal(body["tools"])
	if !strings.Contains(string(tools), `"type":"function"`) || !strings.Contains(string(tools), `"name":"read_file"`) {
		t.Errorf("tools sent = %s", tools)
	}
	msgs, _ := json.Marshal(body["messages"])
	for _, want := range []string{`"tool_calls":[{"function":{"arguments":"{\"path\":\"a.go\"}","name":"read_file"},"id":"c1","type":"function"}]`,
		`"reasoning_content":"I should read a.go."`, `"role":"tool","tool_call_id":"c1"`} {
		if !strings.Contains(string(msgs), want) {
			t.Errorf("messages sent missing %s:\n%s", want, msgs)
		}
	}
}

func TestOllamaToolCalls(t *testing.T) {
	// Ollama sends arguments as an object, and no call ID.
	srv, got := replyServer(t, `{"message":{"content":"","thinking":"Now b.go.","tool_calls":[{"function":{"name":"read_file","arguments":{"path":"b.go"}}}]},"done_reason":"stop"}`)
	c, _ := New(Config{Provider: "ollama", BaseURL: srv.URL, Model: "m"})
	resp, err := c.Chat(context.Background(), Request{Messages: toolConvo, Tools: []Tool{readTool}})
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.ToolCalls) != 1 || resp.ToolCalls[0].ID == "" || resp.ToolCalls[0].Arguments != `{"path":"b.go"}` || resp.Reasoning != "Now b.go." {
		t.Fatalf("response = %+v", resp)
	}
	msgs, _ := json.Marshal((<-got)["messages"])
	for _, want := range []string{`"tool_calls":[{"function":{"arguments":{"path":"a.go"},"name":"read_file"}}]`,
		`"thinking":"I should read a.go."`, `"tool_name":"read_file"`} {
		if !strings.Contains(string(msgs), want) {
			t.Errorf("messages sent missing %s:\n%s", want, msgs)
		}
	}
}

// Missing, null and empty arguments are an empty object; so is anything
// sent back that isn't an object.
func TestToolArgs(t *testing.T) {
	for in, want := range map[string]string{``: `{}`, `null`: `{}`, `""`: `{}`, `"null"`: `{}`, `" "`: `{}`,
		`{"a":1}`: `{"a":1}`, `"{\"a\":1}"`: `{"a":1}`} {
		if got := rawArgs(json.RawMessage(in)); got != want {
			t.Errorf("rawArgs(%s) = %s, want %s", in, got, want)
		}
	}
	for in, want := range map[string]string{`null`: `{}`, `[1]`: `{}`, `x`: `{}`, `{"a":1}`: `{"a":1}`} {
		if got := string(argsObject(in)); got != want {
			t.Errorf("argsObject(%s) = %s, want %s", in, got, want)
		}
	}
}

func TestToolsRefused(t *testing.T) {
	status, body := http.StatusBadRequest, `{"error":"registry.ollama.ai/library/m does not support tools"}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}))
	defer srv.Close()
	c, _ := New(Config{Provider: "ollama", BaseURL: srv.URL, Model: "m"})
	_, err := c.Chat(context.Background(), Request{Messages: toolConvo[:1], Tools: []Tool{readTool}})
	if !errors.Is(err, ErrToolsUnsupported) {
		t.Errorf("with tools: err = %v, want ErrToolsUnsupported", err)
	}
	// Without tools the same refusal is just an error.
	_, err = c.Chat(context.Background(), Request{Messages: toolConvo[:1]})
	if err == nil || errors.Is(err, ErrToolsUnsupported) {
		t.Errorf("without tools: err = %v", err)
	}
	// Other words for tools.
	for _, msg := range []string{`function calling is not supported for this model`, `"tool_choice" is not allowed`, `functions are not enabled`} {
		body = msg
		if _, err := c.Chat(context.Background(), Request{Messages: toolConvo[:1], Tools: []Tool{readTool}}); !errors.Is(err, ErrToolsUnsupported) {
			t.Errorf("%s: err = %v, want ErrToolsUnsupported", msg, err)
		}
	}
	// A rate limit is not a refusal, whatever the model is called.
	status, body = http.StatusTooManyRequests, `{"error":"rate limit exceeded for model codetool"}`
	_, err = c.Chat(context.Background(), Request{Messages: toolConvo[:1], Tools: []Tool{readTool}})
	if err == nil || errors.Is(err, ErrToolsUnsupported) {
		t.Errorf("rate limited: err = %v", err)
	}
}
