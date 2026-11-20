package bot

import (
	"strconv"
	"strings"
	"testing"

	"github.com/OMG-Software/omg-review-bot/internal/diff"
	"github.com/OMG-Software/omg-review-bot/internal/github"
	"github.com/OMG-Software/omg-review-bot/internal/settings"
)

func TestGlobs(t *testing.T) {
	cases := []struct {
		pat, path string
		want      bool
	}{
		{"*.lock", "Cargo.lock", true},
		{"*.lock", "sub/dir/yarn.lock", true},
		{"go.sum", "svc/go.sum", true},
		{"go.sum", "go.summary", false},
		{"vendor/", "vendor/x/y.go", true},
		{"vendor/", "pkg/vendor/x.go", true},
		{"vendor/", "vendored.go", false},
		{"docs/*.md", "docs/a.md", true},
		{"docs/*.md", "x/docs/a.md", false},
		{"**/gen/**", "a/gen/b/c.go", true},
		{"*.min.js", "static/app.min.js", true},
		{"*.min.js", "static/app.js", false},
	}
	for _, c := range cases {
		if got := matchAny(compileGlobs([]string{c.pat}), c.path); got != c.want {
			t.Errorf("%q vs %q = %v, want %v", c.pat, c.path, got, c.want)
		}
	}
}

func TestBuildPlanSplitsLargeDiffs(t *testing.T) {
	var patch strings.Builder
	for h := 0; h < 20; h++ {
		patch.WriteString("@@ -1,1 +" + itoa(h*100+1) + ",3 @@\n")
		for i := 0; i < 3; i++ {
			patch.WriteString("+" + strings.Repeat("x", 100) + "\n")
		}
	}
	files := []github.PRFile{
		{Filename: "big.go", Status: "modified", Patch: patch.String()},
		{Filename: "small.go", Status: "added", Patch: "@@ -0,0 +1 @@\n+hi"},
		{Filename: "old.go", Status: "removed", Patch: "@@ -1 +0,0 @@\n-bye"},
		{Filename: "img.png", Status: "added"},
	}
	p := buildPlan(settings.Defaults(), files, 2000)
	if len(p.batches) < 3 {
		t.Fatalf("expected the large file to be split, got %d batches", len(p.batches))
	}
	for i, b := range p.batches {
		if len(b.text) > 2000+400 {
			t.Errorf("batch %d is %d chars, over budget", i, len(b.text))
		}
	}
	if p.reviewed != 2 || len(p.skipped) != 2 {
		t.Errorf("reviewed=%d skipped=%v", p.reviewed, p.skipped)
	}
}

func itoa(n int) string { return strconv.Itoa(n) }

func TestPrepareFindings(t *testing.T) {
	df := diff.Parse("pkg/a.go", "@@ -1,2 +1,3 @@\n x\n+y\n z")
	p := plan{files: map[string]*diff.File{"pkg/a.go": df}}
	st := settings.Defaults()
	st.Review.MaxComments = 3
	line := 2
	mine := []github.ReviewComment{{Path: "pkg/a.go", Line: &line, Body: "🟠 **Possible nil pointer dereference**"}}
	all := []finding{
		{Path: "a.go", Line: 2, Severity: "HIGH", Title: "SQL injection", Body: "..."},            // suffix path match, severity mapped
		{Path: "pkg/a.go", Line: 5, Severity: "minor", Title: "Near line", Body: "..."},           // snapped to 3
		{Path: "pkg/a.go", Line: 2, Severity: "major", Title: "Possible nil pointer dereference"}, // duplicate of earlier comment
		{Path: "other.go", Line: 1, Severity: "minor", Title: "Elsewhere", Body: "..."},           // not in diff -> note
		{Path: "pkg/a.go", Line: 1, Severity: "nit", Title: "Over the cap", Body: "..."},
	}
	inline, notes, dropped := prepareFindings(st, p, all, mine)
	if len(inline) != 2 || inline[0].Severity != "major" || inline[0].Path != "pkg/a.go" || inline[1].Line != 3 {
		t.Errorf("inline = %+v", inline)
	}
	if len(notes) != 1 || notes[0].Title != "Elsewhere" {
		t.Errorf("notes = %+v", notes)
	}
	if dropped != 2 {
		t.Errorf("dropped = %d, want 2 (one duplicate, one over the cap)", dropped)
	}
}

func TestMentionRegexp(t *testing.T) {
	re := mentionRE("testbot")
	for s, want := range map[string]bool{
		"@testbot can you explain?": true,
		"hey @TestBot[bot] why":     true,
		"cc @testbot-other":         false,
		"email me@testbot":          false,
		"@testbotx":                 false,
	} {
		if re.MatchString(s) != want {
			t.Errorf("%q: got %v want %v", s, !want, want)
		}
	}
}
