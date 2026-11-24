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
	inline, notes, held, dropped := prepareFindings(st, p, all, mine, st.Review.MaxComments)
	if len(inline) != 2 || inline[0].Severity != "P2" || inline[0].Path != "pkg/a.go" || inline[1].Line != 3 {
		t.Errorf("inline = %+v", inline)
	}
	if len(notes) != 1 || notes[0].Title != "Elsewhere" {
		t.Errorf("notes = %+v", notes)
	}
	if len(held) != 1 || held[0].Title != "Over the cap" || held[0].Severity != "P5" {
		t.Errorf("held = %+v, want the nit over the cap", held)
	}
	if dropped != 1 {
		t.Errorf("dropped = %d, want 1 (the duplicate)", dropped)
	}

	// With no limit (the last automatic review), nothing is held.
	inline, notes, held, _ = prepareFindings(st, p, all, mine, 0)
	if len(held) != 0 || len(inline)+len(notes) != 4 {
		t.Errorf("unlimited: inline %d, notes %d, held %d; want 4 posted, none held", len(inline), len(notes), len(held))
	}

	// A finding reworded by a later pass, on the same line, is the same finding.
	again := append(all, finding{Path: "pkg/a.go", Line: 2, Severity: "P2", Title: "SQL injection risk", Body: "..."})
	inline, notes, held, _ = prepareFindings(st, p, again, mine, 0)
	if n := len(inline) + len(notes) + len(held); n != 4 {
		t.Errorf("reworded duplicate: %d findings kept, want 4", n)
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

func TestNormSeverity(t *testing.T) {
	for in, want := range map[string]string{
		"P1": "P1", "p4": "P4", "[P3] MINOR (Defect)": "P3", "MINOR (Quality)": "P4", "critical": "P1",
		"HIGH": "P2", "minor": "P3", "nitpick": "P5", "maintainability": "P4", "whatever": "P3",
	} {
		if got := normSeverity(in); got != want {
			t.Errorf("normSeverity(%q) = %s, want %s", in, got, want)
		}
	}
}

func TestFindingCommentRoundTrip(t *testing.T) {
	body := commentBody(finding{Severity: "P4", Title: "Duplicated parsing logic", Body: "**Issue:** x"})
	if !strings.HasPrefix(body, "**[P4] MINOR (Quality)** Duplicated parsing logic\n") {
		t.Errorf("comment = %q", body)
	}
	if s := findingSeverity(body); s != "P4" {
		t.Errorf("severity = %q", s)
	}
	if s := findingTitle(body); s != "Duplicated parsing logic" {
		t.Errorf("title = %q", s)
	}
	// Comments from before the P1..P5 scale.
	if s, ti := findingSeverity("🟠 **Possible nil dereference**\n\nx"), findingTitle("🟠 **Possible nil dereference**"); s != "P2" || ti != "Possible nil dereference" {
		t.Errorf("legacy = %q / %q", s, ti)
	}
}

func TestBlockingCounts(t *testing.T) {
	st := settings.Defaults()
	st.Review.ApprovalLevel = "P3"
	mk := func(sev string, resolved bool) botThread {
		return botThread{root: &github.ReviewComment{Body: commentBody(finding{Severity: sev, Title: "t"})}, resolved: resolved}
	}
	threads := []botThread{mk("P1", true), mk("P2", false), mk("P3", false), mk("P4", false),
		{root: &github.ReviewComment{Body: "no severity at all"}}} // unreadable: counts, to be safe
	if n := countOpen(st.Review.ApprovalLevel, threads); n != 3 {
		t.Errorf("blocking threads = %d, want 3", n)
	}
	if n := countFindings(st.Review.ApprovalLevel, []finding{{Severity: "P3"}, {Severity: "P5"}}); n != 1 {
		t.Errorf("blocking findings = %d, want 1", n)
	}
	// A level that's off is reached by nothing, not even an unreadable finding.
	if n := countOpen(settings.LevelOff, threads); n != 0 {
		t.Errorf("threads open at an off level = %d, want 0", n)
	}
	if n := countLevels("P2", []string{"P1", "P2", "P3", "?"}); n != 3 {
		t.Errorf("levels at P2 or above = %d, want 3 (the unreadable one counts)", n)
	}
	sevs, ok := noteSeverities(reviewBody(reviewInfo{notes: []finding{{Severity: "P2"}, {Severity: "P5"}}}))
	if !ok || strings.Join(sevs, ",") != "P2,P5" {
		t.Errorf("note severities = %v %v", sevs, ok)
	}
	if _, ok := noteSeverities("<!-- prbot:approval -->"); ok {
		t.Error("approval body read as a review")
	}
}

func TestReviewEvent(t *testing.T) {
	st := settings.Defaults() // each case sets both levels
	for _, c := range []struct {
		name                 string
		apLevel, rcLevel     string
		blocking, requesting int
		want, verdict        string
	}{
		{"P2 open", "P3", "P2", 1, 1, "REQUEST_CHANGES", "Changes requested:** 1 unresolved finding(s) at [P2] MAJOR"},
		{"only P3 open", "P3", "P2", 1, 0, "COMMENT", "Not approved:** 1 unresolved finding(s) at [P3] MINOR (Defect)"},
		{"nothing open", "P3", "P2", 0, 0, "APPROVE", "Approved:** no unresolved findings at [P3]"},
		{"approval off", "off", "P2", 0, 0, "COMMENT", ""},
		{"approval off, P1 open", "off", "P2", 0, 2, "REQUEST_CHANGES", "Changes requested:** 2 unresolved"},
		{"request changes off", "P3", "off", 1, 0, "COMMENT", "Not approved:** 1"},
	} {
		st.Review.ApprovalLevel, st.Review.RequestChangesLevel = c.apLevel, c.rcLevel
		got := reviewEvent(st, c.blocking, c.requesting)
		if got != c.want {
			t.Errorf("%s: event = %s, want %s", c.name, got, c.want)
		}
		v := verdictLine(st, got, c.blocking, c.requesting)
		if (c.verdict == "") != (v == "") || !strings.Contains(v, c.verdict) {
			t.Errorf("%s: verdict = %q, want it to contain %q", c.name, v, c.verdict)
		}
	}
}

func TestFileView(t *testing.T) {
	var sb strings.Builder
	for i := 1; i <= 500; i++ {
		sb.WriteString("line " + itoa(i) + "\n")
	}
	if v := fileView("a\nb", nil, 1000); v != "    1 | a\n    2 | b\n" {
		t.Errorf("small file = %q", v)
	}
	v := fileView(sb.String(), []int{250, 255}, 8000)
	if !strings.Contains(v, "  250 | line 250") || strings.Contains(v, "  100 | line 100") || !strings.Contains(v, "  ... |") {
		t.Errorf("excerpt view wrong:\n%s", v)
	}
}

func TestProjectDocScoping(t *testing.T) {
	changed := []string{"main.go", "svc/api/handler.go"}
	for doc, want := range map[string]bool{
		"CLAUDE.md":                 true,
		"CONTRIBUTING.md":           true,
		"docs/CONVENTIONS.md":       true, // not a scoped doc: applies everywhere
		"svc/CLAUDE.md":             true,
		"svc/api/AGENTS.md":         true,
		"svc/web/CLAUDE.md":         false,
		"svcx/CLAUDE.md":            false, // prefix of a directory name, not a parent
		"svc/api/handler/CLAUDE.md": false,
	} {
		if got := docApplies(doc, changed); got != want {
			t.Errorf("docApplies(%q) = %v, want %v", doc, got, want)
		}
	}

	docs := []projectDoc{
		{Path: "CLAUDE.md", Content: strings.Repeat("a", 3000)},
		{Path: "svc/web/CLAUDE.md", Content: "web only"},
		{Path: "CONTRIBUTING.md", Content: strings.Repeat("b", 3000)},
		{Path: "svc/CLAUDE.md", Content: strings.Repeat("c", 100)},
	}
	text, used, left := selectDocs(docs, changed, 5000)
	if strings.Join(used, ",") != "CLAUDE.md,CONTRIBUTING.md" || strings.Join(left, ",") != "svc/CLAUDE.md" {
		t.Errorf("used=%v left=%v", used, left)
	}
	if len(text) > 5000 || !strings.Contains(text, "(truncated to fit the model context)") || strings.Contains(text, "web only") {
		t.Errorf("selected text (%d chars) wrong", len(text))
	}
	if guidelinesPrompt("main", "") != "" {
		t.Error("no docs should add no prompt section")
	}
}
