package settings

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestLevelsNormalize(t *testing.T) {
	s := Defaults()
	s.Normalize()
	if s.Review.CommentLevel != "P5" || s.Review.ApprovalLevel != ApprovalOff || s.Review.ApprovalOn() {
		t.Errorf("defaults = %q / %q", s.Review.CommentLevel, s.Review.ApprovalLevel)
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
