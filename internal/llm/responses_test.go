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
	"sync"
	"testing"
)

// tokens is a sign-in for tests: each renewal gives the next token.
type tokens struct {
	mu      sync.Mutex
	n       int
	renewed int
	err     error
}

func (t *tokens) Token(_ context.Context, refused string) (string, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.err != nil {
		return "", t.err
	}
	cur := fmt.Sprintf("tok-%d", t.n)
	if t.n == 0 || refused == cur {
		if t.n > 0 {
			t.renewed++
		}
		t.n++
	}
	return fmt.Sprintf("tok-%d", t.n), nil
}

// planServer answers each Responses request with the next of streams (or
// an error status and body). Its model catalog is fixed. accept is the only
// token it takes ("" for any).
func planServer(t *testing.T, accept string, streams ...any) (*httptest.Server, <-chan seen) {
	got := make(chan seen, 8)
	n := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		got <- seen{r.Method, r.URL.Path, r.Header.Clone(), body}
		if accept != "" && r.Header.Get("Authorization") != "Bearer "+accept {
			http.Error(w, `{"error":{"code":"invalid_token","message":"expired"}}`, http.StatusUnauthorized)
			return
		}
		if r.URL.Path == "/v1/models" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"models":[{"slug":"gpt-b","visibility":"list","context_window":400000,"max_output_tokens":128000},{"slug":"internal","visibility":"hide"},{"slug":"gpt-a","visibility":"list"}]}`)
			return
		}
		s := streams[min(n, len(streams)-1)]
		n++
		if e, ok := s.(apiError); ok {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(e.status)
			_, _ = io.WriteString(w, e.body)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, s.(string))
	}))
	t.Cleanup(srv.Close)
	return srv, got
}

// completed is a stream ending with a completed response holding output.
func completed(output ...string) string {
	return "event: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"status\":\"in_progress\"}}\n\n" +
		"event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"ignored\"}\n\n" +
		"event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\",\"output\":[" +
		strings.Join(output, ",") + "],\"usage\":{\"input_tokens\":30,\"output_tokens\":7}}}\n\n"
}

const planText = `{"type":"message","role":"assistant","content":[{"type":"output_text","text":"{\"findings\":[]}"}]}`

func plan(t *testing.T, srv *httptest.Server, tok TokenSource) Client {
	t.Helper()
	c, err := New(Config{Provider: "chatgpt", BaseURL: srv.URL + "/v1", Model: "gpt-b", Structured: true, Tokens: tok})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// TestResponsesChat: a request has the shape the plan's preview requires
// (store false, streamed, the system prompt as instructions, no temperature
// or output cap) and carries the sign-in's token; the answer is read from
// the completed response.
func TestResponsesChat(t *testing.T) {
	srv, got := planServer(t, "", completed(
		`{"type":"reasoning","id":"rs_1","summary":[{"type":"summary_text","text":"Checked."}]}`, planText))
	resp, err := plan(t, srv, &tokens{}).Chat(context.Background(), Request{
		Messages: []Message{{Role: "system", Content: "You review code."}, {Role: "user", Content: "Review this."}},
		Schema:   map[string]any{"type": "object"}, SchemaName: "review",
		Temperature: 0.4, MaxTokens: 1000,
	})
	if err != nil {
		t.Fatal(err)
	}
	if resp.Content != `{"findings":[]}` || resp.Reasoning != "Checked." || resp.PromptTokens != 30 || resp.CompletionTokens != 7 || resp.Native != nil {
		t.Errorf("response = %+v", resp)
	}
	r := <-got
	b := r.body
	if r.path != "/v1/responses" || r.header.Get("Authorization") != "Bearer tok-1" || r.header.Get("Accept") != "text/event-stream" {
		t.Errorf("request %s, headers %v", r.path, r.header)
	}
	if b["store"] != false || b["stream"] != true || b["instructions"] != "You review code." || b["model"] != "gpt-b" {
		t.Errorf("body = %v", b)
	}
	for _, k := range []string{"temperature", "max_output_tokens", "max_tokens"} {
		if _, ok := b[k]; ok {
			t.Errorf("%s sent: the plan refuses it", k)
		}
	}
	if in, _ := b["input"].([]any); len(in) != 1 || in[0].(map[string]any)["role"] != "user" {
		t.Errorf("input = %v (the system prompt isn't an input item)", b["input"])
	}
	if f, _ := b["text"].(map[string]any)["format"].(map[string]any); f["type"] != "json_schema" || f["name"] != "review" || f["strict"] != true {
		t.Errorf("text.format = %v", b["text"])
	}
}

// TestResponsesRenewsToken: a refused token is renewed once and the request
// made again; refused again, the error says to sign in again.
func TestResponsesRenewsToken(t *testing.T) {
	srv, got := planServer(t, "tok-2", completed(planText))
	tok := &tokens{}
	if _, err := plan(t, srv, tok).Chat(context.Background(), Request{Messages: []Message{{Role: "user", Content: "Hi"}}}); err != nil {
		t.Fatal(err)
	}
	if a, b := (<-got).header.Get("Authorization"), (<-got).header.Get("Authorization"); a != "Bearer tok-1" || b != "Bearer tok-2" || tok.renewed != 1 {
		t.Errorf("tokens sent %q then %q, renewed %d times", a, b, tok.renewed)
	}

	never, _ := planServer(t, "tok-never", completed(planText))
	_, err := plan(t, never, &tokens{}).Chat(context.Background(), Request{Messages: []Message{{Role: "user", Content: "Hi"}}})
	var he *httpError
	if !errors.As(err, &he) || he.status != http.StatusUnauthorized || !strings.Contains(err.Error(), "sign in again") {
		t.Errorf("refused twice = %v", err)
	}

	ended := &tokens{err: errors.New("the ChatGPT sign-in has expired")}
	if _, err := plan(t, srv, ended).Chat(context.Background(), Request{Messages: []Message{{Role: "user", Content: "Hi"}}}); err == nil || !strings.Contains(err.Error(), "expired") {
		t.Errorf("sign-in that can't give a token = %v", err)
	}
}

// TestResponsesToolTurn: tools go inside a namespace; a call comes back by
// its bare name, and its item is sent back as it came, without the id a
// server that stores nothing couldn't look up, before its output.
func TestResponsesToolTurn(t *testing.T) {
	srv, got := planServer(t, "", completed(
		`{"type":"reasoning","id":"rs_1","summary":[],"encrypted_content":"enc-1"}`,
		`{"type":"function_call","id":"fc_1","call_id":"call_1","name":"repo.read_file","arguments":"{\"path\":\"a.go\"}","status":"completed"}`),
		completed(planText))
	c := plan(t, srv, &tokens{})
	msgs := []Message{{Role: "user", Content: "Review this."}}
	resp, err := c.Chat(context.Background(), Request{Messages: msgs, Tools: []Tool{readTool}})
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.ToolCalls) != 1 || resp.ToolCalls[0].ID != "call_1" || resp.ToolCalls[0].Name != "read_file" || resp.ToolCalls[0].Arguments != `{"path":"a.go"}` {
		t.Fatalf("tool calls = %+v", resp.ToolCalls)
	}
	first := (<-got).body
	tools, _ := first["tools"].([]any)
	ns, _ := tools[0].(map[string]any)
	if len(tools) != 1 || ns["type"] != "namespace" || ns["name"] != toolNamespace || len(ns["tools"].([]any)) != 1 {
		t.Errorf("tools = %v", tools)
	}
	if inc, _ := first["include"].([]any); len(inc) != 1 || inc[0] != "reasoning.encrypted_content" {
		t.Errorf("include = %v, want the encrypted reasoning", first["include"])
	}

	turn := append(msgs, Message{Role: "assistant", ToolCalls: resp.ToolCalls, Native: resp.Native},
		Message{Role: "tool", Content: "package a", ToolCallID: "call_1", ToolName: "read_file"})
	if _, err := c.Chat(context.Background(), Request{Messages: turn, Tools: []Tool{readTool}}); err != nil {
		t.Fatal(err)
	}
	in := (<-got).body["input"].([]any)
	if len(in) != 4 {
		t.Fatalf("input = %v", in)
	}
	// The reasoning goes back ahead of the call it led to, as it came (IDs
	// and all).
	rs, call, out := in[1].(map[string]any), in[2].(map[string]any), in[3].(map[string]any)
	if rs["type"] != "reasoning" || rs["encrypted_content"] != "enc-1" || rs["id"] != "rs_1" {
		t.Errorf("reasoning sent back = %v", rs)
	}
	if call["type"] != "function_call" || call["call_id"] != "call_1" || call["name"] != "repo.read_file" || call["id"] != "fc_1" {
		t.Errorf("call sent back = %v", call)
	}
	if out["type"] != "function_call_output" || out["call_id"] != "call_1" || out["output"] != "package a" {
		t.Errorf("output = %v", out)
	}

	// Another model (a failover) gets the call, not this one's reasoning.
	other, got2 := planServer(t, "", completed(planText))
	c2, _ := New(Config{Provider: "chatgpt", BaseURL: other.URL + "/v1", Model: "gpt-c", Tokens: &tokens{}})
	if _, err := c2.Chat(context.Background(), Request{Messages: turn, Tools: []Tool{readTool}}); err != nil {
		t.Fatal(err)
	}
	in = (<-got2).body["input"].([]any)
	if len(in) != 3 || in[1].(map[string]any)["type"] != "function_call" {
		t.Errorf("input to another model = %v", in)
	}
}

// TestResponsesIncompleteCall: a call cut off at the output cap isn't run
// or sent back; with no other call, the answer is cut off.
func TestResponsesIncompleteCall(t *testing.T) {
	trunc := "data: {\"type\":\"response.incomplete\",\"response\":{\"status\":\"incomplete\",\"incomplete_details\":{\"reason\":\"max_output_tokens\"},\"output\":[" +
		`{"type":"function_call","call_id":"call_1","name":"repo.read_file","arguments":"{\"path\":\"a","status":"incomplete"}` + "]}}\n\n"
	srv, _ := planServer(t, "", trunc)
	resp, err := plan(t, srv, &tokens{}).Chat(context.Background(), Request{Messages: []Message{{Role: "user", Content: "Hi"}}, Tools: []Tool{readTool}})
	if err != nil || len(resp.ToolCalls) != 0 || !resp.Truncated || resp.Native != nil {
		t.Errorf("incomplete call = %+v, %v", resp, err)
	}
}

// TestResponsesNoEncryptedReasoning: a model that won't give its reasoning
// encrypted is asked again without, and isn't asked for it again.
func TestResponsesNoEncryptedReasoning(t *testing.T) {
	srv, got := planServer(t, "",
		apiError{400, `{"error":{"message":"Encrypted content is not supported with this model.","type":"invalid_request_error","param":"include"}}`},
		completed(planText))
	c := plan(t, srv, &tokens{})
	hi := Request{Messages: []Message{{Role: "user", Content: "Hi"}}}
	if _, err := c.Chat(context.Background(), hi); err != nil {
		t.Fatal(err)
	}
	<-got
	if _, ok := (<-got).body["include"]; ok {
		t.Error("asked again for the encrypted reasoning")
	}
	if _, err := c.Chat(context.Background(), hi); err != nil {
		t.Fatal(err)
	}
	if _, ok := (<-got).body["include"]; ok {
		t.Error("asked for the encrypted reasoning after the model refused it")
	}
}

// TestResponsesFailures: failures in the stream are errors with what they
// mean for the plan; a content filter or refusal is ErrRefused; a stream
// that stops early is an error, not an empty answer.
func TestResponsesFailures(t *testing.T) {
	hi := Request{Messages: []Message{{Role: "user", Content: "Hi"}}}
	cases := []struct {
		name, stream string
		check        func(error) bool
	}{
		{"usage limit", "data: {\"type\":\"error\",\"code\":\"subscription_sharing_usage_limit_exceeded\",\"message\":\"limit\"}\n\n",
			func(err error) bool {
				var he *httpError
				return errors.As(err, &he) && he.status == http.StatusTooManyRequests && strings.Contains(err.Error(), "chatgpt.com/settings/usage")
			}},
		{"failed", "data: {\"type\":\"response.failed\",\"response\":{\"status\":\"failed\",\"error\":{\"code\":\"server_error\",\"message\":\"boom\"}}}\n\n",
			func(err error) bool { return err != nil && strings.Contains(err.Error(), "boom") }},
		{"content filter", "data: {\"type\":\"response.incomplete\",\"response\":{\"status\":\"incomplete\",\"incomplete_details\":{\"reason\":\"content_filter\"},\"output\":[]}}\n\n",
			func(err error) bool { return errors.Is(err, ErrRefused) }},
		{"refusal", completed(`{"type":"message","content":[{"type":"refusal","refusal":"I can't help with that."}]}`),
			func(err error) bool { return errors.Is(err, ErrRefused) && strings.Contains(err.Error(), "can't help") }},
		{"cut short", "data: {\"type\":\"response.created\",\"response\":{\"status\":\"in_progress\"}}\n\n",
			func(err error) bool { return err != nil && strings.Contains(err.Error(), "ended before") }},
	}
	for _, tc := range cases {
		srv, _ := planServer(t, "", tc.stream)
		if _, err := plan(t, srv, &tokens{}).Chat(context.Background(), hi); !tc.check(err) {
			t.Errorf("%s: err = %v", tc.name, err)
		}
	}
	trunc := "data: {\"type\":\"response.incomplete\",\"response\":{\"status\":\"incomplete\",\"incomplete_details\":{\"reason\":\"max_output_tokens\"},\"output\":[" + planText + "]}}\n\n"
	cut, _ := planServer(t, "", trunc)
	if resp, err := plan(t, cut, &tokens{}).Chat(context.Background(), hi); err != nil || !resp.Truncated {
		t.Errorf("cut off at the output cap = %+v, %v", resp, err)
	}
	tools, _ := planServer(t, "", apiError{400, `{"error":{"code":"subscription_sharing_unsupported_capability","message":"tools are not supported","param":"tools"}}`})
	if _, err := plan(t, tools, &tokens{}).Chat(context.Background(), Request{Messages: hi.Messages, Tools: []Tool{readTool}}); !errors.Is(err, ErrToolsUnsupported) {
		t.Errorf("tools refused = %v, want ErrToolsUnsupported", err)
	}
}

func TestResponsesModels(t *testing.T) {
	srv, got := planServer(t, "", completed(planText))
	c := plan(t, srv, &tokens{})
	ms, err := c.Models(context.Background())
	if err != nil || strings.Join(ms, ",") != "gpt-a,gpt-b" {
		t.Errorf("models = %v, %v (the hidden one left out)", ms, err)
	}
	if r := <-got; r.header.Get("Authorization") != "Bearer tok-1" {
		t.Errorf("catalog asked for without the token: %v", r.header)
	}
	lim, err := c.(LimitReader).Limits(context.Background())
	if err != nil || lim.Context != 400000 || lim.Output != 128000 {
		t.Errorf("limits = %+v, %v", lim, err)
	}
	other, _ := New(Config{Provider: "chatgpt", BaseURL: srv.URL + "/v1", Model: "gpt-z", Tokens: &tokens{}})
	if _, err := other.(LimitReader).Limits(context.Background()); err == nil || !strings.Contains(err.Error(), "gpt-z") {
		t.Errorf("unknown model's limits = %v", err)
	}
	if _, err := New(Config{Provider: "chatgpt", BaseURL: srv.URL, Model: "gpt-b"}); err == nil || !strings.Contains(err.Error(), "signed in") {
		t.Errorf("client without a sign-in = %v", err)
	}
}
