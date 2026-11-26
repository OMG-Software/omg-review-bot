package llm

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

// chatServer answers each chat request with the next of replies (status and
// body), handing each request to the test.
func chatServer(t *testing.T, replies ...apiError) (*httptest.Server, <-chan seen) {
	got := make(chan seen, 8)
	n := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		got <- seen{r.Method, r.URL.Path, r.Header.Clone(), body}
		rep := replies[min(n, len(replies)-1)]
		n++
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(rep.status)
		_, _ = io.WriteString(w, rep.body)
	}))
	t.Cleanup(srv.Close)
	return srv, got
}

const okChat = `{"choices":[{"message":{"content":"OK"},"finish_reason":"stop"}],"usage":{"prompt_tokens":5,"completion_tokens":1}}`

// TestOpenAIAPI: OpenAI's own API gets max_completion_tokens (its reasoning
// models refuse max_tokens) and the key as a bearer token. A model that
// refuses a temperature is asked again without one, and isn't sent one
// again.
func TestOpenAIAPI(t *testing.T) {
	srv, got := chatServer(t,
		apiError{400, `{"error":{"message":"Unsupported value: 'temperature' does not support 0.2 with this model. Only the default (1) value is supported.","type":"invalid_request_error","param":"temperature"}}`},
		apiError{200, okChat})
	c, _ := New(Config{Provider: "openai-api", BaseURL: srv.URL + "/v1", APIKey: "sk-openai", Model: "gpt-x"})
	req := Request{Messages: []Message{{Role: "user", Content: "Hi"}}, Temperature: 0.2, MaxTokens: 900}
	resp, err := c.Chat(context.Background(), req)
	if err != nil || resp.Content != "OK" {
		t.Fatalf("chat = %+v, %v", resp, err)
	}
	first, second := <-got, <-got
	if first.path != "/v1/chat/completions" || first.header.Get("Authorization") != "Bearer sk-openai" || first.header.Get("X-Title") != "" {
		t.Errorf("request %s, headers %v", first.path, first.header)
	}
	if first.body["max_completion_tokens"] != float64(900) || first.body["max_tokens"] != nil || first.body["temperature"] != 0.2 {
		t.Errorf("first body = %v", first.body)
	}
	if _, ok := second.body["temperature"]; ok {
		t.Errorf("retry still sent a temperature: %v", second.body)
	}
	if _, err := c.Chat(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	if _, ok := (<-got).body["temperature"]; ok {
		t.Error("a temperature was sent again after the model refused one")
	}
}

// TestOpenAIOtherErrorsNotRetried: a 400 about something else isn't taken
// for a temperature refusal.
func TestOpenAIOtherErrorsNotRetried(t *testing.T) {
	srv, got := chatServer(t, apiError{400, `{"error":{"message":"model not found"}}`})
	c, _ := New(Config{Provider: "openai-api", BaseURL: srv.URL + "/v1", APIKey: "k", Model: "gpt-x"})
	if _, err := c.Chat(context.Background(), Request{Messages: []Message{{Role: "user", Content: "Hi"}}, Temperature: 0.2}); err == nil {
		t.Fatal("no error")
	}
	<-got
	select {
	case r := <-got:
		t.Errorf("asked again: %v", r.body)
	default:
	}
}

// TestOpenRouter: OpenRouter's requests name the app, keep max_tokens, and
// its "reasoning" field is read as the reasoning.
func TestOpenRouter(t *testing.T) {
	srv, got := chatServer(t, apiError{200, `{"choices":[{"message":{"content":"OK","reasoning":"Short."},"finish_reason":"stop"}]}`})
	c, _ := New(Config{Provider: "openrouter", BaseURL: srv.URL + "/api/v1", APIKey: "sk-or", Model: "vendor/model"})
	resp, err := c.Chat(context.Background(), Request{Messages: []Message{{Role: "user", Content: "Hi"}}, MaxTokens: 500})
	if err != nil || resp.Reasoning != "Short." {
		t.Fatalf("chat = %+v, %v", resp, err)
	}
	r := <-got
	if r.path != "/api/v1/chat/completions" || r.header.Get("X-Title") != appName || r.header.Get("Authorization") != "Bearer sk-or" {
		t.Errorf("request %s, headers %v", r.path, r.header)
	}
	if r.body["max_tokens"] != float64(500) || r.body["max_completion_tokens"] != nil {
		t.Errorf("body = %v", r.body)
	}
}

// TestOpenRouterReasoningDetails: the structured reasoning a tool call came
// with goes back unchanged with its result, to the same model only.
func TestOpenRouterReasoningDetails(t *testing.T) {
	details := `[{"data":"sig-1","format":"google-gemini-v1","index":0,"type":"reasoning.encrypted"}]` // keys as json.Marshal orders them
	srv, got := chatServer(t,
		apiError{200, `{"choices":[{"message":{"content":"","reasoning":"Look first.","reasoning_details":` + details + `,"tool_calls":[{"id":"call_1","type":"function","function":{"name":"read_file","arguments":"{\"path\":\"a.go\"}"}}]},"finish_reason":"tool_calls"}]}`},
		apiError{200, okChat})
	c, _ := New(Config{Provider: "openrouter", BaseURL: srv.URL + "/api/v1", APIKey: "sk-or", Model: "google/gemini-x"})
	msgs := []Message{{Role: "user", Content: "Review this."}}
	resp, err := c.Chat(context.Background(), Request{Messages: msgs, Tools: []Tool{readTool}})
	if err != nil || len(resp.ToolCalls) != 1 {
		t.Fatalf("chat = %+v, %v", resp, err)
	}
	<-got
	turn := append(msgs, Message{Role: "assistant", ToolCalls: resp.ToolCalls, Reasoning: resp.Reasoning, Native: resp.Native},
		Message{Role: "tool", Content: "package a", ToolCallID: "call_1", ToolName: "read_file"})
	if _, err := c.Chat(context.Background(), Request{Messages: turn, Tools: []Tool{readTool}}); err != nil {
		t.Fatal(err)
	}
	sent := (<-got).body["messages"].([]any)[1].(map[string]any)
	if b, _ := json.Marshal(sent["reasoning_details"]); string(b) != details || sent["reasoning_content"] != nil {
		t.Errorf("assistant turn sent back = %v", sent)
	}
	// Another model gets the plain reasoning, not the first one's details.
	other, got2 := chatServer(t, apiError{200, okChat})
	c2, _ := New(Config{Provider: "openrouter", BaseURL: other.URL + "/api/v1", APIKey: "sk-or", Model: "vendor/other"})
	if _, err := c2.Chat(context.Background(), Request{Messages: turn, Tools: []Tool{readTool}}); err != nil {
		t.Fatal(err)
	}
	sent = (<-got2).body["messages"].([]any)[1].(map[string]any)
	if sent["reasoning_details"] != nil || sent["reasoning_content"] != "Look first." {
		t.Errorf("assistant turn sent to another model = %v", sent)
	}
}

// TestReasoningTokensExhausted: a server that hides its reasoning but
// counts it (OpenAI's reasoning models) and is cut off with no answer gets
// the reasoning-exhausted error, not an empty answer.
func TestReasoningTokensExhausted(t *testing.T) {
	srv, _ := chatServer(t, apiError{200, `{"choices":[{"message":{"content":""},"finish_reason":"length"}],"usage":{"prompt_tokens":5,"completion_tokens":600,"completion_tokens_details":{"reasoning_tokens":600}}}`})
	c, _ := New(Config{Provider: "openai-api", BaseURL: srv.URL + "/v1", APIKey: "k", Model: "gpt-x"})
	_, err := c.Chat(context.Background(), Request{Messages: []Message{{Role: "user", Content: "Hi"}}, MaxTokens: 600})
	if !errors.Is(err, ErrReasoningExhausted) {
		t.Errorf("err = %v, want ErrReasoningExhausted", err)
	}
}
