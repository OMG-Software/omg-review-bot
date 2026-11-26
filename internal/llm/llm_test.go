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
		// /props describes the loaded model: another model's window isn't
		// taken for this one's, nor one of several listed.
		{"llama.cpp, another model loaded", "openai", "/v1", map[string]string{
			"GET /v1/models": `{"data":[{"id":"other-8b"}]}`,
			"GET /props":     `{"default_generation_settings":{"n_ctx":8192}}`,
		}, Limits{}, true},
		{"llama.cpp, several models", "openai", "/v1", map[string]string{
			"GET /v1/models": `{"data":[{"id":"other-8b"},{"id":"qwen/coder"}]}`,
			"GET /props":     `{"default_generation_settings":{"n_ctx":8192}}`,
		}, Limits{}, true},
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

	// A name the model list doesn't have is said to be missing, not quiet;
	// one listed without limits is "not reported".
	for name, list := range map[string]string{"no model named \"qwen/coder\"": `{"data":[{"id":"qwen/coderr"}]}`, "doesn't report": `{"data":[{"id":"qwen/coder"}]}`} {
		srv := serve(map[string]string{"GET /v1/models": list})
		cl, _ := New(Config{Provider: "openai", BaseURL: srv.URL + "/v1", Model: "qwen/coder"})
		if _, err := cl.(LimitReader).Limits(context.Background()); err == nil || !strings.Contains(err.Error(), name) {
			t.Errorf("list %s: error %v, want one saying %q", list, err, name)
		}
	}
}

// An object that is JSON but doesn't fit is skipped whole: an object nested
// in it isn't taken as the answer.
func TestDecodeJSONSkipsNestedObjects(t *testing.T) {
	type out struct {
		Summary  string           `json:"summary"`
		Comments []map[string]any `json:"comments"`
	}
	var v out
	if err := DecodeJSON(`{"summary":"x","comments":{"path":"a.go","summary":"inner"}}`, &v); err == nil {
		t.Errorf("decoded %+v from an object whose comments aren't a list", v)
	}
	v = out{}
	if err := DecodeJSON(`{"comments":{"a":1}} then {"summary":"ok","comments":[]}`, &v); err != nil || v.Summary != "ok" {
		t.Errorf("a later top-level object should still be found: %+v, %v", v, err)
	}
}

// Synthetic reviews with the defects models produce: a comma left after the
// comments list or inside it, and a quote not escaped inside a string. Each
// broken object has well-formed findings in it, which must never be taken as
// the answer (they'd read as a review with no summary and no findings).
const (
	finding1 = `{"path":"main.go","line":11,"severity":"P2","title":"Division by zero","body":"Always panics."}`
	finding2 = `{"path":"main.go","line":12,"severity":"P3","title":"Printed, not logged","body":"Use the logger."}`

	commaAfterList = `{"summary":"Adds a division.","comments":[` + finding1 + `,` + finding2 + `],}`
	commaInList    = "{\"summary\":\"Adds a division.\",\"comments\":[\n" + finding1 + ",\n" + finding2 + ", ], }"
	unescapedQuote = `{"summary":"Adds a division.","comments":[` + finding1 +
		`,{"path":"main.go","line":12,"severity":"P3","title":"Printed","body":"Call "log.Printf" instead."}]}`
)

type review struct {
	Summary  string           `json:"summary"`
	Comments []map[string]any `json:"comments"`
}

var reviewKeys = []string{"summary", "comments"}

func TestDecodeRequiredRepairsTrailingCommas(t *testing.T) {
	for name, in := range map[string]string{
		"comma after the list": commaAfterList,
		"comma in the list":    commaInList,
		"fenced, with chatter": "Here it is:\n```json\n" + commaAfterList + "\n```\nDone.",
	} {
		var v review
		if err := DecodeRequired(in, reviewKeys, &v); err != nil || v.Summary != "Adds a division." || len(v.Comments) != 2 {
			t.Errorf("%s: got %+v, %v; want the summary and both findings", name, v, err)
		}
	}
	// Only commas outside strings go: text that looks like one stays.
	var v review
	if err := DecodeRequired(`{"summary":"keep ,] and \",}\" here","comments":[],}`, reviewKeys, &v); err != nil || v.Summary != `keep ,] and ",}" here` {
		t.Errorf("summary = %q, %v; want the string as written", v.Summary, err)
	}
}

func TestDecodeRequiredRejectsBrokenObjects(t *testing.T) {
	for name, c := range map[string]struct {
		in, wantErr string
		keysOnly    bool // valid JSON, refused only for the required keys
	}{
		// The decoder's error, and where: the retry tells the model.
		"unescaped quote": {in: unescapedQuote, wantErr: "\"body\":\"Call \"l`"},
		"cut off":         {in: `{"summary":"Adds a division.","comments":[` + finding1 + `,{"path":"ma`, wantErr: "incomplete"},
		// A fragment with every required field inside a broken object is
		// still a fragment.
		"fragment with the fields": {in: `{"summary":"Outer.","comments":[{"summary":"Inner.","comments":[]}],"note":"say "hi""}`, wantErr: "just after"},
		// Something is missing before that comma: it isn't repaired into an
		// empty list.
		"orphan comma":    {in: `{"summary":"Adds a division.","comments":[,]}`, wantErr: "looking for beginning of value"},
		"missing field":   {in: `{"summary":"Adds a division."}`, wantErr: `no "comments" field`, keysOnly: true},
		"null list":       {in: `{"summary":"Adds a division.","comments":null}`, wantErr: `"comments" field is null`, keysOnly: true},
		"a finding alone": {in: finding1, wantErr: `no "summary" field`, keysOnly: true},
	} {
		v := review{}
		err := DecodeRequired(c.in, reviewKeys, &v)
		if err == nil || !strings.Contains(err.Error(), c.wantErr) {
			t.Errorf("%s: error %v, want one containing %q", name, err, c.wantErr)
		}
		if v.Summary != "" || v.Comments != nil {
			t.Errorf("%s: decoded %+v from a broken answer", name, v)
		}
		// Without required keys too: a finding must not stand in for a
		// broken review, whatever the caller checks.
		v = review{}
		if !c.keysOnly {
			if err := DecodeJSON(c.in, &v); err == nil {
				t.Errorf("%s: DecodeJSON took %+v from a broken object", name, v)
			}
		}
	}
	// A later top-level object is still found past one without the fields,
	// and past braces in prose.
	for _, in := range []string{
		`{"comments":{"a":1}} then {"summary":"ok","comments":[]}`,
		`{"analysis":"x"} {"summary":"ok","comments":[]}`,
		`use {braces} wisely {"summary":"ok","comments":[]}`,
	} {
		var v review
		if err := DecodeRequired(in, reviewKeys, &v); err != nil || v.Summary != "ok" {
			t.Errorf("%s: got %+v, %v", in, v, err)
		}
	}
}

func TestDropTrailingCommas(t *testing.T) {
	for in, want := range map[string]string{
		`[1,2,]`:                    `[1,2]`,
		"{\"a\":[1,\n ],\n}":        "{\"a\":[1\n ]\n}",
		`{"a":"x,]","b":"\",}"}`:    `{"a":"x,]","b":"\",}"}`,
		`{"a":"\\",}`:               `{"a":"\\"}`, // an escaped backslash ends the string
		`{"a":1,"b":2}`:             `{"a":1,"b":2}`,
		`{"a":[{"b":1},{"c":2},],}`: `{"a":[{"b":1},{"c":2}]}`,
		`{"a":"x",}`:                `{"a":"x"}`, // after a string
		// No value before the comma: left for the decoder to refuse.
		`[,]`:         `[,]`,
		`{"a":[ , ]}`: `{"a":[ , ]}`,
		`{,}`:         `{,}`,
		`[1,,]`:       `[1,,]`,
	} {
		if got := dropTrailingCommas(in); got != want {
			t.Errorf("dropTrailingCommas(%q) = %q, want %q", in, got, want)
		}
	}
}
