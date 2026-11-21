package settings

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestLevelsNormalize(t *testing.T) {
	s := Defaults()
	s.Normalize()
	if s.Review.CommentLevel != "P5" || s.Review.ApprovalLevel != "P3" || s.Review.RequestChangesLevel != "P3" ||
		!s.Review.ApprovalOn() || !s.Review.RequestChangesOn() {
		t.Errorf("defaults = %q / %q / %q", s.Review.CommentLevel, s.Review.ApprovalLevel, s.Review.RequestChangesLevel)
	}
	s.Review.CommentLevel, s.Review.ApprovalLevel = "p3", "p2"
	s.Normalize()
	if s.Review.CommentLevel != "P3" || s.Review.ApprovalLevel != "P2" || !s.Review.ApprovalOn() {
		t.Errorf("lower-case levels = %q / %q", s.Review.CommentLevel, s.Review.ApprovalLevel)
	}
	s.Review.ApprovalLevel = "P5" // broader than the comment level
	s.Normalize()
	if s.Review.ApprovalLevel != "P3" {
		t.Errorf("approval level not clamped: %q", s.Review.ApprovalLevel)
	}
	s.Review.CommentLevel, s.Review.ApprovalLevel = "bogus", "bogus"
	s.Normalize()
	if s.Review.CommentLevel != "P5" || s.Review.ApprovalLevel != ApprovalOff {
		t.Errorf("bogus levels = %q / %q", s.Review.CommentLevel, s.Review.ApprovalLevel)
	}
}

func TestRequestChangesLevelNormalize(t *testing.T) {
	for _, c := range []struct {
		name                  string
		comment, approval, rc string
		wantRC                string
	}{
		{"lower case", "P5", "P3", " p1 ", "P1"},
		{"off", "P5", "P3", "off", LevelOff},
		{"bogus", "P5", "P3", "bogus", LevelOff},
		{"empty", "P5", "P3", "", LevelOff},
		// Broader than the approval level: a P4 finding would request
		// changes yet not hold back approval.
		{"broader than approval", "P5", "P3", "P4", "P3"},
		// Without approvals, only the comment level bounds it.
		{"approval off", "P4", "off", "P4", "P4"},
		{"approval off, broader than comments", "P2", "off", "P5", "P2"},
	} {
		s := Defaults()
		s.Review.CommentLevel, s.Review.ApprovalLevel, s.Review.RequestChangesLevel = c.comment, c.approval, c.rc
		s.Normalize()
		if s.Review.RequestChangesLevel != c.wantRC || s.Review.RequestChangesOn() != (c.wantRC != LevelOff) {
			t.Errorf("%s: request-changes level = %q (on %v), want %q", c.name, s.Review.RequestChangesLevel, s.Review.RequestChangesOn(), c.wantRC)
		}
	}
}

// An organization sets its own approval and request-changes levels; a policy
// saved before the request-changes level existed inherits the instance's.
func TestPolicyLevels(t *testing.T) {
	base := Defaults()
	base.Review.RequestChangesLevel = "P1"
	base.Normalize()

	raw, err := EncodePolicy(base, Policy{Review: Review{CommentLevel: "P4", ApprovalLevel: "P2", RequestChangesLevel: "off"}})
	if err != nil {
		t.Fatal(err)
	}
	p, ok := ParsePolicy(raw)
	if !ok {
		t.Fatalf("policy %s not read back", raw)
	}
	got := base.WithPolicy(p).Review
	if got.ApprovalLevel != "P2" || got.RequestChangesLevel != LevelOff || got.RequestChangesOn() {
		t.Errorf("org levels = approve %q / request changes %q, want P2 / off", got.ApprovalLevel, got.RequestChangesLevel)
	}

	old, ok := ParsePolicy(`{"review":{"comment_level":"P5","approval_level":"P3"}}`)
	if !ok {
		t.Fatal("old policy not read")
	}
	if got := base.WithPolicy(old).Review; got.ApprovalLevel != "P3" || got.RequestChangesLevel != "P1" {
		t.Errorf("old policy = approve %q / request changes %q, want P3 / the instance's P1", got.ApprovalLevel, got.RequestChangesLevel)
	}
}

func TestLegacyMinSeverityMigrates(t *testing.T) {
	for old, want := range map[string]string{"critical": "P1", "major": "P2", "minor": "P4", "nit": "P5"} {
		s := Defaults()
		if err := json.Unmarshal([]byte(`{"review":{"min_severity":"`+old+`"}}`), &s); err != nil {
			t.Fatal(err)
		}
		s.Normalize()
		if s.Review.CommentLevel != want || s.Review.LegacyMinSeverity != "" {
			t.Errorf("min_severity %q -> %q (legacy %q), want %q", old, s.Review.CommentLevel, s.Review.LegacyMinSeverity, want)
		}
		b, _ := json.Marshal(s)
		if strings.Contains(string(b), "min_severity") {
			t.Errorf("legacy key written back: %s", b)
		}
	}
}

func TestMaxRounds(t *testing.T) {
	if d := Defaults(); d.Review.MaxRounds != 10 {
		t.Errorf("default review rounds = %d, want 10", d.Review.MaxRounds)
	}
	for in, want := range map[int]int{0: 5, 3: 5, 5: 5, 12: 12, 30: 30, 31: 30, 500: 30} {
		s := Defaults()
		s.Review.MaxRounds = in
		s.Normalize()
		if s.Review.MaxRounds != want {
			t.Errorf("review rounds %d -> %d, want %d", in, s.Review.MaxRounds, want)
		}
	}

	// An organization sets its own; a policy saved before the setting
	// existed inherits the instance's.
	base := Defaults()
	base.Review.MaxRounds = 20
	base.Normalize()
	raw, err := EncodePolicy(base, Policy{Review: Review{MaxRounds: 7}})
	if err != nil {
		t.Fatal(err)
	}
	if p, ok := ParsePolicy(raw); !ok || base.WithPolicy(p).Review.MaxRounds != 7 {
		t.Errorf("org review rounds from %s = %d, want 7", raw, base.WithPolicy(p).Review.MaxRounds)
	}
	old, _ := ParsePolicy(`{"review":{"comment_level":"P5"}}`)
	if got := base.WithPolicy(old).Review.MaxRounds; got != 20 {
		t.Errorf("old policy's review rounds = %d, want the instance's 20", got)
	}
}
