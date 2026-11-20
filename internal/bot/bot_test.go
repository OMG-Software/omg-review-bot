package bot

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
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

// fakeGitHub simulates the slice of the GitHub API the bot uses.
type fakeGitHub struct {
	mu        sync.Mutex
	updatedAt string
	comments  []map[string]any
	reviews   []github.ReviewRequest
	replies   []string
	repliedTo []string
	jwtCalls  int
}

func (f *fakeGitHub) handler(t *testing.T) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		auth := r.Header.Get("Authorization")
		if strings.HasPrefix(r.URL.Path, "/app") && !strings.HasPrefix(auth, "Bearer ") {
			t.Errorf("%s called without App JWT", r.URL.Path)
		}
		if strings.HasPrefix(r.URL.Path, "/repos/") && auth != "token inst-token" {
			t.Errorf("%s called without installation token (got %q)", r.URL.Path, auth)
		}
		pr := map[string]any{
			"number": 7, "title": "Add division", "body": "Adds a thing.", "state": "open",
			"html_url": "https://github.example/acme/web/pull/7", "user": map[string]any{"login": "alice", "type": "User"},
			"updated_at": f.updatedAt, "head": map[string]any{"ref": "feat", "sha": "aaa111"},
			"base": map[string]any{"ref": "main", "sha": "bbb222"},
		}
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
				map[string]any{"filename": "main.go", "status": "modified", "additions": 2, "deletions": 0,
					"patch": "@@ -10,3 +10,5 @@ func main() {\n \ta := 1\n+\tb := a / 0\n+\tfmt.Println(b)\n \treturn"},
				map[string]any{"filename": "go.sum", "status": "modified", "patch": "@@ -1 +1 @@\n-x\n+y"},
			})
		case p == "/repos/acme/web/pulls/7/comments" && r.Method == http.MethodGet:
			send(f.comments)
		case p == "/repos/acme/web/pulls/7/reviews" && r.Method == http.MethodPost:
			var req github.ReviewRequest
			_ = json.NewDecoder(r.Body).Decode(&req)
			f.reviews = append(f.reviews, req)
			send(map[string]any{"id": 555, "html_url": "https://github.example/review/555"})
		case strings.HasPrefix(p, "/repos/acme/web/pulls/7/comments/") && strings.HasSuffix(p, "/replies"):
			var body map[string]string
			_ = json.NewDecoder(r.Body).Decode(&body)
			f.replies = append(f.replies, body["body"])
			f.repliedTo = append(f.repliedTo, p)
			send(map[string]any{"id": 999})
		case p == "/repos/acme/web/issues/7/comments":
			send([]any{})
		case strings.HasPrefix(p, "/repos/acme/web/contents/"):
			_, _ = io.WriteString(w, strings.Repeat("// line\n", 20))
		default:
			t.Errorf("unexpected GitHub call %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
		}
	})
}

// fakeLLM is an OpenAI-compatible server returning a canned review (JSON
// mode) or a canned conversational reply (free text).
func fakeLLM(t *testing.T, calls *int) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			t.Errorf("unexpected LLM call %s", r.URL.Path)
		}
		*calls++
		var req map[string]any
		_ = json.NewDecoder(r.Body).Decode(&req)
		content := "Good point. Dividing by a constant zero will panic at runtime, so please guard it."
		if _, ok := req["response_format"]; ok {
			msgs := req["messages"].([]any)
			user := msgs[1].(map[string]any)["content"].(string)
			if !strings.Contains(user, "    11 + \tb := a / 0") {
				t.Errorf("diff was not annotated with line numbers:\n%s", user)
			}
			if strings.Contains(user, "go.sum") {
				t.Errorf("ignored file go.sum was sent to the model")
			}
			content = "<think>let me look</think>```json\n" + `{"summary":"Adds a division.","comments":[` +
				`{"path":"main.go","line":11,"severity":"critical","title":"Division by zero","body":"This always panics."},` +
				`{"path":"b/main.go","line":999,"severity":"minor","title":"Out of range","body":"Model picked a bad line."},` +
				`{"path":"main.go","line":12,"severity":"nit","title":"Use log","body":"Prefer a logger."}]}` + "\n```"
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

func TestReviewAndReplyEndToEnd(t *testing.T) {
	gh := &fakeGitHub{updatedAt: "2026-01-01T00:00:00Z"}
	ghSrv := httptest.NewServer(gh.handler(t))
	defer ghSrv.Close()
	llmCalls := 0
	llmSrv := httptest.NewServer(fakeLLM(t, &llmCalls))
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
	cfg.Review.MinSeverity = "minor" // drops the nit
	if err := settings.Save(st, cfg); err != nil {
		t.Fatal(err)
	}

	b := New(st, dir, slog.New(slog.NewTextHandler(io.Discard, nil)))
	ctx := context.Background()

	// 1) First poll discovers the PR and reviews it.
	b.pollOnce(ctx)
	drain(ctx, b)
	if s := b.Status(); s.LastPollErr != "" || s.BotLogin != "testbot[bot]" {
		t.Fatalf("status = %+v", s)
	}
	if len(gh.reviews) != 1 {
		t.Fatalf("expected 1 review posted, got %d", len(gh.reviews))
	}
	rv := gh.reviews[0]
	if rv.CommitID != "aaa111" || rv.Event != "COMMENT" {
		t.Errorf("review commit/event = %s/%s", rv.CommitID, rv.Event)
	}
	if len(rv.Comments) != 1 || rv.Comments[0].Path != "main.go" || rv.Comments[0].Line != 11 || rv.Comments[0].Side != "RIGHT" {
		t.Fatalf("inline comments = %+v", rv.Comments)
	}
	for _, want := range []string{"Adds a division.", "Out of range", "go.sum", "1 lower-priority"} {
		if !strings.Contains(rv.Body, want) {
			t.Errorf("review body missing %q:\n%s", want, rv.Body)
		}
	}
	pr, _ := st.GetPR("acme/web", 7)
	if pr.Status != "reviewed" || pr.ReviewedSHA != "aaa111" {
		t.Fatalf("PR after review = %s / %s (%s)", pr.Status, pr.ReviewedSHA, pr.LastError)
	}

	// 2) Polling again with no new commit must not re-review.
	b.pollOnce(ctx)
	drain(ctx, b)
	if len(gh.reviews) != 1 {
		t.Fatalf("re-reviewed without a new commit")
	}

	// 3) A human replies to the bot's comment: the bot answers in the thread, once.
	line := 11
	gh.mu.Lock()
	gh.comments = []map[string]any{
		{"id": 900, "user": map[string]any{"login": "testbot[bot]", "type": "Bot"}, "body": "🔴 **Division by zero**",
			"path": "main.go", "line": line, "diff_hunk": "@@ -10,3 +10,5 @@", "created_at": time.Now().Format(time.RFC3339)},
		{"id": 901, "in_reply_to_id": 900, "user": map[string]any{"login": "alice", "type": "User"}, "body": "Is this really a problem?",
			"path": "main.go", "line": line, "created_at": time.Now().Add(time.Minute).Format(time.RFC3339)},
	}
	gh.updatedAt = "2026-01-01T00:05:00Z"
	gh.mu.Unlock()
	b.pollOnce(ctx)
	drain(ctx, b)
	if len(gh.replies) != 1 || !strings.HasSuffix(gh.repliedTo[0], "/comments/900/replies") {
		t.Fatalf("replies = %v to %v", gh.replies, gh.repliedTo)
	}
	if !st.IsHandled(901) {
		t.Errorf("reply 901 not marked handled")
	}
	b.CheckReplies("acme/web", 7)
	drain(ctx, b)
	if len(gh.replies) != 1 {
		t.Fatalf("replied twice to the same message")
	}
	// 4) Double-clicking "Review now" must produce one review, not two.
	if err := b.ReviewNow("acme/web", 7); err != nil {
		t.Fatal(err)
	}
	if err := b.ReviewNow("acme/web", 7); err == nil {
		t.Error("second ReviewNow while one is queued should be refused")
	}
	drain(ctx, b)
	if len(gh.reviews) != 2 {
		t.Errorf("expected exactly one extra review, got %d total", len(gh.reviews))
	}
	if gh.jwtCalls != 1 {
		t.Errorf("App info fetched %d times; expected it to be cached", gh.jwtCalls)
	}
}
