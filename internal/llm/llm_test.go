package llm

import "testing"

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
