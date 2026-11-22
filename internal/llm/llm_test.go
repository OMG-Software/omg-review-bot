package llm

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

// Prompts (the PR's code) and the API key go only to the configured server.
func TestPromptStaysOnLLMServer(t *testing.T) {
	var elsewhere atomic.Int32
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		elsewhere.Add(1)
	}))
	defer other.Close()
	llmSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, other.URL+r.URL.Path, http.StatusTemporaryRedirect)
	}))
	defer llmSrv.Close()

	c, err := New(Config{BaseURL: llmSrv.URL + "/v1", Model: "m", APIKey: "k"})
	if err != nil {
		t.Fatal(err)
	}
	_, err = c.Chat(context.Background(), Request{Messages: []Message{{Role: "user", Content: "secret code"}}})
	if err == nil || !strings.Contains(err.Error(), "refusing redirect") {
		t.Errorf("err = %v, want a refused redirect", err)
	}
	if n := elsewhere.Load(); n != 0 {
		t.Errorf("prompt reached another host %d time(s)", n)
	}
	for _, u := range []string{"file:///etc/passwd", "ftp://host/v1", "gopher://host"} {
		if _, err := New(Config{BaseURL: u}); err == nil {
			t.Errorf("base URL %q was accepted", u)
		}
	}
}

func TestDecodeJSON(t *testing.T) {
	type out struct {
		Summary string `json:"summary"`
	}
	for name, in := range map[string]string{
		"plain":       `{"summary":"ok"}`,
		"fenced":      "Here you go:\n```json\n{\"summary\":\"ok\"}\n```",
		"thinking":    "<think>hmm {not json}</think>{\"summary\":\"ok\"}",
		"close only":  "reasoning {x} </think>\n{\"summary\":\"ok\"}",
		"brace noise": "use {braces} wisely {\"summary\":\"ok\"} trailing",
	} {
		var v out
		if err := DecodeJSON(in, &v); err != nil || v.Summary != "ok" {
			t.Errorf("%s: got %+v, %v", name, v, err)
		}
	}
	var v out
	if err := DecodeJSON("no json here", &v); err == nil {
		t.Error("expected error for non-JSON")
	}
}
