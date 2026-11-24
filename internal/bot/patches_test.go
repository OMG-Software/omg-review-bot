package bot

import (
	"strings"
	"testing"
)

// GitHub sends no patch for the files past the first megabyte or so of a
// large pull request's diff. The review rebuilds them from the files'
// contents at the merge base and head, so they're reviewed like the rest; a
// binary file is still skipped, and says why.
func TestRebuildsPatchesGitHubLeftOut(t *testing.T) {
	e := newReviewEnv(t, reviewOf(), noApproval)
	e.gh.mergeBase = "mmm333"
	// As GitHub lists them past the cut: no patch, no line counts.
	e.gh.extraFiles = []any{
		map[string]any{"filename": "late.go", "status": "modified"},
		map[string]any{"filename": "fresh.go", "status": "added"},
		map[string]any{"filename": "logo.png", "status": "modified"},
	}
	e.gh.fileAt = map[string]string{
		"mmm333:late.go":  "package late\n\nfunc A() {}\n",
		"aaa111:late.go":  "package late\n\nfunc A() {}\n\nfunc Late() { panic(1) }\n",
		"aaa111:fresh.go": "package fresh\n",
		"mmm333:logo.png": "\x89PNG\x00",
		"aaa111:logo.png": "\x89PNG\x00\x01",
	}
	e.b.pollOnce(e.ctx)
	drain(e.ctx, e.b)

	prompts := strings.Join(e.lm.prompts["code_review"], "\n")
	for _, want := range []string{
		"### File: late.go (modified, +2 -0)",
		"     5 + func Late() { panic(1) }",
		"### File: fresh.go (added, +1 -0)",
		"     1 + package fresh",
	} {
		if !strings.Contains(prompts, want) {
			t.Errorf("review prompt lacks %q:\n%s", want, prompts)
		}
	}
	if strings.Contains(prompts, "logo.png") {
		t.Errorf("a binary file was sent for review:\n%s", prompts)
	}
	if len(e.gh.reviews) != 1 {
		t.Fatalf("want 1 review, got %d", len(e.gh.reviews))
	}
	body := e.gh.reviews[0].Body
	if !strings.Contains(body, "logo.png (binary)") || strings.Contains(body, "late.go (") || strings.Contains(body, "fresh.go (") {
		t.Errorf("files not reviewed should list only logo.png, as binary:\n%s", body)
	}
}

// Without the merge base, a modified file can't be diffed, but an added one
// still can: its whole content is the change.
func TestRebuildsAddedFilesWithoutMergeBase(t *testing.T) {
	e := newReviewEnv(t, reviewOf(), noApproval)
	e.gh.extraFiles = []any{
		map[string]any{"filename": "late.go", "status": "modified"},
		map[string]any{"filename": "fresh.go", "status": "added"},
	}
	e.gh.fileAt = map[string]string{"aaa111:fresh.go": "package fresh\n"}
	e.b.pollOnce(e.ctx)
	drain(e.ctx, e.b)

	prompts := strings.Join(e.lm.prompts["code_review"], "\n")
	if !strings.Contains(prompts, "     1 + package fresh") || strings.Contains(prompts, "late.go") {
		t.Errorf("want fresh.go reviewed and late.go not:\n%s", prompts)
	}
	if len(e.gh.reviews) != 1 || !strings.Contains(e.gh.reviews[0].Body, "late.go (GitHub sent no diff, and its contents couldn't be read)") {
		t.Errorf("late.go should be listed as unread: %+v", e.gh.reviews)
	}
}
