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

func TestLimits(t *testing.T) {
	serve := func(routes map[string]string) *httptest.Server {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			body, ok := routes[r.Method+" "+r.URL.Path]
			if !ok {
				http.NotFound(w, r)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(body))
		}))
		t.Cleanup(srv.Close)
		return srv
	}
	for _, c := range []struct {
		name, provider, path string
		routes               map[string]string
		want                 Limits
		wantErr              bool
	}{
		{"LM Studio", "lmstudio", "/v1", map[string]string{
			"GET /api/v0/models/qwen/coder": `{"id":"qwen/coder","max_context_length":131072}`,
		}, Limits{Context: 131072}, false},
		// Loaded in a smaller window than the model's maximum: reviews must
		// fit the loaded one.
		{"LM Studio v0, loaded", "lmstudio", "/v1", map[string]string{
			"GET /api/v0/models/qwen/coder": `{"id":"qwen/coder","state":"loaded","max_context_length":131072,"loaded_context_length":32768}`,
		}, Limits{Context: 32768}, false},
		{"LM Studio v1, loaded", "lmstudio", "/v1", map[string]string{
			"GET /api/v1/models": `{"models":[{"key":"other","max_context_length":8},{"key":"qwen/coder","max_context_length":131072,"loaded_instances":[{"id":"qwen/coder","config":{"context_length":65536}},{"id":"qwen/coder:2","config":{"context_length":16384}}]}]}`,
		}, Limits{Context: 16384}, false},
		{"LM Studio v1, not loaded", "lmstudio", "/v1", map[string]string{
			"GET /api/v1/models": `{"models":[{"key":"qwen/coder","max_context_length":131072,"loaded_instances":[]}]}`,
		}, Limits{Context: 131072}, false},
		{"OpenRouter", "openai", "/api/v1", map[string]string{
			"GET /api/v1/models": `{"data":[{"id":"other","context_length":1},{"id":"qwen/coder","context_length":262144,"top_provider":{"context_length":262144,"max_completion_tokens":65536}}]}`,
		}, Limits{Context: 262144, Output: 65536}, false},
		{"vLLM", "openai", "/v1", map[string]string{
			"GET /v1/models": `{"data":[{"id":"qwen/coder","max_model_len":32768}]}`,
		}, Limits{Context: 32768}, false},
		{"llama.cpp", "openai", "/v1", map[string]string{
			"GET /v1/models": `{"data":[{"id":"qwen/coder"}]}`,
			"GET /props":     `{"default_generation_settings":{"n_ctx":16384}}`,
		}, Limits{Context: 16384}, false},
		{"not reported", "openai", "/v1", map[string]string{
			"GET /v1/models": `{"data":[{"id":"qwen/coder"}]}`,
		}, Limits{}, true},
		{"Ollama", "ollama", "", map[string]string{
			"POST /api/show": `{"model_info":{"general.architecture":"qwen3","qwen3.context_length":40960,"qwen3.embedding_length":4096}}`,
		}, Limits{Context: 40960}, false},
		{"Ollama, a vision model", "ollama", "", map[string]string{
			"POST /api/show": `{"model_info":{"general.architecture":"gemma3","clip.vision.context_length":1024,"gemma3.context_length":131072,"zzz.context_length":8}}`,
		}, Limits{Context: 131072}, false},
		{"Ollama, no architecture", "ollama", "", map[string]string{
			"POST /api/show": `{"model_info":{"b.context_length":2048,"a.context_length":4096}}`,
		}, Limits{Context: 4096}, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			srv := serve(c.routes)
			cl, err := New(Config{Provider: c.provider, BaseURL: srv.URL + c.path, Model: "qwen/coder"})
			if err != nil {
				t.Fatal(err)
			}
			got, err := cl.(LimitReader).Limits(context.Background())
			if (err != nil) != c.wantErr || got != c.want {
				t.Errorf("Limits() = %+v, %v; want %+v (error: %v)", got, err, c.want, c.wantErr)
			}
		})
	}
}
