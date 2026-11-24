package bot

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/OMG-Software/omg-review-bot/internal/diff"
	"github.com/OMG-Software/omg-review-bot/internal/github"
	"github.com/OMG-Software/omg-review-bot/internal/settings"
	"github.com/OMG-Software/omg-review-bot/internal/textutil"
)

const (
	divFinding   = `{"path":"main.go","line":11,"severity":"P2","title":"Division by zero panics","body":"Always panics."}`
	printFinding = `{"path":"main.go","line":12,"severity":"P3","title":"Result printed to stdout instead of logged","body":"Use the logger."}`
	// The same issue as printFinding, worded differently.
	printReworded = `{"path":"main.go","line":12,"severity":"P3","title":"Result is printed to stdout instead of logged","body":"Same."}`
)

func reviewOf(comments ...string) string {
	return `{"summary":"Adds a division.","comments":[` + strings.Join(comments, ",") + `]}`
}

func noApproval(cfg *settings.Settings) {
	cfg.Review.ApprovalLevel, cfg.Review.RequestChangesLevel = settings.LevelOff, settings.LevelOff
}

// One review keeps asking about the same code until a pass finds nothing
// new, instead of leaving the rest for later pushes.
func TestReviewPassesUntilNothingNew(t *testing.T) {
	e := newReviewEnv(t, reviewOf(), noApproval)
	e.lm.reviews = []string{
		reviewOf(divFinding),
		reviewOf(divFinding, printFinding), // repeats one, finds one
		reviewOf(printReworded),            // nothing new
		reviewOf(`{"path":"main.go","line":11,"severity":"P1","title":"Never asked for","body":"x"}`),
	}
	e.b.pollOnce(e.ctx)
	drain(e.ctx, e.b)

	prompts := e.lm.prompts["code_review"]
	if len(prompts) != 3 {
		t.Fatalf("model asked %d times, want 3 (the third pass found nothing new)", len(prompts))
	}
	if strings.Contains(prompts[0], "This is pass") {
		t.Errorf("first pass presented as a repeat:\n%s", prompts[0])
	}
	for _, want := range []string{"This is pass 2", "main.go:11 [P2] Division by zero panics"} {
		if !strings.Contains(prompts[1], want) {
			t.Errorf("second pass prompt missing %q:\n%s", want, prompts[1])
		}
	}
	if !strings.Contains(prompts[2], "Result printed to stdout instead of logged") {
		t.Errorf("third pass not told about the second pass's finding:\n%s", prompts[2])
	}
	if len(e.gh.reviews) != 1 || len(e.gh.reviews[0].Comments) != 2 {
		t.Fatalf("want one review with 2 comments, got %+v", e.gh.reviews)
	}
}

// The model is told to ignore the skill's count limits, gets the changed
// files in full, and can read the repository through its tools.
func TestReviewToolsAndContext(t *testing.T) {
	e := newReviewEnv(t, reviewOf(divFinding), noApproval)
	e.lm.toolCalls = []map[string]any{
		{"id": "c1", "type": "function", "function": map[string]any{"name": "read_file", "arguments": `{"path":"main.go","start_line":2,"end_line":3}`}},
		{"id": "c2", "type": "function", "function": map[string]any{"name": "search_code", "arguments": `{"pattern":"wrapped","path_glob":"*.md"}`}},
		{"id": "c3", "type": "function", "function": map[string]any{"name": "read_file", "arguments": `{"path":"../secrets"}`}},
		{"id": "c4", "type": "function", "function": map[string]any{"name": "search_code", "arguments": `{"pattern":"must"}`}},
	}
	e.b.pollOnce(e.ctx)
	drain(e.ctx, e.b)

	if len(e.gh.reviews) != 1 || len(e.gh.reviews[0].Comments) != 1 {
		t.Fatalf("want one review with 1 comment, got %+v", e.gh.reviews)
	}
	sys := e.lm.systems["code_review"][0]
	for _, want := range []string{"Ignore any limit in the skill on how many findings", "read_file, search_code and list_files"} {
		if !strings.Contains(sys, want) {
			t.Errorf("system prompt missing %q", want)
		}
	}
	if user := e.lm.prompts["code_review"][0]; !strings.Contains(user, "### Full file: main.go") || !strings.Contains(user, "   20 | // line") {
		t.Errorf("full file not sent:\n%s", user)
	}
	all := strings.Join(e.lm.toolResults, "\n----\n")
	for _, want := range []string{
		"main.go (20 lines)\n    2 | // line\n    3 | // line\n", // read_file, the range asked for
		"CLAUDE.md:1: Errors must be wrapped",                    // search_code, in the glob
		"Error: couldn't read ../secrets.",                       // the client refuses paths outside the repository
		"CLAUDE.md:1: Errors must be wrapped",                    // search_code without a glob
	} {
		if !strings.Contains(all, want) {
			t.Errorf("tool results missing %q:\n%s", want, all)
		}
	}
	if strings.Contains(all, "vendor/x/CLAUDE.md:") {
		t.Errorf("search_code without a glob searched an ignored file:\n%s", all)
	}
}

// A model server without tool support gets the review without them.
func TestReviewWithoutToolSupport(t *testing.T) {
	e := newReviewEnv(t, reviewOf(divFinding), noApproval)
	e.lm.refuseTools = true
	e.b.pollOnce(e.ctx)
	drain(e.ctx, e.b)
	if len(e.gh.reviews) != 1 || len(e.gh.reviews[0].Comments) != 1 {
		t.Fatalf("want one review with 1 comment, got %+v", e.gh.reviews)
	}
	if e.lm.toolOffers != 1 {
		t.Errorf("tools offered %d times, want once: refused tools aren't offered again", e.lm.toolOffers)
	}
	sys := e.lm.systems["code_review"]
	if len(sys) < 2 {
		t.Fatalf("want at least 2 review requests without tools, got %d", len(sys))
	}
	for i, s := range sys {
		if strings.Contains(s, "read_file") {
			t.Errorf("review request %d was told about tools after the server refused them", i+1)
		}
	}
}

// A model that answers in prose after its investigation is asked for the
// JSON in a request whose roles still alternate.
func TestReviewProseThenJSON(t *testing.T) {
	e := newReviewEnv(t, reviewOf(divFinding), noApproval)
	e.lm.toolCalls = []map[string]any{
		{"id": "c1", "type": "function", "function": map[string]any{"name": "read_file", "arguments": `{"path":"main.go"}`}},
	}
	e.lm.prose = 1
	e.b.pollOnce(e.ctx)
	drain(e.ctx, e.b)
	if len(e.gh.reviews) != 1 || len(e.gh.reviews[0].Comments) != 1 {
		t.Fatalf("want one review with 1 comment, got %+v", e.gh.reviews)
	}
	if e.lm.prose != 0 {
		t.Errorf("the prose answer was never asked for")
	}
}

// Only reads that won't change are cached: a rate limit or server error is
// retried on the next read.
func TestSettledReads(t *testing.T) {
	for _, c := range []struct {
		err  error
		want bool
	}{
		{nil, true},
		{&github.APIError{Status: 404}, true},
		{fmt.Errorf("get: %w", &github.APIError{Status: 404}), true},
		{&github.APIError{Status: 403}, false},
		{&github.APIError{Status: 502}, false},
		{context.Canceled, false},
		{errors.New("connection reset"), false},
	} {
		if got := settled(c.err); got != c.want {
			t.Errorf("settled(%v) = %v, want %v", c.err, got, c.want)
		}
	}
}

// With tools turned off in settings, none are offered.
func TestReviewToolsOff(t *testing.T) {
	e := newReviewEnv(t, reviewOf(divFinding), func(cfg *settings.Settings) {
		noApproval(cfg)
		cfg.LLM.NoTools = true
	})
	e.b.pollOnce(e.ctx)
	drain(e.ctx, e.b)
	if len(e.gh.reviews) != 1 || e.lm.toolOffers != 0 {
		t.Fatalf("reviews %d, tool offers %d; want 1 and 0", len(e.gh.reviews), e.lm.toolOffers)
	}
}

// Findings past the comment limit are held, block approval, and are posted
// with the next review once confirmed still present.
func TestHeldFindingsPostedNextReview(t *testing.T) {
	e := newReviewEnv(t, reviewOf(divFinding, printFinding), func(cfg *settings.Settings) {
		cfg.Review.MaxComments = 1
		cfg.Review.ApprovalLevel = "P3"
		cfg.Review.RequestChangesLevel = settings.LevelOff
	})
	e.lm.verify = `{"results":[]}`
	e.b.pollOnce(e.ctx)
	drain(e.ctx, e.b)

	if len(e.gh.reviews) != 1 {
		t.Fatalf("want 1 review, got %d", len(e.gh.reviews))
	}
	first := e.gh.reviews[0]
	if len(first.Comments) != 1 || !strings.Contains(first.Comments[0].Body, "Division by zero") {
		t.Fatalf("first review should post only the P2: %+v", first.Comments)
	}
	for _, want := range []string{"**Held for the next review:**", "at most 1 findings", "<!-- prbot:review notes= held=P3 -->"} {
		if !strings.Contains(first.Body, want) {
			t.Errorf("first review body missing %q:\n%s", want, first.Body)
		}
	}
	if first.Event == "APPROVE" {
		t.Errorf("approved with a held finding open")
	}
	pr, _ := e.st.GetPR("acme/web", 7)
	if data, head, _ := e.st.HeldFindings(pr.AccountID, "acme/web", 7); !strings.Contains(data, "stdout") || head != "aaa111" {
		t.Fatalf("held finding not stored: %q at %q", data, head)
	}

	// The next push leaves the problem in place, a line further down.
	e.lm.held = `{"results":[{"id":1,"present":true,"line":13}]}`
	e.push("ccc333", "@@ -10,3 +10,6 @@ func main() {\n \ta := 1\n+\tb := a / 0\n+\t_ = b\n+\tfmt.Println(b)\n \treturn",
		strings.Repeat("// line\n", 20), reviewOf(), "2026-01-01T00:10:00Z")
	e.b.pollOnce(e.ctx)
	drain(e.ctx, e.b)

	if n := len(e.lm.prompts["held_check"]); n != 1 || !strings.Contains(e.lm.prompts["held_check"][0], "Result printed to stdout") {
		t.Fatalf("held check prompts = %q", e.lm.prompts["held_check"])
	}
	if len(e.gh.reviews) != 2 {
		t.Fatalf("want 2 reviews, got %d", len(e.gh.reviews))
	}
	second := e.gh.reviews[1]
	if len(second.Comments) != 1 || second.Comments[0].Line != 13 || !strings.Contains(second.Comments[0].Body, "stdout") {
		t.Fatalf("second review should post the held finding at its new line: %+v", second.Comments)
	}
	if strings.Contains(second.Body, "Held for the next review") {
		t.Errorf("nothing should be held any more:\n%s", second.Body)
	}
	if data, _, _ := e.st.HeldFindings(pr.AccountID, "acme/web", 7); data != "" {
		t.Errorf("held findings not cleared: %q", data)
	}
}

// A held finding the next push fixed isn't posted.
func TestHeldFindingDroppedWhenFixed(t *testing.T) {
	e := newReviewEnv(t, reviewOf(divFinding, printFinding), func(cfg *settings.Settings) {
		cfg.Review.MaxComments = 1
		noApproval(cfg)
	})
	e.lm.verify = `{"results":[]}`
	e.b.pollOnce(e.ctx)
	drain(e.ctx, e.b)
	e.lm.held = `{"results":[{"id":1,"present":false,"line":0}]}`
	e.push("ccc333", e.gh.patch, e.gh.content, reviewOf(), "2026-01-01T00:10:00Z")
	e.b.pollOnce(e.ctx)
	drain(e.ctx, e.b)
	if len(e.gh.reviews) != 2 || len(e.gh.reviews[1].Comments) != 0 {
		t.Fatalf("fixed held finding was posted: %+v", e.gh.reviews)
	}
}

// While reviews are stopped, due PRs stay pending and manual reviews are
// refused; resuming reviews them.
func TestStoppedReviewsWait(t *testing.T) {
	e := newReviewEnv(t, reviewOf(), noApproval)
	e.b.StopReviews()
	e.b.pollOnce(e.ctx)
	drain(e.ctx, e.b)
	pr, _ := e.st.GetPR("acme/web", 7)
	if len(e.gh.reviews) != 0 || pr == nil || pr.Status != "pending" {
		t.Fatalf("reviewed while stopped: %d reviews, PR %+v", len(e.gh.reviews), pr)
	}
	if err := e.b.ReviewNow(pr.Repo, pr.Number, pr.AccountID); err == nil || !strings.Contains(err.Error(), "stopped") {
		t.Errorf("Review now while stopped: err = %v", err)
	}
	e.b.ResumeReviews()
	e.b.enqueueDue(e.b.Settings())
	drain(e.ctx, e.b)
	if len(e.gh.reviews) != 1 {
		t.Fatalf("not reviewed after resuming: %d reviews", len(e.gh.reviews))
	}
}

// seedHeld stores findings held from a review of heldHead, as if an earlier
// review had kept them back.
func seedHeld(t *testing.T, e *reviewEnv, heldHead string, fs ...finding) {
	t.Helper()
	data, _ := json.Marshal(fs)
	if err := e.st.SetHeldFindings(500, "acme/web", 7, heldHead, string(data)); err != nil {
		t.Fatal(err)
	}
}

// A file renamed after its findings were held keeps them, under the new
// name: the PR's own list of files only knows the name it had at the base.
func TestHeldFindingFollowsRename(t *testing.T) {
	e := newReviewEnv(t, reviewOf(), noApproval)
	seedHeld(t, e, "zzz000", finding{Path: "old.go", Line: 11, Severity: "P3", Title: "Result printed to stdout instead of logged", Body: "Use the logger."})
	e.gh.compareFiles = []any{map[string]any{"filename": "main.go", "previous_filename": "old.go"}}
	e.lm.held = `{"results":[{"id":1,"present":true,"line":12}]}`
	e.b.pollOnce(e.ctx)
	drain(e.ctx, e.b)
	if len(e.gh.reviews) != 1 {
		t.Fatalf("want 1 review, got %d", len(e.gh.reviews))
	}
	if c := e.gh.reviews[0].Comments; len(c) != 1 || c[0].Path != "main.go" || c[0].Line != 12 {
		t.Fatalf("the held finding should be posted on the renamed file: %+v", c)
	}
}

// With nothing to review, held findings can't be checked: those on files the
// PR still changes wait for the next review, and only the rest go.
func TestNothingToReviewKeepsHeldFindings(t *testing.T) {
	e := newReviewEnv(t, reviewOf(), noApproval)
	e.gh.mainStatus = "removed"
	// Too large a diff for GitHub to show: no patch, so nothing to review.
	e.gh.extraFiles = []any{map[string]any{"filename": "big.go", "status": "modified", "additions": 5000}}
	seedHeld(t, e, "zzz000",
		finding{Path: "big.go", Line: 3, Severity: "P2", Title: "Unchecked error", Body: "Check it."},
		finding{Path: "main.go", Line: 11, Severity: "P3", Title: "Printed to stdout", Body: "Log it."})
	e.b.pollOnce(e.ctx)
	drain(e.ctx, e.b)
	if len(e.gh.reviews) != 0 {
		t.Fatalf("posted a review with nothing to review: %+v", e.gh.reviews)
	}
	data, head, err := e.st.HeldFindings(500, "acme/web", 7)
	if err != nil || !strings.Contains(data, "big.go") || strings.Contains(data, "main.go") || head != "zzz000" {
		t.Fatalf("held = %q at %q (err %v); want big.go's finding kept from zzz000, main.go's dropped", data, head, err)
	}
}

// The same issue reported twice, on one line or in another pass, is one
// finding at its most severe; the same kind of problem on another line
// with a different description, even the next line, is another.
func TestSameFinding(t *testing.T) {
	sql := func(line int, sev, body string) finding {
		return finding{Path: "db.go", Line: line, Severity: sev, Title: "SQL injection", Body: body}
	}
	if !sameFinding(sql(10, "P1", "The user ID is concatenated into the query."), sql(10, "P3", "Something else entirely.")) {
		t.Error("the same title on the same line should be the same finding")
	}
	if sameFinding(sql(10, "P1", "The user ID is concatenated into the lookup query."), sql(11, "P1", "The sort column comes from the request and is formatted into ORDER BY.")) {
		t.Error("two queries on adjacent lines with different descriptions should be two findings")
	}
	if !sameFinding(sql(10, "P1", "The user ID is concatenated into the lookup query."), sql(11, "P1", "The user ID is concatenated into this lookup query.")) {
		t.Error("the same description a line apart should be the same finding")
	}
	if sameFinding(sql(10, "P1", "The user ID is concatenated into the lookup query."), sql(12, "P1", "The sort column comes from the request and is formatted into ORDER BY.")) {
		t.Error("two queries 2 lines apart with different descriptions should be two findings")
	}
	if sameFinding(sql(10, "P1", "The user ID is concatenated into the lookup query."), sql(18, "P1", "The sort column comes from the request and is formatted into ORDER BY.")) {
		t.Error("two queries 8 lines apart with different descriptions should be two findings")
	}
	if !sameFinding(sql(10, "P1", "The user ID is concatenated into the lookup query."), sql(18, "P1", "The user ID is concatenated into the lookup query here.")) {
		t.Error("the same description 8 lines apart should be the same finding")
	}

	df := diff.Parse("db.go", "@@ -1,20 +1,20 @@\n"+strings.Repeat("+x\n", 20))
	p := plan{files: map[string]*diff.File{"db.go": df}}
	st := settings.Defaults()
	st.Review.CommentLevel = "P3"
	inline, _, _, _ := prepareFindings(st, p, []finding{sql(10, "P3", "Held from before."), sql(10, "P1", "Found again, worse.")}, nil, 0)
	if len(inline) != 1 || inline[0].Severity != "P1" || inline[0].Body != "Held from before." {
		t.Fatalf("inline = %+v; want the first report, at P1", inline)
	}
}

// A tool-phase answer must have the fields the schema requires: {} would
// read as a clean review.
func TestHasRequired(t *testing.T) {
	for in, want := range map[string]bool{
		`{"summary":"ok","comments":[]}`:                   true,
		"```json\n{\"summary\":\"\",\"comments\":[]}\n```": true,
		`{"analysis":"need more context"}`:                 false,
		`{"summary":"ok"}`:                                 false,
		`I looked at main.go.`:                             false,
		`null`:                                             false,
	} {
		if got := hasRequired(reviewSchema, in); got != want {
			t.Errorf("hasRequired(%q) = %v, want %v", in, got, want)
		}
	}
}

// cachedTools are tools over files already read: nothing reaches GitHub.
func cachedTools(files map[string]string, missing []string, truncated bool) *repoTools {
	rt := &repoTools{repo: "acme/web", head: "h", base: "h",
		cache: &toolCache{files: map[string]toolFile{}, trees: map[string]toolTree{}}}
	var paths []string
	for p, c := range files {
		rt.cache.files["h\x00"+p] = toolFile{content: c}
		paths = append(paths, p)
	}
	for _, p := range missing {
		rt.cache.files["h\x00"+p] = toolFile{err: &github.APIError{Status: 404}}
		paths = append(paths, p)
	}
	rt.cache.trees["h"] = toolTree{paths: paths, truncated: truncated}
	return rt
}

func TestToolEdgeCases(t *testing.T) {
	ctx := context.Background()
	long := strings.Repeat("x", 5000)
	rt := cachedTools(map[string]string{"min.js": long + "\nsecond\n", "a.go": strings.Repeat("match\n", 1000)}, []string{"gone.go"}, false)

	// A line too long for the room still shows its start, and says how to go on.
	out := rt.readFile(ctx, "h", "min.js", 1, 0, 1000)
	if !strings.Contains(out, "    1 | xxx") || !strings.Contains(out, "start_line 2") || len(out) > 1000 {
		t.Errorf("long line read = %q", textutil.Truncate(out, 300))
	}

	// Matches past what's shown are counted, not kept; files that couldn't
	// be read are reported, not taken as having no matches.
	out = rt.search(ctx, "h", "match|second", "")
	if strings.Count(out, "\n") > maxSearchHits+4 || !strings.Contains(out, "more matches") || !strings.Contains(out, "couldn't be read") {
		t.Errorf("search = %q", textutil.Truncate(out, 400))
	}
	if out := rt.search(ctx, "h", "match", "gone.go"); !strings.Contains(out, "No matches in the files read") {
		t.Errorf("search of an unreadable file = %q", out)
	}

	// A cut-short listing doesn't claim a directory is empty.
	rt = cachedTools(map[string]string{"a.go": "x"}, nil, true)
	if out := rt.list(ctx, "h", "pkg"); !strings.Contains(out, "incomplete") {
		t.Errorf("list of a truncated tree = %q", out)
	}
}

// JSON without the fields asked for isn't taken as a clean review: the
// answer is asked for again with the schema.
func TestReviewIncompleteJSONAskedAgain(t *testing.T) {
	e := newReviewEnv(t, reviewOf(divFinding), noApproval)
	e.lm.prose, e.lm.proseText = 1, `{"analysis":"need more context"}`
	e.b.pollOnce(e.ctx)
	drain(e.ctx, e.b)
	if len(e.gh.reviews) != 1 || len(e.gh.reviews[0].Comments) != 1 {
		t.Fatalf("want one review with the finding, got %+v", e.gh.reviews)
	}
}

// Held findings share the cap with new ones, most severe first; at the same
// level the held one goes first, so it isn't held again behind its equals.
func TestHeldFindingsFirstAtTheirLevel(t *testing.T) {
	df := diff.Parse("a.go", "@@ -1,20 +1,20 @@\n"+strings.Repeat("+x\n", 20))
	p := plan{files: map[string]*diff.File{"a.go": df}}
	st := settings.Defaults()
	st.Review.CommentLevel = "P4"
	held := finding{Path: "a.go", Line: 2, Severity: "P3", Title: "Held from before", Body: "Old."}
	sameLevel := finding{Path: "a.go", Line: 12, Severity: "P3", Title: "New at the same level", Body: "New."}
	worse := finding{Path: "a.go", Line: 18, Severity: "P1", Title: "New and worse", Body: "Worse."}
	inline, _, kept, _ := prepareFindings(st, p, []finding{held, sameLevel, worse}, nil, 2)
	if len(inline) != 2 || inline[0].Title != worse.Title || inline[1].Title != held.Title || len(kept) != 1 || kept[0].Title != sameLevel.Title {
		t.Fatalf("posted %+v, held %+v; want the P1, then the held P3, with the new P3 held", inline, kept)
	}
}

// Files too large to search aren't cached: they'd crowd out the smaller ones.
func TestLargeFilesNotCached(t *testing.T) {
	e := newReviewEnv(t, reviewOf(), noApproval)
	e.b.pollOnce(e.ctx) // sets up the App
	drain(e.ctx, e.b)
	e.gh.content = strings.Repeat("x", maxToolFileBytes+1) // what main.go now holds
	rt := newRepoTools(e.b.app.Installation(10), settings.Defaults(), "acme/web", "h", "")
	for _, p := range []string{"main.go", "CLAUDE.md"} {
		if _, err := rt.file(e.ctx, "h", p); err != nil {
			t.Fatalf("read %s: %v", p, err)
		}
	}
	if _, ok := rt.cache.files["h"+string(rune(0))+"main.go"]; ok || rt.cache.bytes != len(projectDocs["CLAUDE.md"]) {
		t.Fatalf("cache holds %d bytes (main.go cached: %v); want only CLAUDE.md", rt.cache.bytes, ok)
	}
}

// Held findings that can't be decoded fail the review, as a failed read
// does: going on without them could approve past them.
func TestUndecodableHeldFindingsFailTheReview(t *testing.T) {
	e := newReviewEnv(t, reviewOf(), func(cfg *settings.Settings) {
		cfg.Review.ApprovalLevel = "P3"
		cfg.Review.RequestChangesLevel = settings.LevelOff
	})
	if err := e.st.SetHeldFindings(500, "acme/web", 7, "zzz000", `[{"path":`); err != nil {
		t.Fatal(err)
	}
	e.b.pollOnce(e.ctx)
	drain(e.ctx, e.b)
	if len(e.gh.reviews) != 0 {
		t.Fatalf("posted %+v past held findings it couldn't read", e.gh.reviews)
	}
	if data, _, _ := e.st.HeldFindings(500, "acme/web", 7); data != `[{"path":` {
		t.Errorf("held findings changed to %q", data)
	}
}

// Only so many tool calls run per turn; the rest get a result saying so.
func TestToolCallsPerTurnCapped(t *testing.T) {
	e := newReviewEnv(t, reviewOf(), noApproval)
	for i := range maxCallsPerStep + 2 {
		e.lm.batchCalls = append(e.lm.batchCalls, map[string]any{"id": fmt.Sprintf("c%d", i), "type": "function",
			"function": map[string]any{"name": "read_file", "arguments": `{"path":"main.go"}`}})
	}
	e.b.pollOnce(e.ctx)
	drain(e.ctx, e.b)
	if len(e.gh.reviews) != 1 {
		t.Fatalf("want 1 review, got %d", len(e.gh.reviews))
	}
	ran, refused := 0, 0
	for _, r := range e.lm.toolResults {
		if strings.HasPrefix(r, "Not run:") {
			refused++
		} else {
			ran++
		}
	}
	if ran != maxCallsPerStep || refused != 2 {
		t.Fatalf("tool results: %d run, %d refused; want %d and 2", ran, refused, maxCallsPerStep)
	}
}
