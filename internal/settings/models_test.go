package settings

import (
	"encoding/json"
	"testing"
)

// TestSingleModelMigrates: settings saved before there could be several
// models keep theirs, as the only one in the set.
func TestSingleModelMigrates(t *testing.T) {
	v := Defaults()
	if err := json.Unmarshal([]byte(`{"llm":{"provider":"ollama","base_url":"http://gpu:11434","model":"qwen","context_tokens":16384}}`), &v); err != nil {
		t.Fatal(err)
	}
	v.Normalize()
	m := v.Models
	if len(m.Configs) != 1 || m.Configs[0].ID != "m1" || m.Configs[0].Model != "qwen" || m.Configs[0].BaseURL != "http://gpu:11434" ||
		m.Strategy != StrategySingle || m.Active != "m1" || v.LLM.Model != "qwen" || v.Limits.OrgSlotPercent != DefaultOrgSlotPercent {
		t.Errorf("migrated settings = %+v", v)
	}
}

func TestModelsNormalize(t *testing.T) {
	m := Models{Strategy: "bogus", Active: "gone", Configs: []LLM{
		{ID: "m2", Model: "a"}, {ID: "m2", Model: "b"}, {Model: "c", ContextTokens: 1, MaxOutputTokens: 1 << 30},
	}}
	m.Normalize()
	ids := []string{m.Configs[0].ID, m.Configs[1].ID, m.Configs[2].ID}
	if ids[0] != "m2" || ids[1] != "m1" || ids[2] != "m3" {
		t.Errorf("IDs = %v, want unique m2 m1 m3", ids)
	}
	if m.Strategy != StrategySingle || m.Active != "m2" || m.Primary().Model != "a" || len(m.InUse()) != 1 {
		t.Errorf("strategy/active = %q/%q", m.Strategy, m.Active)
	}
	if c := m.Configs[2]; c.ContextTokens != 4096 || c.MaxOutputTokens != 2048 {
		t.Errorf("limits not clamped: %+v", c)
	}
	m.Strategy = StrategyCollate
	if len(m.InUse()) != 3 || m.Primary().Model != "a" {
		t.Errorf("collate uses %d models", len(m.InUse()))
	}
	if got := Tightest(m.InUse()); got.Model != "c" {
		t.Errorf("tightest = %s, want c", got.Model)
	}
}

func TestOrgSlots(t *testing.T) {
	for _, tc := range []struct{ pct, want int }{{50, 2}, {25, 1}, {1, 1}, {75, 3}, {100, 4}} {
		if got := (Limits{OrgSlotPercent: tc.pct}).OrgSlots(4); got != tc.want {
			t.Errorf("%d%% of 4 slots = %d, want %d", tc.pct, got, tc.want)
		}
	}
}

func TestCheckOwn(t *testing.T) {
	ok := LLM{BaseURL: "https://api.openai.com/v1", Model: "gpt"}
	if err := CheckOwn(ok); err != nil {
		t.Errorf("public https model refused: %v", err)
	}
	for _, bad := range []LLM{
		{BaseURL: "https://api.openai.com/v1"},
		{BaseURL: "http://api.openai.com/v1", Model: "gpt"},
		{BaseURL: "https://192.168.1.2/v1", Model: "gpt"},
		{BaseURL: "https://localhost/v1", Model: "gpt"},
		{Model: "gpt"},
		{Provider: "ollama-cloud", BaseURL: "https://ollama.com", Model: "gpt"}, // no API key
	} {
		if CheckOwn(bad) == nil {
			t.Errorf("CheckOwn(%+v) accepted", bad)
		}
	}
}

func TestSetMaximums(t *testing.T) {
	for _, c := range []struct {
		name             string
		ctxMax, outMax   int
		setCtx, setOut   bool
		wantCtx, wantOut int
	}{
		{"both reported", 200000, 64000, true, true, 200000, 64000},
		{"window only: half for output", 262144, 0, true, true, 262144, 131072},
		{"output cap above half the window", 32768, 32768, true, true, 32768, 16384},
		{"nothing reported", 0, 0, true, true, FallbackContextTokens, FallbackOutputTokens},
		{"only output blank", 0, 8192, false, true, 50000, 8192},
		{"only context blank", 128000, 0, true, false, 128000, 1000},
		// The context kept (50000 here, below; 131072 in the review) is
		// larger than the server's window: output is half the server's.
		{"only output blank, smaller window", 40960, 0, false, true, 50000, 20480},
	} {
		t.Run(c.name, func(t *testing.T) {
			l := NewLLM()
			l.ContextTokens, l.MaxOutputTokens = 50000, 1000
			l.SetMaximums(c.ctxMax, c.outMax, c.setCtx, c.setOut)
			if l.ContextTokens != c.wantCtx || l.MaxOutputTokens != c.wantOut {
				t.Errorf("limits = %d/%d, want %d/%d", l.ContextTokens, l.MaxOutputTokens, c.wantCtx, c.wantOut)
			}
		})
	}
	if d := NewLLM(); d.ContextTokens != FallbackContextTokens || d.MaxOutputTokens != FallbackOutputTokens {
		t.Errorf("a new model's limits = %d/%d", d.ContextTokens, d.MaxOutputTokens)
	}
}
