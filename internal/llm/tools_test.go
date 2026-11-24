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
	{Role: "assistant", ToolCalls: []ToolCall{{ID: "c1", Name: "read_file", Arguments: `{"path":"a.go"}`}}},
	{Role: "tool", Content: "package a", ToolCallID: "c1", ToolName: "read_file"},
}

func TestOpenAIToolCalls(t *testing.T) {
	var got map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&got)
		// Arguments come as a string holding JSON, as OpenAI sends them.
		_, _ = io.WriteString(w, `{"choices":[{"message":{"content":null,"tool_calls":[{"id":"c2","type":"function","function":{"name":"read_file","arguments":"{\"path\":\"b.go\"}"}}]},"finish_reason":"tool_calls"}]}`)
	}))
	defer srv.Close()
	c, _ := New(Config{Provider: "lmstudio", BaseURL: srv.URL + "/v1", Model: "m"})
	resp, err := c.Chat(context.Background(), Request{Messages: toolConvo, Tools: []Tool{readTool}})
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.ToolCalls) != 1 || resp.ToolCalls[0].ID != "c2" || resp.ToolCalls[0].Name != "read_file" || resp.ToolCalls[0].Arguments != `{"path":"b.go"}` {
		t.Fatalf("tool calls = %+v", resp.ToolCalls)
	}
	tools, _ := json.Marshal(got["tools"])
	if !strings.Contains(string(tools), `"type":"function"`) || !strings.Contains(string(tools), `"name":"read_file"`) {
		t.Errorf("tools sent = %s", tools)
	}
	msgs, _ := json.Marshal(got["messages"])
	for _, want := range []string{`"tool_calls":[{"function":{"arguments":"{\"path\":\"a.go\"}","name":"read_file"},"id":"c1","type":"function"}]`,
		`"role":"tool","tool_call_id":"c1"`} {
		if !strings.Contains(string(msgs), want) {
			t.Errorf("messages sent missing %s:\n%s", want, msgs)
		}
	}
}

func TestOllamaToolCalls(t *testing.T) {
	var got map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&got)
		// Ollama sends arguments as an object, and no call ID.
		_, _ = io.WriteString(w, `{"message":{"content":"","tool_calls":[{"function":{"name":"read_file","arguments":{"path":"b.go"}}}]},"done_reason":"stop"}`)
	}))
	defer srv.Close()
	c, _ := New(Config{Provider: "ollama", BaseURL: srv.URL, Model: "m"})
	resp, err := c.Chat(context.Background(), Request{Messages: toolConvo, Tools: []Tool{readTool}})
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.ToolCalls) != 1 || resp.ToolCalls[0].ID == "" || resp.ToolCalls[0].Arguments != `{"path":"b.go"}` {
		t.Fatalf("tool calls = %+v", resp.ToolCalls)
	}
	msgs, _ := json.Marshal(got["messages"])
	for _, want := range []string{`"tool_calls":[{"function":{"arguments":{"path":"a.go"},"name":"read_file"}}]`, `"tool_name":"read_file"`} {
		if !strings.Contains(string(msgs), want) {
			t.Errorf("messages sent missing %s:\n%s", want, msgs)
		}
	}
}

func TestToolsRefused(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, `{"error":"registry.ollama.ai/library/m does not support tools"}`)
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
}
