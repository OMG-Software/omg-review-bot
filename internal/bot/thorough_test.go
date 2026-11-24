package bot

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"sync"
	"testing"

	"github.com/OMG-Software/omg-review-bot/internal/diff"
	"github.com/OMG-Software/omg-review-bot/internal/github"
	"github.com/OMG-Software/omg-review-bot/internal/settings"
	"github.com/OMG-Software/omg-review-bot/internal/store"
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
	for _, want := range []string{"**Held for the next review:**", "at most 1 finding, most severe first", "- `main.go:12` 🟡 [P3] MINOR (Defect): Result printed to stdout instead of logged", "<!-- prbot:review notes= held=P3 -->"} {
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

// The same issue reported twice on one line, in one pass or another, is one
// finding at its most severe; the same kind of problem on another line,
// even the next one and in the same words, is another.
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
	if sameFinding(sql(10, "P1", "The users query concatenates untrusted input into SQL."), sql(11, "P1", "The orders query concatenates untrusted input into SQL.")) {
		t.Error("two queries on adjacent lines with near-identical descriptions should be two findings")
	}
	if sameFinding(sql(10, "P1", "The user ID is concatenated into the lookup query."), sql(18, "P1", "The user ID is concatenated into the lookup query.")) {
		t.Error("findings 8 lines apart should be two findings, whatever they say")
	}
	if sameFinding(sql(10, "P1", "x"), finding{Path: "db.go", Line: 10, Severity: "P1", Title: "Unchecked error", Body: "x"}) {
		t.Error("different issues on one line should be two findings")
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

// A tool-phase answer must have the fields the schema requires, and fit the
// output: {} would read as a clean review, and so would one finding given
// where the list belongs, if the finding inside it were decoded instead.
func TestDecodeRequired(t *testing.T) {
	for in, want := range map[string]bool{
		`{"summary":"ok","comments":[]}`:                   true,
		"```json\n{\"summary\":\"\",\"comments\":[]}\n```": true,
		`{"analysis":"need more context"}`:                 false,
		`{"summary":"ok"}`:                                 false,
		`I looked at main.go.`:                             false,
		`null`:                                             false,
		`{"summary":"Found an issue","comments":` + divFinding + `}`: false,
	} {
		var out modelOutput
		if got := decodeRequired(reviewSchema, in, &out); got != want {
			t.Errorf("decodeRequired(%q) = %v, want %v", in, got, want)
		}
	}
	var out modelOutput
	if !decodeRequired(reviewSchema, reviewOf(divFinding), &out) || len(out.Comments) != 1 {
		t.Errorf("a list of one finding decoded as %+v", out)
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
	if out := rt.search(ctx, "h", "match", "gone.go"); !strings.Contains(out, "No matches in the files searched") {
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

// If the commit the findings were held on can't be compared with (a
// force-push dropped it, say), a finding on a file the PR doesn't list may
// just have been renamed: it's kept and posted as a note, not dropped. One
// on a file the PR deletes still goes.
func TestHeldFindingKeptWhenRenameUnknown(t *testing.T) {
	e := newReviewEnv(t, reviewOf(), noApproval)
	e.gh.compareGone = true
	e.gh.extraFiles = []any{map[string]any{"filename": "gone.go", "status": "removed"}}
	seedHeld(t, e, "zzz000",
		finding{Path: "renamed.go", Line: 4, Severity: "P2", Title: "Unchecked error from Close", Body: "Check it."},
		finding{Path: "gone.go", Line: 4, Severity: "P2", Title: "Leaked file handle", Body: "Close it."})
	e.b.pollOnce(e.ctx)
	drain(e.ctx, e.b)
	if len(e.gh.reviews) != 1 {
		t.Fatalf("want 1 review, got %d", len(e.gh.reviews))
	}
	if body := e.gh.reviews[0].Body; !strings.Contains(body, "Unchecked error from Close") || strings.Contains(body, "Leaked file handle") {
		t.Fatalf("want the finding on the unknown file posted as a note, and the deleted file's dropped:\n%s", body)
	}
}

// Held findings are saved before the review promising them is posted; if
// posting fails, what was held before is put back for the retry.
func TestHeldFindingsRestoredWhenPostFails(t *testing.T) {
	e := newReviewEnv(t, reviewOf(divFinding, printFinding), func(cfg *settings.Settings) {
		cfg.Review.MaxComments = 1
		noApproval(cfg)
	})
	old := finding{Path: "main.go", Line: 11, Severity: "P3", Title: "Held before", Body: "Old."}
	seedHeld(t, e, "aaa111", old) // on the head, so it's carried unchecked
	e.gh.failReview = true
	e.b.pollOnce(e.ctx)
	drain(e.ctx, e.b)
	if len(e.gh.reviews) != 0 {
		t.Fatalf("posted %d reviews through a failing endpoint", len(e.gh.reviews))
	}
	data, head, _ := e.st.HeldFindings(500, "acme/web", 7)
	var got []finding
	_ = json.Unmarshal([]byte(data), &got)
	if head != "aaa111" || len(got) != 1 || got[0].Title != old.Title {
		t.Fatalf("held = %q at %q; want the findings held before put back", data, head)
	}
}

// With nothing to review, held findings follow a rename since they were
// held, and stay for the next review under the new name.
func TestNothingToReviewFollowsRename(t *testing.T) {
	e := newReviewEnv(t, reviewOf(), noApproval)
	e.gh.mainStatus = "removed"
	// The PR's list knows the file only by its name at the base.
	e.gh.extraFiles = []any{map[string]any{"filename": "c.go", "status": "renamed", "previous_filename": "a.go", "additions": 5000}}
	e.gh.compareFiles = []any{map[string]any{"filename": "c.go", "previous_filename": "b.go"}}
	seedHeld(t, e, "zzz000", finding{Path: "b.go", Line: 3, Severity: "P2", Title: "Unchecked error", Body: "Check it."})
	e.b.pollOnce(e.ctx)
	drain(e.ctx, e.b)
	data, _, err := e.st.HeldFindings(500, "acme/web", 7)
	if err != nil || !strings.Contains(data, `"path":"c.go"`) {
		t.Fatalf("held = %q (err %v); want the finding kept under c.go", data, err)
	}
}

// A held finding whose check failed keeps the commit its line is from, so
// the next review compares from there instead of trusting the old line.
func TestUncheckedHeldFindingKeepsItsCommit(t *testing.T) {
	e := newReviewEnv(t, reviewOf(divFinding), func(cfg *settings.Settings) {
		cfg.Review.MaxComments = 1 // the new P2 is posted; the held P3 is held again
		noApproval(cfg)
	})
	seedHeld(t, e, "zzz000", finding{Path: "main.go", Line: 40, Severity: "P3", Title: "Printed to stdout", Body: "Log it."})
	e.lm.held = "not JSON" // the check fails
	e.b.pollOnce(e.ctx)
	drain(e.ctx, e.b)
	if len(e.gh.reviews) != 1 {
		t.Fatalf("want 1 review, got %d", len(e.gh.reviews))
	}
	data, _, _ := e.st.HeldFindings(500, "acme/web", 7)
	var held []finding
	_ = json.Unmarshal([]byte(data), &held)
	if len(held) != 1 || held[0].Line != 40 || held[0].Seen != "zzz000" {
		t.Fatalf("held = %s; want the unchecked finding at line 40 as seen on zzz000", data)
	}
}

// A finding an empty review moved to a later name keeps its name on the
// commit it's compared from, so a second rename after that still finds it.
func TestNothingToReviewThenRenamedAgain(t *testing.T) {
	e := newReviewEnv(t, reviewOf(), noApproval)
	e.gh.mainStatus = "removed"
	e.gh.extraFiles = []any{map[string]any{"filename": "c.go", "status": "renamed", "previous_filename": "a.go", "additions": 5000}}
	e.gh.compareFiles = []any{map[string]any{"filename": "c.go", "previous_filename": "b.go"}}
	seedHeld(t, e, "zzz000", finding{Path: "b.go", Line: 3, Severity: "P2", Title: "Unchecked error", Body: "Check it."})
	e.b.pollOnce(e.ctx)
	drain(e.ctx, e.b)

	// c.go becomes d.go: compared from zzz000, that's b.go to d.go.
	e.gh.mu.Lock()
	e.gh.extraFiles = []any{map[string]any{"filename": "d.go", "status": "renamed", "previous_filename": "a.go", "additions": 5000}}
	e.gh.compareFiles = []any{map[string]any{"filename": "d.go", "previous_filename": "b.go"}}
	e.gh.mu.Unlock()
	e.push("ccc333", e.gh.patch, e.gh.content, reviewOf(), "2026-01-01T00:10:00Z")
	e.b.pollOnce(e.ctx)
	drain(e.ctx, e.b)

	data, _, err := e.st.HeldFindings(500, "acme/web", 7)
	var held []finding
	_ = json.Unmarshal([]byte(data), &held)
	if err != nil || len(held) != 1 || held[0].Path != "d.go" || held[0].Seen != "zzz000" || held[0].SeenPath != "b.go" {
		t.Fatalf("held = %s (err %v); want the finding under d.go, compared from b.go on zzz000", data, err)
	}
}

// The last review a PR gets automatically posts every finding: no later
// one would post what it held.
func TestLastRoundHoldsNothing(t *testing.T) {
	e := newReviewEnv(t, reviewOf(divFinding, printFinding), func(cfg *settings.Settings) {
		cfg.Review.MaxComments = 1
		cfg.Review.MaxRounds = settings.MinRounds
		noApproval(cfg)
	})
	for i := 1; i < settings.MinRounds; i++ {
		if _, err := e.st.CreateReview(&store.Review{AccountID: 500, Repo: "acme/web", Number: 7, HeadSHA: fmt.Sprintf("old%d", i), Status: "posted"}); err != nil {
			t.Fatal(err)
		}
	}
	e.b.pollOnce(e.ctx)
	drain(e.ctx, e.b)
	if len(e.gh.reviews) != 1 {
		t.Fatalf("want 1 review, got %d", len(e.gh.reviews))
	}
	if c := e.gh.reviews[0].Comments; len(c) != 2 {
		t.Fatalf("the last review should post both findings: %+v", c)
	}
	if data, _, _ := e.st.HeldFindings(500, "acme/web", 7); data != "" {
		t.Errorf("held %q on the last review", data)
	}
}

// A file is binary if it holds a NUL anywhere, not only near the start.
func TestIsBinaryScansWholeFile(t *testing.T) {
	late := strings.Repeat("text\n", 5000) + "\x00tail"
	if !isBinary(late) {
		t.Error("a NUL 25,000 bytes in should make the file binary")
	}
	if isBinary(strings.Repeat("text\n", 5000)) {
		t.Error("plain text is not binary")
	}
}

// A review holds findings past its limit only when a later automatic review
// will post them: not on the last round, and not when the reviews so far
// can't be counted.
func TestCommentLimit(t *testing.T) {
	st := settings.Defaults()
	st.Review.MaxComments, st.Review.MaxRounds = 3, 5
	for _, c := range []struct {
		posted       int
		err          error
		round, limit int
	}{
		{0, nil, 1, 3},
		{3, nil, 4, 3},
		{4, nil, 5, 0}, // the last round
		{2, errors.New("database is locked"), 0, 0},
	} {
		if round, limit := commentLimit(st, c.posted, c.err); round != c.round || limit != c.limit {
			t.Errorf("commentLimit(%d, %v) = %d, %d; want %d, %d", c.posted, c.err, round, limit, c.round, c.limit)
		}
	}
}

// A file too large to search isn't taken as having no matches, and "." is
// the repository's root, like "".
func TestSearchSkipsLargeFilesAndListsDot(t *testing.T) {
	ctx := context.Background()
	big := strings.Repeat("caller()\n", maxToolFileBytes/9+1)
	rt := cachedTools(map[string]string{"gen.go": big, "pkg/a.go": "package pkg\n"}, nil, false)
	out := rt.search(ctx, "h", "caller", "gen.go")
	if strings.Contains(out, "No matches.\n") || !strings.Contains(out, "No matches in the files searched") || !strings.Contains(out, "too large to search") {
		t.Errorf("search of a file too large to search = %q", out)
	}
	for _, dir := range []string{".", "./", ""} {
		if out := rt.list(ctx, "h", dir); !strings.Contains(out, "pkg/a.go") {
			t.Errorf("list %q = %q; want the repository's files", dir, out)
		}
	}
}

// A held check that doesn't say whether a problem is present (no schema
// enforced the field) keeps the finding: only "present": false drops it.
func TestHeldCheckWithoutPresentKeeps(t *testing.T) {
	e := newReviewEnv(t, reviewOf(), noApproval)
	seedHeld(t, e, "zzz000", finding{Path: "main.go", Line: 40, Severity: "P2", Title: "Printed to stdout", Body: "Log it."})
	e.lm.held = `{"results":[{"id":1,"line":12}]}`
	e.b.pollOnce(e.ctx)
	drain(e.ctx, e.b)
	if len(e.gh.reviews) != 1 {
		t.Fatalf("want 1 review, got %d", len(e.gh.reviews))
	}
	if c := e.gh.reviews[0].Comments; len(c) != 1 || c[0].Line != 12 {
		t.Fatalf("the held finding should be posted at line 12: %+v", c)
	}
}

// A held finding whose file can't be traced keeps its own path: it's posted
// as a note, not moved to another file that ends the same way.
func TestHeldFindingNotMovedBySuffix(t *testing.T) {
	e := newReviewEnv(t, reviewOf(), noApproval)
	e.gh.compareGone = true
	e.gh.extraFiles = []any{map[string]any{"filename": "pkg/a.go", "status": "modified", "additions": 1,
		"patch": "@@ -1,1 +1,2 @@\n package pkg\n+var x = 1"}}
	seedHeld(t, e, "zzz000", finding{Path: "a.go", Line: 2, Severity: "P2", Title: "Unchecked error from Close", Body: "Check it."})
	e.b.pollOnce(e.ctx)
	drain(e.ctx, e.b)
	if len(e.gh.reviews) != 1 {
		t.Fatalf("want 1 review, got %d", len(e.gh.reviews))
	}
	r := e.gh.reviews[0]
	for _, c := range r.Comments {
		if c.Path == "pkg/a.go" {
			t.Fatalf("the finding on a.go was posted on pkg/a.go: %+v", c)
		}
	}
	if !strings.Contains(r.Body, "Unchecked error from Close") || !strings.Contains(r.Body, "a.go") {
		t.Fatalf("want the finding posted as a note on a.go:\n%s", r.Body)
	}
}

// Until the review that posts an earlier held finding is posted, that
// finding stays saved with the ones it holds back; then only those stay.
func TestPostedHeldFindingsSavedUntilPosted(t *testing.T) {
	e := newReviewEnv(t, reviewOf(printFinding), func(cfg *settings.Settings) {
		cfg.Review.MaxComments = 1
		noApproval(cfg)
	})
	seedHeld(t, e, "aaa111", finding{Path: "main.go", Line: 11, Severity: "P2", Title: "Held before", Body: "Old."})
	var during string
	e.gh.onReview = func() { during, _, _ = e.st.HeldFindings(500, "acme/web", 7) }
	e.b.pollOnce(e.ctx)
	drain(e.ctx, e.b)
	if len(e.gh.reviews) != 1 || len(e.gh.reviews[0].Comments) != 1 || !strings.Contains(e.gh.reviews[0].Comments[0].Body, "Held before") {
		t.Fatalf("want the held P2 posted: %+v", e.gh.reviews)
	}
	if !strings.Contains(during, "Held before") || !strings.Contains(during, "stdout") {
		t.Errorf("while posting, held = %q; want both the finding being posted and the one held back", during)
	}
	if after, _, _ := e.st.HeldFindings(500, "acme/web", 7); strings.Contains(after, "Held before") || !strings.Contains(after, "stdout") {
		t.Errorf("after posting, held = %q; want only the finding held back", after)
	}
}

// A long list of earlier reviews' comments doesn't crowd this review's own
// findings out of a later pass's prompt.
func TestBatchPromptKeepsRoomForFound(t *testing.T) {
	var previous []string
	for i := range 200 {
		previous = append(previous, fmt.Sprintf("main.go:%d: an earlier review's comment about this line", i))
	}
	found := []string{"main.go:11: Division by zero panics", "main.go:12: Result printed to stdout"}
	out := batchPrompt("PR\n", 0, 1, batch{text: "diff\n"}, previous, found, 2, 2000)
	for _, f := range found {
		if !strings.Contains(out, f) {
			t.Errorf("prompt lacks this review's finding %q", f)
		}
	}
	if !strings.Contains(out, "an earlier review's comment") || !strings.Contains(out, "more\n") {
		t.Error("the earlier comments should fill the rest of the room, and say how many more there are")
	}
}

// A search doesn't download a file the listing says is too large to search.
func TestSearchSkipsLargeFilesByListedSize(t *testing.T) {
	// The cached content would match: if it's read, the size wasn't used.
	rt := cachedTools(map[string]string{"big.go": "caller()\n", "a.go": "package a\n"}, nil, false)
	tr := rt.cache.trees["h"]
	tr.sizes = map[string]int{"big.go": maxToolFileBytes + 1, "a.go": 10}
	rt.cache.trees["h"] = tr
	out := rt.search(context.Background(), "h", "caller", "")
	if strings.Contains(out, "big.go:1") || !strings.Contains(out, "1 of the 2 files are too large to search") {
		t.Errorf("search = %q; want big.go skipped by its listed size", out)
	}
}

// A held finding on a file that's now binary isn't sent to the model to
// check: there's nothing it could check, so the finding is kept as it was.
func TestHeldFindingOnBinaryFileNotChecked(t *testing.T) {
	e := newReviewEnv(t, reviewOf(), noApproval)
	e.gh.content = "func main() {\x00\x01\x02}\n"
	seedHeld(t, e, "zzz000", finding{Path: "main.go", Line: 11, Severity: "P2", Title: "Printed to stdout", Body: "Log it."})
	e.b.pollOnce(e.ctx)
	drain(e.ctx, e.b)
	if n := len(e.lm.prompts["held_check"]); n != 0 {
		t.Fatalf("sent %d held checks of a binary file", n)
	}
	if len(e.gh.reviews) != 1 || !strings.Contains(e.gh.reviews[0].Body+fmt.Sprint(e.gh.reviews[0].Comments), "Printed to stdout") {
		t.Fatalf("the held finding should still be posted: %+v", e.gh.reviews)
	}
	// Kept without a check, and the review's log says so.
	rs, err := e.st.RecentReviews(500, 1)
	if err != nil || len(rs) != 1 {
		t.Fatalf("recent reviews: %v, %v", rs, err)
	}
	r, err := e.st.GetReview(rs[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(r.Raw, "not checked: the file is binary") || !strings.Contains(r.Raw, "(1 of them kept without a check)") {
		t.Errorf("the raw log should say the held finding wasn't checked:\n%.1500s", r.Raw)
	}
}

// A search the ignore patterns kept from every file that could match says
// so, rather than "No matches.".
func TestSearchSaysWhenIgnoredFilesWerentSearched(t *testing.T) {
	rt := cachedTools(map[string]string{"api.pb.go": "caller()\n", "a.go": "package a\n"}, nil, false)
	rt.ignore = []*regexp.Regexp{regexp.MustCompile(`\.pb\.go$`)}
	out := rt.search(context.Background(), "h", "caller", "")
	if strings.Contains(out, "No matches.\n") || !strings.Contains(out, "1 files the review ignores") {
		t.Errorf("search = %q; want it to say the ignored file wasn't searched", out)
	}
	if out := rt.search(context.Background(), "h", "caller", "*.pb.go"); !strings.Contains(out, "api.pb.go:1") {
		t.Errorf("a path_glob should search the ignored file: %q", out)
	}
}

// Models reading the same file at once share one fetch.
func TestToolCacheFetchesOnce(t *testing.T) {
	e := newReviewEnv(t, reviewOf(), noApproval)
	e.b.pollOnce(e.ctx) // sets up the App
	drain(e.ctx, e.b)
	e.gh.mu.Lock()
	before := len(e.gh.docRefs)
	e.gh.mu.Unlock()
	rt := newRepoTools(e.b.app.Installation(10), settings.Defaults(), "acme/web", "h", "")
	start := make(chan struct{})
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			if c, err := rt.fork().file(e.ctx, "h", "CONTRIBUTING.md"); err != nil || c != projectDocs["CONTRIBUTING.md"] {
				t.Errorf("read = %q, %v", c, err)
			}
		}()
	}
	close(start)
	wg.Wait()
	e.gh.mu.Lock()
	defer e.gh.mu.Unlock()
	if n := len(e.gh.docRefs) - before; n != 1 {
		t.Errorf("fetched %d times; want once", n)
	}
	if rt.cache.bytes != len(projectDocs["CONTRIBUTING.md"]) {
		t.Errorf("cache counts %d bytes; want the file once", rt.cache.bytes)
	}
}

// A part's summary is the first a pass gives, not only the first pass's.
func TestSummaryFromALaterPass(t *testing.T) {
	e := newReviewEnv(t, reviewOf(), noApproval)
	e.lm.reviews = []string{`{"summary":"","comments":[` + divFinding + `]}`, `{"summary":"Adds a division.","comments":[]}`}
	e.b.pollOnce(e.ctx)
	drain(e.ctx, e.b)
	if len(e.gh.reviews) != 1 || !strings.Contains(e.gh.reviews[0].Body, "Adds a division.") {
		t.Fatalf("the second pass's summary should be posted: %+v", e.gh.reviews)
	}
}

// Models listing the repository at once share one fetch of the listing.
func TestToolTreeFetchedOnce(t *testing.T) {
	e := newReviewEnv(t, reviewOf(), noApproval)
	e.b.pollOnce(e.ctx) // sets up the App
	drain(e.ctx, e.b)
	e.gh.mu.Lock()
	before := e.gh.treeCalls
	e.gh.mu.Unlock()
	rt := newRepoTools(e.b.app.Installation(10), settings.Defaults(), "acme/web", "h", "")
	start := make(chan struct{})
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			if tr := rt.fork().tree(e.ctx, "h"); tr.err != nil || len(tr.paths) == 0 {
				t.Errorf("tree = %+v", tr)
			}
		}()
	}
	close(start)
	wg.Wait()
	e.gh.mu.Lock()
	defer e.gh.mu.Unlock()
	if n := e.gh.treeCalls - before; n != 1 {
		t.Errorf("listed the repository %d times; want once", n)
	}
}

// A call's arguments count against the room for tool results as soon as the
// model sends them, so a turn whose calls alone fill it runs none of them.
func TestToolArgumentsCountBeforeRunning(t *testing.T) {
	e := newReviewEnv(t, reviewOf(), noApproval)
	pad := strings.Repeat("x", 200_000) // more than this review's room for tool results
	e.lm.batchCalls = []map[string]any{
		{"id": "c0", "type": "function", "function": map[string]any{"name": "read_file", "arguments": `{"path":"main.go","pad":"` + pad + `"}`}},
		{"id": "c1", "type": "function", "function": map[string]any{"name": "read_file", "arguments": `{"path":"main.go"}`}},
	}
	e.b.pollOnce(e.ctx)
	drain(e.ctx, e.b)
	if len(e.gh.reviews) != 1 {
		t.Fatalf("want 1 review, got %d", len(e.gh.reviews))
	}
	// Over the room, the conversation ends with the results flattened into
	// text, so the review's raw log is where the calls show.
	rs, err := e.st.RecentReviews(500, 1)
	if err != nil || len(rs) != 1 {
		t.Fatalf("recent reviews: %v, %v", rs, err)
	}
	r, err := e.st.GetReview(rs[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(r.Raw, "[tool] read_file skipped (no room left)"); n != 2 || strings.Contains(r.Raw, "[tool] read_file {") {
		t.Errorf("want both calls skipped for room, none run; the raw log has %d skipped:\n%.1500s", n, r.Raw)
	}
}

// With several models, the held check's progress line gives way to the
// review's once the models start: the passes don't set it themselves.
func TestManyModelsActivityAfterHeldCheck(t *testing.T) {
	second := &fakeLLM{review: `{"summary":"Second view.","comments":[]}`}
	var b *Bot
	var mu sync.Mutex
	var seen []string
	h := second.handler(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		r.Body = io.NopCloser(bytes.NewReader(body))
		mu.Lock()
		if b != nil && bytes.Contains(body, []byte(`"tools"`)) { // only reviews offer tools
			for _, a := range b.Status().Working {
				seen = append(seen, a.What)
			}
		}
		mu.Unlock()
		h.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)
	e := newReviewEnv(t, reviewOf(), func(cfg *settings.Settings) {
		cfg.Review.ApprovalLevel, cfg.Review.RequestChangesLevel = settings.LevelOff, settings.LevelOff
		cfg.Models = settings.Models{Strategy: settings.StrategyCollate,
			Configs: []settings.LLM{cfg.LLM, modelAt("second", srv.URL)}}
	})
	seedHeld(t, e, "zzz000", finding{Path: "main.go", Line: 11, Severity: "P2", Title: "Printed to stdout", Body: "Log it."})
	mu.Lock()
	b = e.b
	mu.Unlock()
	e.b.pollOnce(e.ctx)
	drain(context.WithValue(e.ctx, workerSlot{}, 0), e.b) // as worker 0, which shows progress
	if len(e.lm.prompts["held_check"]) == 0 {
		t.Fatal("the held finding wasn't checked, so the test shows nothing")
	}
	mu.Lock()
	defer mu.Unlock()
	if len(seen) == 0 {
		t.Fatal("the second model was never asked to review")
	}
	for _, s := range seen {
		if !strings.HasPrefix(s, "Reviewing acme/web#7 with 2 models") {
			t.Errorf("while the models reviewed, the activity was %q", s)
		}
	}
}

// A file's final newline ends its last line, as in the diff gutter and
// read_file, rather than numbering an empty line after it.
func TestNumberedFileEndsAtLastLine(t *testing.T) {
	for _, c := range []struct{ in, want string }{
		{"a\nb\n", "    1 | a\n    2 | b\n"},
		{"a\r\nb", "    1 | a\n    2 | b\n"},
		{"a\n\n", "    1 | a\n    2 | \n"},
		{"", ""},
	} {
		if got := numberedFile(c.in); got != c.want {
			t.Errorf("numberedFile(%q) = %q; want %q", c.in, got, c.want)
		}
	}
	if got := fileView("a\nb\n", []int{2}, 1); strings.Contains(got, "    3 |") {
		t.Errorf("fileView numbers a line past the end: %q", got)
	}
}

// A review's tools stop reading from GitHub once they've made
// maxFetches reads; what's been read can still be read again.
func TestToolReadsHaveABudget(t *testing.T) {
	e := newReviewEnv(t, reviewOf(), noApproval)
	e.b.pollOnce(e.ctx) // sets up the App
	drain(e.ctx, e.b)
	rt := newRepoTools(e.b.app.Installation(10), settings.Defaults(), "acme/web", "h", "")
	rt.cache.maxFetches = 2
	read := func(p string) string { return rt.readFile(e.ctx, "h", p, 0, 0, 10000) }
	for _, p := range []string{"CLAUDE.md", "CONTRIBUTING.md"} {
		if out := read(p); !strings.Contains(out, projectDocs[p]) {
			t.Fatalf("read %s = %q", p, out)
		}
	}
	e.gh.mu.Lock()
	before := len(e.gh.docRefs)
	e.gh.mu.Unlock()
	if out := read("README.md"); !strings.Contains(out, "read as many files as it may") {
		t.Errorf("a read past the budget = %q; want it refused", out)
	}
	if out := read("CLAUDE.md"); !strings.Contains(out, projectDocs["CLAUDE.md"]) {
		t.Errorf("a file already read should still read: %q", out)
	}
	e.gh.mu.Lock()
	defer e.gh.mu.Unlock()
	if n := len(e.gh.docRefs) - before; n != 0 {
		t.Errorf("%d GitHub reads past the budget", n)
	}

	// A search says which files the budget kept it from.
	st := cachedTools(map[string]string{"a.go": "caller()\n"}, nil, false)
	st.cache.trees["h"] = toolTree{paths: []string{"a.go", "b.go"}}
	if out := st.search(e.ctx, "h", "caller", ""); !strings.Contains(out, "a.go:1") || !strings.Contains(out, "1 of the 2 files weren't searched: this review has read as many files as it may") {
		t.Errorf("search = %q; want a.go's match and a note for b.go", out)
	}
}

// A model whose context can't spare the room the tools take reviews without
// them rather than failing the review.
func TestSmallContextReviewsWithoutTools(t *testing.T) {
	fellBack := false
	for tokens := 1500; tokens <= 12000 && !fellBack; tokens += 250 {
		e := newReviewEnv(t, reviewOf(), func(cfg *settings.Settings) {
			noApproval(cfg)
			cfg.LLM.ContextTokens, cfg.LLM.MaxOutputTokens = tokens, 512
		})
		e.b.pollOnce(e.ctx)
		drain(e.ctx, e.b)
		if len(e.gh.reviews) == 0 || e.lm.toolOffers > 0 {
			continue // too small even without tools, or large enough for them
		}
		rs, err := e.st.RecentReviews(500, 1)
		if err != nil || len(rs) != 1 {
			t.Fatalf("recent reviews: %v, %v", rs, err)
		}
		r, err := e.st.GetReview(rs[0].ID)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(r.Raw, "repository tools: off, the model's context has no room for them") {
			t.Errorf("at %d tokens the review ran without tools, but the raw log doesn't say why", tokens)
		}
		fellBack = true
	}
	if !fellBack {
		t.Error("no context size reviewed without tools: too small for them, the review fails")
	}
}
