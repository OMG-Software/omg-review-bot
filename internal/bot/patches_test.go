package bot

import (
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/OMG-Software/omg-review-bot/internal/github"
	"github.com/OMG-Software/omg-review-bot/internal/settings"
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
		map[string]any{"filename": "moved.go", "status": "renamed", "previous_filename": "old.go"},
	}
	e.gh.fileAt = map[string]string{
		"mmm333:old.go":   "package moved\n",
		"aaa111:moved.go": "package moved\n\nfunc Moved() {}\n",
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
		// A renamed file is diffed against its old name at the merge base.
		"### File: moved.go (renamed from old.go, +2 -0)",
		"     3 + func Moved() {}",
	} {
		if !strings.Contains(prompts, want) {
			t.Errorf("review prompt lacks %q:\n%s", want, prompts)
		}
	}
	if strings.Contains(prompts, "logo.png") {
		t.Errorf("a binary file was sent for review:\n%s", prompts)
	}
	if slices.Contains(e.gh.fileReads, "mmm333:moved.go") {
		t.Errorf("read a renamed file under its new name at the merge base: %v", e.gh.fileReads)
	}
	if len(e.gh.reviews) != 1 {
		t.Fatalf("want 1 review, got %d", len(e.gh.reviews))
	}
	body := e.gh.reviews[0].Body
	if !strings.Contains(body, "logo.png (binary)") || strings.Contains(body, "late.go (") || strings.Contains(body, "fresh.go (") {
		t.Errorf("files not reviewed should list only logo.png, as binary:\n%s", body)
	}
}

// A file that can't be rebuilt (binary, or too large) doesn't take one of the
// review's file slots: the next is rebuilt in its place. A file too large at
// head is known from its first megabyte, and its old version isn't fetched.
func TestFailedRebuildsLeaveRoom(t *testing.T) {
	e := newReviewEnv(t, reviewOf(), func(cfg *settings.Settings) {
		noApproval(cfg)
		cfg.Review.MaxFiles = 2 // main.go, and one more
	})
	e.gh.mergeBase = "mmm333"
	e.gh.extraFiles = []any{
		map[string]any{"filename": "blob", "status": "modified"}, // binary by its contents
		map[string]any{"filename": "huge.go", "status": "modified"},
		map[string]any{"filename": "late.go", "status": "modified"},
		map[string]any{"filename": "extra.go", "status": "modified"},
	}
	e.gh.fileAt = map[string]string{
		"aaa111:blob":     "\x7fELF\x00\x01",
		"aaa111:huge.go":  strings.Repeat("// padding line\n", 70000),
		"mmm333:late.go":  "package late\n",
		"aaa111:late.go":  "package late\n\nfunc Late() {}\n",
		"mmm333:extra.go": "package extra\n",
		"aaa111:extra.go": "package extra\n\nfunc Extra() {}\n",
	}
	e.b.pollOnce(e.ctx)
	drain(e.ctx, e.b)

	prompts := strings.Join(e.lm.prompts["code_review"], "\n")
	if !strings.Contains(prompts, "+ func Late() {}") || strings.Contains(prompts, "Extra") {
		t.Errorf("want late.go reviewed in the slot left, and extra.go not:\n%s", prompts)
	}
	if len(e.gh.reviews) != 1 {
		t.Fatalf("want 1 review, got %d", len(e.gh.reviews))
	}
	body := e.gh.reviews[0].Body
	for _, want := range []string{"blob (binary)", "huge.go (too large for a diff)", "extra.go (file limit reached)"} {
		if !strings.Contains(body, want) {
			t.Errorf("files not reviewed lacks %q:\n%s", want, body)
		}
	}
	if slices.Contains(e.gh.fileReads, "mmm333:huge.go") {
		t.Errorf("fetched the old version of a file already too large at head: %v", e.gh.fileReads)
	}
}

// Files GitHub sent no diff for are fetched only so far: a known binary type
// is skipped without a fetch, and after maxFailedRebuilds files fail to
// rebuild, the rest aren't tried, so a PR full of assets costs little.
func TestRebuildFetchesAreCapped(t *testing.T) {
	e := newReviewEnv(t, reviewOf(), noApproval)
	e.gh.mergeBase = "mmm333"
	e.gh.fileAt = map[string]string{}
	e.gh.extraFiles = []any{map[string]any{"filename": "art/logo.PNG", "status": "modified"}}
	for i := range maxFailedRebuilds + 10 {
		name := fmt.Sprintf("assets/blob%02d", i)
		e.gh.extraFiles = append(e.gh.extraFiles, map[string]any{"filename": name, "status": "modified"})
		e.gh.fileAt["aaa111:"+name] = "\x00binary"
	}
	e.b.pollOnce(e.ctx)
	drain(e.ctx, e.b)

	fetched := 0
	for _, r := range e.gh.fileReads {
		switch {
		case strings.Contains(r, "logo.PNG"):
			t.Errorf("fetched a file whose extension says it's binary: %s", r)
		case strings.Contains(r, "assets/blob"):
			fetched++
		}
	}
	if fetched < maxFailedRebuilds || fetched > maxFailedRebuilds+rebuildWorkers {
		t.Errorf("fetched %d blobs; want %d, plus at most %d in flight", fetched, maxFailedRebuilds, rebuildWorkers)
	}
	if len(e.gh.reviews) != 1 {
		t.Fatalf("want 1 review, got %d", len(e.gh.reviews))
	}
	body := e.gh.reviews[0].Body
	if !strings.Contains(body, "art/logo.PNG (binary)") || !strings.Contains(body, "the review stopped rebuilding after 20 files it couldn't use") {
		t.Errorf("want logo.PNG listed as binary and the rest as not tried:\n%s", body)
	}
}

// A patch that only removes lines takes no review slot, whether GitHub sent
// it or it was rebuilt, so it leaves room for the next file.
func TestRemovalOnlyPatchesLeaveRoom(t *testing.T) {
	e := newReviewEnv(t, reviewOf(), func(cfg *settings.Settings) {
		noApproval(cfg)
		cfg.Review.MaxFiles = 2 // main.go, and one more
	})
	e.gh.mergeBase = "mmm333"
	e.gh.extraFiles = []any{
		map[string]any{"filename": "gone.txt", "status": "modified", "patch": "@@ -1,2 +0,0 @@\n-x\n-y"},
		map[string]any{"filename": "emptied.go", "status": "modified"}, // rebuilt: emptied at head
		map[string]any{"filename": "late.go", "status": "modified"},
	}
	e.gh.fileAt = map[string]string{
		"mmm333:emptied.go": "package emptied\n",
		"aaa111:emptied.go": "",
		"mmm333:late.go":    "package late\n",
		"aaa111:late.go":    "package late\n\nfunc Late() {}\n",
	}
	e.b.pollOnce(e.ctx)
	drain(e.ctx, e.b)

	prompts := strings.Join(e.lm.prompts["code_review"], "\n")
	if !strings.Contains(prompts, "+ func Late() {}") {
		t.Errorf("want late.go reviewed in the slot the removal-only files left:\n%s", prompts)
	}
	if len(e.gh.reviews) != 1 {
		t.Fatalf("want 1 review, got %d", len(e.gh.reviews))
	}
	body := e.gh.reviews[0].Body
	for _, want := range []string{"gone.txt (only lines removed)", "emptied.go (only lines removed)"} {
		if !strings.Contains(body, want) {
			t.Errorf("files not reviewed lacks %q:\n%s", want, body)
		}
	}
	if strings.Contains(body, "late.go (") {
		t.Errorf("late.go should be reviewed, not listed:\n%s", body)
	}
}

// With no file slot left, the files GitHub sent no diff for are listed at
// the limit without being fetched.
func TestNoRoomFetchesNothing(t *testing.T) {
	e := newReviewEnv(t, reviewOf(), func(cfg *settings.Settings) {
		noApproval(cfg)
		cfg.Review.MaxFiles = 1 // main.go takes it
	})
	e.gh.mergeBase = "mmm333"
	e.gh.extraFiles = []any{map[string]any{"filename": "late.go", "status": "modified"}}
	e.b.pollOnce(e.ctx)
	drain(e.ctx, e.b)
	for _, r := range e.gh.fileReads {
		if strings.HasSuffix(r, ":late.go") {
			t.Errorf("fetched late.go with no room to review it: %v", e.gh.fileReads)
		}
	}
	if len(e.gh.reviews) != 1 || !strings.Contains(e.gh.reviews[0].Body, "late.go (file limit reached)") {
		t.Errorf("late.go should be listed at the file limit: %+v", e.gh.reviews)
	}
}

// A mention reply chooses skill references by every changed file, so the
// files past GitHub's cut, which come with no patch, still count.
func TestMentionPathsIncludePatchlessFiles(t *testing.T) {
	got := mentionPaths(settings.Defaults(), []github.PRFile{
		{Filename: "a.go", Status: "modified", Patch: "@@ -1 +1 @@\n-x\n+y"},
		{Filename: "late.rs", Status: "modified"}, // past the cut
		{Filename: "gone.py", Status: "removed"},
		{Filename: "go.sum", Status: "modified", Patch: "@@ -1 +1 @@\n-x\n+y"}, // ignored
	})
	if !slices.Equal(got, []string{"a.go", "late.rs"}) {
		t.Fatalf("mentionPaths = %v; want [a.go late.rs]", got)
	}
}

// A file whose only change is lines removed has no line to comment on; it
// is listed as not reviewed rather than dropped without a word.
func TestRemovalOnlyFileIsListed(t *testing.T) {
	p := buildPlan(settings.Defaults(), []github.PRFile{{Filename: "gone.go", Status: "modified", Patch: "@@ -1,2 +0,0 @@\n-x\n-y"}}, 2000)
	if len(p.batches) != 0 || len(p.skipped) != 1 || p.skipped[0] != "gone.go (only lines removed)" {
		t.Fatalf("batches=%d skipped=%v; want gone.go listed as only lines removed", len(p.batches), p.skipped)
	}
}

// Without the merge base, a modified file can't be diffed, but an added one
// still can: its whole content is the change. The modified files are set
// aside without a fetch, so however many come first, they don't use up the
// failed-rebuild cap before the added file is reached.
func TestRebuildsAddedFilesWithoutMergeBase(t *testing.T) {
	e := newReviewEnv(t, reviewOf(), noApproval)
	e.gh.extraFiles = []any{map[string]any{"filename": "late.go", "status": "modified"}}
	for i := range maxFailedRebuilds + 5 {
		e.gh.extraFiles = append(e.gh.extraFiles, map[string]any{"filename": fmt.Sprintf("mod%02d.go", i), "status": "modified"})
	}
	e.gh.extraFiles = append(e.gh.extraFiles, map[string]any{"filename": "fresh.go", "status": "added"})
	e.gh.fileAt = map[string]string{"aaa111:fresh.go": "package fresh\n"}
	e.b.pollOnce(e.ctx)
	drain(e.ctx, e.b)

	prompts := strings.Join(e.lm.prompts["code_review"], "\n")
	if !strings.Contains(prompts, "     1 + package fresh") || strings.Contains(prompts, "late.go") {
		t.Errorf("want fresh.go reviewed and late.go not:\n%s", prompts)
	}
	for _, r := range e.gh.fileReads {
		if strings.Contains(r, "mod") || strings.Contains(r, "late.go") {
			t.Errorf("fetched a modified file with no merge base to diff it against: %s", r)
		}
	}
	if len(e.gh.reviews) != 1 || !strings.Contains(e.gh.reviews[0].Body, "late.go (GitHub sent no diff, and the commit it branched from couldn't be found)") {
		t.Errorf("late.go should be listed as missing its merge base: %+v", e.gh.reviews)
	}
}
