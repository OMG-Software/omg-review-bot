package bot

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/OMG-Software/omg-review-bot/internal/llm"
	"github.com/OMG-Software/omg-review-bot/internal/settings"
)

// Synthetic reviews with the defects models produce: a comma left after the
// comments list or inside it, and a quote not escaped inside a string. Each
// holds real findings. Before, the broken object was passed over for the
// first finding inside it, which read as a review with no summary and no
// findings, and the review was approved.
const (
	badCommaAfterList = `{"summary":"Adds a division.","comments":[` + divFinding + `,` + printFinding + `],}`
	badCommaInList    = "{\"summary\":\"Adds a division.\",\"comments\":[\n" + divFinding + ",\n" + printFinding + ", ], }"
	badQuote          = `{"summary":"Adds a division.","comments":[` + divFinding +
		`,{"path":"main.go","line":12,"severity":"P3","title":"Printed","body":"Call "log.Printf" instead."}]}`
	// Where the decoder stops on badQuote, as the retry's hint quotes it.
	badQuoteAt = "\"body\":\"Call \"l`"
)

// scriptLLM answers each request with the next of its replies.
type scriptLLM struct {
	replies []string
	cut     int // the first this many replies are cut off at the output limit
	reqs    []llm.Request
}

func (s *scriptLLM) Chat(_ context.Context, req llm.Request) (*llm.Response, error) {
	s.reqs = append(s.reqs, req)
	if len(s.replies) == 0 {
		return nil, errors.New("no more replies")
	}
	r := s.replies[0]
	s.replies = s.replies[1:]
	cut := s.cut > 0
	if cut {
		s.cut--
	}
	return &llm.Response{Content: r, Truncated: cut}, nil
}

func (s *scriptLLM) Models(context.Context) ([]string, error) { return nil, nil }

// lastMessage is the last message of request i: a retry's hint.
func (s *scriptLLM) lastMessage(i int) string {
	m := s.reqs[i].Messages
	return m[len(m)-1].Content
}

// Every structured answer must have its schema's fields: the review, the fix
// check, the held findings check and the thread reply. A broken object with a
// well-formed item inside is refused, not read as that item.
func TestDecodeRequiredMalformed(t *testing.T) {
	for name, in := range map[string]string{"comma after the list": badCommaAfterList, "comma in the list": badCommaInList} {
		var out modelOutput
		if err := decodeRequired(reviewSchema, in, &out); err != nil || out.Summary != "Adds a division." || len(out.Comments) != 2 {
			t.Errorf("%s: got %+v, %v; want it repaired, with both findings", name, out, err)
		}
	}
	var out modelOutput
	if err := decodeRequired(reviewSchema, badQuote, &out); err == nil || !strings.Contains(err.Error(), badQuoteAt) {
		t.Errorf("unescaped quote: error %v, want the decoder's, at %s", err, badQuoteAt)
	}
	if out.Summary != "" || out.Comments != nil {
		t.Errorf("unescaped quote: decoded %+v", out)
	}
	// A summary with no list isn't a clean review: it would pass the empty
	// review check and approve.
	for _, in := range []string{
		`{"summary":"Adds a division.","comments":null}`,
		`{"summary":"Adds a division.","comments":[,]}`,
	} {
		out = modelOutput{}
		if err := decodeRequired(reviewSchema, in, &out); err == nil {
			t.Errorf("%s: decoded as %+v", in, out)
		}
	}

	for _, c := range []struct {
		name          string
		schema        map[string]any
		broken, comma string
	}{
		{"fix check", verifySchema,
			`{"results":[{"id":1,"fixed":true,"reason":"ok"},{"id":2,"fixed":true,"reason":"see "x""}]}`,
			`{"results":[{"id":1,"fixed":true,"reason":"ok"},],}`},
		{"held check", heldSchema,
			`{"results":[{"id":1,"present":false,"line":0},{"id":2,"present":"yes" "no","line":3}]}`,
			`{"results":[{"id":1,"present":false,"line":0},]}`},
		{"thread reply", threadSchema,
			`{"reply":"Use "x" here.","resolved":true}`,
			`{"reply":"Thanks.","resolved":true,}`},
	} {
		var m map[string]any
		if err := decodeRequired(c.schema, c.broken, &m); err == nil {
			t.Errorf("%s: broken answer decoded as %v", c.name, m)
		}
		m = nil
		if err := decodeRequired(c.schema, c.comma, &m); err != nil || len(m) == 0 {
			t.Errorf("%s: trailing comma not repaired: %v, %v", c.name, m, err)
		}
	}
	var reply map[string]any
	if err := decodeRequired(threadSchema, `{"reply":"Thanks."}`, &reply); err == nil {
		t.Errorf("thread reply without \"resolved\" accepted: %v", reply)
	}
}

// A malformed answer gets the retry, told the decoder's error; a valid retry
// is used; if every try is malformed, the call fails rather than returning
// an empty answer.
func TestChatJSONRetriesMalformed(t *testing.T) {
	b, st := &Bot{}, settings.Defaults()

	lc := &scriptLLM{replies: []string{badQuote, reviewOf(divFinding, printFinding)}}
	var out modelOutput
	if _, _, err := b.chatJSON(t.Context(), lc, st, reviewSchema, "code_review", "sys", "user", &out); err != nil {
		t.Fatalf("chatJSON: %v", err)
	}
	if len(lc.reqs) != 2 || out.Summary != "Adds a division." || len(out.Comments) != 2 {
		t.Fatalf("%d requests, out %+v; want the retry's two findings", len(lc.reqs), out)
	}
	if hint := lc.lastMessage(1); !strings.Contains(hint, "not a valid JSON object") || !strings.Contains(hint, badQuoteAt) {
		t.Errorf("retry hint doesn't give the decoder's error:\n%s", hint)
	}

	// A reply cut off at the output limit isn't used, even with a complete
	// object before the cut: it's asked for again, briefer.
	lc = &scriptLLM{cut: 1, replies: []string{`{"summary":"Clean.","comments":[]} {"summary":"Also`, reviewOf(divFinding)}}
	out = modelOutput{}
	if _, _, err := b.chatJSON(t.Context(), lc, st, reviewSchema, "code_review", "sys", "user", &out); err != nil || len(lc.reqs) != 2 || len(out.Comments) != 1 {
		t.Fatalf("cut off: %d requests, out %+v, %v; want the retry's finding", len(lc.reqs), out, err)
	}
	if hint := lc.lastMessage(1); !strings.Contains(hint, "cut off") {
		t.Errorf("cut-off retry hint = %q", hint)
	}
	lc = &scriptLLM{cut: 2, replies: []string{reviewOf(), reviewOf(), reviewOf()}}
	out = modelOutput{}
	if _, _, err := b.chatJSON(t.Context(), lc, st, reviewSchema, "code_review", "sys", "user", &out); !errors.As(err, &badJSON{}) || out.Summary != "" {
		t.Errorf("every try cut off: out %+v, error %v; want badJSON and nothing decoded", out, err)
	}

	// Trailing commas are repaired, with no retry.
	lc = &scriptLLM{replies: []string{badCommaInList}}
	out = modelOutput{}
	if _, _, err := b.chatJSON(t.Context(), lc, st, reviewSchema, "code_review", "sys", "user", &out); err != nil || len(lc.reqs) != 1 || len(out.Comments) != 2 {
		t.Errorf("trailing comma: %d requests, out %+v, %v; want it repaired at once", len(lc.reqs), out, err)
	}

	lc = &scriptLLM{replies: []string{badQuote, badQuote, reviewOf()}}
	out = modelOutput{}
	_, _, err := b.chatJSON(t.Context(), lc, st, reviewSchema, "code_review", "sys", "user", &out)
	var bad badJSON
	if !errors.As(err, &bad) || len(lc.reqs) != 2 {
		t.Errorf("every try malformed: %d requests, error %v; want badJSON after 2", len(lc.reqs), err)
	}
	if out.Summary != "" || out.Comments != nil {
		t.Errorf("every try malformed, yet decoded %+v", out)
	}

	// The other structured calls retry the same way.
	for _, c := range []struct {
		name, schemaName string
		schema           map[string]any
		broken, valid    string
	}{
		{"fix check", "fix_check", verifySchema, `{"results":[{"id":1,"fixed":true,"reason":"ok"},{"id":2,"fixed":true,"reason":"see "x""}]}`, `{"results":[{"id":7,"fixed":true,"reason":"ok"}]}`},
		{"held check", "held_check", heldSchema, `{"results":[{"id":1,"present":false,"line":0},{"id":2,"present":"yes" "no","line":3}]}`, `{"results":[{"id":7,"present":true,"line":4}]}`},
		{"thread reply", "thread_reply", threadSchema, `{"reply":"Thanks."}`, `{"reply":"Thanks.","resolved":true}`},
	} {
		lc := &scriptLLM{replies: []string{c.broken, c.valid}}
		var m map[string]any
		if _, _, err := b.chatJSON(t.Context(), lc, st, c.schema, c.schemaName, "sys", "user", &m); err != nil || len(lc.reqs) != 2 {
			t.Errorf("%s: %d requests, %v; want the retry used", c.name, len(lc.reqs), err)
			continue
		}
		if !strings.Contains(lc.lastMessage(1), "not a valid JSON object") {
			t.Errorf("%s: retry hint = %q", c.name, lc.lastMessage(1))
		}
		if res, ok := m["results"].([]any); c.schemaName != "thread_reply" && (!ok || len(res) != 1 || res[0].(map[string]any)["id"] != float64(7)) {
			t.Errorf("%s: decoded %v, want the retry's", c.name, m)
		}
	}
}

// The model's first review has a trailing comma: repaired, its findings are
// posted with its summary, and the review requests changes for them.
func TestTrailingCommaReviewPosted(t *testing.T) {
	e := newReviewEnv(t, reviewOf(), func(*settings.Settings) {}) // approves and requests changes at P3
	e.lm.reviews = []string{badCommaAfterList}
	e.b.pollOnce(e.ctx)
	drain(e.ctx, e.b)
	if len(e.gh.reviews) != 1 {
		t.Fatalf("want 1 review, got %d", len(e.gh.reviews))
	}
	rv := e.gh.reviews[0]
	if len(rv.Comments) != 2 || !strings.Contains(rv.Body, "Adds a division.") || strings.Contains(rv.Body, "did not produce a summary") {
		t.Errorf("want both findings and the summary; got %d comments:\n%s", len(rv.Comments), rv.Body)
	}
	if rv.Event != "REQUEST_CHANGES" {
		t.Errorf("event = %s, want REQUEST_CHANGES", rv.Event)
	}
	for _, m := range e.lm.lasts["code_review"] {
		if strings.Contains(m, "not the JSON") || strings.Contains(m, "not a valid JSON") {
			t.Errorf("asked again for a reply the repair fixes: %q", m)
		}
	}
}

// A review with an unescaped quote is asked for again, with and without the
// tools, told where the JSON broke; the valid retry's findings are posted.
func TestMalformedReviewRetried(t *testing.T) {
	for _, noTools := range []bool{false, true} {
		e := newReviewEnv(t, reviewOf(), func(cfg *settings.Settings) { cfg.LLM.NoTools = noTools })
		e.lm.reviews = []string{badQuote, reviewOf(divFinding, printFinding)}
		e.b.pollOnce(e.ctx)
		drain(e.ctx, e.b)
		if len(e.gh.reviews) != 1 || len(e.gh.reviews[0].Comments) != 2 || e.gh.reviews[0].Event == "APPROVE" {
			t.Fatalf("no tools %v: want one review with the retry's 2 findings, not approved; got %+v", noTools, e.gh.reviews)
		}
		hinted := false
		for _, m := range e.lm.lasts["code_review"] {
			hinted = hinted || strings.Contains(m, badQuoteAt)
		}
		if !hinted {
			t.Errorf("no tools %v: no retry told the decoder's error: %q", noTools, e.lm.lasts["code_review"])
		}
	}
}

// runAttempts runs the PR's review until it's given up on, at most n times.
func (e *reviewEnv) runAttempts(t *testing.T, n int) {
	t.Helper()
	e.b.pollOnce(e.ctx)
	drain(e.ctx, e.b)
	for range n - 1 {
		pr, err := e.st.GetPR("acme/web", 7)
		if err != nil || pr == nil || pr.Status != "pending" {
			return
		}
		pr.NotBefore = time.Now().Add(-time.Second).Unix()
		if err := e.st.SavePR(pr); err != nil {
			t.Fatal(err)
		}
		e.b.enqueueDue(e.cfg)
		drain(e.ctx, e.b)
	}
}

// Every reply malformed, or valid but saying nothing: each attempt fails, so
// nothing is posted, let alone approved, and the review is given up on with
// the reason.
func TestUnusableReviewNeverApproved(t *testing.T) {
	for name, c := range map[string]struct{ reply, wantErr string }{
		"malformed": {badQuote, "did not return valid JSON"},
		"empty":     {`{"summary":"","comments":[]}`, errEmptyReview.Error()},
	} {
		e := newReviewEnv(t, c.reply, func(*settings.Settings) {}) // approves at P3
		e.runAttempts(t, maxAttempts)
		if len(e.gh.reviews) != 0 {
			t.Errorf("%s: posted %+v", name, e.gh.reviews)
		}
		pr, _ := e.st.GetPR("acme/web", 7)
		if pr == nil || pr.Status != "error" || pr.Attempts != maxAttempts || !strings.Contains(pr.LastError, c.wantErr) {
			t.Errorf("%s: PR after %d attempts = %+v; want status error, with %q", name, maxAttempts, pr, c.wantErr)
		}
	}
}

// An empty summary with findings is a real review: posted, not failed.
func TestEmptySummaryWithFindingsPosted(t *testing.T) {
	e := newReviewEnv(t, `{"summary":"","comments":[]}`, func(*settings.Settings) {})
	e.lm.reviews = []string{`{"summary":"","comments":[` + divFinding + `]}`}
	e.b.pollOnce(e.ctx)
	drain(e.ctx, e.b)
	if len(e.gh.reviews) != 1 || len(e.gh.reviews[0].Comments) != 1 || e.gh.reviews[0].Event != "REQUEST_CHANGES" ||
		!strings.Contains(e.gh.reviews[0].Body, "did not produce a summary") {
		t.Fatalf("want the finding posted with changes requested; got %+v", e.gh.reviews)
	}
}
