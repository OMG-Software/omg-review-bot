package bot

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"database/sql"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/OMG-Software/omg-review-bot/internal/github"
	"github.com/OMG-Software/omg-review-bot/internal/settings"
	"github.com/OMG-Software/omg-review-bot/internal/store"
)

const botLogin = "testbot[bot]"

// fakeGitHub simulates the slice of the GitHub REST and GraphQL APIs the bot
// uses. Reviews the bot posts become review comments and review records, so
// later steps see them like they would on GitHub.
type fakeGitHub struct {
	mu        sync.Mutex
	updatedAt string
	head      string
	patch     string
	content   string
	nextID    int64
	comments  []map[string]any
	resolved  map[int64]bool // root comment ID -> thread resolved
	reviews   []github.ReviewRequest
	reviewLog []map[string]any // GET .../reviews
	replies   []string
	repliedTo []string
	dismissed []int64
	resolves  []string // thread IDs resolved through GraphQL
	jwtCalls  int
	treeCalls int
	instCalls int // repository syncs (installation listings)
	// requestDuring: the next installation listing asks for another sync
	// as it starts (read and cleared outside mu, by the test's wrapper)
	requestDuring bool
	docRefs       []string         // refs project docs were read at
	posted        []string         // PR conversation comments the bot posted
	edited        map[int64]string // the latest text of each comment the bot edited
	failEdits     bool             // editing issue comments fails (a 502)
	failList      bool             // listing the PR's conversation comments fails (a 502)
}

// projectDocs are the repository's guideline files in the fake.
var projectDocs = map[string]string{
	"CLAUDE.md":          "Errors must be wrapped with fmt.Errorf and %w.",
	"CONTRIBUTING.md":    "Every exported function needs a doc comment.",
	"pkg/CLAUDE.md":      "Package pkg never logs.",
	"vendor/x/CLAUDE.md": "Vendored rules must not be sent.",
	"README.md":          "Not a guideline file by default.",
}

func newFakeGitHub() *fakeGitHub {
	return &fakeGitHub{
		updatedAt: "2026-01-01T00:00:00Z", head: "aaa111", nextID: 1000, resolved: map[int64]bool{}, edited: map[int64]string{},
		patch:   "@@ -10,3 +10,5 @@ func main() {\n \ta := 1\n+\tb := a / 0\n+\tfmt.Println(b)\n \treturn",
		content: strings.Repeat("// line\n", 20),
	}
}

func (f *fakeGitHub) id() int64 { f.nextID++; return f.nextID - 1 }

func (f *fakeGitHub) handler(t *testing.T) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		auth := r.Header.Get("Authorization")
		if strings.HasPrefix(r.URL.Path, "/app") && !strings.HasPrefix(auth, "Bearer ") {
			t.Errorf("%s called without App JWT", r.URL.Path)
		}
		if (strings.HasPrefix(r.URL.Path, "/repos/") || r.URL.Path == "/graphql") && auth != "token inst-token" {
			t.Errorf("%s called without installation token (got %q)", r.URL.Path, auth)
		}
		pr := map[string]any{
			"number": 7, "title": "Add division", "body": "Adds a thing.", "state": "open",
			"html_url": "https://github.example/acme/web/pull/7", "user": map[string]any{"login": "alice", "type": "User"},
			"updated_at": f.updatedAt, "head": map[string]any{"ref": "feat", "sha": f.head},
			"base": map[string]any{"ref": "main", "sha": "bbb222"},
		}
		bot := map[string]any{"login": botLogin, "type": "Bot"}
		send := func(v any) { _ = json.NewEncoder(w).Encode(v) }
		switch p := r.URL.Path; {
		case p == "/app":
			f.jwtCalls++
			send(map[string]any{"id": 1, "slug": "testbot", "name": "Test Bot"})
		case p == "/app/installations":
			f.instCalls++
			send([]any{map[string]any{"id": 10, "account": map[string]any{"id": 500, "login": "acme", "type": "Organization"}}})
		case p == "/app/installations/10/access_tokens":
			send(map[string]any{"token": "inst-token", "expires_at": time.Now().Add(time.Hour).Format(time.RFC3339)})
		case p == "/installation/repositories":
			send(map[string]any{"repositories": []any{map[string]any{"id": 100, "full_name": "acme/web"}}})
		case p == "/repos/acme/web/pulls":
			send([]any{pr})
		case p == "/repos/acme/web/pulls/7":
			send(pr)
		case p == "/repos/acme/web/pulls/7/files":
			send([]any{
				map[string]any{"filename": "main.go", "status": "modified", "additions": 2, "deletions": 0, "patch": f.patch},
				map[string]any{"filename": "go.sum", "status": "modified", "patch": "@@ -1 +1 @@\n-x\n+y"},
			})
		case p == "/repos/acme/web/pulls/7/comments" && r.Method == http.MethodGet:
			send(f.comments)
		case p == "/repos/acme/web/pulls/7/reviews" && r.Method == http.MethodGet:
			send(f.reviewLog)
		case p == "/repos/acme/web/pulls/7/reviews" && r.Method == http.MethodPost:
			var req github.ReviewRequest
			_ = json.NewDecoder(r.Body).Decode(&req)
			f.reviews = append(f.reviews, req)
			now := time.Now().Format(time.RFC3339)
			for _, c := range req.Comments {
				f.comments = append(f.comments, map[string]any{"id": f.id(), "user": bot, "body": c.Body, "path": c.Path,
					"line": c.Line, "diff_hunk": "@@ -10,3 +10,5 @@\n+\tb := a / 0", "created_at": now})
			}
			state := map[string]string{"APPROVE": "APPROVED", "COMMENT": "COMMENTED", "REQUEST_CHANGES": "CHANGES_REQUESTED"}[req.Event]
			rid := 500 + int64(len(f.reviews))
			f.reviewLog = append(f.reviewLog, map[string]any{"id": rid, "user": bot, "state": state, "commit_id": req.CommitID, "body": req.Body})
			send(map[string]any{"id": rid, "html_url": fmt.Sprintf("https://github.example/review/%d", rid)})
		case strings.HasPrefix(p, "/repos/acme/web/pulls/7/reviews/") && strings.HasSuffix(p, "/dismissals"):
			var rid int64
			_, _ = fmt.Sscanf(strings.TrimPrefix(p, "/repos/acme/web/pulls/7/reviews/"), "%d", &rid)
			f.dismissed = append(f.dismissed, rid)
			for _, rv := range f.reviewLog {
				if rv["id"] == rid {
					rv["state"] = "DISMISSED"
				}
			}
			send(map[string]any{"id": rid})
		case strings.HasPrefix(p, "/repos/acme/web/pulls/7/comments/") && strings.HasSuffix(p, "/replies"):
			var body map[string]string
			_ = json.NewDecoder(r.Body).Decode(&body)
			var parent int64
			_, _ = fmt.Sscanf(strings.TrimPrefix(p, "/repos/acme/web/pulls/7/comments/"), "%d", &parent)
			f.replies = append(f.replies, body["body"])
			f.repliedTo = append(f.repliedTo, p)
			id := f.id()
			f.comments = append(f.comments, map[string]any{"id": id, "in_reply_to_id": parent, "user": bot, "body": body["body"],
				"path": "main.go", "created_at": time.Now().Format(time.RFC3339)})
			send(map[string]any{"id": id})
		case strings.HasPrefix(p, "/repos/acme/web/compare/"):
			send(map[string]any{"files": []any{map[string]any{"filename": "main.go"}}})
		case p == "/repos/acme/web/issues/7/comments" && r.Method == http.MethodPost:
			var body map[string]string
			_ = json.NewDecoder(r.Body).Decode(&body)
			f.posted = append(f.posted, body["body"])
			// Its own ID range, so review comment IDs stay as the test expects.
			send(map[string]any{"id": 9000 + len(f.posted), "user": bot, "body": body["body"]})
		case p == "/repos/acme/web/issues/7/comments" && f.failList:
			w.WriteHeader(http.StatusBadGateway)
		case p == "/repos/acme/web/issues/7/comments":
			// The comments posted so far, as last edited.
			out := []any{}
			for i, body := range f.posted {
				id := int64(9001 + i)
				if e, ok := f.edited[id]; ok {
					body = e
				}
				out = append(out, map[string]any{"id": id, "user": bot, "body": body})
			}
			send(out)
		case strings.HasPrefix(p, "/repos/acme/web/issues/comments/") && r.Method == http.MethodPatch && f.failEdits:
			w.WriteHeader(http.StatusBadGateway)
		case strings.HasPrefix(p, "/repos/acme/web/issues/comments/") && r.Method == http.MethodPatch:
			id, _ := strconv.ParseInt(strings.TrimPrefix(p, "/repos/acme/web/issues/comments/"), 10, 64)
			var body map[string]string
			_ = json.NewDecoder(r.Body).Decode(&body)
			f.edited[id] = body["body"]
			send(map[string]any{"id": id, "user": bot, "body": body["body"]})
		case strings.HasPrefix(p, "/repos/acme/web/git/trees/"):
			f.treeCalls++
			tree := []any{map[string]any{"path": "main.go", "type": "blob"}, map[string]any{"path": "pkg", "type": "tree"}}
			for name := range projectDocs {
				tree = append(tree, map[string]any{"path": name, "type": "blob"})
			}
			send(map[string]any{"tree": tree, "truncated": false})
		case strings.HasPrefix(p, "/repos/acme/web/contents/"):
			name := strings.TrimPrefix(p, "/repos/acme/web/contents/")
			if doc, ok := projectDocs[name]; ok {
				f.docRefs = append(f.docRefs, r.URL.Query().Get("ref"))
				_, _ = io.WriteString(w, doc)
				return
			}
			_, _ = io.WriteString(w, f.content)
		case p == "/graphql":
			var q struct {
				Query     string         `json:"query"`
				Variables map[string]any `json:"variables"`
			}
			_ = json.NewDecoder(r.Body).Decode(&q)
			if strings.Contains(q.Query, "resolveReviewThread") {
				id, _ := q.Variables["id"].(string)
				f.resolves = append(f.resolves, id)
				var root int64
				_, _ = fmt.Sscanf(id, "T%d", &root)
				f.resolved[root] = true
				send(map[string]any{"data": map[string]any{"resolveReviewThread": map[string]any{"thread": map[string]any{"isResolved": true}}}})
				return
			}
			var nodes []any
			for _, c := range f.comments {
				if c["in_reply_to_id"] == nil {
					id := c["id"].(int64)
					nodes = append(nodes, map[string]any{"id": fmt.Sprintf("T%d", id), "isResolved": f.resolved[id],
						"comments": map[string]any{"nodes": []any{map[string]any{"databaseId": id}}}})
				}
			}
			send(map[string]any{"data": map[string]any{"repository": map[string]any{"pullRequest": map[string]any{
				"reviewThreads": map[string]any{"pageInfo": map[string]any{"hasNextPage": false}, "nodes": nodes}}}}})
		default:
			t.Errorf("unexpected GitHub call %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
		}
	})
}

// fakeLLM is an OpenAI-compatible server. It answers by the JSON schema the
// bot asks for: a review, a fix check or a thread reply; free text otherwise.
type fakeLLM struct {
	mu      sync.Mutex
	calls   int
	review  string // code_review reply
	verify  string // fix_check reply
	prompts map[string][]string
	systems map[string][]string
}

func (l *fakeLLM) handler(t *testing.T) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		l.mu.Lock()
		defer l.mu.Unlock()
		if r.URL.Path != "/v1/chat/completions" {
			t.Errorf("unexpected LLM call %s", r.URL.Path)
		}
		l.calls++
		var req struct {
			Messages []struct {
				Content string `json:"content"`
			} `json:"messages"`
			ResponseFormat *struct {
				JSONSchema struct {
					Name string `json:"name"`
				} `json:"json_schema"`
			} `json:"response_format"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		kind := "text"
		if req.ResponseFormat != nil {
			kind = req.ResponseFormat.JSONSchema.Name
		}
		user := req.Messages[1].Content
		if l.prompts == nil {
			l.prompts, l.systems = map[string][]string{}, map[string][]string{}
		}
		l.prompts[kind] = append(l.prompts[kind], user)
		l.systems[kind] = append(l.systems[kind], req.Messages[0].Content)
		content := "A plain-text answer."
		switch kind {
		case "code_review":
			if strings.Contains(user, "go.sum") {
				t.Errorf("ignored file go.sum was sent to the model")
			}
			content = "<think>let me look</think>```json\n" + l.review + "\n```"
		case "fix_check":
			content = l.verify
		case "thread_reply":
			content = `{"reply":"Good point. Dividing by a constant zero will panic at runtime, so please guard it.","resolved":false}`
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []any{map[string]any{"message": map[string]any{"content": content}, "finish_reason": "stop"}},
			"usage":   map[string]any{"prompt_tokens": 100, "completion_tokens": 50},
		})
	})
}

func testKey(t *testing.T) string {
	k, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(k)}))
}

// drain runs queued jobs synchronously, like the worker would.
func drain(ctx context.Context, b *Bot) {
	for {
		select {
		case j := <-b.jobs:
			b.runJob(ctx, b.dequeued(j))
		default:
			return
		}
	}
}

func TestReviewResolveAndApproveEndToEnd(t *testing.T) {
	gh := newFakeGitHub()
	ghSrv := httptest.NewServer(gh.handler(t))
	defer ghSrv.Close()
	lm := &fakeLLM{review: `{"summary":"Adds a division.","comments":[` +
		`{"path":"main.go","line":11,"severity":"P1","title":"Division by zero","body":"This always panics."},` +
		`{"path":"b/main.go","line":999,"severity":"P3","title":"Out of range","body":"Model picked a bad line."},` +
		`{"path":"main.go","line":12,"severity":"P5","title":"Use log","body":"Prefer a logger."}]}`}
	llmSrv := httptest.NewServer(lm.handler(t))
	defer llmSrv.Close()

	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	cfg := settings.Defaults()
	cfg.GitHub.AppID = 1
	cfg.GitHub.PrivateKey = testKey(t)
	cfg.GitHub.APIBaseURL = ghSrv.URL
	cfg.LLM.BaseURL = llmSrv.URL + "/v1"
	cfg.LLM.Model = "test-model"
	cfg.Review.ReviewExisting = true
	cfg.Review.DebounceSeconds = 0
	cfg.Review.CommentLevel = "P4"                     // drops the P5 nit
	cfg.Review.ApprovalLevel = "P3"                    // approve once nothing at P1-P3 is open
	cfg.Review.RequestChangesLevel = settings.LevelOff // TestRequestChangesEndToEnd covers it
	if err := settings.Save(st, cfg); err != nil {
		t.Fatal(err)
	}

	b := New(st, dir, slog.New(slog.NewTextHandler(io.Discard, nil)))
	ctx := context.Background()
	push := func(sha, patch, content, review, updatedAt string) {
		gh.mu.Lock()
		gh.head, gh.patch, gh.content, gh.updatedAt = sha, patch, content, updatedAt
		gh.mu.Unlock()
		lm.mu.Lock()
		lm.review = review
		lm.mu.Unlock()
	}

	// 1) First poll discovers the PR and reviews it. A P1 is open, so no approval.
	b.pollOnce(ctx)
	drain(ctx, b)
	if s := b.Status(); s.LastPollErr != "" || s.BotLogin != botLogin {
		t.Fatalf("status = %+v", s)
	}
	if len(gh.reviews) != 1 {
		t.Fatalf("expected 1 review posted, got %d", len(gh.reviews))
	}
	// The PR is told the review has started, when, and with what.
	if len(gh.posted) != 1 {
		t.Fatalf("expected 1 review-started comment, got %d: %q", len(gh.posted), gh.posted)
	}
	for _, want := range []string{"**Review started** on `aaa111`", "- **Model:** `test-model`",
		"- **Review skill:** `default-code-review`", time.Now().UTC().Format("2006-01-02 "), " UTC\n", "<!-- prbot:review-started sha=aaa111 -->"} {
		if !strings.Contains(gh.posted[0], want) {
			t.Errorf("review-started comment missing %q:\n%s", want, gh.posted[0])
		}
	}
	// Once the review is posted, the notice says so (keeping its marker).
	if e := gh.edited[9001]; !strings.Contains(e, "**Review posted**") || !strings.Contains(e, "<!-- prbot:review-started sha=aaa111 -->") {
		t.Errorf("notice after the review = %q, want it to say the review is posted", e)
	}
	rv := gh.reviews[0]
	if rv.CommitID != "aaa111" || rv.Event != "COMMENT" {
		t.Errorf("review commit/event = %s/%s", rv.CommitID, rv.Event)
	}
	if len(rv.Comments) != 1 || rv.Comments[0].Path != "main.go" || rv.Comments[0].Line != 11 || rv.Comments[0].Side != "RIGHT" {
		t.Fatalf("inline comments = %+v", rv.Comments)
	}
	if c := rv.Comments[0].Body; !strings.HasPrefix(c, "**[P1] CRITICAL** Division by zero") || !strings.Contains(c, "prbot:finding sev=P1") {
		t.Errorf("inline comment body = %q", c)
	}
	if !strings.Contains(lm.prompts["code_review"][0], "    11 + \tb := a / 0") {
		t.Errorf("diff was not annotated with line numbers:\n%s", lm.prompts["code_review"][0])
	}
	for _, want := range []string{"Adds a division.", "[P3] MINOR (Defect)** Out of range", "go.sum", "1 lower-priority",
		"Not approved:** 2 unresolved", "prbot:review notes=P3"} {
		if !strings.Contains(rv.Body, want) {
			t.Errorf("review body missing %q:\n%s", want, rv.Body)
		}
	}
	// The project's docs reach the model, read from the base commit: the root
	// ones, but not pkg/CLAUDE.md (the PR doesn't touch pkg/) or vendored ones.
	sys := lm.systems["code_review"][0]
	for _, want := range []string{"# Project guidelines", "main at bbb222", "## File: CLAUDE.md", "wrapped with fmt.Errorf",
		"## File: CONTRIBUTING.md", "doc comment"} {
		if !strings.Contains(sys, want) {
			t.Errorf("review system prompt missing %q", want)
		}
	}
	for _, unwanted := range []string{"pkg/CLAUDE.md", "Vendored rules", "Not a guideline file"} {
		if strings.Contains(sys, unwanted) {
			t.Errorf("review system prompt should not contain %q", unwanted)
		}
	}
	for _, ref := range gh.docRefs {
		if ref != "bbb222" {
			t.Errorf("project doc read at %q, want the base commit bbb222", ref)
		}
	}
	pr, _ := st.GetPR("acme/web", 7)
	if pr.Status != "reviewed" || pr.ReviewedSHA != "aaa111" {
		t.Fatalf("PR after review = %s / %s (%s)", pr.Status, pr.ReviewedSHA, pr.LastError)
	}
	findingID := int64(1000)

	// 2) Polling again with no new commit must not re-review.
	b.pollOnce(ctx)
	drain(ctx, b)
	if len(gh.reviews) != 1 {
		t.Fatalf("re-reviewed without a new commit")
	}

	// 3) A human replies to the finding: the bot answers in the thread, once,
	// and leaves it open because the model didn't consider it resolved.
	gh.mu.Lock()
	gh.comments = append(gh.comments, map[string]any{"id": int64(901), "in_reply_to_id": findingID,
		"user": map[string]any{"login": "alice", "type": "User"}, "body": "Is this really a problem?",
		"path": "main.go", "line": 11, "created_at": time.Now().Add(time.Minute).Format(time.RFC3339)})
	gh.updatedAt = "2026-01-01T00:05:00Z"
	gh.mu.Unlock()
	b.pollOnce(ctx)
	drain(ctx, b)
	if len(gh.replies) != 1 || !strings.HasSuffix(gh.repliedTo[0], fmt.Sprintf("/comments/%d/replies", findingID)) {
		t.Fatalf("replies = %v to %v", gh.replies, gh.repliedTo)
	}
	if !st.IsHandled(901) {
		t.Errorf("reply 901 not marked handled")
	}
	if s := lm.systems["thread_reply"][0]; !strings.Contains(s, "wrapped with fmt.Errorf") || strings.Contains(s, "Package pkg never logs") {
		t.Errorf("thread reply didn't get the right project guidelines:\n%s", s)
	}
	if len(gh.resolves) != 0 {
		t.Errorf("thread resolved although the model said it wasn't: %v", gh.resolves)
	}
	b.CheckReplies("acme/web", 7, 500)
	drain(ctx, b)
	if len(gh.replies) != 1 {
		t.Fatalf("replied twice to the same message")
	}

	// 4) A push fixes the division. The bot confirms the fix in the thread,
	// then resolves it, and approves because nothing at P1-P3 is left.
	lm.mu.Lock()
	lm.verify = fmt.Sprintf(`{"results":[{"id":%d,"fixed":true,"reason":"The divisor is now checked before dividing."}]}`, findingID)
	lm.mu.Unlock()
	push("ccc333", "@@ -10,3 +10,6 @@ func main() {\n \ta := 1\n+\tif a == 0 {\n+\t\treturn\n+\t}\n \treturn",
		"package main\n\nfunc main() {\n\ta := 1\n\tif a == 0 {\n\t\treturn\n\t}\n}\n",
		`{"summary":"Guards the division.","comments":[]}`, "2026-01-01T00:10:00Z")
	b.pollOnce(ctx)
	drain(ctx, b)
	if n := len(lm.prompts["fix_check"]); n != 1 || !strings.Contains(lm.prompts["fix_check"][0], fmt.Sprintf("Issue id %d", findingID)) ||
		!strings.Contains(lm.prompts["fix_check"][0], "if a == 0") {
		t.Fatalf("fix check prompts = %q", lm.prompts["fix_check"])
	}
	if len(gh.replies) != 2 || !strings.HasPrefix(gh.replies[1], "✅ Confirmed fixed in `ccc333`. The divisor") {
		t.Fatalf("fix confirmation replies = %q", gh.replies)
	}
	if len(gh.resolves) != 1 || gh.resolves[0] != fmt.Sprintf("T%d", findingID) {
		t.Fatalf("resolved threads = %v", gh.resolves)
	}
	if len(gh.reviews) != 2 || gh.reviews[1].Event != "APPROVE" || !strings.Contains(gh.reviews[1].Body, "1 earlier finding(s) confirmed fixed") {
		t.Fatalf("second review = %+v", gh.reviews[len(gh.reviews)-1])
	}

	// 5) The next push brings a P2: the review is a comment and the earlier
	// approval is withdrawn.
	push("ddd444", "@@ -10,3 +10,4 @@ func main() {\n \ta := 1\n+\tvar m map[string]int; m[\"x\"] = a\n \treturn", "package main\n",
		`{"summary":"Adds a map.","comments":[{"path":"main.go","line":11,"severity":"P2","title":"Write to nil map","body":"Panics."}]}`,
		"2026-01-01T00:15:00Z")
	b.pollOnce(ctx)
	drain(ctx, b)
	if len(gh.reviews) != 3 || gh.reviews[2].Event != "COMMENT" {
		t.Fatalf("third review = %+v", gh.reviews[len(gh.reviews)-1])
	}
	if len(gh.dismissed) != 1 || gh.dismissed[0] != 502 {
		t.Fatalf("dismissed = %v, want the approval 502", gh.dismissed)
	}

	// 6) Someone resolves the P2 thread by hand: the next conversation check approves.
	gh.mu.Lock()
	gh.resolved[1003] = true
	gh.mu.Unlock()
	b.CheckReplies("acme/web", 7, 500)
	drain(ctx, b)
	last := gh.reviews[len(gh.reviews)-1]
	if len(gh.reviews) != 4 || last.Event != "APPROVE" || last.CommitID != "ddd444" || !strings.Contains(last.Body, "prbot:approval") {
		t.Fatalf("approval after resolution = %+v (%d reviews)", last, len(gh.reviews))
	}
	b.CheckReplies("acme/web", 7, 500)
	drain(ctx, b)
	if len(gh.reviews) != 4 {
		t.Errorf("approved the same commit twice")
	}

	// 7) Double-clicking "Review now" must produce one review, not two.
	if err := b.ReviewNow("acme/web", 7, 500); err != nil {
		t.Fatal(err)
	}
	if err := b.ReviewNow("acme/web", 7, 500); err == nil {
		t.Error("second ReviewNow while one is queued should be refused")
	}
	drain(ctx, b)
	if len(gh.reviews) != 5 {
		t.Errorf("expected exactly one extra review, got %d total", len(gh.reviews))
	}
	if gh.jwtCalls != 1 {
		t.Errorf("App info fetched %d times; expected it to be cached", gh.jwtCalls)
	}
	// One listing for the base commit (reviews) and one for the base branch
	// (replies): later lookups come from the cache.
	if gh.treeCalls != 2 {
		t.Errorf("repository tree listed %d times, want 2 (cached)", gh.treeCalls)
	}
}

// TestRequestChangesEndToEnd: an organization's own levels (request changes
// at P2, approve at P3) apply over an instance that does neither. The bot
// requests changes while a P1-P2 finding is open, comments while only a P3 is,
// approves once nothing is, and dismisses its change request or approval
// once it no longer holds.
func TestRequestChangesEndToEnd(t *testing.T) {
	gh := newFakeGitHub()
	ghSrv := httptest.NewServer(gh.handler(t))
	defer ghSrv.Close()
	lm := &fakeLLM{review: `{"summary":"Adds a division.","comments":[` +
		`{"path":"main.go","line":11,"severity":"P1","title":"Division by zero","body":"This always panics."}]}`}
	llmSrv := httptest.NewServer(lm.handler(t))
	defer llmSrv.Close()

	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	cfg := settings.Defaults()
	cfg.GitHub.AppID = 1
	cfg.GitHub.PrivateKey = testKey(t)
	cfg.GitHub.APIBaseURL = ghSrv.URL
	cfg.LLM.BaseURL = llmSrv.URL + "/v1"
	cfg.LLM.Model = "test-model"
	cfg.Review.ReviewExisting = true
	cfg.Review.DebounceSeconds = 0
	cfg.Review.ApprovalLevel, cfg.Review.RequestChangesLevel = settings.LevelOff, settings.LevelOff
	if err := settings.Save(st, cfg); err != nil {
		t.Fatal(err)
	}
	// acme (account 500 in the fake) sets its own levels.
	pol := cfg.Policy()
	pol.Review.ApprovalLevel, pol.Review.RequestChangesLevel = "P3", "P2"
	raw, err := settings.EncodePolicy(cfg, pol)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.UpsertOrg(500, "acme", "Organization", 10); err != nil {
		t.Fatal(err)
	}
	if err := st.SetOrgPolicy(500, raw); err != nil {
		t.Fatal(err)
	}

	b := New(st, dir, slog.New(slog.NewTextHandler(io.Discard, nil)))
	ctx := context.Background()
	push := func(sha, patch, content, review, updatedAt string) {
		gh.mu.Lock()
		gh.head, gh.patch, gh.content, gh.updatedAt = sha, patch, content, updatedAt
		gh.mu.Unlock()
		lm.mu.Lock()
		lm.review = review
		lm.mu.Unlock()
	}
	// rootID finds the bot's finding comment with the given title.
	rootID := func(title string) int64 {
		gh.mu.Lock()
		defer gh.mu.Unlock()
		for _, c := range gh.comments {
			if c["in_reply_to_id"] == nil && strings.Contains(c["body"].(string), title) {
				return c["id"].(int64)
			}
		}
		t.Fatalf("no finding comment titled %q", title)
		return 0
	}
	resolveByHand := func(title string) {
		id := rootID(title)
		gh.mu.Lock()
		gh.resolved[id] = true
		gh.mu.Unlock()
		b.CheckReplies("acme/web", 7, 500)
		drain(ctx, b)
	}
	lastReview := func() github.ReviewRequest { return gh.reviews[len(gh.reviews)-1] }
	wantDismissed := func(step string, want ...int64) {
		t.Helper()
		if fmt.Sprint(gh.dismissed) != fmt.Sprint(want) {
			t.Fatalf("%s: dismissed = %v, want %v", step, gh.dismissed, want)
		}
	}

	// 1) A P1 is open: the review requests changes (review 501).
	b.pollOnce(ctx)
	drain(ctx, b)
	if len(gh.reviews) != 1 || lastReview().Event != "REQUEST_CHANGES" ||
		!strings.Contains(lastReview().Body, "Changes requested:** 1 unresolved finding(s) at [P2] MAJOR or above") {
		t.Fatalf("first review = %+v", lastReview())
	}
	if rec, _ := st.ListReviews(500, "acme/web", 7, 1); len(rec) != 1 || rec[0].Event != "REQUEST_CHANGES" {
		t.Errorf("stored review event = %+v", rec)
	}
	wantDismissed("first review")

	// 2) A push fixes the P1 but brings a P3: that holds back approval without
	// requesting changes, so the review is a comment (502) and the change
	// request is withdrawn.
	lm.mu.Lock()
	lm.verify = fmt.Sprintf(`{"results":[{"id":%d,"fixed":true,"reason":"Guarded."}]}`, rootID("Division by zero"))
	lm.mu.Unlock()
	push("ccc333", "@@ -10,3 +10,6 @@ func main() {\n \ta := 1\n+\tif a == 0 {\n+\t\treturn\n+\t}\n \treturn",
		"package main\n\nfunc main() {\n\ta := 1\n\tif a == 0 {\n\t\treturn\n\t}\n}\n",
		`{"summary":"Guards the division.","comments":[{"path":"main.go","line":11,"severity":"P3","title":"Silent return","body":"Log why."}]}`,
		"2026-01-01T00:10:00Z")
	b.pollOnce(ctx)
	drain(ctx, b)
	if len(gh.reviews) != 2 || lastReview().Event != "COMMENT" || !strings.Contains(lastReview().Body, "Not approved:** 1 unresolved finding(s) at [P3]") {
		t.Fatalf("second review = %+v", lastReview())
	}
	wantDismissed("after the P1 was fixed", 501)

	// 3) Resolving the P3 by hand gets the PR approved (503). The approval
	// replaces the (already dismissed) change request; nothing else is dismissed.
	resolveByHand("Silent return")
	if len(gh.reviews) != 3 || lastReview().Event != "APPROVE" || lastReview().CommitID != "ccc333" {
		t.Fatalf("approval after resolution = %+v (%d reviews)", lastReview(), len(gh.reviews))
	}
	wantDismissed("after approving", 501)

	// 4) The next push brings a P2 and a P3: changes are requested again (504)
	// and the approval is withdrawn.
	push("ddd444", "@@ -10,3 +10,5 @@ func main() {\n \ta := 1\n+\tvar m map[string]int; m[\"x\"] = a\n+\tfmt.Println(m)\n \treturn",
		"package main\n",
		`{"summary":"Adds a map.","comments":[`+
			`{"path":"main.go","line":11,"severity":"P2","title":"Write to nil map","body":"Panics."},`+
			`{"path":"main.go","line":12,"severity":"P3","title":"Unlabelled output","body":"Say what is printed."}]}`,
		"2026-01-01T00:15:00Z")
	b.pollOnce(ctx)
	drain(ctx, b)
	if len(gh.reviews) != 4 || lastReview().Event != "REQUEST_CHANGES" {
		t.Fatalf("fourth review = %+v", lastReview())
	}
	wantDismissed("after the P2", 501, 503)

	// 5) Resolving the P2 by hand lifts the change request, but the open P3
	// still holds back approval: no new review.
	resolveByHand("Write to nil map")
	if len(gh.reviews) != 4 {
		t.Fatalf("posted a review although the P3 is open: %+v", lastReview())
	}
	wantDismissed("after resolving the P2", 501, 503, 504)

	// 6) Resolving the P3 too gets the PR approved.
	resolveByHand("Unlabelled output")
	if len(gh.reviews) != 5 || lastReview().Event != "APPROVE" || lastReview().CommitID != "ddd444" {
		t.Fatalf("final approval = %+v (%d reviews)", lastReview(), len(gh.reviews))
	}
	wantDismissed("after the final approval", 501, 503, 504)

	// 7) A P1 requests changes (506, withdrawing the approval 505); the push that fixes it is approved
	// (507), which replaces the change request on GitHub. Later conversation
	// checks must leave it alone, though its own state still reads
	// CHANGES_REQUESTED.
	push("eee555", "@@ -10,3 +10,4 @@ func main() {\n \ta := 1\n+\tp := (*int)(nil); _ = *p\n \treturn", "package main\n",
		`{"summary":"Adds a pointer.","comments":[{"path":"main.go","line":11,"severity":"P1","title":"Nil dereference","body":"Panics."}]}`,
		"2026-01-01T00:20:00Z")
	b.pollOnce(ctx)
	drain(ctx, b)
	if len(gh.reviews) != 6 || lastReview().Event != "REQUEST_CHANGES" {
		t.Fatalf("sixth review = %+v", lastReview())
	}
	lm.mu.Lock()
	lm.verify = fmt.Sprintf(`{"results":[{"id":%d,"fixed":true,"reason":"Removed."}]}`, rootID("Nil dereference"))
	lm.mu.Unlock()
	push("fff666", "@@ -10,3 +10,4 @@ func main() {\n \ta := 1\n+\t_ = a\n \treturn", "package main\n",
		`{"summary":"Drops the pointer.","comments":[]}`, "2026-01-01T00:25:00Z")
	b.pollOnce(ctx)
	drain(ctx, b)
	if len(gh.reviews) != 7 || lastReview().Event != "APPROVE" {
		t.Fatalf("seventh review = %+v", lastReview())
	}
	b.CheckReplies("acme/web", 7, 500)
	drain(ctx, b)
	if len(gh.reviews) != 7 {
		t.Errorf("posted again after approving: %+v", lastReview())
	}
	wantDismissed("after an approval replaced the change request", 501, 503, 504, 505) // 505: withdrawn when 506 requested changes
}

// TestStaleSnapshotIgnored: a PR state older than the one recorded (a poll
// that started before a webhook arrived) doesn't overwrite it.
func TestStaleSnapshotIgnored(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	b := New(st, t.TempDir(), slog.New(slog.NewTextHandler(io.Discard, nil)))
	cfg := settings.Defaults()
	r := store.Repo{ID: 1, InstallationID: 10, AccountID: 500, FullName: "acme/web", Enabled: true, Synced: true}
	pull := func(sha, updated string) *github.PullRequest {
		p := &github.PullRequest{Number: 7, State: "open", UpdatedAt: updated}
		p.Head.SHA = sha
		return p
	}

	if _, err := b.applyPull(cfg, r, pull("bbb", "2026-01-01T00:10:00Z"), false, 0); err != nil { // the webhook
		t.Fatal(err)
	}
	u, err := b.applyPull(cfg, r, pull("aaa", "2026-01-01T00:05:00Z"), false, 0) // the older poll
	if err != nil {
		t.Fatal(err)
	}
	if pr, _ := st.GetPR("acme/web", 7); !u.stale || pr.HeadSHA != "bbb" {
		t.Errorf("stale snapshot applied: stale=%v head=%s", u.stale, pr.HeadSHA)
	}
	if u, _ := b.applyPull(cfg, r, pull("ccc", "2026-01-01T00:15:00Z"), false, 0); u.stale || !u.pushed {
		t.Errorf("newer update not applied: %+v", u)
	}

	// Same updated_at, to the second: a poll that fetched before "ccc" was
	// saved can't tell which is newer, so it keeps "ccc" and fetches again.
	before := time.Now().Unix() - 60
	u, err = b.applyPull(cfg, r, pull("bbb", "2026-01-01T00:15:00Z"), false, before)
	if err != nil {
		t.Fatal(err)
	}
	if pr, _ := st.GetPR("acme/web", 7); !u.stale || !u.refetch || pr.HeadSHA != "ccc" {
		t.Errorf("equal-timestamp poll applied: stale=%v refetch=%v head=%s", u.stale, u.refetch, pr.HeadSHA)
	}
	// A poll that fetched after the save, or a webhook, is current: applied.
	if u, _ := b.applyPull(cfg, r, pull("ddd", "2026-01-01T00:15:00Z"), false, time.Now().Unix()+60); u.stale || !u.pushed {
		t.Errorf("poll fetched after the save not applied: %+v", u)
	}
	if u, _ := b.applyPull(cfg, r, pull("eee", "2026-01-01T00:15:00Z"), false, 0); u.stale || !u.pushed {
		t.Errorf("equal-timestamp webhook not applied: %+v", u)
	}

	// A PR missing from a poll's list but saved since the fetch is left open,
	// and the poll is told to fetch again (not to save the new ETag), so a
	// missed close webhook can't leave it open for good.
	if refetch := b.closeMissing(cfg, "acme/web", map[int]bool{}, before); !refetch {
		t.Error("closeMissing skipped a PR saved since the fetch without asking for a refetch")
	}
	if pr, _ := st.GetPR("acme/web", 7); !pr.Open {
		t.Error("PR saved since the fetch was closed")
	}
	if refetch := b.closeMissing(cfg, "acme/web", map[int]bool{7: true}, time.Now().Unix()+60); refetch {
		t.Error("closeMissing asked for a refetch with nothing skipped")
	}
	if refetch := b.closeMissing(cfg, "acme/web", map[int]bool{}, time.Now().Unix()+60); refetch {
		t.Error("closeMissing asked for a refetch after closing the PR")
	}
	if pr, _ := st.GetPR("acme/web", 7); pr.Open {
		t.Error("PR missing from a later poll's list wasn't closed")
	}
}

// TestAllowListDoesntUninstall: an organization the local allow-list skips
// is still installed, so it isn't marked uninstalled; one whose installation
// is gone is.
func TestAllowListDoesntUninstall(t *testing.T) {
	gh := newFakeGitHub()
	ghSrv := httptest.NewServer(gh.handler(t))
	defer ghSrv.Close()
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	cfg := settings.Defaults()
	cfg.GitHub.AppID = 1
	cfg.GitHub.PrivateKey = testKey(t)
	cfg.GitHub.APIBaseURL = ghSrv.URL
	if err := settings.Save(st, cfg); err != nil {
		t.Fatal(err)
	}
	b := New(st, dir, slog.New(slog.NewTextHandler(io.Discard, nil)))
	ctx := context.Background()
	active := func(id int64) bool {
		o, err := st.GetOrg(id)
		if err != nil || o == nil {
			t.Fatalf("org %d: %v", id, err)
		}
		return o.Active
	}

	if _, err := b.SyncRepos(ctx); err != nil {
		t.Fatal(err)
	}
	if err := st.UpsertOrg(600, "gone", "Organization", 20); err != nil { // no longer installed
		t.Fatal(err)
	}
	// An owner turned watching off for acme/web, which has an open PR.
	if err := st.SetRepoEnabled(100, false); err != nil {
		t.Fatal(err)
	}
	if err := st.SavePR(&store.PR{Repo: "acme/web", Number: 7, Open: true, Status: "pending", AccountID: 500}); err != nil {
		t.Fatal(err)
	}
	cfg.GitHub.Accounts = []string{"someone-else"} // skips acme
	if err := settings.Save(st, cfg); err != nil {
		t.Fatal(err)
	}
	if _, err := b.SyncRepos(ctx); err != nil {
		t.Fatal(err)
	}
	if !active(500) {
		t.Error("an installed organization skipped by the allow-list was marked uninstalled")
	}
	if active(600) {
		t.Error("an organization with no installation is still active")
	}
	// Paused, not removed: the repository keeps its setting, and its PR stays open.
	if r, _ := st.GetRepo(100); r == nil || r.Enabled {
		t.Errorf("repository of an organization the allow-list skips = %+v; want it kept, still not watched", r)
	}
	if p, _ := st.GetPR("acme/web", 7); p == nil || !p.Open {
		t.Errorf("PR of an organization the allow-list skips = %+v; want it kept open", p)
	}
	// Nor is it touched by webhooks while it's paused.
	if err := st.SetRepoEnabled(100, true); err != nil {
		t.Fatal(err)
	}
	ev := &github.WebhookEvent{Action: "synchronize", Repository: &github.Repo{ID: 100, FullName: "acme/web"},
		PullRequest: &github.PullRequest{Number: 7, State: "open", UpdatedAt: "2026-01-01T00:00:00Z"}}
	if got := b.webhookPull(b.Settings(), ev); got != "ignored: repository isn't watched" {
		t.Errorf("webhook for a paused organization = %q", got)
	}
}

// TestRepoSyncThrottled: the poll's own repository sync is held to one
// attempt a minute, like requestSync's, so a failing sync isn't retried at
// every wake.
func TestRepoSyncThrottled(t *testing.T) {
	gh := newFakeGitHub()
	ghSrv := httptest.NewServer(gh.handler(t))
	defer ghSrv.Close()
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	cfg := settings.Defaults()
	cfg.GitHub.AppID = 1
	cfg.GitHub.PrivateKey = testKey(t)
	cfg.GitHub.APIBaseURL = ghSrv.URL
	if err := settings.Save(st, cfg); err != nil {
		t.Fatal(err)
	}
	b := New(st, dir, slog.New(slog.NewTextHandler(io.Discard, nil)))
	ctx := context.Background()
	syncs := func() int {
		gh.mu.Lock()
		defer gh.mu.Unlock()
		return gh.instCalls
	}

	_ = b.poll(ctx, b.Settings())
	if n := syncs(); n != 1 {
		t.Fatalf("first poll synced %d times, want 1", n)
	}
	// As after a failed sync: no successful one on record.
	b.mu.Lock()
	b.lastRepoSync = time.Time{}
	b.mu.Unlock()
	_ = b.poll(ctx, b.Settings())
	if n := syncs(); n != 1 {
		t.Errorf("a poll within a minute of the last attempt synced again (%d syncs)", n)
	}
	b.mu.Lock()
	b.syncTried = time.Now().Add(-2 * time.Minute)
	b.mu.Unlock()
	_ = b.poll(ctx, b.Settings())
	if n := syncs(); n != 2 {
		t.Errorf("a poll a minute after the last attempt didn't sync (%d syncs)", n)
	}
}

// TestReusedRepoName: when a repository name passes to another GitHub account
// (one renamed, another took its login), the new account's PR starts afresh
// instead of inheriting the old one's state; and reply webhooks are ignored
// for a repository that isn't watched, or a PR of another account.
func TestReusedRepoName(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	b := New(st, t.TempDir(), slog.New(slog.NewTextHandler(io.Discard, nil)))
	cfg := settings.Defaults()
	pull := func(sha, updated string) *github.PullRequest {
		p := &github.PullRequest{Number: 7, State: "open", UpdatedAt: updated}
		p.Head.SHA = sha
		return p
	}
	if err := st.UpsertRepo(100, 10, 500, "acme/web", true); err != nil {
		t.Fatal(err)
	}
	old := store.Repo{ID: 100, InstallationID: 10, AccountID: 500, FullName: "acme/web", Enabled: true, Synced: true}
	u, err := b.applyPull(cfg, old, pull("aaa", "2026-01-01T00:10:00Z"), false, 0)
	if err != nil {
		t.Fatal(err)
	}
	u.pr.ReviewedSHA = "aaa"
	if err := st.SavePR(u.pr); err != nil {
		t.Fatal(err)
	}

	// The other account's PR has an older timestamp: still not "stale".
	other := store.Repo{ID: 200, InstallationID: 20, AccountID: 600, FullName: "acme/web", Enabled: true, Synced: true}
	if u, err = b.applyPull(cfg, other, pull("aaa", "2026-01-01T00:05:00Z"), false, 0); err != nil {
		t.Fatal(err)
	}
	pr, _ := st.GetPR("acme/web", 7)
	if !u.isNew || u.stale || pr.AccountID != 600 || pr.ReviewedSHA != "" {
		t.Errorf("reused name: isNew=%v stale=%v account=%d reviewed=%q; want a fresh PR of account 600",
			u.isNew, u.stale, pr.AccountID, pr.ReviewedSHA)
	}

	ev := &github.WebhookEvent{Action: "created", Repository: &github.Repo{ID: 100, FullName: "acme/web"},
		PullRequest: &github.PullRequest{Number: 7}}
	if got := b.webhookConversation(cfg, "pull_request_review_comment", ev); got != "ignored: pull request isn't tracked" {
		t.Errorf("reply on another account's PR: %q", got)
	}
	if err := st.SetRepoEnabled(100, false); err != nil {
		t.Fatal(err)
	}
	if got := b.webhookConversation(cfg, "pull_request_review_comment", ev); got != "ignored: repository isn't watched" {
		t.Errorf("reply on an unwatched repository: %q", got)
	}
	ev.Repository.ID = 999
	if got := b.webhookConversation(cfg, "pull_request_review_comment", ev); got != "ignored: repository isn't watched" {
		t.Errorf("reply on an unknown repository: %q", got)
	}
}

// TestReviewNotice: the notice names the commit, time, model and skill; a
// failed review says so (with no error details, which can name internal
// hosts); and a retry finds the bot's own notice for the same commit.
func TestReviewNotice(t *testing.T) {
	n := reviewNotice{head: "abc1234567", model: "m`x", skill: "", started: time.Date(2026, 11, 20, 14, 3, 5, 0, time.UTC)}
	started := n.startedBody()
	for _, want := range []string{"**Review started** on `abc1234`", "- **Started:** 2026-11-20 14:03:05 UTC",
		"- **Model:** `mx`", "- **Review skill:** (none)", "<!-- prbot:review-started sha=abc1234567 -->"} {
		if !strings.Contains(started, want) {
			t.Errorf("started notice missing %q:\n%s", want, started)
		}
	}
	if f := n.failedBody(true); !strings.Contains(f, "**Review didn't finish**") || !strings.Contains(f, "tried again automatically") ||
		!strings.Contains(f, noticeMarker(n.head)) {
		t.Errorf("failed notice (retrying):\n%s", f)
	}
	if f := n.failedBody(false); !strings.Contains(f, "won't be retried") {
		t.Errorf("failed notice (final):\n%s", f)
	}
	if f := n.skippedBody(); !strings.Contains(f, "**Review skipped**") || !strings.Contains(f, noticeMarker(n.head)) {
		t.Errorf("skipped notice:\n%s", f)
	}
	if f := n.postedBody("https://github.example/r/1"); !strings.Contains(f, "**Review posted**") ||
		!strings.Contains(f, "[here](https://github.example/r/1)") || !strings.Contains(f, noticeMarker(n.head)) {
		t.Errorf("posted notice:\n%s", f)
	}

	bot := github.User{Login: botLogin}
	cs := []github.IssueComment{
		{ID: 1, User: bot, Body: noticeMarker("other")},
		{ID: 2, User: github.User{Login: "mallory"}, Body: noticeMarker(n.head)}, // not the bot's
		{ID: 3, User: bot, Body: started},
		{ID: 4, User: bot, Body: "unrelated"},
	}
	if id := findNotice(cs, botLogin, n.head); id != 3 {
		t.Errorf("findNotice = %d, want 3", id)
	}
	// A notice edited after a failure is found too, so a retry reuses it.
	cs = append(cs, github.IssueComment{ID: 5, User: bot, Body: n.failedBody(true)})
	if id := findNotice(cs, botLogin, n.head); id != 5 {
		t.Errorf("findNotice after a failure = %d, want 5", id)
	}
	if id := findNotice(cs, botLogin, "nothing"); id != 0 {
		t.Errorf("findNotice for an unknown commit = %d, want 0", id)
	}
}

// TestSyncRequestDuringSync: a sync request that arrives while a sync is
// running gets a sync of its own; the running one, which may have read the
// installations before the change, doesn't count for it.
func TestSyncRequestDuringSync(t *testing.T) {
	gh := newFakeGitHub()
	b, st, cleanup := (func() (*Bot, *store.Store, func()) {
		var b *Bot
		inner := gh.handler(t)
		ghSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/app/installations" && b != nil && gh.requestDuring {
				gh.requestDuring = false
				// An installation event arrives while the sync is under way.
				b.mu.Lock()
				b.syncTried = time.Now().Add(-2 * time.Minute)
				b.mu.Unlock()
				b.requestSync()
			}
			inner.ServeHTTP(w, r)
		}))
		dir := t.TempDir()
		st, err := store.Open(filepath.Join(dir, "t.db"))
		if err != nil {
			t.Fatal(err)
		}
		cfg := settings.Defaults()
		cfg.GitHub.AppID = 1
		cfg.GitHub.PrivateKey = testKey(t)
		cfg.GitHub.APIBaseURL = ghSrv.URL
		if err := settings.Save(st, cfg); err != nil {
			t.Fatal(err)
		}
		b = New(st, dir, slog.New(slog.NewTextHandler(io.Discard, nil)))
		return b, st, func() { st.Close(); ghSrv.Close() }
	})()
	defer cleanup()
	_ = st
	ctx := context.Background()
	syncs := func() int {
		gh.mu.Lock()
		defer gh.mu.Unlock()
		return gh.instCalls
	}
	gh.requestDuring = true
	if err := b.poll(ctx, b.Settings()); err != nil {
		t.Fatal(err)
	}
	if n := syncs(); n != 1 {
		t.Fatalf("first poll synced %d times, want 1", n)
	}
	if err := b.poll(ctx, b.Settings()); err != nil {
		t.Fatal(err)
	}
	if n := syncs(); n != 2 {
		t.Errorf("a sync requested during a sync didn't run (%d syncs)", n)
	}
	if err := b.poll(ctx, b.Settings()); err != nil {
		t.Fatal(err)
	}
	if n := syncs(); n != 2 {
		t.Errorf("synced again with no new request (%d syncs)", n)
	}
}

// TestLastWebhookSurvivesRestart: a bot started after deliveries were
// received knows when the last one came, from the stored deliveries.
func TestLastWebhookSurvivesRestart(t *testing.T) {
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	at := time.Now().Add(-5 * time.Minute).Unix()
	if _, err := st.RecordDelivery(store.Delivery{ID: "d1", Event: "ping", ReceivedAt: at}); err != nil {
		t.Fatal(err)
	}
	b := New(st, dir, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if got := b.Status().LastWebhook; got.Unix() != at {
		t.Errorf("last webhook after a restart = %v, want %v", got, time.Unix(at, 0))
	}
	cfg := settings.Defaults()
	cfg.GitHub.WebhookSecret = "s"
	if err := settings.Save(st, cfg); err != nil {
		t.Fatal(err)
	}
	if !b.Status().WebhooksActive {
		t.Error("webhooks not active after a restart, with a delivery five minutes ago")
	}
}

// TestDelayedSyncRequestDuringSync: a sync request inside the gap since the
// last attempt waits for a timer. When that timer fires while a sync is
// still running, exactly one more sync follows the running one.
func TestDelayedSyncRequestDuringSync(t *testing.T) {
	gh := newFakeGitHub()
	inner := gh.handler(t)
	started, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	ghSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/app/installations" {
			once.Do(func() { close(started); <-release }) // the first sync takes a while
		}
		inner.ServeHTTP(w, r)
	}))
	defer ghSrv.Close()
	// Runs before ghSrv.Close, so a failing check can't leave the handler
	// blocked and Close waiting for it.
	var releaseOnce sync.Once
	letGo := func() { releaseOnce.Do(func() { close(release) }) }
	defer letGo()
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	cfg := settings.Defaults()
	cfg.GitHub.AppID = 1
	cfg.GitHub.PrivateKey = testKey(t)
	cfg.GitHub.APIBaseURL = ghSrv.URL
	if err := settings.Save(st, cfg); err != nil {
		t.Fatal(err)
	}
	b := New(st, dir, slog.New(slog.NewTextHandler(io.Discard, nil)))
	b.syncGap = 100 * time.Millisecond
	ctx := context.Background()
	syncs := func() int {
		gh.mu.Lock()
		defer gh.mu.Unlock()
		return gh.instCalls
	}

	done := make(chan error, 1)
	go func() { done <- b.poll(ctx, b.Settings()) }()
	select {
	case <-started:
	case err := <-done:
		t.Fatalf("the poll ended before syncing: %v", err)
	case <-time.After(10 * time.Second):
		t.Fatal("the sync never started")
	}
	b.requestSync() // inside the gap: waits for the timer
	b.mu.Lock()
	pending := b.syncPending
	b.mu.Unlock()
	if !pending {
		t.Fatal("a request inside the gap didn't set a timer")
	}
	time.Sleep(3 * b.syncGap) // the timer fires while the sync is still running
	b.mu.Lock()
	pending = b.syncPending
	b.mu.Unlock()
	if pending {
		t.Fatal("the timer didn't fire")
	}
	letGo()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if n := syncs(); n != 1 {
		t.Fatalf("syncs after the first poll = %d, want 1", n)
	}
	for i, want := range []int{2, 2} { // one follow-up sync, then none
		if err := b.poll(ctx, b.Settings()); err != nil {
			t.Fatal(err)
		}
		if n := syncs(); n != want {
			t.Errorf("syncs after poll %d = %d, want %d", i+2, n, want)
		}
	}
}

// TestPausedOrgNotReviewedOrRepliedIn: with the "only these accounts"
// setting excluding an organization, a manual review is refused and queued
// review and reply jobs do nothing.
func TestPausedOrgNotReviewedOrRepliedIn(t *testing.T) {
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	cfg := settings.Defaults()
	cfg.GitHub.Accounts = []string{"someone-else"} // pauses acme
	if err := settings.Save(st, cfg); err != nil {
		t.Fatal(err)
	}
	if err := st.SavePR(&store.PR{Repo: "acme/web", Number: 7, Open: true, Status: "pending", AccountID: 500}); err != nil {
		t.Fatal(err)
	}
	b := New(st, dir, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err := b.ReviewNow("acme/web", 7, 500); err == nil || !strings.Contains(err.Error(), "paused") {
		t.Errorf("manual review of a paused organization's PR = %v; want it refused", err)
	}
	b.handleReview(context.Background(), job{kind: jobReview, repo: "acme/web", number: 7, force: true, account: 500})
	if p, _ := st.GetPR("acme/web", 7); p == nil || p.Status != "pending" || p.Attempts != 0 || p.LastError != "" {
		t.Errorf("a queued review of a paused organization's PR ran: %+v", p)
	}
}

// TestInterruptedReviewReusesNotice: a review cut short by a restart starts
// again with no failed attempts on record; it reuses the commit's notice
// rather than posting a second one and leaving the first "under way".
func TestInterruptedReviewReusesNotice(t *testing.T) {
	gh := newFakeGitHub()
	ghSrv := httptest.NewServer(gh.handler(t))
	defer ghSrv.Close()
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	cfg := settings.Defaults()
	cfg.GitHub.AppID = 1
	cfg.GitHub.PrivateKey = testKey(t)
	cfg.GitHub.APIBaseURL = ghSrv.URL
	if err := settings.Save(st, cfg); err != nil {
		t.Fatal(err)
	}
	b := New(st, dir, slog.New(slog.NewTextHandler(io.Discard, nil)))
	ctx := context.Background()
	app, err := b.getApp(ctx, b.Settings())
	if err != nil {
		t.Fatal(err)
	}
	cl := app.Installation(10)
	pr := &store.PR{Repo: "acme/web", Number: 7, AccountID: 500, InstallationID: 10} // no attempts on record
	n := reviewNotice{head: "aaa111", model: "m", skill: "s", started: time.Now()}
	first := b.announceReview(ctx, cl, pr, n)
	if first == 0 {
		t.Fatal("no notice posted")
	}
	// The bot restarts mid-review; the review runs again from the start.
	again := b.announceReview(ctx, cl, pr, n)
	gh.mu.Lock()
	defer gh.mu.Unlock()
	if again != first || len(gh.posted) != 1 {
		t.Errorf("notice after an interrupted review = %d (first %d), %d posted; want the first reused", again, first, len(gh.posted))
	}
	// Nor when the existing notice can't be edited just now.
	gh.failEdits = true
	gh.mu.Unlock()
	if id := b.announceReview(ctx, cl, pr, n); id != first {
		t.Errorf("notice when it can't be edited = %d, want the first (%d) kept", id, first)
	}
	gh.mu.Lock()
	gh.failEdits = false
	if len(gh.posted) != 1 {
		t.Errorf("%d notices posted when the first couldn't be edited; want it kept, not a second", len(gh.posted))
	}
	// A new commit gets a notice of its own.
	gh.mu.Unlock()
	other := b.announceReview(ctx, cl, pr, reviewNotice{head: "bbb222", model: "m", skill: "s", started: time.Now()})
	gh.mu.Lock()
	if other == first || len(gh.posted) != 2 {
		t.Errorf("notice for a new commit = %d, %d posted; want a new one", other, len(gh.posted))
	}
}

// TestUnreadablePolicyFailsClosed: if an organization's policy can't be
// read, its automatic reviews and replies are off, rather than the
// instance's settings applying to it.
func TestUnreadablePolicyFailsClosed(t *testing.T) {
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	cfg := settings.Defaults()
	cfg.Review.Enabled, cfg.Replies.Enabled = true, true
	if err := settings.Save(st, cfg); err != nil {
		t.Fatal(err)
	}
	b := New(st, dir, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if got := b.settingsFor(cfg, 500); !got.Review.Enabled {
		t.Fatal("an organization with no policy of its own doesn't get the instance's settings")
	}
	st.Close() // every read fails from here
	got := b.settingsFor(cfg, 500)
	if got.Review.Enabled || conversationsOn(got) {
		t.Errorf("settings with an unreadable policy = reviews %v, conversations %v; want both off", got.Review.Enabled, conversationsOn(got))
	}
}

// TestNoticeLookupFailureSkipsNotice: if the bot can't check for this
// commit's earlier notice, it posts none, rather than a second one.
func TestNoticeLookupFailureSkipsNotice(t *testing.T) {
	gh := newFakeGitHub()
	ghSrv := httptest.NewServer(gh.handler(t))
	defer ghSrv.Close()
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	cfg := settings.Defaults()
	cfg.GitHub.AppID = 1
	cfg.GitHub.PrivateKey = testKey(t)
	cfg.GitHub.APIBaseURL = ghSrv.URL
	if err := settings.Save(st, cfg); err != nil {
		t.Fatal(err)
	}
	b := New(st, dir, slog.New(slog.NewTextHandler(io.Discard, nil)))
	ctx := context.Background()
	app, err := b.getApp(ctx, b.Settings())
	if err != nil {
		t.Fatal(err)
	}
	gh.mu.Lock()
	gh.failList = true
	gh.mu.Unlock()
	pr := &store.PR{Repo: "acme/web", Number: 7, AccountID: 500, InstallationID: 10}
	id := b.announceReview(ctx, app.Installation(10), pr, reviewNotice{head: "aaa111", model: "m", skill: "s", started: time.Now()})
	gh.mu.Lock()
	defer gh.mu.Unlock()
	if id != 0 || len(gh.posted) != 0 {
		t.Errorf("notice when earlier ones can't be listed = %d, %d posted; want none", id, len(gh.posted))
	}
}

// TestExcludedInstallationKeepsLegacyRepos: a repository stored before
// organizations existed (no account yet) survives a sync while the
// allow-list excludes its installation's account.
func TestExcludedInstallationKeepsLegacyRepos(t *testing.T) {
	gh := newFakeGitHub()
	ghSrv := httptest.NewServer(gh.handler(t))
	defer ghSrv.Close()
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	cfg := settings.Defaults()
	cfg.GitHub.AppID = 1
	cfg.GitHub.PrivateKey = testKey(t)
	cfg.GitHub.APIBaseURL = ghSrv.URL
	cfg.GitHub.Accounts = []string{"someone-else"} // excludes acme, installation 10
	if err := settings.Save(st, cfg); err != nil {
		t.Fatal(err)
	}
	if err := st.UpsertRepo(300, 10, 0, "acme/legacy", false); err != nil { // no account yet
		t.Fatal(err)
	}
	b := New(st, dir, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if _, err := b.SyncRepos(context.Background()); err != nil {
		t.Fatal(err)
	}
	if r, _ := st.GetRepoByName("acme/legacy"); r == nil {
		t.Error("an excluded installation's repository without an account was removed")
	}
}

// TestManualActionsBoundToAccount: a manual review, reply check or skip
// asked for one account's PR doesn't act on another account's PR that has
// taken its name and number since.
func TestManualActionsBoundToAccount(t *testing.T) {
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if err := st.SavePR(&store.PR{Repo: "acme/web", Number: 7, Open: true, Status: "pending", AccountID: 500}); err != nil {
		t.Fatal(err)
	}
	b := New(st, dir, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err := b.ReviewNow("acme/web", 7, 600); err == nil {
		t.Error("manual review for another account's PR accepted")
	}
	if err := b.CheckReplies("acme/web", 7, 600); err == nil {
		t.Error("reply check for another account's PR accepted")
	}
	if err := b.SetSkip("acme/web", 7, 600, true); err == nil {
		t.Error("skip for another account's PR accepted")
	}
	// Queued for account 600's PR, which acme's has replaced since.
	b.handleReview(context.Background(), job{kind: jobReview, repo: "acme/web", number: 7, force: true, account: 600})
	if p, _ := st.GetPR("acme/web", 7); p == nil || p.Skip || p.Status != "pending" || p.Attempts != 0 || p.LastError != "" {
		t.Errorf("acme's PR after another account's manual actions = %+v", p)
	}
}

// TestForcedReviewRetriedAfterPolicyError: a manual review postponed
// because the organization's policy couldn't be read is queued again, even
// with automatic reviews off (so enqueueDue won't).
func TestForcedReviewRetriedAfterPolicyError(t *testing.T) {
	b, take, postpone := forcedRetryBot(t, 4096)
	postpone()
	if p, _ := b.store.GetPR("acme/web", 7); p == nil || p.Status != "pending" || p.Attempts != 0 {
		t.Fatalf("PR after a postponed review = %+v", p)
	}
	again, ok := take()
	if !ok || !again.force || again.account != 500 {
		t.Errorf("manual review after the policy read failed = %+v, %v; want it queued again", again, ok)
	}
}

// TestForcedRetryWaitsForQueueRoom: a manual review postponed by a policy
// read error is queued again once there's room, not dropped because the
// queue was full when its retry came round.
func TestForcedRetryWaitsForQueueRoom(t *testing.T) {
	b, take, postpone := forcedRetryBot(t, 1)
	filler := job{kind: jobReplies, repo: "acme/other", number: 1}
	if !b.enqueue(filler) {
		t.Fatal("couldn't fill the queue")
	}
	postpone()
	time.Sleep(100 * time.Millisecond) // several retries find the queue full
	if j, ok := take(); !ok || j.key() != filler.key() {
		t.Fatalf("first job = %+v, %v; want the one filling the queue", j, ok)
	}
	again, ok := take()
	if !ok || !again.force || again.account != 500 {
		t.Errorf("manual review after the queue had room = %+v, %v; want it queued again", again, ok)
	}
}

// TestForcedRetryNotLostToQueuedReview: when an automatic review of the PR
// is already queued as a postponed manual one comes round again, that queue
// entry runs as the manual review, rather than the manual one being dropped.
func TestForcedRetryNotLostToQueuedReview(t *testing.T) {
	b, take, postpone := forcedRetryBot(t, 4096)
	if !b.enqueue(job{kind: jobReview, repo: "acme/web", number: 7}) {
		t.Fatal("couldn't queue the automatic review")
	}
	postpone()
	time.Sleep(100 * time.Millisecond) // the retry has come round
	j, ok := take()
	if !ok || !j.force || j.account != 500 {
		t.Errorf("queued review = %+v, %v; want the manual review", j, ok)
	}
	if extra, ok := take(); ok {
		t.Errorf("a second review was queued: %+v", extra)
	}
}

// forcedRetryBot returns a bot (with a queue of queueLen) that has taken a
// manual review of acme/web#7 off its queue, a take that does what the
// worker does, and postpone, which runs that review while the
// organization's policy can't be read.
func forcedRetryBot(t *testing.T, queueLen int) (*Bot, func() (job, bool), func()) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "t.db")
	st, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	cfg := settings.Defaults()
	cfg.Review.Enabled = false
	if err := settings.Save(st, cfg); err != nil {
		t.Fatal(err)
	}
	if err := st.SavePR(&store.PR{Repo: "acme/web", Number: 7, Open: true, AccountID: 500}); err != nil {
		t.Fatal(err)
	}
	b := New(st, dir, slog.New(slog.NewTextHandler(io.Discard, nil)))
	b.jobs = make(chan job, queueLen)
	b.policyRetry = 10 * time.Millisecond
	if err := b.ReviewNow("acme/web", 7, 500); err != nil {
		t.Fatal(err)
	}
	take := func() (job, bool) {
		select {
		case j := <-b.jobs:
			return b.dequeued(j), true
		case <-time.After(time.Second):
			return job{}, false
		}
	}
	j, ok := take()
	if !ok {
		t.Fatal("the manual review wasn't queued")
	}
	postpone := func() {
		// The policy can't be read for now: its table is out of the way.
		db, err := sql.Open("sqlite", path)
		if err != nil {
			t.Fatal(err)
		}
		defer db.Close()
		if _, err := db.Exec(`ALTER TABLE orgs RENAME TO orgs_away`); err != nil {
			t.Fatal(err)
		}
		b.handleReview(context.Background(), j)
		if _, err := db.Exec(`ALTER TABLE orgs_away RENAME TO orgs`); err != nil {
			t.Fatal(err)
		}
	}
	return b, take, postpone
}

// TestReviewThreadWebhookHasPR: GitHub's review-thread event carries the PR
// at the top level, as the other pull request events do, so a resolved or
// reopened thread is matched to its PR rather than ignored.
func TestReviewThreadWebhookHasPR(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	b := New(st, t.TempDir(), slog.New(slog.NewTextHandler(io.Discard, nil)))
	// Abridged from GitHub's documented pull_request_review_thread payload.
	body := `{"action":"unresolved",
	  "pull_request":{"number":7,"state":"open","head":{"sha":"aaa"},"base":{"ref":"main"}},
	  "thread":{"node_id":"PRRT_1","comments":[{"id":1001,"pull_request_url":"https://api.github.com/repos/acme/web/pulls/7"}]},
	  "repository":{"id":100,"full_name":"acme/web","owner":{"id":500,"login":"acme"}},
	  "installation":{"id":10},"sender":{"login":"alice"}}`
	var ev github.WebhookEvent
	if err := json.Unmarshal([]byte(body), &ev); err != nil {
		t.Fatal(err)
	}
	if ev.PullRequest == nil || ev.PullRequest.Number != 7 {
		t.Fatalf("review-thread payload decoded without its PR: %+v", ev.PullRequest)
	}
	if got := b.webhookConversation(settings.Defaults(), "pull_request_review_thread", &ev); got == "ignored: not a pull request" {
		t.Errorf("review-thread event = %q", got)
	}
}

// newGitHubBot is a bot whose App talks to the fake GitHub, and an App
// client for it.
func newGitHubBot(t *testing.T) (*Bot, *store.Store, *github.App) {
	t.Helper()
	gh := newFakeGitHub()
	ghSrv := httptest.NewServer(gh.handler(t))
	t.Cleanup(ghSrv.Close)
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	cfg := settings.Defaults()
	cfg.GitHub.AppID = 1
	cfg.GitHub.PrivateKey = testKey(t)
	cfg.GitHub.APIBaseURL = ghSrv.URL
	if err := settings.Save(st, cfg); err != nil {
		t.Fatal(err)
	}
	b := New(st, dir, slog.New(slog.NewTextHandler(io.Discard, nil)))
	app, err := b.getApp(context.Background(), b.Settings())
	if err != nil {
		t.Fatal(err)
	}
	return b, st, app
}

// startedTrackingUnder reports whether a "started tracking" entry is filed
// under the account.
func startedTrackingUnder(st *store.Store, acct int64) bool {
	acts, _ := st.RecentActivity(acct, 50, "", 0)
	for _, a := range acts {
		if strings.Contains(a.Message, "started tracking repository") {
			return true
		}
	}
	return false
}

// TestRepoActivityUsesRepoAccount: a repository's own entries go to its
// account, even while another repository (one that's gone, not yet pruned)
// is stored under the same name.
func TestRepoActivityUsesRepoAccount(t *testing.T) {
	b, st, app := newGitHubBot(t)
	ctx := context.Background()
	if _, err := b.SyncRepos(ctx); err != nil {
		t.Fatal(err)
	}
	if err := st.UpsertRepo(999, 30, 900, "acme/web", true); err != nil { // the stale one, newer row
		t.Fatal(err)
	}
	repos, _ := st.ListRepos(500)
	if len(repos) != 1 {
		t.Fatalf("acme's repositories = %+v", repos)
	}
	if err := b.pollRepo(ctx, b.Settings(), app, repos[0]); err != nil {
		t.Fatal(err)
	}
	if !startedTrackingUnder(st, 500) || startedTrackingUnder(st, 900) {
		t.Errorf("repository entry filed under acme = %v, under the stale repository's account = %v",
			startedTrackingUnder(st, 500), startedTrackingUnder(st, 900))
	}
}

// TestUnknownRepoAccountActivityUnassigned: a repository not yet linked to an
// organization has its entries left unassigned (administrators only), not
// filed under another repository that has had the name.
func TestUnknownRepoAccountActivityUnassigned(t *testing.T) {
	b, st, app := newGitHubBot(t)
	if err := st.UpsertRepo(999, 30, 900, "acme/web", true); err != nil { // another account's, under the name
		t.Fatal(err)
	}
	legacy := store.Repo{ID: 100, InstallationID: 10, FullName: "acme/web", Enabled: true} // no account yet
	if err := b.pollRepo(context.Background(), b.Settings(), app, legacy); err != nil {
		t.Fatal(err)
	}
	if startedTrackingUnder(st, 900) || !startedTrackingUnder(st, 0) {
		t.Errorf("entry of a repository with no account: under the other account = %v, unassigned = %v",
			startedTrackingUnder(st, 900), startedTrackingUnder(st, 0))
	}
}
