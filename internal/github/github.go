// Package github is a small GitHub REST client for a GitHub App: JWT auth,
// installation tokens, ETag-aware polling and the PR review endpoints.
package github

import (
	"bytes"
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/OMG-Software/omg-review-bot/internal/textutil"
	"github.com/OMG-Software/omg-review-bot/internal/version"
)

const DefaultBaseURL = "https://api.github.com"

// ---------- API types (only the fields we use) ----------

type User struct {
	ID        int64  `json:"id"`
	Login     string `json:"login"`
	Type      string `json:"type"` // User | Organization | Bot
	Name      string `json:"name"`
	AvatarURL string `json:"avatar_url"`
}

type Installation struct {
	ID      int64 `json:"id"`
	Account User  `json:"account"`
}

type AppInfo struct {
	ID      int64  `json:"id"`
	Slug    string `json:"slug"`
	Name    string `json:"name"`
	HTMLURL string `json:"html_url"`
}

type Repo struct {
	ID       int64  `json:"id"`
	FullName string `json:"full_name"`
	Archived bool   `json:"archived"`
	Owner    User   `json:"owner"`
}

type Ref struct {
	Ref string `json:"ref"`
	SHA string `json:"sha"`
}

type Label struct {
	Name string `json:"name"`
}

type PullRequest struct {
	Number    int     `json:"number"`
	Title     string  `json:"title"`
	Body      string  `json:"body"`
	State     string  `json:"state"`
	Draft     bool    `json:"draft"`
	HTMLURL   string  `json:"html_url"`
	User      User    `json:"user"`
	UpdatedAt string  `json:"updated_at"`
	Head      Ref     `json:"head"`
	Base      Ref     `json:"base"`
	Labels    []Label `json:"labels"`
}

type PRFile struct {
	Filename         string `json:"filename"`
	Status           string `json:"status"`
	Additions        int    `json:"additions"`
	Deletions        int    `json:"deletions"`
	Patch            string `json:"patch"`
	PreviousFilename string `json:"previous_filename"`
}

type ReviewComment struct {
	ID           int64     `json:"id"`
	InReplyToID  int64     `json:"in_reply_to_id"`
	User         User      `json:"user"`
	Body         string    `json:"body"`
	Path         string    `json:"path"`
	Line         *int      `json:"line"`
	OriginalLine *int      `json:"original_line"`
	DiffHunk     string    `json:"diff_hunk"`
	CreatedAt    time.Time `json:"created_at"`
	HTMLURL      string    `json:"html_url"`
}

// AnyLine returns the current line, falling back to the original one for outdated comments.
func (c *ReviewComment) AnyLine() int {
	if c.Line != nil {
		return *c.Line
	}
	if c.OriginalLine != nil {
		return *c.OriginalLine
	}
	return 0
}

type IssueComment struct {
	ID        int64     `json:"id"`
	User      User      `json:"user"`
	Body      string    `json:"body"`
	CreatedAt time.Time `json:"created_at"`
	HTMLURL   string    `json:"html_url"`
}

type DraftComment struct {
	Path string `json:"path"`
	Line int    `json:"line"`
	Side string `json:"side"`
	Body string `json:"body"`
}

type ReviewRequest struct {
	CommitID string         `json:"commit_id,omitempty"`
	Body     string         `json:"body"`
	Event    string         `json:"event"`
	Comments []DraftComment `json:"comments,omitempty"`
}

type Review struct {
	ID       int64  `json:"id"`
	HTMLURL  string `json:"html_url"`
	User     User   `json:"user"`
	State    string `json:"state"` // APPROVED, CHANGES_REQUESTED, COMMENTED, DISMISSED, PENDING
	CommitID string `json:"commit_id"`
	Body     string `json:"body"`
}

// ReviewThread is an inline review thread's resolution state (GraphQL only).
type ReviewThread struct {
	ID            string // GraphQL node ID, used to resolve the thread
	IsResolved    bool
	RootCommentID int64 // REST ID of the thread's first comment
}

// ---------- errors ----------

type APIError struct {
	Status  int
	Method  string
	Path    string
	Message string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("GitHub %s %s: HTTP %d: %s", e.Method, e.Path, e.Status, e.Message)
}

// IsStatus reports whether err is a GitHub API error with the given HTTP status.
func IsStatus(err error, status int) bool {
	var ae *APIError
	return errors.As(err, &ae) && ae.Status == status
}

// ---------- App ----------

type App struct {
	id   int64
	base string
	key  *rsa.PrivateKey
	hc   *http.Client

	mu     sync.Mutex
	tokens map[int64]instToken

	rate atomic.Int64
}

type instToken struct {
	token string
	exp   time.Time
}

func NewApp(appID int64, privateKeyPEM, baseURL string) (*App, error) {
	if appID == 0 {
		return nil, errors.New("GitHub App ID is not set")
	}
	key, err := parseKey(privateKeyPEM)
	if err != nil {
		return nil, err
	}
	baseURL = strings.TrimRight(strings.TrimSpace(baseURL), "/")
	if baseURL == "" {
		baseURL = DefaultBaseURL
	}
	a := &App{id: appID, base: baseURL, key: key, hc: apiHTTPClient(baseURL, 60*time.Second), tokens: map[int64]instToken{}}
	a.rate.Store(-1)
	return a, nil
}

func parseKey(p string) (*rsa.PrivateKey, error) {
	block, _ := pem.Decode([]byte(strings.TrimSpace(p)))
	if block == nil {
		return nil, errors.New("GitHub App private key: no PEM block found")
	}
	if k, err := x509.ParsePKCS1PrivateKey(block.Bytes); err == nil {
		return k, nil
	}
	k, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("GitHub App private key: %w", err)
	}
	rk, ok := k.(*rsa.PrivateKey)
	if !ok {
		return nil, errors.New("GitHub App private key is not an RSA key")
	}
	return rk, nil
}

// RateRemaining is the last X-RateLimit-Remaining seen for installation calls (-1 = unknown).
func (a *App) RateRemaining() int64 { return a.rate.Load() }

func b64(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

// jwt builds the short-lived RS256 token that authenticates as the App itself.
func (a *App) jwt() (string, error) {
	now := time.Now()
	claims, _ := json.Marshal(map[string]any{
		"iat": now.Add(-60 * time.Second).Unix(), // allow for clock drift
		"exp": now.Add(9 * time.Minute).Unix(),
		"iss": a.id,
	})
	signing := b64([]byte(`{"alg":"RS256","typ":"JWT"}`)) + "." + b64(claims)
	sum := sha256.Sum256([]byte(signing))
	sig, err := rsa.SignPKCS1v15(rand.Reader, a.key, crypto.SHA256, sum[:])
	if err != nil {
		return "", err
	}
	return signing + "." + b64(sig), nil
}

// apiHTTPClient follows redirects only while they stay on the API's own host,
// so a token or request body is never sent anywhere else.
func apiHTTPClient(base string, timeout time.Duration) *http.Client {
	return &http.Client{Timeout: timeout, CheckRedirect: func(req *http.Request, via []*http.Request) error {
		if len(via) >= 5 {
			return errors.New("stopped after 5 redirects")
		}
		if !sameOrigin(base, req.URL) {
			return fmt.Errorf("refusing redirect to %s: not the GitHub API host", req.URL.Host)
		}
		return nil
	}}
}

// sameOrigin reports whether u has base's scheme and host.
func sameOrigin(base string, u *url.URL) bool {
	b, err := url.Parse(base)
	return err == nil && b.Host != "" && strings.EqualFold(b.Scheme, u.Scheme) && strings.EqualFold(b.Host, u.Host)
}

func (a *App) send(ctx context.Context, method, urlOrPath, auth string, body any, hdr map[string]string) (*http.Response, []byte, error) {
	u := urlOrPath
	if strings.HasPrefix(u, "/") {
		u = a.base + u
	}
	// Absolute URLs come from response headers (pagination links); like
	// redirects, they must stay on the API host, or the token would go with them.
	if pu, err := url.Parse(u); err != nil || !sameOrigin(a.base, pu) {
		return nil, nil, fmt.Errorf("refusing request to %q: not the GitHub API host", textutil.Truncate(u, 200))
	}
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, nil, err
		}
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, u, rdr)
	if err != nil {
		return nil, nil, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	req.Header.Set("User-Agent", version.UserAgent())
	if auth != "" {
		req.Header.Set("Authorization", auth)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := a.hc.Do(req)
	if err != nil {
		return nil, nil, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if err != nil {
		return resp, nil, err
	}
	if strings.HasPrefix(auth, "token ") {
		if n, err := strconv.ParseInt(resp.Header.Get("X-RateLimit-Remaining"), 10, 64); err == nil {
			a.rate.Store(n)
		}
	}
	if resp.StatusCode >= 400 {
		var m struct {
			Message string          `json:"message"`
			Errors  json.RawMessage `json:"errors"`
		}
		_ = json.Unmarshal(data, &m)
		msg := m.Message
		if len(m.Errors) > 0 && string(m.Errors) != "null" {
			msg += " " + string(m.Errors)
		}
		if msg == "" {
			msg = textutil.Truncate(strings.TrimSpace(string(data)), 300)
		}
		path := req.URL.Path
		return resp, data, &APIError{Status: resp.StatusCode, Method: method, Path: path, Message: msg}
	}
	return resp, data, nil
}

func (a *App) appCall(ctx context.Context, method, path string, out any) (*http.Response, error) {
	tok, err := a.jwt()
	if err != nil {
		return nil, err
	}
	resp, data, err := a.send(ctx, method, path, "Bearer "+tok, nil, nil)
	if err != nil {
		return resp, err
	}
	if out != nil && len(data) > 0 {
		if err := json.Unmarshal(data, out); err != nil {
			return resp, fmt.Errorf("decode %s: %w", path, err)
		}
	}
	return resp, nil
}

// Info returns the App's own metadata (its slug gives the bot login "<slug>[bot]").
func (a *App) Info(ctx context.Context) (*AppInfo, error) {
	var info AppInfo
	if _, err := a.appCall(ctx, http.MethodGet, "/app", &info); err != nil {
		return nil, err
	}
	return &info, nil
}

func (a *App) Installations(ctx context.Context) ([]Installation, error) {
	var all []Installation
	next := "/app/installations?per_page=100"
	for next != "" {
		var page []Installation
		resp, err := a.appCall(ctx, http.MethodGet, next, &page)
		if err != nil {
			return nil, err
		}
		all = append(all, page...)
		next = nextLink(resp.Header)
	}
	return all, nil
}

// token returns a cached installation token, minting a new one shortly before expiry.
func (a *App) token(ctx context.Context, inst int64) (string, error) {
	a.mu.Lock()
	t, ok := a.tokens[inst]
	a.mu.Unlock()
	if ok && time.Until(t.exp) > 5*time.Minute {
		return t.token, nil
	}
	var r struct {
		Token     string    `json:"token"`
		ExpiresAt time.Time `json:"expires_at"`
	}
	if _, err := a.appCall(ctx, http.MethodPost, fmt.Sprintf("/app/installations/%d/access_tokens", inst), &r); err != nil {
		return "", err
	}
	a.mu.Lock()
	a.tokens[inst] = instToken{token: r.Token, exp: r.ExpiresAt}
	a.mu.Unlock()
	return r.Token, nil
}

var linkNextRE = regexp.MustCompile(`<([^>]+)>;\s*rel="next"`)

func nextLink(h http.Header) string {
	if m := linkNextRE.FindStringSubmatch(h.Get("Link")); m != nil {
		return m[1]
	}
	return ""
}

// Base is the REST API base URL the App talks to.
func (a *App) Base() string { return a.base }

// ---------- installation client ----------

// Client calls the API as one App installation (i.e. as the bot in one org).
type Client struct {
	app  *App
	inst int64
}

func (a *App) Installation(id int64) *Client { return &Client{app: a, inst: id} }

func (c *Client) do(ctx context.Context, method, path string, body any, hdr map[string]string, out any) (*http.Response, error) {
	tok, err := c.app.token(ctx, c.inst)
	if err != nil {
		return nil, fmt.Errorf("installation token: %w", err)
	}
	resp, data, err := c.app.send(ctx, method, path, "token "+tok, body, hdr)
	if err != nil {
		return resp, err
	}
	if out != nil && resp.StatusCode != http.StatusNotModified && len(data) > 0 {
		if err := json.Unmarshal(data, out); err != nil {
			return resp, fmt.Errorf("decode %s: %w", path, err)
		}
	}
	return resp, nil
}

func list[T any](ctx context.Context, c *Client, path string, max int) ([]T, error) {
	var all []T
	next := path
	for next != "" {
		var page []T
		resp, err := c.do(ctx, http.MethodGet, next, nil, nil, &page)
		if err != nil {
			return nil, err
		}
		all = append(all, page...)
		if max > 0 && len(all) >= max {
			break
		}
		next = nextLink(resp.Header)
	}
	return all, nil
}

func (c *Client) Repos(ctx context.Context) ([]Repo, error) {
	var all []Repo
	next := "/installation/repositories?per_page=100"
	for next != "" {
		var page struct {
			Repositories []Repo `json:"repositories"`
		}
		resp, err := c.do(ctx, http.MethodGet, next, nil, nil, &page)
		if err != nil {
			return nil, err
		}
		all = append(all, page.Repositories...)
		next = nextLink(resp.Header)
	}
	return all, nil
}

// OpenPulls lists open PRs. With a previous ETag, an unchanged list returns
// notModified=true; those 304 responses don't count against the rate limit.
func (c *Client) OpenPulls(ctx context.Context, fullName, etag string) (pulls []PullRequest, newETag string, notModified bool, err error) {
	path := "/repos/" + fullName + "/pulls?state=open&per_page=100&sort=updated&direction=desc"
	hdr := map[string]string{}
	if etag != "" {
		hdr["If-None-Match"] = etag
	}
	resp, err := c.do(ctx, http.MethodGet, path, nil, hdr, &pulls)
	if err != nil {
		return nil, "", false, err
	}
	if resp.StatusCode == http.StatusNotModified {
		return nil, etag, true, nil
	}
	if next := nextLink(resp.Header); next != "" {
		more, err := list[PullRequest](ctx, c, next, 0)
		if err != nil {
			return nil, "", false, err
		}
		pulls = append(pulls, more...)
	}
	return pulls, resp.Header.Get("ETag"), false, nil
}

// UserByLogin looks up a GitHub user (for adding someone to an org by login).
func (c *Client) UserByLogin(ctx context.Context, login string) (*User, error) {
	var u User
	if _, err := c.do(ctx, http.MethodGet, "/users/"+url.PathEscape(login), nil, nil, &u); err != nil {
		return nil, err
	}
	return &u, nil
}

func (c *Client) Pull(ctx context.Context, fullName string, number int) (*PullRequest, error) {
	var pr PullRequest
	if _, err := c.do(ctx, http.MethodGet, fmt.Sprintf("/repos/%s/pulls/%d", fullName, number), nil, nil, &pr); err != nil {
		return nil, err
	}
	return &pr, nil
}

func (c *Client) PullFiles(ctx context.Context, fullName string, number int) ([]PRFile, error) {
	return list[PRFile](ctx, c, fmt.Sprintf("/repos/%s/pulls/%d/files?per_page=100", fullName, number), 3000)
}

func (c *Client) ReviewComments(ctx context.Context, fullName string, number int) ([]ReviewComment, error) {
	return list[ReviewComment](ctx, c, fmt.Sprintf("/repos/%s/pulls/%d/comments?per_page=100", fullName, number), 0)
}

func (c *Client) IssueComments(ctx context.Context, fullName string, number int) ([]IssueComment, error) {
	return list[IssueComment](ctx, c, fmt.Sprintf("/repos/%s/issues/%d/comments?per_page=100", fullName, number), 0)
}

func (c *Client) CreateReview(ctx context.Context, fullName string, number int, req ReviewRequest) (*Review, error) {
	var r Review
	if _, err := c.do(ctx, http.MethodPost, fmt.Sprintf("/repos/%s/pulls/%d/reviews", fullName, number), req, nil, &r); err != nil {
		return nil, err
	}
	return &r, nil
}

// ReplyToReviewComment replies in the thread whose top-level comment is commentID.
func (c *Client) ReplyToReviewComment(ctx context.Context, fullName string, number int, commentID int64, body string) (*ReviewComment, error) {
	var r ReviewComment
	path := fmt.Sprintf("/repos/%s/pulls/%d/comments/%d/replies", fullName, number, commentID)
	if _, err := c.do(ctx, http.MethodPost, path, map[string]string{"body": body}, nil, &r); err != nil {
		return nil, err
	}
	return &r, nil
}

func (c *Client) CreateIssueComment(ctx context.Context, fullName string, number int, body string) (*IssueComment, error) {
	var r IssueComment
	path := fmt.Sprintf("/repos/%s/issues/%d/comments", fullName, number)
	if _, err := c.do(ctx, http.MethodPost, path, map[string]string{"body": body}, nil, &r); err != nil {
		return nil, err
	}
	return &r, nil
}

// UpdateIssueComment replaces the text of a PR conversation comment.
func (c *Client) UpdateIssueComment(ctx context.Context, fullName string, commentID int64, body string) error {
	path := fmt.Sprintf("/repos/%s/issues/comments/%d", fullName, commentID)
	_, err := c.do(ctx, http.MethodPatch, path, map[string]string{"body": body}, nil, nil)
	return err
}

func (c *Client) Reviews(ctx context.Context, fullName string, number int) ([]Review, error) {
	return list[Review](ctx, c, fmt.Sprintf("/repos/%s/pulls/%d/reviews?per_page=100", fullName, number), 0)
}

// DismissReview withdraws an approval or change request.
func (c *Client) DismissReview(ctx context.Context, fullName string, number int, reviewID int64, message string) error {
	path := fmt.Sprintf("/repos/%s/pulls/%d/reviews/%d/dismissals", fullName, number, reviewID)
	_, err := c.do(ctx, http.MethodPut, path, map[string]string{"message": message, "event": "DISMISS"}, nil, nil)
	return err
}

// ChangedFiles lists the files that differ between two commits.
func (c *Client) ChangedFiles(ctx context.Context, fullName, base, head string) ([]string, error) {
	var r struct {
		Files []struct {
			Filename string `json:"filename"`
		} `json:"files"`
	}
	path := fmt.Sprintf("/repos/%s/compare/%s...%s", fullName, url.PathEscape(base), url.PathEscape(head))
	if _, err := c.do(ctx, http.MethodGet, path, nil, nil, &r); err != nil {
		return nil, err
	}
	if len(r.Files) >= 300 {
		return nil, errors.New("comparison lists 300 files, so it may be incomplete") // GitHub's cap
	}
	out := make([]string, 0, len(r.Files))
	for _, f := range r.Files {
		out = append(out, f.Filename)
	}
	return out, nil
}

// TreePaths lists every file path in the repository at ref (a SHA or branch).
// truncated is true when GitHub cut the listing short (very large repositories).
func (c *Client) TreePaths(ctx context.Context, fullName, ref string) (paths []string, truncated bool, err error) {
	var r struct {
		Tree []struct {
			Path string `json:"path"`
			Type string `json:"type"`
		} `json:"tree"`
		Truncated bool `json:"truncated"`
	}
	path := fmt.Sprintf("/repos/%s/git/trees/%s?recursive=1", fullName, url.PathEscape(ref))
	if _, err := c.do(ctx, http.MethodGet, path, nil, nil, &r); err != nil {
		return nil, false, err
	}
	for _, e := range r.Tree {
		if e.Type == "blob" {
			paths = append(paths, e.Path)
		}
	}
	return paths, r.Truncated, nil
}

// graphqlURL derives the GraphQL endpoint from the REST base URL
// (https://api.github.com/graphql, or https://HOST/api/graphql on GHES).
func graphqlURL(base string) string {
	if b, ok := strings.CutSuffix(base, "/api/v3"); ok {
		return b + "/api/graphql"
	}
	return base + "/graphql"
}

func (c *Client) graphql(ctx context.Context, query string, vars map[string]any, out any) error {
	var resp struct {
		Data   json.RawMessage `json:"data"`
		Errors []struct {
			Message string `json:"message"`
		} `json:"errors"`
	}
	if _, err := c.do(ctx, http.MethodPost, graphqlURL(c.app.base), map[string]any{"query": query, "variables": vars}, nil, &resp); err != nil {
		return err
	}
	if len(resp.Errors) > 0 {
		return fmt.Errorf("GitHub GraphQL: %s", resp.Errors[0].Message)
	}
	if out != nil {
		return json.Unmarshal(resp.Data, out)
	}
	return nil
}

const threadsQuery = `query($owner:String!,$name:String!,$number:Int!,$cursor:String){
  repository(owner:$owner,name:$name){pullRequest(number:$number){
    reviewThreads(first:100,after:$cursor){
      pageInfo{hasNextPage endCursor}
      nodes{id isResolved comments(first:1){nodes{databaseId}}}
    }}}}`

// ReviewThreads returns the resolution state of every inline review thread.
func (c *Client) ReviewThreads(ctx context.Context, fullName string, number int) ([]ReviewThread, error) {
	owner, name, ok := strings.Cut(fullName, "/")
	if !ok {
		return nil, fmt.Errorf("bad repository name %q", fullName)
	}
	var out []ReviewThread
	vars := map[string]any{"owner": owner, "name": name, "number": number, "cursor": nil}
	for page := 0; page < 50; page++ {
		var r struct {
			Repository struct {
				PullRequest struct {
					ReviewThreads struct {
						PageInfo struct {
							HasNextPage bool   `json:"hasNextPage"`
							EndCursor   string `json:"endCursor"`
						} `json:"pageInfo"`
						Nodes []struct {
							ID         string `json:"id"`
							IsResolved bool   `json:"isResolved"`
							Comments   struct {
								Nodes []struct {
									DatabaseID int64 `json:"databaseId"`
								} `json:"nodes"`
							} `json:"comments"`
						} `json:"nodes"`
					} `json:"reviewThreads"`
				} `json:"pullRequest"`
			} `json:"repository"`
		}
		if err := c.graphql(ctx, threadsQuery, vars, &r); err != nil {
			return nil, err
		}
		rt := r.Repository.PullRequest.ReviewThreads
		for _, n := range rt.Nodes {
			t := ReviewThread{ID: n.ID, IsResolved: n.IsResolved}
			if len(n.Comments.Nodes) > 0 {
				t.RootCommentID = n.Comments.Nodes[0].DatabaseID
			}
			out = append(out, t)
		}
		if !rt.PageInfo.HasNextPage {
			break
		}
		vars["cursor"] = rt.PageInfo.EndCursor
	}
	return out, nil
}

// ResolveReviewThread marks an inline review thread as resolved.
func (c *Client) ResolveReviewThread(ctx context.Context, threadID string) error {
	return c.graphql(ctx, `mutation($id:ID!){resolveReviewThread(input:{threadId:$id}){thread{isResolved}}}`,
		map[string]any{"id": threadID}, nil)
}

// FileContent returns a file's raw content at ref.
func (c *Client) FileContent(ctx context.Context, fullName, filePath, ref string) (string, error) {
	tok, err := c.app.token(ctx, c.inst)
	if err != nil {
		return "", err
	}
	segs := strings.Split(filePath, "/")
	for i, s := range segs {
		if s == "" || s == "." || s == ".." {
			return "", fmt.Errorf("refusing file path %q", filePath)
		}
		segs[i] = url.PathEscape(s)
	}
	p := "/repos/" + fullName + "/contents/" + strings.Join(segs, "/") + "?ref=" + url.QueryEscape(ref)
	_, data, err := c.app.send(ctx, http.MethodGet, p, "token "+tok, nil, map[string]string{"Accept": "application/vnd.github.raw+json"})
	if err != nil {
		return "", err
	}
	return string(data), nil
}
