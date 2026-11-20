package bot

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
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
	docRefs   []string // refs project docs were read at
	posted    []string // PR conversation comments the bot posted
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
		updatedAt: "2026-01-01T00:00:00Z", head: "aaa111", nextID: 1000, resolved: map[int64]bool{},
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
			send([]any{map[string]any{"id": 10, "account": map[string]any{"login": "acme"}}})
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
		case p == "/repos/acme/web/issues/7/comments":
			send([]any{})
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
			b.mu.Lock()
			delete(b.queued, j.key())
			b.mu.Unlock()
			b.runJob(ctx, j)
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
	cfg.Review.CommentLevel = "P4"  // drops the P5 nit
	cfg.Review.ApprovalLevel = "P3" // approve once nothing at P1-P3 is open
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
	b.CheckReplies("acme/web", 7)
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
	b.CheckReplies("acme/web", 7)
	drain(ctx, b)
	last := gh.reviews[len(gh.reviews)-1]
	if len(gh.reviews) != 4 || last.Event != "APPROVE" || last.CommitID != "ddd444" || !strings.Contains(last.Body, "prbot:approval") {
		t.Fatalf("approval after resolution = %+v (%d reviews)", last, len(gh.reviews))
	}
	b.CheckReplies("acme/web", 7)
	drain(ctx, b)
	if len(gh.reviews) != 4 {
		t.Errorf("approved the same commit twice")
	}

	// 7) Double-clicking "Review now" must produce one review, not two.
	if err := b.ReviewNow("acme/web", 7); err != nil {
		t.Fatal(err)
	}
	if err := b.ReviewNow("acme/web", 7); err == nil {
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
