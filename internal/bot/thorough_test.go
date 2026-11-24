package bot

import (
	"strings"
	"testing"

	"github.com/OMG-Software/omg-review-bot/internal/settings"
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
	if sys := e.lm.systems["code_review"]; len(sys) < 2 || strings.Contains(sys[len(sys)-1], "read_file") {
		t.Errorf("later passes still told about tools")
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
